package deepseek

// DOM selectors for DeepSeek's web UI (chat.deepseek.com).

var ChatInput = []string{
	"textarea[placeholder]",
	"textarea#chat-input",
	"textarea",
	"div[contenteditable='true']",
}

var SendButton = []string{
	"div[class*='send'] button",
	"button[aria-label='Send']",
	"button[aria-label='发送']",
	"button[type='submit']",
}

var StopButton = []string{
	"button[aria-label='Stop']",
	"button[aria-label='停止']",
	"button[aria-label='Stop generating']",
}

var AssistantMessage = []string{
	"div[data-role='assistant']",
	"div[class*='message-assistant']",
	"div[class*='bot-message']",
	"div[class*='assistant']",
}

var CopyButton = []string{
	"button[aria-label='Copy']",
	"button[aria-label='复制']",
	"button[class*='copy']",
}

var NewChatButton = []string{
	"button[aria-label='新对话']",
	"button[aria-label='New chat']",
	"a[href='/']",
	"button[class*='new-chat']",
}

func LoginIndicators() []string {
	return ChatInput
}
