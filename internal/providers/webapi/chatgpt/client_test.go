package chatgptweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// decodeBody is a test-only convenience for reading a request body.
func decodeBody(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// testServer replays the live handshake shape: a single chat-requirements call
// carrying proofofwork + turnstile, then a /f/conversation SSE stream.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	return testServerWith(t, liveStreamShape)
}

// testServerWith is testServer with a caller-supplied stream body, so each test
// can pin one framing without duplicating the handshake.
func testServerWith(t *testing.T, stream func(write func(string))) *httptest.Server {
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

	mux.HandleFunc("/backend-api/sentinel/chat-requirements", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			P string `json:"p"`
		}
		_ = decodeBody(r, &body)
		if !strings.HasPrefix(body.P, "gAAAAAC") {
			http.Error(w, "missing locally generated requirements token", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"persona":"chatgpt-freeaccount","token":"req-token","proofofwork":{"required":true,"seed":"seed-1","difficulty":"0f"},"turnstile":{"required":true,"dx":""}}`)
	})

	mux.HandleFunc("/backend-api/f/conversation", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConversationID string `json:"conversation_id"`
		}
		_ = decodeBody(r, &body)
		// A random conversation_id on a new chat is rejected by the real server.
		if body.ConversationID == "waf" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html><body>Verifying you are human</body></html>")
			return
		}
		if r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "req-token" {
			http.Error(w, "missing requirements token", http.StatusForbidden)
			return
		}
		if r.Header.Get("OpenAI-Sentinel-Proof-Token") == "" {
			http.Error(w, "missing proof", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		write := func(s string) { fmt.Fprint(w, s); flush.Flush() }
		stream(write)
	})
	return httptest.NewServer(mux)
}

// liveStreamShape is what a measured /f/conversation turn actually emits:
// metadata, the USER message (which is the entire prompt) and system
// scaffolding, then the assistant message re-sent as cumulative snapshots. The
// fixture used to contain one assistant frame only, which is why appending every
// message went unnoticed until a real turn echoed 44k chars of prompt back.
func liveStreamShape(write func(string)) {
	write("data: {\"type\":\"resume_conversation_token\",\"kind\":\"topic\",\"token\":\"jwt\"}\n\n")
	write("data: {\"message\":{\"id\":\"user-1\",\"author\":{\"role\":\"user\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"say PONG\"]}},\"conversation_id\":\"conv-1\"}\n\n")
	write("data: {\"type\":\"input_message\",\"input_message\":{\"id\":\"user-1\",\"author\":{\"role\":\"user\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"say PONG\"]}}}\n\n")
	write("data: {\"message\":{\"id\":\"sys-1\",\"author\":{\"role\":\"system\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"\"]}}}\n\n")
	write("data: {\"message\":{\"id\":\"msg-1\",\"author\":{\"role\":\"assistant\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"\"]}},\"conversation_id\":\"conv-1\"}\n\n")
	write("data: {\"message\":{\"id\":\"msg-1\",\"author\":{\"role\":\"assistant\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"PONG\"]}}}\n\n")
	write("data: {\"message\":{\"id\":\"msg-1\",\"author\":{\"role\":\"assistant\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"PONG!\"]}}}\n\n")
	write("data: {\"type\":\"message_stream_complete\"}\n\n")
	write("data: [DONE]\n\n")
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

// TestRefreshTokenDetectsRefreshError guards the case that fooled both the CLI
// and the assessment probe: a dead session returns HTTP 200 WITH an accessToken,
// so a naive check reports "logged in" while every turn 401s. The `error` field
// is the only reliable signal.
func TestRefreshTokenDetectsRefreshError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Both a banner AND a token: looks healthy unless `error` is checked.
		fmt.Fprint(w, `{"WARNING_BANNER":"do not share","accessToken":"stale-token","error":"RefreshAccessTokenError"}`)
	}))
	defer srv.Close()

	c := NewClient(&webapi.Session{
		AccessToken: "cookie-session",
		Cookies:     map[string]string{"__Secure-next-auth.session-token": "jwt"},
	}, 30*time.Second)
	c.BaseURL = srv.URL

	_, err := c.RefreshToken()
	if err == nil {
		t.Fatal("a session reporting RefreshAccessTokenError must not be treated as logged in")
	}
	if !errors.Is(err, webapi.ErrSessionExpired) {
		t.Errorf("expected ErrSessionExpired, got %v", err)
	}
	if webapi.Reason(err) != "session_expired" {
		t.Errorf("reason = %q, want session_expired", webapi.Reason(err))
	}
}

// TestRefreshTokenAcceptsBannerWithHealthySession pins the inverse: WARNING_BANNER
// accompanies HEALTHY sessions, so its presence must not be treated as failure.
func TestRefreshTokenAcceptsBannerWithHealthySession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"WARNING_BANNER":"do not share","accessToken":"good-token"}`)
	}))
	defer srv.Close()

	c := NewClient(&webapi.Session{
		AccessToken: "cookie-session",
		Cookies:     map[string]string{"__Secure-next-auth.session-token": "jwt"},
	}, 30*time.Second)
	c.BaseURL = srv.URL

	tok, err := c.RefreshToken()
	if err != nil {
		t.Fatalf("a healthy session carrying a banner must be accepted: %v", err)
	}
	if tok != "good-token" {
		t.Errorf("token = %q, want good-token", tok)
	}
}

func TestSentinelHandshake(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	requirements, proof, turnstile, err := c.Sentinel()
	if err != nil {
		t.Fatalf("Sentinel: %v", err)
	}
	if requirements != "req-token" {
		t.Fatalf("requirements token = %q, want req-token", requirements)
	}
	if proof == "" {
		t.Fatal("expected a solved proof token")
	}
	if !VerifyProof(proof, "seed-1", "0f") {
		t.Fatalf("proof %q does not verify", proof)
	}
	// The fixture supplies an empty dx, so no turnstile token is expected; the
	// handshake must still succeed because turnstile is advisory.
	if turnstile != "" {
		t.Errorf("turnstile = %q, want empty for an empty dx", turnstile)
	}
}

func TestSendAssemblesStream(t *testing.T) {
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

// TestSendAssemblesPatchStream covers endpoints that stream increments instead
// of snapshots: no assistant message frame ever arrives, so the patch buffer has
// to supply the text.
func TestSendAssemblesPatchStream(t *testing.T) {
	srv := testServerWith(t, func(write func(string)) {
		write("data: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"PONG\"}\n\n")
		write("data: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"!\"}\n\n")
		write("data: {\"p\":\"/conversation_id\",\"o\":\"add\",\"v\":\"conv-9\"}\n\n")
		write("data: {\"p\":\"/message/id\",\"o\":\"add\",\"v\":\"msg-9\"}\n\n")
		write("data: [DONE]\n\n")
	})
	defer srv.Close()

	c := NewClient(&webapi.Session{
		AccessToken: "cookie-session",
		Cookies:     map[string]string{"__Secure-next-auth.session-token": "jwt"},
	}, 30*time.Second)
	c.BaseURL = srv.URL

	res, err := c.Send("auto", "", "", "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Content != "PONG!" {
		t.Fatalf("content = %q, want %q", res.Content, "PONG!")
	}
	if res.Conversation != "conv-9" || res.MessageID != "msg-9" {
		t.Fatalf("conversation=%q message=%q", res.Conversation, res.MessageID)
	}
}

// TestSendDoesNotEchoPromptOrRepeatSnapshots is the regression pin for the bug
// that returned 44k chars of prompt followed by the answer four times: only the
// assistant's turn counts, and cumulative snapshots replace rather than append.
func TestSendDoesNotEchoPromptOrRepeatSnapshots(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("auto", "", "", "say PONG")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(res.Content, "say PONG") {
		t.Errorf("answer echoed the user prompt: %q", res.Content)
	}
	if strings.Count(res.Content, "PONG") != 1 {
		t.Errorf("answer repeated across snapshots (%d copies): %q", strings.Count(res.Content, "PONG"), res.Content)
	}
}

func TestSendContinuesConversation(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	if _, err := c.Send("auto", "", "", "first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// The second turn must carry the conversation id captured from the stream.
	res, err := c.Send("auto", "conv-1", "msg-1", "second")
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if res.Conversation != "conv-1" {
		t.Fatalf("conversation = %q, want conv-1", res.Conversation)
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

func TestSendRejectsMissingProof(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	// A client with no cookie jar cannot mint a bearer, so the handshake fails
	// before the turn — the failure must be classified, not panic.
	c.cookies = nil
	if _, err := c.Send("auto", "", "", "hi"); err == nil {
		t.Fatal("expected an error without a cookie jar")
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

func TestProviderImplementsInterface(t *testing.T) {
	var _ interface {
		Name() string
		ModelID() string
		NewChat() error
		IsLoggedIn() (bool, error)
	} = New(&config.Config{})
}
