# TheGate Feature Specs

Atomic, implementable specs for every add-on discussed. Each spec is self-contained:
problem → design → files → acceptance. Build in dependency order.

## Conventions (all specs)

- Pure-Go deps only (static binary is a feature). New deps need justification.
- Config follows the `getEnv*` pattern in `internal/config/config.go` + `.env.example` row.
- HTTP errors use `writeJSONError(status, type, msg)`; new error `type` strings are listed per spec.
- New request counters go in `internal/telemetry` following existing names.
- Tests use the existing `mockProvider` pattern in `internal/api/server_test.go`; no live browser in CI.

## Index

| # | Spec | Phase | Effort | Depends on |
|---|---|---|---|---|
| 001 | [MCP stdio mode](001-mcp-stdio-mode.md) | Protocols | M | — |
| 002 | [Responses API](002-responses-api.md) | Protocols | M | 004 |
| 003 | [Anthropic Messages passthrough](003-anthropic-messages.md) | Protocols | M | 004 |
| 004 | [Tool-call hardening](004-tool-call-hardening.md) | Protocols | S | — |
| 005 | [Cross-provider failover](005-cross-provider-failover.md) | Reliability | M | 001-metrics (done) |
| 006 | [Selector packs](006-selector-packs.md) | Reliability | M | — |
| 007 | [Provider supervisor](007-provider-supervisor.md) | Reliability | M | 001-metrics (done) |
| 008 | [Response cache](008-response-cache.md) | Perf | S–M | — |
| 009 | [Playground page](009-playground.md) | DX | S | — |
| 010 | [Eval harness](010-eval-harness.md) | DX | S–M | 006 (fixtures reuse pack schema) |

Suggested build order: 004 → 002 → 003 → 001 → 008 → 005 → 006 → 007 → 009 → 010.
(Tool hardening first because 002/003 consume its parser; MCP any time after 004.)
