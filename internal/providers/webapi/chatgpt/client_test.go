package chatgptweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// decodeBody is a test-only convenience for reading a request body.
func decodeBody(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"no session cookie"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"accessToken":"bearer-1"}`)
	})

	mux.HandleFunc("/backend-api/sentinel/chat-requirements/prepare", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"prep-token","proofofwork":{"required":true,"seed":"seed-1","difficulty":4},"turnstile":{"required":true}}`)
	})

	mux.HandleFunc("/backend-api/sentinel/chat-requirements", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("openai-sentinel-chat-requirements-token") != "prep-token" {
			http.Error(w, "missing prepare token", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"final-token","proofofwork":{"required":true,"seed":"seed-1","difficulty":4}}`)
	})

	mux.HandleFunc("/backend-api/conversation", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConversationID string `json:"conversation_id"`
		}
		_ = decodeBody(r, &body)
		if body.ConversationID == "waf" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html><body>Verifying you are human</body></html>")
			return
		}
		if r.Header.Get("openai-sentinel-proof-token") == "" {
			http.Error(w, "missing proof", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		write := func(s string) { fmt.Fprint(w, s); flush.Flush() }
		// delta_encoding: v1 — first event declares path+op, the rest carry values
		// on the same path (spec 012 §3).
		write("data: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"PONG\"}\n\n")
		write("data: {\"v\":\"!\"}\n\n")
		write("data: {\"p\":\"/conversation_id\",\"o\":\"add\",\"v\":\"conv-1\"}\n\n")
		write("data: {\"p\":\"/message/id\",\"o\":\"add\",\"v\":\"msg-1\"}\n\n")
		write("data: [DONE]\n\n")
	})
	return httptest.NewServer(mux)
}

func testClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	srv := testServer(t)
	c := NewClient(&webapi.Session{
		AccessToken: "cookie-session", // cookie custody: bearer is minted on demand
		Cookies:     map[string]string{"__Secure-next-auth.session-token": "jwt"},
	}, 30*time.Second)
	c.BaseURL = srv.URL
	return c, srv
}

func TestRefreshTokenFromCookie(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	tok, err := c.RefreshToken()
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if tok != "bearer-1" {
		t.Fatalf("token = %q", tok)
	}
}

func TestRefreshTokenWithoutCookieIsSessionExpired(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()
	c.cookies = nil

	_, err := c.RefreshToken()
	if err == nil {
		t.Fatal("expected error with no cookie jar")
	}
	var he *webapi.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("expected a 401 HTTPError, got %T: %v", err, err)
	}
	if !webapi.Retryable(err) {
		t.Errorf("401 should be retryable")
	}
}

func TestSentinelHandshake(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	requirements, proof, err := c.Sentinel()
	if err != nil {
		t.Fatalf("Sentinel: %v", err)
	}
	if requirements != "final-token" {
		t.Fatalf("requirements token = %q, want final-token", requirements)
	}
	if proof == "" {
		t.Fatal("expected a solved proof token")
	}
	if !VerifyProof(proof, "seed-1", 4, DefaultPoWConfig()) {
		t.Fatalf("proof %q does not verify", proof)
	}
}

func TestSendAssemblesDeltaStream(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("auto", "", "", "say PONG")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Content != "PONG!" {
		t.Fatalf("content = %q, want %q", res.Content, "PONG!")
	}
	if res.Conversation != "conv-1" {
		t.Fatalf("conversation = %q", res.Conversation)
	}
	if res.MessageID != "msg-1" {
		t.Fatalf("message id = %q", res.MessageID)
	}
	if res.TTFT <= 0 {
		t.Fatalf("ttft = %v", res.TTFT)
	}
	if c.token != "bearer-1" {
		t.Fatalf("client did not refresh its bearer: %q", c.token)
	}
}

func TestSendDetectsWAF(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	// conversation_id "waf" makes the fixture answer with an HTML challenge.
	res, err := c.Send("auto", "waf", "", "hi")
	if err == nil {
		t.Fatalf("expected WAF error, got %+v", res)
	}
	if !errors.Is(err, webapi.ErrWAF) {
		t.Fatalf("expected ErrWAF, got %v", err)
	}
	if webapi.Reason(err) != "waf" || !webapi.Retryable(err) {
		t.Fatalf("WAF error misclassified: reason=%q retryable=%v", webapi.Reason(err), webapi.Retryable(err))
	}
}

func TestProviderSendMessageThreadsConversations(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	cfg := &config.Config{ChatGPTWebModel: "auto", ResponseTimeout: 30 * time.Second}
	p := New(cfg)
	p.client = c

	resp, err := p.SendMessage("say PONG", "session-a")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.Message != "PONG!" {
		t.Fatalf("message = %q", resp.Message)
	}
	p.mu.Lock()
	conv := p.convs["session-a"]
	p.mu.Unlock()
	if conv.conversationID != "conv-1" || conv.parentMessageID != "msg-1" {
		t.Fatalf("conversation state = %+v", conv)
	}
	if err := p.NewChat(); err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.convs) != 0 {
		t.Fatalf("conversations not cleared: %+v", p.convs)
	}
}

func TestProviderNameAndModel(t *testing.T) {
	p := New(&config.Config{})
	if p.Name() != config.ProviderChatGPT {
		t.Errorf("Name = %q", p.Name())
	}
	if p.ModelID() != "chimera-chatgpt" {
		t.Errorf("ModelID = %q (must match the DOM transport)", p.ModelID())
	}
	if ok, err := p.IsLoggedIn(); err != nil || ok {
		t.Errorf("uninitialized IsLoggedIn = %v, %v; want false, nil", ok, err)
	}
}

func TestRegistryHasChatGPT(t *testing.T) {
	if _, ok := webapi.Lookup(config.ProviderChatGPT); !ok {
		t.Fatal("chatgpt not registered with the webapi registry")
	}
}

func TestIsContentPath(t *testing.T) {
	cases := map[string]bool{
		"/message/content/parts/0": true,
		"/message/content/parts/1": true,
		"/message/author/role":     false,
		"/conversation_id":         false,
		"/message/metadata/foo":    false,
	}
	for path, want := range cases {
		if got := isContentPath(path); got != want {
			t.Errorf("isContentPath(%q) = %v, want %v", path, got, want)
		}
	}
}
