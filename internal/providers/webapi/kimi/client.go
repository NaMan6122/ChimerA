// Package kimiweb replays kimi.ai's web API (Moonshot Kimi) without a browser —
// spec 025. Live-verified 2026-11-13 via scripts/kimi-spike; the parser below
// is tested against the frames captured from that run, not research guesses.
//
// Wire summary (live-verified):
//   - Chat: POST {base}/apiv2/kimi.gateway.chat.v1.ChatService/Chat with
//     Content-Type: application/connect+json. Request and response are both
//     Connect envelopes: [flag][len BE u32][JSON]. flag 0x02 marks the trailer.
//   - Events are OP-DRIVEN: {"op":"set"|"append","mask":"<field>",...}.
//     set replaces the field, append appends to it. The turn ends on
//     {"done":{}} — not on a status value.
//   - Auth: GET /api/auth/token/refresh with Bearer <refresh> → an access JWT
//     (30-day life) plus a ROTATED refresh JWT.
//   - Continuity is native: chat_id + parent_id (the last assistant message
//     id), so the no-history invariant holds — no transcript folding.
package kimiweb

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

const (
	kimiBase    = "https://www.kimi.ai"
	chatPath    = "/apiv2/kimi.gateway.chat.v1.ChatService/Chat"
	refreshPath = "/api/auth/token/refresh"
	scenario    = "SCENARIO_K2D5"
	ua          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// accessTTL is the fallback when the access token is not a decodable JWT.
	// The observed life is 30 days; refresh a little early regardless.
	accessTTL     = 30 * 24 * time.Hour
	refreshBuffer = 5 * time.Minute
	// maxFrameBytes bounds one Connect frame; the largest observed is ~1KB.
	maxFrameBytes = 8 << 20
	// trailerFlag marks the end-of-stream frame in the Connect envelope.
	trailerFlag = 0x02
)

// TurnResult is one completed conversation turn.
type TurnResult struct {
	Content   string
	Reasoning string // discarded at the provider boundary (house rule)
	ChatID    string
	ParentID  string // last assistant message id — the next turn's parent_id
	Duration  time.Duration
}

// Client talks to kimi.ai on top of an exported refresh-token session.
type Client struct {
	// Base overrides the API origin (tests); empty means https://www.kimi.ai.
	Base string
	HTTP *http.Client
	// Model is the KIMI_WEB_MODEL selector; "auto" uses the account default.
	Model     string
	DebugDump io.Writer // KIMI_DUMP: raw frame capture, house pattern

	mu          sync.Mutex
	refresh     string
	cookies     map[string]string // the harvested jar; preserved on rotation
	access      string
	accessUntil time.Time
	sessionPath string // rewritten when the server rotates the refresh token
	deviceID    string
	sessionID   string
}

// NewClient builds a client over an exported session. sessionPath is where the
// rotated refresh token is persisted (best-effort).
func NewClient(sess *webapi.Session, timeout time.Duration, model, sessionPath string) *Client {
	return &Client{
		HTTP:        &http.Client{Timeout: timeout},
		Model:       model,
		refresh:     sess.AccessToken, // the session's token IS the refresh credential
		cookies:     sess.Cookies,
		sessionPath: sessionPath,
		deviceID:    loadOrCreateDeviceID(sessionPath),
		sessionID:   bigID(1700000000000000000),
	}
}

// base resolves the API origin.
func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return kimiBase
}

// bigID returns a random 19-digit id in [min, min+1e18): the vendor's device
// and session ids are huge decimal strings.
func bigID(min uint64) string {
	hi := min + 999999999999999999
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := min + binary.BigEndian.Uint64(b[:])%(hi-min)
	return fmt.Sprintf("%d", n)
}

// loadOrCreateDeviceID keeps the device identity stable across runs, the way
// the reference client persists kimi_client_identity.json. A failure to
// persist is not fatal — a fresh id per process also works.
func loadOrCreateDeviceID(sessionPath string) string {
	path := strings.TrimSuffix(sessionPath, filepath.Ext(sessionPath)) + ".device"
	if sessionPath != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(raw)); len(id) >= 16 {
				return id
			}
		}
	}
	id := bigID(7000000000000000000)
	if sessionPath != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte(id), 0o600)
	}
	return id
}

// fakeHeaders mirrors the reference FAKE_HEADERS. Origin is derived from the
// base URL: the reference sets it from its configured base, and a kimi.ai
// request carrying a kimi.com Origin is a needless mismatch.
func fakeHeaders(base string) map[string]string {
	return map[string]string{
		"Accept":             "*/*",
		"Accept-Language":    "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7",
		"Cache-Control":      "no-cache",
		"Pragma":             "no-cache",
		"Origin":             base,
		"R-Timezone":         "Asia/Shanghai",
		"Sec-Ch-Ua":          `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"User-Agent":         ua,
		"Priority":           "u=1, i",
		"X-Msh-Platform":     "web",
	}
}

// do issues one request with the full browser-shaped header set. contentType
// is explicit because the chat endpoint rejects anything but
// application/connect+json with HTTP 415 — a live-only failure the fake server
// did not model until it was made to assert this.
func (c *Client) do(method, url, bearer, contentType string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range fakeHeaders(c.base()) {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Msh-Device-Id", c.deviceID)
	req.Header.Set("X-Msh-Session-Id", c.sessionID)
	if c.DebugDump != nil {
		fmt.Fprintf(c.DebugDump, "=== %s %s\nbearer=%v body=%.200s\n", method, url, bearer != "", body)
	}
	return c.HTTP.Do(req)
}

// jwtExpiry reads a JWT's exp claim. No signature check: the server is the
// authority on validity.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// Refresh exchanges the session's refresh token for a fresh access token. The
// server hands back a ROTATED refresh token on every call; the new one is
// persisted back into the session file (best-effort) so restarts keep working.
// A 401/403 maps to webapi.ErrSessionExpired — the session must be re-exported.
func (c *Client) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access != "" && time.Now().Before(c.accessUntil) {
		return nil // cached, still fresh
	}
	resp, err := c.do(http.MethodGet, c.base()+refreshPath, c.refresh, "", nil)
	if err != nil {
		return fmt.Errorf("kimi refresh: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: refresh rejected (HTTP %d) — re-export the kimi session", webapi.ErrSessionExpired, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("kimi refresh decode: %w", err)
	}
	access := res.AccessToken
	if access == "" {
		access = res.Token
	}
	if access == "" {
		return fmt.Errorf("kimi refresh carried no access token: %.200s", strings.TrimSpace(string(body)))
	}
	c.access = access
	if exp, ok := jwtExpiry(access); ok {
		c.accessUntil = exp.Add(-refreshBuffer)
	} else {
		c.accessUntil = time.Now().Add(accessTTL - refreshBuffer)
	}
	if res.RefreshToken != "" && res.RefreshToken != c.refresh {
		c.refresh = res.RefreshToken // the server rotates it every call
		if c.sessionPath != "" {
			// Keep the harvested cookie jar — LoadSession rejects token-only
			// sessions, and the jar is still useful material.
			_ = webapi.WriteSession(&webapi.Session{AccessToken: c.refresh, Cookies: c.cookies}, c.sessionPath)
		}
	}
	return nil
}

// encodeConnect frames one Connect-protocol request body.
func encodeConnect(payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 5, 5+len(body))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	return append(out, body...), nil
}

// Send performs one conversation turn. chatID/parentID may be empty (a new
// chat); the response always carries the ids to reuse.
func (c *Client) Send(text, chatID, parentID string) (*TurnResult, error) {
	if err := c.Refresh(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	access := c.access
	c.mu.Unlock()

	message := map[string]any{
		"role":     "user",
		"scenario": scenario,
		"blocks": []map[string]any{
			{"message_id": "", "text": map[string]any{"content": text}},
		},
	}
	if parentID != "" {
		message["parent_id"] = parentID
	}
	payload := map[string]any{
		"scenario": scenario,
		"tools":    []any{},
		"message":  message,
		"options":  map[string]any{"thinking": true},
	}
	if chatID != "" {
		payload["chat_id"] = chatID
	}
	body, err := encodeConnect(payload)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	resp, err := c.do(http.MethodPost, c.base()+chatPath, access, "application/connect+json", body)
	if err != nil {
		return nil, fmt.Errorf("kimi turn: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			c.mu.Lock()
			c.access, c.accessUntil = "", time.Time{} // force a refresh next turn
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: turn rejected (HTTP %d)", webapi.ErrSessionExpired, resp.StatusCode)
		}
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}

	res, err := c.consumeFrames(resp.Body)
	if res != nil {
		res.Duration = time.Since(start)
	}
	return res, err
}

// event is one Connect data frame. The payload is op-driven: op says whether
// the masked field is replaced or appended, so the parser keys on op+mask
// rather than on the presence of a particular object.
type event struct {
	Op      string          `json:"op"`
	Mask    string          `json:"mask"`
	Chat    json.RawMessage `json:"chat"`
	Message json.RawMessage `json:"message"`
	Block   json.RawMessage `json:"block"`
	Done    json.RawMessage `json:"done"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// consumeFrames reads the Connect response stream and accumulates the turn.
// set replaces a field, append extends it; {"done":{}} ends the turn.
func (c *Client) consumeFrames(r io.Reader) (*TurnResult, error) {
	br := bufio.NewReader(r)
	var (
		text, think strings.Builder
		chatID      string
		parentID    string
	)
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				break // stream ended without a done event; keep what we have
			}
			return nil, fmt.Errorf("kimi frame header: %w", err)
		}
		flag := hdr[0]
		n := binary.BigEndian.Uint32(hdr[1:5])
		if n > maxFrameBytes {
			return nil, fmt.Errorf("kimi frame too large: %d bytes", n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, fmt.Errorf("kimi frame body: %w", err)
		}
		if flag&trailerFlag != 0 {
			continue // end-of-stream trailer
		}
		if c.DebugDump != nil {
			fmt.Fprintf(c.DebugDump, "frame flag=%02x len=%d %s\n", flag, n, payload)
		}

		var ev event
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue // heartbeat/unknown frame: not fatal
		}
		if ev.Error != nil {
			return nil, fmt.Errorf("kimi upstream error: %s", ev.Error.Message)
		}
		if ev.Done != nil {
			break
		}
		if len(ev.Chat) > 0 {
			var chat struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(ev.Chat, &chat) == nil && chat.ID != "" {
				chatID = chat.ID
			}
		}
		if len(ev.Message) > 0 {
			var msg struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			}
			if json.Unmarshal(ev.Message, &msg) == nil && msg.Role == "assistant" && msg.ID != "" {
				parentID = msg.ID // the next turn's parent_id
			}
		}
		if len(ev.Block) > 0 {
			var block struct {
				Think *struct {
					Content string `json:"content"`
				} `json:"think"`
				Text *struct {
					Content string `json:"content"`
				} `json:"text"`
			}
			if json.Unmarshal(ev.Block, &block) == nil {
				appendOrSet(&think, ev.Op, block.Think != nil, contentOf(block.Think))
				appendOrSet(&text, ev.Op, block.Text != nil, contentOf(block.Text))
			}
		}
	}
	return &TurnResult{
		Content:   text.String(),
		Reasoning: think.String(),
		ChatID:    chatID,
		ParentID:  parentID,
	}, nil
}

// appendOrSet applies one frame to an accumulator: op "append" extends, any
// other op (in practice "set") replaces. A frame that does not carry the field
// leaves the accumulator untouched.
func appendOrSet(b *strings.Builder, op string, present bool, content string) {
	if !present {
		return
	}
	if op == "append" {
		b.WriteString(content)
		return
	}
	b.Reset()
	b.WriteString(content)
}

// contentOf unwraps an optional content object.
func contentOf(c *struct {
	Content string `json:"content"`
}) string {
	if c == nil {
		return ""
	}
	return c.Content
}
