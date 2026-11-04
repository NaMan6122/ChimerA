#!/bin/bash
# Log in to a provider and export its browserless session.
#
# A Chromium window opens. Log in there; the session is saved to
# AUTH_DIR/<provider>.json (mode 0600, password-equivalent) and then verified
# against the provider.
#
# Usage:
#   ./scripts/login.sh                 # the provider in .env (or chatgpt)
#   ./scripts/login.sh deepseek        # one provider
#   ./scripts/login.sh all             # every provider, in sequence
#   ./scripts/login.sh chatgpt fresh   # discard the browser profile first
#
# Why a browser is needed: the browserless transport replays the provider's own
# web API, but the credentials it replays (cookies + token) can only be earned by
# a real login. There is no API key to paste — the browser IS the auth step.
set -euo pipefail

cd "$(dirname "$0")/.."

TARGET="${1:-}"
MODE="${2:-}"

if [[ -z "$TARGET" ]]; then
  TARGET="$(grep -E '^PROVIDER=' .env 2>/dev/null | cut -d= -f2 || echo chatgpt)"
  TARGET="${TARGET:-chatgpt}"
fi

echo "──────────────────────────────────────────────────────────────"
echo " Chimera session login"
echo "──────────────────────────────────────────────────────────────"
echo
echo "A Chromium window will open. In that window:"
echo "  • log in with email/password, Microsoft, Apple, or a magic link"
echo "  • do NOT use \"Sign in with Google\" — Google blocks automated browsers"
echo "  • wait until the chat UI has fully loaded, then leave it alone"
echo
echo "The window closes by itself once the session is detected."
echo

if [[ "$TARGET" == "all" ]]; then
  exec go run ./cmd/chimera auth all
fi

if [[ "$MODE" == "fresh" ]]; then
  echo "Forcing a fresh login (clearing the stored browser profile for $TARGET)."
  rm -rf "browser_data/$TARGET"
  echo
fi

exec go run ./cmd/chimera auth login "$TARGET"

# After a successful login:
#   • confirm the session:  python3 scripts/provider_assessment.py
#   • start the gateway:    TRANSPORT=webapi PROVIDER=<name> ./chimera
#
# If the login reports a stale profile, re-run with:
#   ./scripts/login.sh <provider> fresh
