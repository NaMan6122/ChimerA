# Security policy

ChimerA handles **credential-grade material**: exported vendor sessions
(`auth_data/*.json`) are password-equivalent — they grant full access to the
chat accounts they belong to. Security issues are treated accordingly.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's **private vulnerability reporting** (Security → Report a
vulnerability on this repository), or contact the maintainer directly. You
will get an acknowledgment within 72 hours and a fix timeline agreed case by
case. Credit is given unless you prefer anonymity.

## In scope

- Session material leaking outside `AUTH_DIR` (logs, transcripts, metrics,
  error bodies, dumps).
- The API layer's auth bypasses: token validation, multi-tenant scoping,
  quota enforcement, `/v1/usage` isolation.
- SSRF/credential exposure via the configurable provider URLs.
- Anything that sends prompts or session material to a third party — Chimera
  promises prompts never leave the machine it runs on.
- Supply-chain issues in the Go dependencies.

## Out of scope

- Vendor-side account bans/rate limits from using the tool (that is the
  documented automation risk, not a ChimerA vulnerability).
- The reverse-engineered transports themselves being against vendor ToS —
  that is disclosed throughout the docs and the Disclaimer in the README.

## Handling your own credentials

- `auth_data/` is mode `0700` (files `0600`) and gitignored; keep it that way.
- Never commit `.env`, `auth_data/`, `logs/` or `browser_data/` — push
  protection is enabled as a backstop, not a substitute for care.
- `chimera auth import` validates material before installing it; do not
  bypass it with hand-written session files.

## The harness tool surface (chimera-spin / chimera-chat)

The agent loop executes tools with your local privileges. That is the
feature — an agent that cannot run `go test` is a toy — but the trust
boundary deserves stating plainly:

- `run_command` and `monitor start` execute **arbitrary `bash -lc`** with no
  allowlist. They are confined to the workspace only by convention (`cmd.Dir`);
  shell commands can reach anywhere your user can. Treat a spin's `-root` as
  advisory scoping, not isolation.
- `-safe` applies an **advisory denylist** (privilege escalation, power
  control, device overwrite, root-recursive delete, pipe-to-shell, fork
  bombs) to both. It is hardening, not a sandbox — do not run an untrusted
  task with `-safe` and assume containment.
- File tools (`read_file`, `write_file`, `edit_file`, `read_pdf`, `grep`,
  `list_dir`, `git`) resolve **symlinks** before confinement checks, so a
  link planted inside the root cannot point a tool outside it.
- `monitor` logs land in a per-run temp directory (`$TMPDIR/chimera-monitors-*`),
  never inside the workspace; they live until the OS cleans them.
- Session transcripts (`sessions/`) and context ledgers (`ledgers/`) stay on
  the local disk and are never sent upstream — prompts reach only the vendor
  you logged into, which is the core promise.
- MCP-spawned spins inherit all of this; if you expose the MCP server to a
  client you do not fully trust, scope its workspace with `-root`/`AUTH_DIR`
  accordingly (see spec 021).
