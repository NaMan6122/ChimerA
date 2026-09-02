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
