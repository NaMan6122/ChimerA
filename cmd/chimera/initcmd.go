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
	"strconv"
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
	// Extra carries settings this command does not own (API_TOKENS, quotas,
	// model selectors) so a rewrite cannot silently drop them. Losing API_TOKENS
	// on a -force would reopen a gateway that was authenticated.
	Extra map[string]string
}

// managedKeys are the settings init writes itself. Everything else in an
// existing file is preserved verbatim on a rewrite.
var managedKeys = map[string]bool{
	"PROVIDER": true, "TRANSPORT": true, "API_HOST": true, "API_PORT": true,
	"API_TOKEN": true, "AUTH_DIR": true, "BROWSER_DATA_DIR": true,
	"LOG_DIR": true, "LOG_LEVEL": true, "METER_DB": true,
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
	p := portFrom(existing, *port)
	extra := map[string]string{}
	for k, v := range existing {
		if !managedKeys[strings.ToUpper(k)] {
			extra[k] = v
		}
	}

	plan := initPlan{
		Provider:    chosen,
		Token:       keyToken,
		Port:        p,
		Home:        home,
		EnvPath:     envPath,
		TokenSource: tokenSource,
		Extra:       extra,
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

	adopted := adoptLegacyState(plan)
	if len(adopted) > 0 {
		fmt.Printf("\nAdopted from this directory (copies — nothing was moved):\n")
		for _, line := range adopted {
			fmt.Printf("  %s\n", line)
		}
	}
	noteLegacyState()
}

// portFrom resolves the listen port: the flag wins, then the port the install
// already uses, then 8000. A rewrite is about regenerating the file, not about
// moving the gateway.
func portFrom(existing map[string]string, flagPort int) int {
	if flagPort > 0 {
		return flagPort
	}
	if v := strings.TrimSpace(existing["API_PORT"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 8000
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
	// WriteFile keeps the mode of a file that already exists, so a settings file
	// that was world-readable before -force would stay that way while holding a
	// token. Tighten it explicitly.
	if err := os.Chmod(p.EnvPath, 0o600); err != nil {
		return fmt.Errorf("securing %s: %w", p.EnvPath, err)
	}
	return nil
}

// adoptLegacyState copies an existing cwd install into the machine-wide layout.
//
// Writing absolute AUTH_DIR/BROWSER_DATA_DIR paths is exactly what ends the
// "legacy cwd" fallback, so init would otherwise be the command that leaves a
// working install with no sessions: the next run would read empty home
// directories. Copies, never moves — a gateway may still be running from the
// old directory.
func adoptLegacyState(p initPlan) []string {
	var adopted []string

	sessions, _ := filepath.Glob("./auth_data/*.json")
	for _, src := range sessions {
		dst := filepath.Join(p.Home, "auth_data", filepath.Base(src))
		if src == dst || fileExists(dst) {
			continue
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		// Create the target directory here rather than relying on writePlan
		// having run: adoption must not depend on the order of the two steps.
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			continue
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			continue
		}
		_ = os.Chmod(dst, 0o600)
		adopted = append(adopted, "session  "+filepath.Base(src))
	}

	if entries, err := os.ReadDir("./browser_data"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dst := filepath.Join(p.Home, "browser_data", e.Name())
			if fileExists(dst) {
				continue
			}
			if err := copyTree(filepath.Join("./browser_data", e.Name()), dst); err != nil {
				continue
			}
			// Stale single-instance files stop Chromium from starting.
			for _, stale := range []string{"DevToolsActivePort", "SingletonLock", "SingletonSocket", "SingletonCookie"} {
				_ = os.Remove(filepath.Join(dst, stale))
			}
			adopted = append(adopted, "profile  browser_data/"+e.Name())
		}
	}

	// The usage ledger may be held open by a running gateway (WAL), so its
	// companions travel with it or the copy would be inconsistent.
	if fileExists("./logs/usage.db") && !fileExists(filepath.Join(p.Home, "usage.db")) {
		if err := copyFile("./logs/usage.db", filepath.Join(p.Home, "usage.db")); err == nil {
			for _, suffix := range []string{"-wal", "-shm"} {
				if fileExists("./logs/usage.db" + suffix) {
					_ = copyFile("./logs/usage.db"+suffix, filepath.Join(p.Home, "usage.db"+suffix))
				}
			}
			adopted = append(adopted, "ledger   logs/usage.db")
		}
	}
	return adopted
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o600)
}

// copyTree copies a directory of regular files (a Chromium profile has no
// symlinks worth preserving).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, fi.Mode().Perm())
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, raw, fi.Mode().Perm())
	})
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
		abs("auth_data"), abs("browser_data"), abs("logs"), filepath.Join(p.Home, "usage.db")) +
		extraBody(p.Extra)
}

// extraBody appends settings init does not own — tenant tokens, quotas, model
// selectors — so a rewrite preserves them instead of quietly reverting an
// authenticated gateway to an open one.
func extraBody(extra map[string]string) string {
	if len(extra) == 0 {
		return ""
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("\n# Preserved from the previous settings file (init does not manage these).\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, extra[k])
	}
	return b.String()
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
