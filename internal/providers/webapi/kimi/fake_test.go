package kimiweb

// newFakeKimiServer: the httptest double both test layers share. It speaks the
// Connect envelope on the chat path and the JSON refresh endpoint, replaying
// the frames captured live on 2026-11-13.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeKimi records what the client sent and replays the captured frames.
type fakeKimi struct {
	mu          sync.Mutex
	seenBodies  []string
	refreshHits int
}

func (f *fakeKimi) recordBody(b []byte) {
	f.mu.Lock()
	f.seenBodies = append(f.seenBodies, string(b))
	f.mu.Unlock()
}

func (f *fakeKimi) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seenBodies...)
}

func (f *fakeKimi) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshHits
}

// fakeJWT builds a decodable (unsigned) JWT carrying an exp claim, so the
// client's expiry cache is exercised rather than its fallback TTL.
func fakeJWT(exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "app_id": "kimi", "typ": "access"})
	return "eyJhbGciOiJIUzUxMiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newFakeKimiServer(t *testing.T, fake *fakeKimi) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"error_type":"auth.token.invalid"}`, http.StatusUnauthorized)
			return
		}
		fake.mu.Lock()
		fake.refreshHits++
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Mirror the live shape: an access JWT plus a ROTATED refresh JWT.
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-rotated"}`,
			fakeJWT(time.Now().Add(30*24*time.Hour)))
	})

	mux.HandleFunc(chatPath, func(w http.ResponseWriter, r *http.Request) {
		// The live endpoint answers HTTP 415 without this exact content type.
		if ct := r.Header.Get("Content-Type"); ct != "application/connect+json" {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		// Strip the 5-byte Connect envelope header to reach the JSON payload.
		if len(raw) <= 5 {
			http.Error(w, "short envelope", http.StatusBadRequest)
			return
		}
		payload := raw[5:]
		fake.recordBody(payload)

		var req struct {
			ChatID  string `json:"chat_id"`
			Message struct {
				ParentID string `json:"parent_id"`
			} `json:"message"`
		}
		_ = json.Unmarshal(payload, &req)
		// A poisoned continuation fails, forcing the provider's self-heal.
		if req.ChatID == "poison" {
			http.Error(w, `{"error":{"message":"chat gone"}}`, http.StatusInternalServerError)
			return
		}

		fixture := "testdata/turn1.frames"
		if req.ChatID != "" {
			fixture = "testdata/turn2.frames"
		}
		frames, err := os.ReadFile(fixture)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write(frames)
	})

	return httptest.NewServer(mux)
}
