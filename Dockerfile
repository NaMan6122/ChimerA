# ── Builder ───────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN mkdir -p /out/bin /out/auth_data /out/logs \
    && chmod 0700 /out/auth_data \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
       go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/bin/chimera ./cmd/chimera \
    && for prov in chatgpt gpt qwen deepseek kimi gemini grok glm; do \
         ln -sf chimera "/out/bin/chimera-${prov}"; \
       done

# ── Slim Runtime (~22 MB browserless webapi image) ────────────────
# Build with: docker build --target slim -t chimera:slim .
FROM gcr.io/distroless/static-debian12:nonroot AS slim

WORKDIR /app
COPY --from=builder --chown=65532:65532 /out/auth_data /app/auth_data
COPY --from=builder --chown=65532:65532 /out/logs /app/logs
COPY --from=builder /out/bin/ /usr/local/bin/
COPY .env.example /app/.env.example

ENV TRANSPORT=webapi \
    API_HOST=0.0.0.0 \
    API_PORT=8000

VOLUME ["/app/auth_data", "/app/logs"]

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/chimera", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/chimera"]

# ── Browser Runtime (Chromium + Xvfb + noVNC universal image) ─────
# Default stage so `docker build -t chimera .` preserves VNC support.
FROM debian:bookworm-slim AS browser

ENV DEBIAN_FRONTEND=noninteractive

# Chromium + Xvfb + VNC + supervisor + apt websockify (no pip bloat)
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
    python3 \
    python3-websockify \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /usr/share/novnc \
    && curl -fsSL https://github.com/novnc/noVNC/archive/refs/tags/v1.4.0.tar.gz \
       | tar -xz -C /usr/share/novnc --strip-components=1 \
    && ln -s /usr/share/novnc/vnc.html /usr/share/novnc/index.html

WORKDIR /app
COPY --from=builder /out/bin/ /usr/local/bin/
COPY .env.example .env.example
COPY docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && mkdir -p /app/auth_data /app/browser_data /app/logs && chmod 0700 /app/auth_data

VOLUME ["/app/auth_data", "/app/browser_data", "/app/logs"]

EXPOSE 8000 5900 6080

HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/chimera", "-healthcheck"]

ENTRYPOINT ["/entrypoint.sh"]
