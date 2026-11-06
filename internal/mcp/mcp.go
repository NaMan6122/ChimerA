// Package mcp implements a Model Context Protocol (MCP) server for Chimera
// over stdio (internal spec 001, 2026 revision): hand-rolled JSON-RPC 2.0
// with newline-delimited JSON framing — no Content-Length headers — and zero
// dependencies beyond the standard library.
//
// stdout carries ONLY protocol bytes. Serve redirects the process-global
// os.Stdout to os.Stderr for its lifetime, so a provider that prints via
// fmt.Println (which resolves the global at call time) cannot corrupt the
// JSON-RPC stream; protocol writes go to the io.Writer captured at Serve
// entry (the real stdout in production). Logging belongs on stderr/files via
// internal/logging. This invariant has its own conformance test
// (TestStdoutPurity).
//
// Requests are processed sequentially in read order — a chat turn blocks the
// loop for its duration, exactly as a terminal session would. Provider-level
// serialization (one turn per provider at a time) is enforced by the same
// per-provider mutexes the HTTP path uses.
package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ProtocolVersion is the newest MCP protocol version this server speaks.
// Clients older than this get their own version echoed back (negotiateVersion).
const ProtocolVersion = "2025-06-18"

// ServerName identifies this implementation in initialize responses.
const ServerName = "chimera"

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Request is one JSON-RPC 2.0 request or notification. ID is raw so string,
// number and null ids echo back byte-identical; a nil ID means notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one JSON-RPC 2.0 response: exactly one of result/error.
// Result is pre-marshaled so an empty-object result (ping) survives omitempty.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error member of a response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// ToolContent is one content block of a tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the result member of a tools/call response. isError is always
// emitted: provider failures are results with isError:true (not JSON-RPC
// errors), so a driving model can read the message and self-correct.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// server carries per-connection state.
type server struct {
	deps *Deps
	out  io.Writer
	wmu  sync.Mutex // serializes protocol writes

	mu          sync.Mutex // guards client/initialized
	client      string     // initialize clientInfo.name (metering tenant)
	initialized bool

	defaultMu sync.Mutex // fallback when a provider has no dedicated mutex
	log       logger
}

// Serve runs the MCP server: it reads newline-delimited JSON requests from in
// until EOF (clean return) or a read error, answering each request with one
// line of JSON on out. See the package comment for the stdout-purity rule.
func Serve(in io.Reader, out io.Writer, deps *Deps) error {
	restore := redirectStdout()
	defer restore()

	s := &server{deps: deps, out: out, log: loggerFor(deps)}
	r := bufio.NewReaderSize(in, 64*1024)
	for {
		line, err := r.ReadString('\n')
		// bufio.Reader (not Scanner) so an oversized line degrades to a parse
		// error instead of killing the loop.
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			if resp := s.handleLine([]byte(trimmed)); resp != nil {
				s.write(resp)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// handleLine parses one request line and returns the response to write, or
// nil for notifications (JSON-RPC 2.0 §4.1: never reply to a notification).
func (s *server) handleLine(line []byte) *Response {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParseError, "parse error: invalid JSON")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		// Structurally invalid. Notifications (no id) get silence; requests
		// get an error so the client does not hang on a missing reply.
		if req.ID == nil {
			return nil
		}
		return errorResponse(req.ID, codeInvalidRequest, `invalid request: want {"jsonrpc":"2.0","method":...}`)
	}
	if req.ID == nil {
		s.handleNotification(req.Method)
		return nil
	}
	return s.dispatch(req)
}

// dispatch routes one request, converting a panicking handler into an
// internal-error response instead of taking the stdio server down.
func (s *server) dispatch(req Request) (resp *Response) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Errorf("MCP handler panic in %s: %v", req.Method, r)
			resp = errorResponse(req.ID, codeInternalError, fmt.Sprintf("internal error: %v", r))
		}
	}()

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "ping":
		return okResponse(req.ID, map[string]any{})
	case "tools/list":
		return okResponse(req.ID, map[string]any{"tools": toolDefinitions()})
	case "tools/call":
		return s.handleToolsCall(req)
	default:
		return errorResponse(req.ID, codeMethodNotFound, fmt.Sprintf("method not found: %q", req.Method))
	}
}

// handleNotification processes notifications (no id → no response).
// notifications/initialized is the MCP lifecycle ack; anything else is
// logged and ignored.
func (s *server) handleNotification(method string) {
	if method == "notifications/initialized" {
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		s.log.Infof("MCP client completed initialization")
		return
	}
	s.log.Debugf("MCP notification ignored: %s", method)
}

// initializeParams is the subset of the MCP initialize request we use.
type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize negotiates the protocol version and records the client
// name (it becomes the metering tenant, e.g. "mcp:claude-code").
func (s *server) handleInitialize(req Request) *Response {
	var p initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errorResponse(req.ID, codeInvalidParams, "invalid params for initialize: "+err.Error())
		}
	}
	s.mu.Lock()
	s.client = strings.TrimSpace(p.ClientInfo.Name)
	s.mu.Unlock()
	s.log.Infof("MCP client %q (protocol %s) initialized", p.ClientInfo.Name, p.ProtocolVersion)

	return okResponse(req.ID, map[string]any{
		"protocolVersion": negotiateVersion(p.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": ServerName, "version": s.deps.Version},
	})
}

// negotiateVersion echoes the client's protocol version when the client is
// older than us (it owns the floor), otherwise answers with the newest
// version this server speaks. Date-stamped versions compare lexicographically.
func negotiateVersion(client string) string {
	if client != "" && client <= ProtocolVersion {
		return client
	}
	return ProtocolVersion
}

// handleToolsCall validates the envelope and routes to the tool. Unknown
// tools and malformed argument objects are -32602; tool-level failures are
// isError results.
func (s *server) handleToolsCall(req Request) *Response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return errorResponse(req.ID, codeInvalidParams, `invalid params for tools/call: want {"name":"...","arguments":{...}}`)
	}
	text, isErr, rpcErr := s.callTool(p.Name, p.Arguments)
	if rpcErr != nil {
		return errorResponse(req.ID, rpcErr.Code, rpcErr.Message)
	}
	return okResponse(req.ID, ToolResult{
		Content: []ToolContent{{Type: "text", Text: text}},
		IsError: isErr,
	})
}

// ── response plumbing ─────────────────────────────────────────

func okResponse(id json.RawMessage, result any) *Response {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorResponse(id, codeInternalError, "internal error: result encoding failed: "+err.Error())
	}
	return &Response{JSONRPC: "2.0", ID: id, Result: raw}
}

func errorResponse(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// write emits one protocol line. It is the ONLY writer of protocol bytes.
func (s *server) write(resp *Response) {
	raw, err := json.Marshal(resp)
	if err != nil {
		s.log.Errorf("MCP response encoding failed: %v", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.out.Write(append(raw, '\n')); err != nil {
		s.log.Errorf("MCP stdout write failed: %v", err)
	}
}

// redirectStdout points the process-global os.Stdout at the current
// os.Stderr and returns a restore func. Protocol bytes flow to the io.Writer
// captured at Serve entry; a stray fmt.Println from a provider (or any other
// library) resolves the global at call time and lands on stderr instead of
// corrupting the JSON-RPC stream. Conformance-tested by TestStdoutPurity.
func redirectStdout() (restore func()) {
	real := os.Stdout
	os.Stdout = os.Stderr
	return func() { os.Stdout = real }
}
