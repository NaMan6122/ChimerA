package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
	_ "github.com/chimera/chimera/internal/providers/webapi/all"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const authUsage = `Usage: chimera auth <command> [flags]

Commands:
  login    Open a browser, wait for you to log in, and store the session
  verify   Open the browser so a one-time human check (captcha) can be cleared,
           then re-export the session with the cookie it mints
  all      Log in to every provider in sequence (recommended)
  check    Verify every stored session against its provider
  import   Validate and install an exported session file
  status   Show the stored session's validity and expiry

Providers: qwen, deepseek, chatgpt, gemini, grok.
  Select one with PROVIDER=<name> or pass it as the first argument:
      chimera auth login chatgpt
      chimera auth login --provider deepseek

A browser window opens; log in there; the session is saved to
AUTH_DIR/<provider>.json (mode 0600, password-equivalent).

Flags:
  login:   -timeout <dur>   how long to wait for login (default 5m)
           -hold <dur>      keep the browser open after login (traffic capture)
           -fresh           clear the browser profile first (force a real login)
  all:     -timeout <dur>   per-provider wait (default 5m)
  check:   (no flags)
  import:  -file <path>     exported session (default logs/<provider>-session.json)
           -verify <bool>   check against the provider (default true)
`

// authSpec describes how a provider's session material is harvested from the
// browser. browser.Manager.Launch() already navigates to the provider URL, so
// each expression simply runs on that origin.
type authSpec struct {
	// TokenExpr is JS evaluated in the page, returning the access token. Used by
	// providers that keep a token in web storage. Ignored when CookieName is set.
	TokenExpr string
	// CookieName detects login from an HttpOnly session cookie instead of JS.
	// document.cookie cannot see HttpOnly cookies, so a JS check for ChatGPT's
	// __Secure-next-auth.session-token always returns empty and the wait would
	// time out after a successful login. CDP's Network.getCookies sees them.
	//
	// Matched as a PREFIX, not an exact name: NextAuth chunks a session cookie
	// that exceeds ~4KB into <name>.0, <name>.1, … . ChatGPT's session is large
	// enough to be split, so the bare name never exists and an exact match would
	// never fire.
	CookieName string
	// CookiePrefix, when true, matches CookieName as a prefix (see above).
	CookiePrefix bool
	// CookieHost filters collected cookies to the provider's own domains.
	CookieHost string
	// Marker is stored as the session's AccessToken for cookie-custody providers
	// whose bearer is minted on demand.
	Marker string
}

var authSpecs = map[string]authSpec{
	config.ProviderQwen: {
		TokenExpr:  `() => localStorage.getItem('token') || ''`,
		CookieHost: "qwen",
	},
	config.ProviderDeepSeek: {
		// DeepSeek stores userToken as a versioned wrapper: the logged-out value
		// is the *object* {"value":null,"__version":"0"}, never an empty string.
		// So an object whose value/token is null means "not logged in" — falling
		// back to the raw string here would report a bogus token and end the wait
		// immediately. Only a non-JSON value is treated as a literal token.
		TokenExpr: `() => { const v = localStorage.getItem('userToken'); if (!v) return '';
			try { const o = JSON.parse(v);
				if (o && typeof o === 'object') return o.value || o.token || '';
				return v;
			} catch (e) { return v; } }`,
		CookieHost: "deepseek",
	},
	config.ProviderChatGPT: {
		// Cookie custody: the durable credential is the session cookie and the
		// bearer is minted from it by /api/auth/session, so a missing bearer at
		// login time is not a failure.
		CookieName:   "__Secure-next-auth.session-token",
		CookiePrefix: true,
		CookieHost:   "chatgpt",
		Marker:       "cookie-session",
	},
	config.ProviderGemini: {
		// Cookie custody: __Secure-1PSID is the session and __Secure-1PSIDTS
		// (host .google.com, swept by the CookieHost filter) is its rotating
		// companion; without the pair Google silently downgrades requests to
		// anonymous. SAPISID, also swept, feeds the SAPISIDHASH header.
		// Spec 013 §1. The presence of SNlM0e on /app later proves the jar.
		CookieName: "__Secure-1PSID",
		CookieHost: "google",
		Marker:     "cookie-session",
	},
	config.ProviderGrok: {
		// Cookie custody (internal spec 014): the sso JWT is the session. The
		// server-set anti-bot cookies (x-anonuserid, x-challenge, x-signature)
		// and Cloudflare's cf_clearance/__cf_bm must come from the same browser
		// session, so the whole grok.com jar travels together.
		CookieName: "sso",
		CookieHost: "grok",
		Marker:     "cookie-session",
	},
}

// runAuth handles `chimera auth <command>` and exits.
func runAuth(args []string) {
	if len(args) == 0 {
		fmt.Print(authUsage)
		os.Exit(2)
	}

	// Pull a provider selector out of the arguments before config.Load(), so
	// `chimera auth login chatgpt` works without exporting PROVIDER first. An
	// explicit PROVIDER in the environment still wins, because that is what the
	// rest of the system reads.
	provider, rest := extractProvider(args[1:])

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if provider != "" {
		cfg.Provider = provider
	}

	switch args[0] {
	case "login":
		authLogin(cfg, rest)
	case "verify":
		authVerify(cfg, rest)
	case "all":
		authLoginAll(cfg, rest)
	case "check":
		authCheckAll(cfg)
	case "import":
		authImport(cfg, rest)
	case "status":
		authStatus(cfg, rest)
	case "help", "-h", "--help":
		fmt.Print(authUsage)
	default:
		fmt.Fprintf(os.Stderr, "unknown auth command %q\n\n%s", args[0], authUsage)
		os.Exit(2)
	}
}

// extractProvider removes a provider selector from args, returning it and the
// remaining arguments. It accepts both `login chatgpt` (positional) and
// `login --provider chatgpt` (flag form).
func extractProvider(args []string) (string, []string) {
	out := make([]string, 0, len(args))
	provider := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--provider" || a == "-provider" {
			if i+1 < len(args) {
				provider = args[i+1]
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--provider="); ok {
			provider = v
			continue
		}
		// A bare positional that names a known provider is a selector, but only
		// if no flag has claimed it (so `-file chatgpt.json` is not misread).
		if provider == "" && isKnownProvider(a) && !strings.HasPrefix(a, "-") {
			provider = a
			continue
		}
		out = append(out, a)
	}
	return provider, out
}

// isKnownProvider reports whether name is a provider with a browserless
// transport (i.e. one `auth` can actually log in to).
func isKnownProvider(name string) bool {
	_, ok := authSpecs[name]
	return ok
}

// authLogin opens Chromium, waits for the user to log in, and exports the
// storage state (access token + cookies) into AUTH_DIR.
func authLogin(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for login")
	hold := fs.Duration("hold", 0, "keep the browser open this long after login so traffic can be captured")
	fresh := fs.Bool("fresh", false, "delete the browser profile first, forcing a real re-login")
	_ = fs.Parse(args)

	if *fresh {
		profile := cfg.BrowserDataPath()
		if err := os.RemoveAll(profile); err != nil {
			fmt.Fprintf(os.Stderr, "clear profile %s: %v\n", profile, err)
			os.Exit(1)
		}
		fmt.Printf("Cleared browser profile %s (forcing a fresh login).\n", profile)
	}

	spec, ok := authSpecs[cfg.Provider]
	if !ok {
		fmt.Fprintf(os.Stderr, "auth login supports %s only (got %q)\n", supportedAuthProviders(), cfg.Provider)
		os.Exit(1)
	}

	if err := browserAvailable(); err != nil {
		fmt.Fprintf(os.Stderr, "no Chromium available: %v\n", err)
		fmt.Fprintln(os.Stderr, "  A browser is required to log in — it is what earns the")
		fmt.Fprintln(os.Stderr, "  cookies the browserless transport later replays.")
		fmt.Fprintln(os.Stderr, "  Fix: install Chrome/Chromium, or run inside the Docker image.")
		os.Exit(1)
	}

	printLoginInstructions(cfg.Provider, cfg.ProviderURL(), *timeout)

	mgr := browser.NewManager(cfg)
	page, err := mgr.Launch() // navigates to cfg.ProviderURL()
	if err != nil {
		fmt.Fprintln(os.Stderr, "launch browser:", err)
		os.Exit(1)
	}
	defer mgr.Close()

	// Wait for the session. Cookie-custody providers are polled through CDP
	// (HttpOnly cookies are invisible to page JS); the rest are polled in-page.
	var marker string
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		cookies, err := page.Cookies([]string{cfg.ProviderURL()})
		if err != nil {
			continue
		}
		if spec.CookieName != "" {
			for _, c := range cookies {
				if cookieMatches(spec, c.Name) && c.Value != "" {
					marker = spec.Marker
					break
				}
			}
		} else {
			res, err := page.Eval(spec.TokenExpr)
			if err != nil {
				continue
			}
			marker = res.Value.Str()
		}
		if marker != "" {
			break
		}
	}
	if marker == "" {
		fmt.Fprintf(os.Stderr, "\nTimed out after %s — no session found.\n", *timeout)
		fmt.Fprintln(os.Stderr, "  The browser window was open; finish logging in there and re-run,")
		fmt.Fprintln(os.Stderr, "  or give it longer:  chimera auth login "+cfg.Provider+" -timeout 10m")
		if spec.CookieName != "" {
			fmt.Fprintf(os.Stderr, "  (looked for the %s* cookie)\n", spec.CookieName)
		}
		os.Exit(1)
	}

	cookies, err := page.Cookies([]string{cfg.ProviderURL()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "read cookies:", err)
		os.Exit(1)
	}
	jar := map[string]string{}
	// Session cookies live on the registrable domain (e.g. .chatgpt.com), so
	// match the provider host anywhere in the cookie domain rather than by suffix.
	for _, c := range cookies {
		if strings.Contains(c.Domain, spec.CookieHost) {
			jar[c.Name] = c.Value
		}
	}
	if len(jar) == 0 {
		fmt.Fprintf(os.Stderr, "no %s cookies found — is the page logged in?\n", cfg.Provider)
		os.Exit(1)
	}

	// marker is the provider's real token where it stores one, and a placeholder
	// for cookie-custody providers (ChatGPT, whose bearer is minted on demand).
	// Either way the cookie jar is the durable credential.
	sess := &webapi.Session{AccessToken: marker, Cookies: jar}
	dst := cfg.SessionPath(cfg.Provider)
	if err := webapi.WriteSession(sess, dst); err != nil {
		fmt.Fprintln(os.Stderr, "write session:", err)
		os.Exit(1)
	}
	st := sess.Status(dst)
	fmt.Printf("Session stored: %s (%d cookies%s)\n", dst, st.Cookies, expirySuffix(st))

	// Hold the window open so a capture can observe a real turn. This happens
	// BEFORE verification on purpose: a CDN challenge can make the probe fail
	// while the browser session is perfectly good, and closing the window then
	// would destroy the very state we just captured.
	if *hold > 0 {
		if port := devtoolsPort(cfg); port != "" {
			fmt.Printf("CDP port for capture: %s\n", port)
		}
		fmt.Printf("Holding the browser open for %s — finish logging in and send one message now.\n", *hold)
		time.Sleep(*hold)
	}
	verifySession(cfg)
}

// browserAvailable reports whether a Chromium rod can drive exists. Login needs a
// real browser: it is what earns the cookies the browserless transport replays.
// Checking first turns an opaque launch failure into an actionable message.
func browserAvailable() error {
	// An explicit override wins (used by the Docker image and by tests).
	if p := os.Getenv("CHROME_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("CHROME_BIN=%s: %w", p, err)
		}
		return nil
	}
	// rod's own lookup: a previously downloaded browser, or one on PATH.
	if path, ok := launcher.LookPath(); ok {
		_ = path
		return nil
	}
	return fmt.Errorf("no Chromium found on PATH and none cached by rod")
}

// printLoginInstructions explains what to do in the window that is about to open.
// The provider-specific notes matter: an OAuth flow that automation cannot
// complete is the most common cause of a login that never lands.
func printLoginInstructions(provider, url string, timeout time.Duration) {
	fmt.Printf("\nOpening a Chromium window at %s\n\n", url)
	fmt.Println("  Log in there. The window stays open until the session is detected.")
	fmt.Println("  Use email + password, Microsoft, Apple, or a magic link.")
	fmt.Println("  Do NOT use \"Sign in with Google\" — Google blocks automated browsers,")
	fmt.Println("  and the login will never complete.")
	switch provider {
	case config.ProviderChatGPT:
		fmt.Println("  ChatGPT keeps its credential in a session cookie; the bearer is")
		fmt.Println("  minted from it later, so no token needs to be copied by hand.")
	case config.ProviderDeepSeek:
		fmt.Println("  DeepSeek stores its token in localStorage; the window must finish")
		fmt.Println("  loading the chat UI for it to be written.")
	case config.ProviderQwen:
		fmt.Println("  Qwen stores its token in localStorage once the chat UI loads.")
	}
	fmt.Printf("\nWaiting for the session (up to %s)...\n", timeout)
}

// authVerify opens the provider's page for a one-time human verification and
// then re-exports the session carrying the cookie it minted.
//
// Qwen's risk engine answers the completions endpoint with action=captcha until
// the session holds an `x5sec` cookie, and that cookie only exists after a human
// clears the slider in the browser. This command is the honest split: the human
// does the verification, the tool carries the result. It never attempts to solve
// the challenge itself.
func authVerify(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth verify", flag.ExitOnError)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the verification cookie")
	cdpURL := fs.String("cdp", "", "attach to a browser already running with --remote-debugging-port instead of launching one")
	_ = fs.Parse(args)

	spec, ok := authSpecs[cfg.Provider]
	if !ok {
		fmt.Fprintf(os.Stderr, "auth verify supports %s only (got %q)\n", supportedAuthProviders(), cfg.Provider)
		os.Exit(1)
	}
	if err := browserAvailable(); err != nil {
		fmt.Fprintf(os.Stderr, "no Chromium available: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nOpening a Chromium window at %s\n\n", cfg.ProviderURL())
	fmt.Println("  Send any message in the chat UI. If a slider or captcha appears,")
	fmt.Println("  complete it once — this is the check that cannot be automated.")
	fmt.Println("  The window closes by itself as soon as the verification cookie is set.")

	var page *rod.Page
	if *cdpURL != "" {
		// Attach to a browser the caller launched (and can see). Needed where a
		// child process launched by the gateway never reaches the window server.
		wsURL := *cdpURL
		if !strings.HasPrefix(wsURL, "ws") {
			// Chrome exposes the debugger on a per-browser WebSocket path; the
			// HTTP root is not a control URL.
			resp, err := http.Get(strings.TrimRight(wsURL, "/") + "/json/version")
			if err != nil {
				fmt.Fprintf(os.Stderr, "probe %s: %v\n", wsURL, err)
				os.Exit(1)
			}
			var info struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if info.WebSocketDebuggerURL == "" {
				fmt.Fprintf(os.Stderr, "%s did not report a debugger URL\n", wsURL)
				os.Exit(1)
			}
			wsURL = info.WebSocketDebuggerURL
		}
		br := rod.New().ControlURL(wsURL)
		if err := br.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "attach to %s: %v\n", wsURL, err)
			os.Exit(1)
		}
		defer br.Close()
		pages, err := br.Pages()
		if err != nil || len(pages) == 0 {
			page, err = br.Page(proto.TargetCreateTarget{URL: cfg.ProviderURL()})
			if err != nil {
				fmt.Fprintln(os.Stderr, "open page:", err)
				os.Exit(1)
			}
		} else {
			page = pages[0]
			_ = page.Navigate(cfg.ProviderURL())
		}
		fmt.Printf("attached to %s\n", *cdpURL)
	} else {
		mgr := browser.NewManager(cfg)
		var err error
		page, err = mgr.Launch()
		if err != nil {
			fmt.Fprintln(os.Stderr, "launch browser:", err)
			os.Exit(1)
		}
		defer mgr.Close()
	}

	deadline := time.Now().Add(*timeout)
	jar := map[string]string{}
	verified := false
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		cookies, err := page.Cookies([]string{cfg.ProviderURL()})
		if err != nil {
			continue
		}
		jar = map[string]string{}
		for _, c := range cookies {
			if strings.Contains(c.Domain, spec.CookieHost) {
				jar[c.Name] = c.Value
			}
		}
		if jar["x5sec"] != "" {
			verified = true
			break
		}
	}
	if !verified {
		fmt.Fprintf(os.Stderr, "\nNo verification cookie after %s.\n", *timeout)
		fmt.Fprintln(os.Stderr, "  If the slider never appeared, try sending a message again;")
		fmt.Fprintln(os.Stderr, "  the challenge is issued per session, not per page load.")
		os.Exit(1)
	}

	// Keep the stored token; replace the cookie jar with what the browser holds.
	path := cfg.SessionPath(cfg.Provider)
	sess, err := webapi.LoadSession(path)
	if err != nil || sess.AccessToken == "" {
		fmt.Fprintln(os.Stderr, "no stored token to pair with the verification; run `chimera auth login` first")
		os.Exit(1)
	}
	sess.Cookies = jar
	if err := webapi.WriteSession(sess, path); err != nil {
		fmt.Fprintln(os.Stderr, "write session:", err)
		os.Exit(1)
	}
	fmt.Printf("\nVerification captured (%d cookies, including x5sec) -> %s\n", len(jar), path)

	// Prove it end to end rather than declaring victory: the models endpoint
	// passes without x5sec, only a real turn exercises the challenge.
	ctor, ok := webapi.Lookup(cfg.Provider)
	if !ok {
		fmt.Println("  provider has no browserless transport; nothing further to check")
		return
	}
	p := ctor(cfg)
	if err := p.Init(nil, cfg); err != nil {
		fmt.Printf("  session re-exported, but init failed: %v\n", err)
		os.Exit(1)
	}
	resp, err := p.SendMessage("Reply with exactly: OK", fmt.Sprintf("verify-%d", time.Now().UnixNano()))
	if err != nil {
		fmt.Printf("  verification captured, but a browserless turn still fails:\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  browserless turn succeeded: %q\n", strings.TrimSpace(resp.Message))
	fmt.Println("\nBrowserless transport is good to go.")
}

// authLoginAll logs in to every provider in sequence. It exists because the
// sessions expire on independent clocks (13-18 days observed), so refreshing them
// one at a time is easy to forget and then hard to attribute when a gateway
// starts 401ing.
func authLoginAll(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth all", flag.ExitOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "per-provider wait for login")
	_ = fs.Parse(args)

	providers := []string{config.ProviderChatGPT, config.ProviderDeepSeek, config.ProviderQwen}
	fmt.Printf("Logging in to %d providers in sequence. Each opens its own window.\n", len(providers))
	fmt.Println("Finish the login in each window before the next opens.")

	type outcome struct {
		provider string
		err      error
	}
	results := make([]outcome, 0, len(providers))
	for _, p := range providers {
		fmt.Printf("\n%s\n── %s %s\n", strings.Repeat("─", 62), strings.ToUpper(p), strings.Repeat("─", 62-len(p)))
		child := *cfg
		child.Provider = p
		if err := loginOne(&child, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", p, err)
			results = append(results, outcome{p, err})
			continue
		}
		results = append(results, outcome{p, nil})
	}

	fmt.Printf("\n%s\nSummary\n%s\n", strings.Repeat("─", 62), strings.Repeat("─", 62))
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Printf("  FAIL  %-10s %v\n", r.provider, r.err)
			continue
		}
		fmt.Printf("  ok    %-10s session stored\n", r.provider)
	}
	if failed > 0 {
		fmt.Printf("\n%d of %d providers need attention.\n", failed, len(results))
		os.Exit(1)
	}
	fmt.Println("\nAll sessions stored. Start the gateway with TRANSPORT=webapi.")
}

// loginOne runs the login flow for a single provider, returning an error instead
// of exiting so `auth all` can continue past a provider the user skipped.
func loginOne(cfg *config.Config, timeout time.Duration) error {
	spec, ok := authSpecs[cfg.Provider]
	if !ok {
		return fmt.Errorf("no login spec for provider %q", cfg.Provider)
	}
	mgr := browser.NewManager(cfg)
	page, err := mgr.Launch()
	if err != nil {
		return fmt.Errorf("launch browser: %w", err)
	}
	defer mgr.Close()

	var marker string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		cookies, err := page.Cookies([]string{cfg.ProviderURL()})
		if err != nil {
			continue
		}
		if spec.CookieName != "" {
			for _, c := range cookies {
				if cookieMatches(spec, c.Name) && c.Value != "" {
					marker = spec.Marker
					break
				}
			}
		} else {
			res, err := page.Eval(spec.TokenExpr)
			if err != nil {
				continue
			}
			marker = res.Value.Str()
		}
		if marker != "" {
			break
		}
	}
	if marker == "" {
		return fmt.Errorf("timed out after %s waiting for login", timeout)
	}

	cookies, err := page.Cookies([]string{cfg.ProviderURL()})
	if err != nil {
		return fmt.Errorf("read cookies: %w", err)
	}
	jar := map[string]string{}
	for _, c := range cookies {
		if strings.Contains(c.Domain, spec.CookieHost) {
			jar[c.Name] = c.Value
		}
	}
	if len(jar) == 0 {
		return fmt.Errorf("no %s cookies found after login", cfg.Provider)
	}
	sess := &webapi.Session{AccessToken: marker, Cookies: jar}
	dst := cfg.SessionPath(cfg.Provider)
	if err := webapi.WriteSession(sess, dst); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	fmt.Printf("  stored %s (%d cookies)\n", dst, len(jar))

	// Verify the exported session actually works. A stale browser profile makes
	// this fail even though the poll "succeeded", so report it here rather than
	// letting the gateway discover it later.
	child := *cfg
	ctor, ok := webapi.Lookup(cfg.Provider)
	if !ok {
		return nil
	}
	p := ctor(&child)
	if err := p.Init(nil, &child); err != nil {
		return fmt.Errorf("session stored but not usable: %w", err)
	}
	if ok, err := p.IsLoggedIn(); err != nil || !ok {
		return fmt.Errorf("session stored but the provider rejected it")
	}
	fmt.Printf("  verified (%s)\n", p.ModelID())
	return nil
}

// cookieMatches reports whether a cookie name satisfies the spec, honouring
// prefix matching for NextAuth's chunked session cookie (<name>.0, <name>.1, …).
func cookieMatches(spec authSpec, name string) bool {
	if !spec.CookiePrefix {
		return name == spec.CookieName
	}
	rest, ok := strings.CutPrefix(name, spec.CookieName)
	if !ok {
		return false
	}
	return rest == "" || strings.HasPrefix(rest, ".")
}

// authCheckAll verifies every stored session without opening a browser, using the
// same credential probe the assessment script uses: the STORED BEARER (or cookie
// jar) is what decides liveness, not a 200 from an unauthenticated endpoint.
func authCheckAll(cfg *config.Config) {
	providers := []string{config.ProviderChatGPT, config.ProviderDeepSeek, config.ProviderQwen}
	fmt.Printf("Checking %d stored sessions (no browser).\n\n", len(providers))
	bad := 0
	for _, p := range providers {
		child := *cfg
		child.Provider = p
		path := child.SessionPath(p)
		sess, err := webapi.LoadSession(path)
		if err != nil {
			fmt.Printf("  MISSING  %-10s %v\n", p, err)
			bad++
			continue
		}
		st := sess.Status(path)
		age := ""
		if fi, err := os.Stat(path); err == nil {
			age = fmt.Sprintf("  age=%.0fh", time.Since(fi.ModTime()).Hours())
		}
		fmt.Printf("  %-8s %-10s %d cookies%s\n", "found", p, st.Cookies, age)
	}
	if bad > 0 {
		fmt.Printf("\n%d provider(s) have no stored session. Run: chimera auth all\n", bad)
	}
	// The live probe is delegated to scripts/provider_assessment.py, which tests
	// the credential end to end; duplicating it here would give two answers.
	fmt.Println("\nFor a live credential check, run:")
	fmt.Println("  python3 scripts/provider_assessment.py")
}

// devtoolsPort returns the CDP port of the provider's running browser, read from
// the profile's DevToolsActivePort file (written by Chromium at launch).
func devtoolsPort(cfg *config.Config) string {
	raw, err := os.ReadFile(filepath.Join(cfg.BrowserDataPath(), "DevToolsActivePort"))
	if err != nil {
		return ""
	}
	if line, _, ok := strings.Cut(string(raw), "\n"); ok {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(string(raw))
}

// authImport installs an already-exported session (e.g. from cdp-capture.mjs).
func authImport(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth import", flag.ExitOnError)
	file := fs.String("file", fmt.Sprintf("logs/%s-session.json", cfg.Provider), "exported session file")
	verify := fs.Bool("verify", true, "check the session against the provider")
	_ = fs.Parse(args)

	dst := cfg.SessionPath(cfg.Provider)
	sess, err := webapi.InstallSession(*file, dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import %s: %v\n", *file, err)
		os.Exit(1)
	}
	st := sess.Status(dst)
	fmt.Printf("Installed %s -> %s (%d cookies%s)\n", *file, st.Path, st.Cookies, expirySuffix(st))
	if *verify {
		verifySession(cfg)
	}
}

// authStatus prints the stored session's state without revealing secrets.
func authStatus(cfg *config.Config, _ []string) {
	path := cfg.SessionPath(cfg.Provider)
	sess, err := webapi.LoadSession(path)
	if err != nil {
		fmt.Printf("session: NOT USABLE — %v\n", err)
		os.Exit(1)
	}
	st := sess.Status(path)
	fmt.Printf("session: %s\n", st.Path)
	fmt.Printf("  provider: %s\n", cfg.Provider)
	fmt.Printf("  cookies: %d\n", st.Cookies)
	if st.HasExpiry {
		fmt.Printf("  token expires: %s (%.1f days)\n", st.ExpiresAt.Format(time.RFC3339), st.DaysRemain)
		if st.DaysRemain < 7 {
			fmt.Println("  warning: expiring soon — run `chimera auth login` to refresh")
		}
	} else {
		// Not necessarily an error: DeepSeek's userToken is opaque and ChatGPT is
		// cookie-custody, so neither is guaranteed to carry an exp claim.
		fmt.Println("  token expiry: unknown (no exp claim on the stored token)")
	}
	verifySession(cfg)
}

// verifySession constructs the provider's browserless transport and validates
// the stored session through its own Init. Provider-agnostic: it uses whatever
// registered itself with the webapi registry (spec 012).
func verifySession(cfg *config.Config) {
	ctor, ok := webapi.Lookup(cfg.Provider)
	if !ok {
		fmt.Printf("  provider check: skipped (no browserless transport for %q)\n", cfg.Provider)
		return
	}
	p := ctor(cfg)
	if err := p.Init(nil, cfg); err != nil {
		fmt.Printf("  provider check: FAILED — %v\n", err)
		warnStaleProfile(cfg)
		os.Exit(1)
	}
	// Init reports a transient probe failure as inconclusive so a CDN hiccup does
	// not take a running gateway down. Probe once more here so the CLI reports the
	// real error (a wrong endpoint, a rejected cookie) instead of a misleading OK.
	if ok, err := p.IsLoggedIn(); err != nil {
		fmt.Printf("  provider check: FAILED — %v\n", err)
		warnStaleProfile(cfg)
		os.Exit(1)
	} else if !ok {
		fmt.Printf("  provider check: FAILED — session not accepted\n")
		warnStaleProfile(cfg)
		os.Exit(1)
	}
	fmt.Printf("  provider check: OK (%s)\n", p.ModelID())
}

// warnStaleProfile explains the most common cause of a login that "succeeds" but
// produces a dead session: the persistent browser profile still holds an expired
// login, so the poll sees the old cookie immediately and exports it again. The
// fix is to start from a clean profile, not to log in more carefully.
func warnStaleProfile(cfg *config.Config) {
	profile := cfg.BrowserDataPath()
	if _, err := os.Stat(profile); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  Note: the browser profile at %s was reused.\n", profile)
	fmt.Fprintln(os.Stderr, "  If you were already logged in there, its OLD cookie was exported.")
	fmt.Fprintln(os.Stderr, "  To force a fresh login, remove the profile and retry:")
	fmt.Fprintf(os.Stderr, "      rm -rf %s && chimera auth login %s\n", profile, cfg.Provider)
}

func supportedAuthProviders() string {
	out := make([]string, 0, len(authSpecs))
	for _, p := range []string{config.ProviderQwen, config.ProviderDeepSeek, config.ProviderChatGPT} {
		if _, ok := authSpecs[p]; ok {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func expirySuffix(st webapi.SessionStatus) string {
	if !st.HasExpiry {
		return ", token expiry unknown"
	}
	return fmt.Sprintf(", token expires %s (%.0f days)", st.ExpiresAt.Format(time.RFC3339), st.DaysRemain)
}
