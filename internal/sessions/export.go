package sessions

// Bulk export — one command turns the whole archive into a browsable folder:
// a readable Markdown (or HTML) runbook per session plus an index. Files are
// overwritten on re-export, so a vault is idempently refreshable, and the
// index always matches what is on disk.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExportAll renders every transcript in dir into outDir — one file per
// session plus an index — and returns the written paths (index last).
// html selects self-contained HTML pages over Markdown. Existing files with
// the same names are overwritten.
func ExportAll(dir, outDir string, html bool) ([]string, error) {
	sums, err := List(dir)
	if err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no sessions recorded in %s", dir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	ext := ".md"
	render := RenderMarkdown
	if html {
		ext = ".html"
		render = RenderHTML
	}

	var written []string
	for _, s := range sums {
		doc, err := render(filepath.Join(dir, s.ID+".jsonl"))
		if err != nil {
			return written, fmt.Errorf("%s: %w", s.ID, err)
		}
		name := sanitizeID(s.ID) + ext
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(doc), 0o644); err != nil {
			return written, err
		}
		written = append(written, name)
	}

	idx, err := renderIndex(sums, ext)
	if err != nil {
		return written, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.md"), []byte(idx), 0o644); err != nil {
		return written, err
	}
	return append(written, "index.md"), nil
}

// renderIndex builds the vault's table of contents: one row per session,
// newest first, linking to its runbook.
func renderIndex(sums []*Summary, ext string) (string, error) {
	var b strings.Builder
	b.WriteString("# Chimera sessions — index\n\n")
	fmt.Fprintf(&b, "%d preserved sessions, exported %s.\n\n", len(sums), time.Now().Format("2006-01-02 15:04"))
	b.WriteString("| Started | Session | Rounds | Tools | Task | Runbook |\n|---|---|---|---|---|---|\n")
	for _, s := range sums {
		task := oneLine(s.Task)
		task = strings.ReplaceAll(task, "|", "\\|")
		fmt.Fprintf(&b, "| %s | `%s` | %d | %d | %s | [%s](%s) |\n",
			s.Started.Format("2006-01-02 15:04"), s.ID,
			s.Rounds, s.ToolCalls, task,
			sanitizeID(s.ID)+ext, sanitizeID(s.ID)+ext)
	}
	return b.String(), nil
}

// sanitizeID keeps a session id usable as a filename (ids from the gateway
// digest form are already safe; hand-named ones may not be).
func sanitizeID(id string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, id)
	if s == "" {
		return "session"
	}
	return s
}
