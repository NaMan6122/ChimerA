#!/usr/bin/env python3
"""
Quantify the agentic penalty on ChimerA's browser substrate.

An agentic run re-sends the whole conversation every turn. On an HTTP API the
constant prefix (system prompt + tool schemas) is served from the prompt cache;
in a browser it is re-typed into a textarea and re-processed by the model.

This measures per-turn latency for a conversation whose constant prefix stays
fixed and whose history grows, both statelessly and with X-Session-Id (which
lets ChimerA's server-side pruning drop already-sent history).

  python3 scripts/agentic_penalty_probe.py --turns 5 --prefix-chars 6000
"""
import argparse
import json
import os
import statistics
import time
import urllib.error
import urllib.request

# Stand-in for an agent's system prompt + tool schemas: large, constant, and
# identical on every turn (exactly what a prefix cache would absorb).
PREFIX_UNIT = (
    "Tool get_weather: returns conditions for a city. Params: city (string). "
    "Tool search_docs: full-text search. Params: query (string), limit (int). "
)


def prelude(n_chars):
    unit = PREFIX_UNIT
    return (unit * (n_chars // len(unit) + 1))[:n_chars]


def turn_payload(prefix, history, question):
    msgs = [{"role": "system", "content": prefix}]
    msgs.extend(history)
    msgs.append({"role": "user", "content": question})
    return msgs


def call(base_url, token, model, messages, timeout):
    body = json.dumps({"model": model, "messages": messages, "stream": False}).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions", data=body,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {token}"}, method="POST")
    t0 = time.perf_counter()
    status, err = None, None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            r.read()
    except urllib.error.HTTPError as e:
        status = e.code
        err = " ".join(e.read().decode("utf-8", "replace").split())[:150]
    except Exception as e:  # noqa: BLE001
        err = f"{type(e).__name__}: {e}"
    return time.perf_counter() - t0, status, err


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://localhost:8000/v1")
    ap.add_argument("--token", default=os.environ.get("CHIMERA_API_KEY", "chimera"))
    ap.add_argument("--model", default="chimera-qwen")
    ap.add_argument("--turns", type=int, default=5)
    ap.add_argument("--prefix-chars", type=int, default=6000)
    ap.add_argument("--tool-chars", type=int, default=0,
                    help="chars of simulated tool output appended per turn, so the "
                         "context actually grows the way an agentic loop's does. "
                         "0 = tiny history (measures a chat, not an agent)")
    ap.add_argument("--timeout", type=float, default=180.0)
    ap.add_argument("--session", action="store_true",
                    help="send X-Session-Id so the server prunes already-sent history")
    args = ap.parse_args()

    prefix = prelude(args.prefix_chars)
    mode = "with X-Session-Id (server prunes)" if args.session else "stateless (full prompt each turn)"
    print(f"agentic penalty probe — {mode}")
    print(f"  constant prefix: {len(prefix)} chars\n")

    history = []
    rows = []
    for i in range(1, args.turns + 1):
        q = f"Turn {i}: reply with exactly the number {i}."
        msgs = turn_payload(prefix, history, q)
        n_chars = sum(len(m["content"]) for m in msgs)
        headers_note = ""
        # threadID/session changes how much of the prompt the server re-sends
        wall, status, err = call(args.base_url, args.token, args.model, msgs, args.timeout)
        grew = f"{n_chars} chars"
        print(f"  turn {i}: {wall:6.2f}s  status={status}  prompt={grew} {err or ''}")
        rows.append({"turn": i, "wall": round(wall, 2), "chars": n_chars, "status": status})
        history.append({"role": "user", "content": q})
        history.append({"role": "assistant", "content": f"Thinking completed\n{i}"})
        if args.tool_chars:
            # A tool result in an agentic loop: large, and it stays in context
            # for every subsequent turn.
            history.append({"role": "tool", "tool_call_id": f"call_{i}",
                            "content": prelude(args.tool_chars)})
            history.append({"role": "assistant", "content": f"Observed result {i}."})

    ok = [r for r in rows if r["status"] == 200]
    if len(ok) >= 2:
        first, last = ok[0], ok[-1]
        growth = last["wall"] - first["wall"]
        print(f"\n  turn 1 -> turn {last['turn']}: {first['wall']:.2f}s -> {last['wall']:.2f}s "
              f"({growth:+.2f}s, {last['chars']/max(first['chars'],1):.1f}x prompt)")
        print(f"  mean turn: {statistics.mean(r['wall'] for r in ok):.2f}s")
        print("\n  On an API with prefix caching the constant prefix is not re-processed,")
        print("  so this curve stays near-flat. Here it does not.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
