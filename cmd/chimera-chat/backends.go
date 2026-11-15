package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// backend is one discovered gateway: where it listens and what it serves.
type backend struct {
	Provider string `json:"provider"` // chatgpt | deepseek | qwen | ...
	BaseURL  string `json:"base_url"`
	// Model is the id the chat opens with; Models is everything the gateway
	// serves. They differ on a pooled gateway (PROVIDER=all), which lists one
	// model per provider.
	Model  string   `json:"model"`
	Models []string `json:"models,omitempty"`
	// Owners runs parallel to Models: the gateway's own claim about which
	// provider each id belongs to, which is the only answer that survives a
	// pooled gateway serving several.
	Owners []string `json:"owners,omitempty"`
	Up     bool     `json:"up"`
	// Err explains an address that did not answer; Note explains one that
	// answered but could not describe itself (rate limited, token rejected).
	Err  string `json:"err,omitempty"`
	Note string `json:"note,omitempty"`
}

// normalizeBase turns whatever the user typed into something probeable: a
// scheme-less "127.0.0.1:8010" gets http://, and a bare origin gets the /v1 path
// the gateway serves. Without this url.Parse rejects "127.0.0.1:8010", so the
// address the user named is recorded dead while another gateway is adopted.
func normalizeBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	if strings.TrimRight(u.Path, "/") == "" {
		u.Path = "/v1"
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/")
}

// canonicalKey is an address's identity for dedupe and comparison. localhost
// and 127.0.0.1 are the same gateway: without this, `-base-url 127.0.0.1:8010`
// is probed twice and then fails to match the row the scan found, so the flag
// silently means nothing.
func canonicalKey(raw string) string {
	n := normalizeBase(raw)
	u, err := url.Parse(n)
	if err != nil || u.Host == "" {
		return strings.ToLower(n)
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "127.0.0.1", "::1", "[::1]":
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + host + ":" + port + strings.TrimRight(u.Path, "/")
}

// probeTargets is the set of addresses /backends inspects: the well-known local
// ports plus wherever the session already points. A gateway serves one provider,
// so switching providers means switching addresses — there is no endpoint that
// changes a gateway's provider at runtime.
func probeTargets(current string, ports []int) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(u string) {
		u = normalizeBase(u)
		if u == "" {
			return
		}
		key := canonicalKey(u)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, u)
	}
	add(current)
	for _, p := range ports {
		add(fmt.Sprintf("http://localhost:%d/v1", p))
	}
	return out
}

// scan probes each base URL in parallel and returns what answered, sorted by
// provider then address. Unreachable targets are included so the user can see
// that port 8010 exists but nothing is listening on it.
func scan(client *http.Client, baseURLs []string, token string) []backend {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		list []backend
	)
	for _, base := range baseURLs {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			b := probe(client, base, token)
			mu.Lock()
			list = append(list, b)
			mu.Unlock()
		}(base)
	}
	wg.Wait()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Up != list[j].Up {
			return list[i].Up
		}
		if list[i].Provider != list[j].Provider {
			return list[i].Provider < list[j].Provider
		}
		return list[i].BaseURL < list[j].BaseURL
	})
	return list
}

// probe asks one address the two questions a gateway answers on different terms:
// whether anything is listening, and what it serves.
//
// Liveness comes from /health because the gateway leaves that path open. It used
// to come from /v1/models, which sits behind the global limiter (burst 1 per
// RATE_LIMIT_SECONDS) and the quota path — so a single recent request was enough
// to make a healthy gateway look absent, and the scan would then quietly open a
// different provider's gateway, or report "nothing is running" with one live.
func probe(client *http.Client, base, token string) backend {
	b := backend{BaseURL: base}

	alive, conclusive := probeHealth(client, base)
	if conclusive && !alive {
		b.Err = "no gateway"
		return b
	}

	ids, owners, err := probeModels(client, base, token)
	if len(ids) > 0 {
		b.Up = true
		b.Models = ids
		b.Owners = owners
		b.Model = ids[0]
		b.Provider = providerFor(ids[0], owners[0])
		return b
	}
	if conclusive && alive {
		// Listening, but not describing itself: the limiter answered 429, or the
		// token was rejected. Still a backend, and the model can be adopted once
		// the bucket refills.
		b.Up = true
		b.Note = describeModelsFailure(err)
		return b
	}
	b.Err = describeModelsFailure(err)
	return b
}

// probeHealth checks the one path that is neither authenticated nor rate
// limited. It reports conclusive=false for a non-200/404 answer, which is how a
// non-chimera OpenAI-compatible server (no /health) falls back to /v1/models
// deciding liveness on its own.
func probeHealth(client *http.Client, base string) (alive, conclusive bool) {
	origin := originOf(base)
	if origin == "" {
		return false, false
	}
	req, err := http.NewRequest(http.MethodGet, origin+"/health", nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, true // refused or killed: nothing is listening
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound:
		return false, false // not a chimera gateway: let /v1/models decide
	default:
		return true, true
	}
}

// originOf strips the /v1 path so the un-prefixed /health can be addressed.
func originOf(base string) string {
	u, err := url.Parse(normalizeBase(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// probeModels reads the model list, returning every id and the gateway's own
// owner claim for each. Errors are returned rather than folded into the backend
// so the caller can distinguish "nothing there" from "would not say".
func probeModels(client *http.Client, base, token string) (ids, owners []string, err error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, nil, fmt.Errorf("bad model list: %v", uerr)
	}
	for _, d := range out.Data {
		if d.ID == "" {
			continue
		}
		ids = append(ids, d.ID)
		owners = append(owners, d.OwnedBy)
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("no models")
	}
	return ids, owners, nil
}

// describeModelsFailure keeps 429 legible: it is the limiter, not a fault, and
// calling it "down" is what made a busy gateway look missing.
func describeModelsFailure(err error) string {
	if err == nil {
		return "no models"
	}
	msg := err.Error()
	if strings.Contains(msg, "429") {
		return "busy (HTTP 429): the gateway rate-limits model discovery"
	}
	return msg
}

// providerFor names the provider behind a model id: owned_by when the gateway
// states it (it does, and it is the only field that survives a pooled gateway
// listing one model per provider), else the chimera-<provider> convention.
func providerFor(id, ownedBy string) string {
	if o := strings.ToLower(strings.TrimSpace(ownedBy)); o != "" && o != strings.ToLower(id) {
		return o
	}
	return strings.ToLower(strings.TrimPrefix(id, "chimera-"))
}

// servedModels is every id a backend serves, whichever way it was discovered.
func servedModels(b backend) []string {
	if len(b.Models) > 0 {
		return b.Models
	}
	if b.Model != "" {
		return []string{b.Model}
	}
	return nil
}

// servesModel reports whether a gateway serves a model id — or a provider, so
// `-model deepseek` reads naturally. On a pooled gateway the provider has to be
// read per model: the backend's own Provider is only the first one listed.
func servesModel(b backend, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for i, id := range servedModels(b) {
		if id == want {
			return true
		}
		owner := ""
		if i < len(b.Owners) {
			owner = b.Owners[i]
		}
		if strings.EqualFold(providerFor(id, owner), want) {
			return true
		}
	}
	return b.Provider != "" && strings.EqualFold(b.Provider, want)
}

// matchBase finds the live backend an address refers to, comparing canonical
// identities so localhost and 127.0.0.1 are interchangeable.
func matchBase(up []backend, base string) (backend, bool) {
	want := canonicalKey(base)
	if want == "" || base == "" {
		return backend{}, false
	}
	for _, b := range up {
		if canonicalKey(b.BaseURL) == want {
			return b, true
		}
	}
	return backend{}, false
}

// resolveTarget picks the backend a /use argument refers to. It accepts a
// provider name ("deepseek"), a port ("8010") or a base URL.
func resolveTarget(list []backend, arg string) (backend, error) {
	arg = strings.TrimSpace(strings.ToLower(arg))
	if arg == "" {
		return backend{}, fmt.Errorf("usage: /use <provider|port|url>")
	}
	var up []backend
	for _, b := range list {
		if b.Up {
			up = append(up, b)
		}
	}
	if len(up) == 0 {
		return backend{}, fmt.Errorf("no gateway answered — start one with: chimera up deepseek")
	}
	for _, b := range up {
		if strings.EqualFold(b.Provider, arg) {
			return b, nil
		}
	}
	for _, b := range up {
		// Parse rather than substring-match: httptest URLs carry no path suffix,
		// and ":8010/" would miss them.
		if u, err := url.Parse(normalizeBase(b.BaseURL)); err == nil && u.Port() == arg {
			return b, nil
		}
	}
	for _, b := range up {
		if canonicalKey(b.BaseURL) == canonicalKey(arg) || servesModel(b, arg) {
			return b, nil
		}
	}
	var names []string
	for _, b := range up {
		names = append(names, b.Provider+" ("+b.BaseURL+")")
	}
	return backend{}, fmt.Errorf("%q is not a live backend\n  live: %s", arg, strings.Join(names, ", "))
}

// resolveStartup decides which gateway the chat opens against, before any UI
// exists.
//
// wantModel is the -model flag and wantBase the -base-url flag; either may be
// empty. An address the user named is intent and wins over ambiguity — otherwise
// `-base-url ...:8010` could silently open :8000. A model nobody serves is an
// error, never a silent substitution. needChoice means several gateways could
// serve and someone should ask.
func resolveStartup(list []backend, wantModel, wantBase string) (b backend, needChoice bool, err error) {
	var up []backend
	for _, x := range list {
		if x.Up {
			up = append(up, x)
		}
	}
	if len(up) == 0 {
		return backend{}, false, noGatewayError(list)
	}

	if wantBase != "" {
		if match, ok := matchBase(up, wantBase); ok {
			if wantModel == "" || servesModel(match, wantModel) {
				return match, false, nil
			}
			return backend{}, false, fmt.Errorf("%s serves %s, not %q\n"+
				"  drop -model to use what it serves, or point at a gateway that has it",
				match.BaseURL, strings.Join(servedModels(match), ", "), wantModel)
		}
	}

	if wantModel != "" {
		for _, x := range up {
			if servesModel(x, wantModel) {
				return x, false, nil
			}
		}
		return backend{}, false, fmt.Errorf("no live gateway serves %q\n  live: %s", wantModel, liveNames(up))
	}
	if len(up) == 1 {
		return up[0], false, nil
	}
	return backend{}, true, nil
}

// preferred is the pick for a caller that cannot ask a human: the address the
// session already pointed at if it answered, else the first live gateway in
// scan order.
func preferred(list []backend, baseURL string) (backend, bool) {
	var up []backend
	for _, b := range list {
		if b.Up {
			up = append(up, b)
		}
	}
	if len(up) == 0 {
		return backend{}, false
	}
	if match, ok := matchBase(up, baseURL); ok {
		return match, true
	}
	return up[0], true
}

// liveNames formats live gateways as "provider (url)" for error messages.
func liveNames(up []backend) string {
	names := make([]string, 0, len(up))
	for _, b := range up {
		name := b.Provider
		if name == "" {
			name = "gateway"
		}
		names = append(names, name+" ("+b.BaseURL+")")
	}
	return strings.Join(names, ", ")
}

// noGatewayError explains an empty scan with the commands that fix it. It reads
// the failed probes first: a 401 is a token problem and a 429 is a gateway that
// is busy, and answering "no gateway" to either sends the user to the wrong fix.
func noGatewayError(list []backend) error {
	for _, b := range list {
		if strings.Contains(b.Err, "401") || strings.Contains(b.Err, "403") {
			return fmt.Errorf("a gateway answered on %s but rejected the token\n"+
				"  pass the token it was started with:  -token <token>", b.BaseURL)
		}
	}
	for _, b := range list {
		if strings.Contains(b.Err, "429") {
			return fmt.Errorf("the gateway on %s is rate limiting model discovery (HTTP 429)\n"+
				"  retry in a moment, or pass -model explicitly", b.BaseURL)
		}
	}
	return fmt.Errorf("no gateway answered on localhost:8000-8011\n" +
		"  start one:     chimera up deepseek      (or: chimera up — every provider with a session)\n" +
		"  no session?    chimera auth login deepseek")
}

// renderBackends formats the list for the transcript.
func renderBackends(list []backend, current string) string {
	want := canonicalKey(current)
	var b strings.Builder
	for _, bk := range list {
		mark := "  "
		if want != "" && canonicalKey(bk.BaseURL) == want {
			mark = "→ "
		}
		if bk.Up {
			name := bk.Provider
			if name == "" {
				name = "gateway"
			}
			what := bk.Model
			if what == "" {
				what = bk.Note
			}
			fmt.Fprintf(&b, "%s%-10s %-32s %s", mark, name, bk.BaseURL, what)
		} else {
			fmt.Fprintf(&b, "%s%-10s %-32s %s", mark, "—", bk.BaseURL, bk.Err)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// defaultProbeClient bounds discovery: a port that is not answering should not
// stall the listing.
func defaultProbeClient() *http.Client {
	return &http.Client{Timeout: 700 * time.Millisecond}
}
