package main

// smoke's contract: three steps in order, each naming its own fix, and a failed
// dependency stops the steps behind it instead of producing noise.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
)

// smokeGateway stubs one healthy gateway: /health at the origin, /v1/models and
// /v1/chat/completions under the API path.
func smokeGateway(t *testing.T, opts ...func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, override := range opts {
			if override(w, r) {
				return
			}
		}
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("OK"))
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"chimera-deepseek","owned_by":"deepseek"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func smokeArgs(base string) smokeOptions {
	return smokeOptions{
		BaseURL: base + "/v1",
		Token:   "t",
		Timeout: 5 * time.Second,
		Prompt:  "Reply with the single word OK.",
	}
}

// TestSmokePassesAgainstAWorkingGateway is the happy path: three steps, all ok,
// and the model comes from what the gateway serves.
func TestSmokePassesAgainstAWorkingGateway(t *testing.T) {
	srv := smokeGateway(t)
	defer srv.Close()

	steps := smokeSteps(smokeArgs(srv.URL))
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(steps), steps)
	}
	for _, s := range steps {
		if s.Status != "ok" {
			t.Errorf("step %s = %s (%s)", s.Name, s.Status, s.Detail)
		}
	}
	if !strings.Contains(steps[1].Detail, "chimera-deepseek") {
		t.Errorf("models step = %q", steps[1].Detail)
	}
	if steps[2].Detail != "OK" {
		t.Errorf("answer step = %q, want the model's reply", steps[2].Detail)
	}
	if !allPassed(steps) {
		t.Error("allPassed disagreed with the steps")
	}
}

// TestSmokeStopsAtADeadGateway: nothing listening means the other two steps are
// not attempted, and the hint names the command that fixes it.
func TestSmokeStopsAtADeadGateway(t *testing.T) {
	srv := smokeGateway(t)
	base := srv.URL
	srv.Close() // nothing is listening on that port any more

	opts := smokeArgs(base)
	opts.Provider = "deepseek"
	steps := smokeSteps(opts)

	if steps[0].Status != "failed" {
		t.Fatalf("health = %s, want failed", steps[0].Status)
	}
	if !strings.Contains(steps[0].Hint, "chimera up deepseek") {
		t.Errorf("hint = %q, want the start command", steps[0].Hint)
	}
	for _, s := range steps[1:] {
		if s.Status != "skipped" {
			t.Errorf("%s = %s, want skipped", s.Name, s.Status)
		}
	}
	if allPassed(steps) {
		t.Error("a failed health step must not be reported as passing")
	}
}

// TestSmokeNamesARejectedToken: a 401 is a credential problem and the fix is the
// token, not another gateway.
func TestSmokeNamesARejectedToken(t *testing.T) {
	srv := smokeGateway(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		return false
	})
	defer srv.Close()

	steps := smokeSteps(smokeArgs(srv.URL))
	if steps[1].Status != "failed" {
		t.Fatalf("models = %s, want failed", steps[1].Status)
	}
	if !strings.Contains(steps[1].Hint, "-token") {
		t.Errorf("hint = %q, want the token fix", steps[1].Hint)
	}
	if steps[2].Status != "skipped" {
		t.Errorf("answer = %s, want skipped", steps[2].Status)
	}
}

// TestSmokeRejectsAModelTheGatewayDoesNotServe keeps an explicit -model honest.
func TestSmokeRejectsAModelTheGatewayDoesNotServe(t *testing.T) {
	srv := smokeGateway(t)
	defer srv.Close()

	opts := smokeArgs(srv.URL)
	opts.Model = "chimera-kimi"
	steps := smokeSteps(opts)

	if steps[1].Status != "failed" {
		t.Fatalf("models = %s, want failed", steps[1].Status)
	}
	if !strings.Contains(steps[1].Detail, "chimera-deepseek") {
		t.Errorf("detail = %q, want the served ids listed", steps[1].Detail)
	}
}

// TestSmokeReportsAFailedTurn: the gateway is healthy and knows its model, but
// the turn itself fails — the step that names it is the answer step.
func TestSmokeReportsAFailedTurn(t *testing.T) {
	srv := smokeGateway(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/chat/completions" {
			// 400 is not retried by the client, so the test stays fast.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"prompt rejected","type":"invalid_request_error"}}`)
			return true
		}
		return false
	})
	defer srv.Close()

	steps := smokeSteps(smokeArgs(srv.URL))
	if steps[2].Status != "failed" {
		t.Fatalf("answer = %s, want failed: %+v", steps[2].Status, steps[2])
	}
	if steps[2].TookMS < 0 {
		t.Error("negative timing")
	}
}

// TestSmokeRendersTheReport pins what the user sees, including exit-status
// meaning.
func TestSmokeRendersTheReport(t *testing.T) {
	ok := []smokeStep{
		{Name: "health", Status: "ok", Detail: "listening", took: 12 * time.Millisecond},
		{Name: "models", Status: "ok", Detail: "chimera-deepseek", took: 9 * time.Millisecond},
		{Name: "answer", Status: "ok", Detail: "OK", took: 1200 * time.Millisecond},
	}
	out := renderSmoke("http://127.0.0.1:8000/v1", ok)
	for _, want := range []string{"chimera smoke", "✓ health", "✓ models", "✓ answer", "all 3 steps passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}

	failed := []smokeStep{
		{Name: "health", Status: "failed", Detail: "nothing answered", Hint: "start it: chimera up", took: time.Millisecond},
		{Name: "models", Status: "skipped", Detail: "needs a listening gateway"},
		{Name: "answer", Status: "skipped", Detail: "needs a listening gateway"},
	}
	out = renderSmoke("http://127.0.0.1:8000/v1", failed)
	for _, want := range []string{"✗ health", "start it: chimera up", "1 step failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure report missing %q:\n%s", want, out)
		}
	}
}

// TestSmokeJSONIsMachineReadable: the report is for scripts too, and times are
// milliseconds there rather than Go durations.
func TestSmokeJSONIsMachineReadable(t *testing.T) {
	steps := []smokeStep{{Name: "health", Status: "ok", Detail: "listening", took: 3 * time.Millisecond, TookMS: 3}}
	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"took_ms":3`) {
		t.Fatalf("json = %s", raw)
	}
}

// TestDefaultSmokeBase points at the configured gateway, and never at a bind
// address: 0.0.0.0 is not somewhere a client can connect.
func TestDefaultSmokeBase(t *testing.T) {
	for host, want := range map[string]string{
		"0.0.0.0":   "http://127.0.0.1:8010/v1",
		"":          "http://127.0.0.1:8010/v1",
		"127.0.0.1": "http://127.0.0.1:8010/v1",
	} {
		cfg := &config.Config{APIHost: host, APIPort: 8010}
		if got := defaultSmokeBase(cfg); got != want {
			t.Errorf("defaultSmokeBase(%q) = %q, want %q", host, got, want)
		}
	}
}

// TestOriginOfBaseAcceptsWhatPeopleType: a scheme-less address is completed
// rather than rejected.
func TestOriginOfBaseAcceptsWhatPeopleType(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8000/v1": "http://localhost:8000",
		"localhost:8000/v1":        "http://localhost:8000",
		"http://localhost:8000":    "http://localhost:8000",
		"http://localhost:8000/":   "http://localhost:8000",
	}
	for in, want := range cases {
		if got := originOfBase(in); got != want {
			t.Errorf("originOfBase(%q) = %q, want %q", in, got, want)
		}
	}
}
