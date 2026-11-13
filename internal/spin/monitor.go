package spin

// monitor — run a shell command in the background and read its output later,
// adapted from Anvaya-CLI's monitor tool (spec-185) to a synchronous tool
// loop: there is no event channel to wake the model mid-turn, so `read`
// polling replaces streaming. One tool, four actions:
//
//	start {command, description, timeout_s?, persistent?} → id + log path, immediately
//	read  {id}                        → recent output (tail) + status
//	stop  {id}                        → kill it, report final status
//	list                              → every monitor with its status
//
// The full log (stdout + stderr) is written to one file under a per-run temp
// directory, so nothing lands inside the workspace and nothing is lost by the
// tail. Monitors never block a round: start returns after spawn, not after
// exit.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// maxLiveMonitors caps concurrently running monitors, mirroring Anvaya's
	// MAX_ARMED_MONITORS: each holds a process and a file handle.
	maxLiveMonitors = 8
	// monitorReadBytes bounds one read action's output.
	monitorReadBytes = 8000
	// monitor timeout clamp, as in Anvaya: never shorter than 10s (no
	// instant-fail monitors), never longer than an hour (runaway processes).
	minMonitorTimeout = 10 * time.Second
	maxMonitorTimeout = time.Hour
	defaultMonitorTTL = 5 * time.Minute
)

// monitorState is one background process and its log.
type monitorState struct {
	id          int
	description string
	command     string
	path        string
	started     time.Time

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     bool
	outcome  string // "running", "exited", "timeout", "stopped"
	exitCode int
}

func (m *monitorState) status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.outcome {
	case "exited":
		return fmt.Sprintf("exited (code %d)", m.exitCode)
	case "timeout":
		return "timed out"
	case "stopped":
		return "stopped"
	default:
		return "running"
	}
}

func (m *monitorState) running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.outcome == "running"
}

func (m *monitorState) finish(outcome string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outcome == "running" {
		m.outcome, m.exitCode = outcome, code
	}
	if m.cancel != nil {
		m.cancel = nil
	}
}

// startMonitor arms one background command. It returns as soon as the child
// is spawned; the log file collects stdout+stderr for the read action.
func (e *Executor) startMonitor(args map[string]any) string {
	command := str(args, "command")
	if command == "" {
		return "error: monitor start needs a command"
	}
	description := str(args, "description")
	if description == "" {
		description = strings.TrimSpace(strings.Join(strings.Fields(command)[:min(4, len(strings.Fields(command)))], " "))
	}
	if len(description) > 80 {
		description = description[:80] + "…"
	}
	if reason := blockedBySafeMode(command); reason != "" {
		return "error: monitor start blocked in safe mode: " + reason
	}

	e.monMu.Lock()
	defer e.monMu.Unlock()
	live := 0
	for _, m := range e.monitors {
		if m.running() {
			live++
		}
	}
	if live >= maxLiveMonitors {
		return fmt.Sprintf("error: too many live monitors (%d max). Read or stop one first (action=list)", maxLiveMonitors)
	}
	if e.monDir == "" {
		dir, err := os.MkdirTemp("", "chimera-monitors-")
		if err != nil {
			return "error: cannot create monitor log directory: " + err.Error()
		}
		e.monDir = dir
	}

	ttl := defaultMonitorTTL
	if v := num(args, "timeout_s"); v > 0 {
		ttl = time.Duration(v) * time.Second
	}
	persistent := false
	if b, ok := args["persistent"].(bool); ok {
		persistent = b
	}
	if !persistent {
		if ttl < minMonitorTimeout {
			ttl = minMonitorTimeout
		}
		if ttl > maxMonitorTimeout {
			ttl = maxMonitorTimeout
		}
	}

	e.monSeq++
	m := &monitorState{
		id:          e.monSeq,
		description: description,
		command:     command,
		path:        filepath.Join(e.monDir, fmt.Sprintf("%d.log", e.monSeq)),
		started:     time.Now(),
		outcome:     "running",
	}
	f, err := os.Create(m.path)
	if err != nil {
		return "error: cannot open monitor log: " + err.Error()
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if persistent {
		f.WriteString(fmt.Sprintf("$ %s\n(persistent: runs until stopped)\n", command))
	} else {
		ctx, cancel = context.WithTimeout(ctx, ttl)
		f.WriteString(fmt.Sprintf("$ %s\n(timeout %s)\n", command, ttl))
	}
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = e.Root
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		if cancel != nil {
			cancel()
		}
		return "error: monitor failed to start: " + err.Error()
	}
	m.cancel = cancel
	e.monitors[m.id] = m

	go func() {
		defer f.Close()
		err := cmd.Wait()
		switch {
		case err == nil:
			m.finish("exited", 0)
		case ctx.Err() == context.DeadlineExceeded:
			m.finish("timeout", exitCodeOf(err))
		case ctx.Err() == context.Canceled:
			m.finish("stopped", exitCodeOf(err))
		default:
			m.finish("exited", exitCodeOf(err))
		}
	}()

	return fmt.Sprintf("monitor %d started (%s) — non-blocking; read with {\"action\":\"read\",\"id\":%d}; log: %s",
		m.id, description, m.id, m.path)
}

func exitCodeOf(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// readMonitor returns the tail of a monitor's log plus its status.
func (e *Executor) readMonitor(args map[string]any) string {
	m, errText := e.monitorByID(args)
	if errText != "" {
		return errText
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return "error: monitor log unreadable: " + err.Error()
	}
	out := string(data)
	trimmed := ""
	if len(out) > monitorReadBytes {
		out = out[len(out)-monitorReadBytes:]
		trimmed = fmt.Sprintf("… [%d earlier bytes in %s]\n", len(data)-monitorReadBytes, m.path)
	}
	fi, _ := os.Stat(m.path)
	total := int64(0)
	if fi != nil {
		total = fi.Size()
	}
	return fmt.Sprintf("monitor %d (%s) — %s — %d bytes total\n\n%s%s",
		m.id, m.description, m.status(), total, trimmed, strings.TrimRight(out, "\n"))
}

// stopMonitor kills a monitor and reports its final state.
func (e *Executor) stopMonitor(args map[string]any) string {
	m, errText := e.monitorByID(args)
	if errText != "" {
		return errText
	}
	was := m.status()
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel() // the waiter goroutine records "stopped"
		// Give the waiter a moment so the reply reflects the kill.
		deadline := time.Now().Add(2 * time.Second)
		for m.running() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return fmt.Sprintf("monitor %d (%s) — was %s, now %s", m.id, m.description, was, m.status())
}

// listMonitors renders every monitor this run has armed.
func (e *Executor) listMonitors() string {
	e.monMu.Lock()
	defer e.monMu.Unlock()
	if len(e.monitors) == 0 {
		return "(no monitors armed this run)"
	}
	var b strings.Builder
	for i := 1; i <= e.monSeq; i++ {
		m, ok := e.monitors[i]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "monitor %d — %s — %s — %s\n", m.id, m.description, m.status(), m.path)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *Executor) monitorByID(args map[string]any) (*monitorState, string) {
	id := num(args, "id")
	e.monMu.Lock()
	defer e.monMu.Unlock()
	m, ok := e.monitors[id]
	if !ok {
		return nil, fmt.Sprintf("error: no monitor %d (action=list shows armed monitors)", id)
	}
	return m, ""
}

// Shutdown stops every live monitor. Called when a run ends so persistent
// monitors do not outlive their harness.
func (e *Executor) Shutdown() {
	e.monMu.Lock()
	ids := make([]int, 0, len(e.monitors))
	for id, m := range e.monitors {
		if m.running() {
			ids = append(ids, id)
		}
	}
	e.monMu.Unlock()
	for _, id := range ids {
		e.stopMonitor(map[string]any{"id": float64(id)})
	}
}

// ── safe mode ─────────────────────────────────────────────────

// unsafePatterns is an advisory denylist for `-safe` mode: patterns whose
// execution is destructive far more often than useful from an agent. It is
// hardening, not a sandbox — a determined model can still do damage through
// any general-purpose command, which is why safe mode is opt-in and
// documented as advisory (see SECURITY.md).
var unsafePatterns = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(^|\s)sudo\s|(^|\s)su\s+-`), "privilege escalation (sudo/su)"},
	{regexp.MustCompile(`(^|\s)(shutdown|reboot|halt|poweroff)\b`), "system power control"},
	{regexp.MustCompile(`\bmkfs\b|\bdd\b[^|;&]*of=/dev/`), "filesystem/device overwrite"},
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\s+/(?:\s|$)`), "recursive delete at filesystem root"},
	{regexp.MustCompile(`(curl|wget)[^|;&]*\|\s*(ba|z)?sh\b`), "remote code execution (pipe to shell)"},
	{regexp.MustCompile(`:\(\)\s*\{.*\}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(`\bchmod\b[^;&|]*\s+777\s+/(?:\s|$)`), "world-writable filesystem root"},
}

// blockedBySafeMode returns the reason a command is denied, or "" to allow.
func blockedBySafeMode(cmdline string) string {
	for _, p := range unsafePatterns {
		if p.re.MatchString(cmdline) {
			return p.why
		}
	}
	return ""
}
