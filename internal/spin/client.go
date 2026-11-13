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

// CheckModel asks what the gateway serves and fails before any work if the
// requested model is absent. A gateway serves one provider at a time, so
// asking a ChatGPT gateway for chimera-deepseek is a configuration error worth
// naming precisely rather than a 400 mid-turn.
func (c *Client) CheckModel() error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil // unreachable: Complete reports it with a remedy
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Data) == 0 {
		return nil
	}
	var names []string
	for _, m := range out.Data {
		if m.ID == c.Model {
			return nil
		}
		names = append(names, m.ID)
	}
	return fmt.Errorf(`%s does not serve %q
  available: %s

  a gateway serves one provider at a time. Either point at a gateway started
  with the matching provider:
    TRANSPORT=webapi PROVIDER=deepseek API_PORT=8010 chimera
    -base-url http://localhost:8010/v1 -model chimera-deepseek
  or use the model this gateway has:
    -model %s`, c.BaseURL, c.Model, strings.Join(names, ", "), names[0])
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
