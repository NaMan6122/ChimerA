package spin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxToolOutput bounds one tool's result. The model pays for every character
	// twice (upload + prefill).
	MaxToolOutput = 8000
	// MaxRoundOutput bounds a whole turn's results. Measured: eight parallel 12k
	// reads produced a 65k-char follow-up prompt — the same accumulation the
	// design avoids, arriving through the tool channel.
	MaxRoundOutput = 16000
)

// Executor runs tool calls confined to one workspace root.
type Executor struct {
	Root string

	// CommandTimeout bounds run_command, git and monitor-adjacent work.
	// Zero selects the default (5m): cold test suites and Docker builds
	// routinely exceed the 60s the first version hard-coded.
	CommandTimeout time.Duration

	// Safe applies the advisory denylist (see monitor.go / SECURITY.md) to
	// run_command and monitor start. Hardening, not a sandbox.
	Safe bool

	// Monitor registry state (see monitor.go).
	monMu    sync.Mutex
	monSeq   int
	monDir   string
	monitors map[int]*monitorState
}

// Exec dispatches one call. Failures are returned as text, not errors: a broken
// call is information the model can act on.
func (e *Executor) Exec(tc ToolCall) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "error: arguments are not valid JSON: " + err.Error()
	}
	switch tc.Function.Name {
	case "read_file":
		return e.readFile(args)
	case "read_pdf":
		return e.readPDF(args)
	case "list_dir":
		return e.listDir(args)
	case "grep":
		return e.grep(args)
	case "write_file":
		return e.writeFile(args)
	case "edit_file":
		return e.editFile(args)
	case "run_command":
		return e.runCommand(args)
	case "git":
		return e.git(args)
	case "monitor":
		return e.monitorTool(args)
	default:
		return fmt.Sprintf("error: unknown tool %q (available: read_file, read_pdf, list_dir, grep, write_file, edit_file, run_command, git, monitor)", tc.Function.Name)
	}
}

// commandTimeout returns the configured command timeout, defaulting to five
// minutes; cold test suites and Docker builds routinely exceed the 60s the
// first version hard-coded.
func (e *Executor) commandTimeout() time.Duration {
	if e.CommandTimeout <= 0 {
		return 5 * time.Minute
	}
	if e.CommandTimeout < 5*time.Second {
		return 5 * time.Second
	}
	return e.CommandTimeout
}

// Describe renders a call for the UI: the tool and its most useful argument.
func Describe(tc ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
	for _, k := range []string{"path", "pattern", "command"} {
		if v, ok := args[k].(string); ok && v != "" {
			return tc.Function.Name + " " + v
		}
	}
	return tc.Function.Name
}

func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func num(args map[string]any, key string) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return 0
}

func (e *Executor) readFile(args map[string]any) string {
	abs, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	lines := strings.Split(string(raw), "\n")
	start, end := num(args, "startLine"), num(args, "endLine")
	if start < 1 {
		start = 1
	}
	if end < start || end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	truncatedAt := 0
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if b.Len()+len(line) > MaxToolOutput {
			truncatedAt = i
			break
		}
		b.WriteString(line)
	}
	if truncatedAt > 0 {
		fmt.Fprintf(&b, "… [truncated at %d bytes; continue with startLine=%d]\n", MaxToolOutput, truncatedAt)
	}
	return b.String()
}

func (e *Executor) listDir(args map[string]any) string {
	abs, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	var dirs, files []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name()+"/")
		} else {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	out := append(dirs, files...)
	if len(out) == 0 {
		return "(empty directory)"
	}
	return clip(strings.Join(out, "\n"))
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "browser_data": true, "auth_data": true,
	"logs": true, ".anvaya": true, "vendor": true, "dist": true, "build": true,
}

func (e *Executor) grep(args map[string]any) string {
	re, err := regexp.Compile(str(args, "pattern"))
	if err != nil {
		return "error: bad pattern: " + err.Error()
	}
	base, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	include := str(args, "include")
	var b strings.Builder
	matches := 0
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if include != "" {
			if ok, _ := filepath.Match(include, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(e.Root, path)
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, i+1, strings.TrimSpace(line))
				matches++
				if matches >= 200 {
					return fmt.Errorf("cap")
				}
			}
		}
		return nil
	})
	if matches == 0 {
		return "no matches"
	}
	return clip(b.String())
}

func (e *Executor) writeFile(args map[string]any) string {
	abs, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "error: " + err.Error()
	}
	if err := os.WriteFile(abs, []byte(str(args, "content")), 0o644); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("wrote %s (%d bytes)", str(args, "path"), len(str(args, "content")))
}

func (e *Executor) editFile(args map[string]any) string {
	abs, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	search, replace := str(args, "search"), str(args, "replace")
	if search == "" {
		return "error: search must not be empty"
	}
	body := string(raw)
	if n := strings.Count(body, search); n != 1 {
		return fmt.Sprintf("error: search text occurs %d times; it must occur exactly once", n)
	}
	if err := os.WriteFile(abs, []byte(strings.Replace(body, search, replace, 1)), 0o644); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("edited %s (1 replacement)", str(args, "path"))
}

func (e *Executor) runCommand(args map[string]any) string {
	cmdline := str(args, "command")
	if cmdline == "" {
		return "error: command must not be empty"
	}
	if reason := blockedBySafeMode(cmdline); reason != "" {
		return "error: blocked in safe mode: " + reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.commandTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdline)
	cmd.Dir = e.Root
	out, err := cmd.CombinedOutput()
	res := clip(string(out))
	if err != nil {
		return fmt.Sprintf("%s\n[exit: %v]", res, err)
	}
	if strings.TrimSpace(res) == "" {
		return "(no output)"
	}
	return res
}

// gitReadOnly lists the subcommands the git tool will run. Writes go through
// write_file/edit_file/run_command where the model already has reviewable,
// budgeted paths; this tool exists so inspection never needs shell quoting.
var gitReadOnly = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
	"rev-parse": true, "describe": true, "remote": true, "tag": true,
	"blame": true, "stash": true, "shortlog": true,
}

// git runs one read-only git subcommand — no shell, so argument text can
// never become a second command.
func (e *Executor) git(args map[string]any) string {
	sub := str(args, "subcommand")
	if sub == "" {
		names := make([]string, 0, len(gitReadOnly))
		for n := range gitReadOnly {
			names = append(names, n)
		}
		sort.Strings(names)
		return "error: git needs a subcommand (read-only ones: " + strings.Join(names, ", ") + ")"
	}
	if !gitReadOnly[sub] {
		return fmt.Sprintf("error: git %q is not a read-only subcommand; use run_command for writes", sub)
	}
	var argv []string
	if extra := strings.TrimSpace(str(args, "args")); extra != "" {
		argv = strings.Fields(extra) // no shell: spaces split, nothing executes
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.commandTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{sub}, argv...)...)
	cmd.Dir = e.Root
	out, err := cmd.CombinedOutput()
	res := clip(string(out))
	if err != nil {
		return fmt.Sprintf("%s\n[exit: %v]", res, err)
	}
	if strings.TrimSpace(res) == "" {
		return "(no output)"
	}
	return res
}

// monitorTool dispatches the background-command tool; semantics in monitor.go.
func (e *Executor) monitorTool(args map[string]any) string {
	if e.monitors == nil {
		e.monitors = map[int]*monitorState{}
	}
	switch str(args, "action") {
	case "start":
		return e.startMonitor(args)
	case "read":
		return e.readMonitor(args)
	case "stop":
		return e.stopMonitor(args)
	case "list", "":
		return e.listMonitors()
	default:
		return "error: monitor action must be start, read, stop or list"
	}
}

// Resolve confines a path to the workspace. A model that can write anywhere is a
// liability, not a feature.
func (e *Executor) Resolve(p string) (string, error) {
	if p == "" {
		return e.Root, nil
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(e.Root, p)
	}
	abs = filepath.Clean(abs)
	// Symlink hardening (spec-021 acceptance): lexical confinement is escaped
	// by a symlink planted inside the root pointing out of it. Evaluate the
	// real paths — the target file may not exist yet (writes), so fall back
	// to the deepest existing ancestor — and re-check containment against
	// both the resolved root and the resolved path.
	root := e.Root
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if real, err := deepestExisting(abs); err == nil {
		abs = real
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return abs, nil
}

// deepestExisting resolves symlinks along p's ancestors as far as they exist,
// keeping the final element (the not-yet-existing file name) intact.
func deepestExisting(p string) (string, error) {
	dir := filepath.Dir(p)
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, filepath.Base(p)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p, os.ErrNotExist
		}
		dir = parent
	}
}

func clip(s string) string {
	if len(s) <= MaxToolOutput {
		return s
	}
	return s[:MaxToolOutput] + fmt.Sprintf("\n… [truncated at %d bytes]", MaxToolOutput)
}
