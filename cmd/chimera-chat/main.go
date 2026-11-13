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
	"github.com/charmbracelet/glamour"
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
	// Assistant messages are rendered once per viewport width and cached: glamour
	// wraps text, so a resize invalidates the cache but a new event must not
	// re-render the whole transcript.
	rendered      string
	renderedWidth int
}

type eventMsg spin.Event

type doneMsg struct {
	res spin.Result
	err error
}

// brandTick advances the footer's travelling highlight.
type brandTick struct{}

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

	// Markdown renderer, rebuilt when the viewport width changes.
	md       *glamour.TermRenderer
	mdWidth  int
	mdFailed bool

	// Slash-command hint and backend picker.
	palMode    paletteMode
	palItems   []palItem
	palIndex   int
	palLoading bool

	// Session transcript. Observational only: nothing is ever sent back from it.
	sessionsDir string
	rec         *spin.Recorder

	// tools lives for the whole TUI session so background monitors survive
	// across messages and backend switches; ledger follows the harness-side
	// session (one per /new).
	tools  *spin.Executor
	ledger *spin.Ledger

	// Footer animation phase.
	phase int
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
		sessionsDir                            string
		maxRounds                              int
		showVersion                            bool
	)
	flag.StringVar(&baseURL, "base-url", envOr("CHIMERA_BASE_URL", "http://localhost:8000/v1"), "gateway base URL")
	flag.StringVar(&token, "token", envOr("CHIMERA_API_KEY", "chimera"), "bearer token")
	flag.StringVar(&modelID, "model", "chimera-chatgpt", "model id")
	flag.StringVar(&session, "session", "", "session id (default: a fresh one)")
	flag.StringVar(&root, "root", ".", "workspace root the tools are confined to")
	flag.IntVar(&maxRounds, "max-rounds", 16, "hard stop on tool rounds per message")
	flag.StringVar(&sessionsDir, "sessions-dir", spin.DefaultSessionsDir(), "where session transcripts are saved")
	var noSave bool
	flag.BoolVar(&noSave, "no-save", false, "do not write a session transcript")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	var listBackends bool
	flag.BoolVar(&listBackends, "list-backends", false, "probe local gateways, print what answers, and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("chimera-chat " + version)
		return
	}
	if listBackends {
		list := scan(defaultProbeClient(), probeTargets(baseURL, portRange(8000, 8011)), token)
		var live []backend
		for _, b := range list {
			if b.Up {
				live = append(live, b)
			}
		}
		if len(live) == 0 {
			fmt.Println("no gateway answered on localhost:8000-8011")
			fmt.Println("start one:  TRANSPORT=webapi PROVIDER=deepseek API_PORT=8010 chimera")
			return
		}
		fmt.Println(renderBackends(live, baseURL))
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
		client:      client,
		msgs:        make(chan tea.Msg, 256),
		token:       token,
		root:        absRoot,
		maxRounds:   maxRounds,
		probePorts:  portRange(8000, 8011),
		autoScroll:  true,
		sessionsDir: sessionsDir,
	}
	m.rewire()

	// Transcript: written for the user, never read back into a request.
	if !noSave {
		if rec, rerr := spin.NewRecorder(sessionsDir); rerr == nil {
			rec.Start(session, modelID, baseURL, absRoot, nil)
			m.rec = rec
			defer rec.Close()
		} else {
			fmt.Fprintf(os.Stderr, "transcript disabled: %v\n", rerr)
		}
	}
	// Reopening a session id restores its transcript for display only; the loop
	// still starts with an empty prompt budget.
	if recs, rerr := spin.LoadTranscript(sessionsDir, session); rerr == nil && len(recs) > 0 {
		m.restore(recs)
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

	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chat:", err)
		os.Exit(1)
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, brandAnimation())
}

// brandAnimation ticks the footer glow. 12 fps is enough to read as motion and
// cheap enough to leave running for a whole session.
func brandAnimation() tea.Cmd {
	return tea.Tick(85*time.Millisecond, func(time.Time) tea.Msg { return brandTick{} })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		m.input.SetWidth(msg.Width - 4)
		m.layout()
		m.refreshViewport()
		return m, nil

	case tea.KeyMsg:
		// The popup owns the arrow keys, enter, tab and esc while it is open.
		if m.palMode != palNone {
			switch msg.String() {
			case "up", "ctrl+p":
				m.movePal(-1)
				return m, nil
			case "down", "ctrl+n":
				m.movePal(1)
				return m, nil
			case "esc":
				m.closePalette()
				return m, nil
			case "tab":
				m.completePal()
				return m, nil
			case "enter":
				return m.applyPal()
			}
		}
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
		if msg.target == "__picker__" {
			m.palLoading = false
			m.palItems = nil
			for _, b := range msg.list {
				if b.Up {
					m.palItems = append(m.palItems, palItem{label: b.Provider, desc: b.BaseURL + " · " + b.Model, be: b})
				}
			}
			if len(m.palItems) == 0 {
				m.palItems = []palItem{{label: "(none)", desc: "start one: TRANSPORT=webapi PROVIDER=deepseek chimera"}}
			}
			m.palIndex = 0
			m.layout()
			return m, nil
		}
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
			if m.rec != nil {
				m.rec.Note("error", msg.err.Error())
			}
			m.append(kindError, msg.err.Error())
		} else if msg.res.Text != "" && msg.res.Text != strings.TrimSpace(msg.res.Text) {
			m.append(kindAssistant, msg.res.Text)
		}
		m.refreshViewport()
		return m, nil

	case brandTick:
		m.phase++
		return m, brandAnimation()

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
	m.syncPalette()
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

	pal := ""
	if pv := m.paletteView(); pv != "" {
		pal = pv + "\n"
	}
	footer := m.brandView()
	if footer != "" {
		footer = "\n" + footer
	}
	return "\n" + head + "\n" + rule + "\n" + m.viewport.View() + "\n" + pal + rule + "\n" + m.input.View() + "\n" + status + footer
}

// brandView is the wordmark with a bright band travelling across dim text: a
// running highlight rather than a static colour.
func (m *model) brandView() string {
	const brand = "Powered by Anvaya"
	if m.width < len(brand)+4 {
		return ""
	}
	// The band sweeps in from the left, crosses, and exits to the right.
	pos := m.phase%(len(brand)+16) - 8

	var b strings.Builder
	for i, r := range brand {
		d := pos - i
		if d < 0 {
			d = -d
		}
		var colour lipgloss.Color
		switch {
		case d == 0:
			colour = "214" // amber core
		case d <= 2:
			colour = "208"
		case d <= 4:
			colour = "203"
		case d <= 7:
			colour = "240"
		default:
			colour = "238"
		}
		b.WriteString(lipgloss.NewStyle().Foreground(colour).Render(string(r)))
	}
	pad := strings.Repeat(" ", max(0, m.width-len(brand)-2))
	return pad + b.String() + "  "
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
	m.closePalette()

	switch task {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/new":
		m.client.NewSession(fmt.Sprintf("chat-%d", time.Now().UnixNano()))
		m.client.Refresh()
		m.rotateTranscript()
		m.ledger = nil // the ledger belongs to the harness-side session
		m.append(kindNotice, "new session "+short(m.client.Session)+" — the next message starts a fresh upstream conversation")
		m.refreshViewport()
		return m, nil
	case "/transcript":
		if m.rec == nil {
			m.append(kindNotice, "transcript recording is off (-no-save)")
		} else {
			m.append(kindNotice, "transcript: "+m.rec.Path())
		}
		m.refreshViewport()
		return m, nil
	case "/help":
		m.append(kindNotice, strings.Join([]string{
			"enter sends · ctrl+j newline · esc quits",
			"/backends        list gateways found on localhost:8000-8011",
			"/use <provider>  switch backend, e.g. /use deepseek  (also /use 8010 or /use <url>)",
			"/new             new conversation on the current backend",
			"/status          current backend, model, session, workspace",
			"/transcript      where this session is being saved on disk",
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
		// Bare /use opens the picker rather than printing usage: the discoverable
		// path is the one people take.
		m.input.SetValue("/use ")
		m.input.CursorEnd()
		return m, m.openBackendPicker()
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
	if m.tools == nil {
		m.tools = &spin.Executor{Root: m.root}
	}
	if m.ledger == nil {
		m.ledger = spin.NewLedger(m.sessionsDir, m.client.Session)
	}
	m.runner = spin.NewRunner(m.client, m.tools, m.maxRounds)
	m.runner.Ledger = m.ledger
	m.runner.OnEvent = func(ev spin.Event) {
		if m.rec != nil {
			m.rec.Observe(ev) // transcript only; never feeds the loop
		}
		select {
		case m.msgs <- eventMsg(ev):
		default: // a slow terminal drops progress events, never the answer
		}
	}
}

// restore replays a saved transcript into the view. Display only: the loop is
// untouched, so the next turn still sends just what the user types.
func (m *model) restore(recs []spin.Record) {
	shown := 0
	for _, r := range recs {
		switch r.Kind {
		case "user":
			m.append(kindUser, r.Text)
			shown++
		case "final":
			m.append(kindAssistant, r.Text)
			shown++
		case "tool":
			m.append(kindTool, "⚙ "+r.Name+" "+r.Arguments)
		case "result":
			m.append(kindResult, summarize(r.Result))
		}
	}
	if shown > 0 {
		m.append(kindNotice, fmt.Sprintf("restored %d message(s) from this session's transcript — shown for context, not sent to the model", shown))
	}
	m.refreshViewport()
}

// rotateTranscript starts a new transcript file after /new or /use.
func (m *model) rotateTranscript() {
	if m.rec != nil {
		m.rec.Start(m.client.Session, m.client.Model, m.client.BaseURL, m.root, nil)
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
	m.rotateTranscript()
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
		text := ev.Text
		if text == "" {
			text = "answered without reading anything — asking for evidence"
		}
		m.append(kindNotice, text)
	case spin.EventCompact:
		text := ev.Text
		if text == "" {
			text = "context compacted — fresh upstream session"
		}
		m.append(kindNotice, text)
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
	m.ensureRenderer()

	var b strings.Builder
	for i := range m.entries {
		e := &m.entries[i]

		if e.kind == kindAssistant {
			if e.renderedWidth != m.viewport.Width || e.rendered == "" {
				e.rendered = renderMarkdown(m.md, e.text)
				e.renderedWidth = m.viewport.Width
			}
			b.WriteString(e.rendered + "\n\n")
			continue
		}

		switch e.kind {
		case kindUser:
			b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Render("› "+e.text) + "\n\n")
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

// ensureRenderer rebuilds the Markdown renderer when the wrap width changes.
func (m *model) ensureRenderer() {
	width := m.viewport.Width
	if m.md != nil && m.mdWidth == width {
		return
	}
	r, err := newMarkdown(width)
	if err != nil {
		m.md, m.mdFailed = nil, true // plain text still renders
		return
	}
	m.md, m.mdWidth, m.mdFailed = r, width, false
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
