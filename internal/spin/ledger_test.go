package spin

// Ledger tests: entries persist in order with the right upstream generation,
// the size estimate tracks what was sent, Rotate switches generations without
// touching the harness session, and the compaction summary is usable.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerRecordsAndPersists(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, "sess-1")
	defer l.Close()

	l.Observe(Event{Kind: EventTask, Text: "fix the bug"})
	l.Observe(Event{Kind: EventRound, Round: 1, PromptChars: 500, Latency: 2e9, Text: "calling read_file"})
	l.Observe(Event{Kind: EventTool, Round: 1, Tool: toolCall("read_file", `{"path":"a.go"}`)})
	l.Observe(Event{Kind: EventResult, Round: 1, Result: "package main…"})
	l.Observe(Event{Kind: EventFinal, Round: 2, Text: "done"})

	// Round 1 estimate: tool schemas (seeded once) + prompt + reply + result.
	// The final's text was already counted as its round's reply.
	if got := l.EstUpstream(); got != toolBlockChars+500+len("calling read_file")+len("package main…") {
		t.Errorf("EstUpstream = %d, want %d", got, toolBlockChars+500+len("calling read_file")+len("package main…"))
	}

	if l.Path() == "" {
		t.Fatal("ledger file path empty after events")
	}
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e ledgerEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad entry %q: %v", line, err)
		}
		if e.Upstream != "sess-1" {
			t.Errorf("entry upstream = %q, want sess-1", e.Upstream)
		}
		kinds = append(kinds, e.Kind)
	}
	want := []string{"task", "round", "tool", "result", "final"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("entry kinds = %v, want %v", kinds, want)
	}
}

func TestLedgerRotate(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, "sess-1")
	defer l.Close()

	l.Observe(Event{Kind: EventTask, Text: "task"})
	l.Observe(Event{Kind: EventResult, Round: 1, Result: strings.Repeat("x", 500)})
	before := l.EstUpstream()
	if before == 0 {
		t.Fatal("estimate empty before rotate")
	}

	newUp := l.Rotate()
	if newUp != "sess-1-c1" {
		t.Errorf("Rotate = %q, want sess-1-c1", newUp)
	}
	if l.Upstream() != newUp {
		t.Errorf("Upstream = %q", l.Upstream())
	}
	if got := l.EstUpstream(); got != 0 {
		t.Errorf("estimate after rotate = %d, want 0 (fresh generation)", got)
	}
	// The next round seeds the NEW generation's estimate, not the old one.
	l.Observe(Event{Kind: EventRound, Round: 2, PromptChars: 100, Text: "continuing"})
	if got := l.EstUpstream(); got != toolBlockChars+100+len("continuing") {
		t.Errorf("estimate after rotate+round = %d", got)
	}
}

func TestLedgerCompactSummary(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, "sess-1")
	defer l.Close()

	l.Observe(Event{Kind: EventTask, Text: "fix the flaky test"})
	l.Observe(Event{Kind: EventRound, Round: 1, Text: "reading"})
	l.Observe(Event{Kind: EventTool, Round: 1, Tool: toolCall("grep", "")})
	l.Observe(Event{Kind: EventResult, Round: 1, Result: "found it in runner.go:42"})
	l.Observe(Event{Kind: EventTool, Round: 2, Tool: toolCall("edit_file", "")})
	l.Observe(Event{Kind: EventResult, Round: 2, Result: "edited timeout.go"})

	summary := l.CompactSummary("fix the flaky test")
	for _, want := range []string{"fix the flaky test", "grep", "edit_file", "runner.go:42", "edited timeout.go", "harness-side"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestNilLedgerIsSafe(t *testing.T) {
	var l *Ledger
	l.Observe(Event{Kind: EventTask, Text: "x"})
	if l.EstUpstream() != 0 {
		t.Error("nil ledger estimate nonzero")
	}
	l.Close()
}

func TestLedgerSurvivesUnwritableDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := NewLedger(filepath.Join(blocker, "sub"), "s")
	l.Observe(Event{Kind: EventTask, Text: "task"}) // warn once, keep memory
	if len(l.entries) == 0 {
		t.Error("in-memory entries lost when the file fails")
	}
	l.Close()
}

// toolCall builds a ToolCall for tests.
func toolCall(name, args string) ToolCall {
	var tc ToolCall
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}
