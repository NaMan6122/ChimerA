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
	default:
		return fmt.Sprintf("error: unknown tool %q (available: read_file, list_dir, grep, write_file, edit_file, run_command)", tc.Function.Name)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	rel, err := filepath.Rel(e.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return abs, nil
}

func clip(s string) string {
	if len(s) <= MaxToolOutput {
		return s
	}
	return s[:MaxToolOutput] + fmt.Sprintf("\n… [truncated at %d bytes]", MaxToolOutput)
}
