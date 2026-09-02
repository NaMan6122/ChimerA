# Chimera — Browser-Based LLM Gateway (Go)

> **Turn any browser ChatGPT / Claude / Qwen / DeepSeek account into an OpenAI-compatible API.** No API keys — just your browser login.

Port of [Chimera-Gateway](https://github.com/GautamVhavle/Chimera-Gateway) (Python + Patchright + FastAPI) to **Go + rod + chi** for a lightweight, static-binary, operationally cheap gateway.

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
| Concurrency | asyncio + GIL | goroutines + `sync.Mutex` (page pool future) |

---

## Quick Start

### Local binary (headful — recommended, harder to detect)

```bash
cp .env.example .env
# edit .env: PROVIDER=chatgpt|claude|qwen|deepseek, API_TOKEN=chimera

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

---

## Configuration

All in `.env` + env (see `.env.example`):

| Var | Default | Purpose |
|-----|---------|---------|
| `PROVIDER` | `chatgpt` | `chatgpt`\|`claude`\|`qwen`\|`deepseek` |
| `BROWSER_DATA_DIR` | `./browser_data` | persistent profile parent (per-provider subdir) |
| `HEADLESS` | `false` | headless Chromium |
| `CHATGPT_URL` / `CLAUDE_URL` / `QWEN_URL` / `DEEPSEEK_URL` | vendor URLs | override for proxies |
| `RESPONSE_TIMEOUT` | `120000` ms | max wait for streaming |
| `SELECTOR_TIMEOUT` | `10000` ms | selector fallback timeout |
| `API_HOST` / `API_PORT` | `0.0.0.0:8000` | listen |
| `API_TOKEN` | `chimera` | Bearer token; empty disables auth |
| `RATE_LIMIT_SECONDS` | `2` | token bucket |
| `LOG_DIR` / `LOG_LEVEL` / `VERBOSE` | `./logs` `debug` `true` | file + stderr |

---

## Architecture

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for deep dive (browser lifecycle, stealth, human behavior, response detection, tool calling, selector fallback, provider abstraction, Docker topology) and [`docs/PROVIDERS.md`](docs/PROVIDERS.md) for per-provider selectors.

```
cmd/chimera/main.go         entry + banner + graceful shutdown
internal/config             env loader, per-provider BrowserDataPath
internal/browser            rod manager (UserDataDir persistence, jittered viewport, lock cleanup), stealth patches, human behavior (random delays, InsertText, hover+click)
internal/models             OpenAI-compatible schemas + ProviderResponse
internal/tools              tool prompt injection (ChatGPT vs Claude phrasing) + brace-depth JSON parser
internal/providers          Provider interface + Base (fallback find, wait, count, extract) + 4 clients (chatgpt/claude/qwen/deepseek) each with selectors.go
internal/api                chi router, Bearer auth, rate limit, SSE streaming emulation, tool-call wiring, mutex-serialized browser access
```

---

## API

See [`docs/API.md`](docs/API.md). OpenAI-compatible:

- `GET /health` (unauthenticated)
- `GET /v1/models`
- `POST /v1/chat/completions` (non-stream + `stream:true` SSE + tools)
- Auth via `Authorization: Bearer <API_TOKEN>`, `x-api-key`, `anthropic-api-key`.

---

## Provider Matrix

| Provider | Model ID | URL | Status |
|----------|----------|-----|--------|
| ChatGPT | `chimera-chatgpt` | `chatgpt.com` | full chat + tools (vision/file TODO, image gen planned) |
| Claude | `chimera-claude` | `claude.ai` | full chat + tools (streaming via `data-is-streaming`) |
| Qwen | `chimera-qwen` | `chat.qwen.ai` | full chat + stability fallback |
| DeepSeek | `chimera-deepseek` | `chat.deepseek.com` | full chat + stability fallback |

Adding a new provider: copy `qwen/` template, define fallback selector lists, register in `config` + `cmd/chimera/main.go:112`. See `docs/PROVIDERS.md`.

---

## Docker (planned, mirroring Chimera)

```dockerfile
# multi-stage: golang:1.23 builder → debian:bookworm-slim + chromium + xvfb + x11vnc + novnc + supervisord → chimera :8000, VNC :6080
docker compose up --build -d
open http://localhost:6080/vnc.html   # one-time login
curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
```

---

## Tool Calling

Browser UIs lack native function-calling; Chimera does prompt engineering (same as Chimera):

- `tools.BuildToolPrompt(provider)` injects signatures.
- Model emits `{"tool_calls":[{"name":"…","arguments":{…}}]}` (code fence or bare JSON).
- `tools.ParseToolCalls` extracts with brace-depth tracker → returned as `message.tool_calls` with `call_1` IDs.
- Next turn: send `{"role":"tool","tool_call_id":"call_1","content":"…"}` → folded to `Tool result (call_1): …`.

---

## Development

```bash
go vet ./... && go build ./...          # health
go run ./cmd/chimera                    # headful run
go test ./internal/tools -v             # tool parser tests (add)
```

---

## Limitations

- 5–30s latency (real browser)
- Sessions expire → re-login via browser window
- Selectors brittle → only `selectors.go` needs edit on vendor UI change
- Tool calling reliable for 1–7 tools via prompting
- Single page → serialized via `sync.Mutex` (Chimera's `BrowserPagePool` multi-tab is roadmap)

---

## License

MIT. Inspired by Chimera-Gateway (MIT).

