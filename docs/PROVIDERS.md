# Provider Guide

Eight providers, two substrates. Browserless (`webapi`) replays the vendor's
HTTP API from an exported session — no browser, ~24 MB, ~1–2 s turns. DOM
drives the real vendor UI in Chromium — the universal fallback and the only
path for `claude` today. All providers implement `providers.Provider`.

| Provider | Model id | DOM | Browserless | Credential |
|---|---|---|---|---|
| ChatGPT | `chimera-chatgpt` | ✅ | ✅ | cookie jar (bearer minted per session) |
| Claude | `chimera-claude` | ✅ | — | browser profile |
| Qwen | `chimera-qwen` | ✅ | ⚠️ captcha-blocked completions | JWT + refresh cookie |
| DeepSeek | `chimera-deepseek` | ✅ | ✅ | `userToken` bearer |
| Kimi | `chimera-kimi` | ✅ | ✅ | rotated refresh JWT (kimi.ai) |
| Gemini | `chimera-gemini` | — | ✅ | cookie jar (1PSID + rotating 1PSIDTS) |
| Grok | `chimera-grok` | — | ✅ | `sso` cookie JWT |
| GLM (Zhipu) | `chimera-glm` | — | ✅ | rotated refresh token |

---

## Shared machinery

- `internal/providers/webapi/session.go` — storage-state sessions
  (`auth_data/<provider>.json`, mode 0600, written atomically). Sessions are
  password-equivalent; `webapi.LoadSession` rejects token-only jars that WAFs
  would reject anyway.
- `internal/providers/webapi/webapi.go` — the transport registry. Vendor
  packages call `webapi.Register(name, ctor)` from `init()`; `cmd/chimera`
  blank-imports `webapi/all`, so a new transport needs **no** API-layer change.
  The registry also defines the shared error vocabulary (`ErrWAF`,
  `ErrSessionExpired`, `HTTPError`) that drives retry/fallback decisions.
- `internal/providers/webapi/convs.go` — bounded thread→conversation caches
  (FIFO eviction at 512) shared by every browserless transport.
- `internal/providers/base.go` — the DOM toolkit: `FindElement` fallback
  lists, stop-button / text-stability response waits, echo detection, clipboard
  extraction.
- `internal/providers/fallback.go` — `Fallback` wraps a primary and secondary
  transport: one retry, then the secondary, with `chimera_transport_fallback_total`
  counting every downgrade.

Every browserless transport self-heals a stale continuation the same way:
when the vendor rejects the parent pointer, the turn retries once on a fresh
conversation. Stale sessions surface as `ErrSessionExpired`, which maps to a
re-export message (`chimera auth login <provider>`), never a mystery 500.

---

## Browserless transports (what each one actually replays)

| Provider | Gate cleared in Go | Turn endpoint | Continuity |
|---|---|---|---|
| ChatGPT | SHA3-512 Sentinel PoW + locally minted requirements token | `backend-api` conversation SSE | `conversation_id` + parent message id |
| DeepSeek | Keccak-256 `DeepSeekHashV1` challenge | JSON-patch SSE (`fragments` snapshots + appends) | chat session + parent id (`biz_code 26` self-heal) |
| Kimi | none — Connect-RPC framing (`application/connect+json`, op-driven `set`/`append`, `{"done":{}}` terminator) | `kimi.gateway.chat.v1.ChatService/Chat` | native `chat_id` + `parent_id`; rotated refresh token persisted |
| GLM | MD5 header signature over digit-mangled timestamp + nonce | `assistant/stream` SSE (full `parts[]` snapshots) | native server-side `conversation_id`; orphaned conversations deleted off-path |
| Gemini | `SNlM0e` CSRF nonce + SAPISIDHASH + rotating `1PSIDTS` | `batchexecute` StreamGenerate (cumulative snapshots) | three-id continuation triple |
| Grok | `x-statsig-id` fallback header | `rest/app-chat/conversations/{new,respond}` | transcript folding (model side is stateless — measured, documented) |
| Qwen | request signing | completions | captcha-blocked by the vendor's risk engine; DOM or official API |

Reasoning/thinking streams (DeepSeek THINK blocks, Kimi `block.think`, GLM
`think` parts) are parsed and counted, then discarded at the provider boundary
— vendors do not replay them, so they never enter any working copy.

---

## DOM transports

Each DOM provider defines ordered fallback selector lists in `selectors.go`
(first hit wins inside `SELECTOR_TIMEOUT`):

| Provider | Input | Send | Completion detector |
|---|---|---|---|
| ChatGPT | `#prompt-textarea` (`div[contenteditable]` fallbacks) | `button[data-testid='send-button']` | stop-button lifecycle |
| Claude | `div.ProseMirror` | `button[aria-label='Send Message']` | `data-is-streaming` attribute |
| Qwen | `textarea[placeholder]` | `button[aria-label='发送']` + EN fallback | stop button → text stability |
| DeepSeek | `textarea` | `div[class*='send'] button` | stop button → text stability |
| Kimi | provider's own selectors | provider's send control | text stability |

A spike in `chimera_selector_fallback_total` means a vendor changed their DOM —
fix `selectors.go`, no logic rewrite.

---

## Adding a provider

### Browserless (preferred)

1. Create `internal/providers/webapi/<vendor>/` with `client.go` (wire protocol)
   and `provider.go` (implements `providers.Provider`, `Browserless() bool`,
   calls `webapi.Register` in `init()`).
2. Reuse `webapi.Session`, the shared error vocabulary, and `webapi.ConvStore`
   for conversation state. Keep the stale-continuation self-heal pattern.
3. Blank-import the package in `internal/providers/webapi/all/all.go`.
4. Add a `chimera auth login <provider>` spec in `cmd/chimera/auth.go` (cookie
   or `localStorage` harvest) — `authSpecs` and `webapi.Registered()` are
   asserted to match 1:1 by test, so forgetting either side fails CI.
5. Test against a hermetic `httptest` fake server: assert exact headers,
   content types, frame state machines, and error paths. No live credentials in
   CI, ever.

### DOM (fallback)

1. Copy the `qwen/` template: `client.go` + `selectors.go` with ordered
   fallback lists.
2. Register the provider in `internal/config` (`SupportedProviders`, URL field)
   and `cmd/chimera/main.go`'s `createProviderByName`.
3. Live-verify selectors against a logged-in profile; log with
   `LOG_LEVEL=debug` to watch waits and extraction.

Either way, run the full gate before pushing:

```bash
CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test -race ./...
```
