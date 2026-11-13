// Package glmweb replays chatglm.cn's web API (Zhipu GLM) without a browser —
// spec 024. Live-verified 2026-11-13 via scripts/glm-spike; the parser below
// is tested against those captured fixtures, not research guesses.
//
// Wire summary (source-verified + live-verified):
//   - Every call carries the FAKE_HEADERS set and an X-Sign mint:
//     sign = md5(ts'-nonce-SECRET) where SECRET is a static public constant
//     and ts' is the digit-mangled epoch-milli (second-to-last digit replaced
//     by (digit-sum − that digit) mod 10).
//   - Refresh: POST /chatglm/user-api/user/refresh, EMPTY JSON body,
//     Authorization: Bearer <refresh_token> → result.access_token (24h) and
//     result.refresh_token, which ROTATES on every call.
//   - Turn: POST /chatglm/backend-api/assistant/stream → SSE of full parts[]
//     SNAPSHOTS (replace, never append); text in parts[].content[].text
//     (type "text"), reasoning in type "think" field "think";
//     conversation_id on every event; finish on status=="finish".
//   - Continuity is native: pass conversation_id back — the no-history
//     invariant holds, no transcript folding needed (unlike Grok).
package glmweb

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

const (
	glmBase          = "https://chatglm.cn"
	signSecret       = "8a1317a7468aa3ad86e997d08f3f31cb" // static public constant (spec 024)
	defaultAssistant = "65940acff94777010aa6b796"         // reference DEFAULT_ASSISTANT_ID (GLM4)
	ua               = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	// expGroups is the static experiment blob the web client sends (reference
	// FAKE_HEADERS). Treated as protocol material, pinned here.
	expGroups = "na_android_config:exp:NA,na_4o_config:exp:4o_A,tts_config:exp:tts_config_a,na_glm4plus_config:exp:open,mainchat_server_app:exp:A,mobile_history_daycheck:exp:a,desktop_toolbar:exp:A,chat_drawing_server:exp:A,drawing_server_cogview:exp:cogview4,app_welcome_v2:exp:A,chat_drawing_streamv2:exp:A,mainchat_rm_fc:exp:add,mainchat_dr:exp:open,chat_auto_entrance:exp:A,drawing_server_hi_dream:control:A,homepage_square:exp:close,assistant_recommend_prompt:exp:3,app_home_regular_user:exp:A,memory_common:exp:enable,mainchat_moe:exp:300,assistant_greet_user:exp:greet_user,app_welcome_personalize:exp:A,assistant_model_exp_group:exp:glm4.5,ai_wallet:exp:ai_wallet_enable"
	// accessTTL is the observed 24h token life; refresh an hour early.
	accessTTL = 23 * time.Hour
)

// TurnResult is one completed conversation turn.
type TurnResult struct {
	Content        string
	Reasoning      string // discarded at the provider boundary (house rule)
	ConversationID string
	Duration       time.Duration
}

// Client talks to chatglm.cn on top of an exported refresh-token session.
type Client struct {
	// Base overrides the API origin (tests); empty means https://chatglm.cn.
	Base string
	HTTP *http.Client
	// Model is the GLM_WEB_MODEL selector: "auto" (or anything without a
	// keyword) → default assistant; contains "think"/"zero" → thinking mode;
	// contains "deepresearch" → deep research; a 24+ char id is used as the
	// assistant_id directly.
	Model     string
	DebugDump io.Writer // GLM_DUMP: raw request/response capture, house pattern

	mu          sync.Mutex
	refresh     string
	cookies     map[string]string // the harvested jar; preserved on rotation
	access      string
	accessUntil time.Time
	sessionPath string // rewritten when the server rotates the refresh token
}

// NewClient builds a client over an exported session. sessionPath is where
// the rotated refresh token is persisted (best-effort).
func NewClient(sess *webapi.Session, timeout time.Duration, model, sessionPath string) *Client {
	return &Client{
		HTTP:        &http.Client{Timeout: timeout},
		Model:       model,
		refresh:     sess.AccessToken, // the session's token IS the refresh credential
		cookies:     sess.Cookies,
		sessionPath: sessionPath,
	}
}

// assistantAndMode resolves the model selector to assistant_id + chat_mode,
// mirroring the reference routing.
func (c *Client) assistantAndMode() (assistantID, chatMode string) {
	m := strings.ToLower(strings.TrimSpace(c.Model))
	assistantID, chatMode = defaultAssistant, "zero" // reference defaults: chat_mode is always zero unless deepresearch
	switch {
	case strings.Contains(m, "deepresearch"):
		chatMode = "deep_research"
	case regexp.MustCompile(`^[a-z0-9]{24,}$`).MatchString(m):
		assistantID = c.Model
	}
	return assistantID, chatMode
}

// fakeHeaders mirrors the reference FAKE_HEADERS — every API call rides with
// the full browser-shaped set.
func fakeHeaders() map[string]string {
	return map[string]string{
		"Accept":             "text/event-stream",
		"Accept-Language":    "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		"App-Name":           "chatglm",
		"Cache-Control":      "no-cache",
		"Content-Type":       "application/json",
		"Origin":             "https://chatglm.cn",
		"Pragma":             "no-cache",
		"Sec-Ch-Ua":          `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"X-App-Fr":           "browser_extension",
		"X-App-Platform":     "pc",
		"X-App-Version":      "0.0.1",
		"X-Exp-Groups":       expGroups,
		"X-Lang":             "zh",
		"User-Agent":         ua,
	}
}

// signMint computes the X-Sign material for one request: the digit-mangled
// epoch-milli timestamp, a random nonce, and md5(ts'-nonce-SECRET). The live
// server is the oracle — a capture of a real accepted mint is the fixture.
func signMint(now time.Time) (tsMangled int64, nonce, sign string) {
	ts := now.UnixMilli()
	s := []byte(strconv.FormatInt(ts, 10))
	if len(s) >= 2 {
		i := len(s) - 2
		sum, that := 0, 0
		for j, c := range s {
			d := int(c - '0')
			sum += d
			if j == i {
				that = d
			}
		}
		s[i] = byte('0' + (sum-that)%10)
	}
	tsMangled, _ = strconv.ParseInt(string(s), 10, 64)
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	nonce = strings.ReplaceAll(hex.EncodeToString(b[:]), "-", "")
	sign = fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%d-%s-%s", tsMangled, nonce, signSecret))))
	return tsMangled, nonce, sign
}

// base resolves the API origin.
func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return glmBase
}

func (c *Client) do(method, url, bearer, body string) (*http.Response, error) {
	tsMangled, nonce, sign := signMint(time.Now())
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range fakeHeaders() {
		req.Header.Set(k, v)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Device-Id", newUUID())
	req.Header.Set("X-Request-Id", newUUID())
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("X-Timestamp", strconv.FormatInt(tsMangled, 10))
	req.Header.Set("X-Sign", sign)
	if c.DebugDump != nil {
		fmt.Fprintf(c.DebugDump, "=== %s %s\nbearer=%v body=%.200s\n", method, url, bearer != "", body)
	}
	return c.HTTP.Do(req)
}

// Refresh exchanges the session's refresh token for a fresh access token.
// The server rotates the refresh token on every call; the new one is
// persisted back into the session file (best-effort) so restarts keep
// working. A 401/400-class answer maps to webapi.ErrSessionExpired — the
// session must be re-exported.
func (c *Client) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access != "" && time.Now().Before(c.accessUntil) {
		return nil // cached, still fresh
	}
	resp, err := c.do(http.MethodPost, c.base()+"/chatglm/user-api/user/refresh", c.refresh, "{}")
	if err != nil {
		return fmt.Errorf("glm refresh: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: refresh rejected (HTTP %d) — re-export the glm session", webapi.ErrSessionExpired, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var wrap struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Result  struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return fmt.Errorf("glm refresh decode: %w", err)
	}
	if wrap.Result.AccessToken == "" {
		return fmt.Errorf("glm refresh rejected: status=%d message=%q (session stale — re-export)", wrap.Status, wrap.Message)
	}
	c.access = wrap.Result.AccessToken
	c.accessUntil = time.Now().Add(accessTTL)
	if wrap.Result.RefreshToken != "" && wrap.Result.RefreshToken != c.refresh {
		c.refresh = wrap.Result.RefreshToken // the server rotates it every call
		if c.sessionPath != "" {
			// Keep the harvested cookie jar — LoadSession rejects
			// token-only sessions, and the jar is still useful material.
			_ = webapi.WriteSession(&webapi.Session{AccessToken: c.refresh, Cookies: c.cookies}, c.sessionPath)
		}
	}
	return nil
}

// DeleteConversation removes one conversation from the account's history list
// (spec 024 hygiene). It is a SOFT delete: the data survives and the id stays
// continuable, so this is a history-cleanup primitive, not a data-destruction
// one. Best-effort by design — a failed delete only means the history list
// keeps one entry.
func (c *Client) DeleteConversation(conversationID string) error {
	if conversationID == "" {
		return nil
	}
	if err := c.Refresh(); err != nil {
		return err
	}
	c.mu.Lock()
	access := c.access
	c.mu.Unlock()
	assistantID, _ := c.assistantAndMode()
	body, _ := json.Marshal(map[string]any{
		"assistant_id":    assistantID,
		"conversation_id": conversationID,
	})
	resp, err := c.do(http.MethodPost, c.base()+"/chatglm/backend-api/assistant/conversation/delete", access, string(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	// The envelope, not the status code, carries the verdict: deleting an
	// already-gone conversation answers HTTP 200 with status:1 "删除失败".
	// Ignoring it would report a leaked history entry as a clean delete.
	var wrap struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("glm conversation/delete decode: %w", err)
	}
	if wrap.Status != 0 {
		return fmt.Errorf("glm conversation/delete rejected: status=%d message=%q", wrap.Status, wrap.Message)
	}
	return nil
}

// Send performs one conversation turn. conversationID may be empty (new
// conversation); the response always carries the id to reuse.
func (c *Client) Send(text, conversationID string) (*TurnResult, error) {
	if err := c.Refresh(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	access := c.access
	c.mu.Unlock()

	assistantID, chatMode := c.assistantAndMode()
	turnBody, _ := json.Marshal(map[string]any{
		"assistant_id":    assistantID,
		"conversation_id": conversationID,
		"project_id":      "",
		"chat_type":       "user_chat",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
		},
		"meta_data": map[string]any{
			"channel":             "",
			"chat_mode":           chatMode,
			"draft_id":            "",
			"if_plus_model":       true,
			"input_question_type": "xxxx",
			"is_networking":       true,
			"is_test":             false,
			"platform":            "pc",
			"quote_log_id":        "",
			"cogview":             map[string]any{"rm_label_watermark": false},
		},
	})

	start := time.Now()
	resp, err := c.do(http.MethodPost, c.base()+"/chatglm/backend-api/assistant/stream", access, string(turnBody))
	if err != nil {
		return nil, fmt.Errorf("glm turn: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			c.mu.Lock()
			c.access, c.accessUntil = "", time.Time{} // force a refresh next turn
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: turn rejected (HTTP %d)", webapi.ErrSessionExpired, resp.StatusCode)
		}
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	res, err := c.consumeStream(resp.Body)
	res.Duration = time.Since(start)
	return res, err
}

// consumeStream reads the SSE body. Events carry FULL parts[] snapshots —
// the text is rebuilt from the latest snapshot, never appended across events
// (the DeepSeek Nov-2026 replaceFragments pattern; fixture-tested).
func (c *Client) consumeStream(r io.Reader) (*TurnResult, error) {
	res := &TurnResult{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if c.DebugDump != nil {
			fmt.Fprintln(c.DebugDump, line)
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Parts          []struct {
				Content []struct {
					Type  string `json:"type"`
					Text  string `json:"text"`
					Think string `json:"think"`
				} `json:"content"`
			} `json:"parts"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue // non-JSON line: not a protocol event
		}
		if ev.ConversationID != "" && res.ConversationID == "" {
			res.ConversationID = ev.ConversationID
		}
		// Snapshot replace: rebuild text/reasoning from THIS event only.
		var t, k strings.Builder
		for _, p := range ev.Parts {
			for _, item := range p.Content {
				switch item.Type {
				case "text":
					t.WriteString(item.Text)
				case "think":
					k.WriteString(item.Think)
				}
			}
		}
		if t.Len() > 0 {
			res.Content = t.String()
		}
		if k.Len() > 0 {
			res.Reasoning = k.String()
		}
		if ev.Status == "finish" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("glm stream: %w", err)
	}
	if strings.TrimSpace(res.Content) == "" && res.ConversationID == "" {
		return res, fmt.Errorf("glm stream carried no conversation and no content")
	}
	return res, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ReplaceAll(hex.EncodeToString(b[:]), "-", "")
}
