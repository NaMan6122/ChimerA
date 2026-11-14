package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/api"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/go-rod/rod"
)

const (
	testProvAlpha   = "ztestalpha"
	testProvBeta    = "ztestbeta"
	testProvExpired = "ztestexpired"
)

type fakePooledWebProvider struct {
	name     string
	delay    time.Duration
	seenCfgP string
}

func (f *fakePooledWebProvider) Name() string        { return f.name }
func (f *fakePooledWebProvider) ModelID() string     { return "chimera-" + f.name }
func (f *fakePooledWebProvider) Browserless() bool   { return true }
func (f *fakePooledWebProvider) NewChat() error      { return nil }
func (f *fakePooledWebProvider) IsLoggedIn() (bool, error) { return true, nil }
func (f *fakePooledWebProvider) ExtractResponse() (string, error) { return "", nil }

func (f *fakePooledWebProvider) Init(_ *rod.Page, cfg *config.Config) error {
	f.seenCfgP = cfg.Provider
	sess, err := webapi.LoadSession(cfg.SessionPath(f.name))
	if err != nil {
		return err
	}
	if sess.AccessToken == "expired" {
		return errors.New("token rejected by fake upstream")
	}
	return nil
}

func (f *fakePooledWebProvider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return &models.ProviderResponse{
		Message:  f.name + ":" + text,
		ThreadID: threadID,
	}, nil
}

var registerFakePoolOnce sync.Once

func ensureFakePoolRegistered() {
	registerFakePoolOnce.Do(func() {
		webapi.Register(testProvAlpha, func(_ *config.Config) providers.Provider {
			return &fakePooledWebProvider{name: testProvAlpha, delay: 80 * time.Millisecond}
		})
		webapi.Register(testProvBeta, func(_ *config.Config) providers.Provider {
			return &fakePooledWebProvider{name: testProvBeta, delay: 80 * time.Millisecond}
		})
		webapi.Register(testProvExpired, func(_ *config.Config) providers.Provider {
			return &fakePooledWebProvider{name: testProvExpired}
		})
	})
}

func writeValidSession(t *testing.T, authDir, name, token string) {
	t.Helper()
	err := webapi.WriteSession(&webapi.Session{
		AccessToken: token,
		Cookies:     map[string]string{"sid": "ok"},
	}, filepath.Join(authDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestInitPooledProvidersBrowserlessWithoutChrome(t *testing.T) {
	ensureFakePoolRegistered()
	t.Setenv("CHROME_BIN", "/nonexistent/chromium")

	root := t.TempDir()
	authDir := filepath.Join(root, "auth_data")
	writeValidSession(t, authDir, testProvAlpha, "live-token-a")
	writeValidSession(t, authDir, testProvBeta, "live-token-b")
	// Add an expired session + a corrupt JSON session: neither may prevent alpha & beta from serving.
	writeValidSession(t, authDir, testProvExpired, "expired")
	if err := os.WriteFile(filepath.Join(authDir, config.ProviderQwen+".json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, transport := range []string{config.TransportWebAPI, config.TransportAuto} {
		cfg := &config.Config{
			Provider:       config.ProviderAll,
			Transport:      transport,
			AuthDir:        authDir,
			BrowserDataDir: filepath.Join(root, "browser_data"),
			LogDir:         filepath.Join(root, "logs"),
		}
		ps, err := initPooledProviders(cfg)
		if err != nil {
			t.Fatalf("transport=%s initPooledProviders failed: %v", transport, err)
		}
		if ps.pool != nil || ps.browser != nil {
			t.Fatalf("transport=%s expected nil Chromium pool/browser", transport)
		}
		if cfg.Provider != config.ProviderAll {
			t.Fatalf("caller cfg.Provider mutated to %q", cfg.Provider)
		}
		if len(ps.providers) != 2 {
			t.Fatalf("transport=%s expected 2 healthy providers, got %v", transport, sortedNames(ps.providers))
		}
		for _, n := range []string{testProvAlpha, testProvBeta} {
			fp, ok := ps.providers[n].(*fakePooledWebProvider)
			if !ok {
				t.Fatalf("missing provider %s", n)
			}
			if fp.seenCfgP != n {
				t.Fatalf("provider %s saw cfg.Provider=%q, want %q", n, fp.seenCfgP, n)
			}
			if ps.mutexes[n] == nil {
				t.Fatalf("provider %s missing mutex", n)
			}
		}
		if ps.mutexes[testProvAlpha] == ps.mutexes[testProvBeta] {
			t.Fatal("distinct providers must not share the same mutex pointer")
		}
	}
}

func TestInitPooledProvidersEmptyWebAPIFailsWithActionableHint(t *testing.T) {
	ensureFakePoolRegistered()
	t.Setenv("CHROME_BIN", "/nonexistent/chromium")

	root := t.TempDir()
	cfg := &config.Config{
		Provider:  config.ProviderAll,
		Transport: config.TransportWebAPI,
		AuthDir:   filepath.Join(root, "empty_auth"),
	}
	_, err := initPooledProviders(cfg)
	if err == nil {
		t.Fatal("expected error when TRANSPORT=webapi finds 0 sessions")
	}
	if !strings.Contains(err.Error(), "chimera auth login") {
		t.Fatalf("expected actionable hint in error, got: %v", err)
	}
}

func TestPooledBrowserlessServerConcurrentCrossProviderTurns(t *testing.T) {
	ensureFakePoolRegistered()

	root := t.TempDir()
	authDir := filepath.Join(root, "auth_data")
	writeValidSession(t, authDir, testProvAlpha, "live-token-a")
	writeValidSession(t, authDir, testProvBeta, "live-token-b")

	cfg := &config.Config{
		Provider:         config.ProviderAll,
		Transport:        config.TransportAuto,
		AuthDir:          authDir,
		BrowserDataDir:   filepath.Join(root, "browser_data"),
		LogDir:           filepath.Join(root, "logs"),
		APIToken:         "sek",
		RateLimitSeconds: 0,
		ResponseTimeout:  5 * time.Second,
	}
	ps, err := initPooledProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewPooledServer(cfg, ps.providers, ps.mutexes, ps.browser)

	// 1. /v1/models lists both browserless models.
	mreq := httptest.NewRequest("GET", "/v1/models", nil)
	mreq.Header.Set("Authorization", "Bearer sek")
	mw := httptest.NewRecorder()
	srv.Router().ServeHTTP(mw, mreq)
	if mw.Code != http.StatusOK {
		t.Fatalf("/v1/models code=%d body=%s", mw.Code, mw.Body.String())
	}
	var list models.ModelList
	if err := json.Unmarshal(mw.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("expected 2 models, got %+v", list.Data)
	}

	// 2. Concurrent requests to Alpha (80ms) and Beta (80ms) run in parallel (<145ms wall).
	start := time.Now()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	call := func(idx int, model string) {
		defer wg.Done()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sek")
		req.Header.Set("X-Session-Id", "sess-"+model)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		codes[idx] = w.Code
	}
	wg.Add(2)
	go call(0, "chimera-"+testProvAlpha)
	go call(1, "chimera-"+testProvBeta)
	wg.Wait()
	elapsed := time.Since(start)

	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("expected 200/200, got %v", codes)
	}
	if elapsed >= 155*time.Millisecond {
		t.Fatalf("cross-provider turns serialized (%v >= 155ms); expected parallel execution", elapsed)
	}
}
