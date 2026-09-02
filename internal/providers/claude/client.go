package claude

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
)

// Client implements the Provider interface for Claude.
type Client struct {
	*providers.Base
}

// NewClient creates a new Claude provider.
func NewClient(page *rod.Page, cfg *config.Config) *Client {
	return &Client{
		Base: providers.NewBase("claude", "chimera-claude", page, cfg),
	}
}

// Init prepares the Claude provider.
func (c *Client) Init(page *rod.Page, cfg *config.Config) error {
	c.Page = page
	c.Cfg = cfg

	c.Log.Infof("Initializing Claude provider (url=%s)", cfg.ClaudeURL)

	err := rod.Try(func() {
		c.Page.Timeout(30 * time.Second).MustNavigate(cfg.ClaudeURL)
	})
	if err != nil {
		return fmt.Errorf("navigating to Claude: %w", err)
	}

	_ = c.Page.WaitLoad()
	time.Sleep(3 * time.Second)

	loggedIn, err := c.Base.IsLoggedIn(LoginIndicators())
	if err != nil {
		return fmt.Errorf("checking login: %w", err)
	}
	if !loggedIn {
		return fmt.Errorf("not logged into Claude — please log in via the browser window")
	}

	c.Log.Info("Claude provider initialized successfully")
	return nil
}

// SendMessage sends a message and waits for the full response.
func (c *Client) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()
	c.Log.Infof("Sending message (thread=%s, len=%d)", threadID, len(text))

	_, _ = c.CountAssistantMessages(AssistantMessage[0])

	c.RandomDelay()

	input, err := c.FindElement(ChatInput, c.Cfg.SelectorTimeout)
	if err != nil {
		return nil, fmt.Errorf("finding chat input: %w", err)
	}

	if err := input.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, fmt.Errorf("clicking input: %w", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := c.Human.InsertText(text); err != nil {
		return nil, fmt.Errorf("typing message: %w", err)
	}

	c.RandomDelay()

	sendBtn, err := c.FindElement(SendButton, 5*time.Second)
	if err != nil {
		c.Log.Warn("Send button not found, pressing Enter")
		_ = c.Page.Keyboard.Press('\r')
	} else {
		if err := c.Human.Click(sendBtn); err != nil {
			c.Log.Warn("Click failed, pressing Enter")
			_ = c.Page.Keyboard.Press('\r')
		}
	}

	// Claude uses a different streaming detection mechanism
	// Wait for the streaming indicator to appear and disappear
	if c.IsSendDisabled(SendButton) {
		if msg, ok := c.HasErrorBanner(); ok {
			return nil, fmt.Errorf("send disabled: %s", msg)
		}
		return nil, fmt.Errorf("send button disabled (message too long or rate limited)")
	}
	if err := c.WaitForStreaming(); err != nil {
		c.Log.Warnf("Streaming detection failed: %v", err)
	}

	time.Sleep(1 * time.Second)

	responseText, err := c.ExtractLastResponseText(AssistantMessage)
	if err != nil {
		return nil, fmt.Errorf("extracting response: %w", err)
	}

	if c.isEcho(text) {
		c.Log.Warn("Echo detected, retrying extraction")
		time.Sleep(3 * time.Second)
		responseText, err = c.ExtractLastResponseText(AssistantMessage)
		if err != nil {
			return nil, fmt.Errorf("extracting response after echo retry: %w", err)
		}
	}

	elapsed := time.Since(start).Milliseconds()
	c.Log.Infof("Response received (%d chars, %dms)", len(responseText), elapsed)

	return &models.ProviderResponse{
		Message:   responseText,
		ThreadID:  threadID,
		ElapsedMs: elapsed,
	}, nil
}

// WaitForStreaming waits for Claude's streaming to complete.
// Claude uses a data-is-streaming attribute that is removed when done.
func (c *Client) WaitForStreaming() error {
	deadline := time.Now().Add(c.Cfg.ResponseTimeout)

	// Phase 1: Wait for streaming to start
	for time.Now().Before(deadline) {
		result, err := c.Page.Eval(`() => !!document.querySelector('[data-is-streaming]')`)
		if err == nil && result.Value.Bool() {
			c.Log.Debugf("Claude streaming detected")
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Phase 2: Wait for streaming to end
	for time.Now().Before(deadline) {
		result, err := c.Page.Eval(`() => !!document.querySelector('[data-is-streaming]')`)
		if err == nil && !result.Value.Bool() {
			c.Log.Debugf("Claude streaming complete")
			time.Sleep(1 * time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("claude response timeout")
}

// NewChat starts a new Claude conversation.
func (c *Client) NewChat() error {
	c.Log.Info("Starting new Claude conversation")

	newChatBtn := c.FindElementExists(NewChatButton, 3*time.Second)
	if newChatBtn != nil {
		if err := c.Human.Click(newChatBtn); err == nil {
			time.Sleep(2 * time.Second)
			return nil
		}
	}

	err := rod.Try(func() {
		c.Page.Timeout(15 * time.Second).MustNavigate(c.Cfg.ClaudeURL)
	})
	if err != nil {
		return fmt.Errorf("navigating to new chat: %w", err)
	}
	_ = c.Page.WaitLoad()
	time.Sleep(2 * time.Second)
	return nil
}

// ExtractResponse reads the last assistant message from the page.
func (c *Client) ExtractResponse() (string, error) {
	return c.ExtractLastResponseText(AssistantMessage)
}

// IsLoggedIn checks if the user is logged into Claude.
func (c *Client) IsLoggedIn() (bool, error) {
	return c.Base.IsLoggedIn(LoginIndicators())
}

func (c *Client) isEcho(userMessage string) bool {
	response, err := c.ExtractResponse()
	if err != nil {
		return false
	}
	if len(userMessage) > 20 && strings.Contains(response, userMessage[:min(50, len(userMessage))]) {
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
