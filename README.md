# Chimera

**Your chat subscriptions, as an OpenAI-compatible API.**

Point any OpenAI SDK, LangChain chain, or agent harness at Chimera and your
ChatGPT / Qwen / DeepSeek / Claude / Kimi / Gemini login answers over `/v1` —
no API keys, no per-token bill, and no prompts leaving the machine it runs on.

```
Go 1.25  ·  ~21 MB static binary  ·  MIT  ·  self-host free, managed single-tenant
```

> **Users** — [Install](#install) · [The native harness](#the-native-harness-context-lives-at-the-provider) · [API](docs/API.md) · [Chat UIs](docs/CHAT-UI.md) · [Harness internals](docs/SPIN.md) · [Changelog](CHANGELOG.md)
>
> **Operators** — [Architecture](docs/ARCHITECTURE.md) · [Providers](docs/PROVIDERS.md) · [Observability](docs/OBSERVABILITY.md) · [Metering](docs/METERING.md) · [Distribution](docs/DISTRIBUTION.md)
>
> **Internal** — engineering decisions, transport specs and product planning live in a
> private repository (the `docs/internal` submodule). This repo stays product-facing.

---

## The two bills problem

You already pay $20/month for a chat subscription. Then you pay again, per
token, to use the same model in code. Two bills, one brain.

Meanwhile the models you actually want to reach — Qwen, Kimi — either have thin
developer APIs or none in your region, and the browser-automation gateways that
solve this are 650 MB Python images with 900 ms cold starts, no metering, no
metrics, and no story for more than one user.

Chimera is the other version of that:

- **Zero token spend.** The subscription you already pay for becomes the backend.
- **Models without APIs.** One OpenAI shape in front of six consumer surfaces.
- **Private by construction.** Prompts stay in your profile/session, not our cloud.
- **Operable as a product.** Quotas, usage ledgers, login health, and a
  selector-fallback radar that turns vendor UI drift into an alert instead of a mystery 500.
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

Browserless clears each vendor's anti-bot work itself — DeepSeek's Keccak-256
proof-of-work, ChatGPT's Sentinel PoW with a locally minted requirements token,
Qwen request signing — replaying browser-earned cookies with the request. That
is why browserless turns land in **~1–2 seconds** instead of ~15, and why a
tenant costs **24 MB** instead of **1.1 GB**.

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
platform and publishes the archives with checksums and a generated Homebrew
formula. The runbook lives in [`docs/DISTRIBUTION.md`](docs/DISTRIBUTION.md).

---

## The native harness: context lives at the provider

Every install ships `chimera-spin` (headless: one task in, answer out) and
`chimera-chat` (the same loop with a TUI). They are not generic OpenAI
clients, and that is the point.

A raw LLM API is stateless: your harness re-sends the entire conversation on
every call, so context management — trimming, summarizing, cache alignment —
is your problem, and every tool round-trip re-pays for the whole transcript.
Consumer chat subscriptions are the opposite: the vendor's web app keeps the
transcript upstream. Chimera makes that state addressable — `X-Session-Id`
pins one upstream conversation — so the native harness sends only what is new:

| Turn | What crosses the wire |
|---|---|
| 1 | system prompt + tool schemas + your task (~1.3k chars) |
| 2+ | only the new tool result or user message |
| | *vs ~48k for a general harness re-sending history every round* |

Consequences worth understanding:

- **It only works against Chimera.** Point `chimera-spin` at a stateless
  endpoint and turn 2 arrives without its history — fragments in, nonsense
  out. The no-history protocol is the feature; Chimera is its counterpart.
  Across Chimera gateways it is universal: the same binary drives ChatGPT on
  `:8000`, DeepSeek on `:8010`, no rebuild.
- **Tool loops get cheap.** Each round costs the tool result it produced, not
  the transcript again: a three-round tool spin is **4.5 s** on DeepSeek's
  webapi and ~20 s on ChatGPT's — the harness overhead is identical, the
  difference is the vendor.
- **No context-window juggling.** No client-side trimming, no summaries. The
  provider accumulates until it can't (expired session → start a fresh spin;
  the harness deliberately cannot replay what it never stored).
- **Transcripts are observe-only.** JSONL written after the fact for you,
  never read back into a request.

Protocol, tools, slash commands, transcripts: [`docs/SPIN.md`](docs/SPIN.md).

```bash
chimera-spin -base-url http://localhost:8000/v1 -model chimera-chatgpt \
  "Read internal/spin/runner.go and count the exported functions."
```

New to the harness? `-preset flashcards -root ~/papers` starts from a named
scaffold — five presets (`-preset help` prints them), and extra words refine
rather than replace.

---

## Quick start

### 1. Browserless — no Chromium, recommended

```bash
cd ~/chimera
chimera auth login chatgpt      # or deepseek, qwen, kimi, glm; opens a browser once
chimera up                      # start every backend you have a session for
```

`chimera up` launches one gateway per provider on the first free port in
8000–8011 and supervises them — Ctrl-C stops all of them. For a single backend
use `chimera up kimi`, or the one-word form `chimera-kimi` (also
`chimera-gpt`, `chimera-glm`, `chimera-deepseek`, …). Then `chimera-chat`
discovers every one of them automatically.

Serves in seconds, ~24 MB each, no browser process. Sessions expire after days
to weeks; `chimera auth status` tells you when.

### 2. Headful — the real browser, harder to detect

```bash
TRANSPORT=dom PROVIDER=claude chimera   # a Chromium window opens; log in once
```

Profiles persist in `./browser_data/<provider>`. Required for `claude` today;
every other provider has a browserless transport.

### 3. Docker — nothing to install

```bash
docker compose up --build -d
open http://localhost:6080/vnc.html   # one-time login
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

---

## What you get

- **One endpoint, five subscriptions.** `PROVIDER=all` routes every provider
  behind one gateway by `chimera-*` model id; `X-Session-Id` gives
  conversation continuity; `POST /v1/refresh` recovers a stale thread without
  a restart. Shapes: [`docs/API.md`](docs/API.md).
- **Tool calling without a tool-calling API.** Signatures injected into the
  prompt, a brace-depth parser that survives code fences, unknown-name
  rejection, `call_<hex>` ids, results folded into the next turn. Reliable
  for 1–7 tools; hardening tracked in an internal spec (004).
- **Observable scraping, not hopeful scraping.** `/metrics` covers request
  outcomes, round-trip histograms, transport downgrades, and a
  selector-fallback radar that fires the moment a vendor changes their DOM.
  Grafana dashboard and alert rules in `docker/`.
- **Billable out of the box.** Per-tenant keys, a SQLite usage ledger,
  monthly quotas returning a real `429 quota_exceeded`, and `GET /v1/usage`.
- **Hardened where it hurts.** Multi-key auth, CORS, body caps, provider
  locks with timeouts, prompt-length guards, echo recovery.
- **Agent-native.** As the model: swap the base URL. As a tool: escalate hard
  reasoning steps through MCP `chat` (internal spec 001),
  or let five subscriptions vote behind one call.

---

## Measured, not asserted

[`scripts/latency_probe.py`](scripts/latency_probe.py) scrapes `/metrics`
before and after each run, separating gateway overhead from model time.
Browserless transport, macOS arm64, `CGO_ENABLED=0`:

| Scenario | n | p50 | p95 |
|---|---|---|---|
| DeepSeek, 50-char prompt | 5 | **1.17 s** | **2.20 s** |
| DeepSeek, 6,323-char prompt | 3 | **1.92 s** | **2.76 s** |
| Native harness, 3-round tool spin, DeepSeek | 3 | 1.6 s first round | **4.5 s wall** |
| Browser (DOM) turn | — | 10–20 s | serialized by a real page |

Static cost, same run:

| | Browserless | Browser |
|---|---|---|
| Spawn → `/health` 200 | **298 ms** (includes live session check) | ~2.5 min |
| Resident memory per tenant | **24.4 MB** | ~1.1 GB (10 processes) |
| Binary | **21.4 MB** stripped | — |

One machine, one account, one network. Treat these as an order of magnitude
and a method, not an SLA — re-run the probe for your own.

---

## Provider matrix

| Provider | Model id | DOM | Browserless | State |
|---|---|---|---|---|
| ChatGPT | `chimera-chatgpt` | ✅ | ✅ cookie custody, Sentinel PoW | chat + tools; re-export session when `/api/auth/session` reports `RefreshAccessTokenError` |
| Claude | `chimera-claude` | ✅ | — | chat + tools, streaming via `data-is-streaming` |
| Qwen | `chimera-qwen` | ✅ | ⚠️ captcha-blocked (below) | DOM works; tokens auto-refresh |
| DeepSeek | `chimera-deepseek` | ✅ | ✅ `DeepSeekHashV1` PoW | chat + text-stability fallback |
| Kimi | `chimera-kimi` | ✅ | — | chat + text-stability fallback |
| Gemini | `chimera-gemini` | — | ✅ cookie custody, `SNlM0e` nonce, three-id continuation (internal spec 013) | chat; model `auto` takes Google's default |
| Grok | `chimera-grok` | — | ✅ live-verified (internal spec 014) | cookie custody; model side is stateless — the provider folds the transcript, so turns re-pay context |
| GLM (Zhipu) | `chimera-glm` | — | ✅ live-verified (internal spec 024) | refresh-token custody + one MD5 header-signature; native `conversation_id` continuity (no-history invariant holds) |

Adding a provider: copy the `qwen/` DOM template, register it in `config` and
`cmd/chimera/main.go`. A browserless transport implements
`providers.Provider` and calls `webapi.Register` in an `init()` — no API-layer
changes. [`docs/PROVIDERS.md`](docs/PROVIDERS.md).

---

## API and configuration

Endpoints, request/response shapes, auth header forms:
[`docs/API.md`](docs/API.md). Everything is env or `.env` —
[`.env.example`](.env.example) is the annotated set. The four you meet first:

| Variable | Default | Purpose |
|---|---|---|
| `PROVIDER` | `chatgpt` | `chatgpt` `claude` `qwen` `deepseek` `kimi` `all` |
| `TRANSPORT` | `auto` | `auto` `dom` `webapi` |
| `API_PORT` | `8000` | listen port |
| `API_TOKEN` / `API_TOKENS` | off | single or multi-tenant bearer auth |

## MCP stdio mode

`chimera --mcp` serves the same providers as a Model Context Protocol server
over stdio (newline-delimited JSON-RPC 2.0; stdout carries protocol bytes
only, logging stays on stderr). Tools: `chat`, `refresh`, `providers`. Register it in Claude Code or Cursor:

```json
{
  "mcpServers": {
    "chimera": {
      "command": "/usr/local/bin/chimera",
      "args": ["--mcp"],
      "env": { "PROVIDER": "all", "TRANSPORT": "auto", "METER_DB": "./logs/usage.db" }
    }
  }
}
```

Metering rows are keyed `mcp:<clientName>`; pass `session_id` to `chat` to continue an upstream conversation.

---

## Self-host or managed

Chimera is MIT and free to run yourself. If you'd rather not own the toil,
managed single-tenant is the same binary with an isolated container, a private
VNC URL, and someone else watching the selectors.

Starter tiers, priced by substrate rather than per token — **Subscriber API
$9–12/mo** (browserless, 10k requests), **Browser Solo $19/mo** (5k), **Team
$79/mo** (auto transport with warm fallback, 50k), Scale custom. Unit
economics: internal SaaS model + ADR-002 (private repo).

---

## Known issues

Documented because they are measured, not because they are ignored.

1. **Concurrent requests on one browserless conversation can poison it.**
   DeepSeek answers `biz_code 26` forever after a stale parent id. Recover
   with `POST /v1/refresh`; isolate concurrent clients with `X-Session-Id`.
2. **Provider failures after SSE headers are committed return HTTP 200** with
   the error inside the stream. Check the body, not just the status.
3. **DeepSeek rate-limits per account.** "Messages too frequent" surfaces as
   an in-stream event, now detected, but the limit is the vendor's to set.
4. **Qwen's browserless completions are captcha-blocked** (`FAIL_SYS_USER_VALIDATE`,
   human-solved slider). Clearing it programmatically is captcha evasion,
   which this project does not do (project policy). Workarounds: solve it once in the
   browser and re-export (`chimera auth login qwen`), use the DOM transport
   (~30 s/turn), or Qwen's official API.

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
path is 10–20 s because it drives a real page with human pacing.

**Will my account get banned?** Automation can trigger CAPTCHAs, rate limits
or bans. Mitigate: headful, human pacing, persistent profiles, your own
account.

**Why not just use the official APIs?** If per-token pricing works for you, do
— it's faster and supported. Chimera kills a second bill, reaches models with
no API, and keeps prompts in your own session.

**How is this different from the Python Chimera-Gateway?** Same idea, Go
runtime: ~21 MB static binary, ~24 MB browserless RAM, ~300 ms start, plus
metering, quotas and Prometheus observability.

---

## Limitations

- Browserless needs a valid exported session; sessions expire (13–18 days
  observed) and re-login is interactive.
- Per-provider serialization caps throughput — one conversation at a time per
  account, by the vendor's design.
- Selectors rot; that is measurable (`chimera_selector_fallback_total`) and
  fixable in `selectors.go`, not invisible.
- Tool calling is prompt engineering: reliable for a handful of tools, not
  arbitrary schemas. Vision and files are scaffolded, not wired.

---

## Disclaimer

Browser automation may violate vendor terms of service (OpenAI, Anthropic, and
others). Automated access to consumer subscriptions is prohibited regardless of
transport — a faster transport changes cost, not legality. Use your own accounts
for personal automation and evaluation; never share credentials or resell tokens.
Not affiliated with any vendor. One tenant, one account, one login.

## License

MIT.

---

<p align="center">
  Made with ❤️ by <a href="https://anvayahq.com">Anvaya</a>
</p>
