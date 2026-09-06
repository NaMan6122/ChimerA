# 002 — Responses API (`POST /v1/responses`)

**Problem:** Codex CLI and newer OpenAI tooling speak `/v1/responses`, not
`/v1/chat/completions`. One endpoint away from that whole user base.

**Goal:** translate Responses → internal chat → ResponseObject. Non-streaming first.

## Design

New file `internal/models/responses.go`:

- `ResponsesRequest`: `model`, `instructions?`, `input` (string **or** array of
  `{type:"message", role, content}` / `{type:"function_call_output", call_id, output}` items),
  `tools?` (function tools with `parameters` schema), `tool_choice?`, `stream?`,
  `max_output_tokens?`, `user?`.
- `ResponseObject`: `id:"resp_<unixnano>"`, `object:"response"`, `model` (resolved
  provider model ID), `status:"completed"|"failed"`, `output[]` with
  `{type:"message", role:"assistant", content:[{type:"output_text", text}]}` and/or
  `{type:"function_call", call_id, name, arguments<string>}`, `usage{input_tokens,
  output_tokens, total_tokens}` via existing `estimateTokens`.

New handler `handleResponses` in `internal/api/server.go` (route `POST /v1/responses`,
same auth/rate-limit/meter middleware as chat):

1. Decode (2MB cap, same as chat). `stream:true` → `400 streaming_unsupported`
   (documented; SSE event mapping is a follow-up, not this spec).
2. Convert: `instructions` → system message; string input → single user message;
   message items → `ChatMessage`; `function_call_output` items → `role:"tool"` messages
   (reuse `buildPrompt` folding).
3. Resolve provider via existing `providerForModel`; reuse pre-flight char guard,
   quota gate, `acquireProviderLock` + lock-wait/error/duration telemetry and
   `recordUsage` with `streaming:false`.
4. Parse tool calls via hardened parser (spec 004). Any calls → `output` gets
   `function_call` items and top-level `status:"completed"` (matching OpenAI's
   `incomplete_details`-free success shape); plain text → message item only.
5. Errors reuse existing `type` strings (`invalid_request`, `message_too_long`,
   `quota_exceeded`, `provider_busy`, `provider_error`). Unknown `model` values
   fall back to the default provider exactly like chat (documented, tested) —
   there is no `model_not_found` on this endpoint in single/pooled mode.

## Files

- `internal/models/responses.go` (new)
- `internal/api/server.go` (route + handler, ~120 lines)
- `internal/api/server_test.go` (mock-backed cases)
- `docs/API.md`, `README.md` (endpoint bullets)

## Acceptance

- Text turn returns `output[0].type=="message"` with echoed mock text; usage present.
- `trigger_tool`-style reply returns a `function_call` item with valid JSON `arguments`.
- `stream:true` → 400 `streaming_unsupported`; unknown model → default-provider
  fallback (chat parity).
- Quota/metering behave identically to chat (counter +1 per attempt).
- `go vet ./... && go test ./internal/api/`.
