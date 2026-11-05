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
