package grokweb

import (
	"strings"
	"testing"
)

// TestFoldPrompt: turn 1 goes out raw; later turns fold the accumulated
// transcript (Grok's model backend is stateless — measured).
func TestFoldPrompt(t *testing.T) {
	if got := foldPrompt(nil, "first question"); got != "first question" {
		t.Fatalf("turn 1 must go out raw, got %q", got)
	}
	history := []turn{
		{role: "user", text: "Remember the word KUMQUAT. Reply OK"},
		{role: "assistant", text: "OK"},
	}
	got := foldPrompt(history, "What word did I ask you to remember?")
	for _, want := range []string{"<conversation>", "KUMQUAT", "Assistant: OK", "What word did I ask you to remember?"} {
		if !strings.Contains(got, want) {
			t.Errorf("folded prompt missing %q:\n%s", want, got)
		}
	}
}

// TestFoldPromptBudget: when the transcript exceeds the budget, the OLDEST
// turns are dropped and the newest survive.
func TestFoldPromptBudget(t *testing.T) {
	var history []turn
	history = append(history, turn{role: "user", text: "OLDEST-UNIQUE-MARKER"}, turn{role: "assistant", text: "ok"})
	for i := 0; i < 400; i++ {
		history = append(history,
			turn{role: "user", text: strings.Repeat("u", 100)},
			turn{role: "assistant", text: strings.Repeat("a", 100)})
	}
	history = append(history,
		turn{role: "user", text: "newest question"},
		turn{role: "assistant", text: "newest answer"})
	got := foldPrompt(history, "again")
	if len(got) > maxFoldChars+200 {
		t.Errorf("folded prompt = %d chars, budget %d", len(got), maxFoldChars)
	}
	if !strings.Contains(got, "newest question") {
		t.Error("newest turn must survive the budget cut")
	}
	if strings.Contains(got, "OLDEST-UNIQUE-MARKER") {
		t.Error("oldest turns must be dropped first")
	}
}
