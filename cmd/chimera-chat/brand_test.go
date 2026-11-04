package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestBrandFooterRenders pins the wordmark: it must be present, right-aligned,
// and readable once the colour codes are stripped.
func TestBrandFooterRenders(t *testing.T) {
	m := newTestModel()
	m.width = 80
	line := stripANSI(m.brandView())
	if !strings.Contains(line, "Powered by Anvaya") {
		t.Fatalf("footer missing wordmark: %q", line)
	}
	if !strings.HasPrefix(line, " ") {
		t.Fatalf("footer is not right-aligned: %q", line)
	}
	if strings.TrimSpace(line) != "Powered by Anvaya" {
		t.Fatalf("footer text = %q", strings.TrimSpace(line))
	}
}

// TestBrandGlowMoves is the "running" part: successive ticks must colour the
// same text differently while leaving the text itself unchanged.
func TestBrandGlowMoves(t *testing.T) {
	// Without a TTY lipgloss strips colour, so every phase would render the same.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := newTestModel()
	m.width = 80

	first := m.brandView()
	m.phase = 3
	second := m.brandView()

	if first == second {
		t.Fatal("glow did not move between phases")
	}
	if stripANSI(first) != stripANSI(second) {
		t.Fatal("the wordmark itself changed; only the colour should move")
	}
}

// TestBrandHiddenWhenNarrow keeps the footer from overflowing narrow terminals.
func TestBrandHiddenWhenNarrow(t *testing.T) {
	m := newTestModel()
	m.width = 10
	if got := m.brandView(); got != "" {
		t.Fatalf("footer should be empty at width 10, got %q", got)
	}
}
