package main

import (
	"regexp"
	"strings"
	"testing"
)

// stripANSI removes styling so tests can assert on what the eye sees.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

const sampleMarkdown = "## Findings\n\nThe loop calls **six** tools, and the prompt is `buildPrompt`:\n\n" +
	"```go\nfunc buildPrompt() {}\n```\n\n- first item\n- second item\n"

// TestMarkdownRendersAssistantMessages is the visible contract for the
// renderer: structure is applied (no literal ## or ** left behind) and the
// content survives.
func TestMarkdownRendersAssistantMessages(t *testing.T) {
	m := newTestModel()
	m.viewport.Width = 80
	m.append(kindAssistant, sampleMarkdown)
	m.refreshViewport()

	view := stripANSI(m.viewport.View())
	t.Logf("rendered:\n%s", view)
	for _, want := range []string{"Findings", "six", "buildPrompt", "first item", "func buildPrompt"} {
		if !strings.Contains(view, want) {
			t.Errorf("rendered view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "**") {
		t.Errorf("bold markers were not consumed:\n%s", view)
	}
	if strings.Contains(view, "## ") {
		t.Errorf("heading markers were not consumed:\n%s", view)
	}
}

// TestMarkdownCacheInvalidatesOnResize guards the wrap: a rendered message is
// cached per width, and a resize must re-render rather than leave text wrapped
// for the old terminal.
func TestMarkdownCacheInvalidatesOnResize(t *testing.T) {
	m := newTestModel()
	m.viewport.Width = 100
	m.append(kindAssistant, strings.Repeat("word ", 200))
	m.refreshViewport()

	wide := m.entries[0].renderedWidth
	m.viewport.Width = 40
	m.refreshViewport()
	if m.entries[0].renderedWidth == wide {
		t.Fatalf("width stayed %d after resize", wide)
	}
	if m.entries[0].renderedWidth != 40 {
		t.Fatalf("renderedWidth = %d, want 40", m.entries[0].renderedWidth)
	}
}

// TestMarkdownSurvivesToolAndUserEntries keeps the non-assistant rendering
// intact: user prompts and the tool trace are styled, not passed through glamour.
func TestMarkdownSurvivesToolAndUserEntries(t *testing.T) {
	m := newTestModel()
	m.viewport.Width = 80
	m.append(kindUser, "read the file")
	m.append(kindTool, "⚙ read_file internal/api/server.go")
	m.append(kindAssistant, "It calls **buildPrompt**.")
	m.refreshViewport()

	view := stripANSI(m.viewport.View())
	for _, want := range []string{"read the file", "read_file internal/api/server.go", "It calls buildPrompt."} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}
