package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fakeGateway(model string) *httptest.Server {
	return fakeGatewayIDs(model)
}

// fakeGatewayIDs serves the two paths probe() asks, the way a real gateway
// answers them: /health is the open liveness path, /models carries owned_by.
func fakeGatewayIDs(ids ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			var parts []string
			for _, id := range ids {
				parts = append(parts, fmt.Sprintf(`{"id":%q,"object":"model","owned_by":%q}`,
					id, strings.TrimPrefix(id, "chimera-")))
			}
			fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(parts, ","))
		default:
			http.NotFound(w, r)
		}
	}))
}

// fakeBusyGateway is alive but rate limited on the model list — the state a
// gateway is in right after any other request touched it.
func fakeBusyGateway() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case "/models":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
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

	b, needChoice, err := resolveStartup(list, "", "")
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

	if _, needChoice, err := resolveStartup(list, "", ""); err != nil || !needChoice {
		t.Fatalf("needChoice = %v, err = %v; want a choice and no error", needChoice, err)
	}

	// Naming one of them answers the question, so no picker is needed.
	got, needChoice, err := resolveStartup(list, "chimera-deepseek", "")
	if err != nil || needChoice {
		t.Fatalf("explicit model: needChoice = %v, err = %v", needChoice, err)
	}
	if got.Provider != "deepseek" {
		t.Fatalf("provider = %q, want deepseek", got.Provider)
	}

	// The bare provider name is accepted too: `-model deepseek` reads naturally.
	if got, _, _ := resolveStartup(list, "deepseek", ""); got.Provider != "deepseek" {
		t.Fatalf("-model deepseek chose %q", got.Provider)
	}
}

// TestExplicitBaseBeatsAmbiguity is the silent-wrong-gateway blocker: with two
// gateways up, the address the user named decides, and it decides the same way
// whether it is spelled localhost or 127.0.0.1.
func TestExplicitBaseBeatsAmbiguity(t *testing.T) {
	chat := fakeGateway("chimera-chatgpt")
	defer chat.Close()
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()
	list := scan(defaultProbeClient(), []string{chat.URL, deep.URL}, "t")

	for _, base := range []string{deep.URL, strings.Replace(deep.URL, "127.0.0.1", "localhost", 1)} {
		b, needChoice, err := resolveStartup(list, "", base)
		if err != nil || needChoice {
			t.Fatalf("resolveStartup(%q): needChoice = %v, err = %v", base, needChoice, err)
		}
		if b.Provider != "deepseek" {
			t.Fatalf("resolveStartup(%q) chose %q, want deepseek", base, b.Provider)
		}
	}

	// An address that is down is an error, not an invitation to open the only
	// other gateway: that is the silent substitution this change removes.
	_, _, err := resolveStartup(list, "", "http://localhost:9999/v1")
	if err == nil || !strings.Contains(err.Error(), "nothing answered") {
		t.Fatalf("dead explicit base should be named, got: %v", err)
	}
	if !strings.Contains(err.Error(), "deepseek") || !strings.Contains(err.Error(), "chatgpt") {
		t.Fatalf("the error should offer what is live, got: %v", err)
	}

	// An address and a model that contradict each other are named, not resolved:
	// silently dropping either would talk to something the user did not ask for.
	_, _, err = resolveStartup(list, "chimera-kimi", deep.URL)
	if err == nil || !strings.Contains(err.Error(), "chimera-deepseek") {
		t.Fatalf("contradictory model/address should list what the address serves, got: %v", err)
	}
}

// TestBusyGatewayAcceptsAnExplicitModel: a gateway that answered /health but not
// /models has nothing to contradict the request, so naming it and its model must
// not be refused with an empty "serves , not ..." message.
func TestBusyGatewayAcceptsAnExplicitModel(t *testing.T) {
	busy := []backend{{BaseURL: "http://localhost:8010/v1", Up: true,
		Note: "busy (HTTP 429): the gateway rate-limits model discovery"}}

	b, needChoice, err := resolveStartup(busy, "chimera-deepseek", "http://localhost:8010/v1")
	if err != nil || needChoice {
		t.Fatalf("needChoice = %v, err = %v; want the named gateway", needChoice, err)
	}
	if b.BaseURL != "http://localhost:8010/v1" {
		t.Fatalf("opened %s", b.BaseURL)
	}
}

// TestPooledModelSelectionPicksTheRequestedID: on a pool, data[0] is the
// alphabetically first provider, so adopting it would answer with the wrong
// model while claiming to honour -model.
func TestPooledModelSelectionPicksTheRequestedID(t *testing.T) {
	pool := fakeGatewayIDs("chimera-chatgpt", "chimera-deepseek")
	defer pool.Close()
	list := scan(defaultProbeClient(), []string{pool.URL}, "t")

	for want, id := range map[string]string{
		"deepseek":        "chimera-deepseek",
		"chimera-gpt":     "", // not served
		"chimera-chatgpt": "chimera-chatgpt",
		"chatgpt":         "chimera-chatgpt",
	} {
		b, _, err := resolveStartup(list, want, "")
		if id == "" {
			if err == nil {
				t.Errorf("resolveStartup(%q) should have failed", want)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveStartup(%q): %v", want, err)
			continue
		}
		if b.Model != id {
			t.Errorf("resolveStartup(%q) chose model %q, want %q", want, b.Model, id)
		}
		// The label has to follow the model: a pool must not answer for
		// -model deepseek while calling itself chatgpt.
		wantProvider := strings.TrimPrefix(id, "chimera-")
		if b.Provider != wantProvider {
			t.Errorf("resolveStartup(%q) labelled the backend %q, want %q", want, b.Provider, wantProvider)
		}
	}
}

// TestHealthURLKeepsMountPrefix: a gateway served under /gw/v1 exposes
// /gw/health, not /health — probing the origin would call a live gateway absent.
func TestHealthURLKeepsMountPrefix(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8010/v1":    "http://localhost:8010/health",
		"http://localhost:8010/gw/v1": "http://localhost:8010/gw/health",
		"localhost:8010":              "http://localhost:8010/health",
	}
	for in, want := range cases {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSlowHealthFallsBackToModels: the probe deadline is 700ms, so a gateway
// that is merely slow on /health must not be declared absent while /models
// answers.
func TestSlowHealthFallsBackToModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			time.Sleep(120 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		case "/models":
			fmt.Fprint(w, `{"data":[{"id":"chimera-deepseek","owned_by":"deepseek"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fast := &http.Client{Timeout: 50 * time.Millisecond}
	list := scan(fast, []string{srv.URL}, "t")
	if len(list) != 1 || !list[0].Up {
		t.Fatalf("slow /health hid a live gateway: %+v", list)
	}
	if list[0].Model != "chimera-deepseek" {
		t.Fatalf("model = %q", list[0].Model)
	}
}

// TestCanonicalKeySpellings: the loopback spellings people actually type collapse
// to one identity, but ::1 stays distinct — a 127.0.0.1-bound gateway is not
// reachable as ::1 and must remain visible as its own row.
func TestCanonicalKeySpellings(t *testing.T) {
	same := []string{
		"http://localhost:8010/v1",
		"http://127.0.0.1:8010/v1",
		"http://0.0.0.0:8010/v1",
		"localhost:8010",
		"http://LOCALHOST:8010/v1/",
	}
	want := canonicalKey(same[0])
	for _, u := range same[1:] {
		if got := canonicalKey(u); got != want {
			t.Errorf("canonicalKey(%q) = %q, want %q", u, got, want)
		}
	}
	if canonicalKey("http://[::1]:8010/v1") == want {
		t.Error("[::1] collapsed into localhost; it is a different listener")
	}
}

// TestProbeTargetsDedupeLocalhostSpellings: one gateway must not appear twice,
// or the picker asks the user to choose between a row and its own reflection.
func TestProbeTargetsDedupeLocalhostSpellings(t *testing.T) {
	got := probeTargets("http://127.0.0.1:8010/v1", []int{8010, 8011})
	if len(got) != 2 {
		t.Fatalf("probeTargets = %v, want 2 entries (8010 deduped, 8011 added)", got)
	}
	if !strings.Contains(got[0], "8010") {
		t.Fatalf("the named address should be first: %v", got)
	}
	for _, u := range got {
		if _, err := url.Parse(u); err != nil {
			t.Fatalf("probeTargets produced an unparseable address %q: %v", u, err)
		}
	}
}

// TestNormalizeBase covers the shapes people actually type.
func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8010":            "http://127.0.0.1:8010/v1",
		"http://localhost:8010":     "http://localhost:8010/v1",
		"http://localhost:8010/v1/": "http://localhost:8010/v1",
		"http://localhost:8010/v1":  "http://localhost:8010/v1",
		"":                          "",
	}
	for in, want := range cases {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
	// localhost and 127.0.0.1 are one gateway, whatever the spelling.
	if canonicalKey("http://127.0.0.1:8010/v1") != canonicalKey("localhost:8010") {
		t.Error("localhost and 127.0.0.1 did not collapse to one identity")
	}
}

// TestProbeKeepsBusyGatewayVisible is the blocker that made a live gateway look
// missing: /v1/models is behind the rate limiter, so a 429 there must not be
// read as "nothing is listening" and must not hand the session to another
// provider.
func TestProbeKeepsBusyGatewayVisible(t *testing.T) {
	busy := fakeBusyGateway()
	defer busy.Close()
	chat := fakeGateway("chimera-chatgpt")
	defer chat.Close()

	list := scan(defaultProbeClient(), []string{busy.URL, chat.URL}, "t")
	var got *backend
	for i := range list {
		if list[i].BaseURL == busy.URL {
			got = &list[i]
		}
	}
	if got == nil || !got.Up {
		t.Fatalf("busy gateway reported down: %+v", list)
	}
	if !strings.Contains(got.Note, "429") {
		t.Fatalf("note should name the rate limit, got %q", got.Note)
	}

	// And the choice the user made still wins over the gateways that answered.
	b, _, err := resolveStartup(list, "", busy.URL)
	if err != nil {
		t.Fatalf("resolveStartup: %v", err)
	}
	if canonicalKey(b.BaseURL) != canonicalKey(busy.URL) {
		t.Fatalf("opened %s instead of the busy gateway", b.BaseURL)
	}
}

// TestPooledGatewayServesEveryProvider: PROVIDER=all lists one model per
// provider, so -model must match any id, not just the first.
func TestPooledGatewayServesEveryProvider(t *testing.T) {
	pool := fakeGatewayIDs("chimera-chatgpt", "chimera-deepseek")
	defer pool.Close()
	list := scan(defaultProbeClient(), []string{pool.URL}, "t")
	if len(list) != 1 || len(list[0].Models) != 2 {
		t.Fatalf("pooled model list not captured: %+v", list)
	}

	for _, want := range []string{"chimera-deepseek", "deepseek", "chimera-chatgpt"} {
		if _, _, err := resolveStartup(list, want, ""); err != nil {
			t.Errorf("resolveStartup(%q): %v", want, err)
		}
	}
	if _, _, err := resolveStartup(list, "chimera-kimi", ""); err == nil {
		t.Error("expected an error for an id the pool does not serve")
	}
}

// TestProviderComesFromOwnedBy: the gateway states the owner, which survives a
// pooled listing; the chimera- prefix convention is only the fallback.
func TestProviderComesFromOwnedBy(t *testing.T) {
	if got := providerFor("some-custom-id", "deepseek"); got != "deepseek" {
		t.Errorf("providerFor = %q, want deepseek", got)
	}
	if got := providerFor("chimera-qwen", ""); got != "qwen" {
		t.Errorf("providerFor fallback = %q, want qwen", got)
	}
}

// TestResolveStartupRejectsAbsentModel keeps an explicit request honest: if the
// user asks for a model no live gateway serves, say so instead of quietly
// talking to a different one.
func TestResolveStartupRejectsAbsentModel(t *testing.T) {
	deep := fakeGateway("chimera-deepseek")
	defer deep.Close()
	list := scan(defaultProbeClient(), []string{deep.URL}, "t")

	_, _, err := resolveStartup(list, "chimera-kimi", "")
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
	_, _, err := resolveStartup([]backend{{BaseURL: "http://localhost:8010/v1", Err: "no gateway"}}, "", "")
	if err == nil || !strings.Contains(err.Error(), "chimera up") {
		t.Fatalf("error must name the start command, got: %v", err)
	}
}

// TestNoGatewayErrorNamesRateLimit: a 429 on a server without /health is still
// not "nothing is running", and the fix is to wait rather than to start another.
func TestNoGatewayErrorNamesRateLimit(t *testing.T) {
	_, _, err := resolveStartup([]backend{{BaseURL: "http://localhost:8000/v1", Err: "HTTP 429"}}, "", "")
	if err == nil || !strings.Contains(err.Error(), "rate limiting") {
		t.Fatalf("error should name the rate limit, got: %v", err)
	}
}

// TestResolveStartupNamesTokenMismatch: a 401 is a token problem, not a missing
// gateway, and pointing at the wrong fix wastes the user's time.
func TestResolveStartupNamesTokenMismatch(t *testing.T) {
	_, _, err := resolveStartup([]backend{{BaseURL: "http://localhost:8000/v1", Err: "HTTP 401"}}, "", "")
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
