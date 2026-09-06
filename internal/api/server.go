// Package api provides the OpenAI-compatible HTTP server.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/auth"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/meter"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/session"
	"github.com/chimera/chimera/internal/telemetry"
	"github.com/chimera/chimera/internal/tools"
	"golang.org/x/time/rate"
)

var log_ = logging.New("api", "./logs", "debug", true)

// Server holds the HTTP server and dependencies.
// Supports both single-provider and pooled (single Chromium, page-per-provider) modes.
type Server struct {
	cfg        *config.Config
	router     *chi.Mux
	provider   providers.Provider             // single-provider mode (legacy)
	providers  map[string]providers.Provider // pooled mode: provider name -> Provider
	mus        map[string]*sync.Mutex        // pooled mode: per-provider mutex
	sessionMgr *session.Manager               // X-Session-Id continuity
	browser    *rod.Browser                   // for session pages
	limiter    *rate.Limiter
	keys       *auth.Keys   // credential -> tenant registry
	meter      *meter.Store // usage recording, nil when METER_DB=""
	mu         sync.Mutex // serializes browser access for single mode
}

// keysForConfig returns the parsed registry, building one from raw tokens
// for hand-constructed configs (tests) that skip config.Load.
func keysForConfig(cfg *config.Config) *auth.Keys {
	if cfg != nil && cfg.APIKeys != nil {
		return cfg.APIKeys
	}
	token, extra := "", ""
	if cfg != nil {
		token, extra = cfg.APIToken, cfg.APITokens
	}
	keys, err := auth.New(token, extra)
	if err != nil {
		// Single raw token never fails parsing; malformed extras fail open
		// here but config.Load refuses to start. Log loudly.
		log_.Errorf("Bad API tokens, starting with primary only: %v", err)
		keys, _ = auth.New(token, "")
	}
	return keys
}

// openMeter opens usage recording unless METER_DB is empty.
// Fail-open with a loud log: a bad path must not take the gateway down,
// but billing gaps are always worth shouting about.
func openMeter(cfg *config.Config) *meter.Store {
	if cfg == nil || strings.TrimSpace(cfg.MeterDB) == "" {
		return nil
	}
	st, err := meter.Open(cfg.MeterDB)
	if err != nil {
		log_.Errorf("Metering disabled, cannot open %q: %v", cfg.MeterDB, err)
		return nil
	}
	return st
}

// NewServer creates a new API server for single provider.
func NewServer(cfg *config.Config, provider providers.Provider) *Server {
	s := &Server{
		cfg:       cfg,
		provider:  provider,
		providers: map[string]providers.Provider{provider.Name(): provider},
		mus:       map[string]*sync.Mutex{provider.Name(): &sync.Mutex{}},
		limiter:   newRateLimiter(cfg),
		keys:      keysForConfig(cfg),
		meter:     openMeter(cfg),
	}

	s.router = chi.NewRouter()
	s.setupMiddleware()
	s.setupRoutes()

	return s
}

// NewPooledServer creates a pooled server that routes by model to the correct provider.
// poolProviders is map from provider name to Provider, mus is per-provider mutex from browser.Pool.
func NewPooledServer(cfg *config.Config, poolProviders map[string]providers.Provider, poolMus map[string]*sync.Mutex, browser *rod.Browser) *Server {
	s := &Server{
		cfg:        cfg,
		providers:  poolProviders,
		mus:        poolMus,
		browser:    browser,
		limiter:    newRateLimiter(cfg),
		keys:       keysForConfig(cfg),
		meter:      openMeter(cfg),
		sessionMgr: session.New(browser, 10),
	}
	// Set default single provider for fallback (prefer chatgpt if present)
	if p, ok := poolProviders[config.ProviderChatGPT]; ok {
		s.provider = p
	} else {
		for _, p := range poolProviders {
			s.provider = p
			break
		}
	}

	s.router = chi.NewRouter()
	s.setupMiddleware()
	s.setupRoutes()

	return s
}

// newRateLimiter builds the global token bucket. RateLimitSeconds<=0 disables limiting.
func newRateLimiter(cfg *config.Config) *rate.Limiter {
	if cfg == nil || cfg.RateLimitSeconds <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(1.0/float64(cfg.RateLimitSeconds)), 1)
}

// providerForModel resolves a model ID to a provider and its mutex.
// Supports "chimera-chatgpt", "chimera-qwen", "chatgpt", etc. Falls back to default.
// Fallback order is deterministic: default provider, then chatgpt, then sorted names.
func (s *Server) providerForModel(model string) (providers.Provider, *sync.Mutex, string) {
	// Direct match on ModelID
	for _, p := range s.providers {
		if p.ModelID() == model {
			name := p.Name()
			return p, s.mus[name], name
		}
	}
	// Match by provider name contained in model
	lower := strings.ToLower(model)
	for name, p := range s.providers {
		if strings.Contains(lower, name) {
			return p, s.mus[name], name
		}
	}
	// Fallback to single/default
	if s.provider != nil {
		name := s.provider.Name()
		mu := s.mus[name]
		if mu == nil {
			mu = &s.mu
		}
		return s.provider, mu, name
	}
	// Last resort: deterministic first pooled (prefer chatgpt, else sorted)
	if p, ok := s.providers[config.ProviderChatGPT]; ok {
		return p, s.mus[config.ProviderChatGPT], config.ProviderChatGPT
	}
	names := s.providerNames()
	if len(names) > 0 {
		// providerNames is sorted; pick first for determinism
		name := names[0]
		return s.providers[name], s.mus[name], name
	}
	return nil, &s.mu, ""
}

// setupMiddleware configures HTTP middleware.
func (s *Server) setupMiddleware() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Logger)
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.Heartbeat("/health"))
	s.router.Use(s.metricsMiddleware)
	s.router.Use(s.corsMiddleware)
	s.router.Use(s.authMiddleware)
	s.router.Use(s.rateLimitMiddleware)
}

// isOpenPath reports paths that skip auth and rate limiting.
func isOpenPath(path string) bool {
	return path == "/health" || path == "/" || path == "/metrics"
}

// setupRoutes registers API routes.
func (s *Server) setupRoutes() {
	s.router.Route("/v1", func(r chi.Router) {
		r.Get("/models", s.handleListModels)
		r.Get("/health/providers", s.handleProvidersHealth)
		r.Get("/usage", s.handleUsage)
		r.Post("/chat/completions", s.handleChatCompletions)
		r.Post("/responses", s.handleResponses)
		r.Post("/refresh", s.handleRefresh)
		r.Get("/refresh", s.handleRefresh)
		r.Post("/new_chat", s.handleRefresh)
		r.Get("/new_chat", s.handleRefresh)
	})

	// Prometheus exposition (unauthenticated like /health; front with a
	// reverse proxy or firewall in multi-tenant deploys).
	s.router.Get("/metrics", telemetry.Handler().ServeHTTP)

	// Root status
	s.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		providerInfo := s.cfg.Provider
		if s.isPooled() {
			providerInfo = "pool:" + strings.Join(s.providerNames(), ",")
		}
		json.NewEncoder(w).Encode(map[string]string{
			"name":     "Chimera Gateway",
			"version":  "0.1.0",
			"status":   "running",
			"provider": providerInfo,
		})
	})
}

func (s *Server) isPooled() bool { return len(s.providers) > 1 }

func (s *Server) providerNames() []string {
	names := make([]string, 0, len(s.providers))
	for k := range s.providers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// corsMiddleware provides permissive CORS (mirrors Chimera's CORSMiddleware).
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Api-Key, X-Session-Id, X-Thread-Id, Anthropic-Api-Key")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSONError writes a consistent OpenAI-style JSON error.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    code,
			"message": message,
		},
	})
}

// tenantOf resolves the caller tenant ("default" for single-key or open gateways).
func (s *Server) tenantOf(r *http.Request) string {
	if s.keys != nil {
		if t, ok := s.keys.Authenticate(r); ok && t != "" {
			return t
		}
	}
	return auth.DefaultTenant
}

// quotaExceeded reports whether the tenant exhausted its monthly request quota.
// Fail-open on store errors (logged): metering must not 500 live traffic.
func (s *Server) quotaExceeded(tenant string) bool {
	limit := 0
	if s.cfg != nil {
		limit = s.cfg.QuotaMonthlyRequests
	}
	if limit <= 0 || s.meter == nil {
		return false
	}
	used, err := s.meter.MonthCount(tenant, time.Now())
	if err != nil {
		log_.Errorf("Quota check failed for tenant %q: %v", tenant, err)
		return false
	}
	return used >= limit
}

// recordUsage stores one provider attempt; no-op when metering is disabled.
func (s *Server) recordUsage(tenant, provider, model string, streaming bool, promptChars, completionChars int, latency time.Duration, code int) {
	if s.meter == nil {
		return
	}
	if err := s.meter.Record(meter.Record{
		TS: time.Now(), Tenant: tenant, Provider: provider, Model: model,
		Streaming: streaming, PromptChars: promptChars, CompletionChars: completionChars,
		LatencyMs: latency.Milliseconds(), Code: code,
	}); err != nil {
		log_.Errorf("Usage record failed for tenant %q: %v", tenant, err)
	}
}

// authMiddleware validates token if configured.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks, root, metrics, and CORS preflight
		if isOpenPath(r.URL.Path) || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if _, ok := s.keys.Authenticate(r); !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware applies global rate limiting (nil limiter = disabled).
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpenPath(r.URL.Path) || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if s.limiter != nil && !s.limiter.Allow() {
			writeJSONError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded, try again later")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response code for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware records per-request HTTP metrics.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		telemetry.ObserveHTTP(r.Method, r.URL.Path, strconv.Itoa(rec.status), time.Since(start))
	})
}
// acquireProviderLock locks mu with timeout + client-disconnect awareness.
// Prevents a hung browser from hanging HTTP forever (returns false on timeout/cancel).
func acquireProviderLock(ctx context.Context, mu *sync.Mutex, timeout time.Duration) bool {
	if mu == nil {
		return true
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if mu.TryLock() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Router returns the HTTP handler/mux.
func (s *Server) Router() http.Handler {
	return s.router
}

// ── Handlers ──────────────────────────────────────────────────

// handleListModels returns available models.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	var data []models.ModelObject
	if s.isPooled() {
		for _, name := range s.providerNames() {
			p := s.providers[name]
			data = append(data, models.ModelObject{
				ID:      p.ModelID(),
				Object:  "model",
				OwnedBy: p.Name(),
			})
		}
	} else {
		data = []models.ModelObject{
			{
				ID:      s.provider.ModelID(),
				Object:  "model",
				OwnedBy: s.provider.Name(),
			},
		}
	}
	resp := models.ModelList{
		Object: "list",
		Data:   data,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleProvidersHealth probes each provider's login state in parallel and
// returns a snapshot for status pages and alerting. Slow probes time out
// individually (overall budget 15s) and keep their previous state.
func (s *Server) handleProvidersHealth(w http.ResponseWriter, r *http.Request) {
	names := s.providerNames()
	type probe struct {
		loggedIn bool
		errStr   string
		done     bool
	}
	results := make([]probe, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			p, ok := s.providers[name]
			if !ok || p == nil {
				results[i] = probe{errStr: "provider not registered", done: true}
				return
			}
			ok, err := p.IsLoggedIn()
			pr := probe{loggedIn: ok, done: true}
			if err != nil {
				pr.errStr = err.Error()
			}
			results[i] = pr
		}(i, name)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	case <-r.Context().Done():
		return
	}
	modelsByName := make(map[string]string, len(names))
	for _, name := range names {
		if p, ok := s.providers[name]; ok && p != nil {
			modelsByName[name] = p.ModelID()
		} else {
			modelsByName[name] = ""
		}
	}
	for i, name := range names {
		if results[i].done {
			telemetry.RecordLoginCheck(name, results[i].loggedIn, results[i].errStr)
		} else {
			results[i].errStr = "probe timeout"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "provider_health",
		"data":   telemetry.Snapshot(modelsByName),
	})
}

// handleUsage returns the caller's usage summary over the inclusive window
// [from, to]. Query params from/to are RFC3339 (default: current UTC month).
// Callers only ever see their own tenant.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "metering_disabled", "usage metering is disabled (METER_DB empty)")
		return
	}
	tenant := s.tenantOf(r)
	now := time.Now().UTC()
	from := now
	{
		y, m, _ := now.Date()
		from = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	}
	to := now
	if v := strings.TrimSpace(r.URL.Query().Get("from")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "bad from timestamp, want RFC3339")
			return
		}
		from = t
	}
	if v := strings.TrimSpace(r.URL.Query().Get("to")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "bad to timestamp, want RFC3339")
			return
		}
		to = t
	}
	if !to.After(from) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "to must be after from")
		return
	}
	u, err := s.meter.Summarize(tenant, from, to)
	if err != nil {
		log_.Errorf("Usage summarize failed for tenant %q: %v", tenant, err)
		writeJSONError(w, http.StatusInternalServerError, "provider_error", "usage lookup failed")
		return
	}
	limit := 0
	if s.cfg != nil {
		limit = s.cfg.QuotaMonthlyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object":          "usage",
		"tenant":          tenant,
		"from":            from.Format(time.RFC3339),
		"to":              to.Format(time.RFC3339),
		"requests":        u.Requests,
		"prompt_chars":    u.PromptChars,
		"completion_chars": u.CompletionChars,
		"errors":          u.Errors,
		"by_provider":     u.ByProvider,
		"quota_monthly":   limit,
	})
}

// doProviderTurn runs one serialized provider attempt: lock with timeout,
// SendMessage, telemetry, usage recording, and session-URL persistence.
// Shared by chat and responses handlers so outcomes can never drift.
// Returns the assistant text on success (code 200); otherwise the HTTP
// code + error type/message for writeJSONError.
func (s *Server) doProviderTurn(r *http.Request, alog *logging.Logger, provider providers.Provider, mu *sync.Mutex, providerName, model, tenant, prompt, threadID, sessionID string) (string, int, string, string) {
	if mu == nil {
		mu = &s.mu
	}
	lockStart := time.Now()
	if !acquireProviderLock(r.Context(), mu, s.cfg.ResponseTimeout+30*time.Second) {
		telemetry.ObserveLockWait(providerName, time.Since(lockStart))
		telemetry.ObserveProviderError(providerName, "lock_timeout")
		if r.Context().Err() != nil {
			telemetry.ObserveChatRequest(providerName, model, "499")
			return "", 499, "client_closed", "client disconnected while waiting for provider lock"
		}
		telemetry.ObserveChatRequest(providerName, model, "504")
		s.recordUsage(tenant, providerName, model, false, len(prompt), 0, time.Since(lockStart), 504)
		return "", http.StatusGatewayTimeout, "provider_busy", "provider busy, try again later"
	}
	telemetry.ObserveLockWait(providerName, time.Since(lockStart))
	sendStart := time.Now()
	resp, err := provider.SendMessage(prompt, threadID)
	mu.Unlock()
	telemetry.ObserveResponseDuration(providerName, false, time.Since(sendStart))
	if err != nil {
		alog.Errorf("Provider %s error: %v", providerName, err)
		telemetry.ObserveProviderError(providerName, "send_error")
		telemetry.ObserveChatRequest(providerName, model, "500")
		s.recordUsage(tenant, providerName, model, false, len(prompt), 0, time.Since(sendStart), 500)
		return "", http.StatusInternalServerError, "provider_error", fmt.Sprintf("provider error: %v", err)
	}
	telemetry.RecordSuccess(providerName)
	telemetry.ObserveChatRequest(providerName, model, "200")
	s.recordUsage(tenant, providerName, model, false, len(prompt), len(resp.Message), time.Since(sendStart), 200)
	s.saveSessionURL(sessionID, provider)
	return resp.Message, http.StatusOK, "", ""
}

// handleChatCompletions handles OpenAI-compatible chat completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	alog := log_.WithRequestID(middleware.GetReqID(r.Context()))
	// Guard against huge bodies (DoS): 2MB is plenty for prompt JSON.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var req models.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		telemetry.ObserveChatRequest("none", "", "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Validate
	if len(req.Messages) == 0 {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "messages array is required")
		return
	}

	alog.Infof("Chat completion request (model=%s, messages=%d, stream=%v)",
		req.Model, len(req.Messages), req.Stream)

	// Extract the last user message
	lastUserMsg := extractLastUserMessage(req.Messages)
	if lastUserMsg == "" {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "no user message found")
		return
	}

	// Resolve provider by model (pooled routing)
	provider, mu, providerName := s.providerForModel(req.Model)
	if provider == nil {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "model_not_found", "no provider available for model "+req.Model)
		return
	}
	if mu == nil {
		mu = &s.mu
	}

	// Build conversation context with correct provider name for tool prompts
	threadID := extractThreadID(req.Messages)
	promptProvider := providerName
	if promptProvider == "" {
		promptProvider = s.cfg.Provider
	}
	prompt := buildPrompt(req.Messages, req.Tools, promptProvider)

	// Session continuity: X-Session-Id / X-Thread-Id header or req.User
	sessionID := extractSessionID(r, req.User)
	if sessionID != "" {
		alog.Infof("Persistent session %s for provider %s (pruning history to avoid duplication)", sessionID, providerName)
		// Prune to latest turn if not first turn (browser already has history)
		if len(req.Messages) > 2 {
			if pruned := buildPrunedPrompt(req.Messages, req.Tools, providerName); pruned != "" {
				prompt = pruned
				alog.Infof("Pruned prompt for session %s: %d -> %d chars", sessionID, len(buildPrompt(req.Messages, req.Tools, promptProvider)), len(prompt))
			}
		}
		// Save session URL after successful response (handled below)
		threadID = sessionID // use sessionID as threadID for provider
	}

	// Pre-flight: message too long → fast 400 instead of 2m hang (upstream #17)
	maxChars := s.cfg.MaxPromptChars
	if maxChars <= 0 {
		maxChars = 12000
	}
	if len(prompt) > maxChars {
		alog.Warnf("Pre-flight reject: prompt %d chars exceeds %d", len(prompt), maxChars)
		telemetry.ObserveChatRequest(providerName, req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "message_too_long", fmt.Sprintf("Prompt exceeds %d chars, send button will be disabled. Split or shorten.", maxChars))
		return
	}

	// Monthly quota gate (after cheap validation, before browser work).
	tenant := s.tenantOf(r)
	if s.quotaExceeded(tenant) {
		alog.Warnf("Quota exceeded for tenant %q", tenant)
		telemetry.ObserveChatRequest(providerName, req.Model, "429")
		writeJSONError(w, http.StatusTooManyRequests, "quota_exceeded", "monthly request quota exceeded, upgrade or wait for reset")
		return
	}

	// Handle streaming (unified pooled-aware path)
	if req.Stream {
		s.handleStreamingCompletionWithProvider(w, r, prompt, threadID, req, provider, mu, sessionID)
		return
	}

	// Non-streaming — per-provider serialized with timeout (no infinite hang).
	text, code, errType, errMsg := s.doProviderTurn(r, alog, provider, mu, providerName, req.Model, tenant, prompt, threadID, sessionID)
	if code != http.StatusOK {
		writeJSONError(w, code, errType, errMsg)
		return
	}

	// Parse tool calls from response (validated against requested names).
	var toolCalls []models.ToolCall
	if len(req.Tools) > 0 {
		toolCalls = parseToolCallsIfPresent(text, req.Tools, providerName)
	}

	completion := models.ChatCompletionResponse{
		ID:      models.NewResponseID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   provider.ModelID(),
		Choices: []models.Choice{
			{
				Index: 0,
				Message: models.ResponseMsg{
					Role:      "assistant",
					Content:   text,
					ToolCalls: toolCalls,
				},
				FinishReason: determineFinishReason(toolCalls),
			},
		},
		Usage: models.Usage{
			PromptTokens:     estimateTokens(prompt),
			CompletionTokens: estimateTokens(text),
			TotalTokens:      estimateTokens(prompt) + estimateTokens(text),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(completion)
}

// handleResponses handles OpenAI-compatible /v1/responses requests.
// Translates to the chat pipeline (same provider resolution, guards, quota,
// locks, telemetry, metering) and shapes the turn as a ResponseObject.
// Streaming is rejected: SSE event mapping is a follow-up, not silent emulation.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	alog := log_.WithRequestID(middleware.GetReqID(r.Context()))
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var req models.ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		telemetry.ObserveChatRequest("none", "", "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid request: %v", err))
		return
	}

	if req.Stream {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "streaming_unsupported", "streaming responses are not supported yet, retry with stream:false")
		return
	}

	msgs, err := req.ToChatMessages()
	if err != nil {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if extractLastUserMessage(msgs) == "" && len(msgs) == 0 {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "input is required")
		return
	}

	alog.Infof("Responses request (model=%s, messages=%d)", req.Model, len(msgs))

	provider, mu, providerName := s.providerForModel(req.Model)
	if provider == nil {
		telemetry.ObserveChatRequest("none", req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "model_not_found", "no provider available for model "+req.Model)
		return
	}

	chatTools := req.ToChatTools()
	threadID := extractThreadID(msgs)
	promptProvider := providerName
	if promptProvider == "" {
		promptProvider = s.cfg.Provider
	}
	prompt := buildPrompt(msgs, chatTools, promptProvider)

	sessionID := extractSessionID(r, req.User)
	if sessionID != "" {
		if len(msgs) > 2 {
			if pruned := buildPrunedPrompt(msgs, chatTools, providerName); pruned != "" {
				prompt = pruned
			}
		}
		threadID = sessionID
	}

	maxChars := s.cfg.MaxPromptChars
	if maxChars <= 0 {
		maxChars = 12000
	}
	if len(prompt) > maxChars {
		alog.Warnf("Pre-flight reject: prompt %d chars exceeds %d", len(prompt), maxChars)
		telemetry.ObserveChatRequest(providerName, req.Model, "400")
		writeJSONError(w, http.StatusBadRequest, "message_too_long", fmt.Sprintf("Prompt exceeds %d chars, send button will be disabled. Split or shorten.", maxChars))
		return
	}

	tenant := s.tenantOf(r)
	if s.quotaExceeded(tenant) {
		alog.Warnf("Quota exceeded for tenant %q", tenant)
		telemetry.ObserveChatRequest(providerName, req.Model, "429")
		writeJSONError(w, http.StatusTooManyRequests, "quota_exceeded", "monthly request quota exceeded, upgrade or wait for reset")
		return
	}

	text, code, errType, errMsg := s.doProviderTurn(r, alog, provider, mu, providerName, req.Model, tenant, prompt, threadID, sessionID)
	if code != http.StatusOK {
		writeJSONError(w, code, errType, errMsg)
		return
	}

	var toolCalls []models.ToolCall
	if len(chatTools) > 0 {
		toolCalls = parseToolCallsIfPresent(text, chatTools, providerName)
	}

	output := make([]models.ResponseOutputItem, 0, len(toolCalls)+1)
	for _, tc := range toolCalls {
		output = append(output, models.ResponseOutputItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if len(toolCalls) == 0 {
		output = append(output, models.ResponseOutputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []models.ResponseOutputContent{{Type: "output_text", Text: text}},
		})
	}

	promptTokens := estimateTokens(prompt)
	completionTokens := estimateTokens(text)
	respObj := models.ResponseObject{
		ID:      models.NewResponseObjectID(),
		Object:  "response",
		Created: time.Now().Unix(),
		Model:   provider.ModelID(),
		Status:  "completed",
		Output:  output,
		Usage: models.ResponsesUsage{
			InputTokens:  promptTokens,
			OutputTokens: completionTokens,
			TotalTokens:  promptTokens + completionTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(respObj)
}

// saveSessionURL persists the conversation URL for X-Session-Id continuity.
// No-op when sessionID empty or sessionMgr nil (single mode).
func (s *Server) saveSessionURL(sessionID string, provider providers.Provider) {
	if sessionID == "" || s.sessionMgr == nil || provider == nil {
		return
	}
	type urlGetter interface{ CurrentURL() string }
	// Providers embed *providers.Base which exposes CurrentURL; use assertion to avoid interface churn.
	if g, ok := provider.(urlGetter); ok {
		if u := g.CurrentURL(); u != "" {
			s.sessionMgr.SaveURL(sessionID, u)
		}
	}
}

// handleStreamingCompletion is the legacy single-provider entrypoint (kept for tests/back-compat).
// Delegates to the unified pooled-aware implementation.
func (s *Server) handleStreamingCompletion(w http.ResponseWriter, r *http.Request, prompt, threadID string, req models.ChatCompletionRequest) {
	provider, mu, _ := s.providerForModel(req.Model)
	if provider == nil {
		provider = s.provider
		mu = &s.mu
	}
	s.handleStreamingCompletionWithProvider(w, r, prompt, threadID, req, provider, mu, extractSessionID(r, req.User))
}

// handleStreamingCompletionWithProvider is the pooled-aware streaming path (per-provider mutex and model).
func (s *Server) handleStreamingCompletionWithProvider(w http.ResponseWriter, r *http.Request, prompt, threadID string, req models.ChatCompletionRequest, provider providers.Provider, mu *sync.Mutex, sessionID string) {
	alog := log_.WithRequestID(middleware.GetReqID(r.Context()))
	providerName := provider.Name()
	tenant := s.tenantOf(r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		telemetry.ObserveChatRequest(providerName, req.Model, "500")
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming not supported")
		return
	}

	respID := models.NewResponseID()
	created := time.Now().Unix()

	initialChunk := models.ChatCompletionChunk{
		ID:      respID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   provider.ModelID(),
		Choices: []models.ChunkChoice{{Index: 0, Delta: models.DeltaMsg{Role: "assistant"}}},
	}
	sendSSE(w, initialChunk)
	flusher.Flush()

	if mu == nil {
		mu = &s.mu
	}
	lockStart := time.Now()
	if !acquireProviderLock(r.Context(), mu, s.cfg.ResponseTimeout+30*time.Second) {
		alog.Errorf("Provider %s streaming lock timeout", providerName)
		telemetry.ObserveLockWait(providerName, time.Since(lockStart))
		telemetry.ObserveProviderError(providerName, "lock_timeout")
		telemetry.ObserveChatRequest(providerName, req.Model, "504")
		s.recordUsage(tenant, providerName, req.Model, true, len(prompt), 0, time.Since(lockStart), 504)
		errorChunk := models.ChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   provider.ModelID(),
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.DeltaMsg{Content: "Error: provider busy, try again later"}, FinishReason: "stop"}},
		}
		sendSSE(w, errorChunk)
		flusher.Flush()
		return
	}
	telemetry.ObserveLockWait(providerName, time.Since(lockStart))
	sendStart := time.Now()
	providerResp, err := provider.SendMessage(prompt, threadID)
	mu.Unlock()
	telemetry.ObserveResponseDuration(providerName, true, time.Since(sendStart))
	if err != nil {
		alog.Errorf("Provider %s streaming error: %v", providerName, err)
		telemetry.ObserveProviderError(providerName, "send_error")
		telemetry.ObserveChatRequest(providerName, req.Model, "500")
		s.recordUsage(tenant, providerName, req.Model, true, len(prompt), 0, time.Since(sendStart), 500)
		errorChunk := models.ChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   provider.ModelID(),
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.DeltaMsg{Content: fmt.Sprintf("Error: %v", err)}, FinishReason: "stop"}},
		}
		sendSSE(w, errorChunk)
		flusher.Flush()
		return
	}
	telemetry.RecordSuccess(providerName)
	telemetry.ObserveChatRequest(providerName, req.Model, "200")
	s.recordUsage(tenant, providerName, req.Model, true, len(prompt), len(providerResp.Message), time.Since(sendStart), 200)
	s.saveSessionURL(sessionID, provider)

	text := providerResp.Message
	chunkSize := 20
	for i := 0; i < len(text); i += chunkSize {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		chunk := models.ChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   provider.ModelID(),
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.DeltaMsg{Content: text[i:end]}}},
		}
		sendSSE(w, chunk)
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
	}

	finalChunk := models.ChatCompletionChunk{
		ID:      respID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   provider.ModelID(),
		Choices: []models.ChunkChoice{{Index: 0, FinishReason: "stop"}},
	}
	sendSSE(w, finalChunk)
	flusher.Flush()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// sendSSE writes a single SSE event.
func sendSSE(w http.ResponseWriter, data interface{}) {
	jsonBytes, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", jsonBytes)
}

// ── Helpers ───────────────────────────────────────────────────

// extractLastUserMessage gets the last user message content.
func extractLastUserMessage(messages []models.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].UnmarshalContent()
		}
	}
	return ""
}

// extractThreadID generates a thread ID from conversation history.
func extractThreadID(messages []models.ChatMessage) string {
	if len(messages) > 0 {
		return fmt.Sprintf("thread-%d", len(messages))
	}
	return "thread-1"
}

// buildPrompt constructs the full prompt from messages.
// Injects tool-calling instructions when tools are present (via prompt engineering,
// mirroring Chimera's approach since browser UIs lack native function-calling APIs).
func buildPrompt(messages []models.ChatMessage, toolDefs []models.Tool, provider string) string {
	var parts []string

	// If tools requested, prepend provider-specific tool prompt
	if len(toolDefs) > 0 {
		toolPrompt := tools.BuildToolPrompt(toolDefs, provider)
		parts = append(parts, toolPrompt)
	}

	for _, msg := range messages {
		content := msg.UnmarshalContent()
		if content == "" {
			continue
		}

		switch msg.Role {
		case "system":
			parts = append(parts, fmt.Sprintf("System: %s", content))
		case "user":
			parts = append(parts, content)
		case "assistant":
			// If assistant already has tool_calls, render them as JSON for context
			if len(msg.ToolCalls) > 0 {
				tcJSON, _ := json.Marshal(map[string]interface{}{"tool_calls": msg.ToolCalls})
				parts = append(parts, fmt.Sprintf("Assistant tool calls: %s", string(tcJSON)))
				if content != "" {
					parts = append(parts, fmt.Sprintf("Assistant: %s", content))
				}
			} else {
				parts = append(parts, fmt.Sprintf("Assistant: %s", content))
			}
		case "tool":
			parts = append(parts, fmt.Sprintf("Tool result (%s): %s", msg.ToolCallID, content))
		}
	}

	return strings.Join(parts, "\n\n")
}

// parseToolCallsIfPresent checks if the response contains tool calls.
// Delegates to internal/tools, validating names against the requested defs so
// hallucinated function names never reach the client.
func parseToolCallsIfPresent(text string, toolDefs []models.Tool, provider string) []models.ToolCall {
	if len(toolDefs) == 0 {
		return nil
	}
	valid := make([]string, 0, len(toolDefs))
	for _, t := range toolDefs {
		valid = append(valid, t.Function.Name)
	}
	return tools.ParseToolCallsWithDefs(text, valid, provider)
}

func trimNonJSON(text string) string {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

// extractSessionID pulls X-Session-Id / Session-Id / X-Thread-Id from headers or req.User.
func extractSessionID(r *http.Request, userField string) string {
	for _, k := range []string{"X-Session-Id", "Session-Id", "X-Thread-Id", "Thread-Id", "x-session-id", "session-id"} {
		if v := r.Header.Get(k); v != "" {
			return strings.TrimSpace(v)
		}
	}
	if strings.TrimSpace(userField) != "" {
		return strings.TrimSpace(userField)
	}
	return ""
}

// buildPrunedPrompt for persistent sessions: only latest user + associated tool results.
func buildPrunedPrompt(messages []models.ChatMessage, toolDefs []models.Tool, provider string) string {
	if len(messages) == 0 {
		return ""
	}
	// If single user message, keep as is (first turn)
	nonSystem := 0
	for _, m := range messages {
		if m.Role != "system" {
			nonSystem++
		}
	}
	if nonSystem <= 1 {
		return buildPrompt(messages, toolDefs, provider)
	}
	// Subsequent turns: take latest user and trailing tool messages
	var pruned []models.ChatMessage
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			// include this user and any following tool messages (already collected)
			pruned = append([]models.ChatMessage{messages[i]}, pruned...)
			break
		}
		if messages[i].Role == "tool" {
			pruned = append([]models.ChatMessage{messages[i]}, pruned...)
		}
	}
	if len(pruned) == 0 {
		pruned = []models.ChatMessage{messages[len(messages)-1]}
	}
	// Prepend system if present and first turn? For pruned we keep system only if first turn, else drop to avoid duplication.
	// Keep system only if messages has system and pruned is first turn equivalent
	return buildPrompt(pruned, toolDefs, provider)
}

// handleRefresh refreshes the provider tab (navigates to new chat).
// Supports ?provider=qwen&model=chimera-qwen or JSON {"model":"chimera-qwen"}.
// Used to recover from stale DOM / “no assistant messages found” after long idle.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	// Resolve target provider: query ?provider= or ?model= or JSON body {"model":"...","provider":"..."}
	targetModel := r.URL.Query().Get("model")
	if targetModel == "" {
		targetModel = r.URL.Query().Get("provider")
	}
	// Try JSON body as fallback (ignore error — body may be empty for GET)
	if targetModel == "" && r.Body != nil && r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var body struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "" {
			targetModel = body.Model
		} else if body.Provider != "" {
			targetModel = body.Provider
		}
	}
	var provider providers.Provider
	var mu *sync.Mutex
	var providerName string
	if targetModel != "" {
		provider, mu, providerName = s.providerForModel(targetModel)
	} else {
		// Default to single provider or deterministic first pooled
		if s.provider != nil {
			providerName = s.provider.Name()
			provider = s.provider
			mu = s.mus[providerName]
			if mu == nil {
				mu = &s.mu
			}
		} else {
			provider, mu, providerName = s.providerForModel("")
		}
	}
	if provider == nil {
		writeJSONError(w, http.StatusBadRequest, "model_not_found", "no provider found for refresh")
		return
	}
	if mu == nil {
		mu = &s.mu
	}
	log_.Infof("Refresh requested for provider %s (model hint=%q) from %s", providerName, targetModel, r.RemoteAddr)
	if !acquireProviderLock(r.Context(), mu, 30*time.Second) {
		writeJSONError(w, http.StatusGatewayTimeout, "provider_busy", "provider busy, try again later")
		return
	}
	start := time.Now()
	err := provider.NewChat()
	elapsed := time.Since(start)
	mu.Unlock()
	if err != nil {
		log_.Errorf("Refresh failed for %s: %v (elapsed %v)", providerName, err, elapsed)
		writeJSONError(w, http.StatusInternalServerError, "provider_error", fmt.Sprintf("refresh failed for %s: %v", providerName, err))
		return
	}
	log_.Infof("Refresh succeeded for %s in %v", providerName, elapsed)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"provider": providerName,
		"model":    provider.ModelID(),
		"elapsed_ms": elapsed.Milliseconds(),
	})
}

// determineFinishReason returns the appropriate finish reason.
func determineFinishReason(toolCalls []models.ToolCall) string {
	if len(toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// estimateTokens provides a rough token count estimate.
func estimateTokens(text string) int {
	// Rough approximation: ~4 chars per token
	return (len(text) + 3) / 4
}
