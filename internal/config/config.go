// Package config handles all configuration loading from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// Config holds all project settings.
type Config struct {
	// Provider
	Provider string

	// Browser
	BrowserDataDir string
	Headless       bool
	SlowMo         time.Duration

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
	TypingSpeedMin  time.Duration
	TypingSpeedMax  time.Duration
	ThinkingPauseMin time.Duration
	ThinkingPauseMax time.Duration

	// Logging
	LogDir    string
	LogLevel  string
	Verbose   bool

	// API Server
	APIHost           string
	APIPort           int
	RateLimitSeconds  int
	APIToken          string

	// VNC
	VNCPassword string

	// Viewport (base, jittered ±20px per launch)
	ViewportWidth  int
	ViewportHeight int
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

		// VNC
		VNCPassword: getEnvStr("VNC_PASSWORD", "chimera"),

		// Viewport
		ViewportWidth:  1280,
		ViewportHeight: 720,
	}

	// Ensure directories exist
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("creating directories: %w", err)
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

// PooledProviders returns the list of providers to launch in pooled mode.
// For now, only the 3 verified providers (chatgpt,qwen,deepseek) to avoid 5× login wait.
// Extend to include claude/kimi once their selectors are verified live.
func (c *Config) PooledProviders() []string {
	if c.IsPooled() {
		return []string{ProviderChatGPT, ProviderQwen, ProviderDeepSeek}
	}
	return []string{c.Provider}
}

// EnsureDirs creates required directories if they don't exist.
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.BrowserDataDir, c.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
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
