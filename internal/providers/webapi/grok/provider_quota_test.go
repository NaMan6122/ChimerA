package grokweb

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// testQuotaProvider builds a provider whose client points at srv. Init is
// deliberately bypassed (it pins BaseURL to grok.com); the quota cache is
// what is under test, and refreshQuota()/cacheQuota() are its writers.
func testQuotaProvider(t *testing.T, srvURL string) *Provider {
	t.Helper()
	cfg := &config.Config{LogDir: t.TempDir(), LogLevel: "error"}
	p := New(cfg)
	p.client = NewClient(
		&webapi.Session{AccessToken: "cookie-session", Cookies: map[string]string{"sso": "test"}},
		5*time.Second, "auto")
	p.client.BaseURL = srvURL
	return p
}

// TestQuotaCacheRoundTrip proves the full path: rate-limits endpoint ->
// cacheQuota -> Quota() -> providers.QuotaReporter shape, plus the
// keep-last-reading behavior when a later probe fails.
func TestQuotaCacheRoundTrip(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rateLimitsPath {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) <= 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remainingQueries":21,"totalQueries":30}`))
			return
		}
		// Every later probe fails: the cache must keep the last good reading.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := testQuotaProvider(t, srv.URL)

	if got := p.Quota(); got != nil {
		t.Fatalf("expected no quota before the first probe, got %v", got)
	}

	p.refreshQuota()
	q := p.Quota()
	if q == nil || q["remaining"] != 21 {
		t.Fatalf("expected cached remaining=21, got %v", q)
	}
	if _, has := q["total"]; has {
		t.Fatalf("totalQueries is not surfaced yet; map must carry remaining only, got %v", q)
	}

	p.refreshQuota() // now 500 — snapshot must survive
	if q := p.Quota(); q == nil || q["remaining"] != 21 {
		t.Fatalf("failed probe must keep the last reading, got %v", q)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 probe calls, got %d", calls.Load())
	}

	p.cacheQuota(0) // an explicit 0 is a real reading, not "unknown"
	if q := p.Quota(); q == nil || q["remaining"] != 0 {
		t.Fatalf("expected remaining=0 to round-trip, got %v", q)
	}
}
