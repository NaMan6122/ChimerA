#!/bin/bash
# One-time login helper — launches Chimera in login mode (no API)
# Mirrors Chimera's login.sh / first_login.py
set -e

PROVIDER=${1:-${PROVIDER:-chatgpt}}

echo "[login] Launching Chimera for one-time login (provider=$PROVIDER)"
echo "[login] A Chromium window will open. Log in to ${PROVIDER} using email/password, Microsoft, Apple, or magic link."
echo "[login] DO NOT use Google OAuth (blocked in automation). Close the window when done."
echo ""

PROVIDER=$PROVIDER go run ./cmd/chimera --login 2>&1 | head -n 50

# Note: --login flag not yet implemented in cmd/chimera; fallback is just normal run
echo ""
echo "[login] If you saw 'Provider ready', your session is saved to ./browser_data/$PROVIDER"
echo "[login] Now run: ./chimera  (or docker compose up)"
