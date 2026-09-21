package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
	_ "github.com/chimera/chimera/internal/providers/webapi/all"
)

const authUsage = `Usage: chimera auth <command>

Commands:
  login    Open a browser, wait for you to log in, and store the session
  import   Validate and install an exported session file
  status   Show the stored session's validity and expiry

Supported providers: qwen, deepseek, chatgpt (selected via PROVIDER=<name>)

Flags:
  login:   -timeout <dur>   how long to wait for login (default 5m)
           -hold <dur>      keep the browser open after login (traffic capture)
  import:  -file <path>     exported session (default logs/<provider>-session.json)
           -verify <bool>   check against the provider (default true)
`

// authSpec describes how a provider's session material is harvested from the
// browser. browser.Manager.Launch() already navigates to the provider URL, so
// each expression simply runs on that origin.
type authSpec struct {
	// TokenExpr is JS evaluated in the page, returning the access token.
	TokenExpr string
	// CookieHost filters collected cookies to the provider's own domains.
	CookieHost string
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
		// The durable material is the session cookie; the bearer is minted from
		// it by /api/auth/session, so a missing bearer is not a login failure.
		TokenExpr:  `() => document.cookie.includes('__Secure-next-auth.session-token') ? 'cookie-session' : ''`,
		CookieHost: "chatgpt",
	},
}

// runAuth handles `chimera auth <command>` and exits.
func runAuth(args []string) {
	if len(args) == 0 {
		fmt.Print(authUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	switch args[0] {
	case "login":
		authLogin(cfg, args[1:])
	case "import":
		authImport(cfg, args[1:])
	case "status":
		authStatus(cfg, args[1:])
	case "help", "-h", "--help":
		fmt.Print(authUsage)
	default:
		fmt.Fprintf(os.Stderr, "unknown auth command %q\n\n%s", args[0], authUsage)
		os.Exit(2)
	}
}

// authLogin opens Chromium, waits for the user to log in, and exports the
// storage state (access token + cookies) into AUTH_DIR.
func authLogin(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for login")
	hold := fs.Duration("hold", 0, "keep the browser open this long after login so traffic can be captured")
	_ = fs.Parse(args)

	spec, ok := authSpecs[cfg.Provider]
	if !ok {
		fmt.Fprintf(os.Stderr, "auth login supports %s only (PROVIDER=%s)\n", supportedAuthProviders(), cfg.Provider)
		os.Exit(1)
	}

	fmt.Printf("Opening %s — log in there; the window stays open.\n", cfg.ProviderURL())
	fmt.Printf("Waiting for the session (up to %s)...\n", *timeout)

	mgr := browser.NewManager(cfg)
	page, err := mgr.Launch() // navigates to cfg.ProviderURL()
	if err != nil {
		fmt.Fprintln(os.Stderr, "launch browser:", err)
		os.Exit(1)
	}
	defer mgr.Close()

	var marker string
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		res, err := page.Eval(spec.TokenExpr)
		if err != nil {
			continue
		}
		if marker = res.Value.Str(); marker != "" {
			break
		}
	}
	if marker == "" {
		fmt.Fprintln(os.Stderr, "timed out waiting for login — no session found")
		os.Exit(1)
	}

	cookies, err := page.Cookies([]string{cfg.ProviderURL()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "read cookies:", err)
		os.Exit(1)
	}
	jar := map[string]string{}
	for _, c := range cookies {
		if strings.Contains(c.Domain, spec.CookieHost) {
			jar[c.Name] = c.Value
		}
	}
	if len(jar) == 0 {
		fmt.Fprintf(os.Stderr, "no %s cookies found — is the page logged in?\n", cfg.Provider)
		os.Exit(1)
	}

	// marker is a real token where the provider stores one, and the literal
	// "cookie-session" for cookie-custody providers (ChatGPT, whose bearer is
	// minted on demand). Either way the cookie jar is the durable credential.
	token := marker
	if marker == "cookie-session" {
		token = "cookie-session"
	}
	sess := &webapi.Session{AccessToken: token, Cookies: jar}
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
		os.Exit(1)
	}
	// Init reports a transient probe failure as inconclusive so a CDN hiccup does
	// not take a running gateway down. Probe once more here so the CLI reports the
	// real error (a wrong endpoint, a rejected cookie) instead of a misleading OK.
	if ok, err := p.IsLoggedIn(); err != nil {
		fmt.Printf("  provider check: FAILED — %v\n", err)
		os.Exit(1)
	} else if !ok {
		fmt.Printf("  provider check: FAILED — session not accepted\n")
		os.Exit(1)
	}
	fmt.Printf("  provider check: OK (%s)\n", p.ModelID())
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
