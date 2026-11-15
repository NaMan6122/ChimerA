package main

// initcmd.go — `chimera init`: make this machine's Chimera install exist.
//
// Everything the gateway needs is configuration, and configuration used to mean
// assembling a 146-line .env by hand in whatever directory you happened to be
// in. init writes the machine-wide file once, in the place the binary already
// looks, and prints the one command that comes next.

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/chimera/chimera/internal/config"
)

const initUsage = `Usage: chimera init [-provider <name>] [-token <token>] [-port <n>] [-force] [-yes]

Creates the machine-wide Chimera directory and writes its settings file:

  <CHIMERA_HOME>/env          default ~/.local/share/chimera/env

From then on every command reads it, in any directory, so nothing depends on
where you happen to be standing. Existing state in the current directory
(./auth_data, ./browser_data) is left alone and named at the end.

Flags:
  -provider <name>   provider to default to (chatgpt, deepseek, qwen, kimi, glm,
                     gemini, grok, claude). Inferred when exactly one provider
                     already has an exported session.
  -token <token>     API token. Generated when omitted; an existing token in the
                     file is never replaced unless -force is given.
  -port <n>          gateway port (default 8000)
  -force             rewrite an existing settings file
  -yes               do not prompt; take the defaults

Next step after init: chimera auth login <provider>, then chimera up.
`

// initPlan is what init decided, before anything touches the disk. Kept separate
// so the decisions can be tested without a filesystem or a terminal.
type initPlan struct {
	Provider string
	Token    string
	Port     int
	Home     string
	EnvPath  string
	// TokenSource is "generated", "kept" or "flag": only a generated token has
	// to be printed, because it is the only copy that has not been seen.
	TokenSource string
}

// runInit handles `chimera init`.
func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	provider := fs.String("provider", "", "provider to default to")
	token := fs.String("token", "", "API token (generated when omitted)")
	port := fs.Int("port", 0, "gateway port (default 8000)")
	force := fs.Bool("force", false, "rewrite an existing settings file")
	yes := fs.Bool("yes", false, "do not prompt; take the defaults")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, initUsage)
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", fs.Arg(0), initUsage)
		os.Exit(2)
	}

	home := config.HomeDir()
	envPath := filepath.Join(home, "env")
	existing, hasExisting := readEnvMap(envPath)

	if hasExisting && !*force {
		fmt.Printf("chimera is already configured for this machine.\n\n")
		fmt.Printf("  settings  %s\n", envPath)
		fmt.Printf("  inspect   chimera config\n")
		fmt.Printf("  rewrite   chimera init -force\n\n")
		fmt.Printf("Nothing was changed.\n")
		return
	}

	if *provider != "" && !isSupportedProvider(*provider) {
		fmt.Fprintf(os.Stderr, "unknown provider %q — one of: %s\n", *provider, providerList())
		os.Exit(2)
	}

	chosen := *provider
	if chosen == "" {
		chosen = providerWithSession(home)
	}
	if chosen == "" {
		if *yes || !initPromptable() {
			fmt.Fprintf(os.Stderr, "no provider given and none has an exported session.\n\n"+
				"  pick one:  chimera init -provider deepseek -yes\n"+
				"  choices:   %s\n", providerList())
			os.Exit(2)
		}
		chosen = askProvider()
	}

	keyToken, tokenSource := tokenFor(*token, existing)
	p := *port
	if p == 0 {
		p = 8000
	}

	plan := initPlan{
		Provider:    chosen,
		Token:       keyToken,
		Port:        p,
		Home:        home,
		EnvPath:     envPath,
		TokenSource: tokenSource,
	}
	if err := writePlan(plan); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}

	fmt.Printf("Wrote %s\n\n", plan.EnvPath)
	fmt.Printf("  provider  %s\n", plan.Provider)
	fmt.Printf("  listen    127.0.0.1:%d\n", plan.Port)
	switch plan.TokenSource {
	case "generated":
		// Printed once: the file is 0600 and is the only other copy.
		fmt.Printf("  api token %s   (generated — pass this as your API key)\n", plan.Token)
	case "flag":
		fmt.Printf("  api token set from -token\n")
	default:
		fmt.Printf("  api token kept (see the file)\n")
	}
	fmt.Printf("\nNext:\n")
	fmt.Printf("  chimera auth login %s     # once, in a real browser\n", plan.Provider)
	fmt.Printf("  chimera up                # start every provider with a session\n")
	fmt.Printf("  chimera-chat              # no -model flag: it asks the gateway\n")

	noteLegacyState()
}

// tokenFor decides the API token. An explicit one wins; otherwise a token that
// is already in the file is kept, because silently rotating a working credential
// would break every client pointed at this gateway; only a fresh install mints a
// new one.
func tokenFor(explicit string, existing map[string]string) (token, source string) {
	if t := strings.TrimSpace(explicit); t != "" {
		return t, "flag"
	}
	if t := strings.TrimSpace(existing["API_TOKEN"]); t != "" {
		return t, "kept"
	}
	return newToken(), "generated"
}

// initPromptable reports whether there is a terminal to ask a question on.
func initPromptable() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// writePlan creates the directory tree and writes the settings file.
func writePlan(p initPlan) error {
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{p.Home, 0o700},
		{filepath.Join(p.Home, "auth_data"), 0o700},
		{filepath.Join(p.Home, "logs"), 0o755},
		{filepath.Join(p.Home, "browser_data"), 0o755},
	} {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("creating %s: %w", dir.path, err)
		}
	}
	body := envFileBody(p)
	if err := os.WriteFile(p.EnvPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", p.EnvPath, err)
	}
	return nil
}

// envFileBody is the settings file: absolute paths, so the file means the same
// thing from any working directory.
func envFileBody(p initPlan) string {
	abs := func(sub string) string { return filepath.Join(p.Home, sub) }
	return fmt.Sprintf(`# Chimera — machine-wide settings, written by `+"`chimera init`"+`.
# Every `+"`chimera`"+` command reads this file, in any directory. Edit it directly,
# or re-run `+"`chimera init -force`"+` to regenerate it.

PROVIDER=%s
TRANSPORT=auto

# Loopback by default: a personal gateway should not be reachable from the
# network until someone decides it should be.
API_HOST=127.0.0.1
API_PORT=%d
API_TOKEN=%s

# Absolute paths: state belongs to the install, not to a working directory.
AUTH_DIR=%s
BROWSER_DATA_DIR=%s
LOG_DIR=%s
LOG_LEVEL=info
METER_DB=%s
`, p.Provider, p.Port, p.Token,
		abs("auth_data"), abs("browser_data"), abs("logs"), filepath.Join(p.Home, "usage.db"))
}

// providerWithSession names the provider that already has an exported session,
// when exactly one does: re-initialising an install should not change which
// provider it serves.
func providerWithSession(home string) string {
	matches, err := filepath.Glob(filepath.Join(home, "auth_data", "*.json"))
	if err != nil || len(matches) != 1 {
		return ""
	}
	name := strings.TrimSuffix(filepath.Base(matches[0]), ".json")
	if isSupportedProvider(name) {
		return name
	}
	return ""
}

// askProvider prompts on a terminal. Reads are line-based and offer the
// session-having provider first.
func askProvider() string {
	fmt.Printf("Which provider should this machine serve?\n\n")
	names := providerNames()
	for i, n := range names {
		fmt.Printf("  %d) %s\n", i+1, n)
	}
	fmt.Printf("\nChoice [1]: ")
	var line string
	fmt.Scanln(&line)
	line = strings.TrimSpace(line)
	if line == "" {
		return names[0]
	}
	for i, n := range names {
		if line == n || line == fmt.Sprint(i+1) {
			return n
		}
	}
	return line
}

// providerNames is every provider a gateway can serve, in a stable order.
func providerNames() []string {
	out := make([]string, 0, len(config.SupportedProviders)-1)
	for _, p := range config.SupportedProviders {
		if p == config.ProviderAll {
			continue
		}
		out = append(out, p)
	}
	return out
}

func providerList() string {
	names := providerNames()
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func isSupportedProvider(p string) bool {
	for _, n := range providerNames() {
		if strings.EqualFold(n, p) {
			return true
		}
	}
	return false
}

// noteLegacyState keeps an existing working directory from being forgotten: the
// old layout is not moved automatically, it is named.
func noteLegacyState() {
	var found []string
	for _, sub := range []string{"auth_data", "browser_data"} {
		if fi, err := os.Stat("./" + sub); err == nil && fi.IsDir() {
			found = append(found, "./"+sub)
		}
	}
	if len(found) == 0 {
		return
	}
	fmt.Printf("\n  note: %s in this directory were not touched.\n", strings.Join(found, " and "))
	fmt.Printf("        Re-login with `chimera auth login <provider>` to store sessions\n")
	fmt.Printf("        under %s, or point AUTH_DIR at the old path.\n", filepath.Join(config.HomeDir(), "auth_data"))
}

// newToken mints a 32-character API token. Not a UUID: it is shown once and
// pasted into clients, so shortness with full entropy beats structure.
func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("chimera-%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// readEnvMap reads a dotenv file without touching the process environment.
func readEnvMap(path string) (map[string]string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}, false
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, true
}
