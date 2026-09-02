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

# DNS pre-resolve for Chrome's built-in resolver (fixes DNS_PROBE_FINISHED_NXDOMAIN in Docker)
# Mirrors reference manager.py _resolve_domains_for_chrome — 17 domains
echo "[entrypoint] Pre-resolving vendor domains..."
python3 - << 'PY' || true
import socket
hosts = [
    "chatgpt.com","cdn.oaistatic.com","ab.chatgpt.com","auth.openai.com","auth0.openai.com",
    "openai.com","api.openai.com","platform.openai.com",
    "challenges.cloudflare.com","static.cloudflareinsights.com","tcr9i.chat.openai.com",
    "claude.ai","api.claude.ai","cdn.claude.ai","anthropic.com","www.anthropic.com",
    "chat.qwen.ai","www.kimi.com","chat.deepseek.com"
]
rules=[]
for h in hosts:
    try:
        ip = socket.gethostbyname(h)
        print(f"  {h} -> {ip}")
        with open("/etc/hosts","a") as f:
            f.write(f"{ip} {h}\n")
        rules.append(f"MAP {h} {ip}")
    except Exception as e:
        print(f"  {h} failed: {e}")
if rules:
    print(f"[entrypoint] host-resolver-rules: {len(rules)} domains")
    # Export for chimera manager to pick up via --host-resolver-rules flag if needed
    with open("/tmp/host_resolver_rules","w") as f:
        f.write(",".join(rules))
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
