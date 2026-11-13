package glmweb

// newFakeGLMServer: the httptest double both test layers share.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newFakeGLMServer(t *testing.T, fake *fakeGLM) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/chatglm/user-api/user/refresh", func(w http.ResponseWriter, r *http.Request) {
		if err := recordSign(fake, r.Header); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.refreshHits++
		// Mirror the live shape: result.access_token (24h) + ROTATING
		// refresh token.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":0,"message":"success","result":{"access_token":"access-1","refresh_token":"refresh-rotated","is_guest":true,"mode":1}}`)
	})

	mux.HandleFunc("/chatglm/backend-api/assistant/stream", func(w http.ResponseWriter, r *http.Request) {
		if err := recordSign(fake, r.Header); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if b, err := json.Marshal(body); err == nil {
			fake.seenBodies = append(fake.seenBodies, string(b))
		}
		raw, err := os.ReadFile("testdata/turn1.sse")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write(raw)
	})

	return httptest.NewServer(mux)
}

// recordSign captures the mint for the server-side recomputation check: the
// test re-derives X-Sign from the public material and compares.
func recordSign(fake *fakeGLM, h http.Header) error {
	ts, nonce, sign := h.Get("X-Timestamp"), h.Get("X-Nonce"), h.Get("X-Sign")
	if ts == "" || nonce == "" || sign == "" {
		return os.ErrNotExist
	}
	if !strings.Contains(h.Get("App-Name"), "chatglm") || h.Get("X-Exp-Groups") == "" {
		return fmt.Errorf("missing FAKE_HEADERS members")
	}
	fake.seenSigns = append(fake.seenSigns, ts+"|"+nonce+"|"+sign)
	return nil
}
