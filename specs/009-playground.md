# 009 — Playground page

**Problem:** First-run friction is three tabs: API client + VNC login + log tail.
Evaluators bounce before the "wow, curl just answered" moment.

**Goal:** one embedded page — chat, VNC, and recent logs side by side. Zero setup.

## Design

- `web/playground/` (new): single static `index.html` (+ tiny inline JS, no framework,
  no build step), embedded via `go:embed` in a new `internal/playground` package
  serving the bytes. Page layout: left = chat panel (`POST /v1/chat/completions`,
  token field persisted to `localStorage`, model dropdown from `/v1/models`,
  streaming render); right-top = VNC iframe (`/vnc.html` path configurable via
  `?vnc=`); right-bottom = log tail (polls new endpoint below).
- `GET /playground` (open, like `/`, but only when `PLAYGROUND_ENABLED=true`,
  default true for single-tenant; managed multi-tenant frontends may disable).
- `GET /v1/logs?tail=N` (authed, `N` clamped 1–500, default 100): last N lines of
  the gateway log file for this process (`LOG_DIR/api.log`), `text/plain`. Read-only,
  no filenames accepted (fixed path — no traversal surface). 503 when the log file
  is absent.
- VNC path: playground iframe `src` defaults to `http://<host>:6080/vnc.html`;
  override with `VNC_PUBLIC_URL` env for reverse-proxied deploys.

## Files

- `web/playground/index.html` (new, embedded)
- `internal/playground/playground.go` (serve + embed; ~40 lines)
- `internal/api/server.go` (`/playground`, `/v1/logs` routes)
- `internal/config/config.go` (`PLAYGROUND_ENABLED`, `VNC_PUBLIC_URL`), `.env.example`
- `internal/api/server_test.go` (page 200 + marker strings; logs endpoint with temp `LOG_DIR`)

## Acceptance

- `GET /playground` → 200 HTML containing `v1/chat/completions`, `v1/models`, `vnc.html`.
- Disabled flag → 404. `/v1/logs?tail=5` returns last 5 lines; `tail=9999` clamps.
- No auth regression: chat calls from the page still 401 without token.
- `go vet ./... && go test ./internal/api/`.
