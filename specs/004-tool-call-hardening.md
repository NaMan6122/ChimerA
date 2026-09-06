# 004 — Tool-call hardening

**Problem:** `ParseToolCalls` trusts the model completely: hallucinated function names
pass through, IDs are deterministic `call_1` (collide across turns), and only the
`{"tool_calls":[...]}` envelope parses — a lone `{"name":...}` object is dropped.

**Goal:** validated, uniquely-identified tool calls in both shapes the models emit.

## Design

`internal/tools/calling.go`:

1. `ParseToolCallsWithDefs(text string, validNames []string) []models.ToolCall`
   - Extract JSON exactly as today (fence → bare-object brace tracker, unchanged).
   - Accept envelope `{"tool_calls":[{name, arguments}]}` **and** single object
     `{"name":..., "arguments":{...}}` (wrapped into a one-element list).
   - Drop entries whose `name` is not in `validNames` (empty `validNames` = keep all,
     preserving current behavior for callers without defs). Count drops via a
     `chimera_tool_name_reject_total{provider}` counter in `internal/telemetry`.
   - Missing/non-object `arguments` → `{}` (never fail the whole batch for one bad item).
2. IDs: `call_<12 hex chars from crypto/rand>` (`newCallID()` helper). Uniqueness
   across turns and providers; no other format change (`type:"function"`,
   `arguments` stays a JSON string per OpenAI spec).
3. Keep `ParseToolCalls(text)` as a wrapper with nil defs (back-compat for existing callers/tests).
4. `internal/api/server.go` `parseToolCallsIfPresent`: build `validNames` from
   `toolDefs` and call the new function. No signature change needed elsewhere.

## Files

- `internal/tools/calling.go` (parser + IDs + validation)
- `internal/telemetry/telemetry.go` (one counter)
- `internal/api/server.go` (pass valid names; ~5 lines)
- `internal/tools/calling_test.go` (new cases)

## Acceptance

- `{"tool_calls":[{"name":"ghost","arguments":{}}]}` with defs `[get_weather]` → zero calls, reject counter +1.
- Single `{"name":"get_weather","arguments":{"city":"x"}}` → one call, `arguments` == `{"city":"x"}`.
- 1000 generated IDs unique, matching `^call_[0-9a-f]{12}$`.
- `go vet ./... && go test ./internal/tools/ ./internal/api/`.
