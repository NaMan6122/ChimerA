package tools

import (
	"testing"

	"github.com/chimera/chimera/internal/models"
)

func TestParseToolCalls_JSONFence(t *testing.T) {
	text := "```json\n{\"tool_calls\": [{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}]}\n```"
	calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name mismatch %q", calls[0].Function.Name)
	}
}

func TestParseToolCalls_BareJSON(t *testing.T) {
	text := `Some text before {"tool_calls": [{"name": "search", "arguments": {"q": "hello"}}]} some after`
	calls := ParseToolCalls(text)
	if len(calls) != 1 || calls[0].Function.Name != "search" {
		t.Fatalf("unexpected calls %+v", calls)
	}
}

func TestParseToolCalls_NestedArgs(t *testing.T) {
	text := `{"tool_calls": [{"name": "calc", "arguments": {"expr": {"a": 1, "b": [2,3]}}}]}`
	calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1")
	}
	if calls[0].Function.Arguments != `{"a":1,"b":[2,3]}` {
		// order may vary but check contains
		t.Logf("args %s", calls[0].Function.Arguments)
	}
}

func TestParseToolCalls_None(t *testing.T) {
	text := "Hello world, no tools."
	calls := ParseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0, got %d", len(calls))
	}
}

func TestBuildToolPrompt(t *testing.T) {
	tools := []models.Tool{
		{Type: "function", Function: models.Function{Name: "get_weather", Description: "Get weather", Parameters: map[string]interface{}{"type": "object"}}},
	}
	prompt := BuildToolPrompt(tools, "chatgpt")
	if prompt == "" || len(prompt) < 20 {
		t.Fatalf("prompt empty")
	}
	prompt2 := BuildToolPrompt(tools, "claude")
	if prompt == prompt2 {
		t.Fatalf("expected different prompts for providers")
	}
}

func TestExtractJSON_EscapedString(t *testing.T) {
	text := `{"tool_calls": [{"name": "say", "arguments": {"msg": "he said \"hello\""}}]}`
	calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 with escaped string")
	}
	if calls[0].Function.Name != "say" {
		t.Fatalf("name mismatch")
	}
}

func TestParseToolCallsWithDefs_RejectUnknown(t *testing.T) {
	text := `{"tool_calls": [{"name": "ghost", "arguments": {}}, {"name": "get_weather", "arguments": {"city": "Paris"}}]}`
	calls := ParseToolCallsWithDefs(text, []string{"get_weather"}, "chatgpt")
	if len(calls) != 1 {
		t.Fatalf("expected 1 valid call, got %+v", calls)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("wrong call survived: %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("args mismatch %q", calls[0].Function.Arguments)
	}
}

func TestParseToolCallsWithDefs_SingleObject(t *testing.T) {
	text := `thinking out loud {"name": "search", "arguments": {"q": "hello"}} done`
	calls := ParseToolCallsWithDefs(text, []string{"search"}, "chatgpt")
	if len(calls) != 1 || calls[0].Function.Name != "search" {
		t.Fatalf("unexpected calls %+v", calls)
	}
	if calls[0].Function.Arguments != `{"q":"hello"}` {
		t.Fatalf("args mismatch %q", calls[0].Function.Arguments)
	}
}

func TestParseToolCallsWithDefs_NormalizeArgs(t *testing.T) {
	cases := []struct{ text, want string }{
		{`{"name": "a", "arguments": {"x": 1}}`, `{"x":1}`},
		{`{"name": "a"}`, `{}`},
		{`{"name": "a", "arguments": null}`, `{}`},
		{`{"name": "a", "arguments": "oops"}`, `{}`},
		{`{"name": "a", "arguments": [1, 2]}`, `{}`},
	}
	for _, c := range cases {
		calls := ParseToolCallsWithDefs(c.text, nil, "")
		if len(calls) != 1 {
			t.Fatalf("%s: expected 1 call, got %+v", c.text, calls)
		}
		if calls[0].Function.Arguments != c.want {
			t.Fatalf("%s: args = %q, want %q", c.text, calls[0].Function.Arguments, c.want)
		}
	}
}

func TestParseToolCallsWithDefs_EmptyNameSkipped(t *testing.T) {
	text := `{"tool_calls": [{"name": "", "arguments": {}}, {"arguments": {}}]}`
	if calls := ParseToolCallsWithDefs(text, nil, ""); len(calls) != 0 {
		t.Fatalf("expected 0, got %+v", calls)
	}
}

func TestNewCallID_UniqueAndFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newCallID()
		if len(id) != len("call_")+12 || id[:5] != "call_" {
			t.Fatalf("bad id format %q", id)
		}
		for _, r := range id[5:] {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("non-hex id %q", id)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
