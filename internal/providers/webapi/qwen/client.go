package qwenweb

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the chat.qwen.ai origin; overridable in tests.
const BaseURL = "https://chat.qwen.ai"

const (
	clientVersion = "0.4.4"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
)

// The session type and the retry/fallback error vocabulary are shared with every
// other browserless transport (internal/providers/webapi, spec 012). They are
// re-exported here under their old names so the qwen call sites and tests read
// unchanged.
type (
	Session   = webapi.Session
	HTTPError = webapi.HTTPError
)

// ErrWAF marks an Alibaba anti-bot challenge. Callers may fall back to the DOM
// transport (spec 011 §4).
var ErrWAF = webapi.ErrWAF

// Client is a low-level chat.qwen.ai web API client.
type Client struct {
	BaseURL string
	// AuthURL is the token service; Qwen moved refresh off the chat host.
	AuthURL string
	// WuURL is the bx-umidtoken endpoint; empty skips the fetch (tests).
	WuURL   string
	http    *http.Client
	token   string
	cookies map[string]string

	mu  sync.Mutex
	mid string
}

// RefreshResult is what the token service returns.
type RefreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

// NewClient builds a client from storage-state material.
func NewClient(s *Session, timeout time.Duration) *Client {
	// HTTP/1.1 only. Measured: the same request succeeds over HTTP/1.1 and is
	// answered with FAIL_SYS_USER_VALIDATE over HTTP/2 — the risk engine scores
	// the connection too, and Go's h2 fingerprint is not a browser's.
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:      8,
		IdleConnTimeout:   90 * time.Second,
	}
	return &Client{
		BaseURL: BaseURL,
		AuthURL: "https://auth.qwen.ai",
		WuURL:   "https://sg-wum.alibaba.com/w/wu.json",
		http:    &http.Client{Timeout: timeout, Transport: tr},
		token:   s.AccessToken,
		cookies: s.Cookies,
	}
}

// RefreshAccessToken exchanges the refresh_token cookie for a new access token.
//
// Qwen moved to ~15-minute access tokens in Nov 2026 and mints them against
// auth.qwen.ai, not the chat host. The browser does exactly this call; replaying
// it is session maintenance, not a challenge bypass. Headers mirror what the app
// sends — the service rejects the request without source/version/x-request-origin.
func (c *Client) RefreshAccessToken() (*RefreshResult, error) {
	if c.AuthURL == "" {
		return nil, fmt.Errorf("no auth endpoint configured")
	}
	if c.cookies["refresh_token"] == "" {
		return nil, fmt.Errorf("session has no refresh_token cookie; re-export it with `chimera auth login qwen`")
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

	req, err := http.NewRequest("GET", c.AuthURL+"/api/v2/auths/refresh", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://chat.qwen.ai")
	req.Header.Set("Referer", "https://chat.qwen.ai/")
	req.Header.Set("source", "web")
	req.Header.Set("version", clientVersion)
	req.Header.Set("x-request-origin", "https://chat.qwen.ai")
	req.Header.Set("X-Request-Id", uuid4())
	req.Header.Set("Timezone", time.Now().Format("Mon Jan 02 2006 15:04:05 GMT-0700"))
	req.Header.Set("Cookie", strings.Join(parts, "; "))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}

	var doc struct {
		Success bool          `json:"success"`
		Data    RefreshResult `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("refresh: non-JSON (%s): %s", resp.Header.Get("Content-Type"), snippet(raw))
	}
	if !doc.Success || doc.Data.AccessToken == "" {
		return nil, fmt.Errorf("refresh rejected: %s", snippet(raw))
	}
	return &doc.Data, nil
}

// SetToken replaces the bearer used for subsequent requests.
func (c *Client) SetToken(t string) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

var midRe = regexp.MustCompile(`(?:umx\.wu|__fycb)\('([^']+)'\)`)

func (c *Client) fetchMidtoken() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mid != "" || c.WuURL == "" {
		return nil
	}
	resp, err := c.http.Get(c.WuURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	m := midRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("bx-umidtoken not found in wu.json (%d bytes)", len(body))
	}
	c.mid = string(m[1])
	return nil
}

func (c *Client) headers(fp string) http.Header {
	itna, itna2 := generateCookies(fp)
	jar := map[string]string{"ssxmod_itna": itna, "ssxmod_itna2": itna2}
	for k, v := range c.cookies {
		jar[k] = v // imported session material wins over generated stamps
	}
	names := make([]string, 0, len(jar))
	for k := range jar {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+jar[k])
	}

	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "*/*")
	h.Set("Origin", c.BaseURL)
	h.Set("Referer", c.BaseURL+"/")
	h.Set("Content-Type", "application/json")
	h.Set("source", "web")
	h.Set("version", clientVersion)
	h.Set("x-request-id", uuid4())
	h.Set("Cookie", strings.Join(parts, "; "))

	// The synthetic anti-bot fingerprint (bx-ua / bx-v / bx-umidtoken plus
	// X-Requested-With and Sec-Fetch-*) is OFF by default.
	//
	// Measured Nov 2026: POST /api/v2/chats/new answers FAIL_SYS_USER_VALIDATE
	// RGV587_ERROR when those headers are present, and 200 with a chat id when
	// the request carries only the session's own cookies and a fresh token.
	// The generated fingerprint is what the risk engine objects to — it is
	// self-consistent but was never issued by a browser.
	//
	// QWEN_BXUA=1 restores the old header set if Qwen's engine changes again.
	if bxuaHeadersEnabled() {
		h.Set("Sec-Fetch-Dest", "empty")
		h.Set("Sec-Fetch-Mode", "cors")
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Connection", "keep-alive")
		h.Set("X-Requested-With", "XMLHttpRequest")
		h.Set("X-Accel-Buffering", "no")
		h.Set("bx-ua", generateBXUA(fp))
		h.Set("bx-v", "2.5.37")
	}

	c.mu.Lock()
	mid, token := c.mid, c.token
	c.mu.Unlock()
	if bxuaHeadersEnabled() && mid != "" {
		h.Set("bx-umidtoken", mid)
	}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

// bxuaHeadersEnabled reports whether the synthetic fingerprint is sent.
func bxuaHeadersEnabled() bool { return os.Getenv("QWEN_BXUA") == "1" }

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if bxuaHeadersEnabled() {
		if err := c.fetchMidtoken(); err != nil {
			return nil, fmt.Errorf("midtoken: %w", err)
		}
	}
	req.Header = c.headers(generateFingerprint())

	// QWEN_HTTP_DUMP=<path> records what we actually sent, header by header.
	// The risk engine answers differently to near-identical requests, so when a
	// turn is refused the only useful evidence is the exact request.
	if path := os.Getenv("QWEN_HTTP_DUMP"); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, ">>> %s %s\n", req.Method, req.URL)
			for k, v := range req.Header {
				if k == "Cookie" || k == "Authorization" {
					fmt.Fprintf(f, "    %s: <redacted>\n", k)
					continue
				}
				fmt.Fprintf(f, "    %s: %s\n", k, strings.Join(v, ","))
			}
			f.Close()
		}
	}
	return c.http.Do(req)
}

// ListModels returns the model ids visible to the session.
func (c *Client) ListModels() ([]string, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/models", nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: snippet(body)}
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("non-JSON (%s): %s", resp.Header.Get("Content-Type"), snippet(body))
	}
	ids := make([]string, 0, len(doc.Data))
	for _, d := range doc.Data {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// NewChat creates a conversation and returns its id.
func (c *Client) NewChat(model string) (string, error) {
	payload := map[string]any{
		"title":      "New Chat",
		"models":     []string{model},
		"chat_mode":  "normal",
		"chat_type":  "t2t",
		"timestamp":  time.Now().UnixMilli(),
		"project_id": "",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v2/chats/new", bytes.NewReader(body))
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(raw) > 0 && raw[0] == '<' {
		return "", fmt.Errorf("%w: chat creation blocked (HTML, %d bytes)", ErrWAF, len(raw))
	}
	var doc struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("non-JSON: %s", snippet(raw))
	}
	if !doc.Success || doc.Data.ID == "" {
		return "", fmt.Errorf("failed to create chat: %s", snippet(raw))
	}
	return doc.Data.ID, nil
}

// StreamResult is the assembled outcome of one turn.
type StreamResult struct {
	TTFT       time.Duration
	Duration   time.Duration
	ResponseID string
	Content    string
	Reasoning  string
	Usage      map[string]any
}

type delta struct {
	Phase     string `json:"phase"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning"`
	Status    string `json:"status"`
}

type chunk struct {
	Choices []struct {
		Delta delta `json:"delta"`
	} `json:"choices"`
	ResponseID string         `json:"response_id"`
	Usage      map[string]any `json:"usage"`
	Ret        []string       `json:"ret"`
	Error      *struct {
		Code    string `json:"code"`
		Details string `json:"details"`
	} `json:"error"`
}

// Send streams one turn and returns the assembled answer. parentID continues an
// existing conversation; empty starts from the chat root.
func (c *Client) Send(model, chatID, parentID, prompt string, thinking bool) (*StreamResult, error) {
	featureConfig := map[string]any{
		"thinking_enabled": thinking,
		"output_schema":    "phase",
		"thinking_format":  "summary",
		"thinking_budget":  81920,
	}
	if thinking {
		featureConfig["auto_thinking"] = true
		featureConfig["thinking_mode"] = "Auto"
		featureConfig["research_mode"] = "normal"
		featureConfig["auto_search"] = false
	}
	now := time.Now().UnixMilli()
	payload := map[string]any{
		"stream":             true,
		"version":            "2.1",
		"incremental_output": true,
		"chat_id":            chatID,
		"chat_mode":          "normal",
		"model":              model,
		"parent_id":          parentID,
		"messages": []map[string]any{{
			"fid":            uuid4(),
			"parentId":       parentID,
			"childrenIds":    []string{},
			"role":           "user",
			"content":        prompt,
			"user_action":    "chat",
			"files":          []any{},
			"timestamp":      now,
			"models":         []string{model},
			"chat_type":      "t2t",
			"feature_config": featureConfig,
			"extra":          map[string]any{"meta": map[string]string{"subChatType": "t2t"}},
			"sub_chat_type":  "t2t",
		}},
		"timestamp": now,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v2/chat/completions?chat_id="+chatID, bytes.NewReader(body))
	start := time.Now()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	// WAF challenges arrive as a plain JSON body instead of an SSE stream.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var ch chunk
		if err := json.Unmarshal(raw, &ch); err == nil && len(ch.Ret) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrWAF, strings.Join(ch.Ret, " "))
		}
		return nil, fmt.Errorf("unexpected content-type %q: %s", ct, snippet(raw))
	}

	res := &StreamResult{}
	var content, reasoning strings.Builder
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
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return nil, fmt.Errorf("upstream error %s: %s", ch.Error.Code, ch.Error.Details)
		}
		if len(ch.Ret) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrWAF, strings.Join(ch.Ret, " "))
		}
		if ch.ResponseID != "" {
			res.ResponseID = ch.ResponseID
		}
		if ch.Usage != nil {
			res.Usage = ch.Usage
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		switch {
		case d.Reasoning != "" || d.Phase == "think":
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
			} else {
				reasoning.WriteString(d.Content)
			}
		case d.Content != "" && d.Phase == "answer":
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			content.WriteString(d.Content)
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

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
