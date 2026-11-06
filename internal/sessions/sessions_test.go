package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

func writeTranscript(t *testing.T, dir, id string, recs []spin.Record) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, r := range recs {
		if r.TS == "" {
			r.TS = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSummaryFromTranscript(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f := func(d time.Duration) string { return ts.Add(d * time.Second).Format(time.RFC3339Nano) }
	writeTranscript(t, dir, "runbook-1", []spin.Record{
		{TS: f(0), Kind: "session", Session: "runbook-1", Model: "chimera-grok", BaseURL: "http://localhost:8014/v1", Root: "/tmp/ws"},
		// f(d) means d SECONDS after start (fixed here after a ns/s mix-up).
		{TS: f(1), Kind: "user", Text: "Count the exported functions in runner.go"},
		{TS: f(2), Kind: "round", Round: 1, PromptChars: 1200, LatencyS: 2.7, Text: "I'll read the file first."},
		{TS: f(3), Kind: "tool", Round: 1, Name: "read_file", Arguments: `{"path":"internal/spin/runner.go"}`},
		{TS: f(4), Kind: "result", Round: 1, Name: "read_file", Result: strings.Repeat("x", 3000)},
		{TS: f(5), Kind: "tool", Round: 1, Name: "grep", Arguments: `{"pattern":"^func"}`},
		{TS: f(6), Kind: "result", Round: 1, Name: "grep", Result: "9 matches"},
		{TS: f(7), Kind: "round", Round: 2, PromptChars: 3100, LatencyS: 3.1, Text: "There are nine exported functions:"},
		{TS: f(8), Kind: "final", Round: 2, Text: "9 exported functions: Runner, Step, …"},
	})

	s, err := LoadSummary(filepath.Join(dir, "runbook-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "runbook-1" || s.Model != "chimera-grok" || s.BaseURL != "http://localhost:8014/v1" {
		t.Errorf("summary header = %+v", s)
	}
	if s.Task != "Count the exported functions in runner.go" {
		t.Errorf("task = %q", s.Task)
	}
	if s.ToolCalls != 2 || s.Rounds != 2 || s.Errors != 0 {
		t.Errorf("rounds/tools/errors = %d/%d/%d", s.Rounds, s.ToolCalls, s.Errors)
	}
	if s.Final == "" || !strings.Contains(s.Final, "9 exported") {
		t.Errorf("final = %q", s.Final)
	}
	if s.Duration < 8*time.Second {
		t.Errorf("duration = %v", s.Duration)
	}
}

func TestRenderMarkdownRunbook(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f := func(d time.Duration) string { return ts.Add(d * time.Second).Format(time.RFC3339Nano) }
	writeTranscript(t, dir, "rb", []spin.Record{
		{TS: f(0), Kind: "session", Session: "rb", Model: "chimera-gemini", BaseURL: "http://localhost:8012/v1", Root: "/tmp/ws"},
		{TS: f(1), Kind: "user", Text: "Find who calls webapi.Register"},
		{TS: f(2), Kind: "round", Round: 1, PromptChars: 900, LatencyS: 1.4, Text: "Searching the tree."},
		{TS: f(3), Kind: "tool", Round: 1, Name: "grep", Arguments: `{"pattern":"webapi.Register"}`},
		{TS: f(4), Kind: "result", Round: 1, Name: "grep", Result: strings.Repeat("hit\n", 900)},
		{TS: f(5), Kind: "note", Text: "answered without reading anything; asked for evidence"},
		{TS: f(6), Kind: "final", Round: 1, Text: "internal/providers/webapi/grok/provider.go:20"},
	})

	md, err := RenderMarkdown(filepath.Join(dir, "rb.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Session runbook — rb",
		"Preserved for reading, not replay",
		"| Model | `chimera-gemini` |",
		"## Task", "Find who calls webapi.Register",
		"### Round 1", "**Tool → `grep`**", "```json", `"pattern":"webapi.Register"`,
		"**Result ← `grep`**", "(truncated; full output in the JSONL transcript)",
		"> answered without reading anything; asked for evidence",
		"## Final answer", "internal/providers/webapi/grok/provider.go:20",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("runbook missing %q", want)
		}
	}
}

func TestListNewestFirst(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "new"} {
		writeTranscript(t, dir, id, []spin.Record{
			{TS: ts.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano), Kind: "session", Session: id, Model: "m"},
			{TS: ts.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano), Kind: "user", Text: "task " + id},
		})
	}
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "new" || list[1].ID != "old" {
		t.Fatalf("list order = %s, %s (want newest first)", list[0].ID, list[1].ID)
	}
}

func TestRenderMarkdownTags(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	writeTranscript(t, dir, "tagged", []spin.Record{
		{TS: ts.Format(time.RFC3339Nano), Kind: "session", Session: "tagged",
			Tags: []string{"research", "telemetry"}},
		{TS: ts.Format(time.RFC3339Nano), Kind: "user", Text: "do the thing"},
	})

	md, err := RenderMarkdown(filepath.Join(dir, "tagged.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "| Tags | `research`, `telemetry` |") {
		t.Errorf("markdown metadata missing tags row:\n%s", md)
	}
}

func TestRenderMarkdownUntaggedHasNoTagsRow(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	writeTranscript(t, dir, "plain", []spin.Record{
		{TS: ts.Format(time.RFC3339Nano), Kind: "session", Session: "plain"},
		{TS: ts.Format(time.RFC3339Nano), Kind: "user", Text: "do the thing"},
	})

	md, err := RenderMarkdown(filepath.Join(dir, "plain.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(md, "| Tags") {
		t.Errorf("untagged session rendered a tags row:\n%s", md)
	}
}

// TestSummaryCarriesTags: the header tags survive JSON round-trip and the
// case-insensitive HasTag filter that backs `list --tag` / `search --tag`.
func TestSummaryCarriesTags(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	writeTranscript(t, dir, "tagged", []spin.Record{
		{TS: ts.Format(time.RFC3339Nano), Kind: "session", Session: "tagged",
			Tags: []string{"research"}},
		{TS: ts.Format(time.RFC3339Nano), Kind: "user", Text: "do the thing"},
	})

	s, err := LoadSummary(filepath.Join(dir, "tagged.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tags) != 1 || s.Tags[0] != "research" {
		t.Fatalf("summary tags = %v, want [research]", s.Tags)
	}
	if !HasTag(s.Tags, "RESEARCH") || HasTag(s.Tags, "other") || HasTag(s.Tags, "") {
		t.Errorf("HasTag = (%v, %v, %v), want (true, false, false)",
			HasTag(s.Tags, "RESEARCH"), HasTag(s.Tags, "other"), HasTag(s.Tags, ""))
	}
}

// TestListTagFilter: `list --tag` keeps only sessions carrying the tag.
func TestListTagFilter(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	mk := func(id string, tags []string) []spin.Record {
		return []spin.Record{
			{TS: ts.Format(time.RFC3339Nano), Kind: "session", Session: id, Tags: tags},
			{TS: ts.Format(time.RFC3339Nano), Kind: "user", Text: "task " + id},
		}
	}
	writeTranscript(t, dir, "researched", mk("researched", []string{"research"}))
	writeTranscript(t, dir, "plain", mk("plain", nil))

	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered list = %d sessions, want 2", len(all))
	}
	var filtered []*Summary
	for _, s := range all {
		if HasTag(s.Tags, "research") {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) != 1 || filtered[0].ID != "researched" {
		t.Errorf("tag filter = %+v, want only researched", filtered)
	}
}

func TestTornLastLineSurvives(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "torn.jsonl")
	body := `{"ts":"2026-11-06T12:00:00Z","kind":"session","session":"torn"}` + "\n" +
		`{"ts":"2026-11-06T12:00:0` // torn write
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSummary(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "torn" {
		t.Errorf("torn transcript lost records: %+v", s)
	}
}
