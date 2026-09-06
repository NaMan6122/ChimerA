# 008 — Response cache

**Problem:** Every turn costs a 5–30s browser round trip — including exact repeats
(dev loops, evals, retries). Repeat page loads also raise ban exposure for zero benefit.

**Goal:** opt-in exact-prompt cache: repeat turns return in milliseconds *and* skip
the browser entirely. Faster, cheaper, safer — all three at once.

## Design

New package `internal/cache` (stdlib only):

- Key: SHA-256 of `provider|model|pruned-prompt|tools-signature` (reuse the exact
  prompt string the handler already built, so cache semantics match send semantics).
- Store: in-memory LRU (`CACHE_MAX_ENTRIES`, default 500) + TTL (`CACHE_TTL_SECONDS`,
  default 0 = disabled — **off unless enabled**, cached mentees must opt in).
  No disk in v1 (tenants restart cheaply; persistence is a follow-up).
- Wiring in `internal/api/server.go` chat + responses handlers (streaming too — cached
  text flows through the existing chunker):
  1. After quota gate, before lock: lookup. Hit → build the normal response envelope
     (fresh IDs/timestamps), header `X-Chimera-Cache: HIT`, telemetry
     `chimera_cache_total{provider,result:hit|miss}`, return. No lock, no `recordUsage`?
     **Record it** (code 200, latency ~0, completion chars real) — billing counts answers,
     and the row documents the hit. Add `cached:1`... schema has no column; v1 records
     normally (follow-up: `cached` column).
  2. Miss → `X-Chimera-Cache: MISS`, proceed; store successful (200, non-empty) turns.
- Never cache: tool-call turns (arguments feed live execution), errors, empty text,
  per-session (`X-Session-Id` present — conversation state lives in the browser).
- Safety: `POST /v1/refresh` clears that provider's entries (stale-answer escape hatch);
  log size/evictions at debug.

## Files

- `internal/cache/cache.go` + `cache_test.go` (hit/miss/TTL/LRU/session-skip)
- `internal/api/server.go` (lookup/store in chat + responses paths)
- `internal/telemetry/telemetry.go` (one counter)
- `internal/config/config.go`, `.env.example`, `docs/API.md`

## Acceptance

- Same prompt twice (cache on) → second is `HIT`, <50ms, zero `SendMessage` calls
  (assert with counting mock), identical text, fresh response ID.
- Tool turn / session turn / error → never stored, always `MISS`.
- Refresh clears; TTL expiry re-misses; 500-entry cap evicts oldest.
- `go vet ./... && go test ./internal/cache/ ./internal/api/`.
