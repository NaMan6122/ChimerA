package spin

// Ledger — the harness-side working copy of a session. By design the provider
// holds the conversation (prebuilt chat services manage their own context),
// which makes upstream state invisible: nothing can say how full the vendor's
// window is, and nothing survives to reason about after the run. The ledger
// fixes the visibility half without breaking the only-new-messages invariant:
//
//   - It observes the same events the transcript does — task, rounds, tool
//     calls, results, replies — and records them keyed by session id, with a
//     per-entry marker of which provider-side generation was in effect.
//   - It is persistence, not prompt: nothing in the ledger is ever re-sent
//     wholesale to the provider. The only thing derived from it is the
//     compaction summary, a distilled seed for a FRESH upstream conversation.
//   - From the entries it can estimate how many characters the current
//     provider-side conversation holds (messages sent + replies received +
//     the tool schemas injected on the generation's first turn), which is the
//     signal compaction needs.
//
// Compaction itself (rotate to a fresh upstream session) lives in the Runner;
// the provider-side session id and the harness-side session id are different
// things: the harness session (and its transcript and ledger file) stays
// stable while the upstream id gains a -cN suffix per generation.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// toolBlockChars estimates the tool-schemas block the gateway injects into a
// generation's first turn (BuildToolPrompt over ToolDefs). Pinned roughly by
// the calling_test.go prompt-size assertions; ±20% is fine — the estimate
// feeds a heuristic threshold, not billing.
const toolBlockChars = 3000

// DefaultCompactChars is the estimated upstream size at which the Runner
// compacts. Vendor web UIs accept far more (a live probe typed 160k chars
// into chat.qwen.ai), but latency grows with the window and silent truncation
// is undetectable — compaction trades a fresh seed for predictability.
const DefaultCompactChars = 80_000

// ledgerEntry is one observed item of the working copy.
type ledgerEntry struct {
	TS          string  `json:"ts"`
	Kind        string  `json:"kind"` // task | round | tool | result | final | note | compact
	Upstream    string  `json:"upstream"`
	Round       int     `json:"round,omitempty"`
	Name        string  `json:"name,omitempty"`
	Arguments   string  `json:"arguments,omitempty"`
	Text        string  `json:"text,omitempty"`
	PromptChars int     `json:"prompt_chars,omitempty"`
	LatencyS    float64 `json:"latency_s,omitempty"`
}

// Ledger accumulates the working copy for one harness session.
type Ledger struct {
	mu       sync.Mutex
	session  string // harness-side id: stable across compactions
	dir      string
	path     string
	f        *os.File
	entries  []ledgerEntry
	upstream string // current provider-side generation id
	gen      int
	chars    int  // estimated upstream chars for the current generation
	seeded   bool // whether this generation's tool schemas are counted
	warned   bool
}

// NewLedger prepares a ledger for session under dir (the sessions directory;
// the file itself lands in dir/ledgers/). Opening the file is lazy: a run
// that fails before its first event creates nothing.
func NewLedger(dir, session string) *Ledger {
	return &Ledger{session: session, dir: dir, upstream: session}
}

// Path is the ledger file being written, or "" until the first event.
func (l *Ledger) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Observe records one loop event. Wired from Runner.emit; safe for nil
// receivers so callers need no nil checks.
func (l *Ledger) Observe(ev Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch ev.Kind {
	case EventTask:
		l.writeLocked(ledgerEntry{Kind: "task", Upstream: l.upstream, Text: ev.Text})
	case EventRound:
		// The generation's first turn carries the system prompt and the
		// gateway-injected tool schemas on top of its message content.
		if !l.seeded {
			l.chars += toolBlockChars
			l.seeded = true
		}
		l.chars += ev.PromptChars + len(ev.Text)
		l.writeLocked(ledgerEntry{Kind: "round", Upstream: l.upstream, Round: ev.Round,
			Text: ev.Text, PromptChars: ev.PromptChars, LatencyS: ev.Latency.Seconds()})
	case EventTool:
		// Arguments are part of the model's reply (already counted); recorded
		// so the working copy shows what was asked of the workspace.
		l.writeLocked(ledgerEntry{Kind: "tool", Upstream: l.upstream, Round: ev.Round,
			Name: ev.Tool.Function.Name, Arguments: ev.Tool.Function.Arguments})
	case EventResult:
		l.chars += len(ev.Result)
		l.writeLocked(ledgerEntry{Kind: "result", Upstream: l.upstream, Round: ev.Round,
			Name: ev.Tool.Function.Name, Text: ev.Result})
	case EventFinal:
		// Same text the round record carries; kept for the working copy's shape.
		l.writeLocked(ledgerEntry{Kind: "final", Upstream: l.upstream, Round: ev.Round, Text: ev.Text})
	case EventNudge:
		text := ev.Text
		if text == "" {
			text = "answered without evidence; asked again"
		}
		l.writeLocked(ledgerEntry{Kind: "note", Upstream: l.upstream, Round: ev.Round, Text: text})
	}
	// EventCompact is written by Rotate, which owns the generation switch.
}

// EstUpstream estimates how many characters the CURRENT provider-side
// conversation holds: everything sent to it plus everything it replied, plus
// the schema block on its first turn.
func (l *Ledger) EstUpstream() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.chars
}

// Rotate switches to a fresh provider-side generation: the harness session
// (and its files) stay stable, the upstream id gains a -cN suffix. Returns
// the new upstream id for Client.NewSession.
func (l *Ledger) Rotate() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := l.chars
	l.gen++
	l.upstream = fmt.Sprintf("%s-c%d", l.session, l.gen)
	l.chars, l.seeded = 0, false
	l.writeLocked(ledgerEntry{Kind: "compact", Upstream: l.upstream,
		Text: fmt.Sprintf("rotated from %s-c%d after ~%d estimated chars; working copy retained harness-side", l.session, l.gen-1, prev)})
	return l.upstream
}

// Upstream is the provider-side generation id currently in effect.
func (l *Ledger) Upstream() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.upstream
}

// CompactSummary distills the working copy into the seed message for a fresh
// upstream conversation: the task, the shape of the work so far, and the most
// recent tool results (the only context a continuation reliably needs).
// Deterministic on purpose: no extra vendor round, no variance.
func (l *Ledger) CompactSummary(task string) string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var b strings.Builder
	b.WriteString("Context was compacted: the earlier provider-side conversation approached its working window, so this is a fresh one. The full working copy lives harness-side and is NOT re-sent; continue the original task from the summary below.\n\n")
	b.WriteString("TASK: " + strings.TrimSpace(task) + "\n\n")

	calls := 0
	type roundLine struct{ tools []string }
	rounds := map[int]*roundLine{}
	var order []int
	var lastResults []string
	for _, e := range l.entries {
		switch e.Kind {
		case "tool":
			calls++
			r, ok := rounds[e.Round]
			if !ok {
				r = &roundLine{}
				rounds[e.Round] = r
				order = append(order, e.Round)
			}
			r.tools = append(r.tools, e.Name)
		case "result":
			lastResults = append(lastResults, fmt.Sprintf("[%s] %s", e.Name, clipTo(e.Text, 1500)))
			if len(lastResults) > 3 {
				lastResults = lastResults[1:]
			}
		}
	}
	fmt.Fprintf(&b, "Progress: %d tool rounds, %d tool calls.\n", len(order), calls)
	for _, rn := range order {
		fmt.Fprintf(&b, "- round %d: %s\n", rn, strings.Join(rounds[rn].tools, ", "))
	}
	if len(lastResults) > 0 {
		b.WriteString("\nMost recent tool results (newest last):\n")
		for _, r := range lastResults {
			b.WriteString(r + "\n")
		}
	}
	b.WriteString("\nContinue with the tools — they are available again in this conversation — or give the final answer if the task is complete.")
	return b.String()
}

// Close flushes and closes the ledger file.
func (l *Ledger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// writeLocked appends one entry to memory and to the ledger file. Caller
// holds l.mu.
func (l *Ledger) writeLocked(e ledgerEntry) {
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	l.entries = append(l.entries, e)

	if l.f == nil {
		dir := filepath.Join(l.dir, "ledgers")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			l.warnLocked(err)
			return
		}
		l.path = filepath.Join(dir, sanitize(l.session)+".ledger.jsonl")
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			l.warnLocked(err)
			return
		}
		l.f = f
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		l.warnLocked(err)
	}
}

func (l *Ledger) warnLocked(err error) {
	if l.warned {
		return
	}
	l.warned = true
	fmt.Fprintf(os.Stderr, "chimera: context ledger disabled (%v)\n", err)
}

func clipTo(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + " …"
}
