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
	Model    string `json:"model"`
	Up       bool   `json:"up"`
	Err      string `json:"err,omitempty"`
}

// probeTargets is the set of addresses /backends inspects: the well-known local
// ports plus wherever the session already points. A gateway serves one provider,
// so switching providers means switching addresses — there is no endpoint that
// changes a gateway's provider at runtime.
func probeTargets(current string, ports []int) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(u string) {
		u = strings.TrimRight(u, "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
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

func probe(client *http.Client, base, token string) backend {
	b := backend{BaseURL: base}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		b.Err = err.Error()
		return b
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		b.Err = "no gateway"
		return b
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		b.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return b
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Data) == 0 {
		b.Err = "no models"
		return b
	}
	b.Up = true
	b.Model = out.Data[0].ID
	b.Provider = strings.TrimPrefix(b.Model, "chimera-")
	return b
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
		return backend{}, fmt.Errorf("no gateway answered — start one with: TRANSPORT=webapi PROVIDER=chatgpt chimera")
	}
	for _, b := range up {
		if strings.EqualFold(b.Provider, arg) {
			return b, nil
		}
	}
	for _, b := range up {
		// Parse rather than substring-match: httptest URLs carry no path suffix,
		// and ":8010/" would miss them.
		if u, err := url.Parse(b.BaseURL); err == nil && u.Port() == arg {
			return b, nil
		}
	}
	for _, b := range up {
		if strings.EqualFold(b.BaseURL, arg) || strings.EqualFold(b.Model, arg) {
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
// exists. wantModel is the -model flag; empty means "whatever the gateway
// serves", because a gateway serves exactly one provider and therefore one
// model — the model id is a property of the backend, not something the user
// should have to restate on the command line.
//
// It returns needChoice when several live gateways could serve, leaving the
// pick to whoever can ask a human. An explicit model that no live gateway
// serves is an error, not something to silently override.
func resolveStartup(list []backend, wantModel string) (b backend, needChoice bool, err error) {
	var up []backend
	for _, x := range list {
		if x.Up {
			up = append(up, x)
		}
	}
	if len(up) == 0 {
		return backend{}, false, noGatewayError(list)
	}
	if wantModel != "" {
		for _, x := range up {
			// Accept the id the gateway reports, and the bare provider name so
			// `-model deepseek` reads naturally.
			if x.Model == wantModel || strings.EqualFold(x.Provider, wantModel) {
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
	want := strings.TrimRight(baseURL, "/")
	for _, b := range up {
		if strings.EqualFold(strings.TrimRight(b.BaseURL, "/"), want) {
			return b, true
		}
	}
	return up[0], true
}

// liveNames formats live gateways as "provider (url)" for error messages.
func liveNames(up []backend) string {
	names := make([]string, 0, len(up))
	for _, b := range up {
		names = append(names, b.Provider+" ("+b.BaseURL+")")
	}
	return strings.Join(names, ", ")
}

// noGatewayError explains an empty scan with the commands that fix it. It looks
// at the failed probes first: a gateway that answered 401 is a token problem,
// and saying "no gateway" there would send the user to the wrong fix.
func noGatewayError(list []backend) error {
	for _, b := range list {
		if strings.Contains(b.Err, "401") || strings.Contains(b.Err, "403") {
			return fmt.Errorf("a gateway answered on %s but rejected the token\n"+
				"  pass the token it was started with:  -token <token>", b.BaseURL)
		}
	}
	return fmt.Errorf("no gateway answered on localhost:8000-8011\n" +
		"  start one:     chimera up deepseek      (or: chimera up — every provider with a session)\n" +
		"  no session?    chimera auth login deepseek")
}

// renderBackends formats the list for the transcript.
func renderBackends(list []backend, current string) string {
	var b strings.Builder
	for _, bk := range list {
		mark := "  "
		if strings.EqualFold(strings.TrimRight(bk.BaseURL, "/"), strings.TrimRight(current, "/")) {
			mark = "→ "
		}
		if bk.Up {
			fmt.Fprintf(&b, "%s%-10s %-32s %s", mark, bk.Provider, bk.BaseURL, bk.Model)
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
