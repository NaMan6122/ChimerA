// chimera-chat is the interactive front-end: a chat transcript, a tool trace and
// a status line, driven by the same agent loop as chimera-spin.
//
// The transcript you see is local. Nothing in it is ever sent back as a prefix:
// the provider keeps the conversation, so each message you type travels alone,
// and each tool round sends only its own results.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chimera/chimera/internal/spin"
)

var version = "dev"

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindTool
	kindResult
	kindRound
	kindNotice
	kindError
)

type entry struct {
	kind entryKind
	text string
}

type eventMsg spin.Event
type doneMsg struct {
	res spin.Result
	err error
}

type model struct {
	client   *spin.Client
	runner   *spin.Runner
	viewport viewport.Model
	input    textarea.Model
	spin     spinner.Model

	entries []entry
	msgs    chan tea.Msg

	busy    bool
	width   int
	height  int
	started time.Time
	rounds  int
}

func main() {
	var (
		baseURL, token, modelID, session, root string
		maxRounds                              int
		showVersion                            bool
	)
	flag.StringVar(&baseURL, "base-url", envOr("CHIMERA_BASE_URL", "http://localhost:8000/v1"), "gateway base URL")
	flag.StringVar(&token, "token", envOr("CHIMERA_API_KEY", "chimera"), "bearer token")
	flag.StringVar(&modelID, "model", "chimera-chatgpt", "model id")
	flag.StringVar(&session, "session", "", "session id (default: a fresh one)")
	flag.StringVar(&root, "root", ".", "workspace root the tools are confined to")
	flag.IntVar(&maxRounds, "max-rounds", 8, "hard stop on tool rounds per message")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("chimera-chat " + version)
		return
	}
	if session == "" {
		session = fmt.Sprintf("chat-%d", time.Now().UnixNano())
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "root:", err)
		os.Exit(1)
	}

	client := spin.NewClient(baseURL, token, modelID, session)
	if err := client.CheckModel(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	client.Refresh()

	m := &model{
		client: client,
		runner: spin.NewRunner(client, &spin.Executor{Root: absRoot}, maxRounds),
		msgs:   make(chan tea.Msg, 256),
	}
	m.runner.OnEvent = func(ev spin.Event) {
		select {
		case m.msgs <- eventMsg(ev):
		default: // a slow terminal drops progress events, never the answer
		}
	}

	m.input = textarea.New()
	m.input.Placeholder = "Ask about this repo…  (enter to send, /new for a fresh session, esc to quit)"
	m.input.Prompt = "› "
	m.input.ShowLineNumbers = false
	m.input.CharLimit = 0
	m.input.SetHeight(1)
	// Enter sends; ctrl+j inserts a newline.
	m.input.KeyMap.InsertNewline.SetKeys("ctrl+j")
	m.input.Focus()

	m.spin = spinner.New()
	m.spin.Spinner = spinner.Line
	m.spin.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m.viewport = viewport.New(80, 20)
	m.append(kindNotice, fmt.Sprintf("session %s · model %s · root %s", short(session), modelID, absRoot))

	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chat:", err)
		os.Exit(1)
	}
}

func (m *model) Init() tea.Cmd { return textarea.Blink }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = max(3, msg.Height-8)
		m.input.SetWidth(msg.Width - 4)
		m.refreshViewport()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		case "enter":
			return m.submit()
		}

	case eventMsg:
		m.handle(spin.Event(msg))
		if m.busy {
			return m, m.wait()
		}
		return m, nil

	case doneMsg:
		m.busy = false
		m.rounds = 0
		if msg.err != nil {
			m.append(kindError, msg.err.Error())
		} else if msg.res.Text != "" && msg.res.Text != strings.TrimSpace(msg.res.Text) {
			m.append(kindAssistant, msg.res.Text)
		}
		m.refreshViewport()
		return m, nil

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) View() string {
	head := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).
		Render(" Chimera ") +
		lipgloss.NewStyle().Faint(true).Render("· "+m.client.Model+" · "+short(m.client.Session))

	rule := lipgloss.NewStyle().Faint(true).Render(strings.Repeat("─", max(10, m.width-2)))

	status := ""
	if m.busy {
		status = m.spin.View() + lipgloss.NewStyle().Faint(true).Render(" working… (esc to quit)")
	} else {
		status = lipgloss.NewStyle().Faint(true).Render("enter send · /new new session · esc quit")
	}

	return "\n" + head + "\n" + rule + "\n" + m.viewport.View() + "\n" + rule + "\n" + m.input.View() + "\n" + status
}

func (m *model) submit() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	task := strings.TrimSpace(m.input.Value())
	if task == "" {
		return m, nil
	}
	m.input.Reset()

	switch task {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/new":
		m.client.NewSession(fmt.Sprintf("chat-%d", time.Now().UnixNano()))
		m.client.Refresh()
		m.append(kindNotice, "new session "+short(m.client.Session)+" — the next message starts a fresh upstream conversation")
		m.refreshViewport()
		return m, nil
	case "/help":
		m.append(kindNotice, "enter sends · ctrl+j newline · /new resets the session · esc quits")
		m.refreshViewport()
		return m, nil
	}

	m.append(kindUser, task)
	m.refreshViewport()
	m.busy, m.started = true, time.Now()
	return m, tea.Batch(m.spin.Tick, m.wait(), m.run(task))
}

// run executes the loop in the background and reports through m.msgs.
func (m *model) run(task string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			res, err := m.runner.Run(task)
			m.msgs <- doneMsg{res, err}
		}()
		return nil
	}
}

// wait reads the next event off the channel.
func (m *model) wait() tea.Cmd {
	return func() tea.Msg { return <-m.msgs }
}

func (m *model) handle(ev spin.Event) {
	switch ev.Kind {
	case spin.EventRound:
		m.rounds++
		m.append(kindRound, fmt.Sprintf("round %d · %s · %.1fs", ev.Round, human(ev.PromptChars), ev.Latency.Seconds()))
	case spin.EventNudge:
		m.append(kindNotice, "answered without reading anything — asking for evidence")
	case spin.EventTool:
		m.append(kindTool, "⚙ "+spin.Describe(ev.Tool))
	case spin.EventResult:
		m.append(kindResult, summarize(ev.Result))
	case spin.EventFinal:
		m.append(kindAssistant, ev.Text)
		m.append(kindRound, fmt.Sprintf("done in %d round(s) · %.1fs", ev.Round, time.Since(m.started).Seconds()))
	}
	m.refreshViewport()
}

func (m *model) append(k entryKind, text string) {
	m.entries = append(m.entries, entry{kind: k, text: strings.TrimRight(text, "\n")})
}

func (m *model) refreshViewport() {
	var b strings.Builder
	for _, e := range m.entries {
		switch e.kind {
		case kindUser:
			b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Render("› "+e.text) + "\n\n")
		case kindAssistant:
			b.WriteString(e.text + "\n\n")
		case kindTool:
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("  "+e.text) + "\n")
		case kindResult:
			b.WriteString(lipgloss.NewStyle().Faint(true).Render("    "+e.text) + "\n")
		case kindRound:
			b.WriteString(lipgloss.NewStyle().Faint(true).Render("  · "+e.text) + "\n")
		case kindNotice:
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("  ‹ "+e.text) + "\n\n")
		case kindError:
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("  ✗ "+e.text) + "\n\n")
		}
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

// summarize renders a tool result compactly: the model sees the whole thing, the
// user sees its shape.
func summarize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	first := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		first = s[:i]
	}
	if len(first) > 100 {
		first = first[:100] + "…"
	}
	return fmt.Sprintf("%s  (%s)", first, human(len(s)))
}

func human(n int) string {
	switch {
	case n >= 1000:
		return fmt.Sprintf("%.1fk chars", float64(n)/1000)
	default:
		return fmt.Sprintf("%d chars", n)
	}
}

func short(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && len(id)-i > 4 {
		return "…" + id[i+1:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
