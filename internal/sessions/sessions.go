// Package sessions turns Chimera's observe-only JSONL transcripts into
// readable, preservable artifacts: an index of past runs and a Markdown
// runbook renderer. Nothing here feeds a request — the transcripts stay
// write-only from the loop's perspective; this package is a reader.
//
// Philosophy (user-facing): a session CANNOT be resumed against a provider —
// vendor-side conversation state expires and is out of our control. What CAN
// be preserved is the run itself: what was asked, what the agent did, what
// it produced, and enough metadata to re-run it by hand. Hence "runbook".
package sessions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

// Summary is the index line for one recorded session.
type Summary struct {
	ID        string
	Path      string
	Started   time.Time
	Model     string
	BaseURL   string
	Root      string
	Task      string // first user record — the task the run was given
	Final     string // the last final record — the answer that came out
	Rounds    int
	ToolCalls int
	Errors    int
	Duration  time.Duration // last.ts - first.ts
	Bytes     int64
}

// LoadSummary reads one transcript and derives its summary.
func LoadSummary(path string) (*Summary, error) {
	recs, err := load(path)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(path)
	s := &Summary{Path: path}
	if fi != nil {
		s.Bytes = fi.Size()
	}
	var first, last time.Time
	for i := range recs {
		r := recs[i]
		ts, _ := time.Parse(time.RFC3339Nano, r.TS)
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
		switch r.Kind {
		case "session":
			s.ID = r.Session
			s.Model = r.Model
			s.BaseURL = r.BaseURL
			s.Root = r.Root
		case "user":
			if s.Task == "" {
				s.Task = oneLine(r.Text)
			}
		case "final":
			s.Final = oneLine(r.Text)
			s.Rounds = r.Round
		case "tool":
			s.ToolCalls++
			if r.Round > s.Rounds {
				s.Rounds = r.Round
			}
		case "error":
			s.Errors++
		}
	}
	s.Started = first
	if !first.IsZero() && last.After(first) {
		s.Duration = last.Sub(first)
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return s, nil
}

// List summarizes every transcript in dir, newest first.
func List(dir string) ([]*Summary, error) {
	paths, err := spin.ListSessions(dir)
	if err != nil {
		return nil, err
	}
	out := make([]*Summary, 0, len(paths))
	for _, p := range paths {
		s, err := LoadSummary(p)
		if err != nil {
			continue // unreadable file: skip, don't break the listing
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// RenderMarkdown renders one transcript as a readable runbook: metadata
// front matter, the task, a step per round (assistant text, tool calls with
// arguments and results), and the final answer.
func RenderMarkdown(path string) (string, error) {
	recs, err := load(path)
	if err != nil {
		return "", err
	}
	sum, err := LoadSummary(path)
	if err != nil {
		return "", err
	}
	var b strings.Builder

	b.WriteString("# Session runbook — " + sum.ID + "\n\n")
	b.WriteString("> Preserved for reading, not replay: the provider side of this conversation\n")
	b.WriteString("> has likely expired, and the harness never stored anything to re-send.\n")
	b.WriteString("> Re-run the task below by hand, or reuse the final answer.\n\n")
	b.WriteString("| | |\n|---|---|\n")
	b.WriteString("| Started | " + sum.Started.Format(time.RFC3339) + " |\n")
	if sum.Model != "" {
		b.WriteString("| Model | `" + sum.Model + "` |\n")
	}
	if sum.BaseURL != "" {
		b.WriteString("| Gateway | `" + sum.BaseURL + "` |\n")
	}
	if sum.Root != "" {
		b.WriteString("| Workspace | `" + sum.Root + "` |\n")
	}
	b.WriteString(fmt.Sprintf("| Rounds | %d |\n| Tool calls | %d |\n", sum.Rounds, sum.ToolCalls))
	if sum.Errors > 0 {
		b.WriteString(fmt.Sprintf("| Errors | %d |\n", sum.Errors))
	}
	if sum.Duration > 0 {
		b.WriteString("| Duration | " + sum.Duration.Round(time.Millisecond).String() + " |\n")
	}
	b.WriteString("\n")

	// Body: walk the records in order, grouping per round.
	if sum.Task != "" {
		b.WriteString("## Task\n\n" + sum.Task + "\n\n")
	}
	b.WriteString("## Run\n\n")
	currentRound := -1
	for _, r := range recs {
		switch r.Kind {
		case "round":
			if r.Round != currentRound {
				currentRound = r.Round
				lat := ""
				if r.LatencyS > 0 {
					lat = fmt.Sprintf(" · %.1fs", r.LatencyS)
				}
				b.WriteString(fmt.Sprintf("### Round %d%s\n\n", r.Round, lat))
			}
			if strings.TrimSpace(r.Text) != "" {
				b.WriteString(strings.TrimSpace(r.Text) + "\n\n")
			}
		case "tool":
			b.WriteString(fmt.Sprintf("**Tool → `%s`**\n\n", r.Name))
			if strings.TrimSpace(r.Arguments) != "" && r.Arguments != "{}" {
				b.WriteString("```json\n" + strings.TrimSpace(r.Arguments) + "\n```\n\n")
			}
		case "result":
			b.WriteString(fmt.Sprintf("**Result ← `%s`**\n\n", r.Name))
			if strings.TrimSpace(r.Result) != "" {
				b.WriteString("```\n" + trimResult(r.Result) + "\n```\n\n")
			}
		case "final":
			// The final answer gets its own section; skip inside the run.
		case "note":
			if strings.TrimSpace(r.Text) != "" {
				b.WriteString("> " + strings.TrimSpace(r.Text) + "\n\n")
			}
		case "error":
			b.WriteString("**ERROR:** `" + strings.TrimSpace(r.Text) + "`\n\n")
		}
	}
	if sum.Final != "" {
		b.WriteString("## Final answer\n\n" + sum.Final + "\n")
	}
	return b.String(), nil
}

// trimResult keeps tool results readable: long outputs collapse around the
// head, with the elision marked.
func trimResult(s string) string {
	s = strings.TrimRight(s, "\n")
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (truncated; full output in the JSONL transcript)"
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = s[:200] + " …"
	}
	return s
}

func load(path string) ([]spin.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []spin.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var r spin.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // torn last line must not lose the rest
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
