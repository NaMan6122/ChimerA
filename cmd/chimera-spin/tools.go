package main

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

// maxToolOutput bounds what one tool may return. The web model pays for every
// character twice (upload + prefill), and a single large file would otherwise
// dominate the next turn's prompt.
const maxToolOutput = 8000

// maxRoundOutput bounds what one turn may return in total. Measured: eight
// parallel 12k reads produced a 65k-char follow-up prompt, which is the same
// accumulation the harness exists to prevent, arriving through the tool channel.
const maxRoundOutput = 16000

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func prop(typ, desc string) map[string]any {
	p := map[string]any{"type": typ}
	if desc != "" {
		p["description"] = desc
	}
	return p
}

func mkTool(name, desc string, props map[string]any, required ...string) toolDef {
	var t toolDef
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = desc
	t.Function.Parameters = map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		t.Function.Parameters["required"] = required
	}
	return t
}

// toolDefs is deliberately six tools, not twenty: past a handful of schemas the
// prompt-injected calling convention degrades, and every schema is prompt bytes.
func toolDefs() []toolDef {
	return []toolDef{
		mkTool("read_file", "Read a text file. Lines are numbered. Optionally limit to a 1-indexed range.",
			map[string]any{"path": prop("string", "path relative to the repository root"),
				"startLine": prop("integer", "first line, 1-indexed"), "endLine": prop("integer", "last line, inclusive")},
			"path"),
		mkTool("list_dir", "List a directory: subdirectories first, then files.",
			map[string]any{"path": prop("string", "directory relative to the root; empty means root")}),
		mkTool("grep", "Search file contents with a regular expression. Returns path:line:text.",
			map[string]any{"pattern": prop("string", "Go regular expression"),
				"path":    prop("string", "directory to search, default root"),
				"include": prop("string", "optional filename glob, e.g. *.go")},
			"pattern"),
		mkTool("write_file", "Create or overwrite a file with the given content.",
			map[string]any{"path": prop("string", "path relative to the root"),
				"content": prop("string", "full file content")},
			"path", "content"),
		mkTool("edit_file", "Replace exactly one occurrence of search with replace in a file.",
			map[string]any{"path": prop("string", "path relative to the root"),
				"search":  prop("string", "exact text to find; must occur exactly once"),
				"replace": prop("string", "replacement text")},
			"path", "search", "replace"),
		mkTool("run_command", "Run a shell command from the repository root and return its combined output (60s limit).",
			map[string]any{"command": prop("string", "command line to execute with bash -lc")},
			"command"),
	}
}

// exec dispatches one tool call. Every failure is returned to the model as text
// rather than aborting the spin: a broken tool call is information it can use.
func (s *spin) exec(tc toolCall) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "error: arguments are not valid JSON: " + err.Error()
	}
	switch tc.Function.Name {
	case "read_file":
		return s.readFile(args)
	case "list_dir":
		return s.listDir(args)
	case "grep":
		return s.grep(args)
	case "write_file":
		return s.writeFile(args)
	case "edit_file":
		return s.editFile(args)
	case "run_command":
		return s.runCommand(args)
	default:
		return fmt.Sprintf("error: unknown tool %q (available: read_file, list_dir, grep, write_file, edit_file, run_command)", tc.Function.Name)
	}
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

func (s *spin) readFile(args map[string]any) string {
	abs, err := resolve(s.cfg.root, str(args, "path"))
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
		if b.Len()+len(line) > maxToolOutput {
			truncatedAt = i
			break
		}
		b.WriteString(line)
	}
	if truncatedAt > 0 {
		// Actionable, not just informative: on a 1,400-line file the model
		// otherwise stops and reports that it cannot see the rest.
		fmt.Fprintf(&b, "… [truncated at %d bytes; continue with startLine=%d]\n", maxToolOutput, truncatedAt)
	}
	return b.String()
}

func (s *spin) listDir(args map[string]any) string {
	abs, err := resolve(s.cfg.root, str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	var dirs, files []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name()+"/")
		} else {
			files = append(files, e.Name())
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

// skipDirs are not source: walking them wastes tokens and shell time.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "browser_data": true, "auth_data": true,
	"logs": true, ".anvaya": true, "vendor": true, "dist": true, "build": true,
}

func (s *spin) grep(args map[string]any) string {
	pattern := str(args, "pattern")
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "error: bad pattern: " + err.Error()
	}
	base, err := resolve(s.cfg.root, str(args, "path"))
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
		rel, _ := filepath.Rel(s.cfg.root, path)
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

func (s *spin) writeFile(args map[string]any) string {
	abs, err := resolve(s.cfg.root, str(args, "path"))
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

func (s *spin) editFile(args map[string]any) string {
	abs, err := resolve(s.cfg.root, str(args, "path"))
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
	n := strings.Count(body, search)
	if n != 1 {
		return fmt.Sprintf("error: search text occurs %d times; it must occur exactly once", n)
	}
	body = strings.Replace(body, search, replace, 1)
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("edited %s (1 replacement)", str(args, "path"))
}

func (s *spin) runCommand(args map[string]any) string {
	cmdline := str(args, "command")
	if cmdline == "" {
		return "error: command must not be empty"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdline)
	cmd.Dir = s.cfg.root
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

// resolve confines a tool path to the workspace. A model that can write anywhere
// on the machine is a liability, not a feature.
func resolve(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return abs, nil
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// clip truncates a tool result and says so, so the model knows it is not seeing
// everything rather than inventing the rest.
func clip(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n… [truncated at %d bytes]", maxToolOutput)
}
