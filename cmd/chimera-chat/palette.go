package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// paletteMode says which popup, if any, is open above the input.
type paletteMode int

const (
	palNone paletteMode = iota
	palCommands
	palBackends
)

// palItem is one selectable row. Commands fill cmd; backends fill be.
type palItem struct {
	label string
	desc  string
	cmd   string
	be    backend
}

// slashCommands is the hint list: typing "/" shows these, filtered as you type.
var slashCommands = []palItem{
	{label: "/use", desc: "switch backend (chatgpt, deepseek, qwen…)", cmd: "/use"},
	{label: "/backends", desc: "list live gateways", cmd: "/backends"},
	{label: "/new", desc: "new conversation on this backend", cmd: "/new"},
	{label: "/status", desc: "backend · model · session", cmd: "/status"},
	{label: "/help", desc: "all commands and keys", cmd: "/help"},
	{label: "/quit", desc: "exit", cmd: "/quit"},
}

// filterCommands matches the typed prefix against the command names.
func filterCommands(value string) []palItem {
	q := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "/"))
	var out []palItem
	for _, c := range slashCommands {
		if q == "" || strings.HasPrefix(strings.TrimPrefix(c.label, "/"), q) {
			out = append(out, c)
		}
	}
	return out
}

// syncPalette recomputes the popup from what is in the input box. Called after
// every edit, so opening and closing follow the text rather than a mode flag.
func (m *model) syncPalette() {
	value := m.input.Value()

	if !strings.HasPrefix(value, "/") {
		if m.palMode != palNone {
			m.closePalette()
		}
		return
	}
	// "/use " (with the space) is the backend picker; anything shorter is still
	// command completion.
	if strings.HasPrefix(value, "/use ") {
		if m.palMode != palBackends {
			m.openBackendPicker()
		}
		return
	}
	if m.palMode == palBackends {
		m.closePalette()
	}

	items := filterCommands(value)
	if len(items) == 0 {
		m.closePalette()
		return
	}
	m.palMode = palCommands
	m.palItems = items
	if m.palIndex >= len(items) {
		m.palIndex = 0
	}
	m.layout()
}

func (m *model) closePalette() {
	m.palMode = palNone
	m.palItems = nil
	m.palLoading = false
	m.palIndex = 0
	m.layout()
}

// openBackendPicker shows live gateways to choose from. Discovery is async, so
// the popup opens immediately in a loading state.
func (m *model) openBackendPicker() tea.Cmd {
	m.palMode = palBackends
	m.palLoading = true
	m.palItems = nil
	m.palIndex = 0
	m.layout()
	return m.scan("__picker__")
}

func (m *model) movePal(delta int) {
	if len(m.palItems) == 0 {
		return
	}
	m.palIndex = (m.palIndex + delta + len(m.palItems)) % len(m.palItems)
}

// completePal puts the highlighted command into the input without running it.
func (m *model) completePal() {
	if m.palMode != palCommands || len(m.palItems) == 0 {
		return
	}
	cmd := m.palItems[m.palIndex].label
	if cmd == "/use" {
		cmd = "/use "
	}
	m.input.SetValue(cmd)
	m.input.CursorEnd()
	m.syncPalette()
}

// applyPal runs the highlighted row.
func (m *model) applyPal() (tea.Model, tea.Cmd) {
	if m.palLoading || len(m.palItems) == 0 {
		return m, nil
	}
	item := m.palItems[m.palIndex]

	if m.palMode == palBackends {
		if item.be.BaseURL == "" {
			return m, nil // "(no gateways)" placeholder
		}
		m.switchBackend(item.be)
		m.input.Reset()
		m.closePalette()
		return m, nil
	}

	m.closePalette()
	m.input.SetValue(item.cmd)
	if item.cmd == "/use" {
		m.input.SetValue("/use ")
		m.input.CursorEnd()
		return m, m.openBackendPicker()
	}
	return m.submit()
}

// paletteView renders the popup above the input, or "" when closed.
func (m *model) paletteView() string {
	if m.palMode == palNone {
		return ""
	}
	title := "commands"
	if m.palMode == palBackends {
		title = "backends"
	}

	width := m.width - 4
	if width > 78 {
		width = 78
	}
	if width < 30 {
		width = 30
	}

	var body strings.Builder
	if m.palLoading {
		body.WriteString(lipgloss.NewStyle().Faint(true).Render("scanning localhost:8000-8011…") + "\n")
	}
	for i, it := range m.palItems {
		label := it.label
		if m.palMode == palBackends && it.be.BaseURL != "" &&
			strings.EqualFold(strings.TrimRight(it.be.BaseURL, "/"), strings.TrimRight(m.client.BaseURL, "/")) {
			label = "→ " + label
		} else {
			label = "  " + label
		}
		line := fmt.Sprintf("%-13s %s", label, lipgloss.NewStyle().Faint(true).Render(it.desc))
		if i == m.palIndex {
			line = lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("255")).Render(
				fmt.Sprintf("%-13s %s", label, it.desc))
		}
		body.WriteString(line + "\n")
	}
	body.WriteString(lipgloss.NewStyle().Faint(true).Render("↑↓ move · enter select · tab complete · esc close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(width)
	return box.Render(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")).Render(title) + "\n" + body.String())
}

// paletteHeight is how many rows the popup needs, for the viewport layout.
func (m *model) paletteHeight() int {
	if m.palMode == palNone {
		return 0
	}
	rows := len(m.palItems) + 2 // border
	if m.palLoading {
		rows++
	}
	if rows > 10 {
		rows = 10
	}
	return rows
}

// layout keeps the transcript sized to whatever is left above the input when
// the popup opens or closes.
func (m *model) layout() {
	if m.height <= 0 {
		return
	}
	m.viewport.Height = max(3, m.height-8-m.paletteHeight())
}
