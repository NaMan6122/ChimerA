# Chimera — Product Requirements Document

**Status:** living doc · **Version:** 0.3 · **Owner:** TheGate team
**One-liner:** Your chat subscriptions, as an OpenAI-compatible API. Bring your own
ChatGPT / Claude / Qwen / DeepSeek / Kimi login — no API keys, session stays in your Chromium.

## 1. Problem

- Developers pay $20/mo for chat subscriptions **and** per-token API fees to use
  intelligence in code. Two bills for the same brain.
- Several capable models (Qwen, Kimi) have weak or no developer APIs in many regions.
- Existing browser-automation gateways are 650MB Python images with 900ms cold starts,
  single-tenant-hostile, unmetered, and unobservable — unusable as a product.

## 2. Goals / non-goals

**Goals**
1. Turn any supported browser chat session into an OpenAI-compatible API (`/v1`).
2. Run as an 18MB static Go binary: `docker compose up` → curl in 2 minutes.
3. Be operable as a product: Prometheus metrics, per-tenant metering + quotas,
   login-health probes, maintained selector packs.
4. Meet agent developers where they live: OpenAI SDK, `/v1/responses`,
   Anthropic Messages shape, MCP stdio tools.

**Non-goals**
- Competing with official APIs on latency/throughput (browser turns take 5–30s).
- Reselling tokens, shared/pooled credentials, bypassing paywalls.
- Vision/file/image-gen parity in v1 (scaffolded, not wired).
- Multi-tenant shared browsers: one tenant = one Chromium = one login, always.

## 3. Users

| Persona | Job | Shape used |
|---|---|---|
| Indie hacker / agent builder | kill the API bill, use Plus/Pro in code | self-host, `base_url` swap |
| Self-hoster / homelab | private AI on own hardware, no data to third parties | Docker, VNC login |
| Agent developer | escalate hard reasoning steps to frontier chat models | MCP `chat` tool inside Claude Code et al. |
| Small team | shared subscription-backed endpoint with quotas + dashboard | managed single-tenant |

## 4. Value propositions

1. **Zero token spend** — spend the subscription you already pay for.
2. **Models without APIs** — Qwen, Kimi and friends behind one OpenAI shape.
3. **Private by construction** — prompts live in your browser profile, not our cloud.
4. **Observable scraping** — fallback radar, echo counters, login health: breakage is
   measured, alerted, and fixed fast (this is the paid-tier moat).
5. **Agent-native** — OpenAI + Responses + Anthropic shapes + MCP tools.

## 5. Feature set

### Shipped (v0.1–v0.3)
- 5 providers (chatgpt, claude, qwen, deepseek, kimi) + `PROVIDER=all` pooled mode.
- `POST /v1/chat/completions` (JSON + emulated SSE), prompt-injected tool calling,
  session continuity (`X-Session-Id`), `/v1/refresh`, `/v1/models`, `/health`.
- Auth: Bearer / `x-api-key` / `anthropic-api-key`; global rate limit; body caps;
  lock timeouts (no infinite hangs); pre-flight long-prompt guard.
- Telemetry: `/metrics`, `/v1/health/providers`, selector-fallback + echo counters,
  JSON logs with request IDs, Grafana dashboard + alert rules.
- Metering: per-tenant SQLite usage, monthly quotas (429 `quota_exceeded`),
  `GET /v1/usage`, multi-key `API_TOKENS`.
- Single-tenant Docker Compose + `scripts/new-tenant.sh` scaffolding.

### Planned (see `specs/`)
- Tool-call hardening (validated names, unique IDs) → Responses API → Anthropic
  Messages passthrough → MCP stdio mode → response cache → cross-provider failover
  → selector packs → provider supervisor → playground → eval harness.

## 6. UX flows

**First run (self-host):** `cp .env.example .env && docker compose up` → open
`:6080/vnc.html`, log in once → `curl /v1/models` → point SDK at `:8000/v1`. Done.
**Managed:** signup → private VNC URL → log in → token + dashboard. Re-login nudge
arrives via health state (`needs_login`), never as a surprise 500.
**Agent:** `base_url` swap for full-model use; MCP `chat` tool for escalation use.

## 7. Pricing (starter, per browser — not per token)

Hobby $0 self-host · Solo $19/mo (1–2 providers, 5k req) · Team $79/mo
(`PROVIDER=all`, 50k req, dashboard, priority selector fixes) · Scale custom.
Full rationale in `docs/SAAS.md`.

## 8. Success metrics

- Time-to-first-200 (compose → curl): < 5 min.
- Selector-breakage detection time (fallback spike → alert): < 15 min.
- p95 browser turn per provider; quota-breach support tickets ≈ 0 (clear 429 copy).
- MCP installs; docs → playground conversion (time on page, no backend needed).

## 9. Risks

- **Vendor ToS / bans:** mitigate with BYOA-only terms, human-behavior pacing,
  cache (fewer loads), honest docs + disclaimer. Never fight detection aggressively.
- **Selector rot:** fallback radar + packs + eval harness (specs 006/010) make fixes
  config pushes, and the breakage report doubles as marketing.
- **Latency expectations:** say 5–30s everywhere; position for reasoning steps, not autocomplete.

## 10. Roadmap checkpoints

- v0.4 protocols (specs 004→002→003→001) · v0.5 reliability (005→006→007) ·
  v0.6 delight (008→009→010) · v1.0: all green + managed pilot tenants.
