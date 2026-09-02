package chatgpt

// DOM selectors for ChatGPT's web UI.
// Ordered by specificity — fallbacks are tried in sequence.

var ChatInput = []string{
	"#prompt-textarea",
	"div[contenteditable='true'][id='prompt-textarea']",
	"div[contenteditable='true'][data-placeholder]",
	"div[contenteditable='true']",
}

var SendButton = []string{
	"button[data-testid='send-button']",
	"button[aria-label='Send prompt']",
	"form button:last-of-type",
	"button[data-testid='composer-submit-btn']",
}

var StopButton = []string{
	"button[data-testid='stop-button']",
	"button[aria-label='Stop generating']",
	"button[aria-label='Stop streaming']",
}

var AssistantMessage = []string{
	"div[data-message-author-role='assistant']",
	"div.agent-turn",
	"div[class*='markdown']",
}

var CopyButton = []string{
	"button[aria-label='Copy']",
	"button[data-testid='copy-button']",
	"button[aria-label='Copy response']",
}

var NewChatButton = []string{
	"a[href='/']",
	"nav a[href='/']",
	"button[data-testid='new-chat-button']",
}

var ChatInputArea = []string{
	"div#prompt-textarea",
	"div[class*='composer']",
	"form[class*='chat']",
}

// LoginIndicators returns selectors that indicate the user is logged in.
func LoginIndicators() []string {
	return ChatInput
}
