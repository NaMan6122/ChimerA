// Package browser manages the Chrome browser lifecycle using rod.
package browser

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
)

var log = logging.New("browser", "./logs", "debug", true)

// Manager handles browser launch, persistence, and cleanup.
type Manager struct {
	cfg     *config.Config
	browser *rod.Browser
	page    *rod.Page
}

// NewManager creates a new browser manager.
func NewManager(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// Launch opens a persistent browser and returns the active page.
func (m *Manager) Launch() (*rod.Page, error) {
	log.Infof("Launching browser (provider=%s, headless=%v)", m.cfg.Provider, m.cfg.Headless)

	// Clean stale lock files
	m.cleanLockFiles()

	browserDataDir := m.cfg.BrowserDataPath()
	if err := os.MkdirAll(browserDataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating browser data dir: %w", err)
	}

	// Build launch arguments with persistent profile
	args := launcher.New().
		UserDataDir(browserDataDir).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-features", "IsolateOrigins,site-per-process").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-infobars").
		Set("disable-extensions").
		Set("disable-dev-shm-usage").
		Set("no-sandbox").
		Set("disable-gpu")

	if m.cfg.Headless {
		args = args.Headless(true)
	} else {
		args = args.Headless(false).Delete("no-startup-window")
	}

	// Jittered viewport
	vw := m.cfg.ViewportWidth + rand.Intn(41) - 20 // ±20px
	vh := m.cfg.ViewportHeight + rand.Intn(41) - 20

	controlURL, err := args.Launch()
	if err != nil {
		return nil, fmt.Errorf("launching browser: %w", err)
	}

	browser := rod.New().ControlURL(controlURL)

	// Slow motion for debugging
	if m.cfg.SlowMo > 0 {
		browser = browser.SlowMotion(m.cfg.SlowMo)
	}

	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connecting to browser: %w", err)
	}

	// Create a page
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		_ = browser.Close()
		return nil, fmt.Errorf("creating page: %w", err)
	}

	// Set viewport
	_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:  vw,
		Height: vh,
	})

	// Remove webdriver flag
	_, _ = page.Eval(`() => { Object.defineProperty(navigator, 'webdriver', { get: () => false }); }`)

	// Navigate to provider URL
	providerURL := m.cfg.ProviderURL()
	log.Infof("Navigating to %s", providerURL)

	err = rod.Try(func() {
		page.Timeout(30 * time.Second).MustNavigate(providerURL)
	})
	if err != nil {
		_ = browser.Close()
		return nil, fmt.Errorf("navigating to %s: %w", providerURL, err)
	}

	// Wait for initial load
	_ = page.WaitLoad()

	// Apply stealth patches
	applyStealth(page)

	m.browser = browser
	m.page = page

	log.Infof("Browser launched successfully (viewport=%dx%d)", vw, vh)
	return page, nil
}

// Page returns the active browser page.
func (m *Manager) Page() *rod.Page {
	return m.page
}

// Browser returns the browser instance.
func (m *Manager) Browser() *rod.Browser {
	return m.browser
}

// NewPage creates a new tab in the existing browser.
func (m *Manager) NewPage() (*rod.Page, error) {
	if m.browser == nil {
		return nil, fmt.Errorf("browser not initialized")
	}
	page, err := m.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("creating new page: %w", err)
	}
	applyStealth(page)
	return page, nil
}

// Close gracefully shuts down the browser.
func (m *Manager) Close() error {
	log.Infof("Closing browser...")
	if m.browser != nil {
		if err := m.browser.Close(); err != nil {
			log.Errorf("Error closing browser: %v", err)
			return err
		}
	}
	m.browser = nil
	m.page = nil
	log.Infof("Browser closed")
	return nil
}

// cleanLockFiles removes stale Chrome singleton lock files.
func (m *Manager) cleanLockFiles() {
	patterns := []string{"SingletonLock", "SingletonSocket", "SingletonCookie"}
	for _, name := range patterns {
		lockPath := filepath.Join(m.cfg.BrowserDataPath(), name)
		if _, err := os.Stat(lockPath); err == nil {
			log.Infof("Removing stale lock file: %s", lockPath)
			_ = os.Remove(lockPath)
		}
	}
}
