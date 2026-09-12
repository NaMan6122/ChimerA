package browser

import (
	"math/rand"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/config"
)

// HumanBehavior simulates human-like interactions.
type HumanBehavior struct {
	page           *rod.Page
	typingSpeedMin time.Duration
	typingSpeedMax time.Duration
	thinkingMin    time.Duration
	thinkingMax    time.Duration
}

// NewHumanBehavior creates a new human behavior simulator.
func NewHumanBehavior(page *rod.Page, cfg *config.Config) *HumanBehavior {
	return &HumanBehavior{
		page:           page,
		typingSpeedMin: cfg.TypingSpeedMin,
		typingSpeedMax: cfg.TypingSpeedMax,
		thinkingMin:    cfg.ThinkingPauseMin,
		thinkingMax:    cfg.ThinkingPauseMax,
	}
}

// RandomDelay waits a random duration within the thinking pause range.
func (h *HumanBehavior) RandomDelay() {
	delay := h.thinkingMin + time.Duration(rand.Int63n(int64(h.thinkingMax-h.thinkingMin+1)))
	time.Sleep(delay)
}

// TypeText types text character by character with variable speed.
func (h *HumanBehavior) TypeText(text string) error {
	for _, char := range text {
		// Cast rune to input.Key for rod compatibility
		if err := h.page.Keyboard.Type(input.Key(char)); err != nil {
			return err
		}
		// Variable typing delay
		delay := h.typingSpeedMin + time.Duration(rand.Int63n(int64(h.typingSpeedMax-h.typingSpeedMin+1)))
		time.Sleep(delay)
	}
	return nil
}

// TypeTextFast pastes text as a whole via JavaScript (no per-char delay).
// For large prompts (>200 chars, e.g., JSON tool calls) it uses ClipboardEvent paste
// which is O(1) vs execCommand O(n) that freezes — see upstream #23.
//
// React textarea fix (Qwen/DeepSeek/Kimi): plain `el.value += text` + Event('input')
// does NOT update React's internal value tracker, so the send button never appears
// and Enter does nothing. We must use the native value setter + InputEvent with
// bubbles, reset _valueTracker, and verify value length afterwards.
func (h *HumanBehavior) TypeTextFast(text string) error {
	// Clear stale input first (selectAll + delete) — mirrors reference human_type
	_, _ = h.page.Eval(`() => {
		const el = document.activeElement;
		if (el) {
			el.focus();
			try { document.execCommand('selectAll', false, null); document.execCommand('delete', false, null); } catch(e) {}
		}
	}`)
	time.Sleep(50 * time.Millisecond)

	// Small/medium prompts: try execCommand insertText first — it fires
	// beforeinput/input with inputType=insertText which React listens to.
	// This matches manual paste behavior most closely.
	if len(text) <= 2000 {
		_, err := h.page.Eval(`(text) => {
			const el = document.activeElement;
			if (!el) return 'no-element';
			el.focus();
			// execCommand works for both contenteditable and textarea when focused
			try {
				const ok = document.execCommand('insertText', false, text);
				if (ok) {
					const v = el.isContentEditable ? (el.innerText || '') : (el.value || '');
					if (v.length >= text.length / 2) return 'ok-exec';
				}
			} catch(e) {}
			return 'exec-failed';
		}`, text)
		if err == nil {
			// Verify insertion actually stuck
			verify, verr := h.page.Eval(`(text) => {
				const el = document.activeElement;
				if (!el) return 0;
				const v = el.isContentEditable ? (el.innerText || '') : (el.value || '');
				return v.length;
			}`, text)
			if verr == nil && verify.Value.Int() >= len(text)/2 {
				return nil
			}
			// else fall through to native setter
		}
	}

	// Large prompt: use ClipboardEvent paste (instant, no freeze)
	if len(text) > 200 {
		_, err := h.page.Eval(`(text) => {
			const el = document.activeElement;
			if (!el) return 'no-element';
			el.focus();
			try {
				const dt = new DataTransfer();
				dt.setData('text/plain', text);
				const ev = new ClipboardEvent('paste', {bubbles: true, cancelable: true, clipboardData: dt});
				el.dispatchEvent(ev);
				// Fallback: if paste didn't insert, try execCommand
				if (el.isContentEditable && el.innerText.length < text.length/2) {
					document.execCommand('insertText', false, text);
				}
				return 'ok';
			} catch(e) { return 'err:'+e.message; }
		}`, text)
		if err == nil {
			if ok := h.verifyInputLength(text); ok {
				return nil
			}
			// fall through to React-native setter
		}
		// Fall through to native setter if paste failed
	}

	// React-compatible path for textarea/input: native setter + InputEvent.
	// Plain `el.value += text` bypasses React's value tracker.
	_, err := h.page.Eval(`(text) => {
		const el = document.activeElement;
		if (!el) return 'no-element';
		el.focus();
		try {
			if (el.isContentEditable) {
				el.focus();
				const ok = document.execCommand('insertText', false, text);
				if (ok) return 'ok-contenteditable';
				// last resort for contenteditable
				el.innerText = text;
				el.dispatchEvent(new InputEvent('input', {bubbles: true, data: text, inputType: 'insertText'}));
				return 'ok-ce-fallback';
			}
			// textarea / input: use native setter so React sees the change
			const proto = el.tagName === 'TEXTAREA'
				? window.HTMLTextAreaElement.prototype
				: window.HTMLInputElement.prototype;
			const desc = Object.getOwnPropertyDescriptor(proto, 'value');
			if (desc && desc.set) {
				// Reset React's tracker first (React 16+)
				if (el._valueTracker) { try { el._valueTracker.setValue(''); } catch(e) {} }
				desc.set.call(el, text);
			} else {
				el.value = text;
			}
			// Fire the events React listens to
			el.dispatchEvent(new InputEvent('input', {bubbles: true, data: text, inputType: 'insertText'}));
			el.dispatchEvent(new Event('change', {bubbles: true}));
			// Move cursor to end + scroll into view so send button activates
			try {
				el.focus();
				const n = (el.value || '').length;
				if (el.setSelectionRange) el.setSelectionRange(n, n);
				el.scrollTop = el.scrollHeight;
			} catch(e) {}
			return 'ok-native';
		} catch(e) { return 'err:'+e.message; }
	}`, text)
	if err != nil {
		// Final fallback: rod's native InsertText (CDP InputInsertText)
		_ = h.page.InsertText(text)
		_, _ = h.page.Eval(`(text) => {
			const el = document.activeElement;
			if (el) el.dispatchEvent(new InputEvent('input', {bubbles: true, data: text, inputType: 'insertText'}));
		}`, text)
		return nil
	}
	if !h.verifyInputLength(text) {
		// Native setter reported ok but value didn't stick — try CDP InsertText + re-fire input
		_ = h.page.InsertText(text)
		time.Sleep(200 * time.Millisecond)
		_, _ = h.page.Eval(`(text) => {
			const el = document.activeElement;
			if (el) el.dispatchEvent(new InputEvent('input', {bubbles: true, data: text, inputType: 'insertText'}));
		}`, text)
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// verifyInputLength checks the focused element actually holds at least half the text.
// Returns true if insertion looks successful.
func (h *HumanBehavior) verifyInputLength(text string) bool {
	res, err := h.page.Eval(`(text) => {
		const el = document.activeElement;
		if (!el) return 0;
		const v = el.isContentEditable ? (el.innerText || '') : (el.value || '');
		return v.length;
	}`, text)
	if err != nil {
		return false
	}
	return res.Value.Int() >= len(text)/2
}

// InsertText is the smart entry used by providers — delegates to TypeTextFast with length gate.
func (h *HumanBehavior) InsertText(text string) error {
	return h.TypeTextFast(text)
}

// Click performs a human-like click with hover first.
// Falls back to JS click when CDP click fails (e.g., covered element).
func (h *HumanBehavior) Click(element *rod.Element) error {
	// Small human pause + hover attempt (best effort, ignore errors)
	time.Sleep(time.Duration(50+rand.Intn(200)) * time.Millisecond)
	// Primary: rod CDP click (scrolls into view)
	if err := element.Click(proto.InputMouseButtonLeft, 1); err == nil {
		return nil
	} else {
		// Fallback: JS click (bypasses actionability checks)
		if _, jerr := element.Eval(`() => { this.click(); return true; }`); jerr == nil {
			return nil
		} else {
			// Return original CDP error with JS error context
			return err
		}
	}
}

// PressEnter presses the Enter key.
func (h *HumanBehavior) PressEnter() error {
	return h.page.Keyboard.Press('\r')
}

// MoveMouse moves the mouse to a position naturally.
func (h *HumanBehavior) MoveMouse(x, y float64) error {
	// Add slight jitter
	jitterX := x + float64(rand.Intn(5)-2)
	jitterY := y + float64(rand.Intn(5)-2)
	return h.page.Mouse.MoveTo(proto.Point{X: jitterX, Y: jitterY})
}
