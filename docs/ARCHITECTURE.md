# Chimera Architecture

> A dual-substrate Go gateway: browserless HTTP replay of the vendor's own web
> API is the fast path (no browser, ~24 MB, ~1–2 s turns); real Chromium via
> [rod](https://github.com/go-rod/rod) is the universal fallback and the only
> path for `claude` today.

Point any OpenAI SDK, LangChain chain, or agent harness at Chimera and your
ChatGPT / Claude / Qwen / DeepSeek / Kimi / Gemini / Grok / GLM login answers
over `/v1`.

---

## 1. System Overview

```
Client (OpenAI SDK / LangChain / curl / chimera-spin / an agent)
    │
    ▼
┌──────────────────────────────────────────────┐
│  Chimera Gateway (Go, chi)         :8000     │
│  /v1/chat/completions · /v1/responses        │
│  /v1/models · /v1/refresh · /v1/usage        │
│  /v1/health/providers · /metrics             │
│  auth · rate limit · quotas · SSE · MCP      │
└───────────────┬──────────────────────────────┘
                │  providerForModel(model)
                ▼
┌──────────────────────────────────────────────┐
│  Provider abstraction (one per vendor)       │
│                                              │
│  TRANSPORT=webapi (preferred)                │
│    replay the vendor's HTTP API over net/http│
│    storage-state session (auth_data/*.json)  │
│    anti-bot solved in-process in Go          │
│    no browser process, ~24 MB, 1–2 s turns   │
│                                              │
│  TRANSPORT=dom (fallback / claude)           │
│    real Chromium via rod                     │
│    stealth + human pacing + selectors        │
│    ~1.1 GB, 10–20 s turns                    │
└───────────────┬──────────────────────────────┘
                ▼
  chatgpt.com · chat.deepseek.com · chat.qwen.ai
  kimi.ai · chatglm.cn · gemini.google.com · grok.com
  claude.ai (dom only)
```

`TRANSPORT=auto` prefers the browserless path when an exported session exists,
falls back to the DOM on WAF/auth failure, and counts every downgrade as
`chimera_transport_fallback_total`.

### Data flow (one browserless turn)

```
1. POST /v1/chat/completions  {messages, tools, stream}
2. buildPrompt() flattens messages and injects the tool schema block (turn 1)
3. provider.SendMessage(text, threadID)
   a. load the storage-state session        (auth_data/<provider>.json, 0600)
   b. solve the vendor's anti-bot gate      (PoW, nonce, header signature)
   c. POST the vendor's chat endpoint       (cookie jar + bearer)
   d. stream-parse the vendor's SSE/frames  (fixtures-tested state machine)
   e. self-heal stale conversations         (fresh thread, one retry)
4. API layer: tools.ParseToolCalls(text) if tools were requested
5. Respond: JSON, or SSE chunks (delta routing: turn 2+ carries only the new
   tool result — see §4)
```

---

## 2. Module Map

```
cmd/chimera/                  the gateway binary
  main.go                     config.Load, transport selection, http.Server, MCP mode
  up.go                       `chimera up [provider|all]` + argv[0] dispatch (chimera-kimi)
  auth.go                     `chimera auth login|status|check|import|verify` (session harvest)
  doctor.go                   `chimera doctor` local install health check
cmd/chimera-spin/             headless harness (stdlib-only HTTP client)
cmd/chimera-chat/             TUI harness (bubbletea)
internal/api/server.go        chi router, auth, rate limit, quota, SSE, delta routing,
                              tool-call parsing, metering + transcript recording
internal/providers/
  provider.go                 Provider interface + QuotaReporter (optional quota)
  base.go                     shared DOM helpers (FindElement fallback lists, wait/extract)
  fallback.go                 Fallback wrapper: primary retry → secondary transport
  webapi/
    webapi.go                 registry (webapi.Register in init()), error vocabulary
    session.go                storage-state session load/write (0600, atomic)
    convs.go                  bounded thread→conversation cache (FIFO eviction)
    <vendor>/                 one package per browserless transport (client + provider)
internal/providers/<vendor>/  DOM transports: client.go + selectors.go
internal/browser/             manager.go (launch/stealth), pool.go (pooled pages), human.go
internal/models/              OpenAI request/response/chunk schemas
internal/tools/calling.go     tool prompt injection + brace-depth JSON parser
internal/mcp/                 MCP stdio server (hand-rolled JSON-RPC 2.0)
internal/meter/store.go       SQLite usage ledger + monthly quotas
internal/telemetry/           Prometheus metrics + login-health snapshots
internal/session/manager.go   X-Session-Id → conversation URL continuity (DOM)
internal/sessions/            transcript vault: list/show/export/search/rehydrate
internal/spin/                harness core: client, runner, tools, ledger, recorder
internal/auth/keys.go         multi-tenant token registry
internal/config/config.go     env + .env loading, validation, paths
```

---

## 3. Provider Abstraction

Every transport implements `providers.Provider`:

```go
Name() string; ModelID() string
Init(page *rod.Page, cfg *config.Config) error        // page is nil on webapi
SendMessage(text, threadID string) (*models.ProviderResponse, error)
NewChat() error; ExtractResponse() (string, error); IsLoggedIn() (bool, error)
```

Optional capabilities are asserted, never required:

- `providers.Browserless` — exempts the transport from the DOM send-button
  prompt-length guard (`MAX_PROMPT_CHARS`).
- `providers.QuotaReporter` — surfaces a cached vendor quota on
  `/v1/health/providers` (Grok).
- `interface{ SessionExpiry() (time.Time, bool) }` — drives the expiry warning.

### Browserless transports (`internal/providers/webapi/<vendor>`)

Each vendor package speaks its own wire protocol and registers itself via
`webapi.Register(name, ctor)` in `init()`; `cmd/chimera` blank-imports
`webapi/all`, so adding a vendor never touches the API layer.

| Provider | Credential (auth_data) | Anti-bot gate | Continuity |
|---|---|---|---|
| chatgpt | cookie jar, bearer minted per session | SHA3-512 Sentinel PoW + requirements token | `conversation_id` + parent message id |
| deepseek | `userToken` bearer | Keccak-256 `DeepSeekHashV1` | chat session id + parent id (self-heals `biz_code 26`) |
| kimi | rotated refresh JWT (kimi.ai) | none — Connect framing only | native `chat_id` + `parent_id` |
| glm | rotated refresh token (chatglm.cn) | MD5 header signature (static public secret) | native server-side `conversation_id` |
| gemini | cookie jar (1PSID + rotating 1PSIDTS) | `SNlM0e` nonce + SAPISIDHASH | three-id continuation triple |
| grok | `sso` cookie JWT | statsig-id fallback header | transcript folding (model side is stateless) |
| qwen | JWT + refresh cookie | request signing (completions captcha-blocked — see README) | chat id + parent id |

All conversation maps are bounded (`webapi.ConvStore`, FIFO eviction) and every
transport self-heals a stale continuation by retrying once on a fresh thread.

### DOM transports (`internal/providers/<vendor>`)

Selectors live in per-provider `selectors.go` as ordered fallback lists; the
first hit wins inside `SELECTOR_TIMEOUT`. When a vendor ships a UI redesign,
only that file changes. Stop-detection is stop-button lifecycle, Claude's
`data-is-streaming` attribute, or text-stability polling (4×500 ms) as a
fallback. Echo detection re-extracts after 3 s when a response repeats the
prompt. Every first-choice miss increments `chimera_selector_fallback_total`.

---

## 4. OpenAI Compatibility Layer

- **Endpoints**: `POST /v1/chat/completions`, `POST /v1/responses`,
  `GET /v1/models`, `POST|GET /v1/refresh` (reset upstream threads),
  `GET /v1/usage` (per-tenant metering), `GET /v1/health/providers`
  (login + quota snapshot), `GET /metrics` (Prometheus), `/health`.
- **Auth**: `Authorization: Bearer`, `x-api-key`, or `anthropic-api-key`;
  single `API_TOKEN` or multi-tenant `API_TOKENS=tenant=token,…`. Empty
  disables auth. Monthly quotas return a real `429 quota_exceeded`.
- **Tool calling**: no native function-calling on these surfaces — schemas are
  injected into the prompt, the brace-depth parser extracts
  `{"tool_calls":[...]}` from code fences or bare JSON, names are validated
  against the requested defs (hallucinations are dropped and counted), and IDs
  are `call_<hex>`. Tool results fold into the next turn.
- **Stateful delta routing**: a stateless client that re-sends growing history
  is recognised by a stable digest of the conversation opening; when the
  upstream conversation already holds that prefix, only the delta is sent.
  `X-Session-Id` names a conversation explicitly and wins over inference.
- **Streaming**: SSE with `chat.completion.chunk` deltas, tool-call deltas when
  calls are parsed, and a `[DONE]` terminator. Chunks are emitted rune-safe so
  multi-byte UTF-8 is never split across events.
- **Pooled mode** (`PROVIDER=all`): one gateway routes by `chimera-*` model id;
  per-provider mutexes serialize turns per vendor (their accounts are
  single-stream by design) while distinct providers run in parallel.
- **MCP stdio mode** (`chimera --mcp`): the same providers served as Model
  Context Protocol tools (`chat`, `refresh`, `providers`) over JSON-RPC 2.0;
  stdout carries protocol bytes only, and stdout purity is enforced by test.

---

## 5. Browser Automation Hardening (DOM substrate)

| Technique | Implementation |
|---|---|
| Persistent profile | `launcher.UserDataDir(browser_data/<provider>)` |
| Singleton lock cleanup | stale lock/WAL/journal files removed on launch |
| Viewport jitter | `1280±20 × 720±20` per launch |
| Webdriver mask | `navigator.webdriver=false`, plugins/languages/WebGL spoofing |
| Headful default | `HEADLESS=false` (anti-bot); headless opt-in |
| Human pacing | typing 30–120 ms/char, thinking pauses 400–1500 ms, paste via `InsertText` |
| Selector fallbacks | ordered lists per element, first hit wins, misses counted |
| DNS hardening | Docker entrypoint pre-resolves vendor domains + host-resolver-rules |

---

## 6. Configuration

All via env + `.env` (godotenv). See [`.env.example`](.env.example) for the
annotated set. The ones you meet first:

| Variable | Default | Purpose |
|---|---|---|
| `PROVIDER` | `chatgpt` | `chatgpt` `claude` `qwen` `deepseek` `kimi` `gemini` `grok` `glm` `all` |
| `TRANSPORT` | `auto` | `auto` `dom` `webapi` |
| `TRANSPORT_FALLBACK` | empty | `dom` keeps a warm Chromium fallback beside webapi |
| `AUTH_DIR` | `./auth_data` | storage-state sessions (0600, password-equivalent) |
| `API_PORT` / `API_TOKEN(S)` | `8000` / off | listen port, single or multi-tenant auth |
| `RESPONSE_TIMEOUT` | `120s` | per-turn budget |
| `MAX_PROMPT_CHARS` | `12000` | DOM pre-flight guard (browserless exempt) |
| `METER_DB` / `QUOTA_MONTHLY_REQUESTS` | on / `0` | SQLite ledger + monthly quota |
| `SESSIONS_DIR` | `~/.local/share/chimera/sessions` | local transcript vault |

---

## 7. Deployment Topologies

### Local binary

```
chimera auth login chatgpt     # one browser login, session stored 0600
chimera up                     # every backend with a session, :8000–8011
```

### Docker

Two image targets from one `Dockerfile`:

- **`--target slim`** — `distroless/static-debian12:nonroot`, ~22 MB,
  `TRANSPORT=webapi`, `/app/auth_data` + `/app/logs` volumes, built-in
  `-healthcheck`. No Chromium, no shell.
- **`browser`** (default) — Debian + Chromium + Xvfb + x11vnc + noVNC +
  supervisord for the DOM substrate and one-time VNC login
  (`docker compose up`, then `http://localhost:6080/vnc.html`).

### Kubernetes (future)

Single replica per provider; sessions are stateful, so scale by adding
gateways per account, not replicas per account. Readiness on `/health`,
PVC for `auth_data` and `browser_data`.

---

## 8. Known limitations

- Browserless turns ~1–2 s p50; DOM turns 10–20 s (real page round-trip).
- Sessions expire on the vendor's clock (days–weeks observed); re-login is
  interactive by design — Chimera never automates human captchas.
- Per-provider serialization caps throughput at one active turn per account.
- Selectors rot on the DOM path; that is measurable
  (`chimera_selector_fallback_total`) and fixed in `selectors.go`, not invisible.
- Tool calling is prompt engineering: reliable for a handful of tools, not
  arbitrary schemas. Vision and file attachments are scaffolded, not wired.
