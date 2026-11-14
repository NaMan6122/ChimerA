# Chimera API — OpenAI-Compatible

Base URL: `http://localhost:8000`
Auth: `Authorization: Bearer <API_TOKEN>` (or `x-api-key` / `anthropic-api-key`). Set `API_TOKEN=""` to disable.

---

## Endpoints

### `GET /health`
Unauthenticated. Docker / K8s probe.

```json
{"status":"ok"}
```

### `GET /`
Gateway info.

```json
{"name":"Chimera Gateway","version":"0.4.0","status":"running","provider":"chatgpt"}
```

### `GET /v1/health/providers`
Per-provider login + quota snapshot for status pages and alerting. Requires auth like other `/v1/*`. Probes each provider's `IsLoggedIn` in parallel (15s budget; slow probes keep their previous state).

```json
{
  "object":"provider_health",
  "data":[
    {"provider":"grok","model":"chimera-grok","up":true,"logged_in":true,
     "last_success_unix":1770000000,"last_check_unix":1770000000,
     "quota":{"remaining":21}},
    {"provider":"qwen","model":"chimera-qwen","up":true,"logged_in":true}
  ]
}
```

- `quota` appears **only** for providers implementing the optional
  `providers.QuotaReporter` interface (currently grok, which caches its
  `/rest/rate-limits` reading at Init and after each turn — no background
  polling). Shape: `{remaining, total?, window?}`; `total`/`window` are
  omitted while unknown. A reporter with nothing cached emits no `quota`
  key at all.
- `last_error` is set when the last probe failed; `last_success_unix` /
  `last_check_unix` are unix seconds, omitted until the first event.

### `GET /v1/models`
Lists the active provider's model.

```bash
curl -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
```
```json
{
  "object":"list",
  "data":[{"id":"chimera-chatgpt","object":"model","owned_by":"chatgpt"}]
}
```
Model IDs: `chimera-chatgpt`, `chimera-claude`, `chimera-qwen`, `chimera-deepseek`,
`chimera-kimi`, `chimera-gemini`, `chimera-grok`, `chimera-glm`. Unknown `model`
values are accepted and routed to the default provider (logged, never rejected).

### `POST /v1/chat/completions`
Core endpoint. Mirrors OpenAI Chat Completions. Browserless turns land in
~1–2 s (p50 measured on DeepSeek); DOM turns take 10–20 s. Streaming emits
OpenAI-format chunks with a `[DONE]` terminator.

#### Request

```json
{
  "model": "chimera-chatgpt",
  "messages": [
    {"role": "system", "content": "You are helpful."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get weather for a city",
        "parameters": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
      }
    }
  ],
  "tool_choice": "auto"
}
```

- `messages[].content` may be string or array of `ContentPart` (`{type:"text", text:"…"}` plus future `{type:"image_url", image_url:{url:"…"}}`)
- `tool_choice`: `auto` (default), `required`, `none`, or `{"type":"function","function":{"name":"…"}}` — mapped to prompt phrasing.

#### Non-streaming response

```json
{
  "id":"chatcmpl-1714600000000",
  "object":"chat.completion",
  "created":1714600000,
  "model":"chimera-chatgpt",
  "choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I …","tool_calls":null},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":12,"completion_tokens":18,"total_tokens":30}
}
```
If tools requested and model emitted `{"tool_calls":[...]}`, they are parsed and returned in `message.tool_calls` with `finish_reason: "tool_calls"`; their JSON `arguments` are stringified per OpenAI spec.

#### Streaming response (`stream:true`)

`Content-Type: text/event-stream`

```
data: {"id":"…","object":"chat.completion.chunk","created":…,"model":"chimera-chatgpt","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"…","object":"chat.completion.chunk","created":…,"model":"chimera-chatgpt","choices":[{"index":0,"delta":{"content":"Hello"}}]}

...

data: {"id":"…","object":"chat.completion.chunk","created":…,"model":"chimera-chatgpt","choices":[{"index":0,"finish_reason":"stop"}]}

data: [DONE]
```

Server buffers full browser response then emits 20-char chunks every 10ms. Compatible with `openai` SDK `stream=True`.

#### Tool calling (browser prompt engineering)

Since no native function-calling CDP, Chimera injects:

- For ChatGPT: `"You are in tool-calling mode. You have access to… output ONLY {\"tool_calls\":…}"`
- For Claude: `"You have access to external tools through a structured interface…"`
and parses the model's JSON output via `tools.ParseToolCalls`. Returned as standard OpenAI `tool_calls` with unique `call_<hex>` IDs; names are validated against the requested defs, so hallucinated calls are dropped (counted in `chimera_tool_name_reject_total`) instead of reaching your client. Next turn you send back `{"role":"tool","tool_call_id":"call_abc123","content":"Sunny, 25C"}` which is folded into `Tool result (call_abc123): Sunny, 25C` in the prompt.

#### cURL examples

```bash
# Simple chat
curl -X POST http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer chimera" \
  -d '{"model":"chimera-chatgpt","messages":[{"role":"user","content":"Explain quantum computing"}]}'

# Streaming
curl -N -X POST http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer chimera" \
  -d '{"model":"chimera-chatgpt","messages":[{"role":"user","content":"Write a story"}],"stream":true}'

# Claude provider (PROVIDER=claude)
curl -X POST http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer chimera" \
  -d '{"model":"chimera-claude","messages":[{"role":"user","content":"Hello!"}]}'

# Qwen / DeepSeek via same endpoint (PROVIDER env selects backing browser)
```

#### Python OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8000/v1", api_key="chimera")
resp = client.chat.completions.create(model="chimera-chatgpt", messages=[{"role":"user","content":"Hello!"}])
print(resp.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(model="chimera-chatgpt", messages=[{"role":"user","content":"Tell a joke"}], stream=True):
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

---

### `POST /v1/responses`

Codex-style endpoint sharing the chat pipeline (provider routing, pre-flight guard,
quota, locks, telemetry, metering). Non-streaming only.

```bash
curl -X POST http://localhost:8000/v1/responses \
  -H "Content-Type: application/json" -H "Authorization: Bearer chimera" \
  -d '{"model":"chimera-chatgpt","instructions":"Be brief.","input":"Why is the sky blue?"}'
```

- `input`: string **or** item array (`message` with `input_text` blocks,
  `function_call_output` for tool follow-ups; unknown items ignored).
- `tools`: flat `{"type":"function","name","description","parameters"}` list.
- Output: `{id:"resp_…", object:"response", status:"completed",
  output:[{type:"message",…}|{type:"function_call",…}], usage:{input_tokens,…}}`.
- `stream:true` → 400 `streaming_unsupported`. Unknown `model` falls back to the
  default provider, like chat.

---

### `POST /v1/refresh`

Starts a fresh upstream conversation for a provider, discarding thread state.
Recover from stale conversations without a restart. Targets `?model=` / `?provider=`,
or a JSON body `{"model":"chimera-qwen"}`; defaults to the active provider.

```json
{"status":"ok","provider":"qwen","model":"chimera-qwen","elapsed_ms":412}
```

### `GET /v1/usage`

Per-tenant usage over an inclusive window (`from`/`to` RFC3339; default: the
current UTC month). Requires metering (`METER_DB` set); `503 metering_disabled`
otherwise. Callers only ever see their own tenant.

```json
{"object":"usage","tenant":"acme","requests":42,"prompt_chars":9130,
 "completion_chars":180224,"errors":0,"by_provider":{"deepseek":30,"chatgpt":12},
 "quota_monthly":10000}
```

### `GET /metrics`

Prometheus exposition (unauthenticated like `/health`; front with a reverse
proxy in multi-tenant deploys). Covers request outcomes, response/lock-wait
histograms, transport fallbacks, PoW solve durations, and the
selector-fallback radar. Grafana dashboard and alert rules ship in `docker/`.

---

## Auth & Rate Limit

- All `/v1/*` require Bearer token if `API_TOKEN` set; `/health` and `/` are open.
- Token bucket: `RATE_LIMIT_SECONDS=2` → 1 request per 2s per gateway instance. 429 `{"error":"rate limit exceeded, try again later"}`.
- CORS: permissive (`allow_origins: *`) via chi middleware (same as Chimera's `CORSMiddleware`).

---

## Headers & Sessions

- `x-session-id` / `x-thread-id` (active): pins one upstream conversation.
  A stateless client that re-sends growing history is recognised by its
  conversation opening and only the new delta is sent upstream — turn 2 of a
  48k-char transcript carries ~400 chars, not the whole history.
- `Content-Type: application/json` required.

---

## Errors

| Code | Body | When |
|------|------|------|
| 400 | `invalid_request` | bad JSON, missing user message, bad usage window |
| 400 | `message_too_long` | DOM pre-flight guard (`MAX_PROMPT_CHARS`; browserless exempt) |
| 400 | `streaming_unsupported` | `stream:true` on `/v1/responses` |
| 401 | `unauthorized` | bad/missing Bearer |
| 429 | `rate_limit_exceeded` | global token bucket empty |
| 429 | `quota_exceeded` | tenant's monthly `QUOTA_MONTHLY_REQUESTS` exhausted |
| 500 | `provider_error` | provider failure mid-turn |
| 504 | `provider_busy` | provider lock held past the timeout budget |

---

## Provider Model Matrix

| Env PROVIDER | Model ID | Transport | Vision | Files | Image gen |
|--------------|----------|-----------|--------|-------|-----------|
| chatgpt | chimera-chatgpt | webapi + dom | TODO | TODO | via DOM scrape (planned) |
| claude | chimera-claude | dom | TODO | TODO | 501 for now |
| qwen | chimera-qwen | webapi ⚠️ + dom | TODO | TODO | — |
| deepseek | chimera-deepseek | webapi + dom | TODO | TODO | — |
| kimi | chimera-kimi | webapi + dom | TODO | TODO | — |
| gemini | chimera-gemini | webapi | TODO | TODO | — |
| grok | chimera-grok | webapi | TODO | TODO | — |
| glm | chimera-glm | webapi | TODO | TODO | — |
