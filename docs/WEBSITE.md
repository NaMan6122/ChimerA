# Chimera Website Brief — for a website-builder agent

Build a marketing + docs website for **Chimera**, an open-source (MIT) Go program that
turns browser chat subscriptions into an OpenAI-compatible API. All facts below are
authoritative; do not invent numbers, providers, or features not listed here.

## Brand

- Name: **Chimera** · Tagline: **"Your chat subscriptions, as an API."**
- Voice: direct, technical, honest. No hype words (never "blazing", "revolutionary",
  "enterprise-grade"). State limits plainly — candor is the brand.
- Dark developer aesthetic. Accent: terminal green. Mono font for code/numbers.

## Facts (use verbatim, never embellish)

- 18MB static Go binary; 18–30MB gateway RAM; 60ms cold start; 22k rps on `/health`.
- Python alternative it replaces: ~650MB image, 900ms cold start.
- Providers: ChatGPT, Claude, Qwen, DeepSeek, Kimi (+ `PROVIDER=all` pooled mode).
- Browser turns take **5–30s**. Say this on every page with a CTA.
- Auth: `Authorization: Bearer`, `x-api-key`, `anthropic-api-key`.
- Endpoints: `POST /v1/chat/completions` (JSON + SSE streaming), `GET /v1/models`,
  `GET /v1/health/providers`, `GET /v1/usage`, `GET /metrics`. Planned: `/v1/responses`,
  `/v1/messages` (Anthropic shape), MCP stdio mode.
- Pricing: Hobby $0 self-host · Solo $19/mo/browser (5k req) · Team $79/mo/browser
  (50k req, all providers, dashboard) · Scale custom. Billed per browser, not per token.
- Legal footer on every page: "Browser automation may violate vendor Terms of Service.
  Use your own accounts. Not affiliated with OpenAI, Anthropic, or others."

## Sitemap (5 pages)

1. `/` **Home** — hero (tagline + `docker compose up` snippet + "curl returns an answer"
   demo GIF placeholder `assets/demo.gif`), numbers strip (18MB / 60ms / 5 providers /
   5–30s with honesty note), "Two ways to use it" cards (AS THE MODEL via `base_url`
   swap → code sample; AS A TOOL via MCP → Claude Code config snippet, mark "coming
   soon" if unshipped), provider logo row (text names only, no vendor logos),
   self-host-vs-managed table, pricing teaser (3 cards + Scale), FAQ preview (4),
   footer (GitHub, docs, disclaimer).
2. `/docs` **Docs** — quickstart (binary + Docker paths), configuration table (mirror
   `.env.example`: PROVIDER, API_TOKEN(S), QUOTA_MONTHLY_REQUESTS, METER_DB, LOG_FORMAT…),
   API reference (each endpoint: method, auth, example request/response curl + Python),
   providers matrix, observability (metrics list + dashboard screenshot placeholder),
   troubleshooting (re-login via VNC, `POST /v1/refresh`, expired sessions).
3. `/pricing` **Pricing** — full tier table (feature matrix: providers, requests/mo,
   logs retention, dashboard, selector-fix SLA, support), browser-hour overage note,
   FAQ (why per-browser not per-token? what counts as a request? what happens over quota?
   → 429 `quota_exceeded` before browser work), CTA: deploy-template buttons.
4. `/changelog` **Changelog** — version list (v0.1 gateway … v0.3 metering), each with
   3-bullet highlights; "selector breakage report" subsection updated weekly.
5. `/playground` (later) — reserved route; link the self-hosted `:playground` concept.

## Components

- Sticky nav: logo, Docs, Pricing, Changelog, GitHub stars button, "Self-host" CTA.
- Code tabs (curl / Python / JS) on all API examples. Copy button on every snippet.
- Comparison table: Chimera vs official APIs (cost model, latency, setup) vs Python
  gateway ports (size, boot, RAM) — numbers only from Facts above.
- Honesty callout component (amber): latency + ToS notes, reused across pages.

## SEO / social

- Title: "Chimera — Your Chat Subscriptions as an API (Open Source)".
- Description: "Self-hosted Go gateway: use ChatGPT, Claude, Qwen, DeepSeek and Kimi
  subscriptions from any OpenAI-compatible client. No API keys. MIT licensed."
- OG image: terminal showing curl → streamed answer + "18MB · MIT" badge.
- Launch assets to request from maintainer: `assets/demo.gif` (VNC login → curl → SSE),
  `assets/dashboard.png` (Grafana), `assets/architecture.svg` (app → :8000 → Chromium → vendors).

## What NOT to build

- No signup/billing flows (tiers are informational until managed pilot).
- No vendor logos, no "official partner" language, no uptime-percentage claims.
- No performance claims beyond Facts; always pair speed claims with the 5–30s note.
