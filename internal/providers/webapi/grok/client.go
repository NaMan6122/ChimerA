// Package grok replays grok.com's own web API over HTTP — no browser
// (internal spec 014). Auth is cookie custody (the sso/sso-rw JWT pair plus
// the rest of the grok.com jar), turns are JSON POSTs to
// /rest/app-chat/conversations/new (fresh) or …/{cid}/responses (continuation
// via responseId — the provider keeps the transcript), and the answer is
// parsed from JSON line frames carrying cumulative-response tokens.
package grokweb

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the grok.com origin; overridable in tests.
const BaseURL = "https://grok.com"

const (
	conversationsNewPath     = "/rest/app-chat/conversations/new"
	conversationsRespondPath = "/rest/app-chat/conversations/%s/responses"
	rateLimitsPath           = "/rest/rate-limits"

	// userAgent matches a current desktop Chrome; Cloudflare binds
	// cf_clearance to the harvesting UA, so a mismatched constant would make
	// those cookies worthless — hence they are stripped (see cookiesForSend)
	// and the jar rides on sso alone until measured otherwise.
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)

// modeIDs are grok.com's payload-level model selector (field modeId).
// Unknown names fall back to "auto" — the server's own routing.
var modeIDs = map[string]bool{"auto": true, "fast": true, "expert": true, "heavy": true}

// Client is a low-level grok.com web API client. Not safe for concurrent
// Sends on one conversation (the provider serialises).
type Client struct {
	BaseURL string
	http    *http.Client
	cookies map[string]string
	modeID  string

	reqID atomic.Int64

	// DebugDump, when set, receives every raw response line (GROK_DUMP).
	DebugDump io.Writer

	mu sync.Mutex // guards the atomic counter's payload randomness use
}

// NewClient builds a client from storage-state material. The session's
// access token is the "cookie-session" custody marker.
func NewClient(s *webapi.Session, timeout time.Duration, model string) *Client {
	mode := strings.ToLower(strings.TrimSpace(model))
	if !modeIDs[mode] {
		mode = "auto"
	}
	return &Client{
		BaseURL: BaseURL,
		http:    &http.Client{Timeout: timeout},
		cookies: s.Cookies,
		modeID:  mode,
	}
}

// cookiesForSend renders the jar for one request. Cloudflare's cf_clearance
// and __cf_bm are bound to the UA that earned them (the login browser's), not
// this constant — sending them under a different UA is worse than not sending
// them, so they are dropped. If a CF wall shows up live, the fix is UA
// pairing at harvest or uTLS, decided by measurement (spec 014 §4).
func (c *Client) cookiesForSend() string {
	names := make([]string, 0, len(c.cookies))
	for k := range c.cookies {
		switch k {
		case "cf_clearance", "__cf_bm":
			continue
		}
		names = append(names, k)
	}
	sortStrings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+c.cookies[k])
	}
	return strings.Join(parts, "; ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// statsigFallback generates the accepted legacy fallback header: base64
// (std, padded) of a JS TypeError line. Verified upstream against live sso
// tokens (chenyme/grok2api commit 7015258); two shapes exist, both generated:
//   x1:TypeError: Cannot read properties of null (reading 'children["<r5>"]')   r5 ∈ [a-z0-9]{5}
//   x1:TypeError: Cannot read properties of undefined (reading '<r10>')         r10 ∈ [a-z]{10}
func statsigFallback(r *rand.Rand) string {
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const alnum = lower + "0123456789"
	randN := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	var s string
	if r.Intn(2) == 0 {
		s = "x1:TypeError: Cannot read properties of null (reading 'children[\"" + randN(alnum, 5) + "\"]')"
	} else {
		s = "x1:TypeError: Cannot read properties of undefined (reading '" + randN(lower, 10) + "')"
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// deviceEnvInfo mirrors the web client's window geometry (aurora values).
func deviceEnvInfo() map[string]any {
	return map[string]any{
		"darkModeEnabled": false,
		"devicePixelRatio": 1,
		"screenHeight":     1080,
		"screenWidth":      1920,
		"viewportHeight":   945,
		"viewportWidth":    1920,
	}
}

// buildPayload assembles the conversations/new body with the web client's
// exact key set. responseID, when non-empty, is the chaining field for the
// conversations/{cid}/responses follow-up form.
func buildPayload(message, modeID, responseID string) map[string]any {
	payload := map[string]any{
		"collectionIds":               []any{},
		"disabledConnectorIds":        []any{},
		"deviceEnvInfo":               deviceEnvInfo(),
		"disableMemory":               true,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []any{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"message":                     message,
		"modeId":                      modeID,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		// temporary=false: the conversation persists so X-Session-Id threads
		// keep working across calls — continuity over tidiness.
		"temporary": false,
		"toolOverrides": map[string]any{
			"gmailSearch":          false,
			"googleCalendarSearch": false,
			"outlookSearch":        false,
			"outlookCalendarSearch": false,
			"googleDriveSearch":    false,
		},
	}
	if responseID != "" {
		payload["responseId"] = responseID
	}
	return payload
}

func (c *Client) doJSON(path string, payload map[string]any) (*http.Response, []byte, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// No manual Accept-Encoding: Go's transport then negotiates gzip AND
	// decompresses transparently. Setting it by hand turns auto-decompression
	// off, and every body arrives as compressed garbage (measured).
	req.Header.Set("Origin", c.BaseURL)
	req.Header.Set("Referer", c.BaseURL+"/")
	req.Header.Set("x-statsig-id", statsigFallback(rand.New(rand.NewSource(time.Now().UnixNano()+c.reqID.Add(1)))))
	if ck := c.cookiesForSend(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	return resp, raw, err
}

// RateLimits probes the session with the quota endpoint — a cheap
// authenticated call that also reports remainingQueries (spec 014 §2).
func (c *Client) RateLimits() (remaining int, err error) {
	resp, raw, err := c.doJSON(rateLimitsPath, map[string]any{"modelName": c.modeID})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return 0, fmt.Errorf("%w: quota probe returned HTTP %d", webapi.ErrSessionExpired, resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return 0, &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	var limits struct {
		RemainingQueries int `json:"remainingQueries"`
		TotalQueries     int `json:"totalQueries"`
	}
	if json.Unmarshal(raw, &limits) != nil {
		return 0, fmt.Errorf("unrecognized rate-limits body: %s", snippet(raw))
	}
	return limits.RemainingQueries, nil
}

// TurnResult is the assembled outcome of one turn.
type TurnResult struct {
	ConversationID string
	ResponseID     string // the userResponse id the NEXT turn chains from
	Content        string
	Reasoning      string
	SoftStop       bool
	TTFT           time.Time
	Duration       time.Duration
}

// Send performs one turn. meta.ResponseID chains the turn to the provider's
// transcript; empty meta starts a new conversation.
func (c *Client) Send(message string, meta TurnMeta) (*TurnResult, error) {
	path := conversationsNewPath
	payload := buildPayload(message, c.modeID, "")
	if meta.ConversationID != "" && meta.ResponseID != "" {
		path = fmt.Sprintf(conversationsRespondPath, meta.ConversationID)
		payload = buildPayload(message, c.modeID, meta.ResponseID)
	}
	start := time.Now()
	resp, raw, err := c.doJSON(path, payload)
	if err != nil {
		return nil, err
	}
	if err := classifyStatus(resp.StatusCode, raw); err != nil {
		return nil, err
	}
	if c.DebugDump != nil {
		fmt.Fprintln(c.DebugDump, string(raw))
	}
	res, err := parseFrames(raw)
	if err != nil {
		return nil, err
	}
	// Continuation responses do not restate the conversation id — the one in
	// the URL is the conversation.
	if res.ConversationID == "" && meta.ConversationID != "" {
		res.ConversationID = meta.ConversationID
	}
	res.Duration = time.Since(start)
	return res, nil
}

// classifyStatus maps non-200s (and the in-band error markers) onto the
// transport error vocabulary so the gateway's fallback machinery can act:
// code:7 → WAF-class (anti-bot), rate-limit bodies → HTTP 429,
// credential markers → session expired (spec 014 §5 of the research).
func classifyStatus(status int, body []byte) error {
	// The root error object appears at ANY status (403 "Model is not found"
	// measured), so it is scanned first — its message decides the class.
	var rootErr struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(body) > 0 && body[0] == '{' && json.Unmarshal(body[:min(len(body), 1<<20)], &rootErr) == nil && rootErr.Error != nil {
		return inBandError(rootErr.Error.Code, rootErr.Error.Message)
	}
	bodyStr := string(body)
	lower := strings.ToLower(bodyStr)
	if status == 200 {
		return nil
	}
	// Upstream markers are kebab-case; normalize spaces so prose-style bodies
	// ("bad credentials") match too.
	normalized := strings.ReplaceAll(lower, " ", "-")
	for _, marker := range []string{"invalid-credentials", "bad-credentials", "failed-to-look-up-session-id",
		"blocked-user", "user-is-blocked", "session-not-found", "account-suspended", "token-revoked", "token-expired"} {
		if strings.Contains(normalized, marker) {
			return fmt.Errorf("%w: grok account marker %q", webapi.ErrSessionExpired, marker)
		}
	}
	if status == 403 && strings.Contains(lower, "anti-bot") {
		return fmt.Errorf("%w: grok rejected the request (anti-bot rules)", webapi.ErrWAF)
	}
	return &webapi.HTTPError{Status: status, Body: snippet(body)}
}

func inBandError(code int, message string) error {
	lower := strings.ToLower(message)
	// The message is the authority: code 7 carries BOTH "anti-bot rules" and
	// "Model is not found" upstream (measured), so classifying on the code
	// alone mislabels model-selection failures as WAF walls.
	switch {
	case strings.Contains(lower, "anti-bot"):
		return fmt.Errorf("%w: grok anti-bot (code %d: %s)", webapi.ErrWAF, code, message)
	case strings.Contains(lower, "model is not found") || strings.Contains(lower, "model not found"):
		return fmt.Errorf("grok model unavailable for this account (modeId %q?): %s", "see GROK_WEB_MODEL", message)
	case code == 8 || strings.Contains(lower, "too many requests") || strings.Contains(lower, "rate limit"):
		return &webapi.HTTPError{Status: 429, Body: message}
	case strings.Contains(lower, "usage limit") || strings.Contains(lower, "usage quota"):
		return &webapi.HTTPError{Status: 429, Body: message}
	}
	return fmt.Errorf("grok error (code %d): %s", code, message)
}

// parseFrames decodes a Grok response body: JSON object lines, "data:"-prefixed
// or bare (the upstream omits the prefix sometimes), `event:` lines skipped.
func parseFrames(body []byte) (*TurnResult, error) {
	res := &TurnResult{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	saw := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			continue
		}
		saw = true
		applyFrame(line, res)
		if res.SoftStop {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !saw || (res.ConversationID == "" && res.Content == "") {
		return nil, fmt.Errorf("no grok frames in response (%d bytes, head: %s)", len(body), snippet(body))
	}
	return res, nil
}

// ResponseBody is the response subset Grok streams. It appears at TWO depths
// (measured): nested under result.response.* on conversations/new, and
// directly under result.* on conversations/{cid}/responses continuations.
// Exported because encoding/json cannot allocate embedded pointers of
// unexported struct types.
type ResponseBody struct {
	Token      string `json:"token"`
	IsThinking bool   `json:"isThinking"`
	MessageTag string `json:"messageTag"`
	IsSoftStop bool   `json:"isSoftStop"`

	UserResponse *struct {
		ResponseID string `json:"responseId"`
	} `json:"userResponse"`
	ModelResponse *struct {
		ResponseID       string `json:"responseId"`
		ParentResponseID string `json:"parentResponseId"`
	} `json:"modelResponse"`
}

// frame is the subset of each response line the transport consumes.
type frame struct {
	Result *struct {
		*ResponseBody // continuation shape: result.token, result.userResponse, …
		Conversation *struct {
			ConversationID string `json:"conversationId"`
		} `json:"conversation"`
		Response *ResponseBody `json:"response"` // new-conversation shape
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// applyFrame folds one frame into the result. Text tokens are cumulative
// fragments arriving in order — they APPEND (unlike Gemini's snapshots);
// thinking tokens are collected separately; tool-usage cards are skipped.
func applyFrame(line string, res *TurnResult) {
	var f frame
	if json.Unmarshal([]byte(line), &f) != nil {
		return
	}
	if f.Error != nil {
		return // classifyStatus already scanned root errors; frame errors are informational here
	}
	if f.Result == nil {
		return
	}
	if f.Result.Conversation != nil && f.Result.Conversation.ConversationID != "" {
		res.ConversationID = f.Result.Conversation.ConversationID
	}
	r := f.Result.Response
	if r == nil {
		r = f.Result.ResponseBody
	}
	if r == nil {
		return
	}
	// The next turn must chain from the ASSISTANT's node
	// (modelResponse.responseId), not the user's: measured live, chaining to
	// the userResponse id branches the thread sideways and the model loses
	// all prior context. The modelResponse frame arrives at stream end, so it
	// naturally supersedes the userResponse id captured earlier.
	if r.ModelResponse != nil && r.ModelResponse.ResponseID != "" {
		res.ResponseID = r.ModelResponse.ResponseID
	} else if r.UserResponse != nil && r.UserResponse.ResponseID != "" {
		res.ResponseID = r.UserResponse.ResponseID
	}
	if r.IsSoftStop {
		res.SoftStop = true
	}
	switch r.MessageTag {
	case "tool_usage_card", "raw_function_result":
		return
	}
	if r.Token != "" {
		if r.IsThinking {
			res.Reasoning += r.Token
		} else {
			res.Content += r.Token
			if res.TTFT.IsZero() {
				res.TTFT = time.Now()
			}
		}
	}
}

// TurnMeta carries the chaining ids between calls (provider-held state).
type TurnMeta struct {
	ConversationID string
	ResponseID     string // previous turn's userResponse.responseId
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
