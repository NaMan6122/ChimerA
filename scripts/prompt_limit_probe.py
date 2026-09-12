#!/usr/bin/env python3
"""
ChimerA prompt-length limit probe.

Finds the largest prompt (in chars) chat.qwen.ai actually accepts, by bypassing
ChimerA's own MAX_PROMPT_CHARS guard (set it high when you start the gateway)
and reading the *vendor's* reaction instead.

Outcome of each trial is classified from ChimerA's error text:
  ok        HTTP 200 — vendor accepted
  vendor    "send button disabled" / "send disabled" / "chat input rejected"
            -> the vendor UI refused it: this is the real ceiling
  guard     "Prompt exceeds" -> ChimerA's own cap, not the vendor (raise it)
  timeout   response never completed within the request budget
  other     anything else (auth, selector, 5xx)

Strategy: exponential ramp to bracket the first failure, then binary search.
An "ok" trial costs a real model round trip (~5-30s), so ask for a 1-word reply.

Prereqs: ChimerA running with MAX_PROMPT_CHARS raised, and a LOGGED-IN session.

  MAX_PROMPT_CHARS=500000 ./chimera
  python3 scripts/prompt_limit_probe.py
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

# Long enough to satisfy the largest probe length we might request.
FILLER = ("The quick brown fox jumps over the lazy dog. " * 20000)
TAIL = "\n\nIgnore the text above. Reply with exactly: OK"


def classify(status, body, err):
    b = (body or "").lower()
    if err == "timeout":
        return "timeout"
    if status == 200:
        return "ok"
    if "prompt exceeds" in b or "message_too_long" in b:
        return "guard"
    if ("send button disabled" in b or "send disabled" in b
            or "chat input rejected" in b or "message too long" in b):
        return "vendor"
    if status == 400:
        return "other400"
    return "other"


def reset_thread(args):
    """Start a fresh browser conversation so trials don't accumulate context.

    ChimerA only calls provider.NewChat() from /v1/refresh -- the chat path
    appends to whatever thread is open. Without this, each trial inherits the
    previous trial's history and the measurement drifts.
    """
    body = json.dumps({"model": args.model}).encode()
    req = urllib.request.Request(
        args.base_url.rstrip("/") + "/refresh", data=body,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {args.token}"},
        method="POST")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            r.read()
        return True
    except Exception as e:  # noqa: BLE001
        print(f"    (thread reset failed: {e})")
        return False


def trial(args, n_chars):
    """Send a prompt of exactly n_chars. Returns (outcome, detail, wall)."""
    msg = (FILLER[: max(0, n_chars - len(TAIL))] + TAIL)[:n_chars]
    payload = json.dumps({
        "model": args.model,
        "messages": [{"role": "user", "content": msg}],
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        args.base_url.rstrip("/") + "/chat/completions",
        data=payload,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {args.token}"},
        method="POST",
    )
    t0 = time.perf_counter()
    status, body, err = None, "", None
    try:
        with urllib.request.urlopen(req, timeout=args.timeout) as r:
            status, body = r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status = e.code
        body = e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        err = "timeout" if "timed out" in str(e).lower() else str(e)
    wall = time.perf_counter() - t0
    outcome = classify(status, body, err)
    detail = " ".join((body or err or "").split())[:160]
    return outcome, detail, wall


def main():
    ap = argparse.ArgumentParser(description="Find chat.qwen.ai's real prompt limit")
    ap.add_argument("--base-url", default=os.environ.get("CHIMERA_BASE_URL", "http://localhost:8000/v1"))
    ap.add_argument("--token", default=os.environ.get("CHIMERA_API_KEY", "chimera"))
    ap.add_argument("--model", default="chimera-qwen")
    ap.add_argument("--timeout", type=float, default=180.0)
    ap.add_argument("--start", type=int, default=500, help="assumed-good lower bound")
    ap.add_argument("--max", type=int, default=256000, help="give up searching above this")
    ap.add_argument("--json", metavar="PATH", default="prompt_limit_result.json")
    ap.add_argument("--no-reset", action="store_true",
                    help="skip the /v1/refresh between trials (faster, but trials "
                         "accumulate context and results drift)")
    args = ap.parse_args()

    print("ChimerA prompt-length limit probe")
    print(f"  target: {args.base_url}  model={args.model}")
    log = []

    def run(n):
        if not args.no_reset:
            reset_thread(args)
        outcome, detail, wall = trial(args, n)
        log.append({"chars": n, "outcome": outcome, "detail": detail, "wall": round(wall, 2)})
        mark = {"ok": "OK  ", "vendor": "VENDOR-REJECT", "guard": "GUARD",
                "timeout": "TIMEOUT", "other": "OTHER", "other400": "OTHER400"}[outcome]
        print(f"  {n:>7} chars  ->  {mark:<13} {wall:6.2f}s  {detail[:90]}")
        return outcome

    print("\n[1/3] sanity: is a small prompt accepted?")
    if run(args.start) != "ok":
        print("\n  ABORT: a short prompt did not succeed. Likely not logged in, or")
        print("  MAX_PROMPT_CHARS is still low. Fix that before probing.")
        with open(args.json, "w") as f:
            json.dump(log, f, indent=2)
        return 1

    print("\n[2/3] ramp up until the vendor refuses")
    good, bad = args.start, None
    n = args.start
    while n < args.max:
        n = min(n * 2, args.max)
        outcome = run(n)
        if outcome == "ok":
            good = n
            if n == args.max:
                break
            continue
        if outcome in ("vendor", "guard", "timeout"):
            bad = n
            break
        bad = n
        break

    if bad is None:
        print(f"\n  No refusal up to {args.max} chars — vendor accepted everything.")
        print("  Raise --max to find the ceiling (costly: each trial is a real turn).")
        with open(args.json, "w") as f:
            json.dump(log, f, indent=2)
        return 0

    print(f"\n[3/3] binary search in ({good}, {bad})")
    while bad - good > max(200, good // 20):
        mid = (good + bad) // 2
        outcome = run(mid)
        if outcome == "ok":
            good = mid
        else:
            bad = mid

    print(f"\n  largest accepted: {good} chars")
    print(f"  first refused   : {bad} chars")
    print(f"  => vendor ceiling is around {good}-{bad} chars "
          f"(~{good // 4}-{bad // 4} tokens at 4 chars/token)")
    print(f"  ChimerA's guard sits at MAX_PROMPT_CHARS (default 12000)")
    with open(args.json, "w") as f:
        json.dump({"largest_ok": good, "first_bad": bad, "trials": log}, f, indent=2)
    print(f"\n  full log -> {args.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
