package spin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// rawCall is one call as the model writes it: arguments stay raw so a malformed
// object degrades to {} rather than discarding the whole batch.
type rawCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseToolCalls extracts a tool-call batch from assistant text.
//
// The harness parses this itself instead of relying on the gateway, because the
// gateway only parses calls when the request declares schemas — and declaring
// schemas on every turn is the accumulation this design avoids.
func ParseToolCalls(content string) []ToolCall {
	body := extractJSON(content)
	if body == "" {
		return nil
	}

	var calls []rawCall
	var envelope struct {
		ToolCalls []rawCall `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil && len(envelope.ToolCalls) > 0 {
		calls = envelope.ToolCalls
	} else {
		var single rawCall
		if err := json.Unmarshal([]byte(body), &single); err != nil || single.Name == "" {
			return nil
		}
		calls = []rawCall{single}
	}

	out := make([]ToolCall, 0, len(calls))
	for i, rc := range calls {
		if rc.Name == "" {
			continue
		}
		args := strings.TrimSpace(string(rc.Arguments))
		if args == "" || !strings.HasPrefix(args, "{") {
			args = "{}"
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(args)); err != nil {
			args = "{}"
		} else {
			args = compact.String()
		}
		var tc ToolCall
		tc.ID = fmt.Sprintf("call_%d", i+1)
		tc.Type = "function"
		tc.Function.Name = rc.Name
		tc.Function.Arguments = args
		out = append(out, tc)
	}
	return out
}

// extractJSON returns the first JSON object in text, preferring a fenced block.
func extractJSON(text string) string {
	if i := strings.Index(text, "```json"); i >= 0 {
		start := i + len("```json")
		if end := strings.Index(text[start:], "```"); end >= 0 {
			return strings.TrimSpace(text[start : start+end])
		}
	}
	if i := strings.Index(text, "```"); i >= 0 {
		start := i + 3
		if nl := strings.IndexByte(text[start:], '\n'); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(text[start:], "```"); end >= 0 {
			return strings.TrimSpace(text[start : start+end])
		}
	}
	return firstObject(text)
}

// firstObject uses brace depth so nested argument objects survive, while strings
// and escapes are respected.
func firstObject(text string) string {
	depth, start := 0, -1
	inString, escaped := false, false
	for i, r := range text {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch r {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}
