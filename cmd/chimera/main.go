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

	// Launch browser
	browserMgr := browser.NewManager(cfg)
	page, err := browserMgr.Launch()
	if err != nil {
		log.Fatalf("Failed to launch browser: %v", err)
	}

	// Create provider client
	provider := createProvider(cfg, page)

	// Initialize provider (verify login, navigate)
	if err := provider.Init(page, cfg); err != nil {
		log_.Infof("Initial provider check failed: %v", err)
		log_.Info("Browser window is OPEN — please log in manually in the Chromium window.")
		log_.Info("  • Use email/password, Microsoft, Apple, or magic link")
		log_.Info("  • DO NOT use Google OAuth (blocked in automation)")
		log_.Info("  • Waiting up to 5 minutes for login to complete...")
		// Poll for login instead of exiting immediately (fixes headful window closing too fast)
		waitUntilLoggedIn(provider, 5*time.Minute)
		log_.Infof("Login detected! Provider %q ready (model=%s)", provider.Name(), provider.ModelID())
	} else {
		log_.Infof("Provider %q ready (model=%s)", provider.Name(), provider.ModelID())
	}

	// Create and start API server
	server := api.NewServer(cfg, provider)

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
		_ = browserMgr.Close()
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

// createProvider instantiates the correct provider based on config.
func createProvider(cfg *config.Config, page *rod.Page) providers.Provider {
	switch cfg.Provider {
	case config.ProviderClaude:
		return claude.NewClient(page, cfg)
	case config.ProviderQwen:
		return qwen.NewClient(page, cfg)
	case config.ProviderDeepSeek:
		return deepseek.NewClient(page, cfg)
	default:
		return chatgpt.NewClient(page, cfg)
	}
}
