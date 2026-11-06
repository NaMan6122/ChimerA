package sessions

// HTML rendering of a session runbook: the same content model as
// RenderMarkdown, emitted as ONE self-contained file — inline CSS only, no
// JavaScript, no external assets — so it can be archived, mailed or printed
// as-is. Every dynamic string is escaped; the only interactivity is the
// print dialog.

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// RenderHTML renders one transcript as a printable, self-contained HTML
// runbook: a metadata table (with tags), the task, a section per round with
// tool calls and results, and the final answer. It fails only when the
// transcript cannot be read.
func RenderHTML(path string) (string, error) {
	recs, err := load(path)
	if err != nil {
		return "", err
	}
	sum, err := LoadSummary(path)
	if err != nil {
		return "", err
	}
	esc := html.EscapeString

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<title>Session runbook — " + esc(sum.ID) + "</title>\n")
	b.WriteString(styleBlock)
	b.WriteString("</head>\n<body>\n")
	b.WriteString("<h1>Session runbook — " + esc(sum.ID) + "</h1>\n")
	b.WriteString("<p class=\"notice\">Preserved for reading, not replay: the provider side of this " +
		"conversation has likely expired, and the harness never stored anything to re-send. " +
		"Re-run the task below by hand, or reuse the final answer.</p>\n")

	// Metadata table: the same fields as the Markdown front matter.
	b.WriteString("<table class=\"meta\">\n<tbody>\n")
	meta := func(k, v string) {
		b.WriteString("<tr><th>" + esc(k) + "</th><td>" + v + "</td></tr>\n")
	}
	meta("Started", esc(sum.Started.Format(time.RFC3339)))
	if sum.Model != "" {
		meta("Model", "<code>"+esc(sum.Model)+"</code>")
	}
	if sum.BaseURL != "" {
		meta("Gateway", "<code>"+esc(sum.BaseURL)+"</code>")
	}
	if sum.Root != "" {
		meta("Workspace", "<code>"+esc(sum.Root)+"</code>")
	}
	if len(sum.Tags) > 0 {
		chips := make([]string, 0, len(sum.Tags))
		for _, t := range sum.Tags {
			chips = append(chips, "<span class=\"tag\">"+esc(t)+"</span>")
		}
		meta("Tags", strings.Join(chips, " "))
	}
	meta("Rounds", fmt.Sprintf("%d", sum.Rounds))
	meta("Tool calls", fmt.Sprintf("%d", sum.ToolCalls))
	if sum.Errors > 0 {
		meta("Errors", fmt.Sprintf("%d", sum.Errors))
	}
	if sum.Duration > 0 {
		meta("Duration", esc(sum.Duration.Round(time.Millisecond).String()))
	}
	b.WriteString("</tbody>\n</table>\n")

	// Body: the same walk, the same groupings as the Markdown renderer.
	if sum.Task != "" {
		b.WriteString("<h2>Task</h2>\n<p>" + esc(sum.Task) + "</p>\n")
	}
	b.WriteString("<h2>Run</h2>\n")
	currentRound := -1
	for _, r := range recs {
		switch r.Kind {
		case "round":
			if r.Round != currentRound {
				currentRound = r.Round
				lat := ""
				if r.LatencyS > 0 {
					lat = fmt.Sprintf(" · %.1fs", r.LatencyS)
				}
				b.WriteString("<h3>Round " + fmt.Sprintf("%d", r.Round) + esc(lat) + "</h3>\n")
			}
			if txt := strings.TrimSpace(r.Text); txt != "" {
				b.WriteString("<p class=\"assistant\">" + esc(txt) + "</p>\n")
			}
		case "tool":
			b.WriteString("<p class=\"tool\"><strong>Tool → <code>" + esc(r.Name) + "</code></strong></p>\n")
			if args := strings.TrimSpace(r.Arguments); args != "" && args != "{}" {
				b.WriteString(preBlock(args))
			}
		case "result":
			b.WriteString("<p class=\"tool\"><strong>Result ← <code>" + esc(r.Name) + "</code></strong></p>\n")
			if res := strings.TrimSpace(r.Result); res != "" {
				b.WriteString(preBlock(trimResult(res)))
			}
		case "final":
			// The final answer gets its own section, same as Markdown.
		case "note":
			if txt := strings.TrimSpace(r.Text); txt != "" {
				b.WriteString("<blockquote>" + esc(txt) + "</blockquote>\n")
			}
		case "error":
			b.WriteString("<p class=\"error\"><strong>ERROR:</strong> <code>" +
				esc(strings.TrimSpace(r.Text)) + "</code></p>\n")
		}
	}
	if sum.Final != "" {
		b.WriteString("<h2>Final answer</h2>\n<p class=\"final\">" + esc(sum.Final) + "</p>\n")
	}
	b.WriteString("</body>\n</html>\n")
	return b.String(), nil
}

// preBlock wraps a chunk of literal text (tool arguments, tool results) as
// escaped preformatted code.
func preBlock(s string) string {
	return "<pre><code>" + html.EscapeString(s) + "</code></pre>\n"
}

// styleBlock is the whole of the styling: one inline <style> element, no
// url(), no import, no web fonts — a runbook must survive offline and on
// paper. Keep it that way; the self-containment test checks it.
const styleBlock = `<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body {
    font: 15px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    color: #1c1c1c; background: #fff;
    max-width: 54rem; margin: 0 auto; padding: 2.5rem 1.25rem 4rem;
  }
  h1 { font-size: 1.45rem; margin: 0 0 .4rem; padding-bottom: .45rem; border-bottom: 2px solid #1c1c1c; }
  h2 { font-size: 1.1rem; margin: 2.2rem 0 .6rem; padding-bottom: .25rem; border-bottom: 1px solid #ddd; break-after: avoid; }
  h3 { font-size: .98rem; margin: 1.6rem 0 .45rem; color: #444; break-after: avoid; }
  p { margin: .5rem 0; }
  .notice { color: #666; font-size: .88rem; font-style: italic; margin: .8rem 0 1.2rem; }
  table.meta { border-collapse: collapse; margin: 1rem 0 1.4rem; font-size: .88rem; }
  table.meta th, table.meta td { border: 1px solid #ddd; padding: .28rem .65rem; text-align: left; vertical-align: top; }
  table.meta th { background: #f5f5f5; font-weight: 600; white-space: nowrap; }
  code { font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; font-size: .86em;
         background: #f4f4f4; padding: .08em .32em; border-radius: 3px; }
  pre { background: #f7f7f7; border: 1px solid #e2e2e2; border-radius: 4px; padding: .6rem .8rem;
        margin: .4rem 0 .9rem; overflow-x: auto; break-inside: avoid; }
  pre code { background: none; padding: 0; font-size: .82rem; line-height: 1.45;
             white-space: pre-wrap; word-break: break-word; }
  blockquote { margin: .7rem 0; padding: .15rem .9rem; border-left: 3px solid #bbb; color: #555; }
  .assistant { white-space: pre-wrap; }
  .tool { margin: .8rem 0 .15rem; }
  .error { color: #a40000; }
  .error code { background: #fbeaea; }
  .tag { display: inline-block; margin-right: .3rem; padding: 0 .55rem; border: 1px solid #c5cae9;
         background: #eef0fb; border-radius: 999px; font-size: .78rem; }
  .final { font-size: 1rem; background: #f7f9f4; border: 1px solid #dfe6d8; border-radius: 4px;
           padding: .7rem .9rem; white-space: pre-wrap; }
  @media print {
    body { max-width: none; padding: 0; font-size: 11pt; }
    pre, blockquote, table.meta, .final { border-color: #999; }
    pre { break-inside: avoid; }
  }
</style>
`
