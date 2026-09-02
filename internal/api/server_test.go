package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-rod/rod"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
)

// mockProvider is a no-browser stub for API tests.
type mockProvider struct {
	name    string
	modelID string
	reply   string
}

func (m *mockProvider) Name() string    { return m.name }
func (m *mockProvider) ModelID() string { return m.modelID }
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

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
})() }
func (m *mockProvider) NewChat() error                     { return nil }
func (m *mockProvider) ExtractResponse() (string, error)   { return m.reply, nil }
func (m *mockProvider) IsLoggedIn() (bool, error)          { return true, nil }

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
