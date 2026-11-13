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
		// A continuation with the poisoned id fails, forcing the provider's
		// self-heal onto a fresh conversation.
		if conv, _ := body["conversation_id"].(string); conv == "poison" {
			http.Error(w, `{"status":1,"message":"conversation gone"}`, http.StatusInternalServerError)
			return
		}
		raw, err := os.ReadFile("testdata/turn1.sse")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write(raw)
	})

	mux.HandleFunc("/chatglm/backend-api/assistant/conversation/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConversationID string `json:"conversation_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.recordDelete(body.ConversationID)
		w.Header().Set("Content-Type", "application/json")
		// Live shape: a repeat delete answers HTTP 200 with status:1
		// "删除失败" — the envelope, not the status code, is the verdict.
		if !fake.firstDelete(body.ConversationID) {
			fmt.Fprint(w, `{"status":1,"message":"删除失败","result":[]}`)
			return
		}
		fmt.Fprint(w, `{"status":0,"message":"success","result":[]}`)
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
