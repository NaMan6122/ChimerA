package claude

// DOM selectors for Claude's web UI.

var ChatInput = []string{
	"div[contenteditable='true'][aria-label*='Reply']",
	"div[contenteditable='true'][data-placeholder]",
	"div.ProseMirror[contenteditable='true']",
	"div[contenteditable='true']",
}

var SendButton = []string{
	"button[aria-label='Send Message']",
	"button[aria-label='Send']",
	"button[aria-label='Send message']",
	"button[class*='send']",
}

var StopButton = []string{
	"button[aria-label='Stop Response']",
	"button[aria-label='Stop']",
	"button[aria-label='Stop generating']",
}

var AssistantMessage = []string{
	"[data-is-streaming] .font-claude-message",
	"[data-message-author='assistant']",
	".font-claude-message",
	"div.font-user-message + div",
}

var CopyButton = []string{
	"button[aria-label='Copy']",
	"button[aria-label='Copy to clipboard']",
	"button[class*='copy']",
}

var NewChatButton = []string{
	"a[href='/new']",
	"button[aria-label='Start new chat']",
	"a[class*='new-chat']",
}

// LoginIndicators returns selectors that indicate the user is logged in.
func LoginIndicators() []string {
	return ChatInput
}
