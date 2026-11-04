package spin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// rawCall is one call as a model writes it: arguments stay raw so a malformed
// object degrades to {} rather than discarding the whole batch.
type rawCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseToolCalls extracts tool calls from assistant text.
//
// The prompt asks for one shape, but models answer in several. Measured against
// DeepSeek's web model, all of these appear, sometimes in the same session:
//
//	{"tool_calls":[{"name":"read_file","arguments":{"path":"a.go"}}]}   // protocol
//	{"name":"read_file","arguments":{"path":"a.go"}}                   // bare
//	{"tool":"list_dir","args":{"path":"internal"}}                     // aliased
//	{"tool":"list_dir","path":"internal"}                              // inline args
//	[{"tool":"list_dir","args":{...}}, {"tool":"read_file",...}]        // array
//
// Only the first used to parse, so real calls were silently answered as prose.
// The harness parses this itself (rather than relying on the gateway) because
// later turns carry no schemas for the gateway to validate against.
func ParseToolCalls(content string) []ToolCall {
	body := extractJSON(content)
	if body == "" {
		return parseDSML(content)
	}

	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return parseDSML(content)
	}

	var calls []parsedCall
	switch d := doc.(type) {
	case []any:
		for _, item := range d {
			calls = append(calls, normalizeCall(item)...)
		}
	case map[string]any:
		if batch, ok := d["tool_calls"].([]any); ok {
			for _, item := range batch {
				calls = append(calls, normalizeCall(item)...)
			}
		} else {
			calls = normalizeCall(d)
		}
	}
	if len(calls) == 0 {
		return parseDSML(content)
	}

	out := make([]ToolCall, 0, len(calls))
	for i, c := range calls {
		var tc ToolCall
		tc.ID = fmt.Sprintf("call_%d", i+1)
		tc.Type = "function"
		tc.Function.Name = c.name
		tc.Function.Arguments = c.args
		out = append(out, tc)
	}
	return out
}

type parsedCall struct {
	name string
	args string
}

// nameKeys are the aliases models use for the function name. Anything left over
// is the argument object when the model inlines it.
var nameKeys = []string{"name", "tool", "tool_name", "function", "action"}

func normalizeCall(v any) []parsedCall {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	name := ""
	for _, k := range nameKeys {
		if s, ok := m[k].(string); ok && s != "" {
			name = s
			break
		}
	}
	if name == "" {
		return nil
	}

	// Explicit argument container wins.
	for _, k := range []string{"arguments", "args", "parameters", "params", "input"} {
		if raw, ok := m[k]; ok {
			if s, ok := marshalArgs(raw); ok {
				return []parsedCall{{name: name, args: s}}
			}
		}
	}

	// Otherwise the remaining keys are the arguments: {"tool":"ls","path":"."}
	inline := map[string]any{}
	for k, val := range m {
		isNameKey := false
		for _, nk := range nameKeys {
			if k == nk {
				isNameKey = true
				break
			}
		}
		if !isNameKey {
			inline[k] = val
		}
	}
	if len(inline) == 0 {
		return []parsedCall{{name: name, args: "{}"}}
	}
	if s, ok := marshalArgs(inline); ok {
		return []parsedCall{{name: name, args: s}}
	}
	return []parsedCall{{name: name, args: "{}"}}
}

func marshalArgs(v any) (string, bool) {
	switch a := v.(type) {
	case string:
		trimmed := strings.TrimSpace(a)
		if strings.HasPrefix(trimmed, "{") {
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(trimmed)); err == nil {
				return compact.String(), true
			}
		}
		return "{}", true
	case map[string]any:
		var compact bytes.Buffer
		raw, err := json.Marshal(a)
		if err != nil {
			return "{}", false
		}
		if err := json.Compact(&compact, raw); err != nil {
			return "{}", false
		}
		return compact.String(), true
	default:
		return "{}", true
	}
}

// DeepSeek's web model sometimes emits its native DSML markup instead of JSON:
//
//	<｜｜DSML｜｜tool_calls>
//	<｜｜DSML｜｜invoke name="list_dir">
//	<｜｜DSML｜｜parameter name="path">internal/spin</｜｜DSML｜｜parameter>
//	</｜｜DSML｜｜invoke>
//
// Tag spelling varies, so the patterns match any DSML-ish tag around the
// structure rather than a fixed literal.
var (
	dsmlInvoke = regexp.MustCompile(`(?s)<[^<>]*?invoke[^<>]*?name="([^"]+)"[^<>]*?>(.*?)</[^<>]*?invoke[^<>]*?>`)
	dsmlParam  = regexp.MustCompile(`(?s)<[^<>]*?parameter[^<>]*?name="([^"]+)"[^<>]*?>(.*?)</[^<>]*?parameter[^<>]*?>`)
)

func parseDSML(content string) []ToolCall {
	if !strings.Contains(content, "DSML") {
		return nil
	}
	matches := dsmlInvoke.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(matches))
	for i, m := range matches {
		args := map[string]any{}
		for _, p := range dsmlParam.FindAllStringSubmatch(m[2], -1) {
			args[p[1]] = strings.TrimSpace(p[2])
		}
		raw, _ := json.Marshal(args)

		var tc ToolCall
		tc.ID = fmt.Sprintf("call_%d", i+1)
		tc.Type = "function"
		tc.Function.Name = m[1]
		tc.Function.Arguments = string(raw)
		out = append(out, tc)
	}
	return out
}

// extractJSON returns the first JSON value in text, preferring a fenced block.
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
	return firstJSONValue(text)
}

// firstJSONValue finds the first object or array, respecting strings and escapes
// so nested argument objects survive.
func firstJSONValue(text string) string {
	for i, r := range text {
		if r != '{' && r != '[' {
			continue
		}
		open, close := r, '}'
		if r == '[' {
			close = ']'
		}
		depth := 0
		inString, escaped := false, false
		for j := i; j < len(text); j++ {
			c := rune(text[j])
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' && inString {
				escaped = true
				continue
			}
			if c == '"' {
				inString = !inString
				continue
			}
			if inString {
				continue
			}
			switch c {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return text[i : j+1]
				}
			}
		}
	}
	return ""
}
