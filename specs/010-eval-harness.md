# 010 — Eval harness (no-browser provider tests)

**Problem:** Provider logic (selector chains, extraction, stability waits) is only
exercised against live vendor pages. CI covers parsers, never the scraping layer —
so UI redesigns are discovered by users, not tests.

**Goal:** recorded DOM fixtures + a lightweight matcher that assert *selector chains
resolve and extraction picks the right node* without Chromium.

## Design

New package `internal/eval` (new dep `golang.org/x/net` for `html` parsing — pure Go):

- Fixtures: `internal/eval/testdata/<provider>/*.html` — minimal saved snippets
  (chat input, send/stop buttons, 2–3 assistant turns, copy buttons, login markers).
  Hand-written to mirror current selectors, clearly labeled synthetic.
- Matcher: parse fixture → for each selector list in a pack (spec 006 schema; falls
  back to the Go `selectors.go` vars until 006 lands), report first-hit index and
  matched node count. Extraction check: last `assistant_message` node's text equals
  expected string in a sidecar `<name>.expect.json`.
- Runner: `go test ./internal/eval/` asserts every provider pack resolves at index 0
  against its fixtures (any fallback hit = test failure with the winning selector
  named — the same signal as the runtime fallback counter).
- Nightly/live job (docs only, not CI): `EVAL_LIVE=1 go test` dumps current vendor DOM
  to testdata for human review — never asserts live (flaky by nature).
- Fixture hygiene: no real user content, no session tokens; README note in testdata.

## Files

- `internal/eval/eval.go` (parse + match + extract-last-text)
- `internal/eval/eval_test.go` (per-provider resolution + extraction cases)
- `internal/eval/testdata/**` (5 providers × html + expect.json)
- `docs/PROVIDERS.md` (fixture contribution guide: save snippet, add expect, run)

## Acceptance

- `go test ./internal/eval/` green with zero browser processes.
- Deliberately breaking one fixture selector fails the test naming the fallback winner.
- Swapping in a spec-006 YAML pack (when it exists) works without harness changes
  (harness reads the pack interface, not Go vars).
- `go vet ./... && go test ./...`.
