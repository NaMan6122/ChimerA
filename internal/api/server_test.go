package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/go-rod/rod"
)

// mockProvider is a no-browser stub for API tests.
type mockProvider struct {
	name    string
	modelID string
	reply   string
}

func (m *mockProvider) Name() string                             { return m.name }
func (m *mockProvider) ModelID() string                          { return m.modelID }
func (m *mockProvider) Init(_ *rod.Page, _ *config.Config) error { return nil }
func (m *mockProvider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	// Echo with tool-call simulation if marker present
	if text == "trigger_tool" || contains(text, "trigger_tool") {
		return &models.ProviderResponse{
			Message: `{"tool_calls": [{"name": "get_weather", "arguments": {"city": "Tokyo"}}]}`,
		}, nil
	}
	return &models.ProviderResponse{Message: "mock reply to: " + text, ThreadID: threadID}, nil
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
func (m *mockProvider) NewChat() error                   { return nil }
func (m *mockProvider) ExtractResponse() (string, error) { return m.reply, nil }
func (m *mockProvider) IsLoggedIn() (bool, error)        { return true, nil }

func testConfig() *config.Config {
	return &config.Config{
		Provider:         "chatgpt",
		APIToken:         "testtoken",
		APIHost:          "127.0.0.1",
		APIPort:          0,
		RateLimitSeconds: 0, // disable rate limit for tests (0 → 1/0? set to 1)
		BrowserDataDir:   "./browser_data",
		LogDir:           "./logs",
		LogLevel:         "error",
	}
}

func TestListModels(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}
	var resp models.ModelList
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "chimera-chatgpt" {
		t.Fatalf("unexpected model list: %+v", resp)
	}
}

func TestChatCompletionsAuth(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	cfg.APIToken = "secret"
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	body := `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	// missing auth
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// with token
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer secret")
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d body %s", w2.Code, w2.Body.String())
	}
}

func TestChatCompletionsNonStream(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	body := `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp models.ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body %s", err, w.Body.String())
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		t.Fatalf("empty choices: %+v", resp)
	}
	if resp.Model != "chimera-chatgpt" {
		t.Fatalf("model mismatch %q", resp.Model)
	}
}

func TestChatCompletionsToolCalls(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	body := `{
		"model":"chimera-chatgpt",
		"messages":[{"role":"user","content":"trigger_tool"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
	}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp models.ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %v", err)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q choices %+v", resp.Choices[0].FinishReason, resp.Choices[0])
	}
	if len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("expected tool_calls, got %+v", resp.Choices[0].Message)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("unexpected tool name %q", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}

func TestHealth(t *testing.T) {
	cfg := testConfig()
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health expected 200 got %d", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	// One completion so chat-request counters exist.
	body := `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hello"}]}`
	creq := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	creq.Header.Set("Content-Type", "application/json")
	creq.Header.Set("Authorization", "Bearer testtoken")
	cw := httptest.NewRecorder()
	srv.Router().ServeHTTP(cw, creq)
	if cw.Code != http.StatusOK {
		t.Fatalf("chat status %d body %s", cw.Code, cw.Body.String())
	}

	// /metrics is unauthenticated like /health.
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics expected 200 got %d", w.Code)
	}
	text := w.Body.String()
	for _, want := range []string{
		"chimera_chat_requests_total",
		"chimera_response_duration_seconds",
		"chimera_http_requests_total",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics exposition missing %q", want)
		}
	}
}

func TestProvidersHealth(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	req := httptest.NewRequest("GET", "/v1/health/providers", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("providers health expected 200 got %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Up       bool   `json:"up"`
			LoggedIn bool   `json:"logged_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body %s", err, w.Body.String())
	}
	if resp.Object != "provider_health" || len(resp.Data) != 1 {
		t.Fatalf("unexpected health payload: %+v", resp)
	}
	if resp.Data[0].Provider != "chatgpt" || !resp.Data[0].Up || !resp.Data[0].LoggedIn {
		t.Fatalf("expected chatgpt up+logged in: %+v", resp.Data[0])
	}
}

func chatBody(msg string) string {
	return `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"` + msg + `"}]}`
}

func doChat(t *testing.T, srv *Server, token, msg string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(chatBody(msg)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func testMeterConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig()
	cfg.RateLimitSeconds = 0 // no rate limiter interference
	cfg.MeterDB = t.TempDir() + "/usage.db"
	return cfg
}

func TestMultiKeyAuth(t *testing.T) {
	cfg := testMeterConfig(t)
	cfg.APIToken = "sek"
	cfg.APITokens = "acme=a1"
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	// Tenant key via x-api-key works and is isolated to its tenant.
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(chatBody("hi")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "a1")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant key expected 200 got %d body %s", w.Code, w.Body.String())
	}

	// Unknown key rejected.
	w2 := doChat(t, srv, "nope", "hi")
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("bad key expected 401 got %d", w2.Code)
	}

	// Usage is attributed per tenant.
	ureq := httptest.NewRequest("GET", "/v1/usage", nil)
	ureq.Header.Set("X-Api-Key", "a1")
	uw := httptest.NewRecorder()
	srv.Router().ServeHTTP(uw, ureq)
	if uw.Code != http.StatusOK {
		t.Fatalf("usage expected 200 got %d body %s", uw.Code, uw.Body.String())
	}
	var uresp struct {
		Tenant   string `json:"tenant"`
		Requests int    `json:"requests"`
	}
	if err := json.Unmarshal(uw.Body.Bytes(), &uresp); err != nil {
		t.Fatal(err)
	}
	if uresp.Tenant != "acme" || uresp.Requests != 1 {
		t.Fatalf("unexpected usage: %+v", uresp)
	}
}

func TestQuotaEnforced(t *testing.T) {
	cfg := testMeterConfig(t)
	cfg.QuotaMonthlyRequests = 1
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	if w := doChat(t, srv, "testtoken", "one"); w.Code != http.StatusOK {
		t.Fatalf("first request expected 200 got %d", w.Code)
	}
	w := doChat(t, srv, "testtoken", "two")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request expected 429 got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "quota_exceeded") {
		t.Fatalf("expected quota_exceeded body, got %s", w.Body.String())
	}
}

func TestUsageEndpoint(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	for _, msg := range []string{"a", "b"} {
		if w := doChat(t, srv, "testtoken", msg); w.Code != http.StatusOK {
			t.Fatalf("chat expected 200 got %d", w.Code)
		}
	}
	req := httptest.NewRequest("GET", "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("usage expected 200 got %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Object       string         `json:"object"`
		Tenant       string         `json:"tenant"`
		Requests     int            `json:"requests"`
		PromptChars  int64          `json:"prompt_chars"`
		ByProvider   map[string]int `json:"by_provider"`
		QuotaMonthly int            `json:"quota_monthly"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "usage" || resp.Tenant != "default" || resp.Requests != 2 {
		t.Fatalf("unexpected usage: %+v", resp)
	}
	if resp.ByProvider["chatgpt"] != 2 || resp.PromptChars <= 0 {
		t.Fatalf("unexpected breakdown: %+v", resp)
	}
}

func TestUsageMeteringDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 0
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p) // MeterDB "" → disabled

	req := httptest.NewRequest("GET", "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d", w.Code)
	}
}

func doResponses(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func TestResponsesTextTurn(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	w := doResponses(t, srv, "testtoken", `{"model":"chimera-chatgpt","input":"hello"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp models.ResponseObject
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	if resp.Model != "chimera-chatgpt" {
		t.Fatalf("model mismatch %q", resp.Model)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" {
		t.Fatalf("expected one message item: %+v", resp.Output)
	}
	if len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text == "" {
		t.Fatalf("empty message content: %+v", resp.Output[0])
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatalf("missing usage: %+v", resp.Usage)
	}
}

func TestResponsesInstructionsAndItems(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	body := `{"model":"chimera-chatgpt","instructions":"Be brief.","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","content":"skip me"}
	]}`
	w := doResponses(t, srv, "testtoken", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp models.ResponseObject
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" {
		t.Fatalf("unexpected output: %+v", resp.Output)
	}
}

func TestResponsesToolCall(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	body := `{"model":"chimera-chatgpt","input":"trigger_tool",
		"tools":[{"type":"function","name":"get_weather","description":"Get weather",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	w := doResponses(t, srv, "testtoken", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp models.ResponseObject
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("expected one function_call: %+v", resp.Output)
	}
	fc := resp.Output[0]
	if fc.Name != "get_weather" || fc.CallID == "" {
		t.Fatalf("bad function call: %+v", fc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(fc.Arguments), &args); err != nil || args["city"] != "Tokyo" {
		t.Fatalf("bad arguments %q", fc.Arguments)
	}
}

func TestResponsesFunctionCallOutput(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	// Follow-up turn carrying only a tool result, no new user message.
	body := `{"model":"chimera-chatgpt","input":[
		{"type":"function_call_output","call_id":"call_abc","output":"Sunny, 25C"}]}`
	w := doResponses(t, srv, "testtoken", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestResponsesRejected(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	cases := []struct {
		name, body, wantType string
		wantCode             int
	}{
		{"stream", `{"model":"chimera-chatgpt","input":"hi","stream":true}`, "streaming_unsupported", 400},
		{"empty", `{"model":"chimera-chatgpt"}`, "invalid_request", 400},
		{"bad input", `{"model":"chimera-chatgpt","input":42}`, "invalid_request", 400},
	}
	for _, c := range cases {
		w := doResponses(t, srv, "testtoken", c.body)
		if w.Code != c.wantCode {
			t.Fatalf("%s: status %d, want %d (%s)", c.name, w.Code, c.wantCode, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), c.wantType) {
			t.Fatalf("%s: body missing %q: %s", c.name, c.wantType, w.Body.String())
		}
	}

	// Unknown models fall back to the default provider (same as chat).
	w := doResponses(t, srv, "testtoken", `{"model":"nope","input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unknown model: status %d, want 200 fallback (%s)", w.Code, w.Body.String())
	}
	var fb models.ResponseObject
	if err := json.Unmarshal(w.Body.Bytes(), &fb); err != nil {
		t.Fatal(err)
	}
	if fb.Model != "chimera-chatgpt" {
		t.Fatalf("fallback model = %q, want chimera-chatgpt", fb.Model)
	}
}

func TestResponsesRecorded(t *testing.T) {
	cfg := testMeterConfig(t)
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)
	if srv.meter != nil {
		defer srv.meter.Close()
	}

	if w := doResponses(t, srv, "testtoken", `{"model":"chimera-chatgpt","input":"hi"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	n, err := srv.meter.MonthCount("default", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("month count = %d, want 1 (shared pipeline records responses turns)", n)
	}
}

// TestChatCompletionsStream guards the metrics middleware from dropping
// http.Flusher: the SSE path must return 200 text/event-stream with chunks and
// a terminating [DONE], not a 500 "streaming not supported".
func TestChatCompletionsStream(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	body := `{"model":"chimera-chatgpt","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	text := w.Body.String()
	if !strings.Contains(text, `"object":"chat.completion.chunk"`) {
		t.Fatalf("missing chunk payload: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] terminator: %s", text)
	}
}

// TestMetricsPathLabelBounded ensures arbitrary request paths can't create new
// Prometheus label values (cardinality DoS).
func TestMetricsPathLabelBounded(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &mockProvider{name: "chatgpt", modelID: "chimera-chatgpt"}
	srv := NewServer(cfg, p)

	randPath := "/v1/no-such-route-" + time.Now().Format("150405.000000")
	req := httptest.NewRequest("GET", randPath, nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", w.Code)
	}

	mw := httptest.NewRecorder()
	srv.Router().ServeHTTP(mw, httptest.NewRequest("GET", "/metrics", nil))
	text := mw.Body.String()
	if strings.Contains(text, "no-such-route") {
		t.Fatalf("raw path leaked into metrics labels")
	}
	if !strings.Contains(text, `path="other"`) {
		t.Fatalf("expected bounded path=\"other\" series, got: %s", text)
	}
}
