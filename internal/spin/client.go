package spin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Client talks to one gateway. One client is one conversation: Session pins the
// upstream thread, and every call sends only the messages given to it.
type Client struct {
	BaseURL string
	Token   string
	Model   string
	Session string
	HTTP    *http.Client

	// Turns counts successful requests on this session. The first turn carries
	// the system prompt and tool schemas; every later turn carries only new
	// messages, because the upstream conversation already holds the rest. That is
	// what lets a chat UI run indefinitely without resending a prefix.
	Turns int
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// NewClient builds a client with sane defaults.
func NewClient(baseURL, token, model, session string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		Model:   model,
		Session: session,
		HTTP:    &http.Client{Timeout: 3 * time.Minute},
	}
}

func (c *Client) root() string { return strings.TrimSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1") }

// Complete performs one turn. withTools is true only for the first turn of a
// conversation: the upstream keeps the schemas from then on.
func (c *Client) Complete(msgs []Message, withTools bool) (Message, time.Duration, error) {
	payload := chatRequest{Model: c.Model, Messages: msgs}
	if withTools {
		payload.Tools = ToolDefs()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Message{}, 0, err
	}
	// Retry with backoff on 429/5xx and connection errors: the turn never
	// landed, so re-sending is safe, and one transient vendor failure must
	// not kill a nine-round run at round nine. Non-retryable statuses (400,
	// 401, 404...) surface immediately.
	t0 := time.Now()
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		var req *http.Request
		req, err = http.NewRequest(http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return Message{}, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("X-Session-Id", c.Session)
		resp, err = c.HTTP.Do(req)
		if err == nil && !retryableStatus(resp.StatusCode) {
			break
		}
		if attempt >= maxSendAttempts {
			break
		}
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
		}
		time.Sleep(sendBackoff[attempt-1])
	}
	if err != nil {
		return Message{}, 0, fmt.Errorf(`cannot reach the Chimera gateway at %s
  %v

  Start it first:
    TRANSPORT=webapi PROVIDER=chatgpt chimera
  or point this client elsewhere:
    -base-url http://host:port/v1`, c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	elapsed := time.Since(t0)

	if resp.StatusCode != http.StatusOK {
		return Message{}, elapsed, fmt.Errorf("gateway %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Message{}, elapsed, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return Message{}, elapsed, fmt.Errorf("gateway error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Message{}, elapsed, fmt.Errorf("gateway returned no choices")
	}
	c.Turns++
	msg := out.Choices[0].Message
	msg.Content = stripEscapeTags(msg.Content)
	return msg, elapsed, nil
}

// maxSendAttempts bounds one Complete call: the original try plus two
// retries, with sendBackoff pacing between them.
const maxSendAttempts = 3

var sendBackoff = [...]time.Duration{time.Second, 3 * time.Second}

// retryableStatus reports whether an HTTP status may succeed on a retry:
// rate limiting and transient upstream/gateway failures only. Permanent
// request errors (400, 401, 404) must surface immediately.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// escapeTagRe matches the markers chatgpt.com wraps fenced blocks in. They are a
// transport artifact, not content: left in place they break Markdown rendering
// and leak "<escape>" into answers.
var escapeTagRe = regexp.MustCompile(`</?escape>`)

func stripEscapeTags(s string) string {
	if !strings.Contains(s, "<escape>") && !strings.Contains(s, "</escape>") {
		return s
	}
	return escapeTagRe.ReplaceAllString(s, "")
}

// NewSession switches to a fresh upstream conversation. The next turn carries
// the system prompt again.
func (c *Client) NewSession(id string) {
	c.Session = id
	c.Turns = 0
}

// Models asks the gateway what it serves. Every failure mode here has a
// different fix, so each one names its own: an unreachable port is a gateway
// that was never started, a 401 is the wrong token, a non-200 is the gateway
// itself, and an empty list is a gateway with no provider session behind it.
//
// A 429 is retried rather than reported: the gateway rate-limits this path with
// a burst of one, so whoever probed a moment earlier (a scan, a health poll, the
// previous attempt) has already spent the token. Without the wait, an
// "ask again once the bucket refills" caller would fail 100% of the time, since
// it retries milliseconds later.
func (c *Client) Models() ([]string, error) {
	var lastErr error
	for attempt, wait := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second} {
		if wait > 0 {
			time.Sleep(wait)
		}
		ids, retry, err := c.modelsOnce()
		if err == nil {
			return ids, nil
		}
		lastErr = err
		if !retry || attempt == 3 {
			return nil, err
		}
	}
	return nil, lastErr
}

// modelsOnce performs one model-list request, reporting whether a 429 makes a
// retry worthwhile.
func (c *Client) modelsOnce() (ids []string, retry bool, err error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf(`no gateway answered at %s
  %v

  start one:
    chimera up deepseek      (or: chimera up - every provider with a session)
  or point at a gateway that is already listening:
    -base-url http://localhost:8010/v1`, c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, false, fmt.Errorf(`the gateway at %s rejected the token
  pass the one it was started with:  -token <token>`, c.BaseURL)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf(`the gateway at %s is rate limiting model discovery (HTTP 429)
  retry in a moment, or pass -model explicitly`, c.BaseURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("gateway %s answered %d", c.BaseURL, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("decode %s/models: %w", c.BaseURL, err)
	}
	ids = make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, false, fmt.Errorf(`the gateway at %s serves no models
  it is listening but has no provider behind it:  chimera auth login <provider>`, c.BaseURL)
	}
	return ids, false, nil
}

// AdoptModel takes the model id from the endpoint when the caller gave none. A
// gateway serves one provider and therefore one model, so the id is a property
// of the endpoint: demanding it as a flag turns a working setup into a flag
// error the user has to look up. An explicit id is never overridden - that is
// how a mismatch gets reported instead of silently corrected.
func (c *Client) AdoptModel() error {
	if c.Model != "" {
		return nil
	}
	ids, err := c.Models()
	if err != nil {
		return err
	}
	// A gateway serves one provider, so the first id is the whole answer.
	c.Model = ids[0]
	return nil
}

// CheckModel fails before any work if the gateway answered with a model list
// that lacks the requested id. A gateway serves one provider at a time, so
// asking a ChatGPT gateway for chimera-deepseek is a configuration error worth
// naming precisely rather than a 400 mid-turn.
//
// Probe trouble is deliberately not an error: an unreachable port, a non-200 or
// an empty list is reported by the first turn, which carries the remedy for it.
func (c *Client) CheckModel() error {
	ids, err := c.Models()
	if err != nil {
		return nil
	}
	for _, id := range ids {
		if id == c.Model {
			return nil
		}
	}
	return fmt.Errorf(`%s does not serve %q
  available: %s

  a gateway serves one provider at a time. Either point at a gateway started
  with the matching provider:
    TRANSPORT=webapi PROVIDER=deepseek API_PORT=8010 chimera
    -base-url http://localhost:8010/v1 -model chimera-deepseek
  or use the model this gateway has:
    -model %s`, c.BaseURL, c.Model, strings.Join(ids, ", "), ids[0])
}

// Refresh resets the provider conversation so a new task never inherits the
// previous run's transcript.
func (c *Client) Refresh() {
	req, err := http.NewRequest(http.MethodPost, c.root()+"/v1/refresh", strings.NewReader("{}"))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.HTTP.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
}
