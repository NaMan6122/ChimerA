// Package models — OpenAI Responses API (/v1/responses) schemas.
//
// Subset of the Responses API sufficient for chat + function calling:
// string or item-list input, instructions, flat function tools,
// function_call_output round-trips. Streaming events are out of scope.
package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ── Responses Request ─────────────────────────────────────────

// ResponsesRequest is an OpenAI-compatible /v1/responses request.
type ResponsesRequest struct {
	Model           string            `json:"model"`
	Instructions    string            `json:"instructions,omitempty"`
	Input           json.RawMessage   `json:"input"`
	Tools           []ResponsesTool   `json:"tools,omitempty"`
	ToolChoice      interface{}       `json:"tool_choice,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	MaxOutputTokens *int              `json:"max_output_tokens,omitempty"`
	User            string            `json:"user,omitempty"`
}

// ResponsesTool is a flat function tool (unlike chat's nested {function:{...}}).
type ResponsesTool struct {
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// ResponsesInputItem is one entry of an array-form input.
type ResponsesInputItem struct {
	Type    string          `json:"type"` // "message" | "function_call_output"
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
}

// ToChatTools converts flat response tools to chat tool definitions.
func (r *ResponsesRequest) ToChatTools() []Tool {
	out := make([]Tool, 0, len(r.Tools))
	for _, t := range r.Tools {
		if t.Name == "" {
			continue
		}
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, Tool{
			Type: typ,
			Function: Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// ToChatMessages converts instructions + input into chat messages.
// function_call_output items become role:"tool" messages so buildPrompt
// folds them exactly like chat tool results.
func (r *ResponsesRequest) ToChatMessages() ([]ChatMessage, error) {
	var msgs []ChatMessage
	if strings.TrimSpace(r.Instructions) != "" {
		msgs = append(msgs, ChatMessage{
			Role:    "system",
			Content: mustMarshalString(r.Instructions),
		})
	}
	if len(bytesTrimSpace(r.Input)) == 0 {
		return msgs, nil
	}
	var text string
	if err := json.Unmarshal(r.Input, &text); err == nil {
		if strings.TrimSpace(text) != "" {
			msgs = append(msgs, ChatMessage{Role: "user", Content: mustMarshalString(text)})
		}
		return msgs, nil
	}
	var items []ResponsesInputItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or item array: %w", err)
	}
	for _, it := range items {
		switch it.Type {
		case "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			if t := ResponsesTextContent(it.Content); strings.TrimSpace(t) != "" {
				msgs = append(msgs, ChatMessage{Role: role, Content: mustMarshalString(t)})
			}
		case "function_call_output":
			out := ResponsesTextContent(it.Output)
			if out == "" && len(it.Output) > 0 {
				out = string(it.Output)
			}
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				Content:    mustMarshalString(out),
				ToolCallID: it.CallID,
			})
		default:
			// Unknown item types (reasoning, etc.) are ignored, not fatal.
			continue
		}
	}
	return msgs, nil
}

// ResponsesTextContent extracts text from a string or content-block array.
// Understands input_text/output_text/text block types; other blocks are skipped.
func ResponsesTextContent(raw json.RawMessage) string {
	if len(bytesTrimSpace(raw)) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "output_text", "text":
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func mustMarshalString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// ── Responses Response ────────────────────────────────────────

// ResponsesUsage counts tokens with Responses API field names.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponseOutputContent is one content part of a message output item.
type ResponseOutputContent struct {
	Type string `json:"type"` // "output_text"
	Text string `json:"text"`
}

// ResponseOutputItem is one entry of the response output array.
type ResponseOutputItem struct {
	Type      string                  `json:"type"` // "message" | "function_call"
	Role      string                  `json:"role,omitempty"`
	Content   []ResponseOutputContent `json:"content,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"`
}

// ResponseObject is an OpenAI-compatible /v1/responses response.
type ResponseObject struct {
	ID     string               `json:"id"`
	Object string               `json:"object"`
	Created int64               `json:"created_at"`
	Model  string               `json:"model"`
	Status string               `json:"status"`
	Output []ResponseOutputItem `json:"output"`
	Usage  ResponsesUsage       `json:"usage"`
}

// NewResponseObjectID generates an OpenAI-style response ID (resp_...).
func NewResponseObjectID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}
