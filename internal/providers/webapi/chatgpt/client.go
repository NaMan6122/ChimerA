package chatgptweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the chatgpt.com origin; overridable in tests.
const BaseURL = "https://chatgpt.com"

// Client-identification values. These are protocol, not secrets; the exact set is
// unconfirmed until a capture (spec 012 §3.1, §Risks).
const (
	clientVersion = "20250610.1420234636"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

// Client is a low-level chatgpt.com backend-api client. Auth is cookie-custody:
// the durable credential is the session cookie, and the bearer is minted from it
// on demand (spec 012 §3).
type Client struct {
	BaseURL string
	http    *http.Client
	token   string // bearer, refreshed from the cookie jar
	cookies map[string]string
	device  string
	// powConfig is the fingerprint mixed into the Sentinel proof. It is fixed for
	// the lifetime of the client so a solved proof stays re-derivable.
	powConfig string

	// OnPowSolve, when set, receives how long each proof-of-work solve took. The
	// provider wires it to telemetry so this package stays dependency-free.
	OnPowSolve func(time.Duration)
}

// NewClient builds a client from storage-state material.
func NewClient(s *webapi.Session, timeout time.Duration) *Client {
	return &Client{
		BaseURL:   BaseURL,
		http:      &http.Client{Timeout: timeout},
		token:     s.AccessToken,
		cookies:   s.Cookies,
		device:    uuid4(),
		powConfig: DefaultPoWConfig(),
	}
}

func (c *Client) cookieHeader() string {
	if len(c.cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.cookies))
	for k := range c.cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+c.cookies[k])
	}
	return strings.Join(parts, "; ")
}

func (c *Client) headers() http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", c.BaseURL)
	h.Set("Referer", c.BaseURL+"/")
	h.Set("Content-Type", "application/json")
	h.Set("oai-device-id", c.device)
	h.Set("oai-client-version", clientVersion)
	h.Set("oai-language", "en-US")
	if ck := c.cookieHeader(); ck != "" {
		h.Set("Cookie", ck)
	}
	if c.token != "" && c.token != "cookie-session" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	return h
}

// do merges base headers under any caller-set header, so per-request tokens
// (sentinel proof, conduit) win.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers() {
		if req.Header.Get(k) == "" {
			req.Header[k] = v
		}
	}
	return c.http.Do(req)
}

func decodeJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if len(raw) > 0 && raw[0] == '<' {
		return fmt.Errorf("%w: HTML challenge (%d bytes)", webapi.ErrWAF, len(raw))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	if resp.StatusCode != 200 {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("non-JSON: %s", snippet(raw))
	}
	return nil
}

// RefreshToken mints a fresh bearer from the session cookie jar. This is the
// browserless equivalent of a page reload: no Chromium required (spec 012 §3).
func (c *Client) RefreshToken() (string, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/auth/session", nil)
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
	}
	if err := decodeJSON(resp, &doc); err != nil {
		return "", err
	}
	if doc.AccessToken == "" {
		return "", fmt.Errorf("%w: no accessToken (session cookie rejected)", webapi.ErrSessionExpired)
	}
	c.token = doc.AccessToken
	return doc.AccessToken, nil
}

// IsLoggedIn verifies the cookie jar by minting a bearer.
func (c *Client) IsLoggedIn() (bool, error) {
	_, err := c.RefreshToken()
	if err != nil {
		return false, err
	}
	return true, nil
}

// sentinelResponse is the shape of both /chat-requirements endpoints.
type sentinelResponse struct {
	Token       string `json:"token"`
	ProofOfWork *struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty int    `json:"difficulty"`
	} `json:"proofofwork"`
	Turnstile *struct {
		Required bool   `json:"required"`
		Dx       string `json:"dx"`
	} `json:"turnstile"`
}

// Sentinel runs the two-step chat-requirements handshake and returns the
// per-turn tokens. Turnstile is advisory for a session with a browser-earned
// cookie jar (spec 012 §3.1), so it is deliberately not solved here.
func (c *Client) Sentinel() (requirements, proof string, err error) {
	body := []byte(`{"p":""}`)
	req, _ := http.NewRequest("POST", c.BaseURL+"/backend-api/sentinel/chat-requirements/prepare", bytes.NewReader(body))
	resp, err := c.do(req)
	if err != nil {
		return "", "", err
	}
	var prep sentinelResponse
	if err := decodeJSON(resp, &prep); err != nil {
		return "", "", err
	}

	req2, _ := http.NewRequest("POST", c.BaseURL+"/backend-api/sentinel/chat-requirements", bytes.NewReader(body))
	req2.Header.Set("openai-sentinel-chat-requirements-token", prep.Token)
	resp2, err := c.do(req2)
	if err != nil {
		return "", "", err
	}
	var final sentinelResponse
	if err := decodeJSON(resp2, &final); err != nil {
		return "", "", err
	}

	requirements = firstNonEmpty(final.Token, prep.Token)
	if final.ProofOfWork != nil && final.ProofOfWork.Required {
		solveStart := time.Now()
		tok, ok := SolveProof(final.ProofOfWork.Seed, final.ProofOfWork.Difficulty, c.powConfig)
		if c.OnPowSolve != nil {
			c.OnPowSolve(time.Since(solveStart))
		}
		if !ok {
			return "", "", fmt.Errorf("%w: sentinel difficulty %d unsolved", webapi.ErrPowFailed, final.ProofOfWork.Difficulty)
		}
		proof = tok
	}
	return requirements, proof, nil
}

// StreamResult is the assembled outcome of one turn.
type StreamResult struct {
	TTFT         time.Duration
	Duration     time.Duration
	Conversation string
	MessageID    string
	Content      string
	Reasoning    string
}

// delta is one entry of the `delta_encoding: v1` patch stream. The first delta
// for a path carries "p" and "o"; the rest carry only "v" (spec 012 §3).
type delta struct {
	P string `json:"p"`
	O string `json:"o"`
	V any    `json:"v"`
}

// Send streams one turn and returns the assembled answer. conversationID
// continues an existing conversation (empty creates one); parentMessageID is the
// previous message id.
func (c *Client) Send(model, conversationID, parentMessageID, prompt string) (*StreamResult, error) {
	if c.token == "" || c.token == "cookie-session" {
		if _, err := c.RefreshToken(); err != nil {
			return nil, err
		}
	}
	requirements, proof, err := c.Sentinel()
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"action":            "next",
		"model":             model,
		"parent_message_id": parentMessageID,
		"messages": []map[string]any{{
			"id":          uuid4(),
			"author":      map[string]string{"role": "user"},
			"content":     map[string]any{"content_type": "text", "parts": []string{prompt}},
			"metadata":    map[string]any{},
			"create_time": time.Now().Unix(),
		}},
		"timezone":          "UTC",
		"conversation_mode": "primary_assistant",
		"is_authenticated":  true,
		"locale":            "en-US",
	}
	if conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", c.BaseURL+"/backend-api/conversation", bytes.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("openai-sentinel-chat-requirements-token", requirements)
	if proof != "" {
		req.Header.Set("openai-sentinel-proof-token", proof)
	}

	start := time.Now()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if len(raw) > 0 && raw[0] == '<' {
			return nil, fmt.Errorf("%w: conversation blocked (HTML, %d bytes)", webapi.ErrWAF, len(raw))
		}
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		// An anti-bot challenge is served as an HTML page with HTTP 200, so the
		// content type is the only signal that this is not a real stream.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if len(raw) > 0 && raw[0] == '<' {
			return nil, fmt.Errorf("%w: conversation challenged (HTML, %d bytes)", webapi.ErrWAF, len(raw))
		}
		return nil, fmt.Errorf("unexpected content-type %q: %s", ct, snippet(raw))
	}

	res := &StreamResult{}
	var content strings.Builder
	lastPath := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var d delta
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			continue
		}
		// Message metadata (id, conversation_id) arrives on non-content paths.
		if d.P == "" && strings.Contains(d.O, "add") {
			continue
		}
		if d.P != "" {
			lastPath = d.P
		}
		text, isStr := d.V.(string)
		if !isStr {
			continue
		}
		// Only append text destined for the assistant message content. Some
		// deltas carry author/role/status strings on other paths.
		if isContentPath(lastPath) {
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			content.WriteString(text)
		}
		// The final patch carrying the conversation id also lands here.
		if d.P == "/conversation_id" {
			res.Conversation = text
		}
		if d.P == "/message/id" {
			res.MessageID = text
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	res.Content = content.String()
	res.Duration = time.Since(start)
	return res, nil
}

// isContentPath reports whether a delta path addresses the assistant message's
// text, which is the only path whose values are answer tokens.
func isContentPath(p string) bool {
	return strings.Contains(p, "/message/content/parts/")
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
