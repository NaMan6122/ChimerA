# Chimera as Open-Source SaaS — Positioning & Serving Guide

> **TL;DR:** Don't sell "cheap ChatGPT API". Sell **managed BYOA gateway**: customer brings their own chat subscription, you run an isolated Chromium + OpenAI-compatible API for them.

## 1. Positioning

**Hero:** `Your chat subscriptions, as an OpenAI-compatible API. Self-hosted, private, MIT.`

* `Your app (OpenAI SDK / LangChain) → Chimera :8000 → Your Chromium profile → chatgpt.com | claude.ai | chat.qwen.ai | chat.deepseek.com | kimi.com`
* No API keys stored server-side. Session lives in `./browser_data/<provider>` or per-tenant PVC.
* Works with existing Pro/Plus subscriptions. No token arbitrage.

**Self-host vs Managed:**

|  | Self-host (MIT, free) | Managed single-tenant (paid) |
|---|---|---|
| Run | `docker compose up` on your box | We run isolated container per tenant |
| Login | you open `:6080/vnc.html` | private VNC URL per tenant |
| Data | your disk | per-tenant volume, encrypted at rest |
| Updates | you pull | maintained selectors, uptime, metrics |
| Cost | your hardware | per browser-hour + requests |

**What we don't do:** shared accounts, pooled credentials, reselling tokens, bypassing paywalls. One tenant = one browser = one login.

## 2. Legal / ToS guardrails (read before charging)

Browser automation violates most vendor ToS (ChatGPT, Claude, etc.). This is a personal-automation bridge, not an official API.

* Require customers own the upstream account. Ban sharing / credential pooling in ToS.
* Show disclaimer in README, signup, and dashboard. See `README.md` Disclaimer.
* Expect bans / CAPTCHAs / selector breakage on vendor redesign. That's the SaaS value: we fix `selectors.go` fast.
* Get legal review before taking money. Provide abuse contact + takedown flow.

## 3. Architecture for SaaS (single-tenant isolation)

```
Tenant A ──► chimera-a :8101 ──► Chromium A (profile /tenants/a) ──► vendor login A
Tenant B ──► chimera-b :8102 ──► Chromium B (profile /tenants/b) ──► vendor login B
                  │                         │
                  └── Postgres (keys, usage) + Prometheus + Loki
```

* 1 container = 1 Chromium = 1 tenant. Never share `browser_data` across tenants.
* `PROVIDER=all` = 1 Chromium with page-per-provider for *that tenant only*.
* Per-tenant: `API_TOKEN` (random 32B), `VNC_PASSWORD` (random), `API_PORT`, `VNC_PORT`, `NOVNC_PORT`.
* Front with Caddy/Traefik: `https://<tenant>.yourdomain.com/v1 → localhost:<API_PORT>`, basic auth or header `Authorization: Bearer <API_TOKEN>`.
* Meter at gateway: request count, browser-minutes, error rate. Bill browser-hour, not tokens.

See `docker-compose.tenant.yml` + `scripts/new-tenant.sh`.

## 4. Pricing tiers (starter template)

Priced on browser isolation cost (~400MB RAM + CPU per tenant), not tokens.

| Tier | Price (example) | What you get |
|---|---|---|
| **Hobby / OSS** | $0 self-host | MIT code, Docker, community selectors, 1 provider |
| **Solo** | $19/mo per browser | 1 isolated browser, 1-2 providers, 5k req/mo, 7d logs, maintained selectors, private VNC login |
| **Team** | $79/mo per browser | `PROVIDER=all` (5 providers), 50k req/mo, 30d logs, metrics dashboard, priority selector fixes, 99.5% target |
| **Scale / BYO-cloud** | custom | VPC / Hetzner / Fly region pinning, SSO, audit logs, SLA, custom selectors (Perplexity, Minimax) |

Overage example: `$5 / 10k requests` + `$0.05 / browser-hour overage`. Adjust to your infra (Hetzner ~€5/4GB handles ~5-8 tenants).

Why this works: customer already pays $20/mo for Plus/Pro; $19 to use it in code is an easy yes. You never compete on token price.

## 5. Serving options

### A. Local / VPS single-tenant (today)
```bash
TENANT_ID=acme API_PORT=8101 VNC_PORT=5901 NOVNC_PORT=6081 ./scripts/new-tenant.sh
TENANT_ID=acme docker compose -f docker-compose.tenant.yml up -d --build
open http://localhost:6081/vnc.html  # tenant logs in once
curl -H "Authorization: Bearer <tenant-token>" http://localhost:8101/v1/models
```

### B. Fly.io (scale-to-zero per tenant)
* 1 Fly Machine per tenant from same image, `volume size 4GB` for `browser_data`.
* `fly machines run --region ams -e TENANT_ID=acme -e API_TOKEN=...`.
* Suspend when idle >10m, resume on `/health`. Keep VNC private via WireGuard.

### C. K8s (many tenants)
* `StatefulSet` or 1 `Deployment` per tenant + PVC `browser_data-<tenant>`.
* `readinessProbe: /health`, `resources: 500m/1Gi`, `shm_size: 2Gi`.
* Ingress: `acme.yourdomain.com → svc/chimera-acme:8000`.

## 6. Ops checklist before public

* [ ] Per-tenant random `API_TOKEN`, `VNC_PASSWORD` (see script). Never commit.
* [ ] Per-tenant volumes: `tenants/<id>/browser_data`, `tenants/<id>/logs`.
* [ ] Backups for profiles (cookies expire; re-login via VNC documented).
* [ ] Prometheus: `response_duration`, `selector_fallback_hits`, `echo_count`. Loki for gateway logs.
* [ ] Status page + selector changelog (vendors break monthly).
* [ ] `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, GH Actions `go vet + test + docker build`.
* [ ] `goreleaser` + `ghcr.io/<org>/chimera:latest` + versioned tags.
* [ ] Support runbook: `POST /v1/refresh` → VNC re-login → wipe `SingletonLock` → restart.

## 7. Pitch copy (reuse)

> Stop pasting API keys. Point your OpenAI SDK at your own browser.
> Chimera is a 18MB Go binary that turns the chat tabs you already pay for into `POST /v1/chat/completions` — streaming, tools, rate-limit, MIT. Self-host in 2 minutes, or let us host your isolated browser.
