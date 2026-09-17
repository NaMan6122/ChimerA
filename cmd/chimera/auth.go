package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/browser"
	"github.com/chimera/chimera/internal/config"
	qwenweb "github.com/chimera/chimera/internal/providers/webapi/qwen"
)

const authUsage = `Usage: chimera auth <command>

Commands:
  login    Open a browser, wait for you to log in, and store the session
  import   Validate and install an exported session file
  status   Show the stored session's validity and expiry

Flags:
  login:   -timeout <dur>   how long to wait for login (default 5m)
  import:  -file <path>     exported session (default logs/qwen-session.json)
           -verify <bool>   check against the provider (default true)
`

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
	_ = fs.Parse(args)

	if cfg.Provider != config.ProviderQwen {
		fmt.Fprintf(os.Stderr, "auth login currently supports qwen only (PROVIDER=%s)\n", cfg.Provider)
		os.Exit(1)
	}

	fmt.Printf("Opening %s — log in there; the window stays open.\n", cfg.QwenURL)
	fmt.Printf("Waiting for the session token (up to %s)...\n", *timeout)

	mgr := browser.NewManager(cfg)
	page, err := mgr.Launch()
	if err != nil {
		fmt.Fprintln(os.Stderr, "launch browser:", err)
		os.Exit(1)
	}
	defer mgr.Close()

	var token string
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		res, err := page.Eval(`() => localStorage.getItem('token') || ''`)
		if err != nil {
			continue
		}
		if token = res.Value.Str(); token != "" {
			break
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "timed out waiting for login — no token found")
		os.Exit(1)
	}

	cookies, err := page.Cookies([]string{cfg.QwenURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "read cookies:", err)
		os.Exit(1)
	}
	jar := map[string]string{}
	for _, c := range cookies {
		if strings.Contains(c.Domain, "qwen") {
			jar[c.Name] = c.Value
		}
	}
	if len(jar) == 0 {
		fmt.Fprintln(os.Stderr, "no qwen cookies found — is the page logged in?")
		os.Exit(1)
	}

	sess := &qwenweb.Session{AccessToken: token, Cookies: jar}
	dst := cfg.QwenSessionPath()
	if err := qwenweb.WriteSession(sess, dst); err != nil {
		fmt.Fprintln(os.Stderr, "write session:", err)
		os.Exit(1)
	}
	st := sess.Status(dst)
	fmt.Printf("Session stored: %s (%d cookies%s)\n", dst, st.Cookies, expirySuffix(st))
	verifySession(sess)
}

// authImport installs an already-exported session (e.g. from cdp-export.mjs).
func authImport(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("auth import", flag.ExitOnError)
	file := fs.String("file", "logs/qwen-session.json", "exported session file")
	verify := fs.Bool("verify", true, "check the session against the provider")
	_ = fs.Parse(args)

	sess, err := qwenweb.InstallSession(*file, cfg.QwenSessionPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "import %s: %v\n", *file, err)
		os.Exit(1)
	}
	st := sess.Status(cfg.QwenSessionPath())
	fmt.Printf("Installed %s -> %s (%d cookies%s)\n", *file, st.Path, st.Cookies, expirySuffix(st))
	if *verify {
		verifySession(sess)
	}
}

// authStatus prints the stored session's state without revealing secrets.
func authStatus(cfg *config.Config, _ []string) {
	sess, err := qwenweb.LoadSession(cfg.QwenSessionPath())
	if err != nil {
		fmt.Printf("session: NOT USABLE — %v\n", err)
		os.Exit(1)
	}
	st := sess.Status(cfg.QwenSessionPath())
	fmt.Printf("session: %s\n", st.Path)
	fmt.Printf("  cookies: %d\n", st.Cookies)
	if st.HasExpiry {
		fmt.Printf("  token expires: %s (%.1f days)\n", st.ExpiresAt.Format(time.RFC3339), st.DaysRemain)
		if st.DaysRemain < 7 {
			fmt.Println("  warning: expiring soon — run `chimera auth login` to refresh")
		}
	} else {
		fmt.Println("  token expiry: unknown (no exp claim)")
	}
	verifySession(sess)
}

func verifySession(sess *qwenweb.Session) {
	client := qwenweb.NewClient(sess, 30*time.Second)
	models, err := client.ListModels()
	if err != nil {
		fmt.Printf("  provider check: FAILED — %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  provider check: OK (%d models)\n", len(models))
}

func expirySuffix(st qwenweb.SessionStatus) string {
	if !st.HasExpiry {
		return ", token expiry unknown"
	}
	return fmt.Sprintf(", token expires %s (%.0f days)", st.ExpiresAt.Format(time.RFC3339), st.DaysRemain)
}
