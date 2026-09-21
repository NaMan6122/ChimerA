# 012 — WebAPI transports: ChatGPT + DeepSeek (browserless replay)

**Problem:** Spec 011 built the browserless transport pattern for one provider and
left ChatGPT/DeepSeek on the DOM path, on the stated ground that they need
"Sentinel/Turnstile VM" and a "PoW wasm" (011 §Out of scope; ADR-002 §4.1, §6).
That reason does not survive inspection: both anti-bot gates are **brute-force
searches over a known hash**, not VM-bound computation, and Turnstile is advisory
for a session that already holds a browser-earned cookie jar. Meanwhile the DOM
path for these two carries everything ADR-002 measured as the cost of the browser:
~1.3 GB/tenant, ~10 s p50, and a per-provider mutex that makes concurrency
useless (Mind: throughput capped one request per session).

Keeping them browser-only is therefore a choice we are paying ~64x memory and
~3x latency for, to avoid writing two small client packages.

**Goal:** extend the spec-011 transport to ChatGPT and DeepSeek in the same shape
as qwen — Chromium earns trust once (login + cookie jar), then plain `net/http`
replays the wire protocol, with the DOM transport kept as the universal fallback.
No browser at inference time.

## Evidence grade

This spec is written from **protocol research, not a live session**. Claims below
are graded, and the first implementation task is to upgrade the `researched` rows
to `verified` against a captured session.

| | Grade |
|---|---|
| DeepSeek endpoints, headers, DeepSeekHashV1 formula | researched (multi-source) |
| ChatGPT endpoints, Sentinel two-step, FNV-1a PoW | researched (multi-source, >=1 working impl) |
| Turnstile advisory for trusted sessions | researched (single strong source) |
| Keccak-256 is the required primitive (not SHA3-256) | **verified locally** (see §2.1) |
| DeepSeekHashV1 + Sentinel PoW solvers | **implemented + unit-tested** against externally computed vectors (`pow_test.go`, `sentinel_test.go`) |
| Exact bodies/headers on a live account | **unverified** — needs capture; run `scripts/webapi-spike/cdp-capture.mjs` |

## Design

### 1. Generalize the transport seam (prerequisite)

The seam is currently single-vendor and must be lifted before a second provider
can exist. In `internal/config/config.go`:

```go
// today
func (c *Config) UseWebAPI() bool { if c.Provider != ProviderQwen { return false } ... }
func (c *Config) QwenSessionPath() string { return filepath.Join(c.AuthDir, "qwen.json") }

// 012
var webAPIProviders = map[string]bool{
    ProviderQwen: true, ProviderDeepSeek: true, ProviderChatGPT: true,
}
func (c *Config) UseWebAPI() bool     // switch on c.Transport, keyed by webAPIProviders
func (c *Config) SessionPath(provider string) string  // AuthDir/<provider>.json
```

A small registry replaces the direct `qwenweb.New(cfg)` call in `cmd/chimera/main.go`:

```go
// internal/providers/webapi/webapi.go
type Constructor func(*config.Config) providers.Provider
func Register(name string, c Constructor)
func Lookup(name string) (Constructor, bool)
```

Each vendor package registers itself from an `init()`. `main.go` then does
`webapi.Lookup(cfg.Provider)` instead of naming qwen. Shared error classification
(`ErrWAF`, `HTTPError`, `Retryable`, `Reason`) moves from `webapi/qwen` to
`webapi` so `main.go`'s `webAPIRetryable`/`webAPIReason` stop importing qwen.

Shared `Session` (`access_token` + `cookies`) and its load/write/status/JWT-expiry
helpers move from `webapi/qwen/session.go` to `webapi/session.go`; qwen imports
them. This is mechanical and verified by the existing qwen tests continuing green.

### 2. DeepSeek (`internal/providers/webapi/deepseek`) — **shipped, live-verified**

Endpoints under `https://chat.deepseek.com/api/v0/`:

- `POST /chat_session/create` `{}` → id at **`data.biz_data.chat_session.id`**
  (not `data.biz_data.id`, as research claimed)
- `POST /chat/create_pow_challenge` `{"target_path":"/api/v0/chat/completion"}`
  → `data.biz_data.challenge` = `{algorithm, challenge, salt, expire_at,
  difficulty, signature, target_path}`
- `POST /chat/completion` — SSE (see below); body
  `{chat_session_id, parent_message_id, model_type, prompt, ref_file_ids,
  thinking_enabled, search_enabled, action, preempt}`

Auth: `Authorization: Bearer <userToken>` from the browser's
`localStorage.userToken` (a versioned wrapper — see §4). Headers, **as captured**:
`x-ds-pow-response` (base64 JSON, §2.1), `x-client-bundle-id: com.deepseek.chat`,
`x-client-platform: web`, `x-client-version: 2.5.0`, `x-client-locale: en_US`,
`x-client-timezone-offset: <seconds east of UTC>`, `x-device-id: <uuid>`,
`x-device-model: ""`, `Accept-Language: en`, `origin`/`referer`.
**There is no `x-app-version`** on this surface (research claimed one).

`model_type` is `"thinking"` for the reasoner and `"default"` otherwise.

Model ids are client-side names (`deepseek-chat`, `deepseek-reasoner`); there is
no models endpoint, so `ListModels` is served from config.

#### 2.1 Proof of work — DeepSeekHashV1 (non-standard; no dependency needed)

```
find nonce in [0, difficulty) such that
    DeepSeekHashV1( "{salt}_{expire_at}_{nonce}" ) == challenge
```

`x-ds-pow-response` = base64(JSON{algorithm, challenge, salt, answer=nonce,
signature, target_path}) — envelope confirmed against a live capture (368 chars,
exactly those keys). Difficulty 144,000 → ~72k hashes, well under 100 ms in Go.
`expire_at` is **milliseconds** (13 digits), used verbatim with no unit conversion.

**DeepSeekHashV1 is neither Keccak-256 nor SHA3-256, and no standard library
provides it.** It is Keccak-f[1600] with *two* deviations:

| | padding | rounds |
|---|---|---|
| legacy Keccak-256 (`x/crypto`) | `0x01` | 24 |
| FIPS SHA3-256 (`crypto/sha3`) | `0x06` | 24 |
| **DeepSeekHashV1** | **`0x06`** | **23** (`RC[1]`…`RC[23]`) |

Either deviation alone changes the digest, which is why the first implementation
attempt failed against the live server with *no nonce found* — a wrong hash looks
exactly like an unsolvable challenge. The construction was read from aiodeepseek's
`_pow.cpp` ("23-round variant" in its docs is literal) and then **confirmed
against a live challenge the server accepted** (nonce 20941; vector in
`pow_test.go`).

**Dependency decision (revised):** the permutation is vendored in pure Go
(`keccak.go`, no imports). The `golang.org/x/crypto` dependency added by the first
attempt was **removed** — it could not have worked, since `NewLegacyKeccak256`
uses `0x01` padding. This satisfies `specs/README.md`'s "pure-Go deps only" rule
by needing no new dependency at all.

#### 2.2 Completion stream is a JSON-patch protocol, not OpenAI SSE

Verified live; fixtures in `testdata/completion_sse.txt` (non-thinking) and
`testdata/completion_thinking_sse.txt` (thinking):

```
event: ready
data: {"request_message_id":1,"response_message_id":2}
data: {"v":{"response":{…,"fragments":[{"type":"THINK","content":"We"}]}}}
data: {"p":"response/fragments/-1/content","o":"APPEND","v":" need"}
data: {"v":" answer"}                              ← bare continuation
data: {"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"P"}]}
data: {"p":"response/fragments/-1/content","v":"ONG"}
data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":65}]}
data: {"p":"response/status","o":"SET","v":"FINISHED"}
```

Three traps that each silently truncate the answer rather than erroring:

1. **Bare `{"v":…}` lines** continue the last announced path and carry *most* of
   the text — dropping them yields partial output (we saw `"The userONG"`).
2. **Reasoning fragments have type `THINK`**, not `THINKING`; the snapshot's
   fragment type switches the destination for the APPENDs that follow.
3. **New fragments arrive as `{"p":"response/fragments","o":"APPEND","v":[…]}`**
   (an array), which is also what ends thinking and starts the answer. The
   `…/content` patch sometimes omits `"o"` entirely, so match on path, not op.
4. An SSE `event:` type is scoped to one event block; it must be reset on the
   blank line or `event: ready` leaks onto the snapshot and swallows the first
   fragment.

Usage comes from the response-level BATCH (`accumulated_token_usage`), and
completion is signalled by `quasi_status`/`status` → `FINISHED`.

### 3. ChatGPT (`internal/providers/webapi/chatgpt`)

Auth: `__Secure-next-auth.session-token` cookie → `GET /api/auth/session` →
short-lived bearer (refreshable without a browser).

Sentinel handshake per turn:

- `POST /backend-api/sentinel/chat-requirements/prepare` → `{token, proofofwork
  {required, seed, difficulty}, turnstile {required, dx}, so {required, collect}}`
- solve PoW (§3.1); `POST /backend-api/sentinel/chat-requirements` → final
  `chat-requirements-token`
- `POST /backend-api/conversation` — SSE with `delta_encoding: v1` patch stream
  (`{"p":"…","o":"append","v":"…"}` then `{"v":"…"}`), body carries `action`,
  `model`, `messages`, `parent_message_id`, `conversation_id`.
- `POST /backend-api/f/conversation/prepare` supplies `x-conduit-token` (warm-up).

Headers: `openai-sentinel-chat-requirements-token`, `openai-sentinel-proof-token`,
`oai-device-id`, `oai-client-version`, `oai-language`, browser UA.

#### 3.1 Proof of work — Sentinel

```
i in [0, 500_000):  FNV-1a( seed ‖ byte(difficulty) ‖ be32(i) ‖ config ) < 1<<(32-difficulty)
token = "gAAAAAB" + base64(be32(result))
```

`config` is a client-constructed browser-fingerprint string (static, not read from
a live browser). Pure Go, ~15 lines; the reference is gpt4free's `sentinel.py`.

**Turnstile:** treated as advisory. `turnstile.required: true` does not block the
conversation endpoint when PoW + requirements-token are valid for a session whose
cookie jar was earned in Chromium. A per-account hard requirement is an accepted
failure mode that falls back to DOM (§5).

### 4. Auth: multi-vendor `chimera auth`

`cmd/chimera/auth.go` currently refuses anything but qwen. Generalize per
provider:

- `authLogin`: navigate the provider URL, poll that provider's token source
  (qwen `localStorage.token`; DeepSeek `localStorage.userToken`; ChatGPT
  `/api/auth/session` `accessToken`), collect cookies from the provider's domains.
- `authImport`/`authStatus`: shared, using `cfg.SessionPath(provider)`.
- Storage stays `AUTH_DIR/<provider>.json`, mode 0600 (password-equivalent).

### 5. Fallback (unchanged in shape)

`TRANSPORT=auto|dom|webapi` and `TRANSPORT_FALLBACK=dom` keep working. A provider
with no webapi package falls through to DOM (Claude, Kimi). Runtime fallback on
WAF/401/403/429/5xx after one retry, incrementing
`chimera_transport_fallback_total{provider,from,to,reason}`. New reason values:

- `pow_failed` — challenge fetched but the solver produced no accepted answer
- `session_expired` — bearer/cookie rejected and unrefreshable

### 6. Observability and security

- `chimera_pow_solve_seconds{provider}` histogram (the new compute path is the one
  thing that could silently regress latency).
- `/v1/health/providers` reports session kind + validity per provider; never material.
- Imported sessions grant full account access; same custody rules as qwen
  (0600, per-tenant `AUTH_DIR`, never logged).

## Files

Shipped:
- `internal/providers/webapi/webapi.go` — registry + shared error vocabulary
  (`ErrWAF`, `ErrPowFailed`, `ErrSessionExpired`, `HTTPError`, `Retryable`, `Reason`)
- `internal/providers/webapi/session.go` — shared session load/write/status/JWT expiry
- `internal/providers/webapi/all/all.go` — blank-import aggregator so adding a vendor
  never edits `main.go`
- `internal/providers/webapi/deepseek/{pow,client,provider}.go` + tests
- `internal/providers/webapi/chatgpt/{sentinel,util,client,provider}.go` + tests
- `scripts/webapi-spike/cdp-capture.mjs` — live header/session capture (§Risks)
- `internal/providers/webapi/qwen/{provider,client}.go` — consume the shared
  package via type aliases (`session.go` moved up a level)
- `internal/config/config.go` — `UseWebAPI`/`SessionPath(provider)`, `DeepSeekWeb*`,
  `ChatGPTWebModel`
- `cmd/chimera/main.go` — registry lookup; `webapi.Retryable`/`webapi.Reason`;
  provider-agnostic session-expiry logging
- `cmd/chimera/auth.go` — multi-provider `login|import|status`
- `internal/telemetry/telemetry.go` — `chimera_pow_solve_seconds`
- `go.mod` / `go.sum` — `golang.org/x/crypto`
- `.env.example`; `specs/README.md`; `docs/ADR-002-...md` §4.1

Verification so far: `go build ./... && go vet ./... && go test ./...` green, no
browser in CI. Unit tests cover both PoW solvers (external vectors), both SSE
assemblers, the Sentinel handshake, cookie→bearer refresh, WAF classification, and
conversation threading.

**Not yet done — needs a live session (see §Risks):**
- The wire format is exercised only against `httptest` fixtures. No real DeepSeek
  or ChatGPT turn has been run, because that requires a logged-in browser session
  for each account.
- `DefaultPoWConfig()` for ChatGPT is structurally plausible but unverified, and
  the `x-ds-pow-response` answer envelope is unconfirmed. Both are pinned in one
  place so a capture can correct them.
- `TRANSPORT_FALLBACK=dom` and the `auto`→webapi path are unchanged for qwen but
  have not been re-measured for the two new providers.

## Acceptance

- `go vet ./... && go test ./...` green; no browser in CI (recorded-SSE fixtures
  under `httptest`, per the qwen `client_test.go` pattern).
- DeepSeek PoW unit test solves a fixed `{salt, expire_at, difficulty, challenge}`
  vector; ChatGPT PoW unit test matches the reference FNV-1a vector.
- `PROVIDER=deepseek TRANSPORT=webapi` and `PROVIDER=chatgpt TRANSPORT=webapi`
  each serve `/v1/chat/completions` end to end.
- Same text as the DOM transport for a fixed non-thinking prompt, per provider.
- Browserless p50 < DOM p50 for the same prompt; no Chromium process for a
  `TRANSPORT=webapi` run.
- WAF/auth failure after one retry falls back to DOM; metric increments with a
  §5 reason.
- `chimera auth login|status` work for qwen, deepseek, and chatgpt; session files
  land at `AUTH_DIR/<provider>.json` mode 0600.
- Adding a fourth provider requires **one new package + one config row + one
  `.env.example` line** — no `main.go` change.

## Out of scope

- Claude and Kimi webapi transports (Anthropic enforcement; Kimi is cookie-auth
  but unmeasured — candidate for 013).
- The official-API BYOK tier (ADR-001 §6): stateless vendor APIs keyed by
  `*_API_KEY`. Still the other half of the escape hatch, and strictly simpler to
  build than this spec; not bundled here.
- Native tool/function-calling on the webapi transports. Tool calling stays
  prompt-injected via the existing `tools.BuildToolPrompt` path.

## Risks / unknowns

1. **CDN bot gate on DeepSeek.** ADR-002 §6 says "AWS WAF token"; secondary
   research says Cloudflare `cf_clearance` with `curl_cffi` impersonation. Either
   way a browser-earned cookie jar is the first mitigation; if plain `net/http` is
   challenged and the jar is insufficient, the options are (a) a TLS-impersonating
   client (new dependency, needs justification), or (b) keep DOM. **Resolve with a
   live capture before committing to (a).**
2. **ChatGPT `so.collect`** — an 18-element browser-fingerprint prekey whose exact
   shape is undocumented. Its absence may or may not be penalised.
3. **Header/protocol drift** — `oai-website-version` changed mid-research;
   `delta_encoding: v1` and the SSE shape are versioned; servers advertise
   `resume_with_websockets: true`, so a WS migration is plausible.
4. **Turnstile per account tier** — free/Plus/Pro may enforce differently.
5. **Rate limits** — per-account quotas and Sentinel-level throttling unknown.
6. **Legal/ToS unchanged** (ADR-001 §4, ADR-002 §3). A cheaper transport is not a
   permitted one; enforcement risk is identical and remains the top existential risk.
