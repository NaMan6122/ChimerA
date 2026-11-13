package sessions

// Rehydration — the one legitimate bridge from a preserved session back into
// work. The transcript is distilled into a deterministic, budgeted context
// block that a FRESH upstream conversation can be seeded with: the provider
// is never asked to remember anything, the harness composes the prompt. The
// block is context, not a replay: nothing in it is re-executed.

import (
	"fmt"
	"strings"
)

// DefaultRehydrateBudget bounds the generated context block.
const DefaultRehydrateBudget = 8000

// RehydrateContext distills the transcript at path into a context block for a
// fresh conversation: the original task, a per-round digest (tool calls plus
// the assistant's text, clipped), and the final answer. budget caps the
// block's size (<=0 selects DefaultRehydrateBudget); when the digest would
// exceed it, middle rounds are dropped first — the task, the latest rounds
// and the final answer survive.
func RehydrateContext(path string, budget int) (string, error) {
	if budget <= 0 {
		budget = DefaultRehydrateBudget
	}
	recs, err := load(path)
	if err != nil {
		return "", err
	}
	sum, err := LoadSummary(path)
	if err != nil {
		return "", err
	}
	if sum.ID == "" && len(recs) == 0 {
		return "", fmt.Errorf("%s: empty transcript", path)
	}

	type round struct {
		n     int
		tools []string
		text  string
	}
	var rounds []round
	for _, r := range recs {
		switch r.Kind {
		case "tool":
			if len(rounds) == 0 || rounds[len(rounds)-1].n != r.Round {
				rounds = append(rounds, round{n: r.Round})
			}
			last := &rounds[len(rounds)-1]
			last.tools = append(last.tools, r.Name)
		case "round":
			if r.Text == "" {
				continue
			}
			if len(rounds) == 0 || rounds[len(rounds)-1].n != r.Round {
				rounds = append(rounds, round{n: r.Round})
			}
			cur := &rounds[len(rounds)-1]
			if cur.text != "" {
				cur.text += "\n"
			}
			cur.text += strings.TrimSpace(r.Text)
		}
	}

	// Size the round digests so the whole block fits the budget: the task,
	// header and final answer have fixed claims; the rest goes to rounds.
	head := fmt.Sprintf("You are continuing work from a previous session. The digest below is CONTEXT, not a replay: nothing in it is re-executed. It comes from a preserved transcript; the provider-side conversation it belonged to is gone by design.\n\nPrevious task:\n%s\n\n", sum.Task)
	tail := ""
	if sum.Final != "" {
		tail = "\nPrevious final answer:\n" + clipTo(sum.Final, min(2000, budget/4)) + "\n"
	}
	roundBudget := budget - len(head) - len(tail) - 200
	if roundBudget < 400 {
		roundBudget = 400
	}

	var b strings.Builder
	b.WriteString(head)
	if len(rounds) > 0 {
		per := roundBudget / len(rounds)
		if per > 1200 {
			per = 1200
		}
		fmt.Fprintf(&b, "What happened (%d tool rounds, newest last):\n", len(rounds))
		for _, r := range rounds {
			line := ""
			if len(r.tools) > 0 {
				line += fmt.Sprintf("tools: %s", strings.Join(r.tools, ", "))
			}
			if t := clipTo(oneLineFor(r.text), per-len(line)); t != "" {
				if line != "" {
					line += " — "
				}
				line += t
			}
			if line == "" {
				line = "(no output)"
			}
			fmt.Fprintf(&b, "- round %d: %s\n", r.n, line)
		}
	} else {
		b.WriteString("(the previous run made no tool calls)\n")
	}
	b.WriteString(tail)
	out := b.String()
	if len(out) > budget {
		out = out[:budget] + "\n… [context block truncated at budget]"
	}
	return out, nil
}

// oneLineFor collapses a round's text to a single line for the digest.
func oneLineFor(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clipTo shortens s to at most max runes, marking the cut.
func clipTo(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " …"
}
