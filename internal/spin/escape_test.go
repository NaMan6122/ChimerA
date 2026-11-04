package spin

import "testing"

// TestStripEscapeTags pins the transport artifact: chatgpt.com wraps fenced
// blocks in <escape> markers, which break Markdown rendering and leak into
// answers if left alone.
func TestStripEscapeTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<escape>```go\nfmt.Println()\n```</escape>", "```go\nfmt.Println()\n```"},
		{"plain answer", "plain answer"},
		{"a <escape>b</escape> c", "a b c"},
	}
	for _, c := range cases {
		if got := stripEscapeTags(c.in); got != c.want {
			t.Errorf("stripEscapeTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
