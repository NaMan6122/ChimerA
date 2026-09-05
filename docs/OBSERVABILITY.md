# Chimera Observability — Metrics, Health, Logs

## Endpoints

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /metrics` | none (like `/health`) | Prometheus exposition. In multi-tenant deploys, firewall it or scrape via the private network — it reveals per-provider request counts. |
| `GET /v1/health/providers` | Bearer / `x-api-key` | Parallel login probe per provider: `{object:"provider_health", data:[{provider, model, up, logged_in, last_success_unix, last_check_unix, last_error}]}`. Slow probes time out (15s budget); timed-out providers keep prior state with `last_error:"probe timeout"`. |

## Metrics

| Metric | Labels | Read it as |
|---|---|---|
| `chimera_chat_requests_total` | provider, model, code | request outcomes; burn-rate denominator |
| `chimera_response_duration_seconds` | provider, streaming | browser round-trip (buckets 1s–300s) |
| `chimera_provider_errors_total` | provider, reason (`send_error`, `lock_timeout`) | why turns fail |
| `chimera_lock_wait_seconds` | provider | contention; grows before `provider_busy` 504s |
| `chimera_selector_fallback_total` | provider | **UI-change radar** — first-choice selector missed |
| `chimera_echo_retry_total` | provider | scrape-quality signal (response contained the prompt) |
| `chimera_http_requests_total` / `chimera_http_duration_seconds` | method, path, code | gateway-level traffic |
| `chimera_provider_up` | provider | 1 after a passing login probe, else 0 |
| `chimera_provider_last_success_timestamp` | provider | freshness of last good turn |

## Logs

- `LOG_FORMAT=text` (default) keeps the historic `[name] LEVEL HH:MM:SS msg` layout.
- `LOG_FORMAT=json` emits one object per line (`ts, level, logger, msg, req_id?`) for Loki/ELK.
- Chat + streaming handlers tag lines with chi's request ID (`[req=…]` / `req_id`), so a slow turn in Grafana links to its exact log lines.

## Wiring Prometheus + Grafana + alerts

```bash
docker run -d -p 9090:9090 \
  -v $PWD/docker/prometheus.yml:/etc/prometheus/prometheus.yml \
  -v $PWD/docker/alerts.yml:/etc/prometheus/alerts.yml \
  prom/prometheus
# Grafana → import docker/grafana-dashboard.json
```

Alert rules (`docker/alerts.yml`): `ChimeraProviderDown` (login lost → re-login via VNC + `POST /v1/refresh`), `ChimeraSelectorFallbackSpike` (vendor UI change → inspect `selectors.go`), `ChimeraHighErrorRate`, `ChimeraSlowResponses`.

Multi-tenant: add one scrape target per tenant API port with a `tenant` label (see `docker/prometheus.yml`).

## SaaS mapping

- **Team dashboard**: Grafana panels above (rate, p50/p95, errors, fallback radar, up).
- **Solo/Team billing**: `chimera_chat_requests_total` is the usage counter Phase 2 metering builds on.
- **SLA proof**: `chimera_provider_up` + `last_success_unix` back the 99.5% target.
