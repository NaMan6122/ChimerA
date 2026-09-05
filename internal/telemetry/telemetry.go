// Package telemetry exposes Prometheus metrics and per-provider health state.
//
// The package owns a dedicated registry (no global prometheus side effects)
// so tests and embedded use never clash with other registrants.
// Use the package-level Default instance via the Observe*/Record* helpers.
package telemetry

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Buckets tuned for real-browser round trips (5–30s typical).
var (
	responseDurationBuckets = []float64{1, 2.5, 5, 10, 15, 20, 30, 45, 60, 120, 300}
	lockWaitBuckets         = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	httpDurationBuckets     = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
)

// ProviderCheck is the last login-probe result for one provider.
type ProviderCheck struct {
	LoggedIn bool
	At       time.Time
	Err      string
}

// ProviderHealth is the JSON snapshot served by /v1/health/providers.
type ProviderHealth struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Up          bool   `json:"up"`
	LoggedIn    bool   `json:"logged_in"`
	LastSuccess *int64 `json:"last_success_unix,omitempty"`
	LastCheck   *int64 `json:"last_check_unix,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// Metrics holds the registry, vectors, and provider state.
type Metrics struct {
	reg *prometheus.Registry

	ChatRequests     *prometheus.CounterVec
	ResponseDuration *prometheus.HistogramVec
	ProviderErrors   *prometheus.CounterVec
	LockWait         *prometheus.HistogramVec
	SelectorFallback *prometheus.CounterVec
	EchoRetry        *prometheus.CounterVec
	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	ProviderUp       *prometheus.GaugeVec
	ProviderLastOK   *prometheus.GaugeVec

	mu          sync.Mutex
	lastSuccess map[string]time.Time
	lastCheck   map[string]ProviderCheck
}

// New builds a fully-registered Metrics instance on its own registry.
func New() *Metrics {
	m := &Metrics{
		reg:         prometheus.NewRegistry(),
		lastSuccess: make(map[string]time.Time),
		lastCheck:   make(map[string]ProviderCheck),
	}
	m.ChatRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chimera_chat_requests_total",
		Help: "Chat completion outcomes by provider, model and HTTP code.",
	}, []string{"provider", "model", "code"})
	m.ResponseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chimera_response_duration_seconds",
		Help:    "Provider SendMessage round-trip time.",
		Buckets: responseDurationBuckets,
	}, []string{"provider", "streaming"})
	m.ProviderErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chimera_provider_errors_total",
		Help: "Provider failures by reason (send_error, lock_timeout, ...).",
	}, []string{"provider", "reason"})
	m.LockWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chimera_lock_wait_seconds",
		Help:    "Time spent waiting to acquire a provider lock.",
		Buckets: lockWaitBuckets,
	}, []string{"provider"})
	m.SelectorFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chimera_selector_fallback_total",
		Help: "Times the first-choice selector missed and a fallback hit. Spikes mean a vendor UI changed.",
	}, []string{"provider"})
	m.EchoRetry = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chimera_echo_retry_total",
		Help: "Echo-detection re-extractions (response contained the prompt).",
	}, []string{"provider"})
	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chimera_http_requests_total",
		Help: "HTTP responses by method, path and code.",
	}, []string{"method", "path", "code"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chimera_http_duration_seconds",
		Help:    "HTTP handler latency.",
		Buckets: httpDurationBuckets,
	}, []string{"method", "path"})
	m.ProviderUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "chimera_provider_up",
		Help: "1 when the last login probe for the provider succeeded, else 0.",
	}, []string{"provider"})
	m.ProviderLastOK = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "chimera_provider_last_success_timestamp",
		Help: "Unix time of the last successful provider response.",
	}, []string{"provider"})
	m.reg.MustRegister(
		m.ChatRequests, m.ResponseDuration, m.ProviderErrors, m.LockWait,
		m.SelectorFallback, m.EchoRetry, m.HTTPRequests, m.HTTPDuration,
		m.ProviderUp, m.ProviderLastOK,
	)
	return m
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RecordLoginCheck stores a login-probe result and flips the up gauge.
func (m *Metrics) RecordLoginCheck(provider string, loggedIn bool, errStr string) {
	m.mu.Lock()
	m.lastCheck[provider] = ProviderCheck{LoggedIn: loggedIn, At: time.Now(), Err: errStr}
	m.mu.Unlock()
	if loggedIn {
		m.ProviderUp.WithLabelValues(provider).Set(1)
	} else {
		m.ProviderUp.WithLabelValues(provider).Set(0)
	}
}

// RecordSuccess stores last-success state for health snapshots.
func (m *Metrics) RecordSuccess(provider string) {
	now := time.Now()
	m.mu.Lock()
	m.lastSuccess[provider] = now
	m.mu.Unlock()
	m.ProviderLastOK.WithLabelValues(provider).Set(float64(now.Unix()))
}

// Snapshot builds health entries for the given providers (name -> model).
func (m *Metrics) Snapshot(providers map[string]string) []ProviderHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProviderHealth, 0, len(providers))
	for name, model := range providers {
		h := ProviderHealth{Provider: name, Model: model}
		if ts, ok := m.lastSuccess[name]; ok {
			u := ts.Unix()
			h.LastSuccess = &u
		}
		if c, ok := m.lastCheck[name]; ok {
			u := c.At.Unix()
			h.LastCheck = &u
			h.LoggedIn = c.LoggedIn
			h.LastError = c.Err
			h.Up = c.LoggedIn
		}
		out = append(out, h)
	}
	return out
}

// Default is the process-wide instance used by the gateway.
var Default = New()

// Handler exposes Default in Prometheus format.
func Handler() http.Handler { return Default.Handler() }

// ObserveChatRequest records one chat completion outcome (code like "200", "400", "500").
func ObserveChatRequest(provider, model, code string) {
	Default.ChatRequests.WithLabelValues(provider, model, code).Inc()
}

// ObserveResponseDuration records a SendMessage round trip.
func ObserveResponseDuration(provider string, streaming bool, d time.Duration) {
	s := "false"
	if streaming {
		s = "true"
	}
	Default.ResponseDuration.WithLabelValues(provider, s).Observe(d.Seconds())
}

// ObserveProviderError counts a provider failure (reason: send_error, lock_timeout, ...).
func ObserveProviderError(provider, reason string) {
	Default.ProviderErrors.WithLabelValues(provider, reason).Inc()
}

// ObserveLockWait records time spent acquiring a provider lock.
func ObserveLockWait(provider string, d time.Duration) {
	Default.LockWait.WithLabelValues(provider).Observe(d.Seconds())
}

// ObserveSelectorFallback counts a first-choice selector miss.
func ObserveSelectorFallback(provider string) {
	Default.SelectorFallback.WithLabelValues(provider).Inc()
}

// ObserveEchoRetry counts an echo-detection re-extraction.
func ObserveEchoRetry(provider string) {
	Default.EchoRetry.WithLabelValues(provider).Inc()
}

// ObserveHTTP records one HTTP response.
func ObserveHTTP(method, path, code string, d time.Duration) {
	Default.HTTPRequests.WithLabelValues(method, path, code).Inc()
	Default.HTTPDuration.WithLabelValues(method, path).Observe(d.Seconds())
}

// RecordLoginCheck stores a login-probe result on Default.
func RecordLoginCheck(provider string, loggedIn bool, errStr string) {
	Default.RecordLoginCheck(provider, loggedIn, errStr)
}

// RecordSuccess stores last-success state on Default.
func RecordSuccess(provider string) {
	Default.RecordSuccess(provider)
}

// Snapshot builds health entries from Default for name -> model.
func Snapshot(providers map[string]string) []ProviderHealth {
	return Default.Snapshot(providers)
}
