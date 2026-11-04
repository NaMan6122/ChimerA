package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSlashOpensCommandHinter pins the discoverable path: "/" shows every
// command, and typing narrows the list by prefix.
func TestSlashOpensCommandHinter(t *testing.T) {
	m := newTestModel()

	m.input.SetValue("/")
	m.syncPalette()
	if m.palMode != palCommands {
		t.Fatalf("palMode = %v, want commands", m.palMode)
	}
	if len(m.palItems) != len(slashCommands) {
		t.Fatalf("got %d hints, want %d", len(m.palItems), len(slashCommands))
	}

	m.input.SetValue("/us")
	m.syncPalette()
	if len(m.palItems) != 1 || m.palItems[0].label != "/use" {
		t.Fatalf("prefix filter wrong: %+v", m.palItems)
	}

	// Deleting the slash must dismiss the popup.
	m.input.SetValue("hello")
	m.syncPalette()
	if m.palMode != palNone {
		t.Fatal("popup stayed open without a leading slash")
	}
}

// TestUseCommandOpensPicker covers the second step: /use switches the popup from
// command hints to live backends, and starts in a loading state.
func TestUseCommandOpensPicker(t *testing.T) {
	m := newTestModel()
	if cmd := m.openBackendPicker(); cmd == nil {
		t.Fatal("opening the picker should kick off a scan")
	}
	if m.palMode != palBackends || !m.palLoading {
		t.Fatalf("picker state = %v loading=%v", m.palMode, m.palLoading)
	}

	_, _ = m.Update(backendsMsg{target: "__picker__", list: []backend{
		{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
		{BaseURL: "http://localhost:8011/v1", Err: "no gateway"},
	}})
	if m.palLoading {
		t.Fatal("still loading after the scan returned")
	}
	if len(m.palItems) != 2 {
		t.Fatalf("got %d pickable backends, want 2 (dead ports excluded): %+v", len(m.palItems), m.palItems)
	}
}

// TestPickerSelectionSwitchesBackend is the payoff: choosing a row repoints the
// client and model without a restart.
func TestPickerSelectionSwitchesBackend(t *testing.T) {
	srv := fakeGateway("chimera-deepseek")
	defer srv.Close()

	m := newTestModel()
	m.palMode = palBackends
	m.palItems = []palItem{{
		label: "deepseek",
		be:    backend{Provider: "deepseek", BaseURL: srv.URL, Model: "chimera-deepseek", Up: true},
	}}
	m.palIndex = 0

	if _, _ = m.applyPal(); m.client.Model != "chimera-deepseek" {
		t.Fatalf("model = %q after selection", m.client.Model)
	}
	if m.client.BaseURL != srv.URL {
		t.Fatalf("base url = %q", m.client.BaseURL)
	}
	if m.palMode != palNone {
		t.Fatal("popup stayed open after selection")
	}
	// A backend switch starts a fresh conversation in the new gateway.
	if m.client.Turns != 0 {
		t.Fatalf("Turns = %d, want 0", m.client.Turns)
	}
}

// TestPaletteRendersAboveInput checks the popup is actually in the frame, above
// the input, with its title and key hints.
func TestPaletteRendersAboveInput(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 80, 24
	m.viewport.Width, m.viewport.Height = 80, 12
	m.input.SetWidth(76)
	m.input.SetValue("/")
	m.syncPalette()

	view := stripANSI(m.View())
	t.Logf("frame:\n%s", view)
	if !strings.Contains(view, "commands") {
		t.Error("popup title missing")
	}
	if !strings.Contains(view, "↑↓ move") {
		t.Error("key hints missing")
	}
	if idx := strings.Index(view, "/use"); idx < 0 || idx > strings.Index(view, "enter send") {
		t.Error("popup is not above the input/status area")
	}
}

// TestEscClosesPalette: esc is quit everywhere else, but with the popup open it
// must only dismiss the popup.
func TestEscClosesPalette(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/")
	m.syncPalette()

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("esc quit the program while the popup was open")
	}
	if m.palMode != palNone {
		t.Fatal("esc did not close the popup")
	}
}

// TestBareUseOpensPicker keeps /use consistent whether it is typed or picked.
func TestBareUseOpensPicker(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/use")
	if _, _ = m.submit(); m.palMode != palBackends {
		t.Fatalf("bare /use did not open the picker (mode=%v)", m.palMode)
	}
}
