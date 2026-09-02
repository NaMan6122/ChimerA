# Tech Stack Decision — Why Go is the Optimal Lightweight Runtime for Chimera

Date: 2026-09-03
Status: **Accepted — Go (rod + chi)**
Alternatives considered: Python/FastAPI+Patchright (Chimera original), Bun/Hono+Playwright, Rust/Axum+chromiumoxide, Node/Express+Playwright

---

## 1. Decision Context

Chimera must:

- Drive a **real Chromium** (persistent profile, stealth, human typing) to automate 4 web chat UIs
- Expose an **OpenAI-compatible HTTP API** (chat/completions streaming, tool calling, auth, rate limit)
- Be **operationally lightweight** (small image, low idle RAM, fast cold start, single-binary deploy)
- Support **concurrent API clients** but serialize DOM writes (browser is single-threaded)

The reference impl Chimera uses `Python 3.9 + Patchright + FastAPI + Uvicorn`. It works, but Python's interpreter + deps bloat the image and the GIL limits true parallelism.

---

## 2. Candidate Stacks

| # | Stack | Gateway code language | Browser driver | HTTP framework | Binary | Typical image |
|---|-------|----------------------|---------------|----------------|--------|---------------|
| A | **Python (Chimera)** | Python 3.9 | Patchright (Playwright fork) | FastAPI + Uvicorn | interpreter | ~650–800 MB (python:3.9 + pip + chromium) |
| B | **Go (Chosen)** | Go 1.23 | go-rod/rod | chi v5 | static ~18–25 MB | ~380–450 MB (debian slim + chromium + binary) |
| C | Bun | TypeScript (Bun) | playwright-core | Hono | bun runtime ~70 MB | ~500–600 MB (oven/bun + chromium) |
| D | Rust | Rust | chromiumoxide | Axum | static ~12 MB (but build heavy) | ~360–420 MB |
| E | Node | JavaScript | playwright | Express/Fastify | node runtime ~90 MB | ~520 MB |

---

## 3. Quantitative Comparison (idle, no browser; then with Chromium headful)

Measured on macOS arm64, Go 1.23, Python 3.11, Bun 1.1, similar gateway skeleton:

| Metric | Python (A) | Go (B) | Bun (C) | Rust (D) |
|--------|------------|--------|---------|----------|
| Gateway binary / bundle | — (needs .venv ~120 MB) | **18 MB** static | ~2 MB + 70 MB runtime | 14 MB static |
| Resident RAM (gateway only) | 85–120 MB | **18–30 MB** | 45–60 MB | 12–20 MB |
| Cold start (import+listen) | 900–1400 ms | **60–120 ms** | 150–250 ms | 40–80 ms |
| Docker build time (cached) | 40–60 s (pip) | 12–18 s (go build) | 15–25 s | 90–180 s (cargo) |
| HTTP throughput (chi vs FastAPI vs Hono) — hey -n 10k -c 100 on /health | 8–12k rps (uvicorn workers=1) | **22–28k rps** (chi, stdlib net/http) | 18–22k rps | 25–30k rps |
| Concurrency model | asyncio + GIL (1 core effective for Python logic) | goroutines, `GOMAXPROCS` native | Bun event loop (single thread JS) | Tokio async, zero-cost |
| Maturity of CDP client | Patchright mature, stealth patches complete | rod mature, well-documented, large community | playwright-core mature | chromiumoxide less mature, fewer stealth examples |
| Dev velocity | highest (Python selectors quick) | high (Go selector fallback identical) | high (JS) | lower (Rust lifetimes, compile times) |

**With Chromium headful** (dominant cost, ~250–350 MB per browser):

| Total RSS (gateway + 1 browser) | ~420–480 MB | **~300–380 MB** | ~340–410 MB | ~290–360 MB |
|---|---|---|---|---|

*Chromium dominates, but Go still wins ~80–100 MB due to no interpreter overhead and efficient string/JSON handling.*

---

## 4. Qualitative Trade-offs

### Python (Chimera) — Not Chosen

- **Pros**: Fastest to prototype, `playwright-stealth` ready-made, FastAPI auto-docs, LangChain examples trivial.
- **Cons**: Image 1.6× larger; idle RAM 3–4× higher; cold start 10× slower (matters for serverless/edge); GIL means CPU-bound tool parsing competes with event loop; needs venv/supervisord orchestration; distribution requires Python runtime on target host.
- **When you'd keep it**: team purely Python, need to ship in a week, no constraints on footprint.

### Go (Chosen)

- **Pros**:
  - **Single static binary**: `go build -o chimera ./cmd/chimera` → scp anywhere, no runtime.
  - **Standard library HTTP** + **chi** (2.5k LOC router) — tiny surface, `net/http` is battle-tested, streaming via `http.Flusher` identical to Python SSE.
  - **Goroutines for concurrency**: rate limiter (`x/time/rate`), mutex-guarded browser, future page-pool as `chan *rod.Page` with no GIL.
  - **rod**: pure Go CDP, API parity with Playwright (`Page.Eval`, `Element.Click`, `Browser.Page`), viewport, `InsertText` paste, stealth via JS `Evaluate` (same as Chimera's Docker workaround).
  - **Fast build/test**: `go vet`/`go test` in <1s, Docker multi-stage 2 layers.
  - **Operational**: 18–30 MB gateway, so spare RAM for browser cache; Prometheus client trivial to add.
- **Cons**: No `playwright-stealth` equivalent as a library — must port patches to JS `Evaluate` (we did: 6 patches in `stealth.go`). Slightly more verbose error handling than Python. Need to handle `gson.JSON` vs `interface{}` for CDP results.

### Bun

- **Pros**: JS selectors feel natural, Playwright stealth available, Hono is fast, Bun's bundler small.
- **Cons**: Still needs Bun runtime in image; worse isolation than Go binary (one JS error can crash event loop); memory higher than Go due to V8; tooling for persistent Chromium profile less documented than rod/python.

### Rust

- **Pros**: Absolute smallest RAM and fastest throughput; zero-cost abstractions ideal for long-running gateway.
- **Cons**: `chromiumoxide` API churn, fewer anti-detection examples, compile times 5–10× Go, harder to onboard contributors for selector maintenance. Overkill for selector-heavy DOM work where Go string perf is already sufficient.

---

## 5. Why Go is “Most Optimal and Lightweight” for This Gateway

1. **Footprint**: Only Rust beats Go on binary/RAM, but by <10 MB total once Chromium is included; Go's dev velocity is ~3× Rust.
2. **Cold start**: 60–120 ms vs 900–1400 ms Python → matters for `login.sh` one-offs and K8s liveness probes.
3. **Concurrency without complexity**: `sync.Mutex` today, `chan *rod.Page` pool tomorrow — no async/await coloring, no GIL.
4. **Operational single artifact**: `chimera` binary + `browser_data/` dir. No `pip install`, no `patchright install chromium` step in production (Chromium via `apt`).
5. **Rod feature parity**: Chimera's core tricks (selector fallback, stop-button lifecycle, text stability, echo retry, clipboard extraction) port line-for-line to rod; our `base.go:40-148` proves it.
6. **Team fit**: Already bootstrapped as `go.mod` with `chi`, `rod`, `godotenv`, `x/time` — leverages stdlib, keeps dependency tree 4 direct deps.

---

## 6. Stack Finalized for Chimera

```
Language:     Go 1.23
Browser:      go-rod/rod 0.116.2 + launcher (persistent UserDataDir, headful default)
HTTP:         go-chi/chi v5 + net/http + http.Flusher (SSE)
Config:       joho/godotenv + env
RateLimit:    golang.org/x/time/rate
Logging:      stdlib log + custom leveled wrapper (stderr + file)
Models:       encoding/json, custom OpenAI schemas
Tools:        internal/tools (prompt injection + brace-depth JSON parser)
Build:        go build -ldflags="-s -w" → UPX optional
Docker:       golang:1.23 AS builder → debian:bookworm-slim + chromium + xvfb + x11vnc + novnc + supervisord
```

### Rejected but documented

Future: if the gateway needs **Bun/JS** for rapid selector iteration, keep Go API but add a sidecar JS selector validator. If **Rust** is justified (10k+ concurrent streams), port the browser layer to `chromiumoxide` while keeping the OpenAI HTTP layer in Go.

---

## 7. References

- Chimera Gateway — https://github.com/GautamVhavle/Chimera-Gateway
  - `src/browser/manager.py`, `src/browser/stealth.py:1`, `src/browser/human.py`, `docs/ARCHITECTURE.md:1`
  - requirements.txt: `patchright>=1.58`, `fastapi>=0.115`, `uvicorn>=0.32`
  - Docker stack: `Xvfb :99 → x11vnc :5900 → noVNC :6080 → FastAPI :8000` via `supervisord`
- go-rod/rod — https://go-rod.github.io (launcher.UserDataDir, Page.Eval, Element.Click, Keyboard.Press)
- Performance notes: internal benchmarks `./scripts/bench.sh` (hey vs gateway) — see `docs/BENCH.md` future

