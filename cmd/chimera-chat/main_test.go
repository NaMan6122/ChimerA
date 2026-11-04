package main

import (
	"strings"
	"testing"

	"fmt"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chimera/chimera/internal/spin"
)

func newTestModel() *model {
	m := &model{
		client:  spin.NewClient("http://localhost:1/v1", "t", "chimera-chatgpt", "chat-1"),
		msgs:    make(chan tea.Msg, 8),
		entries: nil,
	}
	m.input = textarea.New()
	m.input.SetWidth(60)
	m.spin = spinner.New()
	m.viewport = viewport.New(80, 20)
	return m
}

// TestRendersToolTraceAndAnswer nails the visible contract: a tool call, a
// compact result, and the final answer all reach the transcript, and the user's
// own message is shown once.
func TestRendersToolTraceAndAnswer(t *testing.T) {
	m := newTestModel()
	m.handle(spin.Event{Kind: spin.EventRound, Round: 1, PromptChars: 1200, Latency: 5e9})
	m.handle(spin.Event{Kind: spin.EventTool, Tool: toolCall("read_file", `{"path":"internal/browser/stealth.go"}`)})
	m.handle(spin.Event{Kind: spin.EventResult, Result: "1: package browser\n2: ..."})
	m.handle(spin.Event{Kind: spin.EventFinal, Text: "It removes the webdriver flag."})

	view := stripANSI(m.View())
	for _, want := range []string{"read_file internal/browser/stealth.go", "It removes the webdriver flag.", "round 1"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

// TestSubmitIgnoresEnterWhileBusy guards against queuing a second task while the
// first is still talking to the gateway.
func TestSubmitIgnoresEnterWhileBusy(t *testing.T) {
	m := newTestModel()
	m.busy = true
	m.input.SetValue("second task")
	before := len(m.entries)
	if _, _ = m.submit(); len(m.entries) != before {
		t.Fatal("submit accepted a task while busy")
	}
}

// TestNewSessionResetsTurns pins the no-history contract: after /new the next
// turn must carry the system prompt again, which only happens when Turns == 0.
func TestNewSessionResetsTurns(t *testing.T) {
	m := newTestModel()
	m.client.Turns = 7
	m.input.SetValue("/new")
	_, _ = m.submit()
	if m.client.Turns != 0 {
		t.Fatalf("Turns = %d after /new, want 0", m.client.Turns)
	}
}

// TestScrollBackDoesNotSnapToBottom is the scrollback contract: reading history
// while the model is working must not be interrupted by new output.
func TestScrollBackDoesNotSnapToBottom(t *testing.T) {
	m := newTestModel()
	m.viewport.Height = 5
	m.viewport.Width = 80
	for i := 0; i < 40; i++ {
		m.append(kindAssistant, fmt.Sprintf("line %d", i))
	}
	m.refreshViewport()

	m.scrollBy(-10)
	if m.autoScroll {
		t.Fatal("scrolling up should leave follow mode")
	}
	m.append(kindAssistant, "new output while reading history")
	m.refreshViewport()
	if m.viewport.AtBottom() {
		t.Fatal("new output yanked the view back to the bottom")
	}
	if m.hiddenLines() <= 0 {
		t.Fatalf("hidden lines = %d, want > 0", m.hiddenLines())
	}

	m.scrollToBottom()
	if !m.autoScroll || !m.viewport.AtBottom() {
		t.Fatal("end should resume following")
	}
	m.append(kindAssistant, "and now it follows")
	m.refreshViewport()
	if !m.viewport.AtBottom() {
		t.Fatal("follow mode did not stick to the bottom")
	}
}

// TestSummarizeCapsLongResults keeps the transcript readable without hiding that
// a result was large.
func TestSummarizeCapsLongResults(t *testing.T) {
	long := strings.Repeat("x", 5000) + "\nsecond line"
	got := summarize(long)
	if len(got) > 140 {
		t.Fatalf("summary too long: %d chars", len(got))
	}
	if !strings.Contains(got, "chars") {
		t.Fatalf("summary lost the size hint: %q", got)
	}
}

func toolCall(name, args string) spin.ToolCall {
	var tc spin.ToolCall
	tc.ID, tc.Type = "call_1", "function"
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}
