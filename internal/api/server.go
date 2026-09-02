// Package api provides the OpenAI-compatible HTTP server.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/tools"
	"golang.org/x/time/rate"
)

var log_ = logging.New("api", "./logs", "debug", true)

// Server holds the HTTP server and dependencies.
type Server struct {
	cfg      *config.Config
	router   *chi.Mux
	provider providers.Provider
	limiter  *rate.Limiter
	mu       sync.Mutex // serializes browser access (single page = no concurrent DOM writes)
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, provider providers.Provider) *Server {
	s := &Server{
		cfg:      cfg,
		provider: provider,
		limiter:  rate.NewLimiter(rate.Limit(1.0/float64(cfg.RateLimitSeconds)), 1),
	}

	s.router = chi.NewRouter()
	s.setupMiddleware()
	s.setupRoutes()

	return s
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
		json.NewEncoder(w).Encode(map[string]string{
			"name":    "Chimera Gateway",
			"version": "0.1.0",
			"status":  "running",
			"provider": s.cfg.Provider,
		})
	})
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
	resp := models.ModelList{
		Object: "list",
		Data: []models.ModelObject{
			{
				ID:      s.provider.ModelID(),
				Object:  "model",
				OwnedBy: s.provider.Name(),
			},
		},
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

	// Build conversation context
	threadID := extractThreadID(req.Messages)
	prompt := buildPrompt(req.Messages, req.Tools, s.cfg.Provider)

	// Handle streaming
	if req.Stream {
		s.handleStreamingCompletion(w, r, prompt, threadID, req)
		return
	}

	// Non-streaming — serialize browser access
	s.mu.Lock()
	start := time.Now()
	resp, err := s.provider.SendMessage(prompt, threadID)
	s.mu.Unlock()
	_ = start
	if err != nil {
		log_.Errorf("Provider error: %v", err)
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
		Model:   s.provider.ModelID(),
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
