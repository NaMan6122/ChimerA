package qwenweb

import (
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

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"qwen3.8-max"},{"id":"qwen3.7-plus"}]}`)
	})
	mux.HandleFunc("/api/v2/chats/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"chat-1"}}`)
	})
	mux.HandleFunc("/api/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("chat_id") == "waf" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::blocked"]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s)
			flusher.Flush()
		}
		write("data: {\"response.created\":{\"response_id\":\"resp-1\"}}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"phase\":\"think\",\"content\":\"hmm\"}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"phase\":\"answer\",\"content\":\"PONG\"}}],\"response_id\":\"resp-1\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"phase\":\"answer\",\"status\":\"finished\",\"content\":\"\"}}],\"response_id\":\"resp-1\"}\n\n")
		write("data: [DONE]\n\n")
	})
	return httptest.NewServer(mux)
}

func testClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	srv := testServer(t)
	c := NewClient(&Session{
		AccessToken: "test-token",
		Cookies:     map[string]string{"acw_tc": "abc", "token": "test-token"},
	}, 30*time.Second)
	c.BaseURL = srv.URL
	c.WuURL = "" // no network in tests
	return c, srv
}

func TestListModels(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	ids, err := c.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "qwen3.7-plus" || ids[1] != "qwen3.8-max" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestNewChatAndSend(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	chatID, err := c.NewChat("qwen3.8-max")
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if chatID != "chat-1" {
		t.Fatalf("chatID = %q", chatID)
	}
	res, err := c.Send("qwen3.8-max", chatID, "", "say PONG", false)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Content != "PONG" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Reasoning != "hmm" {
		t.Fatalf("reasoning = %q", res.Reasoning)
	}
	if res.ResponseID != "resp-1" {
		t.Fatalf("response id = %q", res.ResponseID)
	}
	if res.Usage["input_tokens"] != float64(10) {
		t.Fatalf("usage = %v", res.Usage)
	}
	if res.TTFT <= 0 {
		t.Fatalf("ttft = %v", res.TTFT)
	}
}

func TestSendDetectsWAF(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("qwen3.8-max", "waf", "", "hi", false)
	if err == nil {
		t.Fatalf("expected WAF error, got result %+v", res)
	}
	if !strings.Contains(err.Error(), "waf challenge") {
		t.Fatalf("expected ErrWAF, got %v", err)
	}
}

func TestHTTPErrorRetryable(t *testing.T) {
	cases := map[int]bool{
		400: false, 401: true, 403: true, 404: false,
		429: true, 500: true, 503: true,
	}
	for status, want := range cases {
		he := &HTTPError{Status: status}
		if got := he.Retryable(); got != want {
			t.Errorf("HTTPError{%d}.Retryable() = %v, want %v", status, got, want)
		}
	}
}

func TestListModelsAuthErrorIsTyped(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()
	c.token = "wrong-token"

	_, err := c.ListModels()
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Status != 401 || !he.Retryable() {
		t.Fatalf("status=%d retryable=%v", he.Status, he.Retryable())
	}
}

func TestProviderSendMessageThreadsConversations(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	cfg := &config.Config{
		QwenWebModel:    "qwen3.8-max",
		QwenWebThinking: false,
		ResponseTimeout: 30 * time.Second,
	}
	p := New(cfg)
	p.client = c

	resp, err := p.SendMessage("say PONG", "session-a")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.Message != "PONG" {
		t.Fatalf("message = %q", resp.Message)
	}
	p.mu.Lock()
	conv := p.convs["session-a"]
	p.mu.Unlock()
	if conv.chatID != "chat-1" || conv.parentID != "resp-1" {
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

func TestProviderImplementsSessionCheck(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	p := New(&config.Config{QwenWebModel: "qwen3.8-max"})
	if ok, _ := p.IsLoggedIn(); ok {
		t.Fatal("uninitialized provider must not report logged in")
	}
	p.client = c
	if ok, err := p.IsLoggedIn(); err != nil || !ok {
		t.Fatalf("IsLoggedIn = %v, %v", ok, err)
	}
}

// qwen must register itself with the webapi registry. Without this the gateway
// silently falls through to launching Chromium: auto mode treats a missing
// constructor as an init failure rather than an error, so the regression is
// invisible except as a ~10x latency and ~64x memory change.
func TestRegistryHasQwen(t *testing.T) {
	c, ok := webapi.Lookup(config.ProviderQwen)
	if !ok {
		t.Fatal("qwen not registered with the webapi registry")
	}
	if p := c(&config.Config{QwenWebModel: "qwen3.8-max"}); p.Name() != config.ProviderQwen {
		t.Fatalf("constructor built provider %q, want %q", p.Name(), config.ProviderQwen)
	}
}
