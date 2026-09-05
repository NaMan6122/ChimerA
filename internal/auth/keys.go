// Package auth resolves API credentials to tenant names.
//
// Single-key mode (API_TOKEN) maps to tenant "default". Multi-key mode
// (API_TOKENS="acme=sek1,basic=sek2") maps each token to its tenant.
// No keys configured means auth is disabled (open gateway).
package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// DefaultTenant is the tenant for single-key (API_TOKEN) credentials.
const DefaultTenant = "default"

// Keys maps bearer tokens to tenant names.
type Keys struct {
	tokenTenant map[string]string
	open        bool
}

// New builds a registry from a primary token plus "tenant=token,..." pairs.
// It errors on malformed pairs or duplicate tokens (fail fast on bad config).
func New(primaryToken, extra string) (*Keys, error) {
	m := make(map[string]string)
	if primaryToken != "" {
		m[primaryToken] = DefaultTenant
	}
	if strings.TrimSpace(extra) != "" {
		for _, pair := range strings.Split(extra, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			name, tok, ok := strings.Cut(pair, "=")
			name, tok = strings.TrimSpace(name), strings.TrimSpace(tok)
			if !ok || name == "" || tok == "" {
				return nil, fmt.Errorf("malformed API_TOKENS entry %q, want tenant=token", pair)
			}
			if _, dup := m[tok]; dup {
				return nil, fmt.Errorf("duplicate token in API_TOKENS for tenant %q", name)
			}
			m[tok] = name
		}
	}
	return &Keys{tokenTenant: m, open: len(m) == 0}, nil
}

// Open returns a registry with auth disabled.
func Open() *Keys { return &Keys{tokenTenant: map[string]string{}, open: true} }

// IsOpen reports whether auth is disabled.
func (k *Keys) IsOpen() bool { return k == nil || k.open }

// ExtractToken pulls the presented credential from Authorization Bearer,
// x-api-key, or anthropic-api-key headers (bare token or Bearer form,
// matching gateway convention).
func ExtractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
		return ""
	}
	for _, h := range []string{"X-Api-Key", "Anthropic-Api-Key"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
	}
	return ""
}

// Authenticate returns the tenant for the request credential.
// When auth is disabled it returns ("", true); callers normalize to DefaultTenant.
func (k *Keys) Authenticate(r *http.Request) (string, bool) {
	if k.IsOpen() {
		return "", true
	}
	t, ok := k.tokenTenant[ExtractToken(r)]
	return t, ok
}
