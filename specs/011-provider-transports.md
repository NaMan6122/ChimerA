# 011 — Provider transports: AuthSession × Transport

**Problem:** `providers.Provider` assumes a browser. `Init(page *rod.Page, …)`,
DOM-shaped `ExtractResponse()`, and stateful tab sessions via `NewChat()` (ADR-001
§7) make Chromium the only way to reach a subscription. Every request pays the DOM
round trip (10–20 s measured, ADR-001 §2) and every tenant ~1.3 GB of Chromium
(§2.3). It also blocks both paths ADR-001 §6 asks for: BYOK HTTP providers and
browserless replay of a provider's own web API.

**Goal:** split authentication from transport. A provider is an `AuthSession`
(session custody) composed with a `Transport` (request/response mechanics). The DOM
transport stays as the universal fallback; web-API, official-CLI, and HTTP-API
transports become peers selected per provider/model/tenant.

## Design

### 1. Interfaces (`internal/providers`)

```go
// AuthSession supplies credential material and can re-mint derived stamps.
type AuthSession interface {
	Name() string
	// Headers returns per-request credential material (cookies, bearer,
	// anti-bot stamps). Called per request; implementations may regenerate.
	Headers(ctx context.Context) (http.Header, error)
	// Refresh re-mints derived material (browser stamps, anti-bot tokens).
	Refresh(ctx context.Context) error
	// Valid reports whether the session can currently serve requests.
	Valid() bool
}

// Transport performs one chat turn, streaming text deltas.
type Transport interface {
	Name() string
	SupportsTools() bool
	// Create starts a conversation (or reuses one when conversationID != "").
	Create(ctx context.Context, model string) (conversationID string, err error)
	// Send streams one turn on a conversation.
	Send(ctx context.Context, conversationID, parentID, prompt string) (<-chan Delta, error)
}
```

`Provider` becomes `NewProvider(cfg, auth, transport)`. Existing DOM clients are
wrapped by `internal/providers/dom` (`domTransport` + `browserSession`), so the API
layer, `X-Session-Id` continuity, and metering keep working unchanged.

### 2. AuthSession kinds

| kind | custody | refresh |
|---|---|---|
| `browser` | rod profile (`BROWSER_DATA_DIR/<provider>`) | existing `POST /v1/refresh` |
| `storage-state` | imported cookies + localStorage JSON | re-import or OAuth refresh |
| `oauth-device` | vendor device flow | refresh_token |
| `api-key` | `*_API_KEY` env | none |

Storage state is password-equivalent: files live under `AUTH_DIR` (default
`./auth_data`, mode 0600), are never logged, and are isolated per tenant.

### 3. WebAPI transport (qwen first)

`chat.qwen.ai` web API, no Chromium (protocol verified against qwen-reverse 0.1.6,
MIT — see `scripts/qwenweb-spike/` for the spike implementation):

- `POST /api/v2/chats/new` → `data.id`
- `POST /api/v2/chat/completions?chat_id=…` — SSE; `version: 2.1`,
  `incremental_output: true`; `feature_config.thinking_enabled` controls reasoning
- anti-bot material is generated client-side, pure Go stdlib:
  - `ssxmod_itna` / `ssxmod_itna2`: fingerprint + LZW + custom base64 (`cookies.go`)
  - `bx-ua`: AES-CBC(SHA-256(fingerprint)[:32]) over a fingerprint payload (`bxua.go`)
  - `bx-umidtoken`: `GET sg-wum.alibaba.com/w/wu.json` + regex extract
  - headers `source: web`, `version`, `bx-v`, `X-Requested-With`, `x-request-id`
- auth: `Authorization: Bearer <access_token>` from storage state; `chat_mode` is
  `normal` when a token is present, `guest` otherwise.

Adding a provider becomes one `*webapi/` package plus recorded-SSE fixture tests —
no selector maintenance, no browser.

### 4. Fallback policy

`TRANSPORT=auto|dom|webapi|http` (default `auto`): prefer `webapi` when a valid
session exists; on `WAF blocked` / 401 / 429 retry once, then fall back to `dom`
and increment `chimera_transport_fallback_total{provider,from,to,reason}`. A
conversation never switches transport mid-stream (continuity lives on one tab or
one chat_id).

### 5. Observability and security

- `chimera_response_duration_seconds` gains a `transport` label; `/v1/health/providers`
  reports session kind + validity, never material.
- Imported sessions grant full account access; document alongside BYOA terms in
  `docs/SAAS.md`. Managed tenants get an isolated `AUTH_DIR` volume.
- ToS posture is unchanged (ADR-001 §4): a transport change buys latency and tenant
  density, not legality. Vendor enforcement applies to automated access regardless
  of transport.

## Spike evidence (2026-09-13)

`scripts/qwenweb-spike/` implements this transport for qwen (pure Go stdlib) and
benchmarks it against the live DOM gateway on the same machine/account:

| prompt | DOM (browser) | WebAPI | delta |
|---|---|---|---|
| PONG, no thinking | ~10–14 s | **2.26 s p50** (TTFT 1.30 s) | ~5x |
| 6k chars, thinking | 15.02 s | **5.62 s p50** | ~2.7x |
| 30k chars, thinking | 14.7 s p50 (anv round 1) | **5.97 s p50** | ~2.5x |

- **Resources:** spike process **17.9 MB RSS** vs the live Chromium tree **1150 MB**
  (10 processes) for the same provider/account (~64x).
- **Session custody:** `access_token` alone is rejected (`FAIL_SYS_USER_VALIDATE`
  WAF challenge). Replaying the browser's cookie jar (`acw_tc`, `atpsida`, `cna`,
  `ssxmod_itna/itna2`, `token`) is mandatory — the browser earns WAF trust and the
  transport borrows it. The token is a JWT with ~30-day expiry.
- **Guest mode is challenged** on completions (`challenges/new` passes, the
  completions call does not), so the transport requires a logged-in storage state.
- **Prefix caching is exposed by the web API** (`cached_tokens` up to 6912/7227
  = 96% on repeated 30k prompts). The DOM path has no equivalent (ADR-001 §2.4),
  so agentic loops get cheaper per round, not just faster.
- **No TLS impersonation needed:** plain `net/http` was accepted once the WAF
  cookies were replayed; `bx-ua`/`bx-umidtoken` are generated in Go.
- **Caveats:** the spike sends a single user message (no tool prompting yet, which
  `qwen-reverse` demonstrates is possible on this API); rate limits, cookie
  lifetime, and WAF drift are unmeasured; ToS exposure is unchanged (ADR-001 §4).

## Files

- `scripts/qwenweb-spike/{main.go,cookies_test.go,cdp-export.mjs}` — spike +
  CDP session export (evidence for this spec; not shipped in the binary)

- `internal/providers/transport.go` — interfaces, `Delta`, fallback wrapper
- `internal/providers/dom/…` — wrap existing clients (mechanical)
- `internal/authsession/{browser,storage,oauth,apikey}.go`
- `internal/providers/webapi/qwen/{client,cookies,bxua,fingerprint}.go` + tests
  (recorded SSE fixtures, LZW golden vectors)
- `internal/config/config.go` — `TRANSPORT`, `AUTH_DIR` (`getEnv*` pattern)
- `internal/telemetry` — `chimera_transport_fallback_total`, `transport` label
- `cmd/chimera/main.go` — wire `auto` selection
- `.env.example` — new rows

## Acceptance

- `go vet ./... && go test ./...` green, no browser in CI (recorded fixtures).
- qwen WebAPI transport returns the same text as the DOM transport for a fixed
  non-thinking prompt.
- Browserless qwen round trip p50 < DOM p50 for the same prompt; no Chromium
  process for a `TRANSPORT=webapi` run.
- WAF/auth failure after one retry falls back to DOM; metric increments.
- `PROVIDER=qwen TRANSPORT=webapi` serves `/v1/chat/completions` end to end.

## Out of scope

- ChatGPT/Claude WebAPI transports (Sentinel/Turnstile VM; Anthropic enforcement —
  keep DOM).
- Official-CLI transport (spec 012 pending) and managed-fleet backend.
