package qwen

// DOM selectors for Qwen's web UI (chat.qwen.ai).
//
// ORDERING RULE: the live UI is English (document.documentElement.lang == "en"),
// so English selectors go FIRST. FindElement() waits SelectorTimeout on a miss,
// so a dead first-choice selector costs seconds on *every* request — the Chinese
// aria-labels used to sit first and burned ~5s per send. Chinese labels are kept
// as fallbacks for zh-CN deployments.
//
// Verified against the live DOM over CDP — see .anvaya/scratch/cdp_eval.py.

var ChatInput = []string{
	"textarea[placeholder]",
	"textarea#chat-input",
	"textarea",
	"div[contenteditable='true']",
}

var SendButton = []string{
	"button[aria-label='Send']",
	"button[aria-label*='Send']",
	"button[aria-label='发送']",
	"button[aria-label*='发送']",
	"button[type='submit']",
	"button[class*='send']",
	// Qwen Ant-Design UI: send lives next to .message-input-textarea,
	// often a plain ant-btn with only an SVG arrow (no aria-label).
	".message-input-textarea button",
	"button[class*='send-btn']",
	"button[class*='submit-btn']",
	"button[class*='message-input']",
	"button.ant-btn",
}

// StopButton can only be validated mid-generation (it does not exist when idle).
var StopButton = []string{
	"button[aria-label='Stop']",
	"button[aria-label='Stop generating']",
	"button[aria-label='停止']",
}

var AssistantMessage = []string{
	"div[class*='message-assistant']",
	"div[class*='assistant']",
	"div[data-role='assistant']",
	"div[class*='bot-message']",
}

var CopyButton = []string{
	"button[aria-label='Copy']",
	"button[class*='copy']",
	"button[aria-label='复制']",
}

var NewChatButton = []string{
	// Live DOM: the new-chat entry is a plain DIV, not a button or an anchor —
	// which is why every previous selector here missed and NewChat() fell back
	// to a full page navigation (~89s).
	"[aria-label='New Chat']",
	"[aria-label='新对话']",
	"div[class*='sidebar-entry-fixed-list-content']",
	"button[class*='new-chat']",
	"a[href='/']",
}

func LoginIndicators() []string {
	return ChatInput
}
