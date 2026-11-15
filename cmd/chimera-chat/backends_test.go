package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func fakeGateway(model string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, model)
	}))
}

// TestScanFindsLiveGateways is the discovery contract: live gateways are
// reported with their provider, dead ports are reported as down rather than
// omitted (so the user can see the port exists but nothing serves it).
func TestScanFindsLiveGateways(t *testing.T) {
	chat := fakeGateway("chimera-chatgpt")
	defer chat.Close()
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()

	list := scan(defaultProbeClient(), []string{chat.URL, deep.URL, "http://127.0.0.1:1/v1"}, "t")
	if len(list) != 3 {
		t.Fatalf("got %d entries, want 3", len(list))
	}
	up := 0
	for _, b := range list {
		if b.Up {
			up++
		}
	}
	if up != 2 {
		t.Fatalf("got %d live gateways, want 2: %+v", up, list)
	}
	if list[0].Provider != "chatgpt" || list[1].Provider != "deepseek" {
		t.Fatalf("live gateways not sorted by provider first: %+v", list)
	}
}

// TestResolveTarget covers the three ways to name a backend.
func TestResolveTarget(t *testing.T) {
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()
	list := scan(defaultProbeClient(), []string{deep.URL}, "t")

	u, _ := url.Parse(deep.URL)

	for _, arg := range []string{"deepseek", "DeepSeek", u.Port(), deep.URL, "chimera-deepseek"} {
		got, err := resolveTarget(list, arg)
		if err != nil {
			t.Errorf("resolveTarget(%q): %v", arg, err)
			continue
		}
		if got.Model != "chimera-deepseek" {
			t.Errorf("resolveTarget(%q) = %s", arg, got.Model)
		}
	}

	if _, err := resolveTarget(list, "kimi"); err == nil {
		t.Error("expected an error for a provider that is not live")
	}
}

// TestResolveStartupAdoptsGatewayModel is the bug that made -model mandatory:
// against a DeepSeek-only gateway the chat must open on chimera-deepseek, never
// on the chimera-chatgpt id it used to carry as a default.
func TestResolveStartupAdoptsGatewayModel(t *testing.T) {
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()
	list := scan(defaultProbeClient(), []string{deep.URL, "http://127.0.0.1:1/v1"}, "t")

	b, needChoice, err := resolveStartup(list, "")
	if err != nil {
		t.Fatalf("resolveStartup: %v", err)
	}
	if needChoice {
		t.Fatal("one live gateway needs no choosing")
	}
	if b.Model != "chimera-deepseek" {
		t.Fatalf("model = %q, want the gateway's own chimera-deepseek", b.Model)
	}
	if b.BaseURL != deep.URL {
		t.Fatalf("base URL = %q, want %q", b.BaseURL, deep.URL)
	}
}

// TestResolveStartupAsksWhenAmbiguous: two live gateways is a real choice, so
// the picker opens instead of silently preferring one.
func TestResolveStartupAsksWhenAmbiguous(t *testing.T) {
	a, b := fakeGateway("chimera-chatgpt"), fakeGateway("chimera-deepseek")
	defer a.Close()
	defer b.Close()
	list := scan(defaultProbeClient(), []string{a.URL, b.URL}, "t")

	if _, needChoice, err := resolveStartup(list, ""); err != nil || !needChoice {
		t.Fatalf("needChoice = %v, err = %v; want a choice and no error", needChoice, err)
	}

	// Naming one of them answers the question, so no picker is needed.
	got, needChoice, err := resolveStartup(list, "chimera-deepseek")
	if err != nil || needChoice {
		t.Fatalf("explicit model: needChoice = %v, err = %v", needChoice, err)
	}
	if got.Provider != "deepseek" {
		t.Fatalf("provider = %q, want deepseek", got.Provider)
	}

	// The bare provider name is accepted too: `-model deepseek` reads naturally.
	if got, _, _ := resolveStartup(list, "deepseek"); got.Provider != "deepseek" {
		t.Fatalf("-model deepseek chose %q", got.Provider)
	}
}

// TestResolveStartupRejectsAbsentModel keeps an explicit request honest: if the
// user asks for a model no live gateway serves, say so instead of quietly
// talking to a different one.
func TestResolveStartupRejectsAbsentModel(t *testing.T) {
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()
	list := scan(defaultProbeClient(), []string{deep.URL}, "t")

	_, _, err := resolveStartup(list, "chimera-kimi")
	if err == nil {
		t.Fatal("expected an error for a model no live gateway serves")
	}
	if !strings.Contains(err.Error(), "deepseek") {
		t.Fatalf("error should list what is live, got: %v", err)
	}
}

// TestResolveStartupExplainsEmptyScan pins the remedy: a missing gateway is a
// startup problem, and the message must name the command that starts one.
func TestResolveStartupExplainsEmptyScan(t *testing.T) {
	_, _, err := resolveStartup([]backend{{BaseURL: "http://localhost:8010/v1", Err: "no gateway"}}, "")
	if err == nil || !strings.Contains(err.Error(), "chimera up") {
		t.Fatalf("error must name the start command, got: %v", err)
	}
}

// TestResolveStartupNamesTokenMismatch: a 401 is a token problem, not a missing
// gateway, and pointing at the wrong fix wastes the user's time.
func TestResolveStartupNamesTokenMismatch(t *testing.T) {
	_, _, err := resolveStartup([]backend{{BaseURL: "http://localhost:8000/v1", Err: "HTTP 401"}}, "")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error should mention the token, got: %v", err)
	}
}

// TestPreferredIsDeterministic covers the no-terminal path: the address the
// session already pointed at wins when it answered, else the first live one.
func TestPreferredIsDeterministic(t *testing.T) {
	list := []backend{
		{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
	}
	if got, _ := preferred(list, "http://localhost:8010/v1"); got.Provider != "deepseek" {
		t.Errorf("preferred followed the flag to %q", got.Provider)
	}
	if got, _ := preferred(list, "http://localhost:9999/v1"); got.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("preferred without a matching flag = %q", got.BaseURL)
	}
	if _, ok := preferred([]backend{{Err: "no gateway"}}, ""); ok {
		t.Error("preferred reported a backend from an empty scan")
	}
}

// TestRenderBackendsMarksCurrent keeps the listing useful: the active gateway
// has to be identifiable at a glance.
func TestRenderBackendsMarksCurrent(t *testing.T) {
	list := []backend{
		{Provider: "chatgpt", BaseURL: "http://localhost:8000/v1", Model: "chimera-chatgpt", Up: true},
		{Provider: "deepseek", BaseURL: "http://localhost:8010/v1", Model: "chimera-deepseek", Up: true},
		{BaseURL: "http://localhost:8011/v1", Err: "no gateway"},
	}
	out := renderBackends(list, "http://localhost:8010/v1")
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[1], "→ ") {
		t.Errorf("current backend not marked:\n%s", out)
	}
	if !strings.Contains(lines[2], "no gateway") {
		t.Errorf("dead port not explained:\n%s", out)
	}
}
