package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCountersAndHistograms(t *testing.T) {
	m := New()

	m.ChatRequests.WithLabelValues("chatgpt", "chimera-chatgpt", "200").Inc()
	if got := testutil.ToFloat64(m.ChatRequests.WithLabelValues("chatgpt", "chimera-chatgpt", "200")); got != 1 {
		t.Fatalf("requests = %v, want 1", got)
	}

	m.ResponseDuration.WithLabelValues("chatgpt", "false").Observe(7.5)
	count := testutil.CollectAndCount(m.ResponseDuration, "chimera_response_duration_seconds")
	if count == 0 {
		t.Fatal("expected response duration observations")
	}

	m.ProviderErrors.WithLabelValues("qwen", "send_error").Inc()
	if got := testutil.ToFloat64(m.ProviderErrors.WithLabelValues("qwen", "send_error")); got != 1 {
		t.Fatalf("errors = %v, want 1", got)
	}

	m.SelectorFallback.WithLabelValues("chatgpt").Inc()
	m.EchoRetry.WithLabelValues("chatgpt").Inc()
	if got := testutil.ToFloat64(m.SelectorFallback.WithLabelValues("chatgpt")); got != 1 {
		t.Fatalf("fallbacks = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EchoRetry.WithLabelValues("chatgpt")); got != 1 {
		t.Fatalf("echo retries = %v, want 1", got)
	}
}

func TestHandlerExposesMetrics(t *testing.T) {
	m := New()
	m.ChatRequests.WithLabelValues("chatgpt", "chimera-chatgpt", "200").Inc()
	m.ResponseDuration.WithLabelValues("chatgpt", "false").Observe(7.5)
	m.RecordLoginCheck("chatgpt", true, "")

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"chimera_chat_requests_total", "chimera_response_duration_seconds", "chimera_provider_up"} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition missing %q", want)
		}
	}
}

func TestLoginCheckAndSnapshot(t *testing.T) {
	m := New()
	m.RecordLoginCheck("chatgpt", true, "")
	m.RecordSuccess("chatgpt")
	m.RecordLoginCheck("qwen", false, "not logged in")

	snap := m.Snapshot(map[string]string{"chatgpt": "chimera-chatgpt", "qwen": "chimera-qwen"})
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	byName := map[string]ProviderHealth{}
	for _, h := range snap {
		byName[h.Provider] = h
	}
	if !byName["chatgpt"].Up || !byName["chatgpt"].LoggedIn {
		t.Fatalf("chatgpt should be up: %+v", byName["chatgpt"])
	}
	if byName["chatgpt"].LastSuccess == nil {
		t.Fatal("chatgpt should have last success")
	}
	if byName["qwen"].Up || byName["qwen"].LastError == "" {
		t.Fatalf("qwen should be down with error: %+v", byName["qwen"])
	}
	if got := testutil.ToFloat64(m.ProviderUp.WithLabelValues("chatgpt")); got != 1 {
		t.Fatalf("up gauge = %v, want 1", got)
	}
	_ = time.Now // keep time import if unused in future edits
}
