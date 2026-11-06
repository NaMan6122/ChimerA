// Compare mode: one task, N providers, one report.
//
// `-models a,b` runs the resolved task once per model, sequentially (vendors
// are quota-sensitive, so there is deliberately no fan-out), each with a fresh
// session id derived from the base session as "<session>-<model>", so no model
// inherits another's upstream conversation. Each run is a full single run —
// same client, runner, recorder and per-round stderr accounting — and after
// the last one a combined report goes to stdout: a comparison table in the
// order the models were given, every final answer verbatim, and a qualitative
// agreement note.
//
// A provider failure is one outcome among N: an error row in the table and an
// empty answer section, never a lost report. Only when every model fails does
// compare mode exit non-zero.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chimera/chimera/internal/spin"
)

const (
	// gistMaxRunes keeps a table row's gist on one line.
	gistMaxRunes = 72
	// reasonMaxRunes bounds a failure reason: context, not a log dump.
	reasonMaxRunes = 160
	// minRedactTokenRunes: shorter "tokens" are not masked out of failure
	// reasons — the mask would do more damage than the leak.
	minRedactTokenRunes = 8
	// taskMaxRunes bounds the echoed task line in the report header.
	taskMaxRunes = 160
)

// compareRun carries everything one compare invocation needs. It mirrors the
// single-run flags in main.go; task is already resolved there (preset scaffold
// and positional refinement included), so compare composes with -preset/-task
// the same way a single model does: the task stays the same, N runs happen.
type compareRun struct {
	baseURL     string
	token       string
	models      []string
	session     string // base session id; each run derives "<session>-<model>"
	root        string
	maxRounds   int
	timeout     time.Duration
	sessionsDir string
	tags        []string
	noSave      bool
	jsonOut     string
	task        string
}

// modelOutcome is one model's slice of the report.
type modelOutcome struct {
	Model   string
	Session string
	Res     spin.Result
	Wall    time.Duration
	Err     error // nil when the run completed
}

// runCompare executes the task once per model, in order, and prints the report.
// It returns the process exit code: 0 while any model produced an answer, 1
// when every model failed.
func runCompare(cfg compareRun) int {
	if cfg.session == "" {
		cfg.session = fmt.Sprintf("spin-%d", time.Now().UnixNano())
	}
	if cfg.jsonOut != "" {
		fmt.Fprintln(os.Stderr, "[compare] -json writes a single-run report; it is not part of compare mode")
	}

	outcomes := make([]modelOutcome, 0, len(cfg.models))
	for i, model := range cfg.models {
		fmt.Fprintf(os.Stderr, "[compare] %d/%d %s\n", i+1, len(cfg.models), model)
		outcomes = append(outcomes, runOneModel(cfg, model))
	}

	fmt.Print(renderCompareReport(cfg.task, cfg.token, outcomes))
	for _, o := range outcomes {
		if o.Err == nil {
			return 0
		}
	}
	return 1
}

// runOneModel is the single-run flow from main.go, parameterised: client ->
// CheckModel -> Refresh -> recorder -> runner, with the same per-round stderr
// accounting. Any failure becomes the outcome's Err, never a process exit, so
// one dead provider cannot take the rest of the report down with it.
func runOneModel(cfg compareRun, model string) modelOutcome {
	started := time.Now()
	session := cfg.session + "-" + model
	o := modelOutcome{Model: model, Session: session}

	client := spin.NewClient(cfg.baseURL, cfg.token, model, session)
	client.HTTP.Timeout = cfg.timeout
	if err := client.CheckModel(); err != nil {
		o.Err, o.Wall = err, time.Since(started)
		fmt.Fprintf(os.Stderr, "[compare] %s failed: %s\n", model, redactReason(err, cfg.token))
		return o
	}
	client.Refresh()

	// The transcript is a side-channel, as in a single run; -no-save and a
	// failing recorder disable it without touching the run.
	var rec *spin.Recorder
	if !cfg.noSave {
		if r, rerr := spin.NewRecorder(cfg.sessionsDir); rerr == nil {
			r.Start(session, model, cfg.baseURL, cfg.root, compareTags(cfg.tags))
			rec = r
			defer rec.Close()
			fmt.Fprintf(os.Stderr, "[spin] transcript %s\n", r.Path())
		} else {
			fmt.Fprintf(os.Stderr, "[spin] transcript disabled: %v\n", rerr)
		}
	}

	runner := spin.NewRunner(client, &spin.Executor{Root: cfg.root}, cfg.maxRounds)
	runner.OnEvent = func(ev spin.Event) {
		if rec != nil {
			rec.Observe(ev)
		}
		if ev.Kind == spin.EventNudge {
			fmt.Fprintln(os.Stderr, "[spin] no tool calls on a repo question — asking for evidence")
		}
	}

	res, err := runner.Run(cfg.task)
	o.Wall = time.Since(started)
	if err != nil {
		if rec != nil {
			rec.Note("error", err.Error())
		}
		o.Err = err
		fmt.Fprintf(os.Stderr, "[compare] %s failed: %s\n", model, redactReason(err, cfg.token))
		return o
	}
	o.Res = res
	report(started, session, model, cfg.task, res)
	return o
}

// compareTags returns the user's tags with "compare" appended (once), so every
// transcript of one compare invocation is findable via --tag compare.
func compareTags(tags []string) []string {
	for _, t := range tags {
		if t == "compare" {
			return tags
		}
	}
	out := make([]string, 0, len(tags)+1)
	out = append(out, tags...)
	return append(out, "compare")
}

// parseModelList splits the -models flag: comma-separated, trimmed, empty
// entries dropped, duplicates collapsed to the first occurrence. An empty list
// is a flag error, and a missing -models never reaches this (main keeps the
// single-run path for that).
func parseModelList(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		m := strings.TrimSpace(part)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("-models needs at least one model id, e.g. -models chimera-gemini,chimera-grok")
	}
	return out, nil
}

// renderCompareReport renders the whole stdout report. Deliberately not a
// leaderboard: the table orders the models as they were given, and the
// agreement note is phrased qualitatively and says so itself.
func renderCompareReport(task, token string, outcomes []modelOutcome) string {
	bar := strings.Repeat("=", 64)
	var b strings.Builder
	b.WriteString(bar + "\n")
	fmt.Fprintf(&b, "chimera-spin compare — one task, %d models\n", len(outcomes))
	fmt.Fprintf(&b, "task: %s\n", clip(oneLine(task), taskMaxRunes))
	b.WriteString(bar + "\n\n")

	tw := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "model\twall\trounds\ttool calls\tout chars\tgist")
	for _, o := range outcomes {
		gist := fmt.Sprintf("failed: %s", redactReason(o.Err, token))
		if o.Err == nil {
			gist = firstSentence(o.Res.Text, gistMaxRunes)
			if gist == "" {
				gist = "(empty answer)"
			}
		}
		fmt.Fprintf(tw, "%s\t%.1fs\t%d\t%d\t%d\t%s\n",
			o.Model, o.Wall.Seconds(), len(o.Res.Rounds), o.Res.Tools,
			utf8.RuneCountInString(o.Res.Text), gist)
	}
	tw.Flush()

	b.WriteString("\nFinal answers\n")
	for _, o := range outcomes {
		fmt.Fprintf(&b, "\n## %s\n\n", o.Model)
		if o.Err != nil {
			fmt.Fprintf(&b, "(no answer — failed: %s)\n", redactReason(o.Err, token))
			continue
		}
		b.WriteString(o.Res.Text + "\n")
	}

	b.WriteString("\n" + agreementNote(outcomes))
	return b.String()
}

// agreementNote compares the successful answers pairwise by token overlap
// (stopword-stripped, lowercased) and phrases the result qualitatively. It is
// a reading aid, not a ranking; the note says so in its own last line.
func agreementNote(outcomes []modelOutcome) string {
	var answered []modelOutcome
	for _, o := range outcomes {
		if o.Err == nil && strings.TrimSpace(o.Res.Text) != "" {
			answered = append(answered, o)
		}
	}
	var b strings.Builder
	b.WriteString("Agreement note\n")
	if len(answered) < 2 {
		b.WriteString("only one usable answer at most — nothing to compare.\n")
		b.WriteString("This is a similarity reading, not a leaderboard score.\n")
		return b.String()
	}

	b.WriteString("pairwise token overlap (stopword-stripped, lowercased):\n")
	var pairs []float64
	for i := 0; i < len(answered); i++ {
		for j := i + 1; j < len(answered); j++ {
			ov := tokenOverlap(answered[i].Res.Text, answered[j].Res.Text)
			pairs = append(pairs, ov)
			fmt.Fprintf(&b, "  %s vs %s — %d%% shared vocabulary\n",
				answered[i].Model, answered[j].Model, pct(ov))
		}
	}

	avg := 0.0
	for _, p := range pairs {
		avg += p
	}
	avg /= float64(len(pairs))

	b.WriteString("verdict: ")
	switch {
	case avg >= 0.55:
		b.WriteString("models largely agree.\n")
	case avg >= 0.20:
		b.WriteString("models overlap in part — skim both before trusting either.\n")
	default:
		b.WriteString("models diverge — read both.\n")
	}
	b.WriteString("This is a similarity reading, not a leaderboard score: shared vocabulary says nothing about which model is better.\n")
	return b.String()
}

// stopwords is the small closed-class list stripped before comparing tokens.
// It is deliberately tiny: content words carry the agreement signal, and every
// one- and two-letter fragment is dropped by tokenize regardless.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"if": true, "then": true, "else": true, "when": true, "while": true,
	"at": true, "by": true, "for": true, "with": true, "about": true,
	"into": true, "to": true, "from": true, "in": true, "on": true, "of": true,
	"is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "being": true, "it": true, "its": true, "this": true,
	"that": true, "these": true, "those": true, "as": true, "so": true,
	"not": true, "no": true, "nor": true, "do": true, "does": true,
	"did": true, "have": true, "has": true, "had": true, "will": true,
	"would": true, "can": true, "could": true, "should": true, "may": true,
	"might": true, "must": true, "than": true, "too": true, "very": true,
	"just": true, "also": true, "there": true, "here": true, "what": true,
	"which": true, "who": true, "whom": true, "how": true, "all": true,
	"any": true, "each": true, "both": true,
}

// tokenize lowercases an answer and splits it on anything that is not a letter
// or digit, dropping stopwords and one-letter fragments.
func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		w := strings.ToLower(f)
		if len([]rune(w)) < 2 || stopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// tokenOverlap is the Jaccard overlap of two answers' token sets.
func tokenOverlap(a, b string) float64 {
	ta, tb := tokenize(a), tokenize(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1
	}
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func pct(f float64) int { return int(f*100 + 0.5) }

// firstSentence is the gist: the final answer's opening, trimmed to one line.
// A leading markdown heading is a title, not the answer's first sentence, so
// it is skipped when a body line follows; whitespace runs collapse; the result
// is clipped to limit runes. Sentence detection is a heuristic: the first
// '.', '!' or '?' followed by a space or the end of the text.
func firstSentence(s string, limit int) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "#") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			if rest := strings.TrimSpace(s[i+1:]); rest != "" {
				s = rest
			}
		}
	}
	s = oneLine(s)
	cut := len(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '.' || c == '!' || c == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			cut = i + 1
			break
		}
	}
	return clip(s[:cut], limit)
}

// redactReason flattens an error into one safe table line: the bearer token
// never appears (error text carries URLs and upstream bodies, and both are
// untrusted surfaces), newlines collapse, and the line is clipped. Tokens
// under minRedactTokenRunes are left alone: masking a one- or two-character
// token would shred the reason text into ellipses while protecting almost
// nothing a real bearer token would.
func redactReason(err error, token string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if r := []rune(token); len(r) >= minRedactTokenRunes {
		s = strings.ReplaceAll(s, token, "…")
	}
	return clip(oneLine(s), reasonMaxRunes)
}

// oneLine collapses every whitespace run to a single space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip caps a string at max runes, appending an ellipsis when it cuts.
func clip(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
