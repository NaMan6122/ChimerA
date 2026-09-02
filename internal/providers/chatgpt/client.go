package chatgpt

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

// Client implements the Provider interface for ChatGPT.
type Client struct {
	*providers.Base
}

// NewClient creates a new ChatGPT provider.
func NewClient(page *rod.Page, cfg *config.Config) *Client {
	return &Client{
		Base: providers.NewBase("chatgpt", "chimera-chatgpt", page, cfg),
	}
}

// Init prepares the ChatGPT provider.
func (c *Client) Init(page *rod.Page, cfg *config.Config) error {
	c.Page = page
	c.Cfg = cfg

	c.Log.Infof("Initializing ChatGPT provider (url=%s)", cfg.ChatGPTURL)

	// Navigate to ChatGPT
	err := rod.Try(func() {
		c.Page.Timeout(30 * time.Second).MustNavigate(cfg.ChatGPTURL)
	})
	if err != nil {
		return fmt.Errorf("navigating to ChatGPT: %w", err)
	}

	_ = c.Page.WaitLoad()
	time.Sleep(3 * time.Second) // Extra settle time

	// Verify login
	loggedIn, err := c.Base.IsLoggedIn(LoginIndicators())
	if err != nil {
		return fmt.Errorf("checking login: %w", err)
	}
	if !loggedIn {
		return fmt.Errorf("not logged into ChatGPT — please log in via the browser window")
	}

	c.Log.Info("ChatGPT provider initialized successfully")
	return nil
}

// SendMessage sends a message and waits for the full response.
func (c *Client) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()
	c.Log.Infof("Sending message (thread=%s, len=%d)", threadID, len(text))

	// Count existing assistant messages before sending (for logging / future copy-button logic)
	_, _ = c.CountAssistantMessages(AssistantMessage[0])

	// Random delay for human simulation
	c.RandomDelay()

	// Find the chat input
	input, err := c.FindElement(ChatInput, c.Cfg.SelectorTimeout)
	if err != nil {
		return nil, fmt.Errorf("finding chat input: %w", err)
	}

	// Focus and type
	if err := input.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, fmt.Errorf("clicking input: %w", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Use InsertText (paste-like) for contenteditable divs
	if err := c.Page.InsertText(text); err != nil {
		return nil, fmt.Errorf("typing message: %w", err)
	}

	c.RandomDelay()

	// Click send button
	sendBtn, err := c.FindElement(SendButton, 5*time.Second)
	if err != nil {
		// Fallback: press Enter
		c.Log.Warn("Send button not found, pressing Enter")
		_ = c.Page.Keyboard.Press('\r')
	} else {
		if err := c.Human.Click(sendBtn); err != nil {
			// Fallback to Enter
			c.Log.Warn("Click failed, pressing Enter")
			_ = c.Page.Keyboard.Press('\r')
		}
	}

	// Wait for response
	stopSelectors := StopButton
	if err := c.WaitForResponse(stopSelectors); err != nil {
		// Try copy button detection as fallback
		c.Log.Warnf("Stop button detection failed, trying copy button: %v", err)
	}

	// DOM settle
	time.Sleep(1 * time.Second)

	// Extract response
	responseText, err := c.ExtractLastResponseText(AssistantMessage)
	if err != nil {
		return nil, fmt.Errorf("extracting response: %w", err)
	}

	// Check for echo (got our own message back)
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

// NewChat navigates to a new ChatGPT conversation.
func (c *Client) NewChat() error {
	c.Log.Info("Starting new ChatGPT conversation")

	// Try clicking the new chat button
	newChatBtn := c.FindElementExists(NewChatButton, 3*time.Second)
	if newChatBtn != nil {
		if err := c.Human.Click(newChatBtn); err == nil {
			time.Sleep(2 * time.Second)
			return nil
		}
	}

	// Fallback: navigate to root
	err := rod.Try(func() {
		c.Page.Timeout(15 * time.Second).MustNavigate(c.Cfg.ChatGPTURL)
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

// IsLoggedIn checks if the user is logged into ChatGPT.
func (c *Client) IsLoggedIn() (bool, error) {
	return c.Base.IsLoggedIn(LoginIndicators())
}

// isEcho checks if the response is an echo of the user's message.
func (c *Client) isEcho(userMessage string) bool {
	response, err := c.ExtractResponse()
	if err != nil {
		return false
	}
	// Simple check: if the response contains a large portion of the user message
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
