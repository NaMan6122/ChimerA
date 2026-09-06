# 005 — Cross-provider failover

**Problem:** One vendor rate-limit, outage, or expired session 500s the turn even
though the tenant has four other healthy logins behind the same endpoint.

**Goal:** on provider-side failure, retry once on the next healthy provider and say
so via header. Local contention (`lock_timeout`, client disconnects) never fails over.

## Design

- Config: `PROVIDER_FALLBACK` string, default `""` (off — current behavior).
  Format `chatgpt>qwen>deepseek` (ordered names; validated at `Load`, unknown names
  → startup error). Only meaningful with `PROVIDER=all`; single-provider mode logs
  a warning and ignores it.
- New helper in `internal/api/server.go`, used by chat (non-stream) + responses
  paths (streaming and Anthropic: no failover — mid-stream provider switch corrupts
  SSE/block streams; document):
  ```
  doChatWithFailover(tenant, model, prompt, threadID, tools) → (providerName, resp, toolCalls, code, fallbackFrom)
  ```
  1. Resolve primary via `providerForModel` (unchanged).
  2. Attempt with existing lock + telemetry + `recordUsage` (code as today).
  3. On `send_error` only: walk the fallback chain (skip primary, skip providers
     whose `chimera_provider_up==0` when known), attempt each once with the same
     instrumentation; first success wins.
  4. Success after failover → response header `X-Chimera-Fallback: <used-provider>`
     and `chimera_provider_errors_total{provider,reason:"failover_source"}` on the
     failed one (in addition to its `send_error`).
- Session caveat: failed-over turn runs on a different provider's conversation;
  pass `threadID` through unchanged and note in `docs/` that multi-turn coherence
  across failover is best-effort.

## Files

- `internal/config/config.go` (`PROVIDER_FALLBACK`, validation)
- `internal/api/server.go` (helper + wiring into chat/responses handlers)
- `internal/api/server_test.go` (pooled server: failing mock + healthy mock; assert
  fallback body, header, and both metrics paths; chain-off parity test)
- `docs/API.md`, `.env.example`

## Acceptance

- Fallback on: primary 500 → 200 from secondary with `X-Chimera-Fallback` set.
- No fallback on: `lock_timeout`, 400s, 429 quota, client disconnect.
- `PROVIDER_FALLBACK=bogus` refuses startup; empty = today's behavior bit-for-bit.
- `go vet ./... && go test ./internal/api/ ./internal/config/`.
