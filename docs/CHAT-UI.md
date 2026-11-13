# Chimera with a chat UI

The gateway speaks the OpenAI Chat Completions API, so any chat front-end that
accepts a custom OpenAI base URL can sit in front of it. This cookbook covers
three, in descending order of capability:

| UI | Type | Good for |
|---|---|---|
| [Open WebUI](https://github.com/open-webui/open-webui) | self-hosted web | the full experience: history, model picker, documents/RAG |
| [LobeChat](https://github.com/lobehub/lobe-chat) | self-hosted web | similar scope, different layout, plugin ecosystem |
| [Chatbox](https://chatboxai.app) | desktop app | no Docker at all — install, paste three fields, chat |

Everything below assumes the browserless gateway from the
[README quick start](../README.md#quick-start):

```bash
cd ~/chimera
chimera auth login chatgpt      # or deepseek, gemini; opens a browser once
TRANSPORT=webapi PROVIDER=chatgpt API_PORT=8000 API_TOKEN=chimera chimera
```

Endpoint shapes and auth header forms: [`docs/API.md`](API.md).

---

## Before you connect

### 1. Confirm the gateway is up

```bash
curl -s http://localhost:8000/health
# {"status":"ok"}
```

### 2. Connectivity check

Run these before opening any UI — they are the exact requests the UI will
make. First the model list:

```bash
curl -s http://localhost:8000/v1/models -H "Authorization: Bearer chimera"
```

```json
{"object":"list","data":[{"id":"chimera-chatgpt","object":"model","owned_by":"chatgpt"}]}
```

Then one real completion:

```bash
curl -s http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer chimera" \
  -d '{"model":"chimera-chatgpt","messages":[{"role":"user","content":"Say OK"}]}'
```

A JSON body with `choices[0].message.content` means the gateway and the
upstream session are both fine — any remaining problem is UI configuration
(see [Troubleshooting](#troubleshooting)). An error naming the session or an
empty `data` list usually means the exported session expired: re-export with
`chimera auth login <provider>` (sessions last days to weeks).

---

## Which model ids appear (and why)

Chimera exposes one model id per provider, `chimera-<provider>`. A UI's model
picker selects the subscription the way it would select a model; the id is a
routing key, not a model name — `chimera-deepseek` means *the DeepSeek account
you exported*, whichever model that account is served today.

`GET /v1/models` lists exactly the provider(s) this gateway serves:

- **Single provider** (the default): one entry, `chimera-chatgpt` when
  `PROVIDER=chatgpt`.
- **`PROVIDER=all`**: one entry per provider — but that is the pooled
  **browser** path (a Chromium per gateway, pages per provider), not the
  browserless path.

Browserless serves **one provider per gateway process**. To chat with two
subscriptions in one UI, run two gateways on two ports:

```bash
TRANSPORT=webapi PROVIDER=chatgpt   API_PORT=8000 API_TOKEN=chimera chimera &
TRANSPORT=webapi PROVIDER=deepseek  API_PORT=8010 API_TOKEN=chimera chimera &
```

Open WebUI takes several base URLs at once via `OPENAI_API_BASE_URLS`
(semicolon-separated, paired with `OPENAI_API_KEYS`).

Browserless coverage as of this writing, checked against the source:

| Provider | Model id | Browserless |
|---|---|---|
| ChatGPT | `chimera-chatgpt` | ✅ |
| DeepSeek | `chimera-deepseek` | ✅ |
| Gemini | `chimera-gemini` | ✅ |
| Qwen | `chimera-qwen` | ⚠️ captcha-blocked — [README known issues](../README.md#known-issues) |
| Claude, Kimi | `chimera-claude`, `chimera-kimi` | — DOM transport only |
| Grok | `chimera-grok` | ✅ live-verified (cookie custody; model side is stateless — see the README matrix note) |
| GLM (Zhipu) | `chimera-glm` | ✅ live-verified (internal spec 024; refresh-token custody + MD5 header-signature, native conversation continuity) |

A UI that hardcodes its own model names still works: unknown `model` values
are accepted and routed to the active provider
([API reference](API.md)).

---

## Open WebUI

Self-hosted web UI: chat history, model picker, per-conversation folders,
document upload.

```bash
docker run -d -p 3000:8080 \
  --add-host=host.docker.internal:host-gateway \
  -e OPENAI_API_BASE_URL=http://host.docker.internal:8000/v1 \
  -e OPENAI_API_KEY=chimera \
  -e ENABLE_OLLAMA=0 \
  -v open-webui:/app/backend/data \
  --name open-webui \
  ghcr.io/open-webui/open-webui:main
```

| Setting | Why |
|---|---|
| `OPENAI_API_BASE_URL` | the UI's upstream: your gateway's `/v1`. `host.docker.internal` is how a container reaches a service on the host. |
| `OPENAI_API_KEY` | sent as `Authorization: Bearer …`; must equal the gateway's `API_TOKEN`. |
| `ENABLE_OLLAMA=0` | Open WebUI probes for a local Ollama by default; off keeps the picker to Chimera models only. |
| `--add-host=host.docker.internal:host-gateway` | required on Linux, where the name does not exist by default; a no-op on Docker Desktop (macOS/Windows). |
| `-v open-webui:/app/backend/data` | accounts, chats and settings — survives container replacement. |

Then open `http://localhost:3000`, create the local account Open WebUI asks
for on first run (that account belongs to the UI, stored in its volume — it
never reaches the gateway), pick `chimera-chatgpt` in the model selector,
chat.

Prefer compose? [`docker-compose.ui.yml`](../docker-compose.ui.yml) starts
Open WebUI and — under the `gateway` profile — a containerized browserless
gateway, every variable annotated:

```bash
docker compose -f docker-compose.ui.yml up -d                     # UI only
docker compose -f docker-compose.ui.yml --profile gateway up -d   # UI + gateway
```

---

## LobeChat

Same idea, slightly different variable names:

```bash
docker run -d -p 3210:3210 \
  --add-host=host.docker.internal:host-gateway \
  -e OPENAI_PROXY_URL=http://host.docker.internal:8000/v1 \
  -e OPENAI_API_KEY=chimera \
  -e OPENAI_MODEL_LIST=chimera-chatgpt \
  --name lobe-chat \
  lobehub/lobe-chat
```

Two differences from Open WebUI:

- the base-URL variable is `OPENAI_PROXY_URL`, and it must include the `/v1`
  suffix;
- LobeChat ships a fixed OpenAI model list, so `OPENAI_MODEL_LIST` (or
  Settings → Language Model → OpenAI → add model) is how `chimera-chatgpt`
  shows up in the picker.

`ACCESS_CODE` is LobeChat's own front-door password for the UI, unrelated to
the gateway.

---

## Chatbox (desktop)

No Docker — install the desktop app from [chatboxai.app](https://chatboxai.app).

1. Settings → Model Provider → **Add** → add a custom provider, API type
   **OpenAI API compatible** (exact labels drift a little between versions;
   what matters is the OpenAI-compatible mode).
2. Fill in:

| Field | Value |
|---|---|
| API Host | `http://localhost:8000` |
| API Path | leave the default `/v1/chat/completions` — host + path must combine to end in exactly one `/v1/chat/completions` |
| API Key | your `API_TOKEN` (any non-empty string if the gateway runs without auth) |
| Models | add one, Model ID `chimera-chatgpt`, type chat |

3. Save, press **Check**, start a new conversation, select the model.

Desktop apps run on the host, so plain `localhost` works — no
`host.docker.internal` anywhere.

---

## How streaming behaves

UIs always request `stream: true`. Chimera emulates SSE: it waits for the
vendor's complete answer, then replays it as OpenAI-format chunks
(`chat.completion.chunk`, ~20 characters every 10 ms, terminated by
`data: [DONE]`). SDKs and UIs cannot tell the difference — but
time-to-first-token equals full generation time: the spinner runs, then the
text arrives quickly rather than token-by-token. On the browserless path that
is ~1–2 s; on the browser path 10–20 s
([measured numbers](../README.md#measured-not-asserted)).

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `connection refused` in the UI | gateway not started, or the UI is inside a container addressing `localhost` | start the gateway and confirm `curl http://localhost:8000/health` on the host; containers must use `host.docker.internal` (`--add-host host.docker.internal:host-gateway` on Linux), never `localhost` |
| `401 unauthorized` | `API_TOKEN` mismatch | the UI key must equal the gateway's `API_TOKEN` (or one entry of `API_TOKENS`). With no `API_TOKEN` set, auth is off and any non-empty value works — but the UI's key field cannot stay blank |
| model picker empty or missing a provider | `/v1/models` lists only the active provider | set `PROVIDER`; browserless serves one provider per process — run one gateway per provider (distinct `API_PORT`s), or `PROVIDER=all` for the pooled browser path |
| `429 rate limit exceeded` | global token bucket: 1 request per `RATE_LIMIT_SECONDS` (default 2) | UIs fire side requests (titles, tags) around each message; raise the interval for headroom or set `RATE_LIMIT_SECONDS=0` to disable while testing |
| `400 … message_too_long` | `MAX_PROMPT_CHARS` guard (12000 chars) — browser transport only | the browserless path is exempt; this reaches you when `TRANSPORT=auto` fell back to the browser |
| long pause, then an error appears as the reply | provider failure after SSE headers were committed → HTTP 200 with the error inside the stream ([README known issue 2](../README.md#known-issues)) | read the reply body and the gateway logs; if it names auth, re-export the session |

---

## Privacy

The UI talks only to your gateway at `http://localhost:8000`. Chimera adds no
cloud hop: it replays your exported session against the vendor you logged
into and returns the answer. Metering is a local SQLite file
(`logs/usage.db`), metrics are scraped locally from `/metrics`, and the UI's
own data — accounts, history, uploads — stays in its volume. Nothing between
the UI and the vendor belongs to Chimera; prompts stay on your machine except
for the one unavoidable hop to the vendor itself. That is the
[core promise](../README.md#the-two-bills-problem).

One boundary to keep straight: `auth_data/*.json` are password-equivalent —
see [`SECURITY.md`](../SECURITY.md). Keep that directory out of any backup you
do not control.

---

## Local history

The gateway preserves every provider turn locally — the same promise the
harness and TUI already keep, so a conversation held in Open WebUI or Chatbox
survives the UI, the container and the vendor's own expiry:

```
~/.local/share/chimera/sessions/<conversation>.jsonl     # SESSIONS_DIR
```

One file per conversation: with an `X-Session-Id` header the client names it,
otherwise the conversation's opening (system + first user message) derives a
stable id, so a UI re-sending growing history appends to one transcript.
Failed turns are kept as error notes. Read it back any time:

```bash
chimera sessions list --tag gateway   # only gateway-recorded conversations
chimera sessions show <id>            # the readable runbook (Markdown)
chimera sessions export <id> --html -o runbook.html
```

Sessions are **preservation, not resumption**: the vendor side expires on its
own schedule; what you keep is what was asked and what came back. Set
`SESSIONS_DIR=""` to disable, or point it elsewhere to separate archives.
UI side-requests (title/tag generation) have a different opening and show up
as their own small transcripts — delete at will.
