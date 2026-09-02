package qwen

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

// Client implements the Provider interface for Qwen.
type Client struct {
	*providers.Base
}

// NewClient creates a new Qwen provider.
func NewClient(page *rod.Page, cfg *config.Config) *Client {
	return &Client{
		Base: providers.NewBase("qwen", "chimera-qwen", page, cfg),
	}
}

// Init prepares the Qwen provider.
func (c *Client) Init(page *rod.Page, cfg *config.Config) error {
	c.Page = page
	c.Cfg = cfg

	c.Log.Infof("Initializing Qwen provider (url=%s)", cfg.QwenURL)

	err := rod.Try(func() {
		c.Page.Timeout(30 * time.Second).MustNavigate(cfg.QwenURL)
	})
	if err != nil {
		return fmt.Errorf("navigating to Qwen: %w", err)
	}

	_ = c.Page.WaitLoad()
	time.Sleep(3 * time.Second)

	loggedIn, err := c.Base.IsLoggedIn(LoginIndicators())
	if err != nil {
		return fmt.Errorf("checking login: %w", err)
	}
	if !loggedIn {
		return fmt.Errorf("not logged into Qwen — please log in via the browser window")
	}

	c.Log.Info("Qwen provider initialized successfully")
	return nil
}

// SendMessage sends a message and waits for the full response.
func (c *Client) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()
	c.Log.Infof("Sending message (thread=%s, len=%d)", threadID, len(text))

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

	// Wait for response via stop button lifecycle
	// Early disabled/error check (upstream #17)
	if c.IsSendDisabled(SendButton) {
		if msg, ok := c.HasErrorBanner(); ok {
			return nil, fmt.Errorf("send disabled: %s", msg)
		}
		return nil, fmt.Errorf("send button disabled (message too long or rate limited)")
	}
	if err := c.WaitForResponse(StopButton); err != nil {
		// Fallback: text stability detection
		c.Log.Warnf("Stop button detection failed: %v, trying text stability", err)
		if err := c.waitForTextStability(); err != nil {
			c.Log.Warnf("Text stability failed too: %v", err)
		}
	}

	time.Sleep(1 * time.Second)

	responseText, err := c.ExtractLastResponseText(AssistantMessage)
	if err != nil {
		return nil, fmt.Errorf("extracting response: %w", err)
	}

	if c.isEcho(text) {
		c.Log.Warn("Echo detected, retrying")
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

// waitForTextStability polls the last message text until it stops changing.
func (c *Client) waitForTextStability() error {
	deadline := time.Now().Add(c.Cfg.ResponseTimeout)
	var lastText string
	stableCount := 0

	for time.Now().Before(deadline) {
		text, err := c.ExtractLastResponseText(AssistantMessage)
		if err != nil || text == "" {
			time.Sleep(1 * time.Second)
			continue
		}

		if text == lastText {
			stableCount++
			if stableCount >= 4 {
				return nil
			}
		} else {
			stableCount = 0
			lastText = text
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("text stability timeout")
}

// NewChat starts a new Qwen conversation.
func (c *Client) NewChat() error {
	c.Log.Info("Starting new Qwen conversation")

	newChatBtn := c.FindElementExists(NewChatButton, 3*time.Second)
	if newChatBtn != nil {
		if err := c.Human.Click(newChatBtn); err == nil {
			time.Sleep(2 * time.Second)
			return nil
		}
	}

	err := rod.Try(func() {
		c.Page.Timeout(15 * time.Second).MustNavigate(c.Cfg.QwenURL)
	})
	if err != nil {
		return fmt.Errorf("navigating to new chat: %w", err)
	}
	_ = c.Page.WaitLoad()
	time.Sleep(2 * time.Second)
	return nil
}

// ExtractResponse reads the last assistant message.
func (c *Client) ExtractResponse() (string, error) {
	return c.ExtractLastResponseText(AssistantMessage)
}

// IsLoggedIn checks if the user is logged into Qwen.
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
