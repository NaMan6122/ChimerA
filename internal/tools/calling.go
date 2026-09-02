// Package tools handles tool/function calling via prompt engineering.
// Since browser-based LLMs don't have native tool-calling APIs,
// we inject instructions into the system prompt to get structured output.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/chimera/chimera/internal/models"
)

// ToolPromptForChatGPT is the system prompt injected for ChatGPT tool calling.
const ToolPromptForChatGPT = `You are in tool-calling mode. You have access to the following functions:

%s

When you need to call a function, output ONLY a JSON code block with no commentary:

{"tool_calls": [{"name": "<function_name>", "arguments": {<key>: <value>}}]}

Rules:
- Output ONLY the JSON code block when calling tools
- No text before or after the JSON when calling tools
- When you receive tool results, summarize them naturally in plain text
- Do NOT call tools again for the same request after receiving results
- You may call multiple tools in a single response`

// ToolPromptForClaude is the system prompt injected for Claude tool calling.
const ToolPromptForClaude = `You have access to external tools through a structured interface. Available functions:

%s

To use a tool, output a JSON code block like this:

{"tool_calls": [{"name": "<function_name>", "arguments": {<key>: <value>}}]}

Important guidelines:
- Only output the JSON code block when calling a function — no additional text
- When tool results come back, provide a natural language summary
- Do NOT attempt to call tools again after receiving their results
- You may invoke multiple tools in one response if needed`

// BuildToolPrompt creates a prompt section describing available tools.
func BuildToolPrompt(tools []models.Tool, provider string) string {
	var descriptions []string
	for _, t := range tools {
		desc := fmt.Sprintf("- %s: %s\n  Parameters: %v", t.Function.Name, t.Function.Description, t.Function.Parameters)
		descriptions = append(descriptions, desc)
	}

	toolList := strings.Join(descriptions, "\n")

	if provider == "claude" {
		return fmt.Sprintf(ToolPromptForClaude, toolList)
	}
	return fmt.Sprintf(ToolPromptForChatGPT, toolList)
}

// ParseToolCalls attempts to extract tool calls from the LLM response text.
// Uses a brace-depth tracker to handle nested JSON objects.
func ParseToolCalls(text string) []models.ToolCall {
	// Look for JSON block in markdown code fence or bare JSON
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return nil
	}

	var parsed struct {
		ToolCalls []struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		} `json:"tool_calls"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil
	}

	var calls []models.ToolCall
	for i, tc := range parsed.ToolCalls {
		argsBytes, _ := json.Marshal(tc.Arguments)
		calls = append(calls, models.ToolCall{
			ID:   fmt.Sprintf("call_%d", i+1),
			Type: "function",
			Function: models.FunctionCall{
				Name:      tc.Name,
				Arguments: string(argsBytes),
			},
		})
	}
	return calls
}

// extractJSON finds the first JSON object in text, handling code fences.
func extractJSON(text string) string {
	// Try to find JSON in a code fence first
	if idx := strings.Index(text, "```json"); idx != -1 {
		start := idx + 7
		if end := strings.Index(text[start:], "```"); end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}
	if idx := strings.Index(text, "```"); idx != -1 {
		start := idx + 3
		// Skip language identifier on same line
		if nl := strings.IndexByte(text[start:], '\n'); nl != -1 {
			start += nl + 1
		}
		if end := strings.Index(text[start:], "```"); end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Try brace-depth tracking for bare JSON
	return extractJSONObject(text)
}

// extractJSONObject uses brace-depth tracking to find a JSON object.
func extractJSONObject(text string) string {
	inString := false
	escaped := false
	depth := 0
	start := -1

	for i, ch := range text {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		if ch == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 && start >= 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}

// StripToolPrompt removes tool-related content from a response
// that may have been echoed.
func StripToolPrompt(text string) string {
	markers := []string{
		"You are in tool-calling mode",
		"Available functions:",
		"You have access to external tools",
		"tool_calls",
	}

	for _, marker := range markers {
		if idx := strings.Index(text, marker); idx >= 0 {
			// Find the end of the paragraph
			end := strings.IndexByte(text[idx:], '\n')
			if end == -1 {
				end = len(text) - idx
			}
			text = strings.TrimSpace(text[:idx] + text[idx+end:])
		}
	}
	return text
}

// IsToolCallResponse checks if the response contains tool calls.
func IsToolCallResponse(text string) bool {
	return strings.Contains(text, "tool_calls")
}

// TrimNonJSON strips any non-JSON prefix/suffix from text.
func TrimNonJSON(text string) string {
	// Find first {
	start := strings.IndexFunc(text, func(r rune) bool { return r == '{' })
	if start == -1 {
		return text
	}
	// Find matching closing brace
	end := strings.LastIndexFunc(text, func(r rune) bool { return r == '}' })
	if end == -1 || end < start {
		return text
	}
	return text[start : end+1]
}

// HasContent checks if the text has meaningful content after stripping whitespace.
func HasContent(text string) bool {
	return strings.TrimSpace(text) != ""
}

// IsWhitespaceOrPunctuation checks if text is just whitespace/punctuation.
func IsWhitespaceOrPunctuation(text string) bool {
	for _, r := range text {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			return false
		}
	}
	return true
}
