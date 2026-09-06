# 006 — Selector packs (config, not rebuilds)

**Problem:** Every vendor UI redesign today requires editing `selectors.go` and
rebuilding/redeploying the binary. Selectors are data; they should ship as data.

**Goal:** versioned per-provider selector YAML, embedded in the binary, hot-reloadable,
overridable from disk.

## Design

- `selectors/<provider>.yaml` (new dir, one file per provider):
  ```yaml
  version: 3
  chat_input: ["#prompt-textarea", "..."]
  send_button: [...]
  stop_button: [...]
  assistant_message: [...]
  copy_button: [...]
  new_chat_button: [...]
  login_indicators: [...]
  ```
  Schema mirrors the existing Go `var` lists 1:1 (mechanical move, no renaming).
- `internal/providers/selectorpack/` (new, deps: `gopkg.in/yaml.v3` — pure Go):
  `Load(provider) (*Pack, error)` from embedded FS (`go:embed`); `LoadFile(path)`
  for disk override; `Version(provider)`. Validation: every key present and non-empty,
  else error naming the key.
- Wiring: `Base` gains a `Selectors *selectorpack.Pack`; provider clients read lists
  from it instead of package vars (keep the Go vars as the embedded source of truth
  during migration? No — single source: YAML is embedded, Go vars deleted when a
  provider migrates; migrate one provider per commit, chatgpt first).
- Override precedence: `SELECTORS_DIR` env → `<dir>/<provider>.yaml` wins over embedded
  (lets ops patch a redesign without rebuild).
- Ops: `GET /v1/selectors/version` (authed, `{provider: version}` map); `POST
  /v1/refresh` accepts `{"reload_selectors":true}` to re-read disk packs.
- Telemetry: add `pack_version` to the fallback log line (not a new label — version
  churn would explode cardinality).

## Files

- `selectors/*.yaml` (5 files, content moved from `selectors.go`)
- `internal/providers/selectorpack/*.go` + tests (parse, validate, override precedence)
- `internal/providers/*/` clients + `base.go` (consume packs)
- `internal/api/server.go` (version endpoint, refresh flag)
- `go.mod` (+yaml), `.env.example` (`SELECTORS_DIR`), `docs/PROVIDERS.md`

## Acceptance

- Binary behavior identical with embedded packs (existing tests green, no selector edits).
- Dropping a modified YAML in `SELECTORS_DIR` + refresh flag changes matching without restart.
- Malformed YAML → startup error naming file + key; missing key → validation error.
- `go vet ./... && go test ./...`.
