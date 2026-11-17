package spin

// Client discovery tests, over an httptest gateway stub: the model id belongs to
// the endpoint, so an unset -model is adopted from what the gateway serves while
// an explicit one is never quietly overridden. The probe itself must stay
// forgiving - transient trouble is the first turn's to report, with a remedy.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// modelsServer serves a fixed /models body and counts the requests it answered.
func modelsServer(t *testing.T, status int, ids ...string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("probed %s, want /v1/models", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// deadServer hands back a URL nothing is listening on: a gateway that was never
// started, which is the most common way discovery fails.
func deadServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// TestAdoptModelUsesGatewayID is the whole point: no flag, no default, and the
// run still names the model the gateway actually serves.
func TestAdoptModelUsesGatewayID(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, "chimera-deepseek")
	c := &Client{BaseURL: srv.URL + "/v1", Token: "t", HTTP: srv.Client()}

	if err := c.AdoptModel(); err != nil {
		t.Fatalf("AdoptModel: %v", err)
	}
	if c.Model != "chimera-deepseek" {
		t.Errorf("Model = %q, want chimera-deepseek", c.Model)
	}
}

// TestAdoptModelKeepsExplicitModel pins that an explicit id wins without a round
// trip: the caller may be deliberately pointing at the other side of a mismatch,
// and that has to be reported, not corrected.
func TestAdoptModelKeepsExplicitModel(t *testing.T) {
	srv, hits := modelsServer(t, http.StatusOK, "chimera-deepseek")
	c := &Client{BaseURL: srv.URL + "/v1", Token: "t", Model: "chimera-gpt", HTTP: srv.Client()}

	if err := c.AdoptModel(); err != nil {
		t.Fatalf("AdoptModel: %v", err)
	}
	if c.Model != "chimera-gpt" {
		t.Errorf("Model = %q, explicit id was overridden", c.Model)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("%d requests made, want none", n)
	}
}

// TestAdoptModelNamesTheFix walks the failure modes that have different fixes:
// nothing listening needs a gateway started, a rejected token needs the token.
func TestAdoptModelNamesTheFix(t *testing.T) {
	unauthorized, _ := modelsServer(t, http.StatusUnauthorized)
	forbidden, _ := modelsServer(t, http.StatusForbidden)
	broken, _ := modelsServer(t, http.StatusInternalServerError)
	empty, _ := modelsServer(t, http.StatusOK)

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"unreachable", deadServer(t), "chimera up"},
		{"401", unauthorized.URL + "/v1", "-token"},
		{"403", forbidden.URL + "/v1", "-token"},
		{"500", broken.URL + "/v1", "500"},
		{"no models", empty.URL + "/v1", "serves no models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{BaseURL: tc.url, Token: "t", HTTP: &http.Client{}}
			err := c.AdoptModel()
			if err == nil {
				t.Fatalf("AdoptModel should have failed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestAdoptModelWaitsOutARateLimit is the failure that made "adopt it later"
// hollow: the gateway rate-limits /models with a burst of one, so whichever
// probe just spent the token leaves the retry milliseconds away from another
// 429. Adoption has to wait for the bucket, not give up on the first refusal.
func TestAdoptModelWaitsOutARateLimit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "chimera-deepseek"}},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1", Token: "t", HTTP: srv.Client()}
	if err := c.AdoptModel(); err != nil {
		t.Fatalf("AdoptModel after one 429: %v", err)
	}
	if c.Model != "chimera-deepseek" {
		t.Fatalf("Model = %q", c.Model)
	}
	if n := atomic.LoadInt32(&hits); n < 2 {
		t.Fatalf("made %d requests, want the retry", n)
	}
}

// TestCheckModelToleratesProbeTrouble pins the old contract: this is a probe
// before the run, not a verdict on it. Unreachable and non-200 must not veto a
// run that Complete would report with its own remedy.
func TestCheckModelToleratesProbeTrouble(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusInternalServerError)
	for name, url := range map[string]string{
		"unreachable": deadServer(t),
		"500":         srv.URL + "/v1",
	} {
		t.Run(name, func(t *testing.T) {
			c := &Client{BaseURL: url, Token: "t", Model: "chimera-chatgpt", HTTP: &http.Client{}}
			if err := c.CheckModel(); err != nil {
				t.Errorf("CheckModel = %v, want nil", err)
			}
		})
	}
}

// TestCheckModelReportsMismatch keeps the precise failure: a real answer that
// lacks the requested id lists what the gateway does serve.
func TestCheckModelReportsMismatch(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, "chimera-deepseek")
	c := &Client{BaseURL: srv.URL + "/v1", Token: "t", Model: "chimera-chatgpt", HTTP: srv.Client()}

	err := c.CheckModel()
	if err == nil {
		t.Fatal("CheckModel = nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "chimera-deepseek") {
		t.Errorf("error %q does not list the available id", err)
	}
	if !strings.Contains(err.Error(), "chimera-chatgpt") {
		t.Errorf("error %q does not name the requested id", err)
	}
}
