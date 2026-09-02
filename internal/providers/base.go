package providers

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
)

// Base provides shared functionality for all providers.
type Base struct {
	Name_    string
	ModelID_ string
	Page     *rod.Page
	Cfg      *config.Config
	Human    *browser.HumanBehavior
	Log      *logging.Logger
}

// NewBase creates a base provider with common fields.
func NewBase(name, modelID string, page *rod.Page, cfg *config.Config) *Base {
	return &Base{
		Name_:    name,
		ModelID_: modelID,
		Page:     page,
		Cfg:      cfg,
		Human:    browser.NewHumanBehavior(page, cfg),
		Log:      logging.New(name, cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

func (b *Base) Name() string    { return b.Name_ }
func (b *Base) ModelID() string { return b.ModelID_ }

// FindElement tries multiple CSS selectors and returns the first match.
func (b *Base) FindElement(selectors []string, timeout time.Duration) (*rod.Element, error) {
	for _, sel := range selectors {
		el, err := b.Page.Timeout(timeout).Element(sel)
		if err == nil {
			return el, nil
		}
	}
	return nil, fmt.Errorf("no element found for selectors: %v", selectors)
}

// FindElementExists checks if any of the selectors match (no error on miss).
func (b *Base) FindElementExists(selectors []string, timeout time.Duration) *rod.Element {
	for _, sel := range selectors {
		el, err := b.Page.Timeout(timeout).Element(sel)
		if err == nil {
			return el
		}
	}
	return nil
}

// WaitForResponse waits for the response to complete by monitoring the stop button.
// Includes early exit for error banners (e.g., "message too long") to avoid 2m hang.
func (b *Base) WaitForResponse(stopSelectors []string) error {
	b.Log.Debugf("Waiting for response (timeout=%v)", b.Cfg.ResponseTimeout)

	deadline := time.Now().Add(b.Cfg.ResponseTimeout)

	// Phase 1: Wait for stop button to appear (streaming started)
	for time.Now().Before(deadline) {
		if msg, ok := b.HasErrorBanner(); ok {
			return fmt.Errorf("provider error banner: %s", msg)
		}
		el := b.FindElementExists(stopSelectors, 500*time.Millisecond)
		if el != nil {
			b.Log.Debugf("Response streaming detected (stop button found)")
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Phase 2: Wait for stop button to disappear (streaming finished)
	for time.Now().Before(deadline) {
		if msg, ok := b.HasErrorBanner(); ok {
			return fmt.Errorf("provider error banner: %s", msg)
		}
		el := b.FindElementExists(stopSelectors, 500*time.Millisecond)
		if el == nil {
			b.Log.Debugf("Response complete (stop button gone)")
			time.Sleep(1 * time.Second) // DOM settle
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("response timeout after %v", b.Cfg.ResponseTimeout)
}

// HasErrorBanner checks for provider error banners like "message too long".
func (b *Base) HasErrorBanner() (string, bool) {
	res, err := b.Page.Eval(`() => {
		const text = document.body ? document.body.innerText : '';
		const markers = [
			'message too long',
			'attachment too large',
			'something went wrong',
			'try again',
			'rate limit',
			'too many requests',
			'error generating'
		];
		const lower = text.toLowerCase();
		for (const m of markers) {
			if (lower.includes(m)) {
				// Return snippet around marker
				const idx = lower.indexOf(m);
				return text.slice(Math.max(0, idx-40), idx+80);
			}
		}
		return '';
	}`)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(res.Value.Str())
	if s != "" {
		return s, true
	}
	return "", false
}

// IsSendDisabled checks if the send button is disabled (upstream #17).
func (b *Base) IsSendDisabled(sendSelectors []string) bool {
	// Check via DOM disabled attribute and outerText
	for _, sel := range sendSelectors {
		selEsc := strings.ReplaceAll(sel, `"`, `\"`)
		res, err := b.Page.Eval(fmt.Sprintf(`() => {
			const el = document.querySelector("%s");
			if (!el) return 'no-el';
			if (el.disabled) return 'disabled';
			if (el.getAttribute('aria-disabled') === 'true') return 'disabled';
			if (el.getAttribute('data-disabled') === 'true') return 'disabled';
			const cls = el.className || '';
			if (cls.includes('disabled') || cls.includes('opacity-50')) return 'maybe-disabled';
			return 'enabled';
		}`, selEsc))
		if err == nil {
			v := res.Value.Str()
			if v == "disabled" {
				return true
			}
		}
	}
	return false
}

// CountAssistantMessages counts the number of assistant messages on the page.
func (b *Base) CountAssistantMessages(selector string) (int, error) {
	// Use double quotes to wrap selector so single quotes inside (e.g., [role='assistant']) don't break JS
	sel := strings.ReplaceAll(selector, `"`, `\"`)
	result, err := b.Page.Eval(fmt.Sprintf(`() => document.querySelectorAll("%s").length`, sel))
	if err != nil {
		return 0, err
	}
	return result.Value.Int(), nil
}

// ExtractTextViaCopy uses the copy button to extract text cleanly.
func (b *Base) ExtractTextViaCopy(copySelectors []string, msgIndex int) (string, error) {
	// Try to find a copy button on the Nth assistant message
	for _, sel := range copySelectors {
		// Get all copy buttons
		buttons, err := b.Page.Elements(sel)
		if err != nil || len(buttons) == 0 {
			continue
		}

		// The Nth button corresponds to the Nth assistant message
		if msgIndex >= 0 && msgIndex < len(buttons) {
			btn := buttons[msgIndex]
			_ = btn.Click(proto.InputMouseButtonLeft, 1)
			time.Sleep(500 * time.Millisecond)

			// Read from clipboard
			result, err := b.Page.Eval(`navigator.clipboard.readText()`)
			if err != nil {
				// Fallback: try to get the text from the message element directly
				continue
			}
			return result.Value.Str(), nil
		}
	}

	return "", fmt.Errorf("could not extract text via copy button")
}

// ExtractLastResponseText extracts the text of the last assistant message.
func (b *Base) ExtractLastResponseText(messageSelectors []string) (string, error) {
	for _, sel := range messageSelectors {
		selEsc := strings.ReplaceAll(sel, `"`, `\"`)
		result, err := b.Page.Eval(fmt.Sprintf(`() => {
				const msgs = document.querySelectorAll("%s");
				if (msgs.length === 0) return '';
				return msgs[msgs.length - 1].innerText;
			}`, selEsc))
		if err == nil {
			text := result.Value.Str()
			if text != "" {
				return text, nil
			}
		}
	}
	return "", fmt.Errorf("no assistant messages found")
}

// RandomDelay waits a random duration for human simulation.
func (b *Base) RandomDelay() {
	b.Human.RandomDelay()
}

// EnsureLoggedIn checks login status and returns error if not logged in.
func (b *Base) EnsureLoggedIn(loginSelectors []string) error {
	loggedIn, err := b.IsLoggedIn(loginSelectors)
	if err != nil {
		return fmt.Errorf("checking login status: %w", err)
	}
	if !loggedIn {
		return fmt.Errorf("not logged in to %s — please log in via the browser", b.Name_)
	}
	return nil
}

// IsLoggedIn checks if any of the login indicators are present.
func (b *Base) IsLoggedIn(selectors []string) (bool, error) {
	for _, sel := range selectors {
		el := b.FindElementExists([]string{sel}, 2*time.Second)
		if el != nil {
			return true, nil
		}
	}
	return false, nil
}

// ScrollToBottom scrolls the chat container to the bottom.
func (b *Base) ScrollToBottom() {
	_, _ = b.Page.Eval(`() => { 
		const el = document.querySelector('[class*="scroll"]') || document.documentElement;
		el.scrollTop = el.scrollHeight;
	}`)
	time.Sleep(300 * time.Millisecond)
}

// InjectClipboardPolyfill ensures clipboard API is available.
func (b *Base) InjectClipboardPolyfill() {
	_, _ = b.Page.Eval(`async () => {
		if (!navigator.clipboard) {
			navigator.clipboard = {
				readText: async () => '',
				writeText: async () => {},
			};
		}
	}`)
}
