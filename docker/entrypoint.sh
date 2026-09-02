#!/bin/bash
set -e

# ── Chimera Docker Entrypoint ────────────────────────────────
# Mirrors Chimera's entrypoint: Xvfb + x11vnc + noVNC + chimera via supervisord
# Also does DNS pre-resolve for vendor domains (fixes Chrome DNS in Docker)

echo "[entrypoint] Chimera starting (provider=${PROVIDER:-chatgpt})"

mkdir -p /app/browser_data /app/logs /tmp

# Clean stale lock files from previous crash
for p in SingletonLock SingletonSocket SingletonCookie; do
  find /app/browser_data -name "$p" -delete 2>/dev/null || true
done

# DNS pre-resolve (like Chimera's Python socket hack)
echo "[entrypoint] Pre-resolving vendor domains..."
python3 - << 'PY' || true
import socket
hosts = ["chatgpt.com","cdn.oaistatic.com","claude.ai","chat.qwen.ai","chat.deepseek.com"]
for h in hosts:
    try:
        ip = socket.gethostbyname(h)
        print(f"  {h} -> {ip}")
        with open("/etc/hosts","a") as f:
            f.write(f"{ip} {h}\n")
    except Exception as e:
        print(f"  {h} failed: {e}")
PY

# VNC password
if [ -n "$VNC_PASSWORD" ]; then
  mkdir -p /root/.vnc
  x11vnc -storepasswd "$VNC_PASSWORD" /root/.vnc/passwd 2>/dev/null || true
  chmod 600 /root/.vnc/passwd
fi

# Verify chromium
if ! command -v chromium >/dev/null 2>&1; then
  echo "[entrypoint] WARNING: chromium not found, trying chromium-browser"
  ln -sf "$(which chromium-browser 2>/dev/null || echo /usr/bin/chromium)" /usr/bin/chromium 2>/dev/null || true
fi

echo "[entrypoint] Starting supervisord..."
exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf
