package api

// Gateway session-recording tests: every provider turn through
// /v1/chat/completions and /v1/responses lands in a local transcript, one
// file per conversation, readable with `chimera sessions`. Recording is
// disabled (no store) when SESSIONS_DIR is empty.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/sessions"
	"github.com/chimera/chimera/internal/spin"
)

// errProvider fails every send, to exercise the error-note path.
type errProvider struct{ mockProvider }

func (m *errProvider) SendMessage(string, string) (*models.ProviderResponse, error) {
	return nil, errBoom{}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func chatPOST(t *testing.T, srv *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func transcriptSummaries(t *testing.T, dir string) []*sessions.Summary {
	t.Helper()
	sums, err := sessions.List(dir)
	if err != nil {
		t.Fatalf("sessions.List: %v", err)
	}
	return sums
}

func TestGatewayRecordsTranscript(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = t.TempDir()
	srv := NewServer(cfg, &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"})

	w := chatPOST(t, srv, `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}

	sums := transcriptSummaries(t, cfg.SessionsDir)
	if len(sums) != 1 {
		t.Fatalf("transcripts = %d, want 1", len(sums))
	}
	s := sums[0]
	if s.Task != "hi" {
		t.Errorf("task = %q, want the user message", s.Task)
	}
	if !strings.Contains(s.Final, "mock reply to: hi") {
		t.Errorf("final = %q, want the provider reply", s.Final)
	}
	if !sessions.HasTag(s.Tags, "gateway") || !sessions.HasTag(s.Tags, "chatgpt") {
		t.Errorf("tags = %v, want gateway + provider", s.Tags)
	}
	if s.Rounds != 1 {
		t.Errorf("rounds = %d, want 1", s.Rounds)
	}
}

func TestGatewayContinuationLandsInOneTranscript(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = t.TempDir()
	srv := NewServer(cfg, &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"})

	// Turn 1: stateless, no session header.
	if w := chatPOST(t, srv, `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`, nil); w.Code != http.StatusOK {
		t.Fatalf("turn 1: %d body %s", w.Code, w.Body.String())
	}
	// Turn 2: the same conversation re-sent with growing history — the same
	// opening must append to the same transcript, not start a new file.
	body := `{"model":"chimera-chatgpt","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"mock reply to: hi"},` +
		`{"role":"user","content":"and now?"}]}`
	if w := chatPOST(t, srv, body, nil); w.Code != http.StatusOK {
		t.Fatalf("turn 2: %d body %s", w.Code, w.Body.String())
	}

	sums := transcriptSummaries(t, cfg.SessionsDir)
	if len(sums) != 1 {
		t.Fatalf("transcripts = %d, want 1 (continuation appends)", len(sums))
	}
	if sums[0].Rounds != 2 {
		t.Errorf("rounds = %d, want 2", sums[0].Rounds)
	}
	if !strings.Contains(sums[0].Final, "mock reply to:") {
		t.Errorf("final = %q, want the last reply", sums[0].Final)
	}
}

func TestGatewaySessionHeaderNamesTranscript(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = t.TempDir()
	srv := NewServer(cfg, &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"})

	w := chatPOST(t, srv, `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Session-Id": "my-chat"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	sums := transcriptSummaries(t, cfg.SessionsDir)
	if len(sums) != 1 || sums[0].ID != "my-chat" {
		t.Fatalf("transcript id = %v, want my-chat", sums)
	}
}

func TestGatewayRecordsStreamTurn(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = t.TempDir()
	srv := NewServer(cfg, &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"})

	w := chatPOST(t, srv, `{"model":"chimera-chatgpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mock reply to: hi") {
		t.Fatalf("stream body missing reply: %s", w.Body.String())
	}

	sums := transcriptSummaries(t, cfg.SessionsDir)
	if len(sums) != 1 {
		t.Fatalf("transcripts = %d, want 1", len(sums))
	}
	if !strings.Contains(sums[0].Final, "mock reply to: hi") {
		t.Errorf("final = %q, want the streamed reply", sums[0].Final)
	}
}

func TestGatewayRecordsFailedTurn(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = t.TempDir()
	srv := NewServer(cfg, &errProvider{mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}})

	w := chatPOST(t, srv, `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}

	sums := transcriptSummaries(t, cfg.SessionsDir)
	if len(sums) != 1 {
		t.Fatalf("transcripts = %d, want 1 (failures are history too)", len(sums))
	}
	recs := loadTranscriptRecords(t, cfg.SessionsDir, sums[0].ID)
	sawUser, sawError := false, false
	for _, r := range recs {
		switch r.Kind {
		case "user":
			sawUser = true
		case "error":
			sawError = true
		case "round", "final":
			t.Errorf("failed turn recorded %q record", r.Kind)
		}
	}
	if !sawUser || !sawError {
		t.Errorf("user=%v error=%v, want both", sawUser, sawError)
	}
}

func TestGatewayRecordingDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.SessionsDir = "" // explicitly disabled
	srv := NewServer(cfg, &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"})

	w := chatPOST(t, srv, `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}
	if srv.transcripts != nil {
		t.Fatal("transcript store should be nil when SESSIONS_DIR is empty")
	}
}

func loadTranscriptRecords(t *testing.T, dir, id string) []spin.Record {
	t.Helper()
	recs, err := spin.LoadTranscript(dir, id)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return recs
}
