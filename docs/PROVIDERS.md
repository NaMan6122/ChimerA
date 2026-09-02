# Provider Guide — ChatGPT, Claude, Qwen, DeepSeek

All 4 providers implement `providers.Provider:32` and share `internal/providers/base.go:93-148`.

---

## Common Base

`Base` in `base.go` provides:

- `FindElement(selectors, timeout)` — fallback list, first match wins.
- `FindElementExists(selectors, timeout)` — no error on miss.
- `WaitForResponse(stopSelectors)` — stop-button appear→disappear.
- `CountAssistantMessages(selector)` — `querySelectorAll(...).length` via `gson Value.Int()`.
- `ExtractLastResponseText(selectors)` — `msgs[msgs.length-1].innerText`.
- `ExtractTextViaCopy(copySelectors, msgIndex)` — click copy button → `navigator.clipboard.readText()`.
- `IsLoggedIn(selectors)` — any login indicator present.
- `Human` (browser/human.go) + `Log`.

Provider-specific logic overrides `Init` (nav + login check), `SendMessage` (input/send/wait/extract/echo), `WaitForStreaming` (Claude), `waitForTextStability` (Qwen/DeepSeek), `NewChat`, `ExtractResponse`, `IsLoggedIn()`.

---

## ChatGPT (`chimera-chatgpt`)

- **URL**: `CHATGPT_URL=https://chatgpt.com` (config)
- **Selectors** `internal/providers/chatgpt/selectors.go:6-53`:
  ```go
  ChatInput  = ["#prompt-textarea", "div[contenteditable='true'][id='prompt-textarea']", ...]
  SendButton = ["button[data-testid='send-button']", "button[aria-label='Send prompt']", ...]
  StopButton = ["button[data-testid='stop-button']", "button[aria-label='Stop generating']", ...]
  AssistantMessage = ["div[data-message-author-role='assistant']", "div.agent-turn", "div[class*='markdown']"]
  ```
- **SendMessage**: paste via `Page.InsertText` (works for contenteditable), click Send or Enter, `WaitForResponse(StopButton)`, extract last assistant `innerText`, echo retry.
- **NewChat**: try click `a[href='/']` else `MustNavigate(CHATGPT_URL)`.
- **Tool prompt**: `ToolPromptForChatGPT` (direct instruction).
- **Notes**: Image generation (DALL-E) would scrape rendered images à la Chimera `image_handler.py`.

## Claude (`chimera-claude`)

- **URL**: `CLAUDE_URL=https://claude.ai`
- **Selectors** `claude/selectors.go:5-47`: ProseMirror editor `div.ProseMirror[contenteditable='true']`, `[data-is-streaming]`, `.font-claude-message`.
- **WaitForStreaming**: polls `!!document.querySelector('[data-is-streaming]')` → true (streaming) then false (done). Falls back to stop button.
- **Prompt framing**: `ToolPromptForClaude` uses collaborative language to avoid prompt-injection filters.

## Qwen (`chimera-qwen`)

- **URL**: `QWEN_URL=https://chat.qwen.ai`
- **Selectors** `qwen/selectors.go`: `textarea[placeholder]`, `textarea#chat-input`, fallback `div[contenteditable='true']`; send `button[aria-label='发送']` (Chinese) + English; assistant `div[data-role='assistant']`, `div[class*='message-assistant']`.
- **Wait**: `WaitForResponse(StopButton)` → on failure `waitForTextStability()` (poll `innerText` stable 4×500ms). Qwen's stop button is less reliable, so stability wins.
- **NewChat**: `button[aria-label='新对话']` (new dialogue) else `/`.

## DeepSeek (`chimera-deepseek`)

- **URL**: `DEEPSEEK_URL=https://chat.deepseek.com`
- **Selectors** `deepseek/selectors.go`: similar to Qwen (textarea, `div[class*='send'] button`, `button[aria-label='发送']`).
- **Wait/extract**: same as Qwen (stop → stability). DeepSeek streams quickly; stability detection is robust.

---

## Adding a New Provider (e.g., Perplexity, Minimax)

1. Create `internal/providers/<name>/client.go` and `selectors.go` copying Qwen template.
2. Define `ChatInput`, `SendButton`, `StopButton`, `AssistantMessage`, `CopyButton`, `NewChatButton`, `LoginIndicators()` with fallback chains.
3. Implement `type Client struct{ *providers.Base }`, `NewClient`, `Init`, `SendMessage`, `NewChat`, `ExtractResponse`, `IsLoggedIn()`, `isEcho`, `waitForTextStability` if needed.
4. Add const `ProviderX = "x"` + entry in `SupportedProviders` + URL field in `Config` + `ProviderURL()` switch (`internal/config/config.go:16-161`).
5. Wire in `cmd/chimera/main.go:112-122` `createProvider` switch and `.env.example` `PROVIDER=x` + `X_URL`.
6. Live-test selectors: `go run ./cmd/chimera --provider=x` then manually log in via headful window; verify `SendMessage` scrapes.

---

## Selector Maintenance

All selectors are ordered fallbacks; first hit wins within `SELECTOR_TIMEOUT` (default 10s). When vendors ship UI redesigns, only `selectors.go` changes — no logic rewrite (same as Chimera's `selectors.py` centralizing breakages).

Debug hint: set `LOG_LEVEL=debug` and `VERBOSE=true` to see `Waiting for response`, `Response streaming detected`, `Echo detected` logs per turn.

