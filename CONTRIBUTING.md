# Contributing to Chimera

Thanks for considering a contribution. The ground rules are short because the
codebase is small and opinionated.

## Ground rules

1. **No captcha evasion.** Solving human captchas programmatically is against
   this project's policy (PRD §9, internal). Anti-bot work that is *computed*
   (proof-of-work, nonces, request signing) is fine; slider/checkbox captchas
   are not.
2. **No reverse-engineering material in this repo.** Vendor protocol
   knowledge lives in a private internal repository (`docs/internal`
   submodule). Public contributions touch code and public docs only.
3. **Prompts never leave the machine.** No feature may ship prompts or
   session material to a third-party endpoint.

## Before you open a PR

```bash
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -race ./...
```

CI runs exactly these plus a Docker smoke test; both checks are required on
`main`. Keep PRs single-purpose; squash-merge is the house style.

## Adding a provider

The two paths are documented in [`docs/PROVIDERS.md`](docs/PROVIDERS.md):
a DOM provider copies the `qwen/` template (selectors + fallbacks), a
browserless transport implements `providers.Provider` and calls
`webapi.Register` in `init()`. New transports land as a numbered internal
spec *before* vendor code is written — open an issue describing the wire
protocol you found and we will spec it together.

## Project layout

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) and
[`docs/API.md`](docs/API.md). Measured claims (latency, memory) come from
[`scripts/latency_probe.py`](scripts/latency_probe.py) — re-measure rather
than assert.
