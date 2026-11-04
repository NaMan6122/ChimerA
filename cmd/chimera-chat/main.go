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

	// Kept so /use can rebuild the client against another gateway without
	// asking the user to restart with different flags.
	token      string
	root       string
	maxRounds  int
	probePorts []int

	busy    bool
	width   int
	height  int
	started time.Time
	rounds  int

	// autoScroll follows new output. It turns off the moment the user scrolls
	// back, so a busy run cannot yank the transcript away from what is being read.
	autoScroll bool
}

// backendsMsg carries a completed discovery scan. target is set when a /use
// asked for a specific backend.
type backendsMsg struct {
	list   []backend
	target string
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
		client:     client,
		msgs:       make(chan tea.Msg, 256),
		token:      token,
		root:       absRoot,
		maxRounds:  maxRounds,
		probePorts: portRange(8000, 8011),
		autoScroll: true,
	}
	m.rewire()

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

	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
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
		case "pgup":
			m.scrollBy(-m.pageSize())
			return m, nil
		case "pgdown":
			m.scrollBy(m.pageSize())
			return m, nil
		case "ctrl+u":
			m.scrollBy(-m.pageSize() / 2)
			return m, nil
		case "ctrl+d":
			m.scrollBy(m.pageSize() / 2)
			return m, nil
		case "home":
			m.autoScroll = false
			m.viewport.GotoTop()
			return m, nil
		case "end":
			m.scrollToBottom()
			return m, nil
		case "enter":
			m.scrollToBottom() // a new message starts at the bottom again
			return m.submit()
		}

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.autoScroll = m.viewport.AtBottom()
		return m, cmd

	case backendsMsg:
		if msg.target == "" {
			m.append(kindNotice, renderBackends(msg.list, m.client.BaseURL))
			m.refreshViewport()
			return m, nil
		}
		b, err := resolveTarget(msg.list, msg.target)
		if err != nil {
			m.append(kindError, err.Error())
			m.refreshViewport()
			return m, nil
		}
		m.switchBackend(b)
		return m, nil

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
		status = m.spin.View() + lipgloss.NewStyle().Faint(true).Render(" working…")
	} else {
		status = lipgloss.NewStyle().Faint(true).Render("enter send · /backends · /use <provider> · /new · esc quit")
	}
	if !m.autoScroll {
		status += lipgloss.NewStyle().Foreground(lipgloss.Color("214")).
			Render(fmt.Sprintf("  ↑ %d lines below · end to follow", m.hiddenLines()))
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
		m.append(kindNotice, strings.Join([]string{
			"enter sends · ctrl+j newline · esc quits",
			"/backends        list gateways found on localhost:8000-8011",
			"/use <provider>  switch backend, e.g. /use deepseek  (also /use 8010 or /use <url>)",
			"/new             new conversation on the current backend",
			"/status          current backend, model, session, workspace",
		}, "\n"))
		m.refreshViewport()
		return m, nil
	case "/backends", "/gateways":
		m.append(kindNotice, "scanning localhost:8000-8011…")
		m.refreshViewport()
		return m, m.scan("")
	case "/status":
		m.append(kindNotice, fmt.Sprintf("backend %s\nmodel   %s\nsession %s (turn %d)\nroot    %s",
			m.client.BaseURL, m.client.Model, short(m.client.Session), m.client.Turns, m.root))
		m.refreshViewport()
		return m, nil
	}

	if strings.HasPrefix(task, "/use ") {
		target := strings.TrimSpace(strings.TrimPrefix(task, "/use "))
		m.append(kindNotice, "looking for "+target+"…")
		m.refreshViewport()
		return m, m.scan(target)
	}
	if task == "/use" {
		m.append(kindNotice, "usage: /use <provider|port|url>   e.g. /use deepseek")
		m.refreshViewport()
		return m, nil
	}
	if strings.HasPrefix(task, "/") {
		m.append(kindError, "unknown command "+task+" — try /help")
		m.refreshViewport()
		return m, nil
	}

	m.append(kindUser, task)
	m.refreshViewport()
	m.busy, m.started = true, time.Now()
	return m, tea.Batch(m.spin.Tick, m.wait(), m.run(task))
}

// rewire (re)builds the runner and points its events at this model. Called on
// start and after every /use.
func (m *model) rewire() {
	m.runner = spin.NewRunner(m.client, &spin.Executor{Root: m.root}, m.maxRounds)
	m.runner.OnEvent = func(ev spin.Event) {
		select {
		case m.msgs <- eventMsg(ev):
		default: // a slow terminal drops progress events, never the answer
		}
	}
}

// scan discovers which gateways are live. It runs off the UI goroutine so a dead
// port cannot freeze the chat.
func (m *model) scan(target string) tea.Cmd {
	return func() tea.Msg {
		list := scan(defaultProbeClient(), probeTargets(m.client.BaseURL, m.probePorts), m.token)
		return backendsMsg{list: list, target: target}
	}
}

// switchBackend repoints the session at another gateway. A fresh session id is
// used because the previous conversation lives in the old gateway.
func (m *model) switchBackend(b backend) {
	m.client = spin.NewClient(b.BaseURL, m.token, b.Model, fmt.Sprintf("chat-%d", time.Now().UnixNano()))
	m.client.Refresh()
	m.rewire()
	m.append(kindNotice, fmt.Sprintf("backend → %s · %s · model %s · session %s",
		b.Provider, b.BaseURL, b.Model, short(m.client.Session)))
	m.refreshViewport()
}

func portRange(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		out = append(out, p)
	}
	return out
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

// scrollBy moves the transcript and remembers whether the user is still
// following the tail.
func (m *model) scrollBy(delta int) {
	if delta < 0 {
		m.viewport.ScrollUp(-delta)
	} else {
		m.viewport.ScrollDown(delta)
	}
	m.autoScroll = m.viewport.AtBottom()
}

func (m *model) scrollToBottom() {
	m.autoScroll = true
	m.viewport.GotoBottom()
}

func (m *model) pageSize() int {
	if m.viewport.Height > 2 {
		return m.viewport.Height - 1
	}
	return 1
}

// hiddenLines is how much output sits below the fold, for the status line.
func (m *model) hiddenLines() int {
	return max(0, m.viewport.TotalLineCount()-(m.viewport.YOffset+m.viewport.Height))
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
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
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
