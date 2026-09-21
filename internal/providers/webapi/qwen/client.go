package qwenweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// BaseURL is the chat.qwen.ai origin; overridable in tests.
const BaseURL = "https://chat.qwen.ai"

const (
	clientVersion = "0.2.84"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

// ErrWAF marks an Alibaba anti-bot challenge. Callers may fall back to the DOM
// transport (spec 011 §4).
var ErrWAF = errors.New("waf challenge")

// HTTPError is a non-2xx response from the qwen web API.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Retryable reports whether the failure may succeed on another transport.
func (e *HTTPError) Retryable() bool {
	switch e.Status {
	case 401, 403, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// Session is the storage-state material exported from a logged-in browser.
// It is password-equivalent: keep the file owner-only and never log it.
// Loading, expiry, and installation live in session.go.
type Session struct {
	AccessToken string            `json:"access_token"`
	Cookies     map[string]string `json:"cookies"`
}

// Client is a low-level chat.qwen.ai web API client.
type Client struct {
	BaseURL string
	// WuURL is the bx-umidtoken endpoint; empty skips the fetch (tests).
	WuURL   string
	http    *http.Client
	token   string
	cookies map[string]string

	mu  sync.Mutex
	mid string
}

// NewClient builds a client from storage-state material.
func NewClient(s *Session, timeout time.Duration) *Client {
	return &Client{
		BaseURL: BaseURL,
		WuURL:   "https://sg-wum.alibaba.com/w/wu.json",
		http:    &http.Client{Timeout: timeout},
		token:   s.AccessToken,
		cookies: s.Cookies,
	}
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
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Origin", c.BaseURL)
	h.Set("Referer", c.BaseURL+"/")
	h.Set("Content-Type", "application/json")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Connection", "keep-alive")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("source", "web")
	h.Set("version", clientVersion)
	h.Set("X-Accel-Buffering", "no")
	h.Set("Cookie", strings.Join(parts, "; "))
	h.Set("bx-ua", generateBXUA(fp))
	h.Set("bx-v", "2.5.37")
	h.Set("x-request-id", uuid4())

	c.mu.Lock()
	mid := c.mid
	c.mu.Unlock()
	if mid != "" {
		h.Set("bx-umidtoken", mid)
	}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	return h
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if err := c.fetchMidtoken(); err != nil {
		return nil, fmt.Errorf("midtoken: %w", err)
	}
	req.Header = c.headers(generateFingerprint())
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
