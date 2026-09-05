# Chimera Metering — Usage, Quotas, Tenants

## Tenants

Credentials resolve to tenants via `internal/auth`:

| Config | Effect |
|---|---|
| `API_TOKEN=sek` | single key → tenant `default` |
| `API_TOKENS=acme=a1,basic=b2` | per-tenant keys (union with `API_TOKEN`) |
| neither set | auth disabled, everything bills to `default` |

Malformed `API_TOKENS` (missing `=`, empty side, duplicate token) fails startup —
a typo must never silently open or misattribute a gateway. Present the key via
`Authorization: Bearer`, `X-Api-Key`, or `Anthropic-Api-Key` (bare or Bearer form).

## Usage recording

Every provider attempt (success, `send_error`, `lock_timeout`) appends one row to
SQLite (`METER_DB`, default `./logs/usage.db`; empty disables → `/v1/usage` is 503):

```
ts, tenant, provider, model, streaming, prompt_chars, completion_chars, latency_ms, code
```

Validation 400s and quota 429s are not recorded (no browser time consumed).
Recording is fail-open: a sick database logs loudly but never 500s live traffic.

## Quotas

`QUOTA_MONTHLY_REQUESTS` (default 0 = unlimited) caps each tenant's rows in the
current UTC month. Breaches return `429 {"error":{"type":"quota_exceeded", ...}}`
before any browser work. Check-then-act races may admit a couple of extra turns
under burst concurrency — quotas are billing guardrails, not hard real-time caps.

## API

`GET /v1/usage?from=<RFC3339>&to=<RFC3339>` (defaults: current UTC month,
inclusive window). Callers only see their own tenant:

```json
{"object":"usage","tenant":"acme","from":"...","to":"...",
 "requests":123,"prompt_chars":45678,"completion_chars":34567,"errors":2,
 "by_provider":{"chatgpt":100,"qwen":23},"quota_monthly":5000}
```

## SaaS mapping

- **Solo/Team billing**: `requests` (+ chars for weight) feed Stripe usage records.
- **Enforcement**: set `QUOTA_MONTHLY_REQUESTS` to the plan cap (5k Solo, 50k Team).
- **Tenant provisioning**: `scripts/new-tenant.sh` should append `tenant=token` to
  the deployment's `API_TOKENS` (per-process gateway today holds one registry;
  true multi-tenant routing is Phase 3 scope).
- **Dashboards**: `chimera_chat_requests_total` mirrors recorded rows; alert if
  `rate(rows) != rate(metric)` over 15m (drop detector).
