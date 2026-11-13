package main

// The /sessions picker's pure helpers: listing is capped and newest-first,
// unreadable transcripts are skipped, and task lines clip cleanly.

import (
	"strings"
	"testing"

	"github.com/chimera/chimera/internal/spin"
)

func writeTranscript(t *testing.T, dir, id string, rounds int) {
	t.Helper()
	rec, err := spin.NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start(id, "chimera-test", "http://x", "/repo", nil)
	rec.Note("user", "task for "+id)
	for i := 1; i <= rounds; i++ {
		rec.Observe(spin.Event{Kind: spin.EventRound, Round: i, Text: "step"})
	}
	rec.Observe(spin.Event{Kind: spin.EventFinal, Round: rounds, Text: "done"})
	rec.Close()
}

func TestRecentSessionsNewestFirstAndCapped(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "old", 1)
	writeTranscript(t, dir, "newer", 2)
	writeTranscript(t, dir, "newest", 3)

	got := recentSessions(dir, 2)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (cap honoured)", len(got))
	}
	if got[0].ID != "newest" || got[1].ID != "newer" {
		t.Errorf("order = %v, want [newest newer]", []string{got[0].ID, got[1].ID})
	}
	if !strings.Contains(got[0].Task, "task for newest") {
		t.Errorf("task = %q", got[0].Task)
	}

	all := recentSessions(dir, 15)
	if len(all) != 3 {
		t.Errorf("uncapped listing = %d, want 3", len(all))
	}
}

func TestRecentSessionsEmptyAndUnreadable(t *testing.T) {
	if got := recentSessions(t.TempDir(), 5); got != nil {
		t.Errorf("empty dir listing = %v, want nil", got)
	}
	// A directory that does not exist returns nil without panicking.
	if got := recentSessions("/definitely/not/here", 5); got != nil {
		t.Errorf("missing dir listing = %v, want nil", got)
	}
}

func TestClipTask(t *testing.T) {
	if got := clipTask("first line\nsecond line", 40); got != "first line" {
		t.Errorf("clipTask multiline = %q", got)
	}
	long := strings.Repeat("x", 50)
	got := clipTask(long, 10)
	if got := []rune(got); len(got) != 11 || got[10] != '…' {
		t.Errorf("clipTask long = %q", string(got))
	}
}
