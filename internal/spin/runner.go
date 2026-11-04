package spin

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// EventKind labels what a Runner is reporting, so a UI can render progress
// without knowing the loop's internals.
type EventKind int

const (
	EventRound  EventKind = iota // a request completed
	EventTool                    // a tool is about to run
	EventResult                  // a tool returned
	EventNudge                   // the model answered without evidence; asking again
	EventFinal                   // the final answer
)

// Event is one observable step of a run.
type Event struct {
	Kind        EventKind
	Round       int
	PromptChars int
	Latency     time.Duration
	Tool        ToolCall
	Result      string
	Text        string
}

// RoundStat is the per-round accounting used by both front-ends.
type RoundStat struct {
	Round   int     `json:"round"`
	Prompt  int     `json:"prompt_chars"`
	Latency float64 `json:"latency_s"`
	Calls   []string
}

// Result is the outcome of one task.
type Result struct {
	Text   string      `json:"text"`
	Rounds []RoundStat `json:"rounds"`
	Tools  int         `json:"tool_calls"`
}

// Runner executes one task at a time against a Client. It holds no transcript:
// each task starts with its own task message and every subsequent turn carries
// only the previous round's tool results.
type Runner struct {
	Client    *Client
	Tools     *Executor
	MaxRounds int
	OnEvent   func(Event)
}

// NewRunner wires a runner with defaults.
func NewRunner(c *Client, e *Executor, maxRounds int) *Runner {
	if maxRounds <= 0 {
		maxRounds = 8
	}
	return &Runner{Client: c, Tools: e, MaxRounds: maxRounds}
}

func (r *Runner) emit(ev Event) {
	if r.OnEvent != nil {
		r.OnEvent(ev)
	}
}

// Run performs one task and returns the final answer plus its accounting.
func (r *Runner) Run(task string) (Result, error) {
	res := Result{}
	// The system prompt and the tool schemas go out once per upstream
	// conversation, not once per task: in a chat, turns 2..n carry only what the
	// user just typed.
	withTools := r.Client.Turns == 0
	pending := []Message{}
	if withTools {
		pending = append(pending, Message{Role: "system", Content: SystemPrompt})
	}
	pending = append(pending, Message{Role: "user", Content: task})
	nudged := false
	lastSig, repeats := "", 0

	for round := 1; round <= r.MaxRounds; round++ {
		reply, elapsed, err := r.Client.Complete(pending, withTools)
		if err != nil {
			return res, err
		}
		stat := RoundStat{Round: round, Prompt: promptChars(pending), Latency: elapsed.Seconds()}

		calls := reply.ToolCalls
		if len(calls) == 0 {
			calls = ParseToolCalls(reply.Content)
		}
		r.emit(Event{Kind: EventRound, Round: round, PromptChars: stat.Prompt, Latency: elapsed, Text: reply.Content})

		if len(calls) == 0 {
			if round == 1 && !nudged && LooksLikeRepoQuestion(task) {
				nudged = true
				res.Rounds = append(res.Rounds, stat)
				r.emit(Event{Kind: EventNudge, Round: round})
				pending = []Message{{
					Role:    "user",
					Content: "You answered without reading anything. Read the relevant file(s) with read_file (or locate them with grep) before answering, then give the final answer.",
				}}
				withTools = false
				continue
			}
			res.Rounds = append(res.Rounds, stat)
			res.Text = strings.TrimSpace(reply.Content)
			r.emit(Event{Kind: EventFinal, Round: round, Text: res.Text})
			return res, nil
		}

		budget := MaxRoundOutput
		results := make([]Message, 0, len(calls))
		for _, tc := range calls {
			stat.Calls = append(stat.Calls, tc.Function.Name)
			r.emit(Event{Kind: EventTool, Round: round, Tool: tc})
			out := r.Tools.Exec(tc)
			if len(out) > budget {
				if budget <= 0 {
					out = fmt.Sprintf("(output withheld: this round's %d-byte budget is spent; request fewer or narrower tools)", MaxRoundOutput)
				} else {
					out = out[:budget] + fmt.Sprintf("\n… [round output budget of %d bytes reached]", MaxRoundOutput)
				}
			}
			if budget > 0 {
				budget -= len(out)
			}
			r.emit(Event{Kind: EventResult, Round: round, Tool: tc, Result: out})
			results = append(results, Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
		res.Rounds = append(res.Rounds, stat)
		res.Tools += len(calls)

		// Loop guard: identical calls three rounds running will not converge.
		sig := signature(calls)
		if sig == lastSig {
			repeats++
		} else {
			lastSig, repeats = sig, 1
		}
		if repeats >= 3 {
			return res, fmt.Errorf("stopping: the same tool call was issued %d rounds running", repeats)
		}

		// Only the new results travel: the upstream conversation holds the rest.
		pending = results
		withTools = false
	}
	return res, fmt.Errorf("hit the %d-round budget without a final answer", r.MaxRounds)
}

func promptChars(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func signature(calls []ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Function.Name+"("+c.Function.Arguments+")")
	}
	return strings.Join(parts, "|")
}

var (
	// Case-insensitive: paths and plain words.
	repoWordsRe = regexp.MustCompile(`(?i)(\.go\b|\.md\b|internal/|cmd/|docs/|scripts/|read the|look at|in this repo|the file|the code|function|struct|method|handler|provider)`)
	// Case-SENSITIVE: Go-style identifiers. Under (?i) this pattern matches any
	// two-letter word, which classified "Reply with exactly: READY" as a repo
	// question and burned a round asking for evidence.
	repoIdentRe = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z]*\b`)
)

// LooksLikeRepoQuestion reports whether a task should be answered from files
// rather than from the model's memory.
func LooksLikeRepoQuestion(task string) bool {
	return repoWordsRe.MatchString(task) || repoIdentRe.MatchString(task)
}
