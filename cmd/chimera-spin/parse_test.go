package main

import "testing"

// TestParseToolCalls pins the shapes the web model actually emits: a fenced
// json block, a bare single-call object, and nested argument objects that a
// naive brace scan would cut in half.
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
			got := parseToolCalls(tc.in)
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

// TestResolveConfinesToWorkspace is the safety property: a model must not be able
// to read or write outside the workspace it was given.
func TestResolveConfinesToWorkspace(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../outside", "../../etc/passwd", "/etc/passwd"} {
		if _, err := resolve(root, bad); err == nil {
			t.Errorf("resolve(%q) should have been rejected", bad)
		}
	}
	if _, err := resolve(root, "internal/api/server.go"); err != nil {
		t.Errorf("relative path rejected: %v", err)
	}
}
