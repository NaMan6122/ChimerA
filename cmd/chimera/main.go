// Chimera — LLM Gateway Provider
//
// Exposes browser-based AI chat applications (ChatGPT, Claude, Qwen, DeepSeek)
// as a standard OpenAI-compatible API. No API keys needed — just your browser login.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/chimera/chimera/internal/api"
	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/chatgpt"
	"github.com/chimera/chimera/internal/providers/claude"
	"github.com/chimera/chimera/internal/providers/deepseek"
	"github.com/chimera/chimera/internal/providers/kimi"
	"github.com/chimera/chimera/internal/providers/qwen"
)

var log_ = logging.New("main", "./logs", "debug", true)

func main() {
	// Banner
	fmt.Println(`
   _____ _                   _   _       
  / ____| |                 | | (_)      
 | (___ | |_ __ _ _ __   __| |  _  ___  
  \___ \| __/ _`+"`"+` | '_ \ / _`+"`"+` | |/ |/ _ \ 
  ____) | || (_| | | | | (_| | | |  __/ 
 |_____/ \__\__,_|_| |_|\__,_|_|_|\___| 
                                         
  LLM Gateway — Browser-based AI API
  `)

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log_.Infof("Chimera starting (provider=%s, port=%d)", cfg.Provider, cfg.APIPort)
	log_.Infof("Provider URL: %s", cfg.ProviderURL())

	// ── Browser launch: single vs pooled (single Chromium with page-per-provider) ──
	var (
		browserMgr *browser.Manager
		pool       *browser.Pool
		server     *api.Server
	)

	if cfg.IsPooled() {
		// Single Chromium, one Page per provider — ~400MB total not 3×400MB
		pool = browser.NewPool(cfg)
		providersToLaunch := cfg.PooledProviders()
		log_.Infof("Chimera pooled mode: launching single Chromium for %v", providersToLaunch)
		if err := pool.Launch(providersToLaunch, func(name string, page *rod.Page, cfg *config.Config) interface{} {
			return createProviderByName(name, page, cfg)
		}); err != nil {
			log.Fatalf("Failed to launch pool: %v", err)
		}
		// Build provider map for API routing
		poolProviders := make(map[string]providers.Provider)
		mutexMap := make(map[string]*sync.Mutex)
		for _, name := range pool.ProviderNames() {
			if provI, ok := pool.ProviderFor(name); ok {
				if prov, ok := provI.(providers.Provider); ok {
					poolProviders[name] = prov
				}
			}
			if mu := pool.MutexFor(name); mu != nil {
				mutexMap[name] = mu
			}
		}
		server = api.NewPooledServer(cfg, poolProviders, mutexMap, pool.Browser())
		log_.Infof("Pool ready: %v", pool.ProviderNames())
	} else {
		browserMgr = browser.NewManager(cfg)
		page, err := browserMgr.Launch()
		if err != nil {
			log.Fatalf("Failed to launch browser: %v", err)
		}
		provider := createProvider(cfg, page)
		if err := provider.Init(page, cfg); err != nil {
			log_.Infof("Initial provider check failed: %v", err)
			log_.Info("Browser window is OPEN — please log in manually in the Chromium window.")
			log_.Info("  • Use email/password, Microsoft, Apple, or magic link")
			log_.Info("  • DO NOT use Google OAuth (blocked in automation)")
			log_.Info("  • Waiting up to 5 minutes for login to complete...")
			waitUntilLoggedIn(provider, 5*time.Minute)
			log_.Infof("Login detected! Provider %q ready (model=%s)", provider.Name(), provider.ModelID())
		} else {
			log_.Infof("Provider %q ready (model=%s)", provider.Name(), provider.ModelID())
		}
		server = api.NewServer(cfg, provider)
	}

	addr := fmt.Sprintf("%s:%d", cfg.APIHost, cfg.APIPort)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      server.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // No timeout for streaming
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log_.Info("Shutting down...")
		cancel()
		if pool != nil {
			_ = pool.Close()
		} else if browserMgr != nil {
			_ = browserMgr.Close()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log_.Infof("API server listening on %s", addr)
	log_.Infof("OpenAI-compatible endpoint: http://localhost:%d/v1", cfg.APIPort)
	log_.Info("Press Ctrl+C to stop")

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log_.Info("Chimera stopped")
}

// waitUntilLoggedIn polls IsLoggedIn until success or timeout, keeping the browser open for manual login.
func waitUntilLoggedIn(provider providers.Provider, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		ok, err := provider.IsLoggedIn()
		if err == nil && ok {
			return
		}
		log_.Info("Still waiting for login... (check the Chromium window)")
	}
	log.Fatalf("Login timeout after %v — please restart after logging in. Tip: keep the browser window open and complete login within 5 minutes.", timeout)
}

// createProvider instantiates the correct provider based on config (single mode).
func createProvider(cfg *config.Config, page *rod.Page) providers.Provider {
	return createProviderByName(cfg.Provider, page, cfg)
}

// createProviderByName instantiates a provider by name (used by Pool).
func createProviderByName(name string, page *rod.Page, cfg *config.Config) providers.Provider {
	switch name {
	case config.ProviderClaude:
		return claude.NewClient(page, cfg)
	case config.ProviderQwen:
		return qwen.NewClient(page, cfg)
	case config.ProviderDeepSeek:
		return deepseek.NewClient(page, cfg)
	case config.ProviderKimi:
		return kimi.NewClient(page, cfg)
	default:
		return chatgpt.NewClient(page, cfg)
	}
}
