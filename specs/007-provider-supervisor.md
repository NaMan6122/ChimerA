# 007 — Provider supervisor (liveness + login expiry)

**Problem:** Tabs die silently (renderer crash, vendor logout, expired session) and the
gateway discovers it only when a user turn 500s. `chimera_provider_up` only moves on
explicit `/v1/health/providers` polls.

**Goal:** background supervisor per provider: detect dead tabs and lost logins,
recover what can be recovered, and keep health state fresh without user traffic.

## Design

New file `internal/browser/supervisor.go` (no new deps):

- `Supervisor` owns one goroutine per provider: every `SUPERVISOR_INTERVAL` (default
  60s, `0` disables) it runs, in order:
  1. **Liveness**: trivial `Eval` on the page. Crash/dead → close tab, open a fresh
     one on the provider URL, re-run stealth + `Init`, count
     `chimera_supervisor_recover_total{provider,result:recovered|failed}`.
  2. **Login**: `IsLoggedIn()` → `telemetry.RecordLoginCheck` (keeps
     `chimera_provider_up` warm). Transition up→down sets `needs_login` state and logs
     the VNC re-login nudge with tenant context; transition down→up logs recovery.
- Needs-login surfacing: extend `/v1/health/providers` items with
  `"needs_login":true` when down (spec 001-metrics endpoint already returns the rest).
- Chat-path interaction: none on the hot path (no extra checks per turn); supervisor
  only repairs between turns. A turn arriving mid-repair waits on the existing
  provider lock as today.
- Graceful shutdown: `Stop()` via context, wired into `main.go` alongside
  browser/pool close and (later) MCP serve.
- Pooled mode: supervise every page in the pool; single mode: the one page.
  Supervisor never touches `browser_data` (no lock-file games — manager owns that).

## Files

- `internal/browser/supervisor.go` + `supervisor_test.go` (injectable probe/recover
  funcs — no real Chromium in tests; assert transitions, counters, backoff)
- `internal/api/server.go` (`needs_login` field in health response)
- `cmd/chimera/main.go` (start/stop wiring), `.env.example` (`SUPERVISOR_INTERVAL`)
- `docs/OBSERVABILITY.md` (new metric + alert `ChimeraNeedsLogin`)

## Acceptance

- Killed tab (simulated probe failure) → fresh tab + `recovered` counter, no restart.
- Login lost → `up==0`, `needs_login==true` in health JSON within 2 intervals.
- Interval `0` → zero goroutines (assert in test).
- `go vet ./... && go test ./internal/browser/ ./internal/api/`.
