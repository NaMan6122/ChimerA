package main

import (
	"strings"
	"testing"
)

// TestPresetsRenderOutputConvention pins the teaching contract: every preset's
// scaffold is non-empty, says exactly where its output goes, names the
// structure it writes, and carries the tool-failure rule.
func TestPresetsRenderOutputConvention(t *testing.T) {
	writes := map[string]string{
		"summarize":     "notes/<file>.md",
		"flashcards":    "notes/<file>.flashcards.md",
		"eli12":         "notes/<file>.eli12.md",
		"cite-check":    "stdout",
		"meeting-notes": "notes/<file>.minutes.md",
	}
	structure := map[string]string{
		"summarize":     "## Key points",
		"flashcards":    "**Q.**",
		"eli12":         "## The big idea",
		"cite-check":    "<file>:<line>",
		"meeting-notes": "## Actions",
	}
	if len(presets) != len(writes) || len(presets) != len(structure) {
		t.Fatalf("presets table has %d entries; the test table expects %d and %d",
			len(presets), len(writes), len(structure))
	}
	for _, p := range presets {
		task := p.render("")
		if strings.TrimSpace(task) == "" {
			t.Errorf("%s: scaffold is empty", p.name)
		}
		if !strings.Contains(task, writes[p.name]) {
			t.Errorf("%s: scaffold missing its output convention %q", p.name, writes[p.name])
		}
		if !strings.Contains(task, structure[p.name]) {
			t.Errorf("%s: scaffold missing its structure marker %q", p.name, structure[p.name])
		}
		if !strings.Contains(task, "Tool failures") {
			t.Errorf("%s: scaffold missing the tool-failure rule", p.name)
		}
		if p.name == "cite-check" {
			if strings.Contains(task, "write_file") {
				t.Errorf("%s: must not instruct writing files", p.name)
			}
		} else if !strings.Contains(task, "write_file") {
			t.Errorf("%s: scaffold does not name write_file", p.name)
		}
	}
}

// TestPresetRefinementAppended: bare positional text with -preset refines the
// scaffold as a "Constraints: …" line and never replaces it.
func TestPresetRefinementAppended(t *testing.T) {
	p, ok := lookupPreset("summarize")
	if !ok {
		t.Fatal("summarize missing from the table")
	}
	if got := p.render(""); strings.Contains(got, "Constraints:") {
		t.Errorf("bare preset must not carry a Constraints line:\n%s", got)
	}
	got := p.render("focus on the RFC drafts")
	if !strings.Contains(got, "\n\nConstraints: focus on the RFC drafts") {
		t.Errorf("refinement not appended:\n%s", got)
	}
	if !strings.HasPrefix(got, p.scaffold) {
		t.Error("refined task must start from the unmodified scaffold")
	}
	// Whitespace-only refinement is no refinement at all.
	if got := p.render("   "); strings.Contains(got, "Constraints:") {
		t.Errorf("whitespace refinement must be dropped:\n%s", got)
	}
}

// TestPresetTaskMutualExclusion: -preset and an explicit task are two
// different first turns; combining them is a flag error, never a guess.
func TestPresetTaskMutualExclusion(t *testing.T) {
	task, help, err := resolveTask("flashcards", "write me a poem", "refine this")
	if err == nil {
		t.Fatalf("preset + task accepted; task=%.80q help=%v", task, help)
	}
	if help {
		t.Error("the conflict path must not set help")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should name the exclusivity: %v", err)
	}
	// Without a preset the two channels behave exactly as before: an explicit
	// -task wins, a bare positional is the task.
	task, help, err = resolveTask("", "explicit task", "positional task")
	if err != nil || help || task != "explicit task" {
		t.Errorf("no-preset resolution = %q help=%v err=%v; want the explicit task", task, help, err)
	}
	task, help, err = resolveTask("", "", "positional task")
	if err != nil || help || task != "positional task" {
		t.Errorf("bare positional = %q help=%v err=%v; want the positional task", task, help, err)
	}
}

// TestPresetHelpListsAllPresets: `-preset help` is the teaching artifact — it
// prints every preset's scaffold, verbatim but indented, plus the refinement
// convention.
func TestPresetHelpListsAllPresets(t *testing.T) {
	help := presetHelp()
	for _, p := range presets {
		for _, want := range []string{p.name, p.summary, indent(p.scaffold)} {
			if !strings.Contains(help, want) {
				t.Errorf("-preset help missing %q for preset %s", want, p.name)
			}
		}
	}
	if !strings.Contains(help, `Constraints: …`) {
		t.Errorf("-preset help should document the refinement convention:\n%s", help)
	}
	if got, showHelp, err := resolveTask("help", "", ""); err != nil || !showHelp || got != "" {
		t.Errorf("resolveTask(\"help\") = %q showHelp=%v err=%v; want help signal", got, showHelp, err)
	}
}

// TestUnknownPresetErrors: a misspelled preset is a flag error naming the
// bad value and the known ones, not a silent fall-through to a raw task.
func TestUnknownPresetErrors(t *testing.T) {
	task, help, err := resolveTask("quotations", "", "")
	if err == nil {
		t.Fatalf("unknown preset accepted; task=%.80q", task)
	}
	if help {
		t.Error("an unknown preset must not set help")
	}
	if !strings.Contains(err.Error(), `unknown preset "quotations"`) {
		t.Errorf("error should name the bad preset: %v", err)
	}
	for _, known := range presetNames() {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error should list known preset %q: %v", known, err)
		}
	}
}
