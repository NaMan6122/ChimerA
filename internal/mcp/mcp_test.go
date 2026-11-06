// mcp_test.go — conformance tests for the MCP stdio server. Everything runs
// in-process over io.Pipes with mock providers; there is never a live vendor
// call here. TestStdoutPurity is the spec-001 conformance test: a chatty
// provider writing to the process-global stdout must not corrupt the
// JSON-RPC stream.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/meter"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/go-rod/rod"
)

// ── mocks ─────────────────────────────────────────────────────

// mockProvider implements the core providers.Provider interface only — it
// exercises the type-assertion fallbacks (no Browserless, no QuotaReporter).
type mockProvider struct {
	name, model string
	reply       string
	err         error
	noise       string // if set, SendMessage prints this to the global stdout
	panicOnSend bool

	mu       sync.Mutex
	threads  []string
	messages []string
	newChats int
}

func (m *mockProvider) Name() string                             { return m.name }
func (m *mockProvider) ModelID() string                          { return m.model }
func (m *mockProvider) Init(_ *rod.Page, _ *config.Config) error { return nil }

func (m *mockProvider) SendMessage(text, threadID string) (*models.ProviderResponse, error) {
	m.mu.Lock()
	m.threads = append(m.threads, threadID)
	m.messages = append(m.messages, text)
	m.mu.Unlock()
	if m.noise != "" {
		// Resolve os.Stdout at call time, exactly like a real fmt.Println in a
		// provider would — the conformance test relies on this.
		fmt.Fprintln(os.Stdout, m.noise)
	}
	if m.panicOnSend {
		panic("provider exploded")
	}
	if m.err != nil {
		return nil, m.err
	}
	return &models.ProviderResponse{Message: m.reply, ThreadID: threadID}, nil
}

func (m *mockProvider) NewChat() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newChats++
	return nil
}

func (m *mockProvider) ExtractResponse() (string, error) { return m.reply, nil }
func (m *mockProvider) IsLoggedIn() (bool, error)        { return true, nil }

func (m *mockProvider) snapshot() (threads, messages []string, newChats int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.threads...), append([]string(nil), m.messages...), m.newChats
}

// mockWebProvider adds the optional interfaces (browserless web transport
// with a cached quota), like the grok provider does.
type mockWebProvider struct {
	mockProvider
	browserless bool
	quota       map[string]any
}

func (m *mockWebProvider) Browserless() bool     { return m.browserless }
func (m *mockWebProvider) Quota() map[string]any { return m.quota }

var _ providers.Provider = (*mockProvider)(nil)
var _ providers.Provider = (*mockWebProvider)(nil)
var _ providers.Browserless = (*mockWebProvider)(nil)
var _ providers.QuotaReporter = (*mockWebProvider)(nil)

// ── harness ───────────────────────────────────────────────────

// runServe feeds lines to Serve over a pipe and returns the raw protocol
// output after the server hits EOF.
func runServe(t *testing.T, deps *Deps, lines ...string) string {
	t.Helper()
	r, w := io.Pipe()
	out := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_ = Serve(r, out, deps)
		close(done)
	}()
	for _, ln := range lines {
		if _, err := io.WriteString(w, ln+"\n"); err != nil {
			t.Fatalf("writing request: %v", err)
		}
	}
	w.Close()
	<-done
	return out.String()
}

// responses decodes the NDJSON protocol output.
func responses(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, ln := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("protocol line %d is not JSON: %v\nline: %s", i+1, err, ln)
		}
		out = append(out, m)
	}
	return out
}

func reqJSON(id any, method string, params any) string {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func initReq(id any, version, clientName string) string {
	return reqJSON(id, "initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "1.2.3"},
	})
}

// resultOf returns the result object of response i (panic-friendly helper).
func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", resp)
	}
	return res
}

func errorCode(t *testing.T, resp map[string]any) float64 {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", resp)
	}
	code, ok := e["code"].(float64)
	if !ok {
		t.Fatalf("error object has no numeric code: %v", e)
	}
	return code
}

// toolText extracts the first text content block of a tools/call result.
func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	res := resultOf(t, resp)
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no content array: %v", res)
	}
	first, ok := content[0].(map[string]any)
	if !ok || first["type"] != "text" {
		t.Fatalf("content[0] is not a text block: %v", content)
	}
	return first["text"].(string)
}

func newDeps(provs ...providers.Provider) *Deps {
	m := map[string]providers.Provider{}
	for _, p := range provs {
		m[p.Name()] = p
	}
	return &Deps{Version: "test", Cfg: &config.Config{}, Providers: m}
}

// ── handshake & version negotiation ───────────────────────────

func TestInitializeHandshake(t *testing.T) {
	deps := newDeps()
	raw := runServe(t, deps, initReq(1, "2025-06-18", "claude-code"))
	resp := responses(t, raw)
	if len(resp) != 1 {
		t.Fatalf("want 1 response, got %d", len(resp))
	}
	res := resultOf(t, resp[0])
	if got := res["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want 2025-06-18", got)
	}
	caps, ok := res["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing: %v", res)
	}
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities.tools missing: %v", caps)
	}
	info, ok := res["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo missing: %v", res)
	}
	if info["name"] != "chimera" {
		t.Errorf("serverInfo.name = %v, want chimera", info["name"])
	}
	if info["version"] != "test" {
		t.Errorf("serverInfo.version = %v, want test", info["version"])
	}
	if _, hasErr := resp[0]["error"]; hasErr {
		t.Errorf("initialize returned an error: %v", resp[0]["error"])
	}
}

func TestInitializeVersionNegotiation(t *testing.T) {
	cases := []struct {
		client string
		want   string
	}{
		{"2024-11-05", "2024-11-05"}, // older client → echo
		{"2025-03-26", "2025-03-26"}, // older client → echo
		{"2025-06-18", "2025-06-18"}, // exact match
		{"2099-01-01", "2025-06-18"}, // newer client → ours
		{"", "2025-06-18"},           // absent → ours
	}
	for _, tc := range cases {
		raw := runServe(t, newDeps(), initReq(1, tc.client, "c"))
		res := resultOf(t, responses(t, raw)[0])
		if got := res["protocolVersion"]; got != tc.want {
			t.Errorf("client %q: protocolVersion = %v, want %q", tc.client, got, tc.want)
		}
	}
}

// ── tools/list ────────────────────────────────────────────────

func TestToolsList(t *testing.T) {
	raw := runServe(t, newDeps(), reqJSON(2, "tools/list", nil))
	res := resultOf(t, responses(t, raw)[0])
	tools, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing: %v", res)
	}
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		tm := tl.(map[string]any)
		byName[tm["name"].(string)] = tm
	}
	for _, want := range []string{"chat", "refresh", "providers"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("tools/list is missing %q", want)
		}
	}
	if len(byName) != 3 {
		t.Errorf("want exactly 3 tools, got %d", len(byName))
	}
	chat := byName["chat"]
	schema, ok := chat["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("chat inputSchema wrong: %v", chat["inputSchema"])
	}
	reqd, ok := schema["required"].([]any)
	if !ok || len(reqd) != 1 || reqd[0] != "message" {
		t.Errorf("chat required = %v, want [message]", schema["required"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("chat properties missing: %v", schema)
	}
	for _, want := range []string{"message", "provider", "session_id"} {
		if _, ok := props[want]; !ok {
			t.Errorf("chat schema missing property %q", want)
		}
	}
}

// ── tools/call: chat ──────────────────────────────────────────

func TestToolsCallChat(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", reply: "hello from alpha"}
	deps := newDeps(mock)
	raw := runServe(t, deps,
		initReq(1, "2025-06-18", "claude-code"),
		reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi there"}}),
	)
	resp := responses(t, raw)
	if len(resp) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resp))
	}
	if _, hasErr := resp[1]["error"]; hasErr {
		t.Fatalf("chat returned a JSON-RPC error: %v", resp[1]["error"])
	}
	res := resultOf(t, resp[1])
	if isError, _ := res["isError"].(bool); isError {
		t.Errorf("isError = true, want false")
	}
	if got := toolText(t, resp[1]); got != "hello from alpha" {
		t.Errorf("text = %q, want %q", got, "hello from alpha")
	}
	threads, msgs, _ := mock.snapshot()
	if len(threads) != 1 || len(msgs) != 1 || msgs[0] != "hi there" {
		t.Errorf("mock got messages=%v threads=%v", msgs, threads)
	}
}

func TestSessionReuseSameThread(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", reply: "ok"}
	deps := newDeps(mock)
	raw := runServe(t, deps,
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "a", "session_id": "sess-1"}}),
		reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "b", "session_id": "sess-1"}}),
		reqJSON(3, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "c"}}),
	)
	if resp := responses(t, raw); len(resp) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resp))
	}
	threads, _, _ := mock.snapshot()
	if len(threads) != 3 {
		t.Fatalf("want 3 sends, got %d", len(threads))
	}
	if threads[0] != "sess-1" || threads[1] != "sess-1" {
		t.Errorf("session_id reuse must feed the same threadID: %v", threads)
	}
	if threads[2] == "sess-1" || !strings.HasPrefix(threads[2], "mcp-") {
		t.Errorf("omitted session_id must get a fresh thread, got %q", threads[2])
	}
}

func TestFreshThreadPerCall(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", reply: "ok"}
	raw := runServe(t, newDeps(mock),
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "a"}}),
		reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "b"}}),
	)
	if resp := responses(t, raw); len(resp) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resp))
	}
	threads, _, _ := mock.snapshot()
	if len(threads) != 2 || threads[0] == threads[1] {
		t.Errorf("each stateless call must use a fresh thread: %v", threads)
	}
}

func TestProviderErrorIsResultNotRPCError(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", err: errors.New("upstream exploded")}
	raw := runServe(t, newDeps(mock),
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	)
	resp := responses(t, raw)[0]
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("provider failure must be a result, not a JSON-RPC error: %v", resp["error"])
	}
	res := resultOf(t, resp)
	if isError, _ := res["isError"].(bool); !isError {
		t.Errorf("isError = false, want true")
	}
	if txt := toolText(t, resp); !strings.Contains(txt, "upstream exploded") {
		t.Errorf("error message missing from text: %q", txt)
	}
}

func TestQuotaHintAppended(t *testing.T) {
	mock := &mockWebProvider{
		mockProvider: mockProvider{name: "grok", model: "chimera-grok", reply: "answer"},
		quota:        map[string]any{"remaining": 21},
	}
	raw := runServe(t, newDeps(mock),
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	)
	if got := toolText(t, responses(t, raw)[0]); !strings.HasSuffix(got, "\n\nquota (grok): 21 remaining") {
		t.Errorf("quota hint missing: %q", got)
	}

	mockTotal := &mockWebProvider{
		mockProvider: mockProvider{name: "grok", model: "chimera-grok", reply: "answer"},
		quota:        map[string]any{"remaining": 3, "total": 100},
	}
	raw = runServe(t, newDeps(mockTotal),
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	)
	if got := toolText(t, responses(t, raw)[0]); !strings.HasSuffix(got, "\n\nquota (grok): 3/100 remaining") {
		t.Errorf("quota hint with total wrong: %q", got)
	}
}

// ── tools/call: refresh & providers ───────────────────────────

func TestRefresh(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", reply: ""}
	raw := runServe(t, newDeps(mock),
		reqJSON(1, "tools/call", map[string]any{"name": "refresh", "arguments": map[string]any{}}),
	)
	resp := responses(t, raw)[0]
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("refresh returned an error: %v", resp["error"])
	}
	if isError, _ := resultOf(t, resp)["isError"].(bool); isError {
		t.Errorf("refresh isError = true, want false")
	}
	if txt := toolText(t, resp); !strings.Contains(txt, "alpha") {
		t.Errorf("refresh text should name the provider: %q", txt)
	}
	if _, _, n := mock.snapshot(); n != 1 {
		t.Errorf("NewChat called %d times, want 1", n)
	}
}

func TestProvidersTool(t *testing.T) {
	dom := &mockProvider{name: "toolprov-dom", model: "chimera-dom"}
	web := &mockWebProvider{
		mockProvider: mockProvider{name: "toolprov-web", model: "chimera-web"},
		browserless:  true,
	}
	deps := newDeps(dom, web)
	deps.SeedLoginStates() // startup cache seed — never inside a tool call

	raw := runServe(t, deps, reqJSON(1, "tools/call", map[string]any{"name": "providers", "arguments": map[string]any{}}))
	entries := []providerEntry{}
	if err := json.Unmarshal([]byte(toolText(t, responses(t, raw)[0])), &entries); err != nil {
		t.Fatalf("providers tool text is not a JSON array: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// Sorted by name: dom < web.
	if entries[0].Name != "toolprov-dom" || entries[1].Name != "toolprov-web" {
		t.Errorf("unexpected entries: %+v", entries)
	}
	if entries[0].LoggedIn == nil || !*entries[0].LoggedIn || entries[1].LoggedIn == nil || !*entries[1].LoggedIn {
		t.Errorf("seeded providers must report logged_in: %+v", entries)
	}
	if entries[1].ModelID != "chimera-web" || !entries[1].Browserless || entries[0].Browserless {
		t.Errorf("model_id/browserless wrong: %+v", entries)
	}

	// Without a seed, login state is unknown — the field is omitted and no
	// probe happens inside the tool call.
	unseeded := newDeps(&mockProvider{name: "toolprov-x", model: "chimera-x"})
	raw = runServe(t, unseeded, reqJSON(1, "tools/call", map[string]any{"name": "providers", "arguments": map[string]any{}}))
	entries = nil
	if err := json.Unmarshal([]byte(toolText(t, responses(t, raw)[0])), &entries); err != nil {
		t.Fatalf("providers tool text is not a JSON array: %v", err)
	}
	if len(entries) != 1 || entries[0].LoggedIn != nil {
		t.Errorf("unseeded provider must omit logged_in: %+v", entries)
	}
}

// ── protocol errors ───────────────────────────────────────────

func TestUnknownToolIsInvalidParams(t *testing.T) {
	raw := runServe(t, newDeps(),
		reqJSON(1, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}),
	)
	resp := responses(t, raw)[0]
	if code := errorCode(t, resp); code != -32602 {
		t.Errorf("unknown tool code = %v, want -32602", code)
	}
	if msg, _ := resp["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "nope") {
		t.Errorf("error should name the tool: %v", msg)
	}
}

func TestInvalidParams(t *testing.T) {
	cases := []struct {
		note string
		line string
	}{
		{"chat without arguments", reqJSON(1, "tools/call", map[string]any{"name": "chat"})},
		{"chat with empty message", reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "   "}})},
		{"tools/call without name", reqJSON(3, "tools/call", map[string]any{"arguments": map[string]any{}})},
		{"bad argument JSON", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"chat","arguments":"not-an-object"}}`},
	}
	for _, tc := range cases {
		raw := runServe(t, newDeps(&mockProvider{name: "alpha", model: "chimera-alpha", reply: "x"}), tc.line)
		resp := responses(t, raw)[0]
		if code := errorCode(t, resp); code != -32602 {
			t.Errorf("%s: code = %v, want -32602", tc.note, code)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	raw := runServe(t, newDeps(), reqJSON(7, "foo/bar", nil))
	resp := responses(t, raw)[0]
	if code := errorCode(t, resp); code != -32601 {
		t.Errorf("unknown method code = %v, want -32601", code)
	}
}

func TestPing(t *testing.T) {
	raw := runServe(t, newDeps(), reqJSON(5, "ping", nil))
	res := resultOf(t, responses(t, raw)[0])
	if len(res) != 0 {
		t.Errorf("ping result should be empty, got %v", res)
	}
}

func TestMalformedJSONDoesNotKillLoop(t *testing.T) {
	raw := runServe(t, newDeps(),
		"this is not json at all",
		reqJSON(2, "ping", nil),
	)
	resp := responses(t, raw)
	if len(resp) != 2 {
		t.Fatalf("want 2 responses (parse error + ping), got %d", len(resp))
	}
	if code := errorCode(t, resp[0]); code != -32700 {
		t.Errorf("malformed line code = %v, want -32700", code)
	}
	if resp[0]["id"] != nil {
		t.Errorf("parse error id should be null, got %v", resp[0]["id"])
	}
	if _, hasErr := resp[1]["error"]; hasErr {
		t.Errorf("loop did not survive: ping got an error: %v", resp[1]["error"])
	}
}

func TestNotificationsGetNoReply(t *testing.T) {
	raw := runServe(t, newDeps(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		reqJSON(2, "ping", nil),
	)
	resp := responses(t, raw)
	if len(resp) != 1 {
		t.Fatalf("notifications must not be answered; want 1 response, got %d", len(resp))
	}
	if resp[0]["id"] != float64(2) {
		t.Errorf("the surviving response should be the ping: %v", resp[0])
	}
}

func TestHandlerPanicIsInternalError(t *testing.T) {
	mock := &mockProvider{name: "alpha", model: "chimera-alpha", panicOnSend: true}
	raw := runServe(t, newDeps(mock),
		reqJSON(1, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	)
	resp := responses(t, raw)[0]
	if code := errorCode(t, resp); code != -32603 {
		t.Errorf("panic code = %v, want -32603", code)
	}
}

// ── metering ──────────────────────────────────────────────────

func TestMeteringKeysByClientName(t *testing.T) {
	db := filepath.Join(t.TempDir(), "usage.db")
	st, err := meter.Open(db)
	if err != nil {
		t.Fatalf("meter open: %v", err)
	}
	defer st.Close()

	mock := &mockProvider{name: "alpha", model: "chimera-alpha", reply: "answer text"}
	deps := newDeps(mock)
	deps.Meter = st
	raw := runServe(t, deps,
		initReq(1, "2025-06-18", "claude-code"),
		reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hello"}}),
	)
	if resp := responses(t, raw); len(resp) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resp))
	}
	from, to := meter.MonthBounds(time.Now())
	u, err := st.Summarize("mcp:claude-code", from, to)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if u.Requests != 1 || u.ByProvider["alpha"] != 1 {
		t.Errorf("usage keyed wrong: %+v", u)
	}
	if u.PromptChars != int64(len("hello")) || u.CompletionChars != int64(len("answer text")) {
		t.Errorf("char counts wrong: %+v", u)
	}

	// A client that never initialized falls back to the mcp:client tenant.
	raw = runServe(t, deps,
		reqJSON(3, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	)
	if resp := responses(t, raw); len(resp) != 1 {
		t.Fatalf("want 1 response, got %d", len(resp))
	}
	u, err = st.Summarize("mcp:client", from, to)
	if err != nil {
		t.Fatalf("summarize fallback: %v", err)
	}
	if u.Requests != 1 {
		t.Errorf("fallback tenant should carry the anonymous request: %+v", u)
	}
}

// ── provider resolution ───────────────────────────────────────

func TestProviderForResolution(t *testing.T) {
	chatgpt := &mockProvider{name: "chatgpt", model: "chimera-chatgpt"}
	qwen := &mockProvider{name: "qwen", model: "chimera-qwen"}
	deps := newDeps(chatgpt, qwen)
	deps.Cfg = &config.Config{Provider: "qwen"}
	s := &server{deps: deps, log: nopLog{}}

	if _, _, name := s.providerFor("chimera-qwen"); name != "qwen" {
		t.Errorf("model id resolution failed: %q", name)
	}
	if _, _, name := s.providerFor("qwen"); name != "qwen" {
		t.Errorf("name resolution failed: %q", name)
	}
	if _, _, name := s.providerFor(""); name != "qwen" {
		t.Errorf("empty ref should default to cfg.Provider: %q", name)
	}
	if _, _, name := s.providerFor("nope"); name != "" {
		t.Errorf("unknown explicit ref must not silently default: %q", name)
	}

	deps.Cfg = &config.Config{}
	if _, _, name := s.providerFor(""); name != "chatgpt" {
		t.Errorf("no cfg.Provider: want deterministic chatgpt fallback, got %q", name)
	}
}

// ── stdout purity (spec 001 conformance) ──────────────────────

// TestStdoutPurity proves the invariant that stdout carries ONLY protocol
// bytes: a provider that prints to the process-global os.Stdout mid-turn
// must not corrupt the JSON-RPC stream. Serve redirects the global to
// os.Stderr; the test swaps the global stderr too, so we can observe exactly
// where the noise went.
func TestStdoutPurity(t *testing.T) {
	oldOut, oldErr := os.Stdout, os.Stderr
	// os.Stdout is a *os.File, so the fake "stdout" the mock prints to is a
	// pipe we can read back.
	noiseR, noiseW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = noiseW // a stray fmt.Println resolves this global
	os.Stderr = noiseW // Serve redirects os.Stdout here
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr; _ = noiseW.Close() }()

	mock := &mockProvider{name: "chatty", model: "chimera-chatty", reply: "fine", noise: "PROVIDER NOISE: should never reach the protocol stream"}
	r, w := io.Pipe()
	proto := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_ = Serve(r, proto, newDeps(mock))
		close(done)
	}()
	for _, ln := range []string{
		initReq(1, "2025-06-18", "claude-code"),
		reqJSON(2, "tools/call", map[string]any{"name": "chat", "arguments": map[string]any{"message": "hi"}}),
	} {
		if _, err := io.WriteString(w, ln+"\n"); err != nil {
			t.Fatalf("writing request: %v", err)
		}
	}
	w.Close()
	<-done

	// Every protocol line must be intact JSON with an id/result shape.
	lines := strings.Split(strings.TrimSpace(proto.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 protocol lines, got %d:\n%s", len(lines), proto.String())
	}
	for i, ln := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(ln), &resp); err != nil {
			t.Fatalf("protocol stream corrupted at line %d: %v\nline: %s", i+1, err, ln)
		}
		if resp["jsonrpc"] != "2.0" {
			t.Errorf("line %d: jsonrpc = %v", i+1, resp["jsonrpc"])
		}
	}
	// Serve has returned and its internal redirect is undone; drain what the
	// mock wrote to the redirected stdout.
	_ = noiseW.Close()
	noiseBuf, _ := io.ReadAll(noiseR)
	if !strings.Contains(string(noiseBuf), "PROVIDER NOISE") {
		t.Errorf("provider noise was not redirected: %q", string(noiseBuf))
	}
	_ = noiseR.Close()
	// And the noise must not appear anywhere in the protocol stream.
	if strings.Contains(proto.String(), "PROVIDER NOISE") {
		t.Errorf("noise leaked into the protocol stream")
	}
}
