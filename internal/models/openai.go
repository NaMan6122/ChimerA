// Package models defines OpenAI-compatible request/response schemas.
package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// ── Chat Completions Request ──────────────────────────────────

// ChatCompletionRequest represents an OpenAI-compatible chat request.
type ChatCompletionRequest struct {
	Model       string            `json:"model"`
	Messages    []ChatMessage     `json:"messages"`
	Stream      bool              `json:"stream,omitempty"`
	Tools       []Tool            `json:"tools,omitempty"`
	ToolChoice  interface{}       `json:"tool_choice,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	MaxTokens   *int              `json:"max_tokens,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	User        string            `json:"user,omitempty"`
}

// ChatMessage represents a single message in a conversation.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// UnmarshalContent returns the content as a string, handling both
// string and array (multimodal) content formats.
func (m *ChatMessage) UnmarshalContent() string {
	if len(m.Content) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Try array of content parts
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var result string
		for _, p := range parts {
			if p.Type == "text" {
				result += p.Text
			}
		}
		return result
	}
	return string(m.Content)
}

// ContentPart represents a part of multimodal content.
type ContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *ImageURL    `json:"image_url,omitempty"`
}

// ImageURL represents an image in a content part.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ── Tool Calling ──────────────────────────────────────────────

// Tool represents a function tool definition.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function describes a callable function.
type Function struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// ToolCall represents an LLM's request to call a function.
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolMessageResult carries the result of a tool call back to the LLM.
type ToolMessageResult struct {
	ToolCallID string
	Content    string
}

// ── Chat Completions Response ─────────────────────────────────

// ChatCompletionResponse represents an OpenAI-compatible response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single completion choice.
type Choice struct {
	Index        int          `json:"index"`
	Message      ResponseMsg  `json:"message"`
	FinishReason string       `json:"finish_reason"`
}

// ResponseMsg is the assistant's response message.
type ResponseMsg struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage tracks token counts.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ── Streaming ─────────────────────────────────────────────────

// ChatCompletionChunk is a single SSE chunk.
type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []ChunkChoice  `json:"choices"`
}

// ChunkChoice is a choice within a streaming chunk.
type ChunkChoice struct {
	Index        int         `json:"index"`
	Delta        DeltaMsg    `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

// DeltaMsg is the incremental content in a streaming chunk.
type DeltaMsg struct {
	Role    string     `json:"role,omitempty"`
	Content string     `json:"content,omitempty"`
}

// ── Model Listing ─────────────────────────────────────────────

// ModelList is an OpenAI-compatible model list response.
type ModelList struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ModelObject describes a single model.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// ── Response from provider ────────────────────────────────────

// ProviderResponse is what providers return after extracting from the browser.
type ProviderResponse struct {
	Message    string
	ThreadID   string
	ElapsedMs  int64
	Images     []ImageInfo
	ToolCalls  []ToolCall
}

// ImageInfo holds metadata for a generated image.
type ImageInfo struct {
	URL         string
	LocalPath   string
	RevisedPrompt string
}

// NewResponseID generates an OpenAI-style response ID.
func NewResponseID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}
