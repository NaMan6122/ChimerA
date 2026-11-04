package qwenweb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// jwtWithExp builds an unsigned token carrying only an exp claim; ExpiresAt does
// not verify signatures, and the server is the authority on validity.
func jwtWithExp(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestIsLoggedInRejectsExpiredToken pins the false positive that sent users to
// retry a dead session: /api/models answers with a stale token, but chat
// creation does not. The exp claim decides, without a network call.
func TestIsLoggedInRejectsExpiredToken(t *testing.T) {
	p := &Provider{sess: &webapi.Session{AccessToken: jwtWithExp(t, time.Now().Add(-18*24*time.Hour))}}

	ok, err := p.IsLoggedIn()
	if ok {
		t.Fatal("an expired token must not report as logged in")
	}
	if !errors.Is(err, webapi.ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}
}

// TestIsLoggedInAllowsFutureToken guards the inverse: a valid exp must not be
// rejected locally, or every fresh session would fail.
func TestIsLoggedInAllowsFutureToken(t *testing.T) {
	p := &Provider{sess: &webapi.Session{AccessToken: jwtWithExp(t, time.Now().Add(24*time.Hour))}}
	// No client: the method returns false (uninitialized) rather than an error,
	// which proves the exp check did not reject it.
	ok, err := p.IsLoggedIn()
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want false,nil for an uninitialized client", ok, err)
	}
}
