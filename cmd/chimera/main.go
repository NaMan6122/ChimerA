// Chimera — LLM Gateway Provider
//
// Exposes browser-based AI chat applications (ChatGPT, Claude, Qwen, DeepSeek)
// as a standard OpenAI-compatible API. No API keys needed — just your browser login.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chimera/chimera/internal/api"
	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/mcp"
	"github.com/chimera/chimera/internal/meter"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/chatgpt"
	"github.com/chimera/chimera/internal/providers/claude"
	"github.com/chimera/chimera/internal/providers/deepseek"
	"github.com/chimera/chimera/internal/providers/kimi"
	"github.com/chimera/chimera/internal/providers/qwen"
	"github.com/chimera/chimera/internal/providers/webapi"
	_ "github.com/chimera/chimera/internal/providers/webapi/all"
	"github.com/chimera/chimera/internal/session"
	"github.com/chimera/chimera/internal/telemetry"
	"github.com/go-rod/rod"
)

var log_ = logging.New("main", "./logs", "debug", true)

// version is stamped at install time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// `chimera auth ...` manages storage-state sessions; it is not the server.
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		runAuth(os.Args[2:])
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		runSessions(os.Args[2:])
		return
	}

	// `chimera doctor ...` checks the local install; it is not the server.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		runDoctor(os.Args[2:])
		return
	}

	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Printf("chimera %s\n", version)
		return
	}

	// `chimera up [provider|all]` starts backends without the env-var dance.
	// A `chimera-<provider>` symlink (chimera-kimi, chimera-gpt, …) is the same
	// thing in one word. When runUp returns true the environment is prepared and
	// this process becomes the gateway — but "up" is still sitting in os.Args,
	// so the unknown-command check below must be told it was consumed.
	upHandled := false
	if prov := providerFromArgv0(os.Args[0]); prov != "" {
		if !runUp([]string{prov}) {
			return
		}
		upHandled = true
	} else if len(os.Args) > 1 && os.Args[1] == "up" {
		if !runUp(os.Args[2:]) {
			return
		}
		upHandled = true
	}

	// The gateway takes no other subcommand. Saying so beats starting a server
	// under a name that looks like one ("chimera spin" is the most likely typo:
	// the harness is a separate binary).
	if !upHandled && len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "  gateway:  chimera                       (no arguments; serves :8000)")
		fmt.Fprintln(os.Stderr, "  backends: chimera up [provider|all]     (first free port in 8000-8011)")
		fmt.Fprintln(os.Stderr, "            chimera-kimi, chimera-gpt, …  (same thing, one word)")
		fmt.Fprintln(os.Stderr, "  auth:     chimera auth login|status|check")
		fmt.Fprintln(os.Stderr, "  sessions: chimera sessions list|show|export")
		fmt.Fprintln(os.Stderr, "  doctor:   chimera doctor [--probe]            (local install health check)")
		fmt.Fprintln(os.Stderr, "  harness:  chimera-spin \"<task>\"          (separate binary, needs a running gateway)")
		os.Exit(2)
	}

	// MCP stdio mode (chimera --mcp, internal spec 001): stdout carries ONLY
	// JSON-RPC bytes, so the banner and the HTTP server are skipped entirely.
	mcpMode := false
	for _, a := range os.Args[1:] {
		if a == "-mcp" || a == "--mcp" {
			mcpMode = true
			break
		}
	}

	// Banner (suppressed in MCP mode — stdout is the protocol stream)
	if !mcpMode {
		fmt.Println(`
   _____ _                   _   _       
  / ____| |                 | | (_)      
 | (___ | |_ __ _ _ __   __| |  _  ___  
  \___ \| __/ _` + "`" + ` | '_ \ / _` + "`" + ` | |/ |/ _ \ 
  ____) | || (_| | | | | (_| | | |  __/ 
 |_____/ \__\__,_|_| |_|\__,_|_|_|\___| 
                                         
  LLM Gateway — Browser-based AI API
  `)
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log_.Infof("Chimera starting (provider=%s, transport=%s, port=%d)", cfg.Provider, cfg.Transport, cfg.APIPort)
	if !cfg.IsPooled() {
		log_.Infof("Provider URL: %s", cfg.ProviderURL())
	}

	// MCP stdio mode: no HTTP server, no port. Providers init as usual, then
	// Serve until stdin EOF or SIGINT (spec 001).
	if mcpMode {
		runMCP(cfg)
		return
	}

	// Bind before any provider work. Otherwise a second instance validates a
	// session (a network round trip) and only then discovers the port is taken,
	// which reads as though the session caused the failure.
	addr := fmt.Sprintf("%s:%d", cfg.APIHost, cfg.APIPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n  Another Chimera is probably running: pgrep -fl 'chimera$'", addr, err)
	}
	defer ln.Close()

	// ── Browser launch: single vs pooled (single Chromium with page-per-provider) ──
	var (
		browserMgr *browser.Manager
		pool       *browser.Pool
		server     *api.Server
	)

	if cfg.IsPooled() {
		ps, err := initPooledProviders(cfg)
		if err != nil {
			log.Fatalf("Failed to initialize pooled providers: %v", err)
		}
		pool = ps.pool
		server = api.NewPooledServer(cfg, ps.providers, ps.mutexes, ps.browser)
		log_.Infof("Pool ready: %v", sortedNames(ps.providers))
	} else {
		if cfg.UseWebAPI() {
			// Browserless web-API transport (specs 011/012). In auto mode a broken
			// session falls back to the DOM instead of taking the gateway down.
			ctor, ok := webapi.Lookup(cfg.Provider)
			if !ok {
				log_.Errorf("provider %q has no browserless transport (registered: %v)", cfg.Provider, webapi.Registered())
				if cfg.Transport == config.TransportWebAPI {
					os.Exit(1)
				}
				telemetry.ObserveTransportFallback(cfg.Provider, "webapi", "dom", "init_failed")
			} else {
				log_.Infof("Chimera webapi transport (no browser): provider=%s session=%s",
					cfg.Provider, cfg.SessionPath(cfg.Provider))
				web := ctor(cfg)
				err := web.Init(nil, cfg)
				switch {
				case err == nil:
					if cfg.TransportFallback == config.TransportDOM {
						dom, mgr, domErr := launchDOMProvider(cfg)
						if domErr != nil {
							log_.Warnf("fallback DOM transport unavailable (%v); running webapi only", domErr)
							server = api.NewServer(cfg, web)
						} else {
							browserMgr = mgr
							fb := providers.NewFallback(web, dom, webapi.Retryable, webapi.Reason)
							fb.FromTransport, fb.ToTransport = "webapi", "dom"
							fb.OnFallback = func(from, to, reason string) {
								telemetry.ObserveTransportFallback(cfg.Provider, from, to, reason)
								log_.Warnf("transport fallback %s -> %s (%s)", from, to, reason)
							}
							server = api.NewServer(cfg, fb)
						}
					} else {
						server = api.NewServer(cfg, web)
					}
					if server != nil {
						logSessionExpiry(web)
						log_.Infof("Provider %q ready (model=%s, transport=webapi)", web.Name(), web.ModelID())
					}
				case cfg.Transport == config.TransportWebAPI:
					log_.Errorf("webapi transport unavailable: %v", err)
					log_.Errorf("Export a session with: node scripts/webapi-spike/cdp-capture.mjs %s", cfg.Provider)
					os.Exit(1)
				default:
					telemetry.ObserveTransportFallback(cfg.Provider, "webapi", "dom", "init_failed")
					log_.Warnf("webapi transport unavailable (%v); falling back to DOM", err)
				}
			}
		}
		if server == nil {
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
	}

	server.SetVersion(version)

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

	if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
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

// launchDOMProvider launches Chromium and initializes the DOM provider. Used
// for the warm webapi->dom fallback (spec 011 §4).
func launchDOMProvider(cfg *config.Config) (providers.Provider, *browser.Manager, error) {
	mgr := browser.NewManager(cfg)
	page, err := mgr.Launch()
	if err != nil {
		return nil, mgr, err
	}
	provider := createProvider(cfg, page)
	if err := provider.Init(page, cfg); err != nil {
		_ = mgr.Close()
		return nil, nil, fmt.Errorf("DOM provider init: %w", err)
	}
	return provider, mgr, nil
}

// logSessionExpiry warns when a browserless session is close to expiry. It is
// best-effort: only transports that expose SessionExpiry report anything.
func logSessionExpiry(p providers.Provider) {
	r, ok := p.(interface{ SessionExpiry() (time.Time, bool) })
	if !ok {
		return
	}
	exp, has := r.SessionExpiry()
	if !has {
		log_.Warnf("%s web session token has no exp claim; expiry unknown", p.Name())
		return
	}
	days := time.Until(exp).Hours() / 24
	if days < 7 {
		log_.Warnf("%s web session expires in %.1f days — run `chimera auth login` to refresh", p.Name(), days)
	} else {
		log_.Infof("%s web session valid for %.0f more days", p.Name(), days)
	}
}

// runMCP serves the Model Context Protocol over stdio (chimera --mcp, spec
// 001). Provider construction mirrors main(): pooled (PROVIDER=all) with the
// pool's per-provider mutexes, webapi single, or DOM single. There is no HTTP
// server and no port bind; mcp.Serve blocks until stdin EOF or SIGINT, and
// shutdown reuses the same close path (browser/pool) as the HTTP server.
func runMCP(cfg *config.Config) {
	var (
		provs      = map[string]providers.Provider{}
		mus        = map[string]*sync.Mutex{}
		browserMgr *browser.Manager
		pool       *browser.Pool
		br         *rod.Browser // the browser behind session.Manager (nil when browserless-only)
	)

	if cfg.IsPooled() {
		ps, err := initPooledProviders(cfg)
		if err != nil {
			log.Fatalf("Failed to initialize pooled providers: %v", err)
		}
		provs = ps.providers
		mus = ps.mutexes
		pool = ps.pool
		br = ps.browser
	} else {
		if cfg.UseWebAPI() {
			ctor, ok := webapi.Lookup(cfg.Provider)
			if !ok {
				log_.Errorf("provider %q has no browserless transport (registered: %v)", cfg.Provider, webapi.Registered())
				if cfg.Transport == config.TransportWebAPI {
					os.Exit(1)
				}
				telemetry.ObserveTransportFallback(cfg.Provider, "webapi", "dom", "init_failed")
			} else {
				log_.Infof("Chimera MCP webapi transport (no browser): provider=%s session=%s",
					cfg.Provider, cfg.SessionPath(cfg.Provider))
				web := ctor(cfg)
				err := web.Init(nil, cfg)
				switch {
				case err == nil:
					provs[web.Name()] = web
					mus[web.Name()] = &sync.Mutex{}
					logSessionExpiry(web)
					log_.Infof("Provider %q ready (model=%s, transport=webapi)", web.Name(), web.ModelID())
				case cfg.Transport == config.TransportWebAPI:
					log_.Errorf("webapi transport unavailable: %v", err)
					log_.Errorf("Export a session with: node scripts/webapi-spike/cdp-capture.mjs %s", cfg.Provider)
					os.Exit(1)
				default:
					telemetry.ObserveTransportFallback(cfg.Provider, "webapi", "dom", "init_failed")
					log_.Warnf("webapi transport unavailable (%v); falling back to DOM", err)
				}
			}
		}
		if len(provs) == 0 {
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
			provs[provider.Name()] = provider
			mus[provider.Name()] = &sync.Mutex{}
		}
	}

	// Metering: same store the HTTP path uses; rows are keyed by the MCP
	// client name from initialize (tenant "mcp:<client>").
	var mtr *meter.Store
	if strings.TrimSpace(cfg.MeterDB) != "" {
		if st, err := meter.Open(cfg.MeterDB); err != nil {
			log_.Errorf("Metering disabled, cannot open %q: %v", cfg.MeterDB, err)
		} else {
			mtr = st
		}
	}

	// SIGINT/SIGTERM: reuse the shutdown path — close browser/pool.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log_.Info("Shutting down...")
		closeBrowsers(pool, browserMgr)
		os.Exit(0)
	}()

	deps := &mcp.Deps{
		Cfg:        cfg,
		Providers:  provs,
		Mus:        mus,
		SessionMgr: session.New(br, 10),
		Meter:      mtr,
		Log:        log_,
		Version:    version,
	}
	// One login-state cache seed at startup, so the `providers` tool can
	// answer from cache without ever touching a browser mid-call.
	deps.SeedLoginStates()

	log_.Infof("MCP stdio server ready (providers: %v)", sortedNames(provs))
	log_.Info("Serving JSON-RPC on stdin/stdout until EOF")

	if err := mcp.Serve(os.Stdin, os.Stdout, deps); err != nil {
		log_.Errorf("MCP server error: %v", err)
	}

	log_.Info("Chimera stopped")
	closeBrowsers(pool, browserMgr)
	if mtr != nil {
		_ = mtr.Close()
	}
}

// closeBrowsers is the shared shutdown step for both serve modes.
func closeBrowsers(pool *browser.Pool, mgr *browser.Manager) {
	if pool != nil {
		_ = pool.Close()
	} else if mgr != nil {
		_ = mgr.Close()
	}
}

func sortedNames(provs map[string]providers.Provider) []string {
	names := make([]string, 0, len(provs))
	for n := range provs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// webapi.Retryable / webapi.Reason (internal/providers/webapi) classify webapi
// failures for the retry-and-fallback path; every transport shares one
// vocabulary (spec 012).
