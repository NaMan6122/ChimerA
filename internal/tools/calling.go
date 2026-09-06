// Package tools handles tool/function calling via prompt engineering.
// Since browser-based LLMs don't have native tool-calling APIs,
// we inject instructions into the system prompt to get structured output.
package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/telemetry"
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
// No name validation; see ParseToolCallsWithDefs for the validated path.
func ParseToolCalls(text string) []models.ToolCall {
	return ParseToolCallsWithDefs(text, nil, "")
}

// rawCall mirrors one emitted call; Arguments stays raw so non-object
// values can be normalized to {} instead of failing the whole batch.
type rawCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseToolCallsWithDefs extracts tool calls like ParseToolCalls, plus:
//   - accepts the single-object shape {"name":..., "arguments":{...}} in
//     addition to the {"tool_calls":[...]} envelope;
//   - drops calls whose name is not in validNames (empty = keep all),
//     counting rejects under provider ("" skips counting);
//   - normalizes missing/non-object arguments to {};
//   - assigns unique call_<hex> IDs.
func ParseToolCallsWithDefs(text string, validNames []string, provider string) []models.ToolCall {
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return nil
	}

	var raws []rawCall
	var env struct {
		ToolCalls []rawCall `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		return nil
	}
	if len(env.ToolCalls) > 0 {
		raws = env.ToolCalls
	} else {
		var single rawCall
		if err := json.Unmarshal([]byte(jsonStr), &single); err != nil {
			return nil
		}
		if single.Name == "" {
			return nil
		}
		raws = []rawCall{single}
	}

	allowed := make(map[string]bool, len(validNames))
	for _, n := range validNames {
		allowed[n] = true
	}

	var calls []models.ToolCall
	for _, rc := range raws {
		if rc.Name == "" {
			continue
		}
		if len(allowed) > 0 && !allowed[rc.Name] {
			if provider != "" {
				telemetry.ObserveToolNameReject(provider)
			}
			continue
		}
		args := normalizeArguments(rc.Arguments)
		calls = append(calls, models.ToolCall{
			ID:   newCallID(),
			Type: "function",
			Function: models.FunctionCall{
				Name:      rc.Name,
				Arguments: args,
			},
		})
	}
	return calls
}

// normalizeArguments keeps JSON objects as-is (compacted) and maps
// missing/non-object values to {} so one bad item never kills the batch.
func normalizeArguments(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "{}"
	}
	if !strings.HasPrefix(t, "{") {
		return "{}"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(t)); err != nil {
		return "{}"
	}
	return compact.String()
}

// newCallID returns a unique call_<12 hex> ID.
func newCallID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	return "call_" + hex.EncodeToString(b[:])
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
