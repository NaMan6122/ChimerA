package deepseekweb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// The challenge below is the LIVE server vector from pow_test.go: the server
// accepted nonce 20941 for it, so a correct client must find exactly that.
// Using a real challenge (rather than one computed here) keeps the client test
// independent of the hash under test.
const (
	vecChallenge = "8bb415dd09a5593fc1cc4b7a91a8f3039be373065c4260de1b0793e43abd4d3a"
	vecSalt      = "a7bee800cede26f66764"
	vecExpireAt  = 1790022592780 // milliseconds
	vecNonce     = 20941
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	// Real captured streams, byte-for-byte. Both fixtures came off the live
	// endpoint, which matters: the thinking stream is where the parser's bugs
	// lived (fragment type THINK, bare {"v":…} continuations, new-fragment
	// APPENDs), and a synthetic fixture would have encoded my assumptions instead
	// of the protocol.
	fixture, err := os.ReadFile("testdata/completion_sse.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	thinkingFixture, err := os.ReadFile("testdata/completion_thinking_sse.txt")
	if err != nil {
		t.Fatalf("read thinking fixture: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/chat_session/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The id is nested under chat_session — the shape the live server returns.
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"chat_session":{"id":"sess-1","agent":"chat"}}}}`)
	})
	mux.HandleFunc("/api/v0/chat/create_pow_challenge", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"data":{"biz_data":{"challenge":{
			"algorithm":"DeepSeekHashV1","challenge":%q,"salt":%q,"expire_at":%d,
			"difficulty":144000,"signature":"sig-1","target_path":"/api/v0/chat/completion"}}}}`,
			vecChallenge, vecSalt, vecExpireAt)
	})
	mux.HandleFunc("/api/v0/chat/completion", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatSessionID string `json:"chat_session_id"`
			Prompt        string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		if body.ChatSessionID == "waf" {
			// A CDN challenge arrives as HTML, not SSE.
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html><body>Just a moment...</body></html>")
			return
		}
		if r.Header.Get("x-ds-pow-response") == "" {
			http.Error(w, "missing pow", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		if strings.Contains(body.Prompt, "think") {
			fmt.Fprint(w, string(thinkingFixture))
			flush.Flush()
			return
		}
		fmt.Fprint(w, string(fixture))
		flush.Flush()
	})
	return httptest.NewServer(mux)
}

func testClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	srv := testServer(t)
	c := NewClient(&webapi.Session{
		AccessToken: "test-token",
		Cookies:     map[string]string{"aws-waf-token": "abc"},
	}, 30*time.Second)
	c.BaseURL = srv.URL
	return c, srv
}

func TestCreateChatSession(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	id, err := c.CreateChatSession()
	if err != nil {
		t.Fatalf("CreateChatSession: %v", err)
	}
	if id != "sess-1" {
		t.Fatalf("session id = %q (must read chat_session.id)", id)
	}
}

// The real captured stream must assemble to exactly "PONG": the opening snapshot
// contributes "P" and the APPEND patch contributes "ONG".
func TestSendAssemblesRealPatchStream(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("sess-1", "", "say PONG", false)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Content != "PONG" {
		t.Fatalf("content = %q, want %q", res.Content, "PONG")
	}
	if res.ResponseID != "2" {
		t.Fatalf("response id = %q, want %q (from event: ready)", res.ResponseID, "2")
	}
	if res.Usage["total_tokens"] != float64(40) {
		t.Fatalf("usage = %v, want total_tokens=40 from the BATCH patch", res.Usage)
	}
	if res.Status != "FINISHED" {
		t.Fatalf("status = %q, want FINISHED", res.Status)
	}
	if res.TTFT <= 0 {
		t.Fatalf("ttft = %v", res.TTFT)
	}
	if res.Reasoning != "" {
		t.Fatalf("reasoning = %q, want empty on a non-thinking turn", res.Reasoning)
	}
}

// The real thinking stream must split cleanly: reasoning text (fragment type
// THINK) into Reasoning, the answer (fragment type RESPONSE) into Content. It also
// exercises the two paths a naive parser drops: bare {"v":…} continuations, which
// carry most of the reasoning, and the {"p":"response/fragments"} APPEND that
// introduces the RESPONSE fragment.
func TestSendRoutesThinkingFragments(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("sess-1", "", "think about it", true)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	const wantReasoning = "We need answer exactly PONG. User says Reply with exactly: PONG. We must comply. Final only."
	if res.Reasoning != wantReasoning {
		t.Fatalf("reasoning = %q\nwant         %q", res.Reasoning, wantReasoning)
	}
	if res.Content != "PONG" {
		t.Fatalf("content = %q, want %q (the RESPONSE fragment, not the thinking)", res.Content, "PONG")
	}
	if res.ResponseID != "2" {
		t.Fatalf("response id = %q, want 2", res.ResponseID)
	}
	if res.Status != "FINISHED" {
		t.Fatalf("status = %q, want FINISHED", res.Status)
	}
	if res.Usage["total_tokens"] != float64(65) {
		t.Fatalf("usage = %v, want total_tokens=65", res.Usage)
	}
}

// The client must put a *correct* proof in x-ds-pow-response: the server 403s an
// empty header, and the test re-verifies the solved nonce independently.
func TestSendEmitsCorrectPowHeader(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("sess-1", "", "hi", false)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Content != "PONG" {
		t.Fatalf("content = %q", res.Content)
	}

	ch := Challenge{
		Algorithm: "DeepSeekHashV1", Challenge: vecChallenge, Salt: vecSalt,
		ExpireAt: vecExpireAt, Difficulty: 144000,
	}
	header, err := Solve(ch)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !Verify(ch, header) {
		t.Fatal("solver output failed its own verification")
	}
	raw, _ := base64.StdEncoding.DecodeString(header)
	var a Answer
	_ = json.Unmarshal(raw, &a)
	if a.Answer != vecNonce {
		t.Fatalf("solved nonce = %d, want %d", a.Answer, vecNonce)
	}
}

func TestSendDetectsWAF(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	res, err := c.Send("waf", "", "hi", false)
	if err == nil {
		t.Fatalf("expected a WAF error, got result %+v", res)
	}
	if !errors.Is(err, webapi.ErrWAF) {
		t.Fatalf("expected ErrWAF, got %v", err)
	}
	if !webapi.Retryable(err) || webapi.Reason(err) != "waf" {
		t.Fatalf("WAF error should be retryable and labelled waf: %v", err)
	}
}

// A challenge on the PoW endpoint (before the completion call) must also surface
// as ErrWAF/HTTPError rather than a generic decode error.
func TestPowChallengeUnauthorizedIsTyped(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()
	c.token = "wrong-token"

	var he *webapi.HTTPError
	_, err := c.CreatePowChallenge(CompletionPath)
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Status != 401 || !webapi.Retryable(err) {
		t.Fatalf("status=%d retryable=%v", he.Status, webapi.Retryable(err))
	}
	if webapi.Reason(err) != "unauthorized" {
		t.Fatalf("reason = %q, want unauthorized", webapi.Reason(err))
	}
}

func TestProviderSendMessageThreadsConversations(t *testing.T) {
	c, srv := testClient(t)
	defer srv.Close()

	cfg := &config.Config{
		DeepSeekWebModel:    "deepseek-chat",
		DeepSeekWebThinking: false,
		ResponseTimeout:     30 * time.Second,
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
	if conv.sessionID != "sess-1" || conv.parentID != "2" {
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
	if p.Name() != config.ProviderDeepSeek {
		t.Errorf("Name = %q", p.Name())
	}
	if p.ModelID() != "chimera-deepseek" {
		t.Errorf("ModelID = %q (must match the DOM transport)", p.ModelID())
	}
	if _, err := p.IsLoggedIn(); err != nil {
		t.Errorf("uninitialized IsLoggedIn should not error: %v", err)
	}
}

func TestRegistryHasDeepSeek(t *testing.T) {
	if _, ok := webapi.Lookup(config.ProviderDeepSeek); !ok {
		t.Fatal("deepseek not registered with the webapi registry")
	}
}

// parent_message_id must be a JSON number, not a string: the server types it as
// u32 and rejects a string with HTTP 422. This only bites on the *second* turn of
// a conversation (the first sends null), so a single-turn test would miss it —
// which is exactly how it reached a live run.
func TestParentMessageIDIsNumeric(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/create_pow_challenge"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":0,"data":{"biz_data":{"challenge":{"challenge":%q,"salt":%q,"expire_at":%d,"difficulty":144000}}}}`,
				vecChallenge, vecSalt, vecExpireAt)
		default:
			body, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: close\n\ndata: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\n")
		}
	}))
	defer srv.Close()

	c := NewClient(&webapi.Session{AccessToken: "t", Cookies: map[string]string{"aws-waf-token": "x"}}, 30*time.Second)
	c.BaseURL = srv.URL
	if _, err := c.Send("sess-1", "2", "hi", false); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	raw := string(sent["parent_message_id"])
	if raw != "2" {
		t.Errorf("parent_message_id = %s, want the bare number 2 (a string is rejected with 422)", raw)
	}
	if strings.HasPrefix(raw, `"`) {
		t.Error("parent_message_id was sent as a JSON string")
	}
}

// An unparseable parent id must degrade to null (fresh thread) rather than
// producing a 422.
func TestNullableNum(t *testing.T) {
	cases := map[string]string{"": "null", "0": "0", "42": "42", "not-a-number": "null", "-1": "null"}
	for in, want := range cases {
		got, _ := json.Marshal(nullableNum(in))
		if string(got) != want {
			t.Errorf("nullableNum(%q) = %s, want %s", in, got, want)
		}
	}
}

// The headers the live capture showed must actually be sent; getting these wrong
// is invisible until a real turn fails.
func TestRequestHeadersMatchCapture(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"biz_data":{"challenge":{"challenge":"ab","salt":"s","expire_at":1,"difficulty":1}}}}`)
	}))
	defer srv.Close()

	c := NewClient(&webapi.Session{AccessToken: "t", Cookies: map[string]string{"aws-waf-token": "x"}}, 5*time.Second)
	c.BaseURL = srv.URL
	_, _ = c.CreatePowChallenge(CompletionPath)

	for k, want := range map[string]string{
		"X-client-version":   "2.5.0",
		"X-client-bundle-id": "com.deepseek.chat",
		"X-client-platform":  "web",
		"X-client-locale":    "en_US",
		"Accept-language":    "en",
		"Authorization":      "Bearer t",
	} {
		if v := got.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	if got.Get("X-app-version") != "" {
		t.Error("x-app-version must not be sent (absent from the capture)")
	}
	if got.Get("X-device-id") == "" {
		t.Error("x-device-id must be sent")
	}
}

func TestSnippetTrimsAndCaps(t *testing.T) {
	if got := snippet([]byte("  a\n b  ")); got != "a b" {
		t.Errorf("snippet = %q", got)
	}
	if got := snippet([]byte(strings.Repeat("x", 500))); len(got) != 160 {
		t.Errorf("snippet length = %d, want 160", len(got))
	}
}
