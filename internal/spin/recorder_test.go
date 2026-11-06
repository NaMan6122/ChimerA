package spin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tc(name, args string) ToolCall {
	var c ToolCall
	c.ID, c.Type = "call_1", "function"
	c.Function.Name, c.Function.Arguments = name, args
	return c
}

// TestRecorderWritesAndLoads covers the contract: every loop event becomes one
// line, and a transcript can be read back for handoff/display.
func TestRecorderWritesAndLoads(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start("sess-1", "chimera-deepseek", "http://localhost:8010/v1", "/repo", nil)
	rec.Observe(Event{Kind: EventTask, Text: "read runner.go"})
	rec.Observe(Event{Kind: EventRound, Round: 1, PromptChars: 1200, Latency: 2e9})
	rec.Observe(Event{Kind: EventTool, Round: 1, Tool: tc("read_file", `{"path":"a.go"}`)})
	rec.Observe(Event{Kind: EventResult, Round: 1, Tool: tc("read_file", `{"path":"a.go"}`), Result: "package a"})
	rec.Observe(Event{Kind: EventFinal, Round: 2, Text: "it does X"})
	rec.Close()

	recs, err := LoadTranscript(dir, "sess-1")
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	want := []string{"session", "user", "round", "tool", "result", "final"}
	if len(recs) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(recs), len(want), recs)
	}
	for i, k := range want {
		if recs[i].Kind != k {
			t.Errorf("record %d kind = %q, want %q", i, recs[i].Kind, k)
		}
	}
	if recs[0].Model != "chimera-deepseek" || recs[0].Root != "/repo" {
		t.Errorf("session header lost metadata: %+v", recs[0])
	}
	if recs[5].Text != "it does X" {
		t.Errorf("final text = %q", recs[5].Text)
	}
}

// TestRecorderTagsInHeader: tags given to Start land in the session header
// record, which is what `chimera sessions list --tag` filters on.
func TestRecorderTagsInHeader(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start("tagged", "m", "u", "r", []string{"research", "telemetry"})
	rec.Close()

	recs, err := LoadTranscript(dir, "tagged")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[0].Kind != "session" {
		t.Fatalf("transcript missing session header: %+v", recs)
	}
	if len(recs[0].Tags) != 2 || recs[0].Tags[0] != "research" || recs[0].Tags[1] != "telemetry" {
		t.Errorf("header tags = %v, want [research telemetry]", recs[0].Tags)
	}
}

// TestRecorderConfinesSessionNames: a session id is user input and must not be
// able to write outside the sessions directory.
func TestRecorderConfinesSessionNames(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start("../../etc/passwd", "m", "u", "r", nil)
	rec.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one file inside %s, got %d", dir, len(entries))
	}
	if strings.Contains(entries[0].Name(), "..") || strings.Contains(entries[0].Name(), "/") {
		t.Fatalf("session name escaped: %q", entries[0].Name())
	}
}

// TestRecorderRotatesOnSessionChange keeps transcripts from interleaving when a
// chat switches backend or starts a new conversation.
func TestRecorderRotatesOnSessionChange(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start("sess-a", "m", "u", "r", nil)
	rec.Note("user", "first")
	rec.Start("sess-b", "m", "u", "r", nil)
	rec.Note("user", "second")
	rec.Close()

	a, _ := LoadTranscript(dir, "sess-a")
	b, _ := LoadTranscript(dir, "sess-b")
	if len(a) != 2 || a[1].Text != "first" {
		t.Fatalf("session a = %+v", a)
	}
	if len(b) != 2 || b[1].Text != "second" {
		t.Fatalf("session b = %+v", b)
	}
	if rec.Path() == "" || !strings.HasSuffix(rec.Path(), filepath.Base(rec.Path())) {
		t.Fatalf("path = %q", rec.Path())
	}
}
