# Chimera as Open-Source SaaS — Positioning & Serving Guide

> **TL;DR:** Don't sell "cheap ChatGPT API". Sell **managed BYOA gateway**: customer brings their own chat subscription, you run an isolated Chromium + OpenAI-compatible API for them.

## 1. Positioning

**Hero:** `Your chat subscriptions, as an OpenAI-compatible API. Self-hosted, private, MIT.`

* `Your app (OpenAI SDK / LangChain) → Chimera :8000 → your subscription → chatgpt.com | claude.ai | chat.qwen.ai | chat.deepseek.com | kimi.com`
* No API keys stored server-side. Browser tenants keep the session in `./browser_data/<provider>`; browserless tenants keep an exported storage state in `./auth_data/<provider>.json` (0600).
* Works with existing Pro/Plus subscriptions. No token arbitrage.

**Two substrates, one API** (spec 011, ADR-002):

| Substrate | How it reaches the subscription | When to use |
|---|---|---|
| Browser isolation (`TRANSPORT=dom`) | real Chromium, persistent profile, private VNC login | ChatGPT/Claude; providers without a web API; maximum compatibility |
| Browserless (`TRANSPORT=webapi`) | the provider's own web API, replayed with an imported session | Qwen today: 3–5x faster, ~65x lighter, no VNC in steady state |

**Self-host vs Managed:**

|  | Self-host (MIT, free) | Managed single-tenant (paid) |
|---|---|---|
| Run | `docker compose up` on your box | We run isolated containers/processes per tenant |
| Login | `:6080/vnc.html` (dom) or one-time session export (webapi) | private VNC URL (dom) / one-time export link (webapi) |
| Data | your disk | per-tenant volume, encrypted at rest |
| Updates | you pull | maintained selectors + transports, uptime, metrics |
| Cost | your hardware | per browser-hour (dom) or per account-month + requests (webapi) |

**What we don't do:** shared accounts, pooled credentials, reselling tokens, bypassing paywalls. One tenant = one account = one login, on either substrate.

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
* **Browserless tenants need no Chromium** (ADR-002): one storage-state file per tenant (`AUTH_DIR/<provider>.json`, mode 0600) and ~18 MB RAM. Login is one command — `chimera auth login` opens a browser once and stores the session; `chimera auth status` checks validity/expiry; `chimera auth import` ingests an external export. No persistent VNC session. A 32 GB host holds ~20 browser tenants **or** hundreds of browserless ones — the binding constraint is the vendor's per-account rate limit, not our hardware.
* Per-tenant: `API_TOKEN` (random 32B), `VNC_PASSWORD` (random, dom only), `API_PORT`, `VNC_PORT`, `NOVNC_PORT`.
* Front with Caddy/Traefik: `https://<tenant>.yourdomain.com/v1 → localhost:<API_PORT>`, basic auth or header `Authorization: Bearer <API_TOKEN>`.
* Meter at gateway: request count, browser-minutes (dom), error rate, transport fallbacks. Bill browser-hour for dom, account-month + requests for webapi — never tokens.

See `docker-compose.tenant.yml` + `scripts/new-tenant.sh`.

## 4. Pricing tiers (starter template)

Priced by substrate, never by tokens: browser tenants pay for the Chromium
(~1.3 GB RAM + VNC), browserless tenants pay per authenticated account
(~18 MB RAM, no browser process). See ADR-002 for the measured basis.

| Tier | Substrate | Price (proposed — owner ratifies) | What you get |
|---|---|---|---|
| **Hobby / OSS** | self-host | $0 | MIT code, Docker, community selectors, 1 provider |
| **Subscriber API Solo** | webapi | **$9–12/mo per account** | 1 account, 10k req/mo, auto-transport, session health, 7d logs |
| **Browser Solo** | dom | $19/mo per browser | 1 isolated browser, 1–2 providers, 5k req/mo, 7d logs, maintained selectors, private VNC login |
| **Team** | auto + warm fallback | $79/mo | webapi primary with warm browser fallback, 50k req/mo, 30d logs, metrics dashboard, priority selector fixes, 99.5% target |
| **Scale / BYO-cloud** | any | custom | VPC / Hetzner / Fly region pinning, SSO, audit logs, SLA (vendor-side limits excluded), custom transports (Perplexity, Minimax) |

Overage: `$5 / 10k requests` on webapi (its infra cost is cents/tenant — support
and session refresh are the real cost, which is why the entry tier is capped at
10k req rather than cheap-and-unlimited) and `$0.05 / browser-hour` on dom.

Why this works: the customer already pays $20/mo for Plus/Pro; $9–19 to use it in
code is an easy yes. The browserless tier undercuts the browser tier on price
because it undercuts it on cost — and it removes the VNC onboarding loop that
browser tenants need. You never compete on token price.

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

### D. Browserless tenant (webapi, ADR-002)
* No browser, no VNC: `./chimera auth login` once → `AUTH_DIR/qwen.json` (0600) → `TRANSPORT=auto`.
* One 18 MB process per tenant today (`AUTH_DIR` is process-wide); hundreds fit on
  one host. Routing many tenants/sessions through one shared gateway is a follow-up.
* `TRANSPORT_FALLBACK=dom` opts a tenant back into a warm browser for request-time
  fallback at the cost of that browser's RAM.

## 6. Ops checklist before public

* [ ] Per-tenant random `API_TOKEN`, `VNC_PASSWORD` (see script). Never commit.
* [ ] Per-tenant volumes: `tenants/<id>/browser_data`, `tenants/<id>/auth_data`, `tenants/<id>/logs`.
* [ ] Backups for sessions: browser profiles (cookies expire; re-login via VNC documented) and webapi storage states (JWT ~30 days; re-export flow documented).
* [ ] Prometheus: `response_duration`, `selector_fallback_hits`, `echo_count`, `transport_fallback_total`. Loki for gateway logs.
* [ ] Status page + selector changelog (vendors break monthly).
* [ ] `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, GH Actions `go vet + test + docker build`.
* [ ] `goreleaser` + `ghcr.io/<org>/chimera:latest` + versioned tags.
* [ ] Support runbook: webapi → `chimera auth login` (or `auth import`); dom → `POST /v1/refresh` → VNC re-login → wipe `SingletonLock` → restart.
* [ ] Session-expiry awareness: startup warning at <7 days (`chimera auth status` for on-demand); `/v1/health/providers` reports `logged_in=false` before a tenant sees a 401/WAF.
* [ ] Managed-tier legal review before charging for either substrate (ADR-001 §4, ADR-002 §6).

## 7. Pitch copy (reuse)

> Stop pasting API keys. Point your OpenAI SDK at your own subscription.
> Chimera is an 18MB Go binary that turns the chat plans you already pay for into `POST /v1/chat/completions` — streaming, tools, rate-limit, MIT. Self-host in 2 minutes, or let us host it for you: a real browser where the provider demands one, or a browserless replay (18MB, 3–5x faster) where it doesn't.
