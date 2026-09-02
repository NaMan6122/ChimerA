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
func (h *HumanBehavior) TypeTextFast(text string) error {
	// Use JS insertText for contenteditable divs (paste-like behavior)
	_, err := h.page.Eval(`(text) => {
		const el = document.activeElement;
		if (el && el.isContentEditable) {
			el.focus();
			document.execCommand('insertText', false, text);
		} else if (el) {
			el.value += text;
			el.dispatchEvent(new Event('input', { bubbles: true }));
		}
	}`, text)
	return err
}

// Click performs a human-like click with hover first.
func (h *HumanBehavior) Click(element *rod.Element) error {
	// Get element position via JS
	result, err := element.Eval(`(el) => {
		const rect = el.getBoundingClientRect();
		return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };
	}`)
	if err != nil {
		return err
	}

	// Parse the result
	var pos struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := result.Value.Unmarshal(&pos); err != nil {
		return err
	}

	// Hover with slight offset
	offsetX := rand.Float64()*10 - 5 // ±5px
	offsetY := rand.Float64()*10 - 5

	_ = h.page.Mouse.MoveTo(proto.Point{X: pos.X + offsetX, Y: pos.Y + offsetY})
	time.Sleep(time.Duration(50+rand.Intn(200)) * time.Millisecond)

	// Click
	return element.Click(proto.InputMouseButtonLeft, 1)
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
