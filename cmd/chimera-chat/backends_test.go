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
