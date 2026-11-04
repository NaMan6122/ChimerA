// Package spin is the Chimera agent protocol: message shapes, the HTTP client,
// the tool set and the tool loop.
//
// It deliberately holds no conversation state. The provider keeps the
// transcript; this package sends only what is new on each turn, so both the
// headless CLI and the TUI can run indefinitely without growing a context that
// is re-uploaded every round.
package spin

// Message is one entry in a request. The harness never accumulates these: the
// slice passed to Complete contains only the current turn's new messages.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one requested function invocation.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDef is the schema advertised to the model on the first turn only.
type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func prop(typ, desc string) map[string]any {
	p := map[string]any{"type": typ}
	if desc != "" {
		p["description"] = desc
	}
	return p
}

func mkTool(name, desc string, props map[string]any, required ...string) ToolDef {
	var t ToolDef
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = desc
	t.Function.Parameters = map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		t.Function.Parameters["required"] = required
	}
	return t
}

// ToolDefs returns the six tools. Past a handful of schemas the prompt-injected
// calling convention degrades, and every schema is prompt bytes on turn one.
func ToolDefs() []ToolDef {
	return []ToolDef{
		mkTool("read_file", "Read a text file. Lines are numbered. Optionally limit to a 1-indexed range.",
			map[string]any{"path": prop("string", "path relative to the repository root"),
				"startLine": prop("integer", "first line, 1-indexed"), "endLine": prop("integer", "last line, inclusive")},
			"path"),
		mkTool("list_dir", "List a directory: subdirectories first, then files.",
			map[string]any{"path": prop("string", "directory relative to the root; empty means root")}),
		mkTool("grep", "Search file contents with a regular expression. Returns path:line:text.",
			map[string]any{"pattern": prop("string", "Go regular expression"),
				"path":    prop("string", "directory to search, default root"),
				"include": prop("string", "optional filename glob, e.g. *.go")},
			"pattern"),
		mkTool("write_file", "Create or overwrite a file with the given content.",
			map[string]any{"path": prop("string", "path relative to the root"),
				"content": prop("string", "full file content")},
			"path", "content"),
		mkTool("edit_file", "Replace exactly one occurrence of search with replace in a file.",
			map[string]any{"path": prop("string", "path relative to the root"),
				"search":  prop("string", "exact text to find; must occur exactly once"),
				"replace": prop("string", "replacement text")},
			"path", "search", "replace"),
		mkTool("run_command", "Run a shell command from the repository root and return its combined output (60s limit).",
			map[string]any{"command": prop("string", "command line to execute with bash -lc")},
			"command"),
	}
}

// SystemPrompt is short on purpose: a general-purpose harness inlines its whole
// persona and tool set into one message (45k+ chars measured), which a web model
// pays for on every upload and prefill.
//
// The "bridge" framing is load-bearing. Measured against a live ChatGPT session:
// told it merely "has access to functions", it answers that it cannot read files;
// told it cannot read directly and must request through the bridge, it emits the
// JSON.
const SystemPrompt = `You are a coding agent working inside a git repository. You cannot read files, list directories or run commands directly: every action is requested by emitting a JSON block, which the harness executes and returns to you as text.

Available tools: read_file, list_dir, grep, write_file, edit_file, run_command.

To act, reply with ONLY a JSON code block and no commentary:

` + "```json" + `
{"tool_calls":[{"name":"read_file","arguments":{"path":"internal/api/server.go"}}]}
` + "```" + `

You may request several tools in one block. When the user asks about files, code or commands, emit the JSON block instead of answering that you lack access. When the question is about this repository's code, read the file before answering: never answer from memory, and never guess at a detail.

After each result you may request another tool or give the final answer. The final answer is plain text with no JSON block: say what you found or what you changed, in as few words as the question allows. Prefer reading before editing, change as little as possible, and never guess at file contents.

If a result says it was truncated, request the next range with read_file startLine/endLine instead of guessing or giving up.`
