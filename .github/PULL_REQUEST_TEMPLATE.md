<!--
  Redact everything credential-grade before submitting:
  auth_data/*.json contents, cookies, tokens, prompts, log dumps.
-->
## What does this PR change?

## Why? (measured, not asserted)
<!-- numbers, captures, or a reproduction — this repo's culture -->

## Checklist
- [ ] `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test -race ./...` green
- [ ] No internal SDLC material (specs/ADRs) added to this public repo
- [ ] No reverse-engineering material or gate knowledge in code comments
- [ ] No captcha automation, no prompts/session material leaving the machine
- [ ] New provider transports registered via `webapi.Register` in `init()`
- [ ] Docs updated (README matrix / public docs) where behavior is user-visible
