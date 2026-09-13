# ADR-001 — Substrate and market: browser automation vs HTTP APIs

Date: 2026-09-13
Status: **Proposed** (awaiting ratification by the owner)
Relates to: `docs/TECH_STACK_DECISION.md` (Go/rod, Accepted), `docs/PRD.md` v0.3,
`docs/SAAS.md`, `specs/010-eval-harness.md`
Evidence: all latency/throughput numbers below were measured against a live
`PROVIDER=qwen` gateway on this machine, using `scripts/latency_probe.py`,
`scripts/prompt_limit_probe.py`, `scripts/agentic_penalty_probe.py`, and anv
headless spins (`scripts/latency_probe.py --mode anv --agentic`).

---

## 1. Context

The question was whether to furnish Chimera as a SaaS, and whether Chromium-based
automation is the right substrate for that.

Two decisions already exist and constrain this one:

- `TECH_STACK_DECISION.md` accepted **Go + rod + real Chromium** as the runtime. That
  decision was about *language and process shape*, and it stands — this ADR does not
  reopen it.
- `SAAS.md` positions the product as a **managed BYOA gateway**: the customer brings
  their own subscription, one tenant = one browser = one login, billed "per
  browser-hour + requests". `PRD.md` §9 already accepts vendor-ToS risk as the
  dominant one and mitigates it with BYOA-only terms.

So the live question is narrower and more concrete than "is Chromium slow": **can the
browser substrate support the business SAAS.md describes, and where does it need an
HTTP path alongside it?**

---

## 2. What we measured

### 2.1 Latency, single client (n=6, warm session)

| | p50 | p95 | mean |
|---|---|---|---|
| Before selector fix | 21.0s | 30.1s | 22.3s |
| After selector fix | **10.4s** | **11.9s** | **10.6s** |

Gateway overhead is **0.04s** — the HTTP layer is not the problem. Per-request
composition after the fix:

```
send -> click         3.17s   browser DOM/selector work
click -> complete     6.33s   model generation (incl. Qwen's thinking pass)
complete -> received  1-2s    extraction + fixed sleep (qwen/client.go)
```

~40% of the remaining time is browser-bus protocol, not inference. This is a floor
set by the approach, not a tunable.

### 2.2 Throughput and concurrency

| Load | p50 | p95 | lock wait | throughput |
|---|---|---|---|---|
| n=6, c=1 | 10.4s | 11.9s | 0.00s | **339 req/hr** |
| n=6, c=3 | 10.8s | **68.6s** | 15.7s | **315 req/hr** |

Concurrency adds **queueing, not throughput**: a per-provider mutex serializes
`SendMessage`. Capacity is ~1 request per logged-in browser session, and
`MAX_CONCURRENT_PER_PROVIDER` cannot change that.

### 2.3 Resource footprint (measured, one live session)

- Chromium tree: **10 processes, ~1311 MB**; gateway itself 27 MB.
- **≈1.3 GB per tenant**, because `SAAS.md`'s "one tenant = one browser = one login"
  forbids sharing a profile.

### 2.4 Agentic workloads

The intended use (anv headless spins) failed at first for two separate reasons:

1. `MAX_PROMPT_CHARS=12000` rejected every tool-using prompt as HTTP 400 before the
   browser was touched. We proved the cap was a **phantom limit**: chat.qwen.ai
   accepted 20k / 40k / 80k chars live, and a 160k prompt was typed and sent too — it
   simply outran the 120s `RESPONSE_TIMEOUT`. Fixed (raised, rationale corrected).
2. With the cap lifted, the spin works end to end (`rc=0`, "PONG").

The corrected agentic measurement is the important one:

```
turn 1:  15.02s   prompt=   6040 chars
turn 2:  14.90s   prompt=  36118 chars    <- 6x prompt, same latency
turn 3:   0.00s   prompt=  66196 chars    400 message_too_long
turn 4:   0.00s   prompt=  96274 chars    400
turn 5:   0.00s   prompt= 126352 chars    400
```

**Latency is flat in prompt size** at these scales, so context processing is not
the cost. The real agentic blocker is structural: a conversation has nowhere to live
except in the textarea, so the whole history is re-flattened and re-sent every turn,
and any agentic loop walks into the guard by turn 3.

### 2.5 Agentic harness end-to-end (anv headless spins, with tools)

The intended workload — anv driving ChimerA as its model provider — needed a third
fix before tools actually ran. anv streams, and the SSE path replayed the model's
tool-call JSON as plain `content` and always ended `finish_reason: "stop"`; the
harness therefore never executed a tool call. The non-streaming path already parsed
calls. Fixed: the emulated stream now emits OpenAI-shaped `delta.tool_calls` and
`finish_reason: "tool_calls"` (`internal/api/server.go`, regression test
`TestChatCompletionsStreamToolCalls`).

Five independent tool-using spins (browser refreshed before each spin; 6k-char
fixture files read through the harness's own `read_file` tool; all 5 answers
verified; `scripts/latency_probe.py --mode anv --agentic --refresh-each
--anv-yolo`):

| stage | prompt | wall |
|---|---|---|
| round 1 — tool call | 30.1k chars | 14.7s p50 |
| round 2 — tool result | 34.6k chars | 16.4s p50 |
| round 3 — answer | 34.7k chars | 20.5s p50 |
| full spin (2–3 rounds) | — | **49.6s p50** (27.3s min, 53.7s max) |
| harness overhead (anv startup, tool exec, folding) | — | **0.11s mean** |

All 13 rounds returned 200 — no lock waits, no selector fallbacks. Two readings:

- The harness is free; the cost is browser+model round trips, and per-round cost is
  nearly flat from 30k to 35k chars, confirming §2.4.
- anv's artifact folding keeps tool output out of the prompt (≈13k chars offloaded
  per spin), holding each round at ~30–35k chars. But history is still re-flattened
  every round (~+4.4k chars), so a long agentic loop reaches the 60k guard around
  round 7–8, not turn 3 — better than §2.4's stateless probe, still bounded.

Raw: `logs/anv-agentic.log`, `logs/anv-agentic.json`.

### 2.6 Retraction — the prefix-cache claim

I previously claimed that the missing prompt cache compounds across an agentic run
until it times out, and that this alone disqualified the browser path. **That claim
does not survive measurement and is withdrawn.** At 6x the prompt (6k → 36k chars)
latency was unchanged (15.02s → 14.90s). The missing cache is real (§2.4) and it does
cap long conversations, but it is *not* currently a latency driver, and using it as
the headline argument against the substrate was wrong. The headline argument is
throughput and cost per tenant (§2.2, §2.3), not caching.

---

## 3. Market findings

Research into the hosted LLM-gateway market (sources in §9). Load-bearing claims
independently verified:

- **Consolidating and crowded.** Stripe agreed to acquire **OpenRouter** (Aug 2026);
  Unify, well funded, **pivoted out of routing entirely**.
- **Being commoditised from above.** **Cloudflare AI Gateway is free on all plans**
  and passes inference through at **0% markup**; Vercel also charges zero markup.
  Verified against Cloudflare's docs, 29 Aug 2026.
- **Router take-rates are ~5%** (OpenRouter 5.5%, Requesty 5%) and inference prices
  fall 3–5x/year, so per-token revenue erodes mechanically.
- **Durable margin sits in enterprise self-hosted + governance/compliance**, not PLG
  token markup (LiteLLM ~$7M ARR with ~10 staff; Portkey $15M Series A).

**But this applies to token routers, and `SAAS.md` does not describe a token router.**
Chimera as specified bills *per browser-hour*, i.e. it sells hosted compute, not a
percentage of tokens. That sidesteps token-margin compression, and it changes the
comparison set from "OpenRouter et al." to something closer to managed hosting, whose
economics are:

- ~20 tenants per 32 GB host at 1.3 GB each;
- a 32 GB dedicated host runs roughly $60–150/month, so **per-tenant infra cost is
  single-digit dollars/month at density**;
- against a per-seat price, gross margin is viable **if** density is reached and
  support is contained.

Two honest caveats. First, tenants who use more than ~340 requests/hour cannot be
served at all — acceptable for the personal/small-team segment `PRD.md` targets,
disqualifying for a high-volume API business. Second, the dominant cost is likely to
be **support, not compute**: selector rot and VNC re-login are recurring per-tenant
events, and `specs/006/010` are the mitigation, not a solved problem.

---

## 4. Legal exposure

Distinguish two activities, because conflating them was an error in my earlier
analysis:

- **Reselling access to your own accounts** — prohibited everywhere, and it is what
  the market research's legal conclusion describes. `SAAS.md` explicitly rules this
  out ("What we don't do: shared accounts, pooled credentials, reselling tokens").
- **BYOA — automating the customer's own account, in isolation, for them** — this is
  what is actually proposed. It avoids the resale prohibition.

BYOA does **not** avoid the second prohibition, which every relevant vendor has
separately: **automated/scripted access as such.** Anthropic's consumer terms prohibit
automated non-human access and have been *enforced* (account bans and legal requests
that caused OpenCode to drop Claude Pro/Max support). OpenAI's terms prohibit
programmatic access and circumventing protective measures. Qwen prohibits non-
interactive/automated use on its personal plan. `PRD.md` §9 already names this as the
top risk; this ADR's contribution is that the research finds enforcement is real and
precedented, not theoretical.

Consequence: legal risk is **reduced but not removed** by BYOA, it is concentrated on
the *customer's own account* (the party who gets banned), and it is the single largest
uncertainty in the plan. It cannot be engineered away, only positioned honestly around.

---

## 5. Reconciliation with existing docs

- `TECH_STACK_DECISION.md` — unaffected; Go + rod remains right for driving a real
  browser. The selector work in this branch (commit `1ee7310`) validates that choice.
- `PRD.md` §9 ("say 5–30s everywhere; position for reasoning steps, not autocomplete")
  — **confirmed correct by measurement.** 10.4s p50 sits inside that band. The PRD's
  latency expectation was sound and the engineering has now met it. It should be
  updated from 5–30s to 10–12s p50 for a warm session.
- `SAAS.md` — its billing unit (browser-hour) is the right one given §2.2/§2.3, and
  its BYOA guardrails are the correct mitigation given §4. No change to positioning;
  **add the quantitative limits** so customers self-select: ~340 req/hr/session, ~10s
  p50, one session per tenant.
- `specs/010-eval-harness.md` — more valuable than it looked, because selector rot is
  the dominant operating cost (§3), and the harness is the mechanism that turns a
  vendor redesign into a fixture update rather than an outage.

---

## 6. Decision

**Proposed: separate the substrate from the product, and serve two segments on one
codebase.**

1. **Keep the browser substrate for the BYOA product** exactly as `SAAS.md` describes.
   It is coherent, its unit economics work at personal/small-team volume, and its
   limits (§2.2, §2.3) are acceptable there. This is not a compromise — 340 req/hr is
   far beyond individual use.
2. **Add an HTTP API provider** behind the existing `providers.Provider` interface, so
   the same gateway can serve customers who have API keys, with no browser at all.
   This is the tier that can scale horizontally, and it is the escape hatch if
   automation enforcement (§4) becomes fatal.
3. **Do not position either tier as a token router.** That market is commoditised
   (§3); the defensible assets here are the unification layer, the hardening, and the
   metering — not a take-rate on tokens.

This supersedes the unstated assumption that the browser substrate must also carry
any future high-volume SaaS. It does not supersede `TECH_STACK_DECISION.md`.

---

## 7. Consequences

**Accepted:** ~10s p50 and ~340 req/hr/session are product constraints, documented
rather than fought. Selector rot is a permanent operating cost with a permanent
mitigation (`specs/006`, `specs/010`). Each tenant costs ~1.3 GB. Vendor enforcement
remains the top existential risk and is outside our control.

**Requires work:** the HTTP provider needs the `Provider` interface refactored — it
currently leaks browser concerns via `Init(page *rod.Page, …)` and DOM-shaped
`ExtractResponse()`, and assumes stateful tab sessions via `NewChat()`. Split into a
transport-agnostic core plus browser-specific extensions. Spec pending.

**Becomes possible:** horizontal scaling for API-key tenants; a single SDK surface
across both tiers; caching and failover applied where they actually pay (API tier).

---

## 8. Open questions (deliberately not decided here)

- **Which API vendors** to support first, and whether to be BYOK-only.
- **Whether the managed tier is worth operating at all**, given support cost may
  exceed compute cost (§3). A pilot would settle this better than analysis.
- **Pricing.** `SAAS.md` says per browser-hour; no number has been validated.
- **The browser-vs-API comparison number.** A spike was specified but is **blocked on
  credentials** (no usable API key on this machine, no local model). §2.1 and §3 are
  therefore the best available evidence, and the spike should be run before any
  pricing commitment.

---

## 9. References

Measured (this repo):
- `scripts/latency_probe.py`, `scripts/prompt_limit_probe.py`,
  `scripts/agentic_penalty_probe.py`; raw runs in `logs/`.
- Selector fix: commit `1ee7310` (p50 21.0s → 10.4s; `/v1/refresh` 89s → 2.3s;
  fallbacks 3/request → 0).

Market (verified):
- Stripe newsroom — agrees to acquire OpenRouter (19 Aug 2026):
  https://stripe.com/newsroom/news/stripe-agrees-to-acquire-openrouter
- Cloudflare AI Gateway pricing — free on all plans, 0% markup on inference
  (verified 29 Aug 2026): https://developers.cloudflare.com/ai-gateway/reference/pricing/
- Vercel AI Gateway — zero markup: https://vercel.com/i/vercel-ai-gateway-vs-cloudflare-ai-gateway
- Portkey Series A: https://www.globenewswire.com/news-release/2026/02/19/3241385/0/en/Portkey-Raises-15M-Series-A
- Unify pivot away from routing:
  https://www.upstartsmedia.com/p/unify-ai-gtm-pipeline-40-million-raise
- Noah Intelligence — "a tollbooth, not a model bet":
  https://noah-news.com/stripes-7bn-openrouter-buy-is-a-tollbooth-not-a-model-bet/

Legal:
- Anthropic consumer terms (automated access prohibition) and enforcement against
  OpenCode: https://www.theregister.com/2025/06/27/anthropic_claude_code/
- OpenAI terms: https://openai.com/terms
- Qwen personal plan (no automated/non-interactive use):
  https://docs.qwencloud.com/token-plan/personal/token-plan-personal-overview
