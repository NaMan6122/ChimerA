package deepseekweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the chat.deepseek.com origin; overridable in tests.
const BaseURL = "https://chat.deepseek.com"

// Client-identification values the web UI sends. These are protocol, not secrets,
// and were verified against a live capture (logs/deepseek-capture.json) rather
// than taken from documentation. Note there is no x-app-version header here.
const (
	clientVersion = "2.5.0"
	bundleID      = "com.deepseek.chat"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36"
)

// CompletionPath is the endpoint the proof of work is bound to.
const CompletionPath = "/api/v0/chat/completion"

// Client is a low-level chat.deepseek.com web API client.
type Client struct {
	BaseURL string
	http    *http.Client
	token   string
	cookies map[string]string
	device  string // stable per client; the web UI persists this as x-device-id
	tzOff   string // seconds east of UTC, as the UI sends it

	// OnPowSolve, when set, receives how long each proof-of-work solve took. The
	// provider wires it to telemetry so this package stays dependency-free.
	OnPowSolve func(time.Duration)

	// DebugDump, when set, receives every raw SSE line. It exists because this
	// stream is a vendor patch protocol with no public spec: when parsing goes
	// wrong the only reliable move is to look at the bytes. The provider sets it
	// from DEEPSEEK_SSE_DUMP; never enable it in production (it writes answers to
	// disk).
	DebugDump io.Writer
}

// NewClient builds a client from storage-state material.
func NewClient(s *webapi.Session, timeout time.Duration) *Client {
	_, off := time.Now().Zone()
	return &Client{
		BaseURL: BaseURL,
		http:    &http.Client{Timeout: timeout},
		token:   s.AccessToken,
		cookies: s.Cookies,
		device:  uuid4(),
		tzOff:   strconv.Itoa(off),
	}
}

func (c *Client) headers() http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en")
	h.Set("Origin", c.BaseURL)
	h.Set("Referer", c.BaseURL+"/")
	h.Set("Content-Type", "application/json")
	h.Set("x-client-bundle-id", bundleID)
	h.Set("x-client-platform", "web")
	h.Set("x-client-version", clientVersion)
	h.Set("x-client-locale", "en_US")
	h.Set("x-client-timezone-offset", c.tzOff)
	h.Set("x-device-id", c.device)
	h.Set("x-device-model", "")
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	if len(c.cookies) > 0 {
		names := make([]string, 0, len(c.cookies))
		for k := range c.cookies {
			names = append(names, k)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, k := range names {
			parts = append(parts, k+"="+c.cookies[k])
		}
		h.Set("Cookie", strings.Join(parts, "; "))
	}
	return h
}

// do merges the base headers under any per-request header already set (notably
// x-ds-pow-response on the completion call), so caller-set values win.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers() {
		if req.Header.Get(k) == "" {
			req.Header[k] = v
		}
	}
	return c.http.Do(req)
}

// envelope is the standard DeepSeek response wrapper. WAF/CF challenges arrive
// as HTML instead, which callers detect by a leading '<'.
type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BizCode int             `json:"biz_code"`
		BizMsg  string          `json:"biz_msg"`
		BizData json.RawMessage `json:"biz_data"`
	} `json:"data"`
}

func decodeEnvelope(resp *http.Response) (*envelope, error) {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(raw) > 0 && raw[0] == '<' {
		return nil, fmt.Errorf("%w: %s blocked (HTML, %d bytes)", webapi.ErrWAF, resp.Request.URL.Path, len(raw))
	}
	if resp.StatusCode != 200 {
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("non-JSON: %s", snippet(raw))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("deepseek error %d: %s", env.Code, env.Msg)
	}
	if env.Data.BizCode != 0 {
		return nil, fmt.Errorf("deepseek biz error %d: %s", env.Data.BizCode, env.Data.BizMsg)
	}
	return &env, nil
}

// CreateChatSession creates a conversation and returns its id. The id is nested
// at data.biz_data.chat_session.id (verified live), not directly under biz_data.
func (c *Client) CreateChatSession() (string, error) {
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v0/chat_session/create", bytes.NewReader([]byte("{}")))
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	env, err := decodeEnvelope(resp)
	if err != nil {
		return "", err
	}
	var biz struct {
		ID          string `json:"id"`
		ChatSession struct {
			ID string `json:"id"`
		} `json:"chat_session"`
	}
	if err := json.Unmarshal(env.Data.BizData, &biz); err != nil {
		return "", fmt.Errorf("failed to create chat session: %s", snippet(env.Data.BizData))
	}
	id := firstNonEmpty(biz.ChatSession.ID, biz.ID)
	if id == "" {
		return "", fmt.Errorf("failed to create chat session: no id in %s", snippet(env.Data.BizData))
	}
	return id, nil
}

// CreatePowChallenge requests a proof-of-work challenge for targetPath.
func (c *Client) CreatePowChallenge(targetPath string) (*Challenge, error) {
	body, _ := json.Marshal(map[string]string{"target_path": targetPath})
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v0/chat/create_pow_challenge", bytes.NewReader(body))
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope(resp)
	if err != nil {
		return nil, err
	}
	var biz struct {
		Challenge Challenge `json:"challenge"`
	}
	if err := json.Unmarshal(env.Data.BizData, &biz); err != nil || biz.Challenge.Challenge == "" {
		return nil, fmt.Errorf("empty pow challenge: %s", snippet(env.Data.BizData))
	}
	return &biz.Challenge, nil
}

// StreamResult is the assembled outcome of one turn.
type StreamResult struct {
	TTFT       time.Duration
	Duration   time.Duration
	ResponseID string
	Status     string
	Content    string
	Reasoning  string
	Usage      map[string]any
}

// Send streams one turn and returns the assembled answer.
//
// The completion endpoint is NOT OpenAI-shaped: it emits a JSON-patch stream
// (verified live, fixture in testdata/completion_sse.txt):
//
//	event: ready
//	data: {"request_message_id":1,"response_message_id":2,...}
//	data: {"v":{"response":{...,"fragments":[{"type":"RESPONSE","content":"P"}]}}}
//	data: {"p":"response/fragments/-1/content","o":"APPEND","v":"ONG"}
//	data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":40}]}
//	data: {"p":"response/status","o":"SET","v":"FINISHED"}
//
// The answer is the opening snapshot's fragments plus every APPEND to
// response/fragments/-1/content; reasoning is the same stream with fragment type
// THINKING.
func (c *Client) Send(sessionID, parentID, prompt string, thinking bool) (*StreamResult, error) {
	ch, err := c.CreatePowChallenge(CompletionPath)
	if err != nil {
		return nil, err
	}
	solveStart := time.Now()
	header, solveErr := Solve(*ch)
	if c.OnPowSolve != nil {
		c.OnPowSolve(time.Since(solveStart))
	}
	if solveErr != nil {
		return nil, fmt.Errorf("%w: %v", webapi.ErrPowFailed, solveErr)
	}

	payload := map[string]any{
		"chat_session_id":   sessionID,
		"parent_message_id": nullableNum(parentID),
		"model_type":        modelType(thinking),
		"prompt":            prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  thinking,
		"search_enabled":    false,
		"action":            nil,
		"preempt":           false,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.BaseURL+CompletionPath, bytes.NewReader(body))
	req.Header.Set("x-ds-pow-response", header)

	start := time.Now()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if len(raw) > 0 && raw[0] == '<' {
			return nil, fmt.Errorf("%w: completion blocked (HTML, %d bytes)", webapi.ErrWAF, len(raw))
		}
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if len(raw) > 0 && raw[0] == '<' {
			return nil, fmt.Errorf("%w: completion blocked (HTML, %d bytes)", webapi.ErrWAF, len(raw))
		}
		return nil, fmt.Errorf("unexpected content-type %q: %s", ct, snippet(raw))
	}

	res := &StreamResult{Usage: map[string]any{}}
	var content, reasoning strings.Builder
	lastFragment := "RESPONSE" // fragment type of the text currently streaming
	currentPath := ""          // last explicit patch path, for bare {"v":…} continuations
	event := ""

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		raw := sc.Text()
		if c.DebugDump != nil {
			fmt.Fprintln(c.DebugDump, raw)
		}
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			// A blank line ends the current SSE event, so the event type applies
			// only to the data line(s) that follow it. Not resetting this made
			// "event: ready" leak onto the next block and swallow the opening
			// snapshot, which carries the first answer fragment.
			event = ""
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		case !strings.HasPrefix(line, "data:"):
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var doc struct {
			ResponseMessageID int64           `json:"response_message_id"`
			P                 string          `json:"p"`
			O                 string          `json:"o"`
			V                 json.RawMessage `json:"v"`
		}
		if err := json.Unmarshal([]byte(data), &doc); err != nil {
			continue
		}

		// event: ready announces the ids before any patch arrives.
		if event == "ready" {
			if doc.ResponseMessageID != 0 {
				res.ResponseID = strconv.FormatInt(doc.ResponseMessageID, 10)
			}
			continue
		}

		// A patch carrying an explicit path either opens a new fragment, carries
		// a batch, or appends to the current fragment's text.
		if doc.P != "" {
			currentPath = doc.P
			switch {
			case doc.P == "response/fragments" && doc.O == "APPEND":
				appendNewFragments(doc.V, res, &content, &reasoning, &lastFragment, start)
			case doc.P == "response" && doc.O == "BATCH":
				applyBatch(doc.V, res)
			case doc.P == "response/status" && doc.O == "SET":
				var s string
				if json.Unmarshal(doc.V, &s) == nil {
					res.Status = s
				}
			case strings.HasSuffix(doc.P, "/fragments/-1/content"):
				// Note: this patch sometimes omits "o" entirely, so match on the
				// path alone rather than requiring APPEND.
				if s, ok := jsonString(doc.V); ok {
					appendText(s, res, &content, &reasoning, lastFragment, start)
				}
			}
			continue
		}

		if len(doc.V) == 0 {
			continue
		}

		// A bare {"v":"…"} continues whatever path was last announced. Most of a
		// thinking stream arrives this way, so ignoring it silently truncates the
		// answer — the bug that produced "The userONG" instead of "PONG".
		if s, ok := jsonString(doc.V); ok {
			if strings.HasSuffix(currentPath, "/fragments/-1/content") {
				appendText(s, res, &content, &reasoning, lastFragment, start)
			}
			continue
		}

		// Otherwise it is the opening snapshot:
		// {"v":{"response":{…,"fragments":[{"type":…,"content":…}]}}}
		var snap struct {
			Response struct {
				MessageID int64      `json:"message_id"`
				Fragments []fragment `json:"fragments"`
			} `json:"response"`
		}
		if json.Unmarshal(doc.V, &snap) != nil {
			continue
		}
		if snap.Response.MessageID != 0 {
			res.ResponseID = strconv.FormatInt(snap.Response.MessageID, 10)
		}
		for _, f := range snap.Response.Fragments {
			lastFragment = f.Type
			appendText(f.Content, res, &content, &reasoning, lastFragment, start)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	res.Content = content.String()
	res.Reasoning = reasoning.String()
	res.Duration = time.Since(start)
	return res, nil
}

// fragment is one piece of the assistant response. Thinking (reasoner) text and
// the final answer arrive as separate fragments in the same stream, distinguished
// by Type: "THINK" for reasoning, "RESPONSE" for the answer.
type fragment struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// jsonString reports whether raw is a JSON string and returns it.
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// appendText routes streamed text to the reasoning or the answer builder
// according to the fragment type currently being written.
func appendText(s string, res *StreamResult, content, reasoning *strings.Builder, fragType string, start time.Time) {
	if s == "" {
		return
	}
	if res.TTFT == 0 {
		res.TTFT = time.Since(start)
	}
	if fragType == "THINK" {
		reasoning.WriteString(s)
		return
	}
	content.WriteString(s)
}

// appendNewFragments handles {"p":"response/fragments","o":"APPEND","v":[…]},
// which introduces one or more new fragments (and therefore switches the type of
// the text that follows).
func appendNewFragments(raw json.RawMessage, res *StreamResult, content, reasoning *strings.Builder, fragType *string, start time.Time) {
	var frags []fragment
	if json.Unmarshal(raw, &frags) != nil {
		return
	}
	for _, f := range frags {
		*fragType = f.Type
		appendText(f.Content, res, content, reasoning, *fragType, start)
	}
}

// applyBatch reads the response-level BATCH patch, which carries token usage and
// the quasi_status that flips to FINISHED when the answer is complete.
func applyBatch(raw json.RawMessage, res *StreamResult) {
	var ops []struct {
		P string          `json:"p"`
		V json.RawMessage `json:"v"`
	}
	if json.Unmarshal(raw, &ops) != nil {
		return
	}
	for _, op := range ops {
		switch op.P {
		case "accumulated_token_usage":
			var n float64
			if json.Unmarshal(op.V, &n) == nil {
				res.Usage["total_tokens"] = n
			}
		case "quasi_status", "status":
			var s string
			if json.Unmarshal(op.V, &s) == nil {
				res.Status = s
			}
		}
	}
}

// modelType maps the thinking toggle to the model_type the UI sends: "thinking"
// for the reasoner (R1), "default" otherwise.
func modelType(thinking bool) string {
	if thinking {
		return "thinking"
	}
	return "default"
}

// nullableNum renders a message id for parent_message_id, which the server types
// as u32. Sending it as a JSON string is rejected with HTTP 422 ("invalid type:
// string, expected u32"), so a numeric string becomes a number and anything
// unparseable becomes null (start a fresh thread) rather than a bad request.
func nullableNum(s string) any {
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseUint(s, 10, 32); err == nil {
		return n
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
