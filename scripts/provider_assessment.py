#!/usr/bin/env python3
"""
ChimerA provider assessment probe.

Answers one question per provider: does it work, and if not, WHY — with the
failure classified so a dead session is not mistaken for a broken transport.

The distinction matters. Every webapi transport can fail in two unrelated ways:

  * the TRANSPORT is broken  (wrong protocol, wrong PoW, missing token)
  * the SESSION is dead      (cookie jar challenged, bearer expired)

A dead session is an operational problem (re-export), not an engineering one, and
conflating the two is how a working transport gets "fixed" for no reason. This
probe separates them by testing the session BEFORE the transport.

Usage:
  python3 scripts/provider_assessment.py                 # all providers
  python3 scripts/provider_assessment.py --provider chatgpt
  python3 scripts/provider_assessment.py --provider chatgpt deepseek
  python3 scripts/provider_assessment.py --gateway http://localhost:8000/v1

Exit code is 0 when every assessed provider is HEALTHY, 1 otherwise.
"""
import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request

SCRIPT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AUTH_DIR = os.path.join(SCRIPT_ROOT, "auth_data")

# Providers with a browserless transport, in assessment order.
WEBAPI_PROVIDERS = ["chatgpt", "deepseek", "qwen"]

# The model id the gateway exposes per provider (must match ModelID()).
MODEL_IDS = {
    "chatgpt": "chimera-chatgpt",
    "deepseek": "chimera-deepseek",
    "qwen": "chimera-qwen",
}

PROBE_PROMPT = "Reply with exactly the word PONG and nothing else."


# ── session liveness ─────────────────────────────────────────────────────────
def session_age_hours(path):
    try:
        return (time.time() - os.path.getmtime(path)) / 3600.0
    except OSError:
        return None


def load_session(provider):
    path = os.path.join(AUTH_DIR, f"{provider}.json")
    if not os.path.exists(path):
        return None, path, "no session file"
    try:
        with open(path, "r", encoding="utf-8") as f:
            doc = json.load(f)
    except Exception as e:  # noqa: BLE001
        return None, path, f"unreadable: {e}"
    return doc, path, None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Do not follow redirects.

    A 302 to the provider's login page otherwise surfaces as a 200 carrying HTML,
    which reads as a healthy session when the credential is actually dead. That
    false positive is exactly what this probe exists to prevent.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_opener = urllib.request.build_opener(_NoRedirect)


def _classify_http_error(e):
    """Map an HTTPError to (verdict, detail)."""
    body = e.read(8192).decode("utf-8", "replace")
    low = body.lower()
    if "cf_chl" in low or "turnstile" in low or "just a moment" in low:
        return "challenged", f"HTTP {e.code} Cloudflare challenge"
    if e.code in (401, 403):
        if "token_expired" in low or "expired" in low:
            return "expired", f"HTTP {e.code} token expired"
        return "expired", f"HTTP {e.code} auth rejected"
    return "unknown", f"HTTP {e.code}: {body[:120]}"


def check_session(provider):
    """Probe the provider's credential. Returns (verdict, detail).

    verdict is one of: alive, challenged, expired, unknown.

    The probe uses the STORED BEARER, not the cookie jar. A cookie jar can be
    Cloudflare-challenged while the bearer is still valid, and conversely a 200
    from an unauthenticated endpoint proves nothing — so the bearer is the only
    credential whose acceptance is evidence of liveness.
    """
    doc, path, err = load_session(provider)
    if doc is None:
        return "unknown", err or "no session"

    token = doc.get("access_token") or ""
    if not token or token == "cookie-session":
        # Cookie-custody providers mint the bearer from the jar; probe the jar.
        cookies = doc.get("cookies") or {}
        if not cookies:
            return "unknown", "no bearer and no cookies"
        jar = "; ".join(f"{k}={v}" for k, v in cookies.items())
        url = {
            "chatgpt": "https://chatgpt.com/api/auth/session",
            "deepseek": "https://chat.deepseek.com/api/v0/user/info",
            "qwen": "https://chat.qwen.ai/api/user/info",
        }.get(provider)
        if not url:
            return "unknown", "no session probe for this provider"
        req = urllib.request.Request(url, headers={"Cookie": jar, "User-Agent": user_agent()})
        try:
            with _opener.open(req, timeout=25) as r:
                # Read the WHOLE body: /api/auth/session is ~7.6 KB and a short
                # read truncates it mid-JSON, which reads as "not JSON" rather
                # than as the real verdict.
                body = r.read().decode("utf-8", "replace")
                if body.lstrip()[:1].lower() == "<":
                    return "challenged", f"HTTP {r.status} returned HTML (challenge/login page)"
                # The decisive signal is the `error` field, not the presence of a
                # token: a dead ChatGPT session still returns HTTP 200 WITH an
                # accessToken, but sets error=RefreshAccessTokenError because the
                # cookie can no longer mint a fresh one. WARNING_BANNER is NOT a
                # signal — it accompanies healthy sessions too.
                try:
                    doc = json.loads(body)
                except json.JSONDecodeError:
                    return "unknown", f"HTTP {r.status} non-JSON body ({len(body)} bytes)"
                if doc.get("error"):
                    return "expired", f"HTTP {r.status} error={doc['error']}"
                if not doc.get("accessToken"):
                    return "expired", f"HTTP {r.status} no accessToken"
                return "alive", f"cookie jar minted a token (HTTP {r.status})"
        except urllib.error.HTTPError as e:
            return _classify_http_error(e)
        except Exception as e:  # noqa: BLE001
            return "unknown", f"{type(e).__name__}: {e}"

    # Bearer-custody: call an authenticated endpoint with the stored token.
    url, header = {
        "deepseek": ("https://chat.deepseek.com/api/v0/user/info", "Authorization"),
        "qwen": ("https://chat.qwen.ai/api/user/info", "Authorization"),
    }.get(provider, (None, None))
    if not url:
        return "unknown", "no bearer probe for this provider"
    req = urllib.request.Request(url, headers={header: f"Bearer {token}",
                                              "User-Agent": user_agent()})
    try:
        with _opener.open(req, timeout=25) as r:
            body = r.read(4096).decode("utf-8", "replace")
            # A 200 carrying HTML is the login page, not an authenticated
            # response — the token is dead even though the status says ok.
            if body.lstrip()[:1].lower() == "<":
                return "expired", f"HTTP {r.status} returned HTML (login page)"
            return "alive", f"bearer accepted (HTTP {r.status}, JSON)"
    except urllib.error.HTTPError as e:
        return _classify_http_error(e)
    except Exception as e:  # noqa: BLE001
        return "unknown", f"{type(e).__name__}: {e}"


def user_agent():
    return ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")


# ── gateway turn ─────────────────────────────────────────────────────────────
def gateway_turn(base_url, token, model, timeout=180):
    """One non-streaming turn. Returns (ok, detail, elapsed, answer)."""
    url = base_url.rstrip("/") + "/chat/completions"
    payload = {"model": model,
               "messages": [{"role": "user", "content": PROBE_PROMPT}],
               "stream": False}
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {token}"}, method="POST")
    t0 = time.perf_counter()
    try:
        with _opener.open(req, timeout=timeout) as r:
            body = json.loads(r.read().decode("utf-8", "replace"))
        elapsed = time.perf_counter() - t0
        answer = ""
        try:
            answer = body["choices"][0]["message"]["content"] or ""
        except Exception:  # noqa: BLE001
            pass
        return True, f"HTTP {r.status}", elapsed, answer
    except urllib.error.HTTPError as e:
        elapsed = time.perf_counter() - t0
        body = e.read(2048).decode("utf-8", "replace")
        return False, f"HTTP {e.code}: {body[:200]}", elapsed, ""
    except Exception as e:  # noqa: BLE001
        elapsed = time.perf_counter() - t0
        return False, f"{type(e).__name__}: {e}", elapsed, ""


# ── chromium check ───────────────────────────────────────────────────────────
def chromium_running():
    """True when a Chromium/Chrome process is alive (webapi must have none)."""
    try:
        out = subprocess.run(["pgrep", "-fl", "Chromium|chrome-headless|headless_shell"],
                             capture_output=True, text=True, timeout=10).stdout
    except Exception:  # noqa: BLE001
        return None
    return bool(out.strip())


# ── per-provider assessment ──────────────────────────────────────────────────
def assess(provider, base_url, token, turns):
    print(f"\n{'=' * 72}")
    print(f"  {provider.upper()}")
    print(f"{'=' * 72}")

    doc, path, _ = load_session(provider)
    age = session_age_hours(path)
    if age is not None:
        print(f"  session : {path}")
        print(f"  age     : {age:.1f} hours")
        if doc:
            print(f"  cookies : {len(doc.get('cookies') or {})}  "
                  f"access_token={len(doc.get('access_token') or '')} chars")

    # Stage 1 — session liveness, independent of the transport.
    verdict, detail = check_session(provider)
    print(f"  stage 1 : session probe -> {verdict} ({detail})")

    if verdict == "challenged":
        print("  VERDICT : SESSION CHALLENGED — the cookie jar is Cloudflare-blocked.")
        print("            Re-export with `chimera auth login`. The transport is NOT")
        print("            implicated; no code change can fix this.")
        return {"provider": provider, "stage": "session", "verdict": verdict,
                "detail": detail, "turns": []}
    if verdict == "expired":
        print("  VERDICT : SESSION EXPIRED — the bearer is dead. Re-export needed.")
        return {"provider": provider, "stage": "session", "verdict": verdict,
                "detail": detail, "turns": []}
    if verdict != "alive":
        print("  VERDICT : SESSION UNKNOWN — cannot assess the transport.")
        return {"provider": provider, "stage": "session", "verdict": verdict,
                "detail": detail, "turns": []}

    # Stage 2 — the transport, through the gateway.
    model = MODEL_IDS[provider]
    print(f"  stage 2 : gateway turn (model={model}, n={turns})")
    results = []
    for i in range(turns):
        ok, detail, elapsed, answer = gateway_turn(base_url, token, model)
        mark = "ok " if ok else "!! "
        print(f"    {mark}turn {i + 1}: {elapsed:6.2f}s  {detail[:90]}")
        if ok:
            print(f"        answer: {answer[:70]!r}")
        results.append({"ok": ok, "detail": detail, "elapsed": elapsed, "answer": answer})

    good = [r for r in results if r["ok"]]
    if not good:
        print("  VERDICT : TRANSPORT FAILING — the session is alive but the turn is")
        print("            rejected. This IS an engineering problem.")
        return {"provider": provider, "stage": "transport", "verdict": "failing",
                "detail": results[-1]["detail"], "turns": results}

    lat = sorted(r["elapsed"] for r in good)
    p50 = lat[len(lat) // 2]
    print(f"  VERDICT : HEALTHY — {len(good)}/{len(results)} turns ok, "
          f"p50={p50:.2f}s, min={lat[0]:.2f}s, max={lat[-1]:.2f}s")
    return {"provider": provider, "stage": "transport", "verdict": "healthy",
            "detail": f"{len(good)}/{len(results)} ok, p50={p50:.2f}s",
            "turns": results, "p50": p50}


def main():
    ap = argparse.ArgumentParser(description="ChimerA provider assessment")
    ap.add_argument("--provider", nargs="*", default=WEBAPI_PROVIDERS,
                    help="providers to assess (default: all webapi providers)")
    ap.add_argument("--gateway", default="http://localhost:8000/v1")
    ap.add_argument("--token", default=os.environ.get("CHIMERA_API_KEY", "chimera"))
    ap.add_argument("--turns", type=int, default=3, help="turns per provider")
    ap.add_argument("--json", metavar="PATH", help="write raw results as JSON")
    args = ap.parse_args()

    print("ChimerA provider assessment")
    print(f"  gateway : {args.gateway}")
    print(f"  providers: {', '.join(args.provider)}")
    print(f"  turns   : {args.turns} per provider")

    # A webapi run must have no Chromium; a browser process means the gateway
    # fell back to DOM and the measurement is not a browserless one.
    chrome = chromium_running()
    if chrome:
        print("\n  NOTE: a Chromium process is running — the gateway may have fallen")
        print("        back to the DOM transport, so these numbers are not browserless.")

    report = {"gateway": args.gateway, "providers": {}}
    for p in args.provider:
        report["providers"][p] = assess(p, args.gateway, args.token, args.turns)

    print(f"\n{'=' * 72}")
    print("  SUMMARY")
    print(f"{'=' * 72}")
    healthy = 0
    for p, r in report["providers"].items():
        icon = {"healthy": "OK ", "failing": "FAIL"}.get(r["verdict"], " -- ")
        print(f"  {icon} {p:10s} {r['verdict']:10s} {r['detail'][:58]}")
        if r["verdict"] == "healthy":
            healthy += 1

    total = len(report["providers"])
    print(f"\n  {healthy}/{total} providers healthy")
    print("  healthy  = session alive AND turn succeeds")
    print("  challenged/expired = re-export the session (`chimera auth login`); not a code bug")
    print("  failing  = session alive but turn rejected; this is a transport bug")

    if args.json:
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(report, f, indent=2)
        print(f"\n  wrote {args.json}")

    return 0 if healthy == total else 1


if __name__ == "__main__":
    sys.exit(main())
