package main

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// chimeraStyle mirrors the block set Anvaya's renderer supports (headings,
// paragraphs, fenced code, tables, lists, blockquotes, rules) and its palette:
// orange headings, blue subheads, a recessed code panel, a dim quoted gutter.
const chimeraStyle = `{
  "document":       {"block_prefix": "", "block_suffix": "", "color": "252"},
  "paragraph":      {},
  "heading":        {"block_suffix": "\n", "color": "208", "bold": true},
  "h1":             {"prefix": "", "suffix": "", "color": "208", "bold": true},
  "h2":             {"prefix": "", "suffix": "", "color": "208", "bold": true},
  "h3":             {"prefix": "", "suffix": "", "color": "39",  "bold": true},
  "h4":             {"prefix": "", "suffix": "", "color": "39"},
  "h5":             {"prefix": "", "suffix": "", "color": "39"},
  "h6":             {"prefix": "", "suffix": "", "color": "244"},
  "text":           {},
  "code":           {"color": "203", "background_color": "236"},
  "code_block":     {"color": "252", "background_color": "236", "margin": 1},
  "list":           {"level_indent": 2},
  "item":           {"block_prefix": "• "},
  "enumeration":    {"block_prefix": ". "},
  "block_quote":    {"indent": 1, "indent_token": "│ ", "color": "244"},
  "link":           {"color": "39", "underline": true},
  "link_text":      {"color": "39"},
  "horizontal_rule": {"color": "240"},
  "table":          {"center_separator": "┼", "column_separator": "│", "row_separator": "─"}
}`

// newMarkdown builds a renderer for a given viewport width. It is rebuilt when
// the terminal resizes, because wrapping is baked into the rendered string.
func newMarkdown(width int) (*glamour.TermRenderer, error) {
	return glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes([]byte(chimeraStyle)),
		glamour.WithWordWrap(max(40, width-2)),
		glamour.WithEmoji(),
	)
}

// renderMarkdown renders one assistant message. Plain text passes through on any
// error: a formatting problem must never cost the user the answer itself.
func renderMarkdown(r *glamour.TermRenderer, text string) string {
	if r == nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}
