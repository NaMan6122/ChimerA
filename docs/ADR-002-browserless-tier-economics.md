# ADR-002 — Browserless transport and the subscriber-API tier

Date: 2026-09-16
Status: **Proposed** (awaiting ratification by the owner)
Relates to: ADR-001 (substrate & market), `specs/011-provider-transports.md`,
`docs/SAAS.md`, `docs/PRD.md`
Evidence: measured against a live qwen session on this machine. Raw runs in
`logs/qwenweb-*.json`, `logs/anv-webapi*.log`, `logs/probe-run.log`; spike in
`scripts/qwenweb-spike/`; implementation in `internal/providers/webapi/qwen/`.

---

## 1. Context

ADR-001 §6 separated the substrate from the product and planned two tiers: BYOA
browser isolation, plus a BYOK HTTP API tier as the escape hatch. It assumed the
browser was the *only* way to reach a consumer subscription, and therefore billed
the business in browser-hours (`SAAS.md` §1/§4).

Spec 011 then implemented a third path for qwen: **replaying the provider's own
web API over HTTP with an imported storage-state session** (cookies + access
token; anti-bot material generated in Go). It is measured, not hypothetical, and
it changes the cost model the pricing was built on.

## 2. What changed (measured)

| | Browser (dom) | Browserless (webapi) |
|---|---|---|
| PONG round trip, full gateway | 13.72 s | **3.91 s** |
| 30k-char prompt, full gateway | 15.66 s | **4.99 s** |
| Per-tenant RSS | 1150 MB (10 processes) | **17.9 MB** |
| Cold start | ~2.5 min | seconds |
| Concurrency | serialized per provider mutex | HTTP-parallel (vendor rate limits apply) |
| Prefix caching | none (ADR-001 §2.4) | **`cached_tokens` up to 96%** on repeated 30k prompts |
| Session custody | live Chromium profile | imported cookies + ~30-day JWT; browser only for login/refresh |
| Agentic tool loop through anv | 2–3 rounds, 27–54 s/spin | 2 rounds, **16.3 s best spin**, correct answers |

The agentic harness path was verified end to end: tool calls are parsed by the
gateway, executed by the harness, and answered correctly on both the browser and
browserless transports.

## 3. What did NOT change

- **Legal/ToS posture (ADR-001 §4 stands).** Automated access to consumer
  subscriptions is prohibited regardless of transport, and enforcement is real
  (Anthropic, Feb–Apr 2026). A faster transport does not make the activity
  permitted; it changes cost, not legality.
- **One tenant = one account.** WebAPI sessions cannot be pooled or shared any
  more than browser profiles can.
- **Reliability is best-effort, with new failure modes:** WAF challenges
  (`FAIL_SYS_USER_VALIDATE`), upstream `internal_error`, session expiry, and
  model-side tool-call confabulation (prompt-engineered tool calling degrades as
  the tool count grows). None of these have an SLA.
- **Support, not compute, is the dominant cost** (ADR-001 §3). WebAPI removes the
  VNC/re-login toil but adds session import/refresh toil.

## 4. Decision (proposed)

1. **Ship three transports as peers.** `TRANSPORT=auto|dom|webapi`: `auto`
   prefers `webapi` when a session exists, falls back to `dom` on startup failure
   (`init_failed`), and — opt-in `TRANSPORT_FALLBACK=dom` — at request time on
   WAF/401/403/429/5xx after one retry. The DOM path remains the universal
   fallback and the only path for providers without a webapi transport
   (ChatGPT, Claude today).
2. **Split the paid offering by substrate, not by provider.**
   - **Subscriber API tier (webapi):** no browser, 17.9 MB/tenant; priced per
     authenticated account-month + request bundle.
   - **Browser isolation tier (dom):** 1.3 GB/tenant, private VNC login; priced
     per browser-hour + requests as today.
3. **Stop treating "browser-hour" as the universal billing unit.** It is
   meaningless for webapi tenants, whose marginal infra cost is cents. Proposed
   starter prices (owner ratifies; pilot before committing):

   | Tier | Substrate | Proposed | Includes |
   |---|---|---|---|
   | Subscriber API Solo | webapi | **$9–12/mo** | 1 account, 10k req, auto-transport, session health |
   | Browser Solo | dom | $19/mo (unchanged) | 1 browser, 5k req, VNC login, maintained selectors |
   | Team | auto + warm fallback | $79/mo (unchanged) | webapi primary, browser fallback, 50k req |
   | Scale | any | custom | region pinning, SSO, audit, SLA (vendor-side limits excluded) |

4. **WebAPI onboarding is session import, not VNC.** A one-time browser login +
   export replaces the managed VNC loop. Required work: a `chimera auth import`
   path (today: `scripts/qwenweb-spike/cdp-export.mjs`), session-expiry alerts in
   `/v1/health/providers`, and per-account rate-limit governance.
5. **Density:** a 32 GB host holds ~20 DOM tenants **or** hundreds of webapi
   tenants. For webapi, the binding constraint is the vendor's per-account rate
   limit plus session-refresh support — not our hardware.

## 5. Consequences

- `SAAS.md` §1/§3/§4 gain the two-tier split; `PRD.md` latency claims and pricing
  are updated; README pricing line updated.
- Gross margin improves on webapi tenants; ARPU need not drop. The lower entry
  price is a go-to-market choice, not a cost floor.
- Already shipped: transport selection, fallback + `chimera_transport_fallback_total`,
  session validation at startup. Still required: import UX, expiry health,
  per-account concurrency governance.
- **Marketing constraint:** neither tier is an official API, and neither is
  "compliant automation". Keep the BYOA-only terms and disclaimers.

## 6. Open questions (not decided here)

- Whether to offer the managed webapi tier before a legal review, given the
  enforcement precedents (ADR-001 §4).
- Which provider to port next (DeepSeek needs an AWS WAF token + PoW wasm; Kimi
  is cookie auth) — each needs a logged-in session to even spike.
- Whether the browser tier is long-term product or a compatibility shim for
  ChatGPT/Claude.
- Pricing: validate the $9–12 range with 5–10 pilot users before publishing.

## 7. References

- ADR-001 §2.3/§2.4 — the DOM side of every table above.
- `specs/011-provider-transports.md` — transport abstraction + acceptance.
- `scripts/qwenweb-spike/` — browserless spike; `internal/providers/webapi/qwen/` — implementation.
- Raw evidence: `logs/qwenweb-pong.json`, `logs/qwenweb-30k.json`,
  `logs/anv-webapi.log`, `logs/anv-webapi-oldanv.log`.
