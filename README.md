# Chimera

**Your chat subscriptions, as an OpenAI-compatible API.**

Point any OpenAI SDK, LangChain chain, or agent harness at Chimera and your
ChatGPT / Claude / Qwen / DeepSeek / Kimi login answers over `/v1` — no API keys,
no per-token bill, and no prompts leaving the machine it runs on.

One Go binary. Five providers. Two ways to reach each of them: drive the real
browser where the vendor insists, or replay the vendor's own web API over HTTP
when a session can be exported — because a browser is an implementation detail,
not a business model.

```
Go 1.25  ·  ~21 MB static binary  ·  MIT  ·  self-host free, managed single-tenant
```

> **Product** — [PRD](docs/PRD.md) · [SaaS model](docs/SAAS.md) · [Website brief](docs/WEBSITE.md)
>
> **Operators** — [API](docs/API.md) · [Architecture](docs/ARCHITECTURE.md) · [Providers](docs/PROVIDERS.md) · [Observability](docs/OBSERVABILITY.md) · [Metering](docs/METERING.md) · [Distribution](docs/DISTRIBUTION.md)
>
> **Planned work** — [`specs/`](specs/): every transport and protocol lands as a numbered spec first.
>
> **Picking this up?** — [`docs/HANDOFF.md`](docs/HANDOFF.md): current state, invariants, open work.

---

## Install

Prebuilt binaries, three per archive (`chimera` gateway, `chimera-spin`
harness, `chimera-chat` TUI) — macOS and Linux, amd64 and arm64:

```bash
# curl — installs to ~/.local/bin, sha256-verified against the release
curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh | bash

# Homebrew
brew tap NaMan6122/chimera
brew install chimera

# from source — same install layout as the installer
git clone https://github.com/NaMan6122/ChimerA.git && cd ChimerA && ./build.sh
```

Releases are cut by pushing a `v*` tag; GitHub Actions cross-compiles every
platform, publishes the archives with checksums and a generated Homebrew
formula. The runbook and the tap setup live in [`docs/DISTRIBUTION.md`](docs/DISTRIBUTION.md).

---

## The two bills problem

You already pay $20/month for a chat subscription. Then you pay again, per token,
to use the same model in code. Two bills, one brain.

Meanwhile the models you actually want to reach — Qwen, Kimi — either have thin
developer APIs or none in your region, and the browser-automation gateways that
solve this are 650 MB Python images with 900 ms cold starts, no metering, no
metrics, and no story for more than one user.

Chimera is the other version of that:

- **Zero token spend.** The subscription you already pay for becomes the backend.
- **Models without APIs.** One OpenAI shape in front of five consumer surfaces.
- **Private by construction.** Prompts stay in your profile/session, not our cloud.
- **Operable as a product.** Quotas, usage ledgers, login health, and a selector-fallback radar that turns vendor UI drift into an alert instead of a mystery 500.
- **Agent-native.** OpenAI Chat Completions today; Responses, Anthropic Messages and MCP stdio specced.

---

## Two substrates, one API

The browser is the universal fallback, not the foundation.

```
   Your app — OpenAI SDK · LangChain · curl · an agent
                          │
                          ▼
        ┌─────────────────────────────────────┐
        │  Chimera gateway  :8000  (Go, chi)  │
        │  /v1/chat/completions · /v1/models  │
        │  auth · rate limit · quotas · SSE   │
        └───────────────┬─────────────────────┘
                        │
        ┌───────────────┴──────────────────┐
        ▼                                  ▼
  TRANSPORT=webapi                  TRANSPORT=dom
  replay the vendor's HTTP API      real Chromium (rod)
  cookies + generated anti-bot      stealth, human pacing,
  no browser process, ~24 MB        selector fallback, DOM scrape
        │                                  │
        └───────────────┬──────────────────┘
                        ▼
      chatgpt.com · chat.qwen.ai · chat.deepseek.com
      claude.ai · kimi.com
```

`TRANSPORT=auto` prefers the browserless path when an exported session exists,
falls back to the browser on WAF/auth failure, and counts every downgrade as
`chimera_transport_fallback_total`.

### What "browserless" actually means

Export a session once (`chimera auth login`) and the gateway replays the same
calls the web app makes, solving the anti-bot work itself:

| Vendor gate | How Chimera clears it |
|---|---|
| DeepSeek `DeepSeekHashV1` | Keccak-256 proof-of-work solved in-process |
| ChatGPT Sentinel | FNV-1a proof-of-work + locally minted requirements token, bearer re-minted from the cookie jar |
| Qwen request signing | generated in Go from the exported session |
| CDN / WAF | real browser-earned cookies replayed with the request |

That is why browserless turns land in **~1–2 seconds** instead of ~15, and why a
tenant costs **24 MB** instead of **1.1 GB**.

---

## What you get

### One endpoint, five subscriptions

`POST /v1/chat/completions` speaks the OpenAI dialect, including emulated SSE with
tool-call deltas. `PROVIDER=all` runs every provider behind one Chromium, one tab
each, routing by `chimera-*` model id. `X-Session-Id` gives you conversation
continuity; `POST /v1/refresh` recovers a stale thread without a restart.

### Tool calling without a tool-calling API

Browser UIs have no native function calling, so Chimera does it the honest way:
inject the signatures into the prompt, parse what comes back with a brace-depth
tracker that survives code fences and nested JSON, reject names that were never
offered, normalize malformed arguments, mint `call_<hex>` ids, and fold the tool
result back into the next turn. Reliable for 1–7 tools; hardening tracked in
[spec 004](specs/004-tool-call-hardening.md).

### Observable scraping, not hopeful scraping

`/metrics` exposes request outcomes, round-trip histograms, lock waits, error
reasons, echo retries, transport downgrades — and a **selector-fallback radar**
that fires the moment a vendor changes their DOM. `/v1/health/providers` probes
login state so a status page can tell the truth. JSON logs carry `req_id`. A
Grafana dashboard and alert rules ship in `docker/`.

### Billable out of the box

Per-tenant keys (`API_TOKENS`), a SQLite usage ledger, monthly quotas that return
a real `429 quota_exceeded`, and `GET /v1/usage` so a caller can see only their
own spend.

### Hardened where it hurts

Multi-key auth, CORS, request body caps, provider locks **with timeouts** instead
of infinite hangs, a pre-flight prompt-length guard, copy-button completion
detection, and echo recovery for the reply that parrots your prompt back.

---

## Measured, not asserted

Last measured on this repo's development machine (macOS arm64, `CGO_ENABLED=0`,
`TRANSPORT=webapi`, no Chromium running) with
[`scripts/latency_probe.py`](scripts/latency_probe.py). It scrapes `/metrics`
before and after each run, so gateway overhead and lock contention are separated
from model time rather than folded into one flattering number.

| Scenario | n | p50 | p95 | Notes |
|---|---|---|---|---|
| DeepSeek, 50-char prompt | 5 | **1.17 s** | **2.20 s** | streamed, sequential, warm conversation |
| DeepSeek, 6,323-char prompt | 3 | **1.92 s** | **2.76 s** | 1,581 prompt tokens; gateway overhead 0.01 s |
| Qwen / ChatGPT session replay | — | 3.9 s / 5.6 s | — | recorded in `logs/anv-webapi*` and `logs/chatgpt-web.log` before those sessions expired; re-login to re-measure |
| Browser (DOM) turn | — | 10–20 s | — | documented range; serialized by a real page |

Static cost, same run:

| | Browserless | Browser |
|---|---|---|
| Spawn → `/health` 200 | **298 ms** (includes live session check) | ~2.5 min (Xvfb + Chromium + page load) |
| Resident memory per tenant | **24.4 MB** | ~1.1 GB (10 processes) |
| Chromium processes | **0** | — |
| Binary | **21.4 MB** stripped | — |
| Concurrency | serialized per provider (see [known issues](#known-issues)) | serialized per provider |

Numbers are one machine, one account, one network. Treat them as an order of
magnitude and a method, not an SLA — re-run the probe for your own.

---

## Quick start

### 1. Browserless (no Chromium — recommended)

```bash
go build -o chimera ./cmd/chimera

./chimera auth login qwen      # opens a browser once; stores auth_data/qwen.json
./chimera auth status          # validity + expiry, never the secret material
TRANSPORT=webapi PROVIDER=qwen ./chimera
```

Serves in seconds, ~24 MB, no browser process. Works for `qwen`, `deepseek`,
`chatgpt`.

### 2. Local binary (headful — harder to detect)

```bash
cp .env.example .env           # set PROVIDER and API_TOKEN
go build -o chimera ./cmd/chimera
./chimera                      # opens Chromium, log in once, profile persists
```

Sessions live in `./browser_data/<provider>`. Headless is supported
(`HEADLESS=true`) and explicitly **not** recommended: it is easier to fingerprint.

### 3. Docker (browser path, VNC login, nothing to install)

```bash
cp .env.example .env
docker compose up --build -d
open http://localhost:6080/vnc.html   # one-time login
curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
```

### Point your app at it

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8000/v1", api_key="chimera")

resp = client.chat.completions.create(
    model="chimera-deepseek",
    messages=[{"role": "user", "content": "Explain quantum computing"}],
)
print(resp.choices[0].message.content)

# streaming is emulated, but the SDK cannot tell
for chunk in client.chat.completions.create(
    model="chimera-deepseek",
    messages=[{"role": "user", "content": "Write a story"}],
    stream=True,
):
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

Only `base_url` and `api_key` change. Everything downstream — LangChain,
opencode, your own agent loop — keeps working.

### Use it with agents

**As the model:** swap the base URL, and your subscription answers. Zero token spend.
**As a tool** ([spec 001](specs/001-mcp-stdio-mode.md)): keep your agent on API
credits and escalate the hard reasoning steps through MCP `chat` — or let five
subscriptions vote behind one tool call.

**As its own harness:** [`chimera-spin`](docs/SPIN.md) drives the gateway with a
six-tool loop that never stores a transcript — the provider already keeps the
conversation, so each turn sends only what is new (~1k chars instead of ~48k).

---

## API

All endpoints except `/health`, `/metrics` and `/` require
`Authorization: Bearer <token>` (or `x-api-key` / `anthropic-api-key`).

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | liveness, no auth |
| GET | `/metrics` | Prometheus exposition, no auth |
| GET | `/v1/models` | models this gateway serves |
| GET | `/v1/health/providers` | per-provider login/transport state |
| GET | `/v1/usage?from&to` | usage ledger, scoped to the calling tenant |
| POST | `/v1/chat/completions` | OpenAI chat, JSON or `stream:true` SSE, tools |
| POST | `/v1/responses` | Codex-style Responses API (`stream:true` not yet) |
| POST/GET | `/v1/refresh`, `/v1/new_chat` | reset the provider conversation |

Full request/response shapes: [`docs/API.md`](docs/API.md).

---

## Configuration

Everything is env or `.env`; see [`.env.example`](.env.example) for the annotated set.

| Variable | Default | Purpose |
|---|---|---|
| `PROVIDER` | `chatgpt` | one of `chatgpt`, `claude`, `qwen`, `deepseek`, `kimi`, `all` |
| `TRANSPORT` | `auto` | one of `auto`, `dom`, `webapi` |
| `TRANSPORT_FALLBACK` | — | `dom` keeps Chromium warm for webapi→dom fallback |
| `AUTH_DIR` | `./auth_data` | exported sessions, mode `0600`, password-equivalent |
| `BROWSER_DATA_DIR` | `./browser_data` | persistent Chromium profiles, per provider |
| `HEADLESS` | `false` | headless Chromium (less stealthy) |
| `CHATGPT_URL` / `CLAUDE_URL` / `QWEN_URL` / `DEEPSEEK_URL` / `KIMI_URL` | vendor URLs | override for proxies |
| `QWEN_WEB_MODEL` / `DEEPSEEK_WEB_MODEL` / `CHATGPT_WEB_MODEL` | `qwen3.8-max` / `deepseek-chat` / `auto` | browserless model selection |
| `RESPONSE_TIMEOUT` | `120000` ms | max wait for a turn |
| `SELECTOR_TIMEOUT` | `10000` ms | per-selector fallback budget |
| `MAX_PROMPT_CHARS` | `12000` | pre-flight cap; returns 400 instead of hanging |
| `API_HOST` / `API_PORT` | `0.0.0.0:8000` | listen address |
| `API_TOKEN` | empty (auth off) | single bearer token; `.env.example` uses `chimera` |
| `API_TOKENS` | — | multi-tenant `tenant=token,...` |
| `QUOTA_MONTHLY_REQUESTS` | `0` (unlimited) | per-tenant monthly cap |
| `METER_DB` | `./logs/usage.db` | SQLite ledger; empty disables metering |
| `RATE_LIMIT_SECONDS` | `2` | token bucket refill interval |
| `LOG_DIR` / `LOG_LEVEL` / `VERBOSE` | `./logs` `debug` `true` | file + stderr logs |
| `LOG_FORMAT` | `text` | `text` or `json` (Loki/ELK-friendly, includes `req_id`) |

---

## Provider matrix

| Provider | Model id | DOM | Browserless | State |
|---|---|---|---|---|
| ChatGPT | `chimera-chatgpt` | ✅ | ✅ cookie custody, Sentinel PoW | chat + tools; re-export session when `/api/auth/session` reports `RefreshAccessTokenError` |
| Claude | `chimera-claude` | ✅ | — | chat + tools, streaming via `data-is-streaming` |
| Qwen | `chimera-qwen` | ✅ | ⚠️ blocked (see below) | DOM works; browserless is WAF-rejected with Qwen's new short-lived token |
| DeepSeek | `chimera-deepseek` | ✅ | ✅ `DeepSeekHashV1` PoW | chat + text-stability fallback |
| Kimi | `chimera-kimi` | ✅ | — | chat + text-stability fallback |

Adding a provider: copy the `qwen/` DOM template, define ordered fallback
selectors, register it in `config` and `cmd/chimera/main.go`. To add a
browserless transport, implement `providers.Provider` and call `webapi.Register`
in an `init()` — no changes to the API layer. See [`docs/PROVIDERS.md`](docs/PROVIDERS.md).

---

## Architecture

```
cmd/chimera/            entry, `auth login|status|check|import`, transport selection, shutdown
internal/api/           chi router, auth, rate limit, quotas, SSE emulation, tool wiring
internal/providers/     Provider interface + Base (selector fallback, wait, extract)
  ├─ chatgpt/ claude/ qwen/ deepseek/ kimi/     DOM clients, one selectors.go each
  └─ webapi/            browserless transports, registry, PoW solvers, retry classification
internal/browser/       rod manager, multi-tab pool, stealth patches, human behaviour
internal/tools/         tool-prompt injection + brace-depth JSON parser
internal/session/       X-Session-Id continuity with LRU eviction
internal/meter/         SQLite usage ledger + monthly summaries
internal/telemetry/     Prometheus metrics, health state, selector radar
internal/config/        env loader, per-provider paths, transport rules
```

Deeper dive, including browser lifecycle, stealth stack, response detection and
the provider abstraction: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Self-host or managed

Chimera is MIT and free to run yourself. If you'd rather not own the toil,
managed single-tenant is the same binary with an isolated container, a private
VNC URL, and someone else watching the selectors.

| | Self-host | Managed single-tenant |
|---|---|---|
| Run | `docker compose up` | isolated container per tenant |
| Login | `:6080/vnc.html` | private VNC URL |
| Data | your disk | per-tenant volume |
| Updates | you pull | maintained selectors + uptime |

Proposed starter tiers, priced by substrate rather than per token —
**Subscriber API $9–12/mo** (browserless, 10k requests), **Browser Solo $19/mo**
(5k), **Team $79/mo** (auto transport with warm fallback, 50k), Scale custom.
Rationale and unit economics: [`docs/SAAS.md`](docs/SAAS.md),
[`ADR-002`](docs/ADR-002-browserless-tier-economics.md).

```bash
TENANT_ID=acme API_PORT=8101 VNC_PORT=5901 NOVNC_PORT=6081 ./scripts/new-tenant.sh
TENANT_ID=acme docker compose -f docker-compose.tenant.yml up -d --build
```

---

## Known issues

Documented because they are measured, not because they are ignored.

1. **Concurrent requests on one browserless conversation can poison it.**
   DeepSeek answers with `biz_code 26 "invalid message id"` and keeps doing so,
   because the parent message id is read before the provider lock is taken and a
   stale parent gets sent. Recover with `POST /v1/refresh`; avoid it by giving
   each concurrent client its own `X-Session-Id`.
2. **Provider failures after SSE headers are committed return HTTP 200** with an
   `{"error":…}` event inside the stream, so naive clients (and the latency
   probe) count a dead turn as success. Check the stream body, not just the status.
3. **DeepSeek rate-limits per account.** "Messages too frequent" arrives as an
   in-stream event rather than an HTTP error; it is now surfaced as one instead
   of a successful empty turn, but the limit itself is the vendor's to set.

4. **Qwen's browserless transport is blocked by a captcha on the completions
   endpoint.** Qwen now issues ~15-minute access tokens (refreshed from a
   `refresh_token` cookie against `auth.qwen.ai`), which the transport now
   handles automatically. After that fix the block narrowed to exactly one
   request: `POST /api/v2/chat/completions` answers `FAIL_SYS_USER_VALIDATE`
   with `action=captcha` and an `x5secdata` challenge, while `chats/new`,
   `models` and the token refresh all succeed. Reproduced identically from Go,
   Python and curl, so it is the session's risk state, not the HTTP client.

   The `x5sec` cookie that clears it is only minted after a human solves the
   slider in the browser. Solving that programmatically is captcha evasion,
   which this project does not do (PRD §9). Workarounds that stay inside the
   rules: complete the captcha once in the browser and re-export the session
   (`chimera auth login qwen` without `-fresh`), use the DOM transport (measured
   30–36 s per turn, tool calls included), or use Qwen's official API.

Fixed since this section was written: `chimera auth status` used to report a
Qwen token 18 days past its `exp` as healthy (the probe endpoint tolerates a
stale token); the `exp` claim is now checked first, and DeepSeek turn failures
self-heal by starting a fresh conversation.

Tracking and fixes live in [`specs/`](specs/).

---

## Roadmap

```
v0.4  protocols     tool-call hardening → Responses API → Anthropic Messages → MCP stdio
v0.5  reliability   cross-provider failover → selector packs → provider supervisor
v0.6  delight       response cache → playground → eval harness
v1.0                all green + managed pilot tenants
```

---

## FAQ

**Why is it slow?** It isn't, on the browserless path — ~1–2 s p50. The browser
path is 10–20 s because it drives a real page with human pacing. Either way this
is for reasoning steps and async agents, not autocomplete.

**Will my account get banned?** Browser automation can trigger CAPTCHAs, rate
limits or bans. Mitigations: headful mode, human pacing, persistent profiles,
and read-only use of your own account. Use accounts you can afford to verify.

**Why not just use the official APIs?** If per-token pricing works for you, use
it — it is faster and supported. Chimera exists to kill a second bill, reach
models with no API, and keep prompts in your own session.

**Does it do vision, files, image generation?** Multimodal parts are parsed but
only text is sent today. Uploads and DALL·E scraping are post-v1.

**How is this different from the Python Chimera-Gateway?** Same idea, Go runtime:
a ~21 MB static binary, ~24 MB browserless RAM, ~300 ms start, plus metering,
quotas and Prometheus observability the Python project does not ship.

---

## Limitations

- Browserless needs a valid exported session; they expire (13–18 days observed) and re-login is interactive.
- Per-provider serialization caps throughput; a multi-tab pool exists for the DOM path, not the browserless one.
- Selectors rot. That is measurable (`chimera_selector_fallback_total`) and fixable in `selectors.go`, not invisible.
- Tool calling is prompt engineering: reliable for a handful of tools, not arbitrary schemas.
- Vision, files and image generation are scaffolded, not wired.

---

## Disclaimer

Browser automation may violate vendor terms of service (OpenAI, Anthropic, and
others). Automated access to consumer subscriptions is prohibited regardless of
transport — a faster transport changes cost, not legality. Use your own accounts
for personal automation and evaluation; never share credentials or resell tokens.
Not affiliated with any vendor. One tenant, one account, one login.

## License

MIT. Inspired by [Chimera-Gateway](https://github.com/GautamVhavle/Chimera-Gateway) (MIT).
