package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chimera/chimera/internal/spin"
)

// fakeGateway is a minimal OpenAI-compatible gateway for the compare tests:
// GET /v1/models lists the given ids, POST /v1/chat/completions answers per
// model (optionally one list_dir tool round first, or failing with a 500),
// POST /v1/refresh acknowledges. It records which session id and task each
// model actually saw, so the tests can pin session freshness and prompt
// equality rather than trusting the report.
type fakeGateway struct {
	srv         *httptest.Server
	mu          sync.Mutex
	toolReplied map[string]bool            // session -> a tool-call round was served
	sessions    map[string]map[string]bool // model -> session ids seen
	firstUser   map[string]string          // model -> first user message (the task)
}

func newFakeGateway(t *testing.T, models []string, answers map[string]string, toolCall, fail map[string]bool) *fakeGateway {
	t.Helper()
	f := &fakeGateway{
		toolReplied: map[string]bool{},
		sessions:    map[string]map[string]bool{},
		firstUser:   map[string]string{},
	}
	listed := make([]map[string]string, 0, len(models))
	for _, m := range models {
		listed = append(listed, map[string]string{"id": m, "object": "model"})
	}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			writeJSON(w, map[string]any{"object": "list", "data": listed})

		case r.Method == http.MethodPost && r.URL.Path == "/v1/refresh":
			writeJSON(w, map[string]any{"ok": true})

		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var req struct {
				Model    string         `json:"model"`
				Messages []spin.Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request body", http.StatusBadRequest)
				return
			}
			session := r.Header.Get("X-Session-Id")
			f.mu.Lock()
			if f.sessions[req.Model] == nil {
				f.sessions[req.Model] = map[string]bool{}
			}
			f.sessions[req.Model][session] = true
			for _, m := range req.Messages {
				if m.Role == "user" {
					f.firstUser[req.Model] = m.Content
					break
				}
			}
			f.mu.Unlock()

			if fail[req.Model] {
				http.Error(w, "upstream quota exhausted", http.StatusInternalServerError)
				return
			}
			if !toolCall[req.Model] || hasToolResult(req.Messages) {
				writeJSON(w, chatCompletion(chatMsg{Role: "assistant", Content: answers[req.Model]}))
				return
			}
			// One round of tool use; the final answer comes on the next turn.
			f.mu.Lock()
			replied := f.toolReplied[session]
			f.toolReplied[session] = true
			f.mu.Unlock()
			if replied {
				writeJSON(w, chatCompletion(chatMsg{Role: "assistant", Content: answers[req.Model]}))
				return
			}
			tc := spin.ToolCall{ID: "call_1", Type: "function"}
			tc.Function.Name = "list_dir"
			tc.Function.Arguments = `{"path":""}`
			writeJSON(w, chatCompletion(chatMsg{Role: "assistant", ToolCalls: []spin.ToolCall{tc}}))

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type chatMsg struct {
	Role      string          `json:"role"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []spin.ToolCall `json:"tool_calls,omitempty"`
}

func chatCompletion(msg chatMsg) map[string]any {
	return map[string]any{
		"object": "chat.completion",
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
	}
}

func hasToolResult(msgs []spin.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed,
// so the tests read the report exactly as a user would.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	return <-done
}

// TestCompareReportShape: two models answering differently produce one report
// with table rows in the order given, a trimmed gist per row, verbatim answers
// under per-model headings, an agreement note that disclaims leaderboard use —
// and each model ran on its own fresh session id with its own transcript,
// tagged "compare", given the same task.
func TestCompareReportShape(t *testing.T) {
	geminiAnswer := "The gateway keeps one conversation per session id. It prunes each turn's prompt to the latest tool results, so context never accumulates."
	grokAnswer := "The gateway pins one upstream conversation per session id and sends only the latest tool results each turn. Context never accumulates anywhere client side."
	gw := newFakeGateway(t,
		[]string{"chimera-gemini", "chimera-grok"},
		map[string]string{"chimera-gemini": geminiAnswer, "chimera-grok": grokAnswer},
		map[string]bool{"chimera-gemini": true}, // one tool round before answering
		nil,
	)
	sessionsDir := t.TempDir()
	cfg := compareRun{
		baseURL: gw.srv.URL + "/v1", token: "test-token",
		models:      []string{"chimera-gemini", "chimera-grok"},
		session:     "cmpbase",
		root:        t.TempDir(),
		maxRounds:   8,
		timeout:     5 * time.Second,
		sessionsDir: sessionsDir,
		tags:        []string{"mine"},
		task:        "Write a two line haiku about sessions.",
	}

	var out string
	out = captureStdout(t, func() {
		if code := runCompare(cfg); code != 0 {
			t.Errorf("runCompare exit = %d, want 0", code)
		}
	})

	// Table rows in the order the models were given.
	iG, iK := strings.Index(out, "chimera-gemini"), strings.Index(out, "chimera-grok")
	if iG < 0 || iK < 0 || iG > iK {
		t.Fatalf("table rows not in -models order (gemini@%d grok@%d):\n%s", iG, iK, out)
	}
	// Gists: the first sentence of each final answer, trimmed to one line.
	if !strings.Contains(out, "The gateway keeps one conversation per session id.") {
		t.Errorf("gemini gist (first sentence) missing:\n%s", out)
	}
	if !strings.Contains(out, "The gateway pins one upstream conversation") {
		t.Errorf("grok gist (first sentence) missing:\n%s", out)
	}
	// Full answers verbatim, under a heading each.
	if !strings.Contains(out, "## chimera-gemini\n\n"+geminiAnswer) {
		t.Errorf("gemini answer not verbatim under its heading:\n%s", out)
	}
	if !strings.Contains(out, "## chimera-grok\n\n"+grokAnswer) {
		t.Errorf("grok answer not verbatim under its heading:\n%s", out)
	}
	// Agreement note present, qualitative, explicitly not a leaderboard.
	if !strings.Contains(out, "Agreement note") || !strings.Contains(out, "not a leaderboard") {
		t.Errorf("agreement note / disclaimer missing:\n%s", out)
	}
	if !strings.Contains(out, "% shared vocabulary") {
		t.Errorf("pairwise overlap not reported:\n%s", out)
	}
	// Row accounting: gemini ran 2 rounds with 1 tool call, grok 1 round, 0.
	for model, want := range map[string]string{
		"chimera-gemini": `(?m)^chimera-gemini\s+\d+\.\d+s\s+2\s+1\s+\d+\s+`,
		"chimera-grok":   `(?m)^chimera-grok\s+\d+\.\d+s\s+1\s+0\s+\d+\s+`,
	} {
		if ok, err := regexp.MatchString(want, out); err != nil || !ok {
			t.Errorf("table row for %s missing or wrong shape (err=%v):\n%s", model, err, out)
		}
	}

	// Fresh session per model, derived as <session>-<model>, same task for both.
	for model, wantSession := range map[string]string{
		"chimera-gemini": "cmpbase-chimera-gemini",
		"chimera-grok":   "cmpbase-chimera-grok",
	} {
		gw.mu.Lock()
		seen := gw.sessions[model]
		task := gw.firstUser[model]
		gw.mu.Unlock()
		if len(seen) != 1 || !seen[wantSession] {
			t.Errorf("%s ran on sessions %v; want exactly [%s]", model, seen, wantSession)
		}
		if task != cfg.task {
			t.Errorf("%s received task %q; want %q", model, task, cfg.task)
		}
	}

	// One transcript per model, header tagged "compare", final answer recorded.
	for model, answer := range map[string]string{"chimera-gemini": geminiAnswer, "chimera-grok": grokAnswer} {
		path := filepath.Join(sessionsDir, "cmpbase-"+model+".jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("transcript for %s: %v", model, err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var header struct {
			Kind  string   `json:"kind"`
			Model string   `json:"model"`
			Tags  []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
			t.Fatalf("transcript header for %s: %v", model, err)
		}
		if header.Kind != "session" || header.Model != model {
			t.Errorf("transcript header for %s = %s/%s", model, header.Kind, header.Model)
		}
		if len(header.Tags) != 2 || header.Tags[0] != "mine" || header.Tags[1] != "compare" {
			t.Errorf("transcript tags for %s = %v; want [mine compare]", model, header.Tags)
		}
		var final struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &final); err != nil ||
			final.Kind != "final" || final.Text != answer {
			t.Errorf("transcript for %s does not end with its final answer (err=%v)", model, err)
		}
	}
}

// TestCompareProviderFailureKeepsReport: a failing provider becomes an error
// row (redacted reason) and an empty answer section; the other model's answer
// renders untouched and the run still counts as useful. When every model
// fails, the report still renders but the exit code is 1.
func TestCompareProviderFailureKeepsReport(t *testing.T) {
	t.Run("one failure does not sink the report", func(t *testing.T) {
		geminiAnswer := "The gateway keeps one conversation per session id."
		gw := newFakeGateway(t,
			[]string{"chimera-broken", "chimera-gemini"},
			map[string]string{"chimera-gemini": geminiAnswer, "chimera-broken": ""},
			nil,
			map[string]bool{"chimera-broken": true},
		)
		cfg := compareRun{
			baseURL: gw.srv.URL + "/v1", token: "sekrit-token",
			models:      []string{"chimera-broken", "chimera-gemini"},
			session:     "cmpbase",
			root:        t.TempDir(),
			maxRounds:   8,
			timeout:     5 * time.Second,
			sessionsDir: t.TempDir(),
			task:        "Write a two line haiku about sessions.",
		}
		code := -1
		out := captureStdout(t, func() { code = runCompare(cfg) })

		if code != 0 {
			t.Errorf("exit = %d; want 0 while one model answered", code)
		}
		// The failed model is first (as given) and carries the failure row.
		iB, iG := strings.Index(out, "chimera-broken"), strings.Index(out, "chimera-gemini")
		if iB < 0 || iG < 0 || iB > iG {
			t.Fatalf("rows not in -models order:\n%s", out)
		}
		if !strings.Contains(out, "failed: gateway 500: upstream quota exhausted") {
			t.Errorf("error row missing its redacted reason:\n%s", out)
		}
		if !strings.Contains(out, geminiAnswer) {
			t.Errorf("the surviving model's answer was lost:\n%s", out)
		}
		if !strings.Contains(out, "## chimera-broken\n\n(no answer — failed:") {
			t.Errorf("failing model lacks an empty answer section:\n%s", out)
		}
		if !strings.Contains(out, "nothing to compare") {
			t.Errorf("agreement note should degenerate gracefully with one answer:\n%s", out)
		}
	})

	t.Run("every model failing still renders, exit 1", func(t *testing.T) {
		gw := newFakeGateway(t,
			[]string{"chimera-broken", "chimera-alsobroken"},
			map[string]string{}, nil,
			map[string]bool{"chimera-broken": true, "chimera-alsobroken": true},
		)
		cfg := compareRun{
			baseURL: gw.srv.URL + "/v1", token: "gate-token",
			models:      []string{"chimera-broken", "chimera-alsobroken"},
			session:     "cmpbase",
			root:        t.TempDir(),
			maxRounds:   8,
			timeout:     5 * time.Second,
			sessionsDir: t.TempDir(),
			task:        "Write a two line haiku about sessions.",
		}
		code := -1
		out := captureStdout(t, func() { code = runCompare(cfg) })
		if code != 1 {
			t.Errorf("exit = %d; want 1 when every model failed", code)
		}
		// Each failed model shows the reason twice: once in its table row's
		// gist, once in its answer section.
		if !strings.Contains(out, "chimera-spin compare") ||
			strings.Count(out, "failed: gateway 500: upstream quota exhausted") != 4 {
			t.Errorf("report with two failure rows not rendered:\n%s", out)
		}
	})
}

// TestCompareSingleModelRun: one model in -models behaves like the single run —
// one session, one transcript, one table row, the answer verbatim.
func TestCompareSingleModelRun(t *testing.T) {
	answer := "Rounds are capped at eight by default."
	gw := newFakeGateway(t,
		[]string{"chimera-deepseek"},
		map[string]string{"chimera-deepseek": answer}, nil, nil,
	)
	sessionsDir := t.TempDir()
	cfg := compareRun{
		baseURL: gw.srv.URL + "/v1", token: "test-token",
		models:      []string{"chimera-deepseek"},
		session:     "cmpbase",
		root:        t.TempDir(),
		maxRounds:   8,
		timeout:     5 * time.Second,
		sessionsDir: sessionsDir,
		task:        "Write a two line haiku about sessions.",
	}
	code := -1
	out := captureStdout(t, func() { code = runCompare(cfg) })

	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, answer) {
		t.Errorf("answer not in the report:\n%s", out)
	}
	if ok, err := regexp.MatchString(`(?m)^chimera-deepseek\s+\d+\.\d+s\s+1\s+0\s+\d+\s+`, out); err != nil || !ok {
		t.Errorf("expected exactly one 1-round table row:\n%s", out)
	}
	gw.mu.Lock()
	seen := gw.sessions["chimera-deepseek"]
	gw.mu.Unlock()
	if len(seen) != 1 || !seen["cmpbase-chimera-deepseek"] {
		t.Errorf("sessions = %v; want exactly [cmpbase-chimera-deepseek]", seen)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "cmpbase-chimera-deepseek.jsonl")); err != nil {
		t.Errorf("transcript missing: %v", err)
	}
}

// TestParseModelList: comma-separated, trimmed, empties dropped, duplicates
// collapsed in order — and an empty list is a flag error, so a missing
// -models can never hijack the single-run path.
func TestParseModelList(t *testing.T) {
	got, err := parseModelList(" chimera-gemini , chimera-grok ,, chimera-gemini ")
	if err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	want := []string{"chimera-gemini", "chimera-grok"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseModelList = %v; want %v", got, want)
	}
	for _, empty := range []string{"", " , ,"} {
		if _, err := parseModelList(empty); err == nil {
			t.Errorf("parseModelList(%q) accepted; want error", empty)
		}
	}
}

// TestFirstSentenceGist pins the gist heuristic: first sentence, heading
// skipped, whitespace collapsed, abbreviations like v1.2 not treated as
// sentence ends, clipping rune-safe and ellipsised.
func TestFirstSentenceGist(t *testing.T) {
	cases := []struct{ in, want string }{
		{"First sentence here. Second sentence.", "First sentence here."},
		{"# Heading\nBody text follows. More.", "Body text follows."},
		{"v1.2 shipped today", "v1.2 shipped today"},
		{"one\ntwo. three", "one two."},
		{"No terminator", "No terminator"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstSentence(c.in, 200); got != c.want {
			t.Errorf("firstSentence(%q) = %q; want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("word ", 40)
	got := firstSentence(long, 20)
	if utf8.RuneCountInString(got) != 20 || !strings.HasSuffix(got, "…") {
		t.Errorf("clipped gist = %q (%d runes); want 20 runes ending in …", got, utf8.RuneCountInString(got))
	}
}

// TestAgreementNotePhrasing: identical vocabulary reads as agreement, disjoint
// vocabulary as divergence, and the note always disclaims leaderboard use.
func TestAgreementNotePhrasing(t *testing.T) {
	same := "The gateway prunes history every turn."
	out := agreementNote([]modelOutcome{
		{Model: "a", Res: spin.Result{Text: same}},
		{Model: "b", Res: spin.Result{Text: same}},
	})
	if !strings.Contains(out, "largely agree") || !strings.Contains(out, "not a leaderboard") {
		t.Errorf("identical answers should read as agreement:\n%s", out)
	}

	out = agreementNote([]modelOutcome{
		{Model: "a", Res: spin.Result{Text: "Alpha beta gamma delta epsilon."}},
		{Model: "b", Res: spin.Result{Text: "Zeta eta theta iota kappa."}},
	})
	if !strings.Contains(out, "diverge — read both") {
		t.Errorf("disjoint answers should read as divergence:\n%s", out)
	}

	one := agreementNote([]modelOutcome{{Model: "a", Res: spin.Result{Text: same}}})
	if !strings.Contains(one, "nothing to compare") {
		t.Errorf("a single answer should not be compared:\n%s", one)
	}
}

// TestRedactReason: the bearer token never survives into a failure row,
// newlines collapse, and long reasons are clipped rune-safely.
func TestRedactReason(t *testing.T) {
	got := redactReason(errors.New("dial failed\ntoken Bearer supersekrit\nretry"), "supersekrit")
	if strings.Contains(got, "supersekrit") {
		t.Errorf("token leaked: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("reason stayed multiline: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("token not masked: %q", got)
	}
	got = redactReason(errors.New(strings.Repeat("é", 400)), "")
	if utf8.RuneCountInString(got) > reasonMaxRunes {
		t.Errorf("clip = %d runes; want <= %d", utf8.RuneCountInString(got), reasonMaxRunes)
	}
	// A token too short to mask safely is left alone: masking it would shred
	// the reason into ellipses.
	if got := redactReason(errors.New("gateway 500: upstream quota exhausted"), "t"); !strings.Contains(got, "gateway 500: upstream quota exhausted") {
		t.Errorf("short token must not shred the reason: %q", got)
	}
}

// TestCompareTagsAppended: the runs are tagged "compare" in addition to the
// user's tags, never twice.
func TestCompareTagsAppended(t *testing.T) {
	if got := compareTags([]string{"mine"}); len(got) != 2 || got[0] != "mine" || got[1] != "compare" {
		t.Errorf("compareTags([mine]) = %v; want [mine compare]", got)
	}
	if got := compareTags(nil); len(got) != 1 || got[0] != "compare" {
		t.Errorf("compareTags(nil) = %v; want [compare]", got)
	}
	if got := compareTags([]string{"compare", "mine"}); len(got) != 2 || got[0] != "compare" {
		t.Errorf("compareTags must not duplicate the tag: %v", got)
	}
}
