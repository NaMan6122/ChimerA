package models

import (
	"testing"
)

func TestToChatMessagesStringInput(t *testing.T) {
	r := ResponsesRequest{Model: "m", Instructions: "Be nice.", Input: jsonRaw(`"hello"`)}
	msgs, err := r.ToChatMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if msgs[1].UnmarshalContent() != "hello" {
		t.Fatalf("bad user content %q", msgs[1].UnmarshalContent())
	}
}

func TestToChatMessagesItems(t *testing.T) {
	r := ResponsesRequest{Model: "m", Input: jsonRaw(`[
		{"type":"message","role":"developer","content":"sys"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]},
		{"type":"function_call_output","call_id":"c1","output":{"ok":true}},
		{"type":"reasoning","content":"skip"}
	]`)}
	msgs, err := r.ToChatMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %+v", msgs)
	}
	if msgs[0].Role != "system" || msgs[0].UnmarshalContent() != "sys" {
		t.Fatalf("developer should map to system: %+v", msgs[0])
	}
	if msgs[1].UnmarshalContent() != "a" {
		t.Fatalf("image blocks should be skipped: %q", msgs[1].UnmarshalContent())
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" {
		t.Fatalf("bad tool message: %+v", msgs[2])
	}
}

func TestToChatMessagesBadInput(t *testing.T) {
	r := ResponsesRequest{Model: "m", Input: jsonRaw(`42`)}
	if _, err := r.ToChatMessages(); err == nil {
		t.Fatal("expected error for non-string non-array input")
	}
}

func TestToChatTools(t *testing.T) {
	r := ResponsesRequest{Tools: []ResponsesTool{
		{Type: "function", Name: "f", Description: "d", Parameters: map[string]string{"type": "object"}},
		{Type: "function", Name: ""},
	}}
	tools := r.ToChatTools()
	if len(tools) != 1 || tools[0].Function.Name != "f" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

func TestResponsesTextContent(t *testing.T) {
	if got := ResponsesTextContent(jsonRaw(`"hi"`)); got != "hi" {
		t.Fatalf("string: %q", got)
	}
	if got := ResponsesTextContent(jsonRaw(`[{"type":"output_text","text":"a"},{"type":"refusal","text":"b"}]`)); got != "a" {
		t.Fatalf("blocks: %q", got)
	}
	if got := ResponsesTextContent(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

func jsonRaw(s string) []byte { return []byte(s) }
