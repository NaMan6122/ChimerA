// Package config handles all configuration loading from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/auth"
	"github.com/joho/godotenv"
)

// Provider type constants.
const (
	ProviderChatGPT  = "chatgpt"
	ProviderClaude   = "claude"
	ProviderQwen     = "qwen"
	ProviderDeepSeek = "deepseek"
	ProviderKimi     = "kimi"
	ProviderAll      = "all"
)

// SupportedProviders lists all available providers.
var SupportedProviders = []string{
	ProviderChatGPT,
	ProviderClaude,
	ProviderQwen,
	ProviderDeepSeek,
	ProviderKimi,
	ProviderAll,
}

// Transport selects how a provider is reached.
const (
	// TransportDOM drives the real browser UI (default fallback).
	TransportDOM = "dom"
	// TransportWebAPI replays the provider's own web API over HTTP, no browser.
	TransportWebAPI = "webapi"
	// TransportAuto prefers webapi when an auth session exists, else dom.
	TransportAuto = "auto"
)

// Config holds all project settings.
type Config struct {
	// Provider
	Provider string

	// Browser
	BrowserDataDir string
	Headless       bool
	SlowMo         time.Duration

	// Transport + auth session material (storage-state files, mode 0600)
	Transport string
	AuthDir   string
	// TransportFallback optionally keeps a second transport warm ("dom").
	TransportFallback string

	// WebAPI model selection (chat.qwen.ai model id + thinking toggle)
	QwenWebModel    string
	QwenWebThinking bool

	// Provider URLs
	ChatGPTURL  string
	ClaudeURL   string
	QwenURL     string
	DeepSeekURL string
	KimiURL     string

	// Timeouts
	ResponseTimeout time.Duration
	SelectorTimeout time.Duration

	// Human simulation
	TypingSpeedMin   time.Duration
	TypingSpeedMax   time.Duration
	ThinkingPauseMin time.Duration
	ThinkingPauseMax time.Duration

	// Logging
	LogDir   string
	LogLevel string
	Verbose  bool

	// API Server
	APIHost          string
	APIPort          int
	RateLimitSeconds int
	APIToken         string
	APITokens        string
	APIKeys          *auth.Keys

	// Metering (per-tenant usage + quotas). MeterDB "" disables recording.
	MeterDB              string
	QuotaMonthlyRequests int

	// VNC
	VNCPassword string

	// Viewport (base, jittered ±20px per launch)
	ViewportWidth  int
	ViewportHeight int

	// Pool concurrency per provider
	MaxConcurrentPerProviderVal int

	// Max prompt chars before fast 400 (upstream #17: send disabled / hang)
	MaxPromptChars int
}

// Load creates a Config from environment variables, with .env file support.
func Load() (*Config, error) {
	// Load .env from cwd first
	if err := godotenv.Load(); err != nil {
		// .env is optional
		_ = err
	}

	cfg := &Config{
		// Provider
		Provider: getEnvStr("PROVIDER", ProviderChatGPT),

		// Browser
		BrowserDataDir: getEnvStr("BROWSER_DATA_DIR", "./browser_data"),
		Headless:       getEnvBool("HEADLESS", false),
		SlowMo:         time.Duration(getEnvInt("SLOW_MO", 0)) * time.Millisecond,

		// Transport + auth sessions
		Transport:         getEnvStr("TRANSPORT", TransportAuto),
		TransportFallback: getEnvStr("TRANSPORT_FALLBACK", ""),
		AuthDir:           getEnvStr("AUTH_DIR", "./auth_data"),

		// WebAPI model
		QwenWebModel:    getEnvStr("QWEN_WEB_MODEL", "qwen3.8-max"),
		QwenWebThinking: getEnvBool("QWEN_WEB_THINKING", true),

		// Provider URLs
		ChatGPTURL:  getEnvStr("CHATGPT_URL", "https://chatgpt.com"),
		ClaudeURL:   getEnvStr("CLAUDE_URL", "https://claude.ai"),
		QwenURL:     getEnvStr("QWEN_URL", "https://chat.qwen.ai"),
		DeepSeekURL: getEnvStr("DEEPSEEK_URL", "https://chat.deepseek.com"),
		KimiURL:     getEnvStr("KIMI_URL", "https://www.kimi.com"),

		// Timeouts
		ResponseTimeout: time.Duration(getEnvInt("RESPONSE_TIMEOUT", 120000)) * time.Millisecond,
		SelectorTimeout: time.Duration(getEnvInt("SELECTOR_TIMEOUT", 10000)) * time.Millisecond,

		// Human simulation
		TypingSpeedMin:   time.Duration(getEnvInt("TYPING_SPEED_MIN", 30)) * time.Millisecond,
		TypingSpeedMax:   time.Duration(getEnvInt("TYPING_SPEED_MAX", 120)) * time.Millisecond,
		ThinkingPauseMin: time.Duration(getEnvInt("THINKING_PAUSE_MIN", 400)) * time.Millisecond,
		ThinkingPauseMax: time.Duration(getEnvInt("THINKING_PAUSE_MAX", 1500)) * time.Millisecond,

		// Logging
		LogDir:   getEnvStr("LOG_DIR", "./logs"),
		LogLevel: getEnvStr("LOG_LEVEL", "debug"),
		Verbose:  getEnvBool("VERBOSE", true),

		// API Server
		APIHost:          getEnvStr("API_HOST", "0.0.0.0"),
		APIPort:          getEnvInt("API_PORT", 8000),
		RateLimitSeconds: getEnvInt("RATE_LIMIT_SECONDS", 2),
		APIToken:         getEnvStr("API_TOKEN", ""),
		APITokens:        getEnvStr("API_TOKENS", ""),

		// VNC
		VNCPassword: getEnvStr("VNC_PASSWORD", "chimera"),

		// Viewport
		ViewportWidth:  1280,
		ViewportHeight: 720,

		// Pool
		MaxConcurrentPerProviderVal: getEnvInt("MAX_CONCURRENT_PER_PROVIDER", 3),

		// Pre-flight guard (chars). 12000 ≈ send-disabled threshold.
		MaxPromptChars: getEnvInt("MAX_PROMPT_CHARS", 12000),
	}

	// Ensure directories exist
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("creating directories: %w", err)
	}

	// Metering defaults: on, under the log dir. Explicitly empty METER_DB disables.
	cfg.MeterDB = filepath.Join(cfg.LogDir, "usage.db")
	if v, ok := os.LookupEnv("METER_DB"); ok {
		cfg.MeterDB = strings.TrimSpace(v)
	}
	cfg.QuotaMonthlyRequests = getEnvInt("QUOTA_MONTHLY_REQUESTS", 0)

	// Parse credentials now so bad API_TOKENS fails fast instead of opening the gateway.
	keys, err := auth.New(cfg.APIToken, cfg.APITokens)
	if err != nil {
		return nil, fmt.Errorf("parsing API tokens: %w", err)
	}
	cfg.APIKeys = keys

	// Validate transport
	switch cfg.Transport {
	case TransportDOM, TransportWebAPI, TransportAuto:
	default:
		return nil, fmt.Errorf("unsupported transport %q, supported: %s, %s, %s",
			cfg.Transport, TransportDOM, TransportWebAPI, TransportAuto)
	}
	switch cfg.TransportFallback {
	case "", TransportDOM:
	default:
		return nil, fmt.Errorf("unsupported transport fallback %q, supported: %q (or empty)",
			cfg.TransportFallback, TransportDOM)
	}

	// Validate provider
	valid := false
	for _, p := range SupportedProviders {
		if cfg.Provider == p {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("unsupported provider %q, supported: %s",
			cfg.Provider, strings.Join(SupportedProviders, ", "))
	}

	return cfg, nil
}

// ProviderURL returns the base URL for the configured provider.
func (c *Config) ProviderURL() string {
	switch c.Provider {
	case ProviderClaude:
		return c.ClaudeURL
	case ProviderQwen:
		return c.QwenURL
	case ProviderDeepSeek:
		return c.DeepSeekURL
	case ProviderKimi:
		return c.KimiURL
	default:
		return c.ChatGPTURL
	}
}

// ProviderURLs returns map of provider->URL for pooled mode.
func (c *Config) ProviderURLs() map[string]string {
	return map[string]string{
		ProviderChatGPT:  c.ChatGPTURL,
		ProviderClaude:   c.ClaudeURL,
		ProviderQwen:     c.QwenURL,
		ProviderDeepSeek: c.DeepSeekURL,
		ProviderKimi:     c.KimiURL,
	}
}

// IsPooled returns true if Provider is "all" (single Chromium with page per provider).
func (c *Config) IsPooled() bool { return c.Provider == ProviderAll }

// QwenSessionPath returns the storage-state file for the qwen web transport.
func (c *Config) QwenSessionPath() string {
	return filepath.Join(c.AuthDir, "qwen.json")
}

// UseWebAPI reports whether the qwen web transport must be used. In auto mode
// it is preferred when an auth session file has been exported; otherwise the
// gateway falls back to the browser. Non-qwen providers always use the DOM.
func (c *Config) UseWebAPI() bool {
	if c.Provider != ProviderQwen {
		return false
	}
	switch c.Transport {
	case TransportWebAPI:
		return true
	case TransportAuto:
		_, err := os.Stat(c.QwenSessionPath())
		return err == nil
	default:
		return false
	}
}

// PooledProviders returns the list of providers to launch in pooled mode.
// For now, only the 3 verified providers (chatgpt,qwen,deepseek) to avoid 5× login wait.
// Extend to include claude/kimi once their selectors are verified live.
func (c *Config) PooledProviders() []string {
	if c.IsPooled() {
		return []string{ProviderChatGPT, ProviderQwen, ProviderDeepSeek}
	}
	return []string{c.Provider}
}

func (c *Config) MaxConcurrentPerProvider() int {
	if c.MaxConcurrentPerProviderVal <= 0 {
		return 3
	}
	return c.MaxConcurrentPerProviderVal
}

// EnsureDirs creates required directories if they don't exist.
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.BrowserDataDir, c.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// Auth sessions are password-equivalent: keep them owner-only.
	if err := os.MkdirAll(c.AuthDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", c.AuthDir, err)
	}
	return nil
}

// BrowserDataPath returns the provider-specific browser data directory.
func (c *Config) BrowserDataPath() string {
	return filepath.Join(c.BrowserDataDir, c.Provider)
}

// PoolBrowserDataPath returns the shared pool directory for single-Chromium mode.
func (c *Config) PoolBrowserDataPath() string {
	return filepath.Join(c.BrowserDataDir, "pool")
}

// ── helpers ──────────────────────────────────────────────────

func getEnvStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
			return b
		}
	}
	return fallback
}
