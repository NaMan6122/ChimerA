// Package main implements chimera-spin: a small agent loop that drives a
// Chimera gateway with a compact, purpose-built prompt.
//
// The gateway exposes consumer chat models behind an OpenAI shape. Those models
// are good reasoners and mediocre tool-callers, and they pay for every character
// of prompt twice (upload + prefill). A general-purpose harness sends 45k+ chars
// of system prompt and tool schemas per turn; this one sends ~1.5k of tools and
// only the messages that are new since the previous round, relying on the
// gateway's upstream conversation for continuity via X-Session-Id.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []toolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type spin struct {
	cfg    config
	http   *http.Client
	rounds []roundStat
	tools  int
}

type roundStat struct {
	Round   int     `json:"round"`
	Prompt  int     `json:"prompt_chars"`
	Latency float64 `json:"latency_s"`
	Calls   []string
}

type config struct {
	baseURL   string
	token     string
	model     string
	task      string
	session   string
	root      string
	maxRounds int
	timeout   time.Duration
	jsonOut   string
}

// version is stamped at install time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var cfg config
	var showVersion bool
	flag.StringVar(&cfg.baseURL, "base-url", envOr("CHIMERA_BASE_URL", "http://localhost:8000/v1"), "gateway base URL")
	flag.StringVar(&cfg.token, "token", envOr("CHIMERA_API_KEY", "chimera"), "bearer token")
	flag.StringVar(&cfg.model, "model", "chimera-chatgpt", "model id")
	flag.StringVar(&cfg.session, "session", "", "session id (default: a fresh one per run)")
	flag.StringVar(&cfg.root, "root", ".", "workspace root the tools are confined to")
	flag.IntVar(&cfg.maxRounds, "max-rounds", 8, "hard stop on tool rounds")
	flag.DurationVar(&cfg.timeout, "timeout", 180*time.Second, "per-turn HTTP timeout")
	flag.StringVar(&cfg.jsonOut, "json", "", "write a run report as JSON")
	var prompt string
	flag.StringVar(&prompt, "task", "", "task for the agent (or pass it as the last argument)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("chimera-spin " + version)
		return
	}

	task := prompt
	if task == "" && flag.NArg() > 0 {
		task = strings.Join(flag.Args(), " ")
	}
	if strings.TrimSpace(task) == "" {
		fmt.Fprintln(os.Stderr, "usage: chimera-spin [flags] \"<task>\"")
		flag.PrintDefaults()
		os.Exit(2)
	}
	cfg.task = task

	root, err := absPath(cfg.root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "root:", err)
		os.Exit(1)
	}
	cfg.root = root
	if cfg.session == "" {
		cfg.session = fmt.Sprintf("spin-%d", time.Now().UnixNano())
	}

	s := &spin{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.timeout},
	}
	if err := s.run(); err != nil {
		fmt.Fprintln(os.Stderr, "spin:", err)
		os.Exit(1)
	}
}

func (s *spin) run() error {
	started := time.Now()
	s.refresh()

	// Turn 1 carries the persona and the task; every later turn carries only what
	// is new (tool results), because the upstream conversation already holds the
	// rest.
	pending := []message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: s.cfg.task},
	}

	var lastSig string
	repeats := 0
	nudged := false
	for round := 1; round <= s.cfg.maxRounds; round++ {
		// Tool schemas are sent on the first turn only. The upstream conversation
		// keeps them from then on; re-sending them is exactly the accumulation this
		// harness exists to avoid.
		reply, elapsed, err := s.complete(pending, round == 1)
		if err != nil {
			return err
		}
		stat := roundStat{Round: round, Prompt: promptChars(pending), Latency: elapsed.Seconds()}

		// Tool calls arrive structured when schemas were declared, and as a JSON
		// block in the text once they are not. Both are the same protocol; parsing
		// the text locally is what lets follow-up turns carry no schemas at all.
		calls := reply.ToolCalls
		if len(calls) == 0 {
			calls = parseToolCalls(reply.Content)
		}

		if len(calls) == 0 {
			// A repo question answered without reading anything is a guess. Nudge
			// once before accepting it; measured: the model happily answers
			// "what does applyStealth do" from memory and gets the detail wrong.
			if round == 1 && !nudged && looksLikeRepoQuestion(s.cfg.task) {
				nudged = true
				s.rounds = append(s.rounds, stat)
				fmt.Fprintln(os.Stderr, "[spin] no tool calls on a repo question — asking for evidence")
				pending = []message{{
					Role:    "user",
					Content: "You answered without reading anything. Read the relevant file(s) with read_file (or locate them with grep) before answering, then give the final answer.",
				}}
				continue
			}
			s.rounds = append(s.rounds, stat)
			fmt.Println(strings.TrimSpace(reply.Content))
			s.report(started, true)
			return nil
		}

		// One round's worth of tool output is capped in aggregate, not per call:
		// eight 12k reads in a single turn cost 65k of prompt on the next one, and
		// the model rarely needs every line of every file.
		results := make([]message, 0, len(calls))
		budget := maxRoundOutput
		for _, tc := range calls {
			stat.Calls = append(stat.Calls, tc.Function.Name)
			out := s.exec(tc)
			if len(out) > budget {
				if budget <= 0 {
					out = fmt.Sprintf("(output withheld: this round's %d-byte budget is spent; request fewer or narrower tools)", maxRoundOutput)
				} else {
					out = out[:budget] + fmt.Sprintf("\n… [round output budget of %d bytes reached]", maxRoundOutput)
				}
			}
			if budget > 0 {
				budget -= len(out)
			}
			results = append(results, message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
		s.rounds = append(s.rounds, stat)
		s.tools += len(reply.ToolCalls)

		// Loop guard: identical calls three rounds running will not converge.
		sig := signature(calls)
		if sig == lastSig {
			repeats++
		} else {
			lastSig, repeats = sig, 1
		}
		if repeats >= 3 {
			return fmt.Errorf("stopping: the same tool call was issued %d rounds running", repeats)
		}
		pending = results
	}
	return fmt.Errorf("hit the %d-round budget without a final answer", s.cfg.maxRounds)
}

// repoQuestionRe matches tasks that can only be answered from the workspace:
// a source path, a file extension, or an explicit instruction to read.
var repoQuestionRe = regexp.MustCompile(`(?i)(\.go\b|\.md\b|internal/|cmd/|docs/|scripts/|read the|look at|in this repo|the file|the code|\b[a-z]+[A-Z][A-Za-z]*\b|function|struct|method|handler|provider)`)

// looksLikeRepoQuestion reports whether a task should be answered from files
// rather than from the model's memory.
func looksLikeRepoQuestion(task string) bool { return repoQuestionRe.MatchString(task) }

// signature identifies a round's calls, so a repeated round is detectable.
func signature(calls []toolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Function.Name+"("+c.Function.Arguments+")")
	}
	return strings.Join(parts, "|")
}

// baseRoot strips the /v1 suffix: refresh lives at the gateway root.
func baseRoot(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// complete performs one turn and returns the assistant message. withTools is
// true only for the first turn: the upstream conversation keeps the schemas from
// then on, and repeating them is the accumulation this harness is built to avoid.
func (s *spin) complete(msgs []message, withTools bool) (message, time.Duration, error) {
	payload := chatRequest{Model: s.cfg.model, Messages: msgs}
	if withTools {
		payload.Tools = toolDefs()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return message{}, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.cfg.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	// Session continuity: the gateway keeps one upstream conversation per id and
	// sends only deltas to the web model.
	req.Header.Set("X-Session-Id", s.cfg.session)

	t0 := time.Now()
	resp, err := s.http.Do(req)
	if err != nil {
		return message{}, 0, fmt.Errorf(`cannot reach the Chimera gateway at %s
  %v

  Start it first:
    TRANSPORT=webapi PROVIDER=chatgpt ./chimera
  or point this harness elsewhere:
    chimera-spin -base-url http://host:port/v1 "..."`, s.cfg.baseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	elapsed := time.Since(t0)

	if resp.StatusCode != http.StatusOK {
		return message{}, elapsed, fmt.Errorf("gateway %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return message{}, elapsed, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return message{}, elapsed, fmt.Errorf("gateway error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return message{}, elapsed, fmt.Errorf("gateway returned no choices")
	}
	return out.Choices[0].Message, elapsed, nil
}

// refresh resets the provider conversation so a retried task never inherits the
// previous run's transcript.
func (s *spin) refresh() {
	u := strings.TrimRight(baseRoot(s.cfg.baseURL), "/") + "/v1/refresh"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.http.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
}

// repeatedToolCall guards the loop: once the model has issued the same call
// three rounds running it is not going to converge.
func repeatedToolCall(total, round int) bool { return total > 0 && round >= 4 && total == round }

func (s *spin) report(started time.Time, ok bool) {
	total := time.Since(started)
	var prompts int
	for _, r := range s.rounds {
		prompts += r.Prompt
	}
	fmt.Fprintf(os.Stderr, "\n[spin] rounds=%d tools=%d prompt_chars=%d wall=%.1fs session=%s\n",
		len(s.rounds), s.tools, prompts, total.Seconds(), s.cfg.session)
	for _, r := range s.rounds {
		fmt.Fprintf(os.Stderr, "[spin]   round %d: %d chars, %.1fs, tools=%v\n", r.Round, r.Prompt, r.Latency, r.Calls)
	}
	if s.cfg.jsonOut != "" {
		rep := map[string]any{
			"session": s.cfg.session, "model": s.cfg.model, "task": s.cfg.task,
			"rounds": s.rounds, "tool_calls": s.tools, "wall_s": total.Seconds(),
			"completed": ok,
		}
		if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
			_ = os.WriteFile(s.cfg.jsonOut, b, 0o644)
		}
	}
}

func promptChars(msgs []message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
