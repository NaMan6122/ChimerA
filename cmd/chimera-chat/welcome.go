package main

// welcome.go — the first screen: choose which gateway to talk to.
//
// A gateway serves exactly one provider, so "which model?" is really "which
// backend?". Discovery already knows what answered (backends.go); this screen
// turns that list into one keystroke instead of a required -model flag.
//
// It opens only when there is something to decide: one live gateway is selected
// automatically, several need a human, none gets the command that starts one.
// Quitting here leaves without allocating the agent loop, the tools or the
// transcript.

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// errNoBackendChosen reports a deliberate quit, not a failure.
var errNoBackendChosen = errors.New("no backend chosen")

// welcomeModel is the backend chooser. It is a tea.Program of its own, run
// before the chat exists.
type welcomeModel struct {
	list   []backend
	index  int
	phase  int
	width  int
	height int
	chosen *backend

	// scanning guards against queueing a second probe while one is in flight.
	scanning bool

	// rescan inputs: the same probe /backends uses.
	baseURL string
	ports   []int
	token   string
}

// welcomeScanMsg carries the result of an r-triggered rescan. keep/keepBase
// identify the row that was selected, so a rescan does not move the cursor under
// the user's fingers.
type welcomeScanMsg struct {
	list     []backend
	keep     string
	keepBase string
}

func (m *welcomeModel) Init() tea.Cmd { return brandAnimation() }

func (m *welcomeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case brandTick:
		m.phase++
		return m, brandAnimation()

	case welcomeScanMsg:
		hadNone := len(m.live()) == 0
		m.list, m.scanning = msg.list, false
		live := m.live()
		m.index = indexOf(live, msg.keep, msg.keepBase)
		// A cold start that now finds exactly one gateway has answered the
		// question for the user. A rescan of an already-populated list must not:
		// one press of r would otherwise open a backend they never chose.
		if hadNone && len(live) == 1 {
			m.chosen = &live[0]
			return m, tea.Quit
		}
		return m, nil

	case tea.KeyMsg:
		live := m.live()
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if len(live) > 0 {
				m.index = (m.index - 1 + len(live)) % len(live)
			}
			return m, nil
		case "down", "j", "tab":
			if len(live) > 0 {
				m.index = (m.index + 1) % len(live)
			}
			return m, nil
		case "enter":
			if len(live) > 0 {
				m.chosen = &live[m.index]
				return m, tea.Quit
			}
			return m, nil
		case "r":
			if !m.scanning {
				return m, m.rescan()
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *welcomeModel) View() string {
	var b strings.Builder
	b.WriteString("\n")

	// Centre the wordmark on the plain width, then colour it: measuring after
	// styling would count escape bytes as columns.
	pad := strings.Repeat(" ", max(2, (m.width-wordmarkWidth())/2))
	for _, row := range wordmarkRows(m.phase) {
		b.WriteString(pad + row + "\n")
	}
	b.WriteString("\n")

	sub := "chat subscriptions, as an OpenAI-compatible API"
	b.WriteString(strings.Repeat(" ", max(2, (m.width-len(sub))/2)) + lipgloss.NewStyle().Faint(true).Render(sub) + "\n\n")

	if m.scanning {
		b.WriteString("  " + lipgloss.NewStyle().Faint(true).Render("scanning localhost:8000-8011…") + "\n\n")
	}

	live := m.live()
	if len(live) == 0 {
		b.WriteString(m.emptyView())
		b.WriteString("\n")
		b.WriteString(m.hintView(false))
		return b.String()
	}

	// One gateway is the only option, so it needs no choosing — but showing it
	// confirms what the scan found before the chat opens.
	choosing := len(live) > 1
	for i, bk := range live {
		cursor := "  "
		if choosing && i == m.index {
			cursor = "▸ "
		}
		// Pad before styling: escape codes count as columns, so a styled string
		// run through %-10s would not line up with the row above it.
		name := fmt.Sprintf("%-10s", orGateway(bk.Provider))
		addr := fmt.Sprintf("%-28s", bk.BaseURL)
		what := bk.Model
		if what == "" {
			what = bk.Note
		}
		if choosing && i == m.index {
			name = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render(name)
			addr = lipgloss.NewStyle().Faint(true).Render(addr)
		} else {
			addr = lipgloss.NewStyle().Faint(true).Render(addr)
		}
		fmt.Fprintf(&b, "  %s%s %s %s\n", cursor, name, addr, what)
	}
	b.WriteString("\n")
	b.WriteString(m.hintView(choosing))
	return b.String()
}

// orGateway names a backend whose provider is unknown — a gateway that answered
// but would not say what it serves.
func orGateway(provider string) string {
	if provider == "" {
		return "gateway"
	}
	return provider
}

// emptyView explains an empty scan with the commands that fill it.
func (m *welcomeModel) emptyView() string {
	head := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("no gateway answered on localhost:8000-8011")
	label := lipgloss.NewStyle().Faint(true)
	var b strings.Builder
	b.WriteString("  " + head + "\n\n")
	fmt.Fprintf(&b, "  %s  %s\n", label.Render("start one    "), lipgloss.NewStyle().Bold(true).Render("chimera up deepseek"))
	fmt.Fprintf(&b, "  %s  %s\n", label.Render("all of them  "), lipgloss.NewStyle().Bold(true).Render("chimera up"))
	fmt.Fprintf(&b, "  %s  %s\n", label.Render("first time?  "), lipgloss.NewStyle().Bold(true).Render("chimera auth login deepseek"))
	return b.String()
}

func (m *welcomeModel) hintView(hasChoice bool) string {
	keys := "r rescan · esc quit"
	if hasChoice {
		keys = "↑↓ choose · enter open · " + keys
	} else if len(m.live()) > 0 {
		keys = "enter open · " + keys
	}
	return "  " + lipgloss.NewStyle().Faint(true).Render(keys) + "\n"
}

// live returns the gateways that answered, in scan order (sorted by provider).
func (m *welcomeModel) live() []backend {
	var up []backend
	for _, b := range m.list {
		if b.Up {
			up = append(up, b)
		}
	}
	return up
}

func (m *welcomeModel) rescan() tea.Cmd {
	m.scanning = true
	keep, keepBase := "", ""
	if live := m.live(); len(live) > 0 && m.index < len(live) {
		keep, keepBase = live[m.index].Provider, live[m.index].BaseURL
	}
	base, ports, token := m.baseURL, m.ports, m.token
	return func() tea.Msg {
		return welcomeScanMsg{
			list:     scan(defaultProbeClient(), probeTargets(base, ports), token),
			keep:     keep,
			keepBase: keepBase,
		}
	}
}

// indexOf finds the row to select: by address first (the identity that cannot
// collide), then by provider, else the first row.
func indexOf(list []backend, provider, base string) int {
	if base != "" {
		for i, b := range list {
			if canonicalKey(b.BaseURL) == canonicalKey(base) {
				return i
			}
		}
	}
	if provider != "" {
		for i, b := range list {
			if strings.EqualFold(b.Provider, provider) {
				return i
			}
		}
	}
	return 0
}

// chooseBackend runs the welcome screen and returns the gateway the user
// opened. A quit returns errNoBackendChosen so the caller can exit quietly.
func chooseBackend(list []backend, baseURL string, ports []int, token string) (backend, error) {
	m := &welcomeModel{list: list, baseURL: baseURL, ports: ports, token: token}
	// Start on the gateway the session already pointed at, not on whichever
	// provider sorts first: the highlight is the default answer.
	m.index = indexOf(m.live(), "", baseURL)
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return backend{}, err
	}
	fm, ok := final.(*welcomeModel)
	if !ok || fm.chosen == nil {
		return backend{}, errNoBackendChosen
	}
	return *fm.chosen, nil
}

// ── wordmark ────────────────────────────────────────────────

// wordmarkWord is the product name in block glyphs: five rows, variable-width
// letters, one column between them — narrow enough for an 80-column terminal,
// wide enough to read as a logo.
var wordmarkWord = "CHIMERA"

var wordmarkFont = map[rune][5]string{
	'C': {"████", "█   ", "█   ", "█   ", "████"},
	'H': {"█  █", "█  █", "████", "█  █", "█  █"},
	'I': {"████", " ██ ", " ██ ", " ██ ", "████"},
	'M': {"█   █", "██ ██", "█ █ █", "█   █", "█   █"},
	'E': {"████", "█   ", "███ ", "█   ", "████"},
	'R': {"████", "█  █", "████", "█ █ ", "█  █"},
	'A': {" ██ ", "█  █", "████", "█  █", "█  █"},
}

// wordmarkWidth is the plain (uncoloured) width of the rendered wordmark.
func wordmarkWidth() int {
	w := -1
	for _, r := range wordmarkWord {
		w += len([]rune(wordmarkFont[r][0])) + 1
	}
	return w
}

// wordmarkRows renders the glyphs with a band of light travelling across them.
// The band sweeps in from the left, crosses, and exits right — the same motion
// as the footer, at logo scale.
func wordmarkRows(phase int) []string {
	w := wordmarkWidth()
	pos := phase%(w+16) - 8

	rows := make([]string, 5)
	for row := range rows {
		var b strings.Builder
		x := 0
		for _, r := range wordmarkWord {
			for _, ch := range []rune(wordmarkFont[r][row]) {
				if ch == ' ' {
					b.WriteRune(' ')
					x++
					continue
				}
				b.WriteString(lipgloss.NewStyle().Foreground(bandColour(abs(pos - x))).Render(string(ch)))
				x++
			}
			b.WriteRune(' ')
			x++
		}
		rows[row] = b.String()
	}
	return rows
}

// bandColour maps distance from the band's core to a colour: amber in the
// middle, embers at the edge, dim slack well behind it.
func bandColour(d int) lipgloss.Color {
	switch {
	case d == 0:
		return "214"
	case d <= 2:
		return "208"
	case d <= 4:
		return "203"
	case d <= 7:
		return "240"
	default:
		return "238"
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
