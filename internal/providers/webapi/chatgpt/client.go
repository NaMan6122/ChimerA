// Package chatgptweb replays chatgpt.com's backend-api over HTTP — no browser
// (spec 012 §3). Verified live: a turn returns 200 + text/event-stream in 5–7 s
// with only the exported cookie jar.
//
// The handshake, as measured against a live session:
//
//	1. mint a bearer from the cookie jar   GET /api/auth/session
//	2. generate the requirements token     locally, SHA3-PoW at difficulty 0fffff
//	3. POST /sentinel/chat-requirements    body {"p": <requirements token>}
//	4. solve the returned proofofwork      SHA3-512 over the fingerprint config
//	5. POST /backend-api/f/conversation   with the two tokens above
//
// Turnstile is advisory for a session holding a browser-earned cookie jar: the
// turn succeeds without it. It is still solved when present, because a future
// OpenAI change could make it mandatory.
package chatgptweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the chatgpt.com origin; overridable in tests.
const BaseURL = "https://chatgpt.com"

// Client-identification values the web app sends. These are protocol, not
// secrets; the set was verified against a live capture (logs/chatgpt-capture3.json)
// rather than taken from documentation.
const (
	clientVersion = "prod-98dd09f10c6ec38bdee2965a6d1988b345c9758b"
	buildNumber   = "11120908"
)

// Client is a low-level chatgpt.com backend-api client. Auth is cookie custody:
// the durable credential is the session cookie, and the bearer is minted from it
// on demand.
type Client struct {
	BaseURL string
	http    *http.Client
	token   string // bearer, refreshed from the cookie jar
	cookies map[string]string
	device  string

	// OnPowSolve, when set, receives how long each proof-of-work solve took. The
	// provider wires it to telemetry so this package stays dependency-free.
	OnPowSolve func(time.Duration)
}

// NewClient builds a client from storage-state material.
func NewClient(s *webapi.Session, timeout time.Duration) *Client {
	return &Client{
		BaseURL: BaseURL,
		http:    &http.Client{Timeout: timeout},
		token:   s.AccessToken,
		cookies: s.Cookies,
		device:  uuid4(),
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

// headers builds the base header set the web app sends, minus per-request
// sentinel/conduit tokens.
func (c *Client) headers() http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", c.BaseURL)
	h.Set("Referer", c.BaseURL+"/")
	h.Set("oai-device-id", c.device)
	h.Set("oai-client-version", clientVersion)
	h.Set("oai-client-build-number", buildNumber)
	h.Set("oai-language", "en-US")
	h.Set("x-openai-web-frontend", "core_web")
	h.Set("Cookie", c.cookieHeader())
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
// browserless equivalent of a page reload: no Chromium required.
func (c *Client) RefreshToken() (string, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/auth/session", nil)
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
		// Error is set when the cookie jar is present but the session cannot be
		// refreshed — e.g. "RefreshAccessTokenError". This is the ONLY reliable
		// liveness signal on this endpoint: a dead session still returns HTTP 200
		// with an `accessToken` field, but that token is already expired, so the
		// next turn 401s. (WARNING_BANNER is NOT a signal — it accompanies healthy
		// sessions too.)
		Error string `json:"error"`
	}
	if err := decodeJSON(resp, &doc); err != nil {
		return "", err
	}
	if doc.Error != "" {
		return "", fmt.Errorf("%w: session endpoint reported %q (re-export the session)", webapi.ErrSessionExpired, doc.Error)
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

// sentinelResponse is the shape of /sentinel/chat-requirements.
type sentinelResponse struct {
	Token       string `json:"token"`
	Persona     string `json:"persona"`
	PrepareTok  string `json:"prepare_token"`
	ProofOfWork *struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
	Turnstile *struct {
		Required bool   `json:"required"`
		Dx       string `json:"dx"`
	} `json:"turnstile"`
}

// Sentinel runs the chat-requirements handshake and returns the per-turn tokens.
//
// The requirements token is generated locally and sent as `p`; the returned
// proof-of-work is solved against the challenge the server issues. Turnstile is
// solved when present but is advisory for a cookie-jar session, so a failure to
// solve it is not fatal.
func (c *Client) Sentinel() (requirements, proof, turnstile string, err error) {
	p, err := GetRequirementsToken()
	if err != nil {
		return "", "", "", fmt.Errorf("requirements token: %w", err)
	}

	req, _ := http.NewRequest("POST", c.BaseURL+"/backend-api/sentinel/chat-requirements",
		strings.NewReader(`{"p":`+jsonString(p)+`}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return "", "", "", err
	}
	var doc sentinelResponse
	if err := decodeJSON(resp, &doc); err != nil {
		return "", "", "", err
	}

	requirements = doc.Token
	if requirements == "" {
		return "", "", "", fmt.Errorf("%w: no chat-requirements token (persona=%q)", webapi.ErrSessionExpired, doc.Persona)
	}

	if doc.ProofOfWork != nil && doc.ProofOfWork.Required {
		solveStart := time.Now()
		tok, ok, err := SolveProof(doc.ProofOfWork.Seed, doc.ProofOfWork.Difficulty)
		if c.OnPowSolve != nil {
			c.OnPowSolve(time.Since(solveStart))
		}
		if err != nil {
			return "", "", "", err
		}
		if !ok {
			return "", "", "", fmt.Errorf("%w: sentinel difficulty %s unsolved", webapi.ErrPowFailed, doc.ProofOfWork.Difficulty)
		}
		proof = tok
	}

	if doc.Turnstile != nil && doc.Turnstile.Required && doc.Turnstile.Dx != "" {
		// The XOR key is the proof token when present, else p.
		tok, _, terr := solveTurnstileAny(doc.Turnstile.Dx, proof, p)
		if terr == nil {
			turnstile = tok
		}
	}
	return requirements, proof, turnstile, nil
}

// jsonString renders s as a JSON string literal, for embedding in a body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
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

// Send streams one turn and returns the assembled answer. conversationID
// continues an existing conversation; empty creates a new one. A new chat must
// NOT carry a conversation_id — a random one yields 404 "Conversation not found".
func (c *Client) Send(model, conversationID, parentMessageID, prompt string) (*StreamResult, error) {
	if c.token == "" || c.token == "cookie-session" {
		if _, err := c.RefreshToken(); err != nil {
			return nil, err
		}
	}
	requirements, proof, turnstile, err := c.Sentinel()
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"action": "next",
		"model":  model,
		"messages": []map[string]any{{
			"id":          uuid4(),
			"author":      map[string]string{"role": "user"},
			"create_time": float64(time.Now().Unix()),
			"content":     map[string]any{"content_type": "text", "parts": []string{prompt}},
			"metadata": map[string]any{
				"serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}},
				"submission_mode":        "manual_send",
			},
		}},
		"parent_message_id":        parentMessageID,
		"client_prepare_state":     "sent",
		"timezone_offset_min":      -330,
		"timezone":                 "Asia/Calcutta",
		"conversation_mode":        map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true,
	}
	if conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", c.BaseURL+"/backend-api/f/conversation", bytes.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", requirements)
	if proof != "" {
		req.Header.Set("OpenAI-Sentinel-Proof-Token", proof)
	}
	if turnstile != "" {
		req.Header.Set("OpenAI-Sentinel-Turnstile-Token", turnstile)
	}
	req.Header.Set("X-OpenAI-Target-Path", "/backend-api/f/conversation")
	req.Header.Set("X-OpenAI-Target-Route", "/backend-api/f/conversation")
	req.Header.Set("x-oai-turn-trace-id", uuid4())
	req.Header.Set("oai-session-id", uuid4())
	req.Header.Set("X-OAI-IS-Client-Observation", "v1.s.p."+randomID(16))
	req.Header.Set("X-OAI-IS-Pending-Updates", `{"v":3,"updates":[]}`)

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
	var delta strings.Builder // only used by patch-style streams
	var lastPath string
	var finalText string // last full assistant snapshot (cumulative, so replace)
	// CHIMERA_SSE_DUMP=<path> records the raw event stream. The framing differs
	// per endpoint (f/conversation vs conversation), and when the answer is
	// duplicated or echoed there is no substitute for reading the real bytes.
	dumpPath := os.Getenv("CHIMERA_SSE_DUMP")
	var dump *os.File
	if dumpPath != "" {
		dump, _ = os.Create(dumpPath)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if dump != nil {
			fmt.Fprintln(dump, line)
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		// Shape 1: a full message object (the app's own framing). The stream
		// carries system and user turns too — the user message IS the entire
		// prompt — so anything that is not the assistant's turn is skipped.
		var msg struct {
			Message *struct {
				ID     string `json:"id"`
				Author struct {
					Role string `json:"role"`
				} `json:"author"`
				Content struct {
					ContentType string `json:"content_type"`
					Parts       []any  `json:"parts"`
					Text        string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal([]byte(data), &msg) == nil && msg.Message != nil {
			if msg.ConversationID != "" {
				res.Conversation = msg.ConversationID
			}
			if msg.Message.Author.Role != "assistant" {
				continue
			}
			if msg.Message.ID != "" {
				res.MessageID = msg.Message.ID
			}
			text := msg.Message.Content.Text
			if text == "" {
				var b strings.Builder
				for _, p := range msg.Message.Content.Parts {
					if s, ok := p.(string); ok {
						b.WriteString(s)
					}
				}
				text = b.String()
			}
			// The same assistant message is re-sent as it grows (0 → 17 → 64 →
			// 64 chars in one measured turn). These are cumulative snapshots, so
			// the newest one replaces the previous rather than extending it;
			// appending produced "HelloHelloHelloHello".
			if text != "" {
				if res.TTFT == 0 {
					res.TTFT = time.Since(start)
				}
				finalText = text
			}
			continue
		}

		// Shape 2: a delta_encoding:v1 patch, used by endpoints that stream
		// increments instead of snapshots.
		var d struct {
			P string `json:"p"`
			O string `json:"o"`
			V any    `json:"v"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		if d.P != "" {
			lastPath = d.P
		}
		s, isStr := d.V.(string)
		if !isStr {
			continue
		}
		switch {
		case strings.Contains(lastPath, "/message/content/parts/"):
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			if d.O == "replace" {
				delta.Reset()
			}
			delta.WriteString(s)
		case strings.Contains(lastPath, "reasoning_content"):
			res.Reasoning += s
		}
		if d.P == "/conversation_id" {
			res.Conversation = s
		}
		if d.P == "/message/id" {
			res.MessageID = s
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Snapshots win when present; patches only serve streams that send no
	// full-message frames at all.
	if finalText != "" {
		res.Content = finalText
	} else {
		res.Content = delta.String()
	}
	res.Duration = time.Since(start)
	return res, nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func randomID(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[mrand(len(alpha))]
	}
	return string(b)
}
