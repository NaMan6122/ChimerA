# 001 — MCP stdio mode

**Problem:** Agentic tools (Claude Code, etc.) consume MCP servers, not HTTP APIs.
Chimera as *a tool inside* an agent (escalate hard steps to your subscriptions) is
a separate distribution surface from *being the model* via `base_url`.

**Goal:** `chimera --mcp` speaks JSON-RPC 2.0 / MCP `2024-11-05` over stdio using the
same launched browser + providers as HTTP mode.

## Design

New package `internal/mcp/` (hand-rolled JSON-RPC over stdio — no new deps):

- Transport: newline-delimited JSON on stdin/stdout; **stdout stays pure protocol**
  (logging already writes stderr; verify no `fmt.Print` to stdout on this path).
- Methods: `initialize` (negotiate, return server info + capabilities),
  `tools/list`, `tools/call`, `ping`. Unknown → `-32601`; bad params → `-32602`;
  internal failures → `-32603` with provider message.
- Tools:
  - `chat{message, provider?="default", model?, session_id?}` → resolve provider
    (reuse `providerForModel` semantics via a small shared resolver — do not import
    `internal/api`), `acquireProviderLock`, `SendMessage`, reply `{content:[{type:"text",
    text}]}`. Records telemetry (`ObserveChatRequest`, duration, errors) and
    `recordUsage`-equivalent when metering is on (share via a tiny interface, not
    by importing `api` — no import cycles: `mcp` may import `providers, config,
    telemetry, meter, tools, models, browser, logging` only).
  - `refresh{provider?}` → `NewChat` under lock (stale-DOM recovery for agents).
  - `providers` → list `{name, model_id}` (no browser touch).
- `cmd/chimera/main.go`: `-mcp` bool flag. When set: skip banner (stdout!), skip
  HTTP server; after provider(s) ready, run `mcp.Serve(os.Stdin, os.Stdout, deps)`
  until EOF/SIGINT; still close browser/pool on shutdown. Single-provider and
  `PROVIDER=all` both supported (map of providers + mutexes passed in).
- Session continuity: honor `session_id` arg exactly like `X-Session-Id` (prune to
  latest turn when history > 2 messages — MCP clients send full history too).

## Files

- `internal/mcp/mcp.go` (transport + dispatch, ~150 lines)
- `internal/mcp/tools.go` (tool schemas + handlers)
- `internal/mcp/mcp_test.go` (in-process pipes + mock provider: init/list/call/refresh/errors)
- `cmd/chimera/main.go` (`-mcp` flag, banner suppression, serve path)
- `README.md`, `docs/API.md` (Claude Code config snippet)

## Acceptance

- `echo '{"jsonrpc":"2.0","id":1,"method":"tools/list",...}' | chimera --mcp` (with
  stubbed browser? No — tests use in-process `Serve` with mock providers, never a binary).
- In-process: initialize → list shows `chat, refresh, providers` → call `chat`
  returns mock text → unknown tool gives `-32602` → provider error gives `-32603`.
- `go vet ./... && go test ./internal/mcp/ ./internal/api/`; `go build` binary size
  delta noted (must stay dependency-free).
