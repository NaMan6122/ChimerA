package sessions

// ExportAll + RehydrateContext tests, over fixture transcripts written with
// the real recorder.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimera/chimera/internal/spin"
)

func writeFixture(t *testing.T, dir, id, task, final string, rounds int) {
	t.Helper()
	rec, err := spin.NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start(id, "chimera-test", "http://localhost:8000/v1", "/repo", []string{"gateway", "test"})
	rec.Note("user", task)
	for i := 1; i <= rounds; i++ {
		rec.Observe(spin.Event{Kind: spin.EventRound, Round: i, Text: "round text " + strings.Repeat("x", 50), Latency: 2e9})
		rec.Observe(spin.Event{Kind: spin.EventResult, Round: i, Result: "result payload"})
	}
	rec.Observe(spin.Event{Kind: spin.EventFinal, Round: rounds, Text: final})
	rec.Close()
}

func TestExportAllVault(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "alpha", "first task", "answer one", 2)
	writeFixture(t, dir, "beta", "second | task", "answer two", 1)

	out := filepath.Join(t.TempDir(), "vault")
	written, err := ExportAll(dir, out, false)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(written) != 3 { // two runbooks + index
		t.Fatalf("written = %v, want alpha.md, beta.md, index.md", written)
	}
	for _, name := range []string{"alpha.md", "beta.md", "index.md"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	idx, err := os.ReadFile(filepath.Join(out, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha", "beta", "first task", "second \\| task"} {
		if !strings.Contains(string(idx), want) {
			t.Errorf("index missing %q:\n%s", want, idx)
		}
	}

	// Re-export is idempotent (overwrites, same file set).
	if _, err := ExportAll(dir, out, false); err != nil {
		t.Fatalf("re-export: %v", err)
	}
}

func TestExportAllHTMLAndEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "gamma", "task", "done", 1)
	out := filepath.Join(t.TempDir(), "vault")
	if _, err := ExportAll(dir, out, true); err != nil {
		t.Fatalf("ExportAll html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gamma.html")); err != nil {
		t.Errorf("missing gamma.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "index.md")); err != nil {
		t.Errorf("missing index.md: %v", err)
	}

	if _, err := ExportAll(t.TempDir(), filepath.Join(t.TempDir(), "v"), false); err == nil {
		t.Error("empty archive should error, not write an empty vault")
	}
}

func TestRehydrateContext(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "sess", "fix the flaky test", "it was a timeout; raised it", 3)

	block, err := RehydrateContext(filepath.Join(dir, "sess.jsonl"), 0)
	if err != nil {
		t.Fatalf("RehydrateContext: %v", err)
	}
	for _, want := range []string{
		"fix the flaky test",
		"CONTEXT, not a replay",
		"3 tool rounds",
		"it was a timeout; raised it",
		"round 1", "round 3",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}

	// A tight budget still produces the load-bearing parts.
	small, err := RehydrateContext(filepath.Join(dir, "sess.jsonl"), 400)
	if err != nil {
		t.Fatalf("RehydrateContext(400): %v", err)
	}
	if len(small) > 400+80 { // budget + truncation marker slack
		t.Errorf("block = %d chars, want within budget", len(small))
	}
	if !strings.Contains(small, "fix the flaky test") {
		t.Errorf("tight budget lost the task:\n%s", small)
	}
}

func TestRehydrateContextMissingFile(t *testing.T) {
	if _, err := RehydrateContext(filepath.Join(t.TempDir(), "nope.jsonl"), 0); err == nil {
		t.Error("missing transcript should error")
	}
}
