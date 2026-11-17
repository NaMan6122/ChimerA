package main

// The welcome screen's contract: it shows the wordmark, lists only gateways that
// answered, turns a keystroke into a backend, and explains an empty scan instead
// of exiting on it.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func newWelcome(list ...backend) *welcomeModel {
	return &welcomeModel{
		list:    list,
		width:   100,
		height:  30,
		baseURL: "http://localhost:8000/v1",
		ports:   portRange(8000, 8011),
		token:   "t",
	}
}

// TestWordmarkRenders pins the logo: five rows of glyphs, and the name is still
// readable once the colour is stripped.
func TestWordmarkRenders(t *testing.T) {
	rows := wordmarkRows(0)
	if len(rows) != 5 {
		t.Fatalf("wordmark has %d rows, want 5", len(rows))
	}
	if got := wordmarkWidth(); got < 30 || got > 60 {
		t.Fatalf("wordmark width = %d, want something 80 columns tolerate", got)
	}
	for i, row := range rows {
		plain := stripANSI(row)
		if strings.TrimSpace(plain) == "" {
			t.Errorf("row %d is blank", i)
		}
		if len([]rune(plain)) != wordmarkWidth()+1 { // trailing separator column
			t.Errorf("row %d is %d columns, want %d", i, len([]rune(plain)), wordmarkWidth()+1)
		}
	}
}

// TestWordmarkAnimates is the "animated" part: successive ticks recolour the
// same glyphs without changing them.
func TestWordmarkAnimates(t *testing.T) {
	// Without a TTY lipgloss strips colour, so every phase would render the same.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	first := strings.Join(wordmarkRows(0), "\n")
	second := strings.Join(wordmarkRows(5), "\n")
	if first == second {
		t.Fatal("the band did not move between phases")
	}
	if stripANSI(first) != stripANSI(second) {
		t.Fatal("the glyphs changed; only the colour should travel")
	}
}

// TestWelcomeListsLiveBackends: dead ports stay out of the chooser — they have
// no model to talk to and would only be selectable noise.
func TestWelcomeListsLiveBackends(t *testing.T) {
	m := newWelcome(
		backend{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		backend{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
		backend{BaseURL: "http://localhost:8011/v1", Err: "no gateway"},
	)
	view := stripANSI(m.View())
	for _, want := range []string{"chatgpt", "chimera-chatgpt", "deepseek", "chimera-deepseek", "↑↓ choose"} {
		if !strings.Contains(view, want) {
			t.Errorf("welcome screen missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "no gateway") {
		t.Errorf("dead port shown in the chooser:\n%s", view)
	}
}

// TestWelcomeSelection covers the keys: arrows move, enter opens that gateway.
func TestWelcomeSelection(t *testing.T) {
	m := newWelcome(
		backend{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		backend{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	)

	// Down moves to the second gateway, and wraps back around.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.index != 1 {
		t.Fatalf("index after down = %d, want 1", m.index)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.index != 0 {
		t.Fatalf("index after wrap = %d, want 0", m.index)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.chosen == nil || m.chosen.Provider != "deepseek" {
		t.Fatalf("enter chose %+v, want deepseek", m.chosen)
	}
	if cmd == nil {
		t.Fatal("enter should quit the program")
	}
}

// TestWelcomeQuitChoosesNothing: escaping is a decision, not a failure — the
// caller distinguishes it from an error and exits quietly.
func TestWelcomeQuitChoosesNothing(t *testing.T) {
	m := newWelcome(backend{Provider: "deepseek", BaseURL: "http://x/v1", Model: "chimera-deepseek", Up: true})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd == nil {
		t.Fatal("esc should quit the program")
	}
	if m.chosen != nil {
		t.Fatalf("esc chose %+v", m.chosen)
	}
}

// TestWelcomeEmptyScanShowsCommands is the cold-start path the old binary made
// impossible: no gateway means instructions, not an exit.
func TestWelcomeEmptyScanShowsCommands(t *testing.T) {
	m := newWelcome(backend{BaseURL: "http://localhost:8000/v1", Err: "no gateway"})
	view := stripANSI(m.View())
	for _, want := range []string{"no gateway answered", "chimera up deepseek", "chimera auth login deepseek"} {
		if !strings.Contains(view, want) {
			t.Errorf("empty state missing %q:\n%s", want, view)
		}
	}
	// Down/enter with nothing live must not panic or select anything.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.chosen != nil {
		t.Fatalf("chose %+v with no gateway live", m.chosen)
	}
}

// TestWelcomeRescanKeepsSelection: pressing r re-probes, and a provider that is
// still live keeps the cursor.
func TestWelcomeRescanKeepsSelection(t *testing.T) {
	m := newWelcome(
		backend{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		backend{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	)
	m.index = 1 // deepseek

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil {
		t.Fatal("r should start a rescan")
	}
	if !m.scanning {
		t.Fatal("rescan did not arm the scanning guard")
	}

	m.Update(welcomeScanMsg{keep: "deepseek", list: []backend{
		{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	}})
	if m.index != 1 {
		t.Fatalf("cursor moved to %d after rescan, want 1 (deepseek)", m.index)
	}
}

// TestWelcomeRescanSingleBackendOpens: a cold start whose rescan finds exactly
// one gateway has answered the question, so the chooser gets out of the way.
func TestWelcomeRescanSingleBackendOpens(t *testing.T) {
	m := newWelcome()
	m.Update(welcomeScanMsg{list: []backend{
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	}})
	if m.chosen == nil || m.chosen.Provider != "deepseek" {
		t.Fatalf("rescan to a single gateway chose %+v", m.chosen)
	}
}

// TestWelcomeRescanDoesNotAutoSelectWhenPopulated is the other half of that
// rule: once gateways are listed, a rescan that happens to return one must not
// open it unprompted — a 429 or a slow port could otherwise decide for the user.
func TestWelcomeRescanDoesNotAutoSelectWhenPopulated(t *testing.T) {
	m := newWelcome(
		backend{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		backend{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	)
	m.Update(welcomeScanMsg{list: []backend{
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	}})
	if m.chosen != nil {
		t.Fatalf("rescan opened %+v without a choice", m.chosen)
	}
	if len(m.live()) != 1 || m.index != 0 {
		t.Fatalf("rescan left the list at %d rows, cursor %d", len(m.live()), m.index)
	}
}

// TestCursorSeedsFromBaseAddress: the highlight is the default answer, so it
// follows the address the session pointed at — under any spelling of localhost.
func TestCursorSeedsFromBaseAddress(t *testing.T) {
	list := []backend{
		{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	}
	for _, base := range []string{"http://localhost:8010/v1", "http://127.0.0.1:8010/v1", "127.0.0.1:8010"} {
		if got := indexOf(list, "", base); got != 1 {
			t.Errorf("indexOf(%q) = %d, want 1 (deepseek)", base, got)
		}
	}
	// An address nothing answered falls back to the first row.
	if got := indexOf(list, "", "http://localhost:9999/v1"); got != 0 {
		t.Errorf("indexOf(unmatched) = %d, want 0", got)
	}
}

// TestWelcomeShowsBusyGateway renders the state probe() now reports: alive, with
// no model yet, and a reason instead of an empty column.
func TestWelcomeShowsBusyGateway(t *testing.T) {
	m := newWelcome(backend{
		BaseURL: "http://localhost:8010/v1",
		Up:      true,
		Note:    "busy (HTTP 429): the gateway rate-limits model discovery",
	})
	view := stripANSI(m.View())
	for _, want := range []string{"gateway", "429"} {
		if !strings.Contains(view, want) {
			t.Errorf("busy row missing %q:\n%s", want, view)
		}
	}
}
