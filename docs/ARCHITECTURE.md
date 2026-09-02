# Chimera Architecture

> Browser-based LLM Gateway — expose ChatGPT, Claude, Qwen, DeepSeek web sessions as an OpenAI-compatible API.

Inspired by [Chimera-Gateway](https://github.com/GautamVhavle/Chimera-Gateway) but re-architected in **Go** for minimal runtime, static binary distribution, and goroutine-native concurrency.

---

## 1. System Overview

```
Client (OpenAI SDK / LangChain / curl / any OpenAI client)
    │
    ▼
┌──────────────────────────────┐
│  Chimera Gateway (Go)        │  :8000
│  ┌────────────────────────┐  │
│  │ API Layer (chi)        │  │  OpenAI-compatible + health + auth + rate-limit + CORS
│  │  /v1/chat/completions  │  │
│  │  /v1/models            │  │
│  │  /health               │  │
│  └──────────┬─────────────┘  │
│             │                │
│  ┌──────────▼─────────────┐  │
│  │ Provider Abstraction   │  │  Interface + Base + per-provider Client
│  └──────────┬─────────────┘  │
│             │                │
│  ┌──────────▼─────────────┐  │
│  │ Browser Manager (rod)  │  │  Launch, stealth, human behavior, page pool (mutex now, pool future)
│  └──────────┬─────────────┘  │
└─────────────┼────────────────┘
              │
              ▼
     Real Chromium (persistent profile at ./browser_data/<provider>)
              │
              ▼
   chatgpt.com | claude.ai | chat.qwen.ai | chat.deepseek.com
              │
              ▼
   Response scraped → normalized to OpenAI JSON → SSE or JSON returned
```

### Data Flow (single chat completion)

```
1. POST /v1/chat/completions  {messages, tools, stream}
2. buildPrompt() → concatenates history + injects tool-calling system prompt (if tools present)
3. Provider.SendMessage(text, threadID)
   a. RandomDelay (400-1500ms human pause)
   b. FindElement(ChatInput) via selector fallback list
   c. Click + Page.InsertText(paste)
   d. RandomDelay + Click SendButton || Press Enter
   e. WaitForResponse:
        Phase1: poll for StopButton appearance (streaming started)
        Phase2: poll for StopButton disappearance (streaming done)
        Fallback: text stability (4 consecutive identical polls) — used for Qwen/DeepSeek
        Special: Claude data-is-streaming attribute
   f. Sleep 1s DOM settle
   g. ExtractLastResponseText via querySelectorAll(AssistantMessage) → innerText of last node
   h. Echo detection: if response contains prefix of user prompt → retry after 3s
   i. Return ProviderResponse{Message, ThreadID, ElapsedMs}
4. API layer: tools.ParseToolCalls(text) if tools requested → []ToolCall
5. Respond: JSON or SSE chunks (20 chars/chunk, 10ms throttle, [DONE] terminator)
```

---

## 2. Module Map

```
cmd/chimera/main.go          Entry, banner, config.Load(), browser.Launch(), provider.Init(), http.Server + graceful shutdown
internal/config/config.go    Env + .env loading, Validate(provider), ProviderURL(), BrowserDataPath(), EnsureDirs()
internal/logging/logging.go  Leveled logger (debug/info/warn/error + Info/Warn aliases), stderr + file tee
internal/browser/
  manager.go                 Persistent Chromium via go-rod/launcher, UserDataDir, headful by default, jittered viewport, lock-file cleanup, NewPage()
  stealth.go                 navigator.webdriver=false, fake plugins/languages, chrome.runtime, WebGL vendor, canvas fingerprint, permissions override
  human.go                   HumanBehavior: RandomDelay, TypeTextFast via JS insertText, Click with hover+offset, PressEnter, jittered MoveMouse
internal/models/openai.go    OpenAI-compatible schemas: ChatCompletionRequest/Response, Streaming Chunk, Tool, ToolCall, ProviderResponse, ModelList
internal/tools/calling.go    Prompt engineering for tool calling:
                               - BuildToolPrompt(provider) → provider-specific system prompt
                               - ParseToolCalls → brace-depth tracker (handles nested JSON + code fences)
                               - StripToolPrompt, IsToolCallResponse, TrimNonJSON
internal/providers/
  provider.go                Interface Provider {Name, ModelID, Init, SendMessage, NewChat, ExtractResponse, IsLoggedIn}
  base.go                    Shared: FindElement (fallback list), FindElementExists, WaitForResponse, CountAssistantMessages (gson Value.Int), ExtractLastResponseText, RandomDelay, IsLoggedIn, ScrollToBottom
  chatgpt/client.go+selectors.go   Model chimera-chatgpt, stop/copy/button selectors, text stability fallback not needed, echo check
  claude/client.go+selectors.go    Model chimera-claude, data-is-streaming detector, ProseMirror selectors
  qwen/client.go+selectors.go      Model chimera-qwen, textarea selectors, Chinese + English arias, text stability fallback
  deepseek/client.go+selectors.go  Model chimera-deepseek, similar to qwen
internal/api/server.go       chi router, middleware (RequestID, RealIP, Logger, Recoverer, Heartbeat /health, Bearer auth, rate limiter, CORS-ish), mutex-guarded provider calls, SSE streaming, token estimation, tool-call wiring
```

---

## 3. Provider Abstraction

All providers satisfy `providers.Provider`. The choice is `PROVIDER=chatgpt|claude|qwen|deepseek` (env).

Each provider:

- embeds `*providers.Base` (page, cfg, human, log)
- defines own `selectors.go` with ordered fallback lists:
  ```go
  var ChatInput = []string{"#prompt-textarea", "div[contenteditable='true'][id='prompt-textarea']", ...}
  ```
  `_find` tries each with short timeout → first match wins. When vendors change UI, only `selectors.go` needs edit.

| Provider | Input selector flavor | Send | Stop detector | Extract |
|---|---|---|---|---|
| ChatGPT | `div[contenteditable]` `#prompt-textarea` | `button[data-testid='send-button']` | `button[data-testid='stop-button']` | `div[data-message-author-role='assistant']` |
| Claude | `div.ProseMirror` `[aria-label*='Reply']` | `button[aria-label='Send Message']` | `data-is-streaming` attribute | `[data-is-streaming] .font-claude-message` |
| Qwen | `textarea[placeholder]` `textarea` | `button[aria-label='发送']` + submit | stop button → text stability fallback | `div[data-role='assistant']` |
| DeepSeek | `textarea` | `div[class*='send'] button` | stop button → text stability fallback | same |

`WaitForResponse` vs `waitForTextStability`:

- **Stop lifecycle** (Chimera primary): poll for stop button to appear then vanish. Works when vendor shows a “Stop generating” button.
- **Claude streaming attr**: `document.querySelector('[data-is-streaming]')` boolean.
- **Text stability** (Qwen/DeepSeek fallback): poll `innerText` of last assistant node; if unchanged for 4×500ms, consider complete. Handles vendors without visible stop button.

**Echo recovery**: after extraction, if response contains leading 50 chars of user prompt or tool markers (`Available functions:`), wait 3s and re-extract. Mirrors Chimera's echo detection.

---

## 4. OpenAI Compatibility Layer

- **Endpoint** `POST /v1/chat/completions` accepts standard `ChatCompletionRequest` (model, messages, stream, tools, tool_choice, temperature, etc.). Multimodal `content` may be string or `[{type:text,...},{type:image_url,…}]` → `UnmarshalContent()` concatenates text parts (images/files are TODO: upload via hidden `<input type=file>` as Chimera does).

- **Tool calling**: no native browser API → prompt injection. `tools.BuildToolPrompt` injects function signatures into system prompt; model is instructed to output `{"tool_calls":[{"name":…, "arguments":{…}}]}` in a JSON code fence or bare JSON. `tools.ParseToolCalls` extracts with brace-depth tracker (handles escaped strings, nested objects). `tool_choice` is mapped to prompt phrasing (`auto`/`required`/`none`/specific function).

- **Streaming**: true browser streaming is not exposed via CDP; gateway emulates SSE by fetching full response then emitting 20-char chunks at 10ms intervals + initial `role:assistant` chunk + final `finish_reason:stop` + `data: [DONE]`. Compatible with `openai` Python/JS SDKs and LangChain.

- **Models**: `GET /v1/models` returns single entry `{id: provider.ModelID(), owned_by: provider.Name()}`. Model field in request is ignored except for logging (mirrors Chimera's `Chimera-browser` / `claude-browser`).

- **Auth**: `Authorization: Bearer <API_TOKEN>` or `x-api-key`/`anthropic-api-key`. Empty token disables auth (like Chimera). Skips `/`, `/health`.

- **Rate limiting**: token-bucket `rate.NewLimiter(1 / RATE_LIMIT_SECONDS)`. Returns 429 when exceeded.

- **Concurrency**: single `rod.Page` → DOM cannot handle parallel writes. `Server.mu sync.Mutex` serializes `SendMessage` (including streaming). Chimera uses `BrowserPagePool` (multiple tabs); Chimera's mutex is the lightweight v1 equivalent. Future: page pool with round-robin + `x-session-id` sticky routing (see Roadmap).

---

## 5. Browser Automation Hardening (from Chimera)

| Technique | Chimera (Go/rod) | Chimera (Python/Patchright) |
|---|---|---|
| Persistent profile | `launcher.UserDataDir(browser_data/<provider>)` | `launch_persistent_context` |
| SingletonLock cleanup | `cleanLockFiles()` on launch | same |
| Viewport jitter | `1280±20 × 720±20` per launch | same |
| Webdriver mask | `Object.defineProperty(navigator,'webdriver',…)` | `playwright-stealth` add_init_script |
| Plugins/languages spoof | fake `navigator.plugins`, `languages` | stealth lib |
| Headful default | `Headless=false` (anti-bot) | same |
| Human delays | `ThinkingPauseMin/Max` 400-1500ms + typing 30-120ms | 500-1200ms + 300-600ms |
| Paste vs type | `Page.InsertText` (paste for contenteditable) | `keyboard.insert_text` |
| Click hover | offset ±5px, hover then click | same |
| Stealth re-injection | `applyStealth(page)` after nav; could hook `framenavigated` for Docker DNS case | `page.evaluate` workaround for Docker DNS |
| DNS pre-resolve | not yet (Docker entrypoint can pre-resolve chatgpt.com etc. via socket) | Python socket hosts injection |

---

## 6. Configuration

All via env + `.env` (godotenv). See `.env.example:61`:

- `PROVIDER`, `BROWSER_DATA_DIR`, `HEADLESS`, `SLOW_MO`
- `CHATGPT_URL`, `CLAUDE_URL`, `QWEN_URL`, `DEEPSEEK_URL`
- `RESPONSE_TIMEOUT=120s`, `SELECTOR_TIMEOUT=10s`
- `TYPING_SPEED_MIN/MAX`, `THINKING_PAUSE_MIN/MAX`
- `API_HOST/PORT`, `RATE_LIMIT_SECONDS`, `API_TOKEN`
- `LOG_DIR/LEVEL/VERBOSE`

`config.Load()` ensures dirs, validates provider.

---

## 7. Deployment Topologies

### Local binary
```
cp .env.example .env   # set PROVIDER + API_TOKEN
go build -o chimera ./cmd/chimera
./chimera              # launches headful Chromium, needs display
# headless alternative: HEADLESS=true ./chimera
curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
```

### Docker (planned, mirroring Chimera)
```
FROM golang:1.23 AS builder → build static binary
FROM debian:bookworm-slim → install chromium, xvfb, x11vnc, novnc, supervisord
COPY --from=builder /app/chimera /usr/local/bin/
# supervisord runs: Xvfb :99, x11vnc :5900, noVNC :6080, chimera :8000
docker compose up --build -d
open http://localhost:6080/vnc.html  # one-time login
open http://localhost:8000/health
```
Xvfb avoids focus-stealing when launched via agent; noVNC gives remote login without RDP.

### Kubernetes (future)
Single replica per provider (browser is stateful, not horizontally scaled without page pool). Use PVC for `browser_data`, readinessProbe on `/health`.

---

## 8. Limitations (shared with Chimera)

- Latency 5-30s per request (real browser round-trip).
- Sessions expire days/weeks → re-login volume.
- UI selectors brittle → centralize in `selectors.go`, monitor vendor DOM.
- Tool calling via prompting reliable for 1-7 tools; complex schemas may need retry.
- Single page concurrency → serialized; true multi-tab pool is v2.
- Vision/file attachments scaffolding present (selectors for hidden file input) but not yet wired to `ContentPart.image_url` extraction.

---

## 9. Roadmap vs Chimera

- [x] Go port (static binary, lower RAM, faster boot)
- [x] 4 providers (ChatGPT, Claude, Qwen, DeepSeek)
- [x] Tool calling prompt injection + brace-depth parser
- [x] Streaming emulation + auth + rate limit
- [ ] **Page pool** (Chimera's BrowserPagePool): N tabs, round-robin, `x-session-id` stickiness, `/preview` SSE screen-share
- [ ] **Vision/files**: download `image_url`/`file` parts → `set_input_files` on hidden `<input type=file>` → wait 3s +1s/file
- [ ] **TUI** (like Chimera's textual TUI) for interactive chat
- [ ] **Metrics**: Prometheus for request duration, selector fallback hits, echo count
- [ ] **Image generation** (ChatGPT DALL-E) via DOM scrape

---

## 10. Why not keep Chimera's Python stack?

See `docs/TECH_STACK_DECISION.md:1` for quantitative runtime comparison. TL;DR: Go static binary wins on footprint, startup, and concurrency; Python's Patchright+FastAPI is excellent for velocity but heavier and GIL-bound.

