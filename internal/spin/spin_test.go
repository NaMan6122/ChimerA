package spin

import "testing"

// TestParseToolCalls pins the shapes the web model actually emits: a fenced json
// block, a bare single-call object, and nested argument objects that a naive
// brace scan would cut in half.
func TestParseToolCalls(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		path string
	}{
		{
			name: "fenced batch",
			in:   "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}]}\n```",
			want: 1, path: "a.go",
		},
		{
			name: "bare single call",
			in:   "{\"name\":\"read_file\",\"arguments\":{\"path\":\"internal/api/server.go\"}}",
			want: 1, path: "internal/api/server.go",
		},
		{
			name: "nested arguments survive",
			in:   "{\"tool_calls\":[{\"name\":\"run_command\",\"arguments\":{\"command\":\"echo {\\\"a\\\":1}\"}}]}",
			want: 1,
		},
		{
			name: "plain prose is not a call",
			in:   "I could not find that file.",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseToolCalls(tc.in)
			if len(got) != tc.want {
				t.Fatalf("got %d calls, want %d (%+v)", len(got), tc.want, got)
			}
			if tc.path != "" && got[0].Function.Arguments != `{"path":"`+tc.path+`"}` {
				t.Errorf("arguments = %s", got[0].Function.Arguments)
			}
			for _, c := range got {
				if c.ID == "" || c.Type != "function" {
					t.Errorf("call not normalised: %+v", c)
				}
			}
		})
	}
}

// TestResolveConfinesToWorkspace is the safety property: a model must not read or
// write outside the workspace it was given.
func TestResolveConfinesToWorkspace(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for _, bad := range []string{"../outside", "../../etc/passwd", "/etc/passwd"} {
		if _, err := e.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) should have been rejected", bad)
		}
	}
	if _, err := e.Resolve("internal/api/server.go"); err != nil {
		t.Errorf("relative path rejected: %v", err)
	}
}

// TestLooksLikeRepoQuestion pins the heuristic that decides whether a zero-tool
// answer gets challenged. The case-sensitive half matters: under (?i) every
// two-letter word matched and "Reply with exactly: READY" was nudged.
func TestLooksLikeRepoQuestion(t *testing.T) {
	for _, yes := range []string{
		"what does applyStealth do to navigator.webdriver?",
		"summarise internal/api/server.go",
		"read the README",
	} {
		if !LooksLikeRepoQuestion(yes) {
			t.Errorf("expected repo question: %q", yes)
		}
	}
	for _, no := range []string{"Reply with exactly: READY", "what is 17 times 23"} {
		if LooksLikeRepoQuestion(no) {
			t.Errorf("not a repo question: %q", no)
		}
	}
}
