package main

// smokecmd.go — `chimera smoke`: prove the whole chain answers, in three steps.
//
// Everything here is the path a real client takes: the same /health the UI
// probes, the same /models the harness adopts a model from, and the same
// /chat/completions a turn arrives on. A failing step names its own fix, because
// "it doesn't work" is what this command exists to replace.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/spin"
)

const smokeUsage = `Usage: chimera smoke [-base-url <url>] [-token <token>] [-model <id>] [-timeout <dur>] [-json]

Runs the three things a working install must do, in order:

  1. health   GET  <origin>/health           the gateway is listening
  2. models   GET  <base>/models             it knows what it serves
  3. answer   POST <base>/chat/completions   a real turn comes back

Defaults come from the resolved configuration (API_HOST, API_PORT, API_TOKEN),
so a local install needs no flags. Exit status is 1 if any step fails.

  -base-url <url>  gateway base URL (default http://127.0.0.1:<API_PORT>/v1)
  -token <token>   bearer token (default API_TOKEN)
  -model <id>      model to use (default: whatever the gateway serves)
  -timeout <dur>   budget for the answer step (default 90s)
  -json            machine-readable output
`

// smokeStep is one check. Took is marshalled as milliseconds because a JSON
// report is read by scripts, not by this command.
type smokeStep struct {
	Name   string `json:"step"`
	Status string `json:"status"` // ok | failed | skipped
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
	TookMS int64  `json:"took_ms"`
	took   time.Duration
}

type smokeOptions struct {
	BaseURL  string
	Token    string
	Model    string
	Provider string // only used to make the start/fix hints concrete
	Timeout  time.Duration
	Prompt   string
}

// runSmoke handles `chimera smoke`.
func runSmoke(args []string) {
	cfg, _, err := config.LoadReadOnly()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	baseURL := fs.String("base-url", defaultSmokeBase(cfg), "gateway base URL")
	token := fs.String("token", cfg.APIToken, "bearer token")
	model := fs.String("model", "", "model id (default: whatever the gateway serves)")
	timeout := fs.Duration("timeout", 90*time.Second, "budget for the answer step")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, smokeUsage)
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", fs.Arg(0), smokeUsage)
		os.Exit(2)
	}

	opts := smokeOptions{
		BaseURL:  *baseURL,
		Token:    *token,
		Model:    *model,
		Provider: cfg.Provider,
		Timeout:  *timeout,
		Prompt:   "Reply with the single word OK.",
	}
	steps := smokeSteps(opts)

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"base_url": opts.BaseURL,
			"ok":       allPassed(steps),
			"steps":    steps,
		})
	} else {
		fmt.Print(renderSmoke(opts.BaseURL, steps))
	}
	if !allPassed(steps) {
		os.Exit(1)
	}
}

// defaultSmokeBase points at the gateway this machine is configured to run.
// 0.0.0.0 and [::] are bind addresses, not destinations, so they become loopback.
func defaultSmokeBase(cfg *config.Config) string {
	host := cfg.APIHost
	if host == "" || host == "0.0.0.0" || host == "[::]" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/v1", net.JoinHostPort(host, fmt.Sprint(cfg.APIPort)))
}

// smokeSteps runs the three checks in order. A failed step stops the ones that
// depend on it: a gateway that is not listening cannot tell you what it serves.
func smokeSteps(opts smokeOptions) []smokeStep {
	steps := make([]smokeStep, 0, 3)

	healthCapped := 5 * time.Second
	if opts.Timeout > 0 && opts.Timeout < healthCapped {
		healthCapped = opts.Timeout
	}

	// 1. health — the open path, so a busy gateway still answers it.
	start := time.Now()
	code, err := getStatus(opts.BaseURL, "/health", healthCapped)
	health := smokeStep{Name: "health", took: time.Since(start)}
	switch {
	case err != nil:
		health.Status = "failed"
		health.Detail = "nothing answered at " + originOfBase(opts.BaseURL)
		health.Hint = "start it:  chimera up " + opts.Provider + "      (or: chimera up)"
	case code != http.StatusOK:
		health.Status = "failed"
		health.Detail = fmt.Sprintf("HTTP %d from /health", code)
		health.Hint = "the gateway is listening but unhealthy — read its log under LOG_DIR"
	default:
		health.Status = "ok"
		health.Detail = "listening"
	}
	health.TookMS = health.took.Milliseconds()
	steps = append(steps, health)
	if health.Status != "ok" {
		return append(steps, skipped("models", "needs a listening gateway"),
			skipped("answer", "needs a listening gateway"))
	}

	// 2. models — the same call the harness adopts its model from.
	client := spin.NewClient(opts.BaseURL, opts.Token, opts.Model, "smoke-"+fmt.Sprint(time.Now().UnixNano()))
	client.HTTP.Timeout = opts.Timeout
	start = time.Now()
	ids, err := client.Models()
	models := smokeStep{Name: "models", took: time.Since(start), TookMS: time.Since(start).Milliseconds()}

	switch {
	case err != nil:
		models.Status = "failed"
		models.Detail = firstLine(err.Error())
		if strings.Contains(err.Error(), "rejected the token") {
			models.Hint = "pass the token the gateway was started with:  -token <token>"
		} else {
			models.Hint = "a gateway that is up but serves no model has no session:  chimera auth login <provider>"
		}
	case opts.Model != "" && !contains(ids, opts.Model):
		models.Status = "failed"
		models.Detail = fmt.Sprintf("serves %s, not %q", strings.Join(ids, ", "), opts.Model)
		models.Hint = "drop -model to use what it serves"
	default:
		models.Status = "ok"
		if opts.Model == "" {
			client.Model = ids[0]
		}
		models.Detail = strings.Join(ids, ", ")
	}
	steps = append(steps, models)
	if models.Status != "ok" {
		return append(steps, skipped("answer", "needs a model"))
	}

	// 3. answer — one real turn, the whole point of the exercise.
	start = time.Now()
	msg, _, err := client.Complete([]spin.Message{{Role: "user", Content: opts.Prompt}}, false)
	answer := smokeStep{Name: "answer", took: time.Since(start)}
	answer.TookMS = answer.took.Milliseconds()
	if err != nil {
		answer.Status = "failed"
		answer.Detail = firstLine(err.Error())
		answer.Hint = "the gateway accepted the request but no answer came back — see its log"
	} else if strings.TrimSpace(msg.Content) == "" {
		answer.Status = "failed"
		answer.Detail = "empty answer"
		answer.Hint = "the provider returned nothing — check the gateway log for the upstream turn"
	} else {
		answer.Status = "ok"
		answer.Detail = clipLine(strings.TrimSpace(msg.Content), 60)
	}
	answer.TookMS = answer.took.Milliseconds()
	return append(steps, answer)
}

func skipped(name, why string) smokeStep {
	return smokeStep{Name: name, Status: "skipped", Detail: why}
}

// getStatus performs one GET with its own bound, so a dead port cannot hold the
// report for the client timeout.
func getStatus(base, path string, timeout time.Duration) (int, error) {
	origin := originOfBase(base)
	if origin == "" {
		return 0, fmt.Errorf("unparseable base URL %q", base)
	}
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(origin + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// originOfBase strips the API path: /health is served at the origin. A
// scheme-less address is completed rather than rejected, because "127.0.0.1:8000"
// is what people type.
func originOfBase(base string) string {
	u := strings.TrimSpace(base)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	return strings.TrimSuffix(u, "/v1")
}

func allPassed(steps []smokeStep) bool {
	for _, s := range steps {
		if s.Status == "failed" {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func clipLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// renderSmoke formats the report. Pure, so the shape can be tested.
func renderSmoke(base string, steps []smokeStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "chimera smoke — %s\n\n", base)
	failed := 0
	for _, s := range steps {
		mark := "✓"
		switch s.Status {
		case "failed":
			mark = "✗"
			failed++
		case "skipped":
			mark = "·"
		}
		fmt.Fprintf(&b, "  %s %-8s %-28s %s\n", mark, s.Name, s.Detail, s.took.Round(time.Millisecond))
		if s.Hint != "" {
			fmt.Fprintf(&b, "    %-8s %s\n", "", s.Hint)
		}
	}
	b.WriteString("\n")
	switch failed {
	case 0:
		fmt.Fprintf(&b, "all %d steps passed in %s\n", len(steps), totalTook(steps).Round(time.Millisecond))
	case 1:
		b.WriteString("1 step failed\n")
	default:
		fmt.Fprintf(&b, "%d steps failed\n", failed)
	}
	return b.String()
}

func totalTook(steps []smokeStep) time.Duration {
	var d time.Duration
	for _, s := range steps {
		d += s.took
	}
	return d
}
