#!/usr/bin/env python3
"""
ChimerA latency probe — measures gateway latency and attributes it to
browser round-trip vs gateway overhead vs concurrency lock contention.

Stdlib only. Two modes:

  http  : calls POST /v1/chat/completions directly (measures the gateway+browser
          path, plus TTFT when --stream is set).
  anv   : shells out to `anv --no-tui` (measures the full agentic harness path:
          anv system prompt + tool schemas -> ChimerA -> model).

Metrics deltas are scraped from /metrics before and after the run, so wall-clock
is split into browser round-trip and everything else.

Examples:
  python3 scripts/latency_probe.py --n 10
  python3 scripts/latency_probe.py --n 10 --concurrency 4
  python3 scripts/latency_probe.py --n 5 --stream
  python3 scripts/latency_probe.py --mode anv --n 3 --prompt "Reply with exactly: PONG"
"""
import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

DEFAULT_PROMPT = "Reply with exactly the word PONG and nothing else."


# ── metrics ────────────────────────────────────────────────────────────────
def root_of(base_url: str) -> str:
    return re.sub(r"/v1/?$", "", base_url.rstrip("/"))


def scrape(url: str, headers: dict, timeout: float = 5.0) -> dict:
    """Return {metric_name: {label_str: value}} for chimera_* counters/gauges."""
    out: dict = {}
    try:
        req = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read().decode("utf-8", "replace")
    except Exception:
        return out
    val_re = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([0-9eE.+-]+)$")
    for line in body.splitlines():
        if not line.startswith("chimera_"):
            continue
        m = val_re.match(line.strip())
        if not m:
            continue
        name, labels, val = m.group(1), m.group(2) or "", m.group(3)
        try:
            out.setdefault(name, {})[labels] = float(val)
        except ValueError:
            pass
    return out


def delta(after: dict, before: dict, name: str) -> dict:
    """Per-label difference for a metric name."""
    res = {}
    for label, v in after.get(name, {}).items():
        d = v - before.get(name, {}).get(label, 0.0)
        if d:
            res[label] = d
    return res


# ── stats ──────────────────────────────────────────────────────────────────
def pct(xs, p):
    if not xs:
        return float("nan")
    s = sorted(xs)
    k = max(0, min(len(s) - 1, int(round((p / 100.0) * (len(s) - 1)))))
    return s[k]


def fmt(x):
    return "n/a" if x != x else f"{x:.2f}s"


# ── http mode ──────────────────────────────────────────────────────────────
def one_http(args, results, idx):
    url = args.base_url.rstrip("/") + "/chat/completions"
    payload = {
        "model": args.model,
        "messages": [{"role": "user", "content": args.prompt}],
        "stream": bool(args.stream),
    }
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {args.token}",
    }
    body = json.dumps(payload).encode()
    t0 = time.perf_counter()
    ttft = None
    err = None
    nbytes = 0
    try:
        req = urllib.request.Request(url, data=body, headers=headers, method="POST")
        with urllib.request.urlopen(req, timeout=args.timeout) as r:
            if args.stream:
                # ChimerA emits an empty "role" chunk immediately, then blocks on
                # the browser round trip, then replays the text in 20-char chunks.
                # So the honest metric is time-to-first-CONTENT, not time to first byte.
                while True:
                    chunk = r.readline()
                    if not chunk:
                        break
                    nbytes += len(chunk)
                    if ttft is None and b"content" in chunk:
                        s = chunk.decode("utf-8", "replace").strip()
                        if s.startswith("data:"):
                            s = s[5:].strip()
                        if s and s != "[DONE]":
                            try:
                                obj = json.loads(s)
                                delta = (obj.get("choices") or [{}])[0].get("delta") or {}
                                if delta.get("content"):
                                    ttft = time.perf_counter() - t0
                            except Exception:  # noqa: BLE001
                                pass
            else:
                nbytes = len(r.read())
    except urllib.error.HTTPError as e:
        err = f"HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:200]}"
    except Exception as e:  # noqa: BLE001 — probe reports whatever happened
        err = f"{type(e).__name__}: {e}"
    wall = time.perf_counter() - t0
    results[idx] = {"idx": idx, "wall": wall, "ttft": ttft, "err": err, "bytes": nbytes}


# ── anv mode ───────────────────────────────────────────────────────────────
def one_anv(args, results, idx):
    env = dict(os.environ)
    env[args.api_key_env] = args.token
    cmd = ["anv", "--no-tui", "-p", args.provider, "-m", args.model, args.prompt]
    t0 = time.perf_counter()
    err = None
    out = ""
    try:
        p = subprocess.run(
            cmd, env=env, capture_output=True, text=True, timeout=args.timeout
        )
        out = (p.stdout or "") + (p.stderr or "")
        if p.returncode != 0:
            err = f"rc={p.returncode}: " + " ".join(out.split())[:200]
    except subprocess.TimeoutExpired:
        err = f"timeout after {args.timeout}s"
    except Exception as e:  # noqa: BLE001
        err = f"{type(e).__name__}: {e}"
    wall = time.perf_counter() - t0
    results[idx] = {"idx": idx, "wall": wall, "ttft": None, "err": err, "bytes": len(out)}


# ── runner ─────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser(description="ChimerA latency probe")
    ap.add_argument("--base-url", default=os.environ.get("CHIMERA_BASE_URL", "http://localhost:8000/v1"))
    ap.add_argument("--token", default=os.environ.get("CHIMERA_API_KEY", "chimera"))
    ap.add_argument("--api-key-env", default="CHIMERA_API_KEY", help="env var name anv reads (anv mode)")
    ap.add_argument("--provider", default="chimera", help="anv provider name (anv mode)")
    ap.add_argument("--model", default="chimera-qwen")
    ap.add_argument("--mode", choices=["http", "anv"], default="http")
    ap.add_argument("--n", type=int, default=5, help="total requests")
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--stream", action="store_true", help="measure TTFT (http mode)")
    ap.add_argument("--prompt", default=DEFAULT_PROMPT)
    ap.add_argument("--timeout", type=float, default=300.0)
    ap.add_argument("--json", metavar="PATH", help="also write raw results as JSON")
    args = ap.parse_args()

    headers = {"Authorization": f"Bearer {args.token}"}
    metrics_url = root_of(args.base_url) + "/metrics"

    print(f"ChimerA latency probe")
    print(f"  target : {args.base_url}  model={args.model}  mode={args.mode}"
          f"{'  stream=on' if args.stream else ''}")
    print(f"  load   : n={args.n}  concurrency={args.concurrency}")
    print(f"  prompt : {args.prompt[:70]!r} ({len(args.prompt)} chars)")
    print()

    before = scrape(metrics_url, headers)
    t_start = time.perf_counter()

    results = [None] * args.n
    sem = threading.Semaphore(args.concurrency)
    worker = one_anv if args.mode == "anv" else one_http

    def run(i):
        with sem:
            worker(args, results, i)

    threads = [threading.Thread(target=run, args=(i,)) for i in range(args.n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.perf_counter() - t_start

    after = scrape(metrics_url, headers)

    ok = [r for r in results if r and not r["err"]]
    bad = [r for r in results if r and r["err"]]
    walls = [r["wall"] for r in ok]
    ttfts = [r["ttft"] for r in ok if r["ttft"] is not None]

    print(f"  result : ok {len(ok)}/{args.n}  errors {len(bad)}   (run wall {elapsed:.2f}s)")
    if walls:
        print(f"  wall   : p50={fmt(pct(walls,50))}  p95={fmt(pct(walls,95))}  "
              f"min={fmt(min(walls))}  max={fmt(max(walls))}  mean={fmt(statistics.mean(walls))}")
    if ttfts:
        print(f"  ttfc   : p50={fmt(pct(ttfts,50))}  p95={fmt(pct(ttfts,95))}  "
              f"mean={fmt(statistics.mean(ttfts))}   ({len(ttfts)} samples)")
        print(f"           (time-to-first-CONTENT; ChimerA streams after the full "
              f"browser turn, so this ~= total latency)")
    if bad:
        print("  errors :")
        for r in bad[:5]:
            print(f"    - {r['err']}")
        if len(bad) > 5:
            print(f"    ... {len(bad)-5} more")

    # ── attribution ────────────────────────────────────────────────────────
    d_req = delta(after, before, "chimera_chat_requests_total")
    d_dur = delta(after, before, "chimera_response_duration_seconds_sum")
    d_dur_n = delta(after, before, "chimera_response_duration_seconds_count")
    d_lock = delta(after, before, "chimera_lock_wait_seconds_sum")
    d_lock_n = delta(after, before, "chimera_lock_wait_seconds_count")
    fb = delta(after, before, "chimera_selector_fallback_total")

    if d_req or d_dur:
        print()
        print("  attribution (from /metrics delta):")
        codes = {}
        for label, v in d_req.items():
            m = re.search(r'code="([^"]*)"', label)
            codes[m.group(1) if m else label] = int(v)
        if codes:
            print(f"    chat requests by code : {codes}")
        br_sum = sum(d_dur.values())
        br_n = sum(d_dur_n.values())
        if br_n:
            print(f"    browser round-trip    : n={int(br_n)}  sum={br_sum:.2f}s  mean={br_sum/br_n:.2f}s")
            if walls:
                print(f"    gateway overhead      : mean={statistics.mean(walls) - br_sum/br_n:.2f}s"
                      f"  (wall mean - browser mean)")
        if d_lock_n:
            lk = sum(d_lock.values()) / sum(d_lock_n.values())
            print(f"    lock wait (contention) : n={int(sum(d_lock_n.values()))}  mean={lk:.3f}s")
        if fb:
            print(f"    !! selector fallbacks  : {fb}   (vendor UI drift)")

    if args.json:
        with open(args.json, "w") as f:
            json.dump({"args": vars(args), "results": results,
                       "metrics_delta": {"requests": d_req, "browser_sum": d_dur}}, f, indent=2)
        print(f"\n  raw results -> {args.json}")

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
