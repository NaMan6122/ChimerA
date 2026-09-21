# Report — agentic loop through anv headless on the browserless transport

Date: 2026-09-16 · Transport: `webapi` (qwen, no Chromium) · Raw:
`logs/anv-webapi-rerun.json` (run B), `logs/anv-webapi-final.json` (run A),
`logs/qwen-web.log`

## Method

`python3 scripts/latency_probe.py --mode anv --agentic --n 5 --fixture-chars 6000
--refresh-each --anv-yolo` against the live gateway (`PROVIDER=qwen`,
`TRANSPORT=auto` → webapi, model `qwen3.8-max`, thinking on).

Each spin asks anv to read two generated fixture files with `read_file` and
report the LINE3 token of each — forcing a tool round and a follow-up answer.
The gateway logs real provider usage per turn
(`turn done ... in= out= cached= ttft_ms= out_tok_s=`), which the probe parses.

Environment: anv 0.1.0 (Sep 16 build), same account/machine as the DOM baseline.
Two runs are reported: run A (vendor-degraded window, two network timeouts) and
run B, the identical re-run after the connection was verified clean
(short-prompt PONG 2.0–2.7s immediately before).

## Results — run B (re-run, clean connection)

| spin | wall | rounds | verified | note |
|---|---|---|---|---|
| 0 | 48.4s | 1 | no | transcript role-play |
| 1 | 61.4s | 1 | no | transcript role-play |
| 2 | 134.0s | 1 | no | transcript role-play |
| 3 | **42.6s** | 2 | **yes** | clean tool round + answer |
| 4 | 32.9s | 1 | no | transcript role-play |

Per-round detail (tokens are vendor-reported):

| spin | round | wall | prompt chars | in | out | cached | TTFT | gen | out tok/s |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 1 | 47.8s | 36464 | 11539 | 360 | 10368 | 31.5s | 16.3s | 22.8 |
| 1 | 1 | 61.3s | 36464 | 12718 | 806 | 11520 | 4.0s | 56.7s | 14.2 |
| 2 | 1 | 133.9s | 36464 | 22508 | 445 | 21888 | 19.7s | 113.5s | 3.9 |
| 3 | 1 | 4.9s | 36464 | 10040 | 98 | 9216 | 3.6s | 1.0s | **97.4** |
| 3 | 2 | 37.6s | 40890 | 13196 | 424 | 12672 | 8.2s | 28.7s | 14.7 |
| 4 | 1 | 32.8s | 36464 | 11378 | 83 | 10368 | 19.8s | 4.2s | 6.5 |

Aggregates: rounds n=6, all HTTP 200, **zero timeouts**; p50 37.6s, p95 133.9s.
Tokens in 81,379 / out 2,216 / cached 76,032 — **93% of input from the prefix
cache**. TTFT p50 8.2s, p95 31.5s. Harness overhead 0.07–0.59s per spin.

## Results — run A (degraded window, for contrast)

Rounds n=8: p50 50.3s, p95 150.3s; 2 rounds hit the 150s client timeout
(recorded as 500s); cache 89%; verified 1/5. The same PONG health check during
run A was also fast (2.1–3.5s) — the endpoint was up, but long generations were
being served slowly.

## Findings

1. **The transport and connection are healthy; the tail is the model.** Run B
   had no timeouts and 93% prefix-cache coverage. The verified spin completed a
   full 2-round tool loop, and its first round is the fastest agentic turn
   measured yet: **4.9s, TTFT 3.6s, 97.4 output tok/s**.
2. **The failure mode is model-side transcript role-play, not tool plumbing.**
   4/5 spins answered in one round with prose like
   `Tool read_file does not exists...` or `[Tool call: read_file] {"path":...}` —
   the model *simulates* the harness transcript instead of emitting
   `{"tool_calls":[...]}`. The gateway parsed nothing (correctly: there is no
   structured call), so no tool ran. In the same run, spin 3's round 1 did emit
   a proper call and the pipeline executed it end to end.
3. **Vendor-side generation variance dominates latency.** For comparable output
   sizes (83–806 tokens), generation time ranged 1.0s to 113.5s, and TTFT from
   3.6s to 31.5s. Nothing in Chimera's path (harness overhead ≤0.6s, lock wait 0,
   cache hits 92–97%) explains the spread.
4. **Prefix caching scales the agentic loop.** Every round re-sends the full
   flattened history (~36–41k chars), yet 9,216–21,888 input tokens per round are
   served from cache; the DOM transport has no equivalent.

## Comparison (same task, same account)

| | DOM (ADR-001 §2.5) | WebAPI run B (verified spin) | WebAPI run B (all) | WebAPI run A |
|---|---|---|---|---|
| spin wall | 49.6s p50 (27.3–53.7s) | **42.6s** | 48.4s p50 | 100.6s p50 |
| best round | 14.7s | **4.9s (97.4 tok/s)** | 4.9s | 7.1s (110 tok/s) |
| TTFT | n/a (post-turn stream) | 3.6–8.2s | 3.6–31.5s | 2.8–9.8s |
| cache | none | 92% | 93% | 89% |
| timeouts | n/a | 0 | 0 | 2 |
| per-tenant RSS | 1150 MB | 17.9 MB | 17.9 MB | 17.9 MB |

## Recommendations

1. **Spec-004 hardening is now the top blocker**: detect the simulated-transcript
   pattern (`does not exists`, `[Tool call:`, `Tool call:`) in a tools-enabled
   turn and retry once with a tightened instruction. That is the difference
   between 1/5 and a reliable loop.
2. Re-run this benchmark on a schedule (nightly) so vendor windows are
   distinguishable from regressions; only the verified spin should be used for
   latency claims.
3. If timeouts recur, map provider timeouts to `504` and make the webapi client
   timeout configurable (it is currently `RESPONSE_TIMEOUT + 30s`).

## Reproduce

```bash
./chimera auth status                    # session healthy, expiry reported
TRANSPORT=auto ./chimera                 # webapi by default with a session
python3 scripts/latency_probe.py --mode anv --agentic --n 5 \
  --fixture-chars 6000 --refresh-each --anv-yolo \
  --json logs/anv-webapi-rerun.json
```
