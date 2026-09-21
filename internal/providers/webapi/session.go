package webapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session is the storage-state material exported from a logged-in browser. It is
// password-equivalent: keep the file owner-only and never log it.
type Session struct {
	AccessToken string            `json:"access_token"`
	Cookies     map[string]string `json:"cookies"`
}

// LoadSession reads a storage-state file. The access token is required; cookie
// material is required in practice too (token-only sessions are WAF-rejected).
func LoadSession(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if s.AccessToken == "" {
		return nil, fmt.Errorf("%s: access_token is empty (create one with `chimera auth login`)", path)
	}
	if len(s.Cookies) == 0 {
		return nil, fmt.Errorf("%s: cookies are empty (token-only sessions are rejected by the WAF)", path)
	}
	return &s, nil
}

// InstallSession validates src and writes it to dst (0600, atomic rename).
// Sessions are password-equivalent; the destination directory is created 0700.
func InstallSession(src, dst string) (*Session, error) {
	s, err := LoadSession(src)
	if err != nil {
		return nil, err
	}
	if err := WriteSession(s, dst); err != nil {
		return nil, err
	}
	return s, nil
}

// WriteSession stores a session at dst with owner-only permissions.
func WriteSession(s *Session, dst string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ExpiresAt returns the access token's exp claim when the token is a JWT.
func (s *Session) ExpiresAt() (time.Time, bool) {
	return tokenExpiry(s.AccessToken)
}

// tokenExpiry decodes a JWT payload far enough to read `exp`. No signature
// verification: the server is the authority on validity.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// SessionStatus is a printable summary that never includes secrets.
type SessionStatus struct {
	Path       string
	Cookies    int
	ExpiresAt  time.Time
	HasExpiry  bool
	DaysRemain float64
}

// Status summarizes a loaded session.
func (s *Session) Status(path string) SessionStatus {
	st := SessionStatus{Path: path, Cookies: len(s.Cookies)}
	if exp, ok := s.ExpiresAt(); ok {
		st.ExpiresAt = exp
		st.HasExpiry = true
		st.DaysRemain = time.Until(exp).Hours() / 24
	}
	return st
}
