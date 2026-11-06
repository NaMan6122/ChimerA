// Package providers defines the interface that all LLM providers implement.
package providers

import (
	"github.com/go-rod/rod"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
)

// Provider is the interface every browser-automated LLM must implement.
type Provider interface {
	// Name returns the provider identifier (e.g., "chatgpt", "claude").
	Name() string

	// ModelID returns the model ID exposed via the API (e.g., "chimera-chatgpt").
	ModelID() string

	// Init prepares the provider after browser launch (e.g., check login).
	Init(page *rod.Page, cfg *config.Config) error

	// SendMessage sends a message and waits for the full response.
	SendMessage(text string, threadID string) (*models.ProviderResponse, error)

	// NewChat navigates to a new conversation.
	NewChat() error

	// ExtractResponse reads the last assistant message from the page.
	ExtractResponse() (string, error)

	// IsLoggedIn checks whether the user is authenticated.
	IsLoggedIn() (bool, error)
}

// QuotaReporter is optionally implemented by providers that can report a
// cached usage quota (e.g. grok's /rest/rate-limits probe). Callers — the
// health handler is the intended one — type-check for it; providers without
// the interface simply carry no quota field in their health snapshot.
//
// Implementations must return the LAST KNOWN snapshot, never a live call:
// quota is refreshed at Init and after turns, not on demand. Recognized
// keys are "remaining" (int), "total" (int, optional) and "window"
// (string, optional); return nil when nothing has been cached yet. The map
// is served verbatim to API clients, so it must never carry credential
// material.
type QuotaReporter interface {
	Quota() map[string]any
}
