# 003 — Anthropic Messages passthrough (`POST /v1/messages`)

**Problem:** The gateway already accepts `anthropic-api-key`/`x-api-key` auth but
speaks only OpenAI shapes. Anthropic SDK users must hand-translate.

**Goal:** accept Messages API requests, fulfill from any provider, reply in
Anthropic shape. Non-streaming first (`stream:true` → 400, like spec 002).

## Design

New file `internal/models/anthropic.go`:

- Request: `model`, `max_tokens`, `messages[]` (`role` + `content` string **or**
  blocks `text|image|tool_use|tool_result`), `system?` (string or blocks),
  `tools?` (`name, description?, input_schema`), `tool_choice?`, `stream?`.
- Response: `id:"msg_<unixnano>"`, `type:"message"`, `role:"assistant"`,
  `content[]` (`text` and/or `tool_use{id, name, input<object>}` blocks),
  `model` (resolved provider model ID), `stop_reason: end_turn|tool_use|max_tokens`,
  `usage{input_tokens, output_tokens}` via `estimateTokens`.

New handler `handleAnthropicMessages` (`POST /v1/messages`, same middleware):

1. Convert inbound → `[]ChatMessage` + `[]Tool`:
   - `system` → system message. Text blocks concatenated; `image` blocks → 400
     `image_unsupported` (no vision path yet — explicit, not silent drop).
   - Assistant `tool_use` blocks → assistant message carrying `ToolCalls`
     (IDs preserved). User `tool_result` blocks → `role:"tool"` messages with
     matching `ToolCallID` (reuse `buildPrompt` folding verbatim).
   - Anthropic tools → `Tool{name, description, parameters: input_schema}`;
     `tool_choice` mapped to prompt phrasing exactly like chat does today.
2. Same pipeline as chat: provider resolution, pre-flight guard, quota gate,
   lock + telemetry + `recordUsage(streaming:false)`.
3. Convert out: hardened parser (spec 004) → `tool_use` blocks with the **same
   IDs** the model emitted; `arguments` string re-parsed to object
   (fallback: `{"_raw": ...}` never fails the turn). Any `tool_use` →
   `stop_reason:"tool_use"`, else `"end_turn"`.

## Files

- `internal/models/anthropic.go` (new)
- `internal/api/server.go` (route + handler, ~150 lines)
- `internal/api/server_test.go` (text turn, tool_use turn, tool_result follow-up, image 400)
- `docs/API.md`, `README.md`

## Acceptance

- Text round-trip preserves content; usage echoes estimateTokens math.
- Mock `{"tool_calls":...}` reply → single `tool_use` block, `stop_reason:"tool_use"`.
- Follow-up with `tool_result` returns 200 and folds result into prompt (assert via mock-seen text or 200 + telemetry).
- Image block → 400 `image_unsupported`; `stream:true` → 400 `streaming_unsupported`.
- `go vet ./... && go test ./internal/api/`.
