// Package gemini replays gemini.google.com's own web API over HTTP — no
// browser (specs/013-gemini-browserless.md). Auth is cookie custody
// (__Secure-1PSID + rotating __Secure-1PSIDTS, plus SAPISID for the
// SAPISIDHASH header), the CSRF nonce SNlM0e and build label cfb2h are
// scraped from /app, turns are batchexecute StreamGenerate calls carrying
// only the new prompt plus the previous turn's (cid, rid, rcid) triple, and
// the answer is parsed from cumulative text snapshots in wrb.fr frames.
package geminiweb

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// BaseURL is the gemini.google.com origin; overridable in tests.
const BaseURL = "https://gemini.google.com"

const (
	streamGeneratePath = "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	rotateCookiesURL   = "https://accounts.google.com/RotateCookies"

	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// innerSlots is the sparse f.req inner-array length the web client sends.
	// Only well-understood slots are set (spec 013 §3); the rest stay null.
	innerSlots = 69

	// refreshAfter is how long a nonce/cookie pair is trusted before it is
	// rotated and re-scraped. 1PSIDTS rotates ~every 30 min upstream;
	// implementations keep a 10-min cadence (spec 013 §5).
	refreshAfter = 9 * time.Minute
)

// Known model hashes for the x-goog-ext-525001261-jspb selection header.
// Stale-prone by nature (Google upgraded 3.7→3.8 in place once already);
// "auto" omits the header entirely and takes the server default, which is
// the resilient path (spec 013 §6).
var modelHashes = map[string]string{
	"flash":      "fbb127bbb056c959",
	"pro":        "9d8ca3786ebdfbea",
	"flash-lite": "cf41b0e0dd7d53e5",
}

// Client is a low-level gemini.google.com web API client. It is not safe for
// concurrent Send calls on one conversation (the provider serialises), but
// the http client and counters are.
type Client struct {
	BaseURL string
	http    *http.Client
	cookies map[string]string

	// model is GEMINI_WEB_MODEL: "auto" or a name from modelHashes or a raw hash.
	model string
	// thinking selects header thinking level 2 (extended) instead of 1.
	thinking bool

	// Scraped per session; both rotate together (spec 013 §2).
	nonce     string // SNlM0e → form field "at"
	buildLabel string // cfb2h → query param "bl"
	scrapedAt  time.Time
	rotatedAt  time.Time

	reqID atomic.Int64

	// DebugDump, when set, receives every raw response line. The provider
	// wires it from GEMINI_DUMP; the stream is a batchexecute protocol with
	// no public spec, so seeing the bytes is the difference between a fix
	// and a guess.
	DebugDump io.Writer

	mu sync.Mutex // guards nonce/buildLabel/scrapedAt/rotatedAt
}

// NewClient builds a client from storage-state material. The session's
// access token is the "cookie-session" custody marker; the cookies are the
// credential.
func NewClient(s *webapi.Session, timeout time.Duration, model string, thinking bool) *Client {
	return &Client{
		BaseURL:  BaseURL,
		http:     &http.Client{Timeout: timeout},
		cookies:  s.Cookies,
		model:    strings.ToLower(strings.TrimSpace(model)),
		thinking: thinking,
	}
}

// headers builds the base header set; caller-set values win (see do).
func (c *Client) headers(origin string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", origin)
	h.Set("Referer", c.BaseURL+"/")
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

// do merges base headers under per-request headers.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers(c.BaseURL) {
		if req.Header.Get(k) == "" {
			req.Header[k] = v
		}
	}
	return c.http.Do(req)
}



// sapisidAuth builds the SAPISIDHASH Authorization header Google's first-party
// frontends send: SAPISIDHASH <ts>_<sha1("<ts> <SAPISID> <origin>")>.
func sapisidAuth(ts int64, sapSID, origin string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d %s %s", ts, sapSID, origin)))
	return fmt.Sprintf("SAPISIDHASH %d_%s", ts, hex.EncodeToString(sum[:]))
}

func (c *Client) authHeader() string {
	sap := c.cookies["SAPISID"]
	if sap == "" {
		return ""
	}
	return sapisidAuth(time.Now().Unix(), sap, c.BaseURL)
}

var (
	nonceRe = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	blRe    = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
)

// ScrapeNonce fetches /app and extracts the CSRF nonce (SNlM0e) and build
// label (cfb2h). The nonce is only issued to authenticated sessions, so a
// miss is evidence about the cookie jar, not just the page: callers turn it
// into the session check.
func (c *Client) ScrapeNonce() error {
	req, _ := http.NewRequest("GET", c.BaseURL+"/app", nil)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if strings.Contains(resp.Request.URL.Path, "/sorry") {
		return fmt.Errorf("%w: /app rate-walled (google.com/sorry)", webapi.ErrWAF)
	}
	if resp.StatusCode != 200 {
		return &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	html := string(raw)
	nonce := firstMatch(nonceRe, html)
	bl := firstMatch(blRe, html)
	if nonce == "" {
		return fmt.Errorf("no SNlM0e on /app — jar not authenticated (re-export the session) or page changed")
	}
	c.mu.Lock()
	c.nonce, c.buildLabel, c.scrapedAt = nonce, bl, time.Now()
	c.mu.Unlock()
	return nil
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// RotateCookies asks accounts.google.com for a fresh __Secure-1PSIDTS. A 401
// here means the base __Secure-1PSID is dead: ErrSessionExpired (spec 013 §5).
func (c *Client) RotateCookies() error {
	req, _ := http.NewRequest("POST", rotateCookiesURL, strings.NewReader(`[000,"-0000000000000000000"]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	req.Header.Set("Referer", "https://accounts.google.com/")
	// Only the google.com cookie material is relevant here; sending the whole
	// jar is harmless and matches the web client.
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return fmt.Errorf("%w: RotateCookies rejected the jar", webapi.ErrSessionExpired)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	updated := 0
	for _, ck := range resp.Cookies() {
		if ck.Value == "" {
			continue
		}
		c.cookies[ck.Name] = ck.Value
		updated++
	}
	c.mu.Lock()
	c.rotatedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// ensureFresh rotates (if due) and re-scrapes (if stale). One request each,
// amortized over the refresh window.
func (c *Client) ensureFresh() error {
	c.mu.Lock()
	stale := time.Since(c.scrapedAt) > refreshAfter
	c.mu.Unlock()
	if !stale {
		return nil
	}
	c.mu.Lock()
	due := time.Since(c.rotatedAt) > refreshAfter
	c.mu.Unlock()
	if due {
		// A dead jar surfaces here as ErrSessionExpired, which the caller
		// maps to the re-export message instead of a mysterious 400.
		if err := c.RotateCookies(); err != nil {
			return err
		}
	}
	return c.ScrapeNonce()
}

// TurnMeta is the (cid, rid, rcid) continuation triple. Zero values start a
// fresh thread; the provider (Google) keeps the transcript, so nothing else
// about the conversation crosses the wire (spec 013 §3).
type TurnMeta struct {
	ConversationID string // c_<hex>
	ResponseID     string // r_<hex>
	CandidateID    string // rc_<hex>
	TurnIndex      int
}

// emptyMetadata is the 10-slot conversation metadata for a fresh thread,
// mirroring the web client's DEFAULT_METADATA.
func emptyMetadata() []any {
	return []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
}

func (m TurnMeta) metadata() []any {
	md := emptyMetadata()
	if m.ConversationID != "" {
		md[0] = m.ConversationID
		md[1] = m.ResponseID
		md[2] = m.CandidateID
	}
	return md
}

// buildInner assembles the sparse 69-slot inner array.
func buildInner(prompt string, meta TurnMeta, requestUUID string) []any {
	inner := make([]any, innerSlots)
	inner[0] = []any{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []any{"en"}
	inner[2] = meta.metadata()
	inner[6] = []any{1}
	inner[7] = 1 // streaming flag
	inner[17] = []any{[]any{meta.TurnIndex}}
	if requestUUID != "" {
		inner[59] = requestUUID
	}
	return inner
}

// modelHeader builds x-goog-ext-525001261-jspb: slot [4] carries the model
// hash, slot [15] the thinking level (1 standard, 2 extended; deep=3 is
// async-only upstream and capped here — spec 013 §3).
func (c *Client) modelHeader() (string, bool) {
	hash := ""
	switch {
	case c.model == "" || c.model == "auto":
		return "", false
	case modelHashes[c.model] != "":
		hash = modelHashes[c.model]
	default:
		hash = c.model // raw hash passthrough for models discovered live
	}
	level := any(1)
	if c.thinking {
		level = 2
	}
	hdr := make([]any, 16)
	hdr[0] = 1
	hdr[4] = hash
	hdr[8] = []any{4, 5, 6, 8}
	hdr[15] = level
	raw, _ := json.Marshal(hdr)
	return string(raw), true
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// TurnResult is the assembled outcome of one turn.
type TurnResult struct {
	ConversationID string
	ResponseID     string
	CandidateID    string
	Status         string // "done" | "running" | ""
	Content        string
	Duration       time.Duration
}

// Send performs one turn: rotate/scrape if stale, POST StreamGenerate with
// only the new prompt plus the continuation triple, parse the frame stream.
func (c *Client) Send(prompt string, meta TurnMeta) (*TurnResult, error) {
	if err := c.ensureFresh(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	nonce, bl := c.nonce, c.buildLabel
	c.mu.Unlock()
	if nonce == "" {
		return nil, fmt.Errorf("no SNlM0e available — cannot send authenticated turn")
	}

	inner := buildInner(prompt, meta, uuid4())
	innerJSON, _ := json.Marshal(inner)
	// batchexecute double-encoding: f.req is an array whose second element is
	// the inner array *as a string*.
	freq, _ := json.Marshal([]any{nil, string(innerJSON)})

	form := url.Values{}
	form.Set("at", nonce)
	form.Set("f.req", string(freq))

	q := url.Values{}
	q.Set("bl", bl)
	q.Set("hl", "en")
	q.Set("_reqid", strconv.FormatInt(c.reqID.Add(1), 10))
	q.Set("rt", "c")

	req, _ := http.NewRequest("POST", c.BaseURL+streamGeneratePath+"?"+q.Encode(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	if auth := c.authHeader(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	uuid := uuid4()
	req.Header.Set("x-goog-ext-525005358-jspb", `["`+uuid+`",1]`)
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("x-goog-ext-73010990-jspb", "[0,0,0]")
	if mh, ok := c.modelHeader(); ok {
		req.Header.Set("x-goog-ext-525001261-jspb", mh)
	}

	start := time.Now()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if strings.Contains(resp.Request.URL.Path, "/sorry") {
		return nil, fmt.Errorf("%w: turn rate-walled (google.com/sorry)", webapi.ErrWAF)
	}
	if resp.StatusCode != 200 {
		return nil, &webapi.HTTPError{Status: resp.StatusCode, Body: snippet(raw)}
	}
	if c.DebugDump != nil {
		fmt.Fprintln(c.DebugDump, string(raw))
	}

	res, err := parseFrames(raw)
	if err != nil {
		return nil, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// parseFrames decodes a StreamGenerate body: an XSSI guard line, then
// alternating length lines and JSON array lines. Every array line is a
// frame; every frame may hold several wrb.fr elements. Text snapshots are
// CUMULATIVE — later frames replace earlier ones, and within a frame the
// longest candidate string wins (spec 013 §4).
func parseFrames(body []byte) (*TurnResult, error) {
	res := &TurnResult{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == ")]}'" {
			continue
		}
		if isAllDigits(line) {
			continue // length prefix (UTF-16 units) — not needed for line scanning
		}
		var frame []any
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue // guard fragments, stray separators
		}
		applyFrame(frame, res)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if res.ResponseID == "" && res.Content == "" {
		return nil, fmt.Errorf("no wrb.fr frames in response (%d bytes, head: %s)", len(body), snippet(body))
	}
	if strings.TrimSpace(res.Content) == "" && res.Status == "done" {
		return nil, fmt.Errorf("gemini returned an empty turn (cid=%s rid=%s)", res.ConversationID, res.ResponseID)
	}
	return res, nil
}

// applyFrame processes one JSON array line, extracting the ids and the
// longest text snapshot from every wrb.fr element it holds.
func applyFrame(frame []any, res *TurnResult) {
	for _, el := range frame {
		parts, ok := el.([]any)
		if !ok || len(parts) < 3 {
			continue
		}
		if rpcid, _ := parts[0].(string); rpcid != "wrb.fr" {
			continue
		}
		innerStr, _ := parts[2].(string)
		var inner []any
		if json.Unmarshal([]byte(innerStr), &inner) != nil {
			continue
		}
		applyInner(inner, res)
	}
}

// applyInner reads one decoded inner array: ids at [1], candidates at [4].
func applyInner(inner []any, res *TurnResult) {
	if len(inner) > 1 {
		if ids, ok := inner[1].([]any); ok && len(ids) >= 2 {
			if id, _ := ids[0].(string); id != "" {
				res.ConversationID = id
			}
			if id, _ := ids[1].(string); id != "" {
				res.ResponseID = id
			}
		}
	}
	if len(inner) <= 4 {
		return
	}
	cands, ok := inner[4].([]any)
	if !ok {
		return
	}
	for _, cd := range cands {
		cand, ok := cd.([]any)
		if !ok || len(cand) == 0 {
			continue
		}
		if id, _ := cand[0].(string); id != "" {
			res.CandidateID = id
		}
		if len(cand) > 1 {
			if text := longestString(cand[1], 0); text != "" {
				res.Content = text // cumulative snapshot: replace, never append
			}
		}
		if len(cand) > 8 {
			if st, ok := cand[8].([]any); ok && len(st) > 0 {
				switch v := st[0].(type) {
				case float64:
					if v == 2 {
						res.Status = "done"
					} else if v == 1 {
						res.Status = "running"
					}
				case string:
					res.Status = v
				}
			}
		}
	}
}

// longestString finds the longest string anywhere in a candidate's [1] slot,
// skipping googleusercontent card_content markers (transient error cards).
// Depth-limited: the shape is [text0, text1, …] with occasional nesting.
func longestString(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "googleusercontent.com/card_content") {
			return ""
		}
		return t
	case []any:
		best := ""
		for _, el := range t {
			if s := longestString(el, depth+1); len(s) > len(best) {
				best = s
			}
		}
		return best
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cleanText strips the web client's tool-call residue: code-reference fences
// and card_content links that leak into answers.
var (
	codeRefRe = regexp.MustCompile("(?s)```[a-z]*code_reference&code_event_index=[0-9]+```")
	cardLinkRe = regexp.MustCompile(`https://[a-z.]*googleusercontent\.com/card_content/[^ )]*`)
)

func cleanText(s string) string {
	s = codeRefRe.ReplaceAllString(s, "")
	s = cardLinkRe.ReplaceAllString(s, "")
	return s
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
