# ── Builder ───────────────────────────────────────────────────────
FROM golang:1.23-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /chimera ./cmd/chimera

# ── Runtime ───────────────────────────────────────────────────────
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Chromium + Xvfb + VNC + supervisor (mirrors Chimera stack but minimal for Go)
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    chromium-sandbox \
    xvfb \
    x11vnc \
    xterm \
    supervisor \
    net-tools \
    curl \
    ca-certificates \
    python3 python3-pip \
    && rm -rf /var/lib/apt/lists/* \
    && pip3 install --no-cache-dir websockify --break-system-packages \
    && mkdir -p /usr/share/novnc \
    && curl -L https://github.com/novnc/noVNC/archive/refs/tags/v1.4.0.tar.gz | tar -xz -C /usr/share/novnc --strip-components=1 \
    && ln -s /usr/share/novnc/vnc.html /usr/share/novnc/index.html

WORKDIR /app
COPY --from=builder /chimera /usr/local/bin/chimera
COPY .env.example .env.example
COPY docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Browser data + logs volumes
VOLUME ["/app/browser_data", "/app/logs"]

EXPOSE 8000 5900 6080

ENTRYPOINT ["/entrypoint.sh"]
