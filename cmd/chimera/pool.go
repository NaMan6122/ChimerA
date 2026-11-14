package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/chimera/chimera/internal/telemetry"
	"github.com/go-rod/rod"
)

// pooledSet holds the active multi-provider routing table and (when DOM mode
// or DOM fallback was requested) the shared Chromium pool.
type pooledSet struct {
	providers map[string]providers.Provider
	mutexes   map[string]*sync.Mutex
	pool      *browser.Pool // non-nil only when Chromium was launched
	browser   *rod.Browser  // pool.Browser() when pool != nil, else nil
}

// initPooledProviders builds the multi-provider set for PROVIDER=all across
// both the HTTP server and `chimera --mcp`:
//
//   - TRANSPORT=webapi: pure browserless (~25MB RSS, zero Chromium) unless
//     TRANSPORT_FALLBACK=dom is also set. Fails fast if 0 sessions succeed.
//   - TRANSPORT=auto: prefers pure browserless whenever >=1 exported session
//     initializes cleanly; falls back to browser.Pool only when 0 sessions exist
//     (or when TRANSPORT_FALLBACK=dom requests a warm fallback).
//   - TRANSPORT=dom: launches browser.Pool directly.
func initPooledProviders(cfg *config.Config) (*pooledSet, error) {
	ps := &pooledSet{
		providers: make(map[string]providers.Provider),
		mutexes:   make(map[string]*sync.Mutex),
	}

	// Step 1: Discover and initialize browserless webapi sessions.
	if cfg.Transport != config.TransportDOM {
		for _, name := range webapi.Registered() {
			sessPath := cfg.SessionPath(name)
			if _, err := os.Stat(sessPath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					log_.Infof("pooled webapi: skipping %s (no session file at %s)", name, sessPath)
					continue
				}
				log_.Warnf("pooled webapi: skipping %s (cannot stat %s: %v)", name, sessPath, err)
				continue
			}

			ctor, ok := webapi.Lookup(name)
			if !ok {
				continue
			}

			// Isolate per-provider config so SessionPath/ProviderURL never see "all".
			childCfg := *cfg
			childCfg.Provider = name

			web := ctor(&childCfg)
			if err := web.Init(nil, &childCfg); err != nil {
				log_.Warnf("pooled webapi: skipping %s (session unusable: %v)", name, err)
				telemetry.RecordLoginCheck(name, false, err.Error())
				continue
			}

			ps.providers[name] = web
			ps.mutexes[name] = &sync.Mutex{}
			logSessionExpiry(web)
			telemetry.RecordLoginCheck(name, true, "")
			log_.Infof("Pooled provider %q ready (model=%s, transport=webapi)", name, web.ModelID())
		}
	}

	// Step 2: Launch Chromium pool only when explicitly requested or when auto
	// mode found zero usable browserless sessions.
	wantDOMPool := cfg.Transport == config.TransportDOM ||
		cfg.TransportFallback == config.TransportDOM ||
		(cfg.Transport == config.TransportAuto && len(ps.providers) == 0)

	if wantDOMPool {
		pool := browser.NewPool(cfg)
		domTargets := cfg.PooledProviders()
		log_.Infof("Chimera pooled DOM mode: launching single Chromium for %v", domTargets)
		err := pool.Launch(domTargets, func(name string, page *rod.Page, c *config.Config) interface{} {
			return createProviderByName(name, page, c)
		})
		if err != nil {
			if len(ps.providers) > 0 && cfg.TransportFallback == config.TransportDOM {
				log_.Warnf("fallback DOM pool unavailable (%v); continuing with browserless webapi pool only", err)
			} else {
				return nil, fmt.Errorf("launching browser pool: %w", err)
			}
		} else {
			ps.pool = pool
			ps.browser = pool.Browser()
			for _, name := range pool.ProviderNames() {
				provI, ok := pool.ProviderFor(name)
				if !ok {
					continue
				}
				domProv, ok := provI.(providers.Provider)
				if !ok {
					continue
				}
				domMu := pool.MutexFor(name)
				if domMu == nil {
					domMu = &sync.Mutex{}
				}

				if webProv, hasWeb := ps.providers[name]; hasWeb && cfg.TransportFallback == config.TransportDOM {
					n := name
					fb := providers.NewFallback(webProv, domProv, webapi.Retryable, webapi.Reason)
					fb.FromTransport, fb.ToTransport = "webapi", "dom"
					fb.OnFallback = func(from, to, reason string) {
						telemetry.ObserveTransportFallback(n, from, to, reason)
						log_.Warnf("transport fallback (%s) %s -> %s (%s)", n, from, to, reason)
					}
					ps.providers[name] = fb
					ps.mutexes[name] = domMu
				} else if !hasWeb {
					ps.providers[name] = domProv
					ps.mutexes[name] = domMu
				}
			}
		}
	}

	if len(ps.providers) == 0 {
		return nil, fmt.Errorf(
			"no usable browserless sessions found in %s (registered: %s)\n  Log in to one or more providers first:  chimera auth login <provider>",
			cfg.AuthDir, strings.Join(webapi.Registered(), ", "),
		)
	}

	return ps, nil
}
