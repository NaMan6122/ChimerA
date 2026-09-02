// Package browser - PagePool for single Chromium multi-provider support.
package browser

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
)

// Pool manages a single Chromium instance with one Page per provider.
// Each provider gets its own tab (rod.Page) + mutex for serialized DOM access per provider.
// Cross-provider requests run concurrently (different mutexes), same-provider serializes.
type Pool struct {
	cfg       *config.Config
	browser   *rod.Browser
	pages     map[string]*rod.Page
	providers map[string]interface{}
	mus       map[string]*sync.Mutex
	log       *logging.Logger
	controlURL string
}

var poolLog = logging.New("pool", "./logs", "debug", true)

// NewPool creates a Pool for the given config.
func NewPool(cfg *config.Config) *Pool {
	return &Pool{
		cfg:       cfg,
		pages:     make(map[string]*rod.Page),
		providers: make(map[string]interface{}),
		mus:       make(map[string]*sync.Mutex),
		log:       poolLog,
	}
}

// Launch starts a single Chromium with UserDataDir=browser_data/pool and creates a Page per provider.
// providersToLaunch is list of provider names, e.g., ["chatgpt","qwen","deepseek","claude"].
// For each, it navigates to provider URL, applies stealth, and waits for login (up to 5m per provider).
func (p *Pool) Launch(providersToLaunch []string, providerFactory func(name string, page *rod.Page, cfg *config.Config) interface{}) error {
	poolLog.Infof("Pool launching single Chromium for %v (headless=%v)", providersToLaunch, p.cfg.Headless)

	poolDir := filepath.Join(p.cfg.BrowserDataDir, "pool")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		return fmt.Errorf("creating pool dir: %w", err)
	}
	// Clean stale locks + WAL/journal + network cache (hardened, ref manager.go)
	for _, n := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(poolDir, n))
		_ = os.Remove(filepath.Join(poolDir, "Default", n))
	}
	removed := 0
	_ = filepath.WalkDir(poolDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, pat := range []string{"*-journal", "*-wal", "*-shm"} {
			if m, _ := filepath.Match(pat, filepath.Base(path)); m {
				_ = os.Remove(path)
				removed++
				break
			}
		}
		return nil
	})
	if removed > 0 {
		poolLog.Infof("Pool removed %d stale WAL/journal files", removed)
	}
	for _, rel := range []string{"Default/Network Persistent State", "Default/Network Action Predictor", "Default/TransportSecurity"} {
		p := filepath.Join(poolDir, rel)
		if _, err := os.Stat(p); err == nil {
			_ = os.Remove(p)
		}
	}
	for _, rel := range []string{"Default/Cache", "Default/Code Cache", "Default/GPUCache", "GrShaderCache", "GraphiteDawnCache"} {
		p := filepath.Join(poolDir, rel)
		_ = os.RemoveAll(p)
	}

	args := launcher.New().
		UserDataDir(poolDir).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-features", "IsolateOrigins,site-per-process").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-infobars").
		Set("disable-extensions").
		Set("disable-dev-shm-usage").
		Set("no-sandbox").
		Set("disable-gpu").
		Set("disable-features", "AsyncDns,DnsOverHttps").
		Set("dns-prefetch-disable")
	if data, err := os.ReadFile("/tmp/host_resolver_rules"); err == nil && len(data) > 0 {
		args = args.Set("host-resolver-rules", string(data))
		poolLog.Infof("Pool applied host-resolver-rules: %d chars", len(data))
	}

	if p.cfg.Headless {
		args = args.Headless(true)
	} else {
		args = args.Headless(false).Delete("no-startup-window")
	}

	vw := p.cfg.ViewportWidth + rand.Intn(41) - 20
	vh := p.cfg.ViewportHeight + rand.Intn(41) - 20

	controlURL, err := args.Launch()
	if err != nil {
		return fmt.Errorf("launching pool browser: %w", err)
	}
	p.controlURL = controlURL

	browser := rod.New().ControlURL(controlURL)
	if p.cfg.SlowMo > 0 {
		browser = browser.SlowMotion(p.cfg.SlowMo)
	}
	if err := browser.Connect(); err != nil {
		return fmt.Errorf("connecting to pool browser: %w", err)
	}
	p.browser = browser

	// Create a Page per provider sequentially (to avoid race on first navigation)
	for _, name := range providersToLaunch {
		poolLog.Infof("Pool creating page for %s", name)
		page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if err != nil {
			return fmt.Errorf("creating page for %s: %w", name, err)
		}
		_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: vw + rand.Intn(10) - 5, Height: vh + rand.Intn(10) - 5})
		_, _ = page.Eval(`() => { Object.defineProperty(navigator, 'webdriver', { get: () => false }); }`)
		applyStealth(page)

		// Navigate to provider URL
		url := providerURLFor(name, p.cfg)
		poolLog.Infof("Pool navigating %s -> %s", name, url)
		err = rod.Try(func() {
			page.Timeout(30 * time.Second).MustNavigate(url)
		})
		if err != nil {
			return fmt.Errorf("navigating %s to %s: %w", name, url, err)
		}
		_ = page.WaitLoad()
		time.Sleep(2 * time.Second)
		applyStealth(page)

		// Create provider instance via factory
		prov := providerFactory(name, page, p.cfg)
		if prov == nil {
			poolLog.Warnf("Pool %s factory returned nil, skipping", name)
			continue
		}
		// Try Init, if not logged in wait for manual login (same as single mode)
		if initer, ok := prov.(interface{ Init(*rod.Page, *config.Config) error }); ok {
			if err := initer.Init(page, p.cfg); err != nil {
				poolLog.Infof("Pool %s not logged in: %v — waiting for login (5m)", name, err)
				poolLog.Infof("PLEASE LOG IN to %s in its Chromium tab (look for tab titled %s)", name, url)
				if err := waitForLogin(prov, 5*time.Minute); err != nil {
					poolLog.Warnf("Pool %s login timeout, skipping (pool continues): %v", name, err)
					continue
				}
				poolLog.Infof("Pool %s login detected", name)
			} else {
				poolLog.Infof("Pool %s ready", name)
			}
		}

		p.pages[name] = page
		p.providers[name] = prov
		p.mus[name] = &sync.Mutex{}
	}

	poolLog.Infof("Pool ready with %d providers", len(p.pages))
	return nil
}

func providerURLFor(name string, cfg *config.Config) string {
	switch name {
	case config.ProviderClaude:
		return cfg.ClaudeURL
	case config.ProviderQwen:
		return cfg.QwenURL
	case config.ProviderDeepSeek:
		return cfg.DeepSeekURL
	case config.ProviderKimi:
		return cfg.KimiURL
	default:
		return cfg.ChatGPTURL
	}
}

func waitForLogin(prov interface{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	checker, ok := prov.(interface{ IsLoggedIn() (bool, error) })
	if !ok {
		return fmt.Errorf("provider has no IsLoggedIn")
	}
	for time.Now().Before(deadline) {
		<-ticker.C
		ok, _ := checker.IsLoggedIn()
		if ok {
			return nil
		}
		poolLog.Info("Pool still waiting for login...")
	}
	return fmt.Errorf("timeout")
}

// Browser returns the underlying rod.Browser.
func (p *Pool) Browser() *rod.Browser { return p.browser }

// PageFor returns the Page for a provider.
func (p *Pool) PageFor(name string) (*rod.Page, bool) {
	page, ok := p.pages[name]
	return page, ok
}

// ProviderFor returns the provider instance for a name.
func (p *Pool) ProviderFor(name string) (interface{}, bool) {
	prov, ok := p.providers[name]
	return prov, ok
}

// MutexFor returns the per-provider mutex.
func (p *Pool) MutexFor(name string) *sync.Mutex {
	if mu, ok := p.mus[name]; ok {
		return mu
	}
	return nil
}

// Providers returns all provider names.
func (p *Pool) ProviderNames() []string {
	names := make([]string, 0, len(p.providers))
	for k := range p.providers {
		names = append(names, k)
	}
	return names
}

// Close shuts down the single browser.
func (p *Pool) Close() error {
	poolLog.Infof("Pool closing single Chromium...")
	if p.browser != nil {
		if err := p.browser.Close(); err != nil {
			poolLog.Errorf("Pool close error: %v", err)
			return err
		}
	}
	p.browser = nil
	p.pages = nil
	poolLog.Infof("Pool closed")
	return nil
}
