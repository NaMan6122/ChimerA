package spin

// Runner resilience tests, over an httptest gateway stub: a mid-run refusal
// gets one nudge instead of silently ending as a final answer, compaction
// rotates the upstream session while the harness session stays put, and the
// client retries transient gateway failures.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubGateway scripts one reply per request and records what arrived.
type stubGateway struct {
	mu       sync.Mutex
	replies  []string // per request, in order; last repeats
	sessions []string // X-Session-Id per request
	bodies   []string
}

func (g *stubGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		flat := make([]string, 0, len(req.Messages))
		for _, m := range req.Messages {
			flat = append(flat, m.Role+":"+m.Content)
		}
		g.mu.Lock()
		i := len(g.sessions)
		if len(g.replies) == 0 {
			g.mu.Unlock()
			http.Error(w, "no scripted reply", http.StatusInternalServerError)
			return
		}
		reply := g.replies[i]
		if i >= len(g.replies) {
			reply = g.replies[len(g.replies)-1]
		}
		g.sessions = append(g.sessions, r.Header.Get("X-Session-Id"))
		g.bodies = append(g.bodies, strings.Join(flat, "\n"))
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}},
			},
		})
	}
}

func (g *stubGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions)
}

func stubServer(t *testing.T, g *stubGateway) *Client {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/v1", Model: "chimera-test", Session: "sess-x", HTTP: srv.Client()}
}

func TestRunnerRefusalNudgeMidRun(t *testing.T) {
	g := &stubGateway{replies: []string{
		"```json\n{\"tool_calls\":[{\"name\":\"list_dir\",\"arguments\":{}}]}\n```",
		"I cannot read files directly, sorry.", // round 2: bridge forgotten
		"final answer after nudge",             // round 3
	}}
	client := stubServer(t, g)
	client.Session = "sess-refuse"

	dir := t.TempDir()
	toolRoot := t.TempDir()
	r := NewRunner(client, &Executor{Root: toolRoot}, 5)
	r.Ledger = NewLedger(dir, client.Session)

	var nudges, compacts int
	r.OnEvent = func(ev Event) {
		switch ev.Kind {
		case EventNudge:
			nudges++
			if !strings.Contains(ev.Text, "refused tools") {
				t.Errorf("nudge text = %q", ev.Text)
			}
		case EventCompact:
			compacts++
		}
	}

	res, err := r.Run("list the files")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text != "final answer after nudge" {
		t.Errorf("final = %q", res.Text)
	}
	if nudges != 1 || compacts != 0 {
		t.Errorf("nudges=%d compacts=%d, want 1/0", nudges, compacts)
	}
	if g.count() != 3 {
		t.Errorf("requests = %d, want 3", g.count())
	}
}

func TestRunnerCompactionRotatesUpstreamOnly(t *testing.T) {
	// Every round asks to read the same file; the file is big enough that the
	// result alone crosses a tiny threshold, forcing a rotation per round.
	big := strings.Repeat("b", 2000)
	dir := t.TempDir()
	toolRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolRoot, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	call := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"big.txt\"}}]}\n```"
	// Three rounds only: the loop guard stops identical calls at three running.
	g := &stubGateway{replies: []string{call, call, call}}
	client := stubServer(t, g)
	client.Session = "sess-comp"

	r := NewRunner(client, &Executor{Root: toolRoot}, 3)
	r.Ledger = NewLedger(dir, "sess-comp")
	r.CompactChars = 1000 // forces rotation after every round

	var compacts int
	r.OnEvent = func(ev Event) {
		if ev.Kind == EventCompact {
			compacts++
		}
	}

	// The run ends via the loop guard (three identical calls), which is fine:
	// the property under test is the upstream rotation, not convergence.
	res, rerr := r.Run("read big.txt")
	if rerr != nil && !strings.Contains(rerr.Error(), "same tool call") {
		t.Fatalf("run: %v", rerr)
	}
	_ = res

	// Rounds 1..4 with a rotation after each: upstream ids are the base, then
	// -c1, -c2, -c3. The harness session id never changes.
	g.mu.Lock()
	defer g.mu.Unlock()
	if compacts != 2 {
		t.Errorf("compactions = %d, want 2 (after rounds 1-2; round 3 ends the run)", compacts)
	}
	want := []string{"sess-comp", "sess-comp-c1", "sess-comp-c2"}
	for i, w := range want {
		if i >= len(g.sessions) {
			break
		}
		if g.sessions[i] != w {
			t.Errorf("request %d session = %q, want %q", i+1, g.sessions[i], w)
		}
	}
	// The seed message reaches the fresh conversation: system prompt + summary.
	seeded := false
	for _, b := range g.bodies {
		if strings.Contains(b, "Context was compacted") && strings.Contains(b, "system:"+SystemPrompt[:30]) {
			seeded = true
		}
	}
	if !seeded {
		t.Error("fresh upstream never received the compaction seed + system prompt")
	}
}

func TestClientRetriesTransientFailures(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	old := sendBackoff
	sendBackoff = [2]time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sendBackoff = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1", Model: "m", Session: "s", HTTP: srv.Client()}
	msg, _, err := c.Complete([]Message{{Role: "user", Content: "hi"}}, false)
	if err != nil {
		t.Fatalf("complete after retries: %v", err)
	}
	if msg.Content != "ok" || attempts != 3 {
		t.Errorf("content=%q attempts=%d, want ok/3", msg.Content, attempts)
	}
}

func TestClientDoesNotRetryPermanentErrors(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1", Model: "m", Session: "s", HTTP: srv.Client()}
	if _, _, err := c.Complete([]Message{{Role: "user", Content: "hi"}}, false); err == nil {
		t.Fatal("400 must surface as an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 400)", attempts)
	}
}
