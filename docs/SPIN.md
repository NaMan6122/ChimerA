# chimera-spin — the no-history harness

`cmd/chimera-spin` is a small agent loop for driving a Chimera gateway. It exists
because of one design rule:

> **The harness must not accumulate context. The provider already keeps it.**

Every consumer transport Chimera speaks to — ChatGPT, DeepSeek, Qwen — maintains
the conversation upstream (`conversation_id` + parent message, or a session id).
A general-purpose harness ignores that and re-sends its whole transcript every
turn: measured at **47,861 chars on turn 1** and **51,936 on turn 2**, growing by
the size of the history each round until it hits a vendor limit.

`chimera-spin` sends:

| Turn | What is sent | Measured |
|---|---|---|
| 1 | system prompt + task + tool schemas | ~1,100 chars |
| 2..n | **only** the tool results from the previous round | 66 – 7,224 chars |
| final | nothing new; the answer comes back | — |

The gateway does the rest: `X-Session-Id` pins one upstream conversation, and
`buildDeltaPrompt`/`buildPrunedPrompt` stop it repeating history the provider
already holds. Because the harness never needs the transcript back, it also
never parses or stores it — `s.rounds` holds latency and character counts only.

## Install

```bash
./build.sh                # build + install both programs
./build.sh spin           # the harness alone — no backend needed to build it
./build.sh gateway        # the gateway alone
./build.sh spin --no-install
./build.sh --prefix=/opt spin
```

The harness and the gateway are independent programs. `cmd/chimera-spin`
imports **nothing** from `internal/` — it is stdlib-only and speaks plain HTTP,
so it can be built, shipped and run on its own against any OpenAI-compatible
endpoint. They share a repository, not a runtime.

Same layout as `anv` — a versioned directory plus a stable symlink on PATH:

```
~/.local/share/chimera/versions/<version>/chimera
~/.local/share/chimera/versions/<version>/chimera-spin
~/.local/bin/chimera       -> .../versions/<version>/chimera
~/.local/bin/chimera-spin  -> .../versions/<version>/chimera-spin
```

The version is `git describe --tags --always --dirty`, stamped into the binary;
`chimera-spin -version` prints it.

## Usage

```bash
# a gateway must be running, e.g.
TRANSPORT=webapi PROVIDER=chatgpt chimera

chimera-spin "Where is MAX_PROMPT_CHARS enforced and which transports are exempt?"
chimera-spin -model chimera-deepseek -max-rounds 6 -json report.json "..."
```

From outside the repository, point `-root` at it:

```bash
cd /tmp && chimera-spin -root ~/Documents/Projects/ChimerA "..."
```

Flags: `-base-url` (default `http://localhost:8000/v1`), `-token` (or
`CHIMERA_API_KEY`), `-model`, `-session`, `-root` (workspace confinement),
`-max-rounds`, `-timeout`, `-json`.

A run prints the answer on stdout and a report on stderr:

```
[spin] rounds=3 tools=1 prompt_chars=9219 wall=19.4s session=spin-17938319
[spin]   round 1: 1032 chars, 6.7s, tools=[grep]
[spin]   round 2: 963 chars, 6.3s, tools=[grep read_file read_file]
[spin]   round 3: 7224 chars, 6.3s, tools=[]
```

## Interactive TUI

`chimera-chat` is the same loop with a transcript, a tool trace and a status
line:

```bash
chimera-chat                                  # current directory as the workspace
chimera-chat -model chimera-deepseek
```

```
 Chimera · chimera-chatgpt · …chat-1793832
──────────────────────────────────────────
› what does applyStealth do to navigator.webdriver?
  · round 1 · 1.2k chars · 4.1s
  ⚙ grep applyStealth
    internal/browser/stealth.go:12:func applyStealth(page *rod.Page) {  (96 chars)
  ⚙ read_file internal/browser/stealth.go
    1: package browser  (3.5k chars)
  · round 4 · 3.5k chars · 4.7s
It redefines navigator.webdriver to return undefined …
──────────────────────────────────────────
› ▊
enter send · /new new session · esc quit
```

The transcript you see is **local only**. Nothing in it is sent back as a
prefix: the first message of a session carries the system prompt and tool
schemas, and every later message carries only what you typed (`Client.Turns`
decides this). `/new` resets the upstream conversation and the next message
starts it again.

### Slash commands

| Command | Effect |
|---|---|
| `/backends` | probe `localhost:8000-8011` and list every gateway with its provider and model |
| `/use <provider>` | switch backend, e.g. `/use deepseek` — also `/use 8010` or `/use http://host:8011/v1` |
| `/new` | new conversation on the current backend |
| `/status` | backend, model, session, turn count, workspace |
| `/help` | the list above |
| `/quit` | exit (same as `esc`) |

`/use` starts a fresh session on the new gateway, because the previous
conversation lives in the old process. Discovery is a real probe, so a dead port
is reported as dead rather than assumed absent:

```
→ chatgpt    http://localhost:8000/v1    chimera-chatgpt
  —          http://localhost:8010/v1    no gateway
```

### Scrolling

The transcript is a scrollback, not a tail: new output does not yank you back
to the bottom while you are reading history.

| Key | Effect |
|---|---|
| `pgup` / `pgdn` | page through the transcript (also the mouse wheel) |
| `ctrl+u` / `ctrl+d` | half page |
| `home` / `end` | jump to start / resume following |
| `enter` | send (also jumps to the bottom) · `ctrl+j` newline |
| `esc` | quit |

While scrolled back the status line says how far: `↑ 42 lines below · end to follow`.
Following resumes automatically when you scroll to the bottom.

`chimera-chat` links a TUI toolkit (bubbletea); `chimera-spin` stays
stdlib-only, and both share `internal/spin` for the protocol and tools.

## One harness, any backend

The harness names no provider. Point it at whichever gateway is running:

```bash
# ChatGPT web session on :8000
chimera-spin -base-url http://localhost:8000/v1 -model chimera-chatgpt "..."

# DeepSeek web session on :8010 — same binary, no rebuild
chimera-spin -base-url http://localhost:8010/v1 -model chimera-deepseek "..."
```

Measured on the same task (read one file, answer from it):

| Backend | Round 1 | Round 2 | Round 3 | Wall |
|---|---|---|---|---|
| `chimera-deepseek` | 1.6 s | 1.2 s | 1.7 s | **4.5 s** |
| `chimera-chatgpt` | 6.2 s | 5.8 s | 4.9 s | 21.3 s |

The harness cost is identical; the difference is the vendor's web API.

## Tools

Six, not twenty — past a handful of schemas the prompt-injected calling
convention degrades, and every schema is prompt bytes on the first turn.

`read_file` · `list_dir` · `grep` · `write_file` · `edit_file` · `run_command`

Paths are confined to `-root`; a model that can escape the workspace is a
liability. `edit_file` requires the search text to occur exactly once. Tool
failures are returned to the model as text rather than aborting the spin — a
broken call is information it can use.

Output budgets: 8,000 chars per call, 16,000 per round. Measured: eight parallel
12k file reads produced a 65k-char follow-up prompt, which is the same
accumulation arriving through the tool channel.

## Protocol notes

Tool schemas are declared **on the first turn only**. Later turns carry no
`tools` field, so the gateway injects no schemas and the harness parses the
model's JSON block itself (`parseToolCalls`). This requires the gateway to accept
a request whose only messages are tool results — allowed when `X-Session-Id`
identifies the conversation.

The system prompt says the model "cannot read files directly" and must request
through the bridge. That wording is load-bearing: told it merely "has access to
functions", a live ChatGPT session answers that it cannot read files at all.

## Known limits

- If the upstream conversation is lost (session expiry, gateway restart), the
  harness cannot resume — by design, it has no transcript to replay. Start a new
  spin; the session id is fresh per run.
- Tool calling remains prompt engineering: reliable for a few tools, not
  arbitrary schemas. Cross-provider failover mid-spin is not implemented.
- `run_command` executes in the workspace with the user's permissions. There is
  no sandbox beyond path confinement for file tools.
