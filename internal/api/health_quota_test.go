package api

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/go-rod/rod"
)

// Self-contained stubs: these tests must not depend on server_test.go's
// mockProvider, and the plain variant must NOT implement QuotaReporter so
// the "quota only for reporters" contract is exercised both ways.

type healthQuotaProvider struct {
	name    string
	modelID string
	quota   map[string]any
}

func (p *healthQuotaProvider) Name() string                             { return p.name }
func (p *healthQuotaProvider) ModelID() string                          { return p.modelID }
func (p *healthQuotaProvider) Init(_ *rod.Page, _ *config.Config) error { return nil }
func (p *healthQuotaProvider) SendMessage(_ string, threadID string) (*models.ProviderResponse, error) {
	return &models.ProviderResponse{Message: "ok", ThreadID: threadID}, nil
}
func (p *healthQuotaProvider) NewChat() error                   { return nil }
func (p *healthQuotaProvider) ExtractResponse() (string, error) { return "ok", nil }
func (p *healthQuotaProvider) IsLoggedIn() (bool, error)        { return true, nil }
func (p *healthQuotaProvider) Quota() map[string]any            { return p.quota }

type healthPlainProvider struct {
	name    string
	modelID string
}

func (p *healthPlainProvider) Name() string                             { return p.name }
func (p *healthPlainProvider) ModelID() string                          { return p.modelID }
func (p *healthPlainProvider) Init(_ *rod.Page, _ *config.Config) error { return nil }
func (p *healthPlainProvider) SendMessage(_ string, threadID string) (*models.ProviderResponse, error) {
	return &models.ProviderResponse{Message: "ok", ThreadID: threadID}, nil
}
func (p *healthPlainProvider) NewChat() error                   { return nil }
func (p *healthPlainProvider) ExtractResponse() (string, error) { return "ok", nil }
func (p *healthPlainProvider) IsLoggedIn() (bool, error)        { return true, nil }

// TestProvidersHealthQuotaOnlyForReporters: the health snapshot carries a
// quota object exactly for providers implementing the optional interface —
// and the field is absent (not null, not empty) for everyone else.
func TestProvidersHealthQuotaOnlyForReporters(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	grok := &healthQuotaProvider{name: "grok", modelID: "chimera-grok",
		quota: map[string]any{"remaining": 21, "total": 30, "window": "5h"}}
	chatgpt := &healthPlainProvider{name: "chatgpt", modelID: "chimera-chatgpt"}

	srv := NewServer(cfg, grok)
	srv.providers["chatgpt"] = chatgpt
	srv.mus["chatgpt"] = &sync.Mutex{}

	req := httptest.NewRequest("GET", "/v1/health/providers", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body %s", err, w.Body.String())
	}
	byName := map[string]map[string]any{}
	for _, e := range resp.Data {
		name, _ := e["provider"].(string)
		byName[name] = e
	}

	qraw, ok := byName["grok"]["quota"]
	if !ok {
		t.Fatalf("grok implements QuotaReporter — quota key missing: %v", byName["grok"])
	}
	q, ok := qraw.(map[string]any)
	if !ok {
		t.Fatalf("quota should decode as an object, got %T", qraw)
	}
	if q["remaining"].(float64) != 21 || q["total"].(float64) != 30 || q["window"] != "5h" {
		t.Fatalf("unexpected quota payload: %v", q)
	}

	if _, ok := byName["chatgpt"]["quota"]; ok {
		t.Fatalf("plain provider must not carry a quota key: %v", byName["chatgpt"])
	}
}

// TestProvidersHealthQuotaNilOmitted: a reporter with nothing cached must
// not emit a quota object at all (QuotaFromMap(nil) -> omitted).
func TestProvidersHealthQuotaNilOmitted(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitSeconds = 10
	p := &healthQuotaProvider{name: "grok", modelID: "chimera-grok", quota: nil}
	srv := NewServer(cfg, p)

	req := httptest.NewRequest("GET", "/v1/health/providers", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Data))
	}
	if _, ok := resp.Data[0]["quota"]; ok {
		t.Fatalf("quota must be omitted when nothing is cached: %v", resp.Data[0])
	}
}
