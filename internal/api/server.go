// Package api provides the OpenAI-compatible HTTP server.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/session"
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
	mu         sync.Mutex // serializes browser access for single mode
}

// NewServer creates a new API server for single provider.
func NewServer(cfg *config.Config, provider providers.Provider) *Server {
	s := &Server{
		cfg:       cfg,
		provider:  provider,
		providers: map[string]providers.Provider{provider.Name(): provider},
		mus:       map[string]*sync.Mutex{provider.Name(): &sync.Mutex{}},
		limiter:   rate.NewLimiter(rate.Limit(1.0/float64(cfg.RateLimitSeconds)), 1),
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
		limiter:    rate.NewLimiter(rate.Limit(1.0/float64(cfg.RateLimitSeconds)), 1),
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

// providerForModel resolves a model ID to a provider and its mutex.
// Supports "chimera-chatgpt", "chimera-qwen", "chatgpt", etc. Falls back to default.
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
	// Last resort: first pooled
	for name, p := range s.providers {
		return p, s.mus[name], name
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
	s.router.Use(s.authMiddleware)
	s.router.Use(s.rateLimitMiddleware)
}

// setupRoutes registers API routes.
func (s *Server) setupRoutes() {
	s.router.Route("/v1", func(r chi.Router) {
		r.Get("/models", s.handleListModels)
		r.Post("/chat/completions", s.handleChatCompletions)
	})

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
	return names
}

// authMiddleware validates Bearer token if configured.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks and root
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		// If no token configured, skip auth
		if s.cfg.APIToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		token := r.Header.Get("Authorization")
		if token != "Bearer "+s.cfg.APIToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware applies per-client rate limiting.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		if !s.limiter.Allow() {
			http.Error(w, `{"error":"rate limit exceeded, try again later"}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
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
		for _, p := range s.providers {
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

// handleChatCompletions handles OpenAI-compatible chat completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req models.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid request: %v"}`, err), http.StatusBadRequest)
		return
	}

	// Validate
	if len(req.Messages) == 0 {
		http.Error(w, `{"error":"messages array is required"}`, http.StatusBadRequest)
		return
	}

	log_.Infof("Chat completion request (model=%s, messages=%d, stream=%v)",
		req.Model, len(req.Messages), req.Stream)

	// Extract the last user message
	lastUserMsg := extractLastUserMessage(req.Messages)
	if lastUserMsg == "" {
		http.Error(w, `{"error":"no user message found"}`, http.StatusBadRequest)
		return
	}

	// Resolve provider by model (pooled routing)
	provider, mu, providerName := s.providerForModel(req.Model)
	if provider == nil {
		http.Error(w, `{"error":"no provider available for model `+req.Model+`"}`, http.StatusBadRequest)
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
		log_.Infof("Persistent session %s for provider %s (pruning history to avoid duplication)", sessionID, providerName)
		// Prune to latest turn if not first turn (browser already has history)
		if len(req.Messages) > 2 {
			if pruned := buildPrunedPrompt(req.Messages, req.Tools, providerName); pruned != "" {
				prompt = pruned
				log_.Infof("Pruned prompt for session %s: %d -> %d chars", sessionID, len(buildPrompt(req.Messages, req.Tools, promptProvider)), len(prompt))
			}
		}
		// Save session URL after successful response (handled below)
		threadID = sessionID // use sessionID as threadID for provider
	}

	// Pre-flight: message too long → fast 400 instead of 2m hang (upstream #17)
	if len(prompt) > 12000 {
		log_.Warnf("Pre-flight reject: prompt %d chars exceeds 12000", len(prompt))
		http.Error(w, `{"error":{"type":"message_too_long","message":"Prompt exceeds 12000 chars, send button will be disabled. Split or shorten."}}`, http.StatusBadRequest)
		return
	}

	// Handle streaming
	if req.Stream {
		s.handleStreamingCompletionWithProvider(w, r, prompt, threadID, req, provider, mu)
		return
	}

	// Non-streaming — per-provider serialized
	mu.Lock()
	start := time.Now()
	resp, err := provider.SendMessage(prompt, threadID)
	mu.Unlock()
	_ = start
	if err != nil {
		log_.Errorf("Provider %s error: %v", providerName, err)
		http.Error(w, fmt.Sprintf(`{"error":"provider error: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Parse tool calls from response
	var toolCalls []models.ToolCall
	if len(req.Tools) > 0 {
		toolCalls = parseToolCallsIfPresent(resp.Message, req.Tools)
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
					Content:   resp.Message,
					ToolCalls: toolCalls,
				},
				FinishReason: determineFinishReason(toolCalls),
			},
		},
		Usage: models.Usage{
			PromptTokens:     estimateTokens(prompt),
			CompletionTokens: estimateTokens(resp.Message),
			TotalTokens:      estimateTokens(prompt) + estimateTokens(resp.Message),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(completion)
}

// handleStreamingCompletion handles streaming SSE responses.
func (s *Server) handleStreamingCompletion(w http.ResponseWriter, r *http.Request, prompt, threadID string, req models.ChatCompletionRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	respID := models.NewResponseID()
	created := time.Now().Unix()

	// Send initial chunk with role
	initialChunk := models.ChatCompletionChunk{
		ID:      respID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   s.provider.ModelID(),
		Choices: []models.ChunkChoice{
			{
				Index: 0,
				Delta: models.DeltaMsg{Role: "assistant"},
			},
		},
	}
	sendSSE(w, initialChunk)
	flusher.Flush()

	// Get full response from provider (serialized)
	s.mu.Lock()
	providerResp, err := s.provider.SendMessage(prompt, threadID)
	s.mu.Unlock()
	if err != nil {
		log_.Errorf("Provider streaming error: %v", err)
		errorChunk := models.ChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   s.provider.ModelID(),
			Choices: []models.ChunkChoice{
				{
					Index:        0,
					Delta:        models.DeltaMsg{Content: fmt.Sprintf("Error: %v", err)},
					FinishReason: "stop",
				},
			},
		}
		sendSSE(w, errorChunk)
		flusher.Flush()
		return
	}

	// Simulate streaming by sending text in chunks
	text := providerResp.Message
	chunkSize := 20 // characters per chunk
	for i := 0; i < len(text); i += chunkSize {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}

		chunk := models.ChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   s.provider.ModelID(),
			Choices: []models.ChunkChoice{
				{
					Index: 0,
					Delta: models.DeltaMsg{Content: text[i:end]},
				},
			},
		}
		sendSSE(w, chunk)
		flusher.Flush()
		time.Sleep(10 * time.Millisecond) // Throttle
	}

	// Final chunk
	finalChunk := models.ChatCompletionChunk{
		ID:      respID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   s.provider.ModelID(),
		Choices: []models.ChunkChoice{
			{
				Index:        0,
				FinishReason: "stop",
			},
		},
	}
	sendSSE(w, finalChunk)
	flusher.Flush()

	// End of stream
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleStreamingCompletionWithProvider is the pooled-aware streaming path (per-provider mutex and model).
func (s *Server) handleStreamingCompletionWithProvider(w http.ResponseWriter, r *http.Request, prompt, threadID string, req models.ChatCompletionRequest, provider providers.Provider, mu *sync.Mutex) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
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
	mu.Lock()
	providerResp, err := provider.SendMessage(prompt, threadID)
	mu.Unlock()
	if err != nil {
		log_.Errorf("Provider %s streaming error: %v", provider.Name(), err)
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

	text := providerResp.Message
	chunkSize := 20
	for i := 0; i < len(text); i += chunkSize {
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
// Delegates to internal/tools.ParseToolCalls which uses brace-depth tracking.
func parseToolCallsIfPresent(text string, toolDefs []models.Tool) []models.ToolCall {
	if len(toolDefs) == 0 {
		return nil
	}
	return tools.ParseToolCalls(text)
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
