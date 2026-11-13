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
	EventTask    EventKind = iota // a task was accepted (recorded, never replayed)
	EventRound                    // a request completed
	EventTool                     // a tool is about to run
	EventResult                   // a tool returned
	EventNudge                    // the model answered without evidence (or refused tools mid-run); asking again
	EventFinal                    // the final answer
	EventCompact                  // upstream context compacted; provider session rotated
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

	// Ledger, when set, accumulates the harness-side working copy (see
	// ledger.go) and enables compaction: when the estimated upstream window
	// crosses CompactChars, the runner rotates to a fresh provider-side
	// session seeded with a digest of the work so far.
	Ledger       *Ledger
	CompactChars int
}

// compactionThreshold returns the effective threshold: unset selects the
// default; negative disables compaction entirely.
func (r *Runner) compactionThreshold() int {
	switch {
	case r.CompactChars > 0:
		return r.CompactChars
	case r.CompactChars < 0:
		return 0 // disabled
	default:
		return DefaultCompactChars
	}
}

// NewRunner wires a runner with defaults.
func NewRunner(c *Client, e *Executor, maxRounds int) *Runner {
	if maxRounds <= 0 {
		maxRounds = 8
	}
	return &Runner{Client: c, Tools: e, MaxRounds: maxRounds}
}

func (r *Runner) emit(ev Event) {
	r.Ledger.Observe(ev)
	if r.OnEvent != nil {
		r.OnEvent(ev)
	}
}

// refusalRe matches the "I cannot do that" shape a model falls into when it
// forgets the tool bridge mid-run. Narrow on purpose: a false positive
// hijacks a legitimate final answer, while the round-1 evidence nudge above
// already covers the common early case.
var refusalRe = regexp.MustCompile(`(?i)(cannot read|can't read|cannot access|can't access|don't have access|do not have access|no access to|unable to access|cannot run|can't run|cannot execute|can't execute|don't have the ability|no tools available|cannot use tools)`)

// refusalReminder re-asserts the bridge after a mid-run refusal.
const refusalReminder = `You DO have the tools from the first turn of this conversation: read_file, list_dir, grep, write_file, edit_file, run_command, git, monitor, read_pdf. To use them, reply with ONLY a JSON code block: {"tool_calls":[{"name":"...","arguments":{...}}]}. If the task is complete instead, reply with plain text.`

// Run performs one task and returns the final answer plus its accounting.
func (r *Runner) Run(task string) (Result, error) {
	res := Result{}
	r.emit(Event{Kind: EventTask, Text: task})
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
	refusalNudged := false
	prevHadCalls := false
	lastSig, repeats := "", 0

	for round := 1; round <= r.MaxRounds; round++ {
		// Recomputed per round: after a compaction the fresh upstream session
		// must carry the system prompt and schemas again, which a loop-invariant
		// flag would miss.
		withTools = r.Client.Turns == 0
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
				r.emit(Event{Kind: EventNudge, Round: round, Text: "answered without reading anything; asked for evidence"})
				pending = []Message{{
					Role:    "user",
					Content: "You answered without reading anything. Read the relevant file(s) with read_file (or locate them with grep) before answering, then give the final answer.",
				}}
				continue
			}
			// Mid-run refusal: earlier rounds used tools, now the model claims
			// it cannot — that is the bridge being forgotten, not a final
			// answer. Re-assert once, then let the run conclude either way.
			if round > 1 && prevHadCalls && !refusalNudged && refusalRe.MatchString(reply.Content) {
				refusalNudged = true
				res.Rounds = append(res.Rounds, stat)
				r.emit(Event{Kind: EventNudge, Round: round, Text: "refused tools mid-run; re-asserted the bridge"})
				pending = []Message{{Role: "user", Content: refusalReminder}}
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
		prevHadCalls = true

		// Compaction: when the estimated provider-side window is full, rotate
		// to a fresh upstream session seeded with a digest of the work. The
		// harness-side session — transcript, ledger — never changes.
		if thr := r.compactionThreshold(); r.Ledger != nil && thr > 0 && r.Ledger.EstUpstream() > thr {
			newUpstream := r.Ledger.Rotate()
			r.Client.NewSession(newUpstream)
			seed := r.Ledger.CompactSummary(task)
			pending = []Message{
				{Role: "system", Content: SystemPrompt},
				{Role: "user", Content: seed},
			}
			r.emit(Event{Kind: EventCompact, Round: round,
				Text: fmt.Sprintf("context compacted after ~%d estimated chars; fresh upstream session %s", thr, newUpstream)})
		}
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
