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
{"name":"Chimera Gateway","version":"0.1.0","status":"running","provider":"chatgpt"}
```

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
Model IDs: `chimera-chatgpt`, `chimera-claude`, `chimera-qwen`, `chimera-deepseek`. Any `model` sent in chat completions is accepted and logged.

### `POST /v1/chat/completions`
Core endpoint. Mirrors OpenAI Chat Completions. Browser round-trip is 5–30s; streaming is emulated after full response.

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
and parses the model's JSON output via `tools.ParseToolCalls`. Returned as standard OpenAI `tool_calls` with `call_1`, `call_2` IDs. Next turn you send back `{"role":"tool","tool_call_id":"call_1","content":"Sunny, 25C"}` which is folded into `Tool result (call_1): Sunny, 25C` in the prompt.

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

## Auth & Rate Limit

- All `/v1/*` require Bearer token if `API_TOKEN` set; `/health` and `/` are open.
- Token bucket: `RATE_LIMIT_SECONDS=2` → 1 request per 2s per gateway instance. 429 `{"error":"rate limit exceeded, try again later"}`.
- CORS: permissive (`allow_origins: *`) via chi middleware (same as Chimera's `CORSMiddleware`).

---

## Headers & Sessions

- `x-session-id` (future): stick to a tab in the page pool for multi-turn coherence without reloads. v1 serializes on single page; header is reserved.
- `Content-Type: application/json` required.

---

## Errors

| Code | Body | When |
|------|------|------|
| 401 | `{"error":"unauthorized"}` | bad/missing Bearer |
| 429 | `{"error":"rate limit exceeded…"}` | bucket empty |
| 400 | `{"error":"messages array is required"}` / `invalid request` | bad JSON / missing user message |
| 500 | `{"error":"provider error: …"}` | browser nav, selector, timeout |

---

## Provider Model Matrix

| Env PROVIDER | Model ID | Upstream URL | Vision | Files | Image gen |
|--------------|----------|--------------|--------|-------|-----------|
| chatgpt | chimera-chatgpt | https://chatgpt.com | TODO | TODO | via DOM scrape (planned) |
| claude | chimera-claude | https://claude.ai | TODO | TODO | 501 for now |
| qwen | chimera-qwen | https://chat.qwen.ai | TODO | TODO | — |
| deepseek | chimera-deepseek | https://chat.deepseek.com | TODO | TODO | — |
