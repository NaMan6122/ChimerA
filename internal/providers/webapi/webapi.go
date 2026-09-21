// Package webapi holds the transport-agnostic pieces shared by every browserless
// replay client (specs/011-provider-transports.md, specs/012-webapi-chatgpt-deepseek.md):
// the storage-state session, the retry/fallback error vocabulary, and a registry
// that lets the gateway select a transport by provider name.
package webapi

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers"
)

// ErrWAF marks an anti-bot challenge (Alibaba ssxmod for qwen, Cloudflare for
// DeepSeek). Callers may fall back to the DOM transport (spec 011 §4).
var ErrWAF = errors.New("waf challenge")

// ErrPowFailed marks a proof-of-work that could not be solved or was rejected.
// Spec 012 adds this to the fallback reason vocabulary.
var ErrPowFailed = errors.New("pow failed")

// ErrSessionExpired marks a rejected and unrefreshable credential.
var ErrSessionExpired = errors.New("session expired")

// HTTPError is a non-2xx response from a provider's web API.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Retryable reports whether the failure may succeed on another transport.
func (e *HTTPError) Retryable() bool {
	switch e.Status {
	case 401, 403, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// Retryable classifies a transport failure worth one retry and a DOM fallback.
func Retryable(err error) bool {
	if errors.Is(err, ErrWAF) || errors.Is(err, ErrPowFailed) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Retryable()
	}
	return false
}

// Reason maps a failure to a low-cardinality fallback metric label.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrWAF):
		return "waf"
	case errors.Is(err, ErrPowFailed):
		return "pow_failed"
	case errors.Is(err, ErrSessionExpired):
		return "session_expired"
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 401, 403:
			return "unauthorized"
		case 429:
			return "rate_limited"
		}
		return "http_error"
	}
	return "unknown"
}

// Constructor builds an uninitialized transport for a provider. Init is called
// separately by the caller, as with the existing providers.
type Constructor func(*config.Config) providers.Provider

var (
	regMu sync.RWMutex
	reg   = map[string]Constructor{}
)

// Register makes a transport selectable by provider name. Vendors call this from
// init(); cmd/chimera imports internal/providers/webapi/all to pull them in.
func Register(name string, c Constructor) {
	regMu.Lock()
	defer regMu.Unlock()
	if name == "" || c == nil {
		panic("webapi: Register called with empty name or nil constructor")
	}
	reg[name] = c
}

// Lookup returns the constructor registered for a provider name.
func Lookup(name string) (Constructor, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	c, ok := reg[name]
	return c, ok
}

// Registered lists the providers that have a webapi transport, sorted.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(reg))
	for k := range reg {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
