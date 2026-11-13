# Changelog

All notable changes to Chimera are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions link to
their GitHub releases. Cutting a release includes updating this file —
see [docs/DISTRIBUTION.md](docs/DISTRIBUTION.md).

## [Unreleased]

### Added — providers

- **Zhipu GLM browserless transport** (`chimera-glm`, internal spec 024):
  replays chatglm.cn's web API with refresh-token custody and one MD5
  header-signature (static public secret — no PoW, no VM). Live-verified at
  spike level before implementation; the parser is tested against the
  captured fixtures. Native server-side `conversation_id` continuity — the
  no-history invariant holds without transcript folding. `GLM_WEB_MODEL`
  selects the assistant (`auto`, `deepresearch`, or a raw 24-char
  assistant id); reasoning is parsed and counted, never persisted.

### Added — session tooling

- **Vault export**: `chimera sessions export --all -o <dir>` writes one
  runbook per session plus an `index.md` table (`--html` for HTML pages);
  idempotent on re-export.
- **`/sessions` picker (TUI)**: browse and view any preserved session from
  the chat UI, display only — the live conversation is untouched. Bare
  `/sessions` opens the picker; `/sessions <id>` restores directly.
- **Rehydrate — reuse without resumption**: `chimera-spin -from-session <id>`
  seeds a fresh upstream conversation with a budgeted digest of a preserved
  transcript (task, per-round digest, previous final answer);
  `chimera sessions rehydrate <id>` prints the same block standalone.
  Nothing is re-executed and the provider is never asked to remember.

## [v0.3.0](https://github.com/NaMan6122/ChimerA/releases/tag/v0.3.0) — 2026-11-13

### Added — agent hardening (SWE readiness)

- **`monitor` tool** (adapted from Anvaya-CLI spec-185): run a command in the
  background — `start` returns an id immediately, `read` polls the log tail +
  status, `stop` kills, `list` enumerates; ≤8 live monitors, 10s–1h timeout
  clamp, persistent until stopped or run end.
- **`git` tool**: read-only subcommands (status/diff/log/blame/…) with no
  shell in the path, so arguments can never execute as a second command.
- **Command budget**: `run_command`/`git` bounded by `-cmd-timeout`
  (default 5m, was a hard 60s); partial output still returns on timeout.
- **Context ledger**: the harness keeps a persisted working copy of the whole
  session (task, rounds, tool calls, results, replies) under
  `<sessions-dir>/ledgers/`, tagged by provider-side generation — visible
  context sizing without ever re-sending the copy. At `-compact-chars`
  (default 80k estimated chars) the loop **compacts**: fresh provider-side
  session (`<session>-cN`) seeded with a deterministic digest; harness-side
  session, transcript and ledger unchanged.
- **Resilience**: mid-run tool refusals get one re-asserting nudge instead of
  silently becoming the final answer; the client retries 429/5xx/connection
  errors (1s/3s backoff, ≤3 attempts) — permanent errors surface immediately.
- **Safety**: `-safe` advisory denylist for `run_command`/`monitor`; tool
  path confinement now resolves symlinks (spec-021 acceptance closed);
  `-max-rounds` default 8 → 16.
- Spec'd internally (022).

### Added — local history

- **Gateway session recording**: every provider turn through the
  OpenAI-compatible API (Open WebUI, Chatbox, curl, harnesses) is appended to
  a local transcript alongside spin and TUI history — one file per
  conversation, tagged `gateway` + provider, readable with the existing
  `chimera sessions list | show | export`.
- Stateless clients: a conversation's growing history lands in one transcript
  (stable digest of the opening); a client `X-Session-Id` names the file
  directly.
- `SESSIONS_DIR` env (default `~/.local/share/chimera/sessions`; empty
  disables). Failed turns are preserved as error notes; pre-flight rejections
  (too long, quota, rate limit) are not recorded.
- Multi-turn runbooks: later user messages now render inside the run; the
  first remains the Task.

### Fixed

- **DeepSeek empty replies**: DeepSeek's thinking stream now delivers the
  answer inside a `{"p":"response/fragments"}` patch with no `"o"` op (a full
  replacement snapshot, verified live 2026-11-13) instead of the old
  `"o":"APPEND"` increment. The parser dropped it, so thinking-mode turns
  populated reasoning but returned an empty message. The snapshot is now
  applied (fixture + regression test from the live capture), and a turn that
  is still empty after the fresh-chat retry fails with an error instead of a
  blank reply.

## [v0.2.0](https://github.com/NaMan6122/ChimerA/releases/tag/v0.2.0) — 2026-11-06

### Added — providers

- **Gemini browserless transport** — replays gemini.google.com's batchexecute
  API with cookie custody (`__Secure-1PSID` + rotating `1PSIDTS` + SAPISIDHASH),
  `SNlM0e`/`cfb2h` scraped from `/app`, and three-id continuation.
  Live-verified: a follow-up call recalls context that never crossed the wire.
- **Grok browserless transport** — replays grok.com's REST chat API with
  `sso` cookie custody and the accepted `x-statsig-id` fallback. Measured
  deviation, documented: Grok's model backend is stateless per turn, so the
  provider keeps the transcript and folds it (the no-history invariant does
  not hold on this path).
- `chimera-gemini` / `chimera-grok` model ids; `GEMINI_WEB_MODEL`,
  `GEMINI_WEB_THINKING`, `GROK_WEB_MODEL` selectors.

### Added — MCP stdio server

- `chimera --mcp`: Model Context Protocol over stdin/stdout — hand-rolled
  JSON-RPC 2.0, zero dependencies, protocol negotiation (2025-06-18).
- Tools: `chat{message, provider?, session_id?}`, `refresh{provider?}`,
  `providers`. Session continuity via the same manager as the HTTP API;
  metering keyed by the MCP client's name; provider errors are tool results
  (`isError`), not protocol errors; quota hints appended where the vendor
  exposes them.
- **stdout purity proven by test**: a chatty provider cannot corrupt the
  protocol stream.

### Added — harness (`chimera-spin`)

- **Task presets**: `-preset summarize|flashcards|eli12|cite-check|meeting-notes`
  — named prompt scaffolds with explicit output conventions; `-preset help`
  prints every scaffold.
- **`read_pdf` tool** — papers and slides via `pdftotext` (poppler), page-range
  slicing, workspace-confined, budgeted.
- **Compare mode**: `-models a,b,c` runs one task across providers and renders
  a side-by-side report with per-run stats and a qualitative agreement note.

### Added — sessions as readable runbooks

- `chimera sessions list | show | export` — the observe-only JSONL transcripts
  become indexed, human-readable Markdown runbooks (metadata, task, rounds
  with tool calls, final answer). Sessions are preservation, not resumption:
  the provider side expires; what you keep is the run.
- **Tags** (`chimera-spin -tag …`, `list --tag`), **full-text search**
  (`sessions search`), and **standalone HTML export**.

### Added — operability

- **`chimera doctor`**: sessions, provider session files, gateway ports,
  `pdftotext` availability, disk, `.env` sanity — OK/WARN/FIX with actionable
  hints; `--probe` reads live `/v1/health/providers`.
- **Provider quota surfaces**: where a vendor exposes quota (Grok),
  `/v1/health/providers` reports `remaining`/`total`.
- **Chat UI cookbook**: [`docs/CHAT-UI.md`](docs/CHAT-UI.md) — Open WebUI,
  LobeChat and Chatbox over the local gateway, with a validated compose file.

### Changed

- `internal/spin` declares **seven** tools (was six) and still imports nothing
  outside the standard library.

### Fixed

- `chimera sessions export <id> -o file` silently ignored `-o` (the flag
  package stops at the first positional) — flags now parse anywhere.
- Qwen's `auth status` no longer reports an expired token as healthy when the
  probe endpoint tolerates it (the `exp` claim is checked first).

## [v0.1.1](https://github.com/NaMan6122/ChimerA/releases/tag/v0.1.1) — 2026-11-05

### Fixed

- The generated Homebrew formula dropped its redundant explicit version
  stanza (`brew audit` flags it; the version scans from the release URL).

## [v0.1.0](https://github.com/NaMan6122/ChimerA/releases/tag/v0.1.0) — 2026-11-05

First tagged release.

### Added

- **Gateway** (`chimera`): OpenAI-compatible `/v1` (chat completions with
  emulated SSE and tool-call deltas, models, usage), token + multi-tenant
  auth, rate limiting, monthly quotas, SQLite metering, Prometheus metrics
  with a selector-fallback radar, health endpoints.
- **Providers**: ChatGPT, Claude, Qwen, DeepSeek, Kimi on two substrates —
  DOM (real Chromium, stealth + selector fallback) and browserless replay
  (ChatGPT Sentinel PoW, DeepSeek Keccak proof-of-work, Qwen request signing;
  Qwen completions captcha-blocked by vendor, documented).
- **`chimera-spin`**: the no-history harness — tool schemas declared on turn 1
  only, delta-only turns, observe-only JSONL transcripts.
- **`chimera-chat`**: TUI over the same loop (backend picker, scrollback,
  markdown, tool trace).
- **`chimera auth`**: login / status / check / import — one browser login per
  provider, sessions stored mode 0600.
- **Distribution**: curl installer with mandatory sha256 verification, tag-
  driven release pipeline (5 platform archives + checksums + Homebrew
  formula), Homebrew tap.
- Docker compose for the browser path (VNC login) and a tenant compose for
  managed deployments.
