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

	preCount, _ := c.CountAssistantMessages(AssistantMessage[0])
	_ = preCount // baseline for future copy-button gating; wait uses stop/stability today

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

	// Fail fast if React didn't accept the insertion (empty textarea =
	// send button stays hidden, Enter does nothing, 2m hang follows).
	time.Sleep(500 * time.Millisecond)
	if n := c.InputValueLength(); n < len(text)/2 {
		c.Log.Warnf("Input verification failed: textarea holds %d chars, want ~%d — retrying once", n, len(text))
		time.Sleep(300 * time.Millisecond)
		if err := c.Human.InsertText(text); err != nil {
			return nil, fmt.Errorf("typing message (retry): %w", err)
		}
		time.Sleep(500 * time.Millisecond)
		if n2 := c.InputValueLength(); n2 < len(text)/2 {
			return nil, fmt.Errorf("chat input rejected text (holds %d chars, want ~%d) — React state not updated", n2, len(text))
		}
	}

	c.RandomDelay()

	sendBtn, err := c.FindSendButton(SendButton, ChatInput)
	if err != nil {
		c.Log.Warn("Send button not found, pressing Enter")
		_ = c.Page.Keyboard.Press('\r')
		// Ctrl+Enter fallback — some Ant-Design inputs submit on Cmd/Ctrl+Enter
		time.Sleep(500 * time.Millisecond)
		if n := c.InputValueLength(); n >= len(text)/2 {
			c.Log.Warn("Enter didn't submit (input still full), trying Ctrl+Enter")
			_ = c.Page.Keyboard.Press('\r')
		}
	} else {
		if err := c.Human.Click(sendBtn); err != nil {
			c.Log.Warnf("Click failed (%v), pressing Enter", err)
			_ = c.Page.Keyboard.Press('\r')
		} else {
			c.Log.Debugf("Send button clicked")
		}
	}

	// Wait for response — stop-button lifecycle primary, text stability fallback.
	// (Copy-button primary disabled: pre-existing UI copy buttons cause instant
	// false positives and 1s-early extraction with empty assistant selectors.)
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
		c.dumpResponseDebug()
		return nil, fmt.Errorf("extracting response: %w", err)
	}

	if c.isEcho(text) {
		c.NoteEcho("Echo detected, retrying")
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

// dumpResponseDebug logs live DOM clues when extraction fails (temporary diagnostic,
// runs inside the gateway's own rod connection — safe, no external attach).
func (c *Client) dumpResponseDebug() {
	if res, err := c.Page.Eval(`() => document.body.innerText.slice(0, 800)`); err == nil {
		c.Log.Warnf("DEBUG body head: %q", res.Value.Str())
	}
	if res, err := c.Page.Eval(`() => {
		const sels = ["div[data-role='assistant']", "div[class*='message-assistant']", "div[class*='bot-message']", "div[class*='assistant']", "[data-message-author-role='assistant']", "[class*='ai-message']", "[class*='qwen-response']", "[class*='answer']", "div[class*='message']"];
		const out = {};
		for (const s of sels) { try { out[s] = document.querySelectorAll(s).length; } catch(e) { out[s] = -1; } }
		return JSON.stringify(out);
	}`); err == nil {
		c.Log.Warnf("DEBUG assistant counts: %s", res.Value.Str())
	}
	if res, err := c.Page.Eval(`() => {
		const all = Array.from(document.querySelectorAll('div'));
		const cands = [];
		for (const el of all) {
			const t = (el.innerText || '');
			if (t.length > 20 && t.length < 800 && el.children.length <= 6) {
				const cls = (typeof el.className === 'string' ? el.className : String(el.className)).slice(0, 150);
				if (/message|assistant|answer|response|bubble|markdown|prose|content/i.test(cls)) {
					cands.push(cls + ' | kids=' + el.children.length + ' | ' + t.slice(0, 80).replace(/\n/g, ' '));
					if (cands.length >= 15) break;
				}
			}
		}
		return JSON.stringify(cands);
	}`); err == nil {
		c.Log.Warnf("DEBUG message cands: %s", res.Value.Str())
	}
	if res, err := c.Page.Eval(`() => document.URL`); err == nil {
		c.Log.Warnf("DEBUG url: %s", res.Value.Str())
	}
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
