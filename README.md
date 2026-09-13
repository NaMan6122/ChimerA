# Chimera — Browser-Based LLM Gateway (Go)

> **Your chat subscriptions, as an OpenAI-compatible API.** Bring your own ChatGPT / Claude / Qwen / DeepSeek / Kimi login — no API keys, session stays in your Chromium. Self-host MIT, or let us host your isolated browser.
>
> See [`docs/SAAS.md`](docs/SAAS.md) for open-source SaaS positioning, pricing, and single-tenant serving.

Port of [Chimera-Gateway](https://github.com/GautamVhavle/Chimera-Gateway) (Python + Patchright + FastAPI) to **Go + rod + chi** for a lightweight, static-binary, operationally cheap gateway.

> **Product docs:** [`docs/PRD.md`](docs/PRD.md) (vision, users, pricing, roadmap) ·
> [`docs/WEBSITE.md`](docs/WEBSITE.md) (website builder brief) ·
> [`docs/SAAS.md`](docs/SAAS.md) (self-host vs managed).
>
> **Operator docs:** [`docs/API.md`](docs/API.md) · [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) ·
> [`docs/PROVIDERS.md`](docs/PROVIDERS.md) · [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md) ·
> [`docs/METERING.md`](docs/METERING.md). Planned work lives in [`specs/`](specs/).

```
Your app (OpenAI SDK / LangChain / curl)
          │
          ▼
   Chimera :8000 (Go, chi, OpenAI-compatible)
          │
          ▼
   Real Chromium (rod, persistent profile, stealth, human typing)
          │
          ▼
chatgpt.com | claude.ai | chat.qwen.ai | chat.deepseek.com
```

---

## Why Go?

See [`docs/TECH_STACK_DECISION.md`](docs/TECH_STACK_DECISION.md) for full comparison.

**TL;DR**: Python's Chimera is great for velocity but ships a 650 MB image with 900 ms cold start. Chimera's Go port is **18 MB static binary, 18–30 MB gateway RAM, 60 ms cold start, 22k rps on /health, single-`go build` deploy** — while preserving Chimera's selector fallback, stop-button lifecycle, text stability, echo retry, and prompt-engineered tool calling.

|  | Python (Chimera) | Go (Chimera) |
|---|---|---|
| Gateway RAM (idle) | 85–120 MB | **18–30 MB** |
| Image (gateway + Chromium) | ~650 MB | **~380–450 MB** |
| Cold start | 900 ms | **60 ms** |
| Concurrency | asyncio + GIL | goroutines + per-provider locks (multi-tab pool) |

---

## Features

- **OpenAI-compatible gateway** — `POST /v1/chat/completions` (JSON + emulated SSE
  streaming), `GET /v1/models`, tool calling via prompt engineering, session
  continuity (`X-Session-Id`), `POST /v1/refresh` for stale-DOM recovery.
- **5 providers, one endpoint** — ChatGPT, Claude, Qwen, DeepSeek, Kimi; `PROVIDER=all`
  runs them in a single Chromium with model-based routing (`chimera-*` IDs).
- **Built for agents** — works as *the model* (`base_url` swap in any OpenAI client)
  or, once [`specs/001`](specs/001-mcp-stdio-mode.md) lands, as *a tool* (MCP `chat`
  inside Claude Code et al.). `/v1/responses` and Anthropic Messages shapes are specced.
- **Observable scraping** — `/metrics` (request outcomes, round-trip histograms, error
  reasons, lock waits, **selector-fallback radar**, echo retries), `/v1/health/providers`
  login probes, JSON logs with request IDs, Grafana dashboard + alert rules.
- **Billable** — per-tenant keys (`API_TOKENS`), SQLite usage ledger, monthly quotas
  (429 `quota_exceeded`), `GET /v1/usage`.
- **Honest hardening** — multi-key auth, CORS, body caps, lock timeouts (no infinite
  hangs), long-prompt pre-flight guard, copy-button completion detection, echo recovery.

---

## Quick Start

### Local binary (headful — recommended, harder to detect)

```bash
cp .env.example .env
# edit .env: PROVIDER=chatgpt|claude|qwen|deepseek|kimi|all, API_TOKEN=chimera

go build -o chimera ./cmd/chimera
./chimera
# → launches Chromium headful, navigates to provider URL
# → log in once in the opened browser window (session persisted to ./browser_data/<provider>)
# → API live at http://localhost:8000

curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
curl -H "Authorization: Bearer chimera" -H "Content-Type: application/json" \
  -d '{"model":"chimera-chatgpt","messages":[{"role":"user","content":"Hello!"}]}' \
  http://localhost:8000/v1/chat/completions
```

Headless (not recommended — easier to detect):

```bash
HEADLESS=true ./chimera
```

### OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8000/v1", api_key="chimera")
resp = client.chat.completions.create(model="chimera-chatgpt", messages=[{"role":"user","content":"Explain quantum computing"}])
print(resp.choices[0].message.content)

# Streaming (emulated)
for chunk in client.chat.completions.create(model="chimera-chatgpt", messages=[{"role":"user","content":"Write a story"}], stream=True):
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

### Two ways to use it with agents

**1. As the model** — point any OpenAI-compatible client at Chimera and your
subscription answers (zero token spend, access to models with no API):

```python
# opencode / LangChain / any OpenAI SDK: only base_url + api_key change
client = OpenAI(base_url="http://localhost:8000/v1", api_key="chimera")
```

**2. As a tool** (planned, [`specs/001`](specs/001-mcp-stdio-mode.md)) — keep your
agent on API credits and escalate hard steps to your subscriptions via MCP `chat`,
or let five subscriptions vote/fall back behind one tool call.

---

## Self-Host vs Managed (Open-Source SaaS)

|  | Self-host (MIT, free) | Managed single-tenant (paid) |
|---|---|---|
| Run | `docker compose up` | isolated container per tenant |
| Login | `:6080/vnc.html` | private VNC URL |
| Data | your disk (`./browser_data`) | per-tenant volume |
| Updates | you pull | maintained selectors + uptime |

Starter pricing: **Solo $19/mo/browser (5k req)** · **Team $79/mo/browser (50k req, `PROVIDER=all`)** · **Scale custom**. Billed on browser-hours, not tokens. Full tiers + Fly/K8s guide in [`docs/SAAS.md`](docs/SAAS.md).

Single-tenant deploy:

```bash
TENANT_ID=acme API_PORT=8101 VNC_PORT=5901 NOVNC_PORT=6081 ./scripts/new-tenant.sh
TENANT_ID=acme docker compose -f docker-compose.tenant.yml up -d --build
open http://localhost:6081/vnc.html  # tenant logs in once
curl -H "Authorization: Bearer <tenant-token>" http://localhost:8101/v1/models
```

---

## Configuration

All in `.env` + env (see `.env.example`):

| Var | Default | Purpose |
|-----|---------|---------|
| `PROVIDER` | `chatgpt` | `chatgpt`\|`claude`\|`qwen`\|`deepseek`\|`kimi`\|`all` |
| `BROWSER_DATA_DIR` | `./browser_data` | persistent profile parent (per-provider subdir) |
| `HEADLESS` | `false` | headless Chromium |
| `CHATGPT_URL` / `CLAUDE_URL` / `QWEN_URL` / `DEEPSEEK_URL` / `KIMI_URL` | vendor URLs | override for proxies |
| `RESPONSE_TIMEOUT` | `120000` ms | max wait for streaming |
| `SELECTOR_TIMEOUT` | `10000` ms | selector fallback timeout |
| `API_HOST` / `API_PORT` | `0.0.0.0:8000` | listen |
| `API_TOKEN` | `chimera` | Bearer token; empty disables auth |
| `API_TOKENS` | `` | multi-tenant `tenant=token,...` pairs |
| `QUOTA_MONTHLY_REQUESTS` | `0` | per-tenant monthly cap (`0` = unlimited) |
| `METER_DB` | `./logs/usage.db` | SQLite usage DB; empty disables metering |
| `RATE_LIMIT_SECONDS` | `2` | token bucket |
| `LOG_DIR` / `LOG_LEVEL` / `VERBOSE` | `./logs` `debug` `true` | file + stderr |
| `LOG_FORMAT` | `text` | `text` or `json` (Loki/ELK-friendly, includes `req_id`) |

---

## Architecture

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for deep dive (browser lifecycle, stealth, human behavior, response detection, tool calling, selector fallback, provider abstraction, Docker topology) and [`docs/PROVIDERS.md`](docs/PROVIDERS.md) for per-provider selectors.

```
cmd/chimera/main.go         entry + banner + graceful shutdown
internal/config             env loader, per-provider BrowserDataPath
internal/auth               credential → tenant registry (single + multi-key)
internal/meter              SQLite usage ledger + monthly summaries
internal/telemetry          Prometheus metrics + provider health state
internal/browser            rod manager (UserDataDir persistence, jittered viewport, lock cleanup), pool (multi-tab), stealth patches, human behavior (random delays, InsertText, hover+click)
internal/models             OpenAI-compatible schemas + ProviderResponse
internal/tools              tool prompt injection (ChatGPT vs Claude phrasing) + brace-depth JSON parser
internal/providers          Provider interface + Base (fallback find, wait, count, extract) + 5 clients (chatgpt/claude/qwen/deepseek/kimi) each with selectors.go
internal/session            X-Session-Id continuity with LRU tab eviction
internal/api                chi router, multi-key auth, rate limit, SSE streaming emulation, tool-call wiring, quotas, usage, per-provider locks
```

---

## API

See [`docs/API.md`](docs/API.md). OpenAI-compatible:

- `GET /health` (unauthenticated)
- `GET /metrics` (unauthenticated Prometheus exposition)
- `GET /v1/health/providers` (per-provider login status for status pages)
- `GET /v1/usage?from&to` (per-tenant usage; callers see only their tenant)
- `GET /v1/models`
- `POST /v1/chat/completions` (non-stream + `stream:true` SSE + tools)
- `POST /v1/responses` (Codex-style; `stream:true` rejected for now)
- Auth via `Authorization: Bearer <API_TOKEN>`, `x-api-key`, `anthropic-api-key`.

See [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md) for metrics, alerts, and Grafana dashboard.

---

## Provider Matrix

| Provider | Model ID | URL | Status |
|----------|----------|-----|--------|
| ChatGPT | `chimera-chatgpt` | `chatgpt.com` | full chat + tools (vision/file TODO, image gen planned) |
| Claude | `chimera-claude` | `claude.ai` | full chat + tools (streaming via `data-is-streaming`) |
| Qwen | `chimera-qwen` | `chat.qwen.ai` | full chat + stability fallback |
| DeepSeek | `chimera-deepseek` | `chat.deepseek.com` | full chat + stability fallback |
| Kimi | `chimera-kimi` | `kimi.com` | full chat + stability fallback |

Adding a new provider: copy `qwen/` template, define fallback selector lists, register in `config` + `cmd/chimera/main.go:112`. See `docs/PROVIDERS.md`.

---

## Docker (planned, mirroring Chimera)

```dockerfile
# multi-stage: golang:1.25 builder → debian:bookworm-slim + chromium + xvfb + x11vnc + novnc + supervisord → chimera :8000, VNC :6080
docker compose up --build -d
open http://localhost:6080/vnc.html   # one-time login
# VNC is password-protected: set VNC_PASSWORD in .env, or read the generated one:
#   docker compose logs chimera | grep "VNC_PASSWORD"
curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
```

---

## Tool Calling

Browser UIs lack native function-calling; Chimera does prompt engineering (same as Chimera):

- `tools.BuildToolPrompt(provider)` injects signatures.
- Model emits `{"tool_calls":[{"name":"…","arguments":{…}}]}` (code fence or bare JSON;
  a lone `{"name":"…","arguments":{…}}` object is accepted too).
- `tools.ParseToolCallsWithDefs` extracts with brace-depth tracker, drops names that
  weren't requested (counted as `chimera_tool_name_reject_total`), normalizes bad
  arguments to `{}`, and assigns unique `call_<hex>` IDs.
- Next turn: send `{"role":"tool","tool_call_id":"call_…","content":"…"}` → folded to `Tool result (call_…): …`.

---

## Development

```bash
go vet ./... && go build ./...          # health
go run ./cmd/chimera                    # headful run
go test ./...                           # full suite (mock-backed, no browser needed)
```

---

## FAQ

**Why is it slow?** Every turn drives a real Chromium tab: 5–30s. Chimera is for
reasoning steps and async agents, not autocomplete-speed loops. Repeats get faster
once the response cache ([`specs/008`](specs/008-response-cache.md)) lands.

**Will my account get banned?** Browser automation can trigger CAPTCHAs, rate limits,
or bans. Mitigations: headful mode, human-behavior pacing, persistent profiles, and
(soon) caching to reduce page loads. Use accounts you can afford to verify.

**Why not just use the official APIs?** If per-token pricing works for you, use it —
it's faster and supported. Chimera is for killing a second bill, reaching models with
no API, and keeping prompts in your own browser profile.

**Does it do vision / files / image gen?** Not yet — multimodal parts are parsed but
only text is sent today. Wiring uploads is tracked post-v1.

**How do tool calls work without an API?** Prompt engineering: signatures are injected
into the system prompt, the model emits JSON, a brace-depth parser extracts it
(`internal/tools`). Reliable for 1–7 tools; hallucinations are filtered once
[`specs/004`](specs/004-tool-call-hardening.md) lands.

**How is this different from the Python Chimera-Gateway?** Same idea, Go runtime:
18MB static binary, 60ms cold start, ~1/4 the RAM, plus metering, per-tenant quotas,
and Prometheus observability the Python project doesn't ship.

---

## Limitations

- 5–30s latency (real browser)
- Sessions expire → re-login via browser window or VNC
- Selectors brittle → fallback radar alerts; only `selectors.go` needs edit on vendor UI change (config packs planned: [`specs/006`](specs/006-selector-packs.md))
- Tool calling reliable for 1–7 tools via prompting
- Per-provider serialization via locks; cross-provider failover planned ([`specs/005`](specs/005-cross-provider-failover.md))

---

## Disclaimer

Browser automation may violate vendor ToS (OpenAI, Anthropic, etc.). Use your own accounts for personal automation / evaluation, never share credentials or resell tokens. Not affiliated. One tenant = one browser = one login. See [`docs/SAAS.md`](docs/SAAS.md).

---

## License

MIT. Inspired by Chimera-Gateway (MIT).

