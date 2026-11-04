// chimera-spin is the headless front-end: one task in, one answer out, with
// per-round accounting on stderr.
//
// It holds no transcript. The provider keeps the conversation, so turn 1 sends
// the system prompt, the task and six tool schemas, and every later turn sends
// only the tool results from the previous round.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

// version is stamped at install time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		baseURL, token, model, session, root, jsonOut, task string
		maxRounds                                           int
		timeout                                             time.Duration
		showVersion                                         bool
	)
	flag.StringVar(&baseURL, "base-url", envOr("CHIMERA_BASE_URL", "http://localhost:8000/v1"), "gateway base URL")
	flag.StringVar(&token, "token", envOr("CHIMERA_API_KEY", "chimera"), "bearer token")
	flag.StringVar(&model, "model", "chimera-chatgpt", "model id")
	flag.StringVar(&session, "session", "", "session id (default: a fresh one per run)")
	flag.StringVar(&root, "root", ".", "workspace root the tools are confined to")
	flag.IntVar(&maxRounds, "max-rounds", 8, "hard stop on tool rounds")
	flag.DurationVar(&timeout, "timeout", 180*time.Second, "per-turn HTTP timeout")
	flag.StringVar(&jsonOut, "json", "", "write a run report as JSON")
	flag.StringVar(&task, "task", "", "task for the agent (or pass it as the last argument)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("chimera-spin " + version)
		return
	}

	if task == "" && flag.NArg() > 0 {
		task = strings.Join(flag.Args(), " ")
	}
	if strings.TrimSpace(task) == "" {
		fmt.Fprintln(os.Stderr, `usage: chimera-spin [flags] "<task>"

examples:
  chimera-spin "where is MAX_PROMPT_CHARS enforced?"
  chimera-spin -model chimera-deepseek -base-url http://localhost:8010/v1 "..."
  chimera-spin -root ~/code/project "summarise internal/api/server.go"

flags:`)
		flag.PrintDefaults()
		os.Exit(2)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "root:", err)
		os.Exit(1)
	}
	if session == "" {
		session = fmt.Sprintf("spin-%d", time.Now().UnixNano())
	}

	client := spin.NewClient(baseURL, token, model, session)
	client.HTTP.Timeout = timeout
	if err := client.CheckModel(); err != nil {
		fmt.Fprintln(os.Stderr, "spin:", err)
		os.Exit(1)
	}
	client.Refresh()

	runner := spin.NewRunner(client, &spin.Executor{Root: absRoot}, maxRounds)
	runner.OnEvent = func(ev spin.Event) {
		if ev.Kind == spin.EventNudge {
			fmt.Fprintln(os.Stderr, "[spin] no tool calls on a repo question — asking for evidence")
		}
	}

	started := time.Now()
	res, err := runner.Run(task)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spin:", err)
		os.Exit(1)
	}
	fmt.Println(res.Text)
	report(started, session, model, task, res)

	if jsonOut != "" {
		rep := map[string]any{
			"session": session, "model": model, "task": task,
			"rounds": res.Rounds, "tool_calls": res.Tools,
			"wall_s": time.Since(started).Seconds(), "completed": true,
		}
		if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
			_ = os.WriteFile(jsonOut, b, 0o644)
		}
	}
}

// report prints per-round accounting to stderr, keeping stdout to the answer.
func report(started time.Time, session, model, task string, res spin.Result) {
	total, prompts := time.Since(started), 0
	for _, r := range res.Rounds {
		prompts += r.Prompt
	}
	fmt.Fprintf(os.Stderr, "\n[spin] rounds=%d tools=%d prompt_chars=%d wall=%.1fs session=%s\n",
		len(res.Rounds), res.Tools, prompts, total.Seconds(), session)
	for _, r := range res.Rounds {
		fmt.Fprintf(os.Stderr, "[spin]   round %d: %d chars, %.1fs, tools=%v\n", r.Round, r.Prompt, r.Latency, r.Calls)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
