# Chimera Workplan — Hardening Against Upstream Lessons

*Derived from analysis of a reference browser-gateway (Python + Playwright) — sanitized, no upstream naming in repo.*

## Goal
Single `Chimera` binary `:8000` `PROVIDER=all` with **1 Chromium, page-per-provider pool**, OpenAI-compatible, resilient to UI rotation, large prompts, and Docker DNS.

## P0 — Ship Blockers (do now, before public open source)

### 1. Large-prompt freeze (ref #23 analog)
- **File** `internal/browser/human.go:54` `TypeTextFast`
- **Fix** Gate `>200 chars` → `ClipboardEvent('paste')` primary, `execCommand('insertText')` fallback, else `Page.InsertText`. Add `selectAll/delete` clear before insert (ref `human_type`).
- **Test** `internal/tools/calling_test.go` with 5k-char JSON tool payload, assert `<2s` not freeze.

### 2. “Message too long” hang (ref #17)
- **File** `internal/providers/base.go:61` `WaitForResponse` + `internal/providers/chatgpt/client.go:102`
- **Fix** Pre-flight `len(prompt) > 8000` → immediate `400` with `error_type: message_too_long`. Add detector for `button:disabled` + banner `div:text("message too long")` → `502` not 2m hang.
- **Test** mock page with disabled send.

### 3. Selector quoting + IIFE bug (already fixed, harden)
- **File** `internal/providers/base.go:92` `CountAssistantMessages` + `130` `ExtractLastResponseText`
- **Fix** Already `"` wrapping + `() => {}` not `(() =>)()`; add unit test for `div[data-message-author-role='assistant']` with `'` inside.
- **Guard** Centralize `selectorEsc` helper, reuse.

### 4. Docker DNS + stale state (ref `manager.py: _resolve_domains_for_chrome`, `_cleanup_stale_locks`)
- **File** `docker/entrypoint.sh:13` (5 domains) + `internal/browser/manager.go:162` (3 locks)
- **Fix**
  - Expand `entrypoint.sh` to 17 domains `chatgpt.com, cdn.oaistatic.com, ab.chatgpt.com, auth.openai.com, claude.ai, api.claude.ai, kimi.com, chat.deepseek.com…` + `--host-resolver-rules`
  - `manager.go:162` purge `**/*-journal, **/*-wal, **/*-shm`, `Default/Network Persistent State`, `Default/Cache`, `Code Cache`, `GPUCache` (4 steps from ref)
  - `stealth.go:8` split Docker vs non-Docker: `page.Evaluate()` + `framenavigated` listener, not `add_init_script` (kills DNS)
- **Test** `docker compose up --build` cold + crash-recovery.

### 5. Login continuity (`/c/<id>` pinning, ref #16)
- **File** `internal/api/server.go:344` `extractThreadID` currently `len(messages)`; `internal/browser/pool.go:22` pool pages
- **Fix** Persist `session_id → conversation_url` (`/c/<uuid>`) like `PersistentSessionManager` — add `X-Session-Id` header support, `LRU` `MAX_ACTIVE_TABS=10`, save URL after `SendMessage` (`page.url`), restore on `acquire_session_page`.
- **Test** `server_test.go` with `x-session-id: demo` two turns, assert second prompt pruned not duplicated.

## P1 — Concurrency & Sessions (before load)

### 6. PagePool per-provider (ref #1 parallel queuing, ref commit `BrowserPagePool`)
- **File** `internal/browser/pool.go:22` current 1 Page per provider + `sync.Mutex` per provider — good for 1 Chromium.
- **Fix** Extend to `chan *rod.Page` pool per provider `max=3` (config `MAX_CONCURRENT_REQUESTS=3`), `acquire_clean_page` that `new_page` or reuse, `ensure fresh chat` (`goto providerURL` or `click new_chat`), `defer put`.
- **Metric** `hey -n 30 -c 10` on `POST /v1/chat/completions` should not queue >2s.

### 7. Copy-button primary detector (ref `8bfc5a5`, `detector.py`)
- **File** `internal/providers/base.go:61` currently stop-button only
- **Fix** Add `waitForCopyButton(msgIndex)` as primary for ChatGPT (aria `Copy response`), ordered fallback `Copy response` > `copy-turn-action-button` > `Copy`. Keep stop → text-stability `4×500ms`.
- **Test** Long reply `>2000` tokens, assert not truncated.

### 8. Echo & tool-call robustness
- **File** `internal/providers/chatgpt/client.go:172` `isEcho`, `internal/tools/calling.go:63`
- **Fix** Expand markers to `Available functions:` + retry via `copy` button 3s, validate `tool_calls` names vs `valid_names`, `call_<uuid>` not `call_1`, support `tool_choice="required"` / forced name.
- **Test** `calling_test.go` already, add forced-tool test.

## P2 — Feature Parity (after hardening)

### 9. Vision / file upload (ref `openai_routes.py: _extract_image_urls`, `_upload_files`)
- **File** `internal/models/openai.go:61` `ContentPart.ImageURL` + `internal/providers/base.go:100` `ExtractTextViaCopy`
- **Fix** Wire `_download_file` (base64/http) → `DownloadDir=/tmp/chimera_files` → `page.MustElement("input[type=file]").SetFiles()` → `wait 3s+1s/file` before `SendMessage`.
- **Test** `server_test.go` with `image_url: data:image/png;base64,…`.

### 10. Image generation (ref #9)
- **File** `internal/providers/chatgpt/client.go:102`
- **Fix** After `WaitForResponse`, scrape `img[src*=dalle]` / `img[src*=oai]` not `Edit` button, download, return `b64_json` vs `url`. Handle `422` when no image.

### 11. Responses API `/v1/responses` (ref #7)
- **File** `internal/api/server.go:60` only `/v1/chat/completions` + `/v1/models`
- **Fix** Add `POST /v1/responses` translator `ResponsesRequest` → `ChatCompletionRequest` → `SendMessage` → `ResponseObject` (Codex CLI compat). Reuse `buildPrompt`, `tools`.

### 12. Observability & ops
- **File** `internal/browser/manager.go:33`, `pool.go:22`, `server.go:34`
- **Fix** Add `/preview` double-buffered grid (pool tabs), `/v1/tabs` API, `Loki` logs `Xvfb auto-detect` `server.go` analogue, `supervisord` already, add `prometheus` `response_duration`, `selector_fallback_hits`, `echo_count`.

## P3 — Polish

- **Timeout per model** `RESPONSE_TIMEOUT=120s` → `o1` 20m `config.go:100` + `504` structured `error_type/tip`.
- **Locale/timezone** `en-US` `America/Los_Angeles` `pool.go:104` viewport already, add.
- **CI** `go vet` + `go test ./...` + `docker build` on PR, selector snapshot test.

## Execution Order (2-week slice)

**Week 1:** P0 1-4 → `human.go`, `base.go`, `manager.go`, `entrypoint.sh` + tests → tag `v0.2.0-hardened`
**Week 2:** P1 5-8 → `pool.go` multi-page, `server.go` pooled routing (already done), copy-button detector, echo → `hey` load test → `v0.3.0-concurrent`
**Week 3:** P2 9-11 → vision, `/v1/responses` → `v0.4.0-parity` then public.

## Acceptance Criteria

- `go vet && go test ./...` green, `hey -c 10` no `429` beyond rate limit, large prompt `<2s` insert, `message too long` → `400` not hang, `docker compose up` cold DNS OK, `PROVIDER=all` single Chromium `~400MB` `lsof :8000` 1 process 3 tabs, `curl` ChatGPT/Qwen/DeepSeek all `200`.

