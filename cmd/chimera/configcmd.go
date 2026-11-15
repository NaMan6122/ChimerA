package main

// configcmd.go — `chimera config`: what this binary actually resolved, and why.
//
// The gateway takes its settings from four places (process environment, ./.env,
// <CHIMERA_HOME>/env, compiled defaults), so "why is it doing that" used to need
// the source. This prints the winner for each setting next to its value, never
// the secret itself.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
	_ "github.com/chimera/chimera/internal/providers/webapi/all"
)

const configUsage = `Usage: chimera config [-json]

Prints the settings this binary resolves, each with the source that won:
the process environment, ./.env, <CHIMERA_HOME>/env, or a compiled default.

Also shows where sessions, logs and the usage ledger live, and which providers
have an exported session. Secret values are never printed — only whether a token
is set.

  -json   machine-readable output
`

// setting is one reported value: the resolved value, where it came from, and
// whether it is safe to show verbatim.
type setting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
	Secret bool   `json:"secret,omitempty"`
}

// runConfig handles `chimera config`.
func runConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, configUsage)
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", fs.Arg(0), configUsage)
		os.Exit(2)
	}

	// Read-only: reporting the configuration must not create directories or
	// otherwise change the install it is describing.
	cfg, src, err := config.LoadReadOnly()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"settings":  resolveSettings(cfg, src),
			"env_files": envFileState(src, cfg),
			"sessions":  providerSessions(cfg),
		})
		return
	}
	fmt.Print(renderConfig(cfg, src))
}

// renderConfig formats the report. Kept free of side effects so a test can read
// what the user will see.
func renderConfig(cfg *config.Config, src *config.Sources) string {
	var b strings.Builder
	fmt.Fprintf(&b, "chimera config — %s\n\n", config.HomeDir())
	// Paths are the most copyable thing here, so they are printed whole: a
	// width that fits them would make the table unreadable on a narrow terminal,
	// and truncation would hide the answer to the question being asked.
	b.WriteString("  setting              source                  value\n")
	b.WriteString("  ───────────────────  ──────────────────────  ─────────────────────────────\n")
	for _, s := range resolveSettings(cfg, src) {
		value := s.Value
		if s.Secret {
			value = redact(s.Value)
		}
		fmt.Fprintf(&b, "  %-19s  %-22s  %s\n", s.Key, s.Source, value)
	}

	b.WriteString("\n  env files\n")
	for _, f := range envFileState(src, cfg) {
		mark := "—  "
		if f.Present {
			mark = "✓  "
		}
		note := ""
		if f.Path == "" {
			note = "(" + f.Note + ")"
		} else if f.Note != "" {
			note = "(" + f.Note + ")"
		}
		fmt.Fprintf(&b, "    %s%-46s %s\n", mark, f.Path, note)
	}

	b.WriteString("\n  sessions\n")
	for _, s := range providerSessions(cfg) {
		mark := "—  "
		if s.Present {
			mark = "✓  "
		}
		fmt.Fprintf(&b, "    %s%-10s %s\n", mark, s.Provider, s.Path)
	}
	return b.String()
}

// resolveSettings is the ordered report: provider first, then what it listens
// on, then where it keeps state.
func resolveSettings(cfg *config.Config, src *config.Sources) []setting {
	// The table is read at a glance, so the source column names the kind of
	// source; the exact files are listed underneath.
	from := func(key string) string {
		if src == nil {
			return "default"
		}
		switch s := src.Of(key); {
		case s == "default", s == "environment", s == ".env":
			return s
		case strings.Contains(s, "legacy"):
			return "legacy cwd"
		default:
			return "home env"
		}
	}
	port := fmt.Sprintf("%d", cfg.APIPort)
	settings := []setting{
		{Key: "provider", Value: cfg.Provider, Source: from("PROVIDER")},
		{Key: "transport", Value: cfg.Transport, Source: from("TRANSPORT")},
	}
	if cfg.TransportFallback != "" {
		settings = append(settings, setting{Key: "fallback", Value: cfg.TransportFallback, Source: from("TRANSPORT_FALLBACK")})
	}
	settings = append(settings,
		setting{Key: "listen", Value: fmt.Sprintf("%s:%s", cfg.APIHost, port), Source: joinSources(from("API_HOST"), from("API_PORT"))},
	)
	// A gateway with tenant tokens requires auth, so describing it as open would
	// send an operator looking for a problem that does not exist — or worse, hide
	// one that does.
	if tenants := tenantNames(cfg.APITokens); len(tenants) > 0 {
		settings = append(settings, setting{
			Key:    "api tokens",
			Value:  fmt.Sprintf("%d tenant(s): %s", len(tenants), strings.Join(tenants, ", ")),
			Source: from("API_TOKENS"),
		})
	} else {
		settings = append(settings, setting{Key: "api token", Value: cfg.APIToken, Source: from("API_TOKEN"), Secret: true})
	}
	settings = append(settings,
		setting{Key: "auth dir", Value: cfg.AuthDir, Source: from("AUTH_DIR")},
		setting{Key: "browser dir", Value: cfg.BrowserDataDir, Source: from("BROWSER_DATA_DIR")},
		setting{Key: "log dir", Value: cfg.LogDir, Source: from("LOG_DIR")},
		setting{Key: "log level", Value: cfg.LogLevel, Source: from("LOG_LEVEL")},
		setting{Key: "verbose", Value: fmt.Sprintf("%t", cfg.Verbose), Source: from("VERBOSE")},
		setting{Key: "usage ledger", Value: cfg.MeterDB, Source: from("METER_DB")},
		setting{Key: "sessions dir", Value: cfg.SessionsDir, Source: sessionsSource(from)},
		setting{Key: "quota / month", Value: quotaText(cfg.QuotaMonthlyRequests), Source: from("QUOTA_MONTHLY_REQUESTS")},
		setting{Key: "max prompt", Value: fmt.Sprintf("%d chars", cfg.MaxPromptChars), Source: from("MAX_PROMPT_CHARS")},
		setting{Key: "chimera home", Value: config.HomeDir(), Source: from("CHIMERA_HOME")},
	)
	if m := webModel(cfg); m != "" {
		settings = append(settings, setting{Key: "web model", Value: m, Source: from(modelKey(cfg.Provider))})
	}
	return settings
}

// tenantNames lists the tenant names in an API_TOKENS value ("acme=a1,basic=b2").
// Names are reported, secrets never: the tokens are what must stay private.
func tenantNames(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var names []string
	for _, pair := range strings.Split(spec, ",") {
		name, _, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.TrimSpace(name) != "" {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// sessionsSource names the variable that actually decided the sessions dir: the
// harness's own override wins over the gateway's SESSIONS_DIR, and both are
// consulted from the environment.
func sessionsSource(from func(string) string) string {
	if s := from("CHIMERA_SESSIONS_DIR"); s != "default" {
		return s
	}
	return from("SESSIONS_DIR")
}

func quotaText(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d requests", n)
}

// webModel is the browserless model selector for the configured provider, if it
// has one.
func webModel(cfg *config.Config) string {
	switch cfg.Provider {
	case config.ProviderDeepSeek:
		return cfg.DeepSeekWebModel
	case config.ProviderQwen:
		return cfg.QwenWebModel
	case config.ProviderChatGPT:
		return cfg.ChatGPTWebModel
	case config.ProviderGemini:
		return cfg.GeminiWebModel
	case config.ProviderGrok:
		return cfg.GrokWebModel
	case config.ProviderGLM:
		return cfg.GLMWebModel
	case config.ProviderKimi:
		return cfg.KimiWebModel
	}
	return ""
}

func modelKey(provider string) string {
	return strings.ToUpper(provider) + "_WEB_MODEL"
}

// joinSources reports the more interesting of two sources: a default on either
// side means the other one decided.
func joinSources(a, b string) string {
	if a == b {
		return a
	}
	if a == "default" {
		return b
	}
	return a
}

// envFile is one file that can supply settings.
type envFile struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Note    string `json:"note,omitempty"`
}

func envFileState(src *config.Sources, cfg *config.Config) []envFile {
	cwd, home := ".env", filepath.Join(config.HomeDir(), "env")
	if src != nil {
		cwd, home = src.EnvFiles()
	}
	out := []envFile{{Path: cwd, Present: fileExists(cwd), Note: noteFile(cwd, "project overrides")}}
	if home == "" {
		out = append(out, envFile{Path: "(no home dir)", Present: false, Note: "CHIMERA_HOME unset"})
	} else {
		out = append(out, envFile{Path: home, Present: fileExists(home), Note: noteFile(home, "machine-wide settings")})
	}
	// A legacy cwd layout is worth naming: it is why sessions may not be where
	// the home-based defaults suggest.
	if cfg != nil && legacyPath(cfg.AuthDir) {
		out = append(out, envFile{Path: cfg.AuthDir, Present: true, Note: "legacy cwd sessions"})
	}
	return out
}

func noteFile(path, whenPresent string) string {
	if fileExists(path) {
		return whenPresent
	}
	return "not present"
}

func legacyPath(p string) bool {
	return p == "./auth_data" || p == "./browser_data" || p == "./logs"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// providerSessions reports which providers have an exported session. Presence only:
// the file's contents are password-equivalent.
type sessionState struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Present  bool   `json:"present"`
}

func providerSessions(cfg *config.Config) []sessionState {
	seen := map[string]bool{}
	for _, p := range config.SupportedProviders {
		if p == config.ProviderAll {
			continue
		}
		seen[p] = true
	}
	for _, p := range webapi.Registered() {
		seen[p] = true
	}
	names := make([]string, 0, len(seen))
	for p := range seen {
		names = append(names, p)
	}
	sort.Strings(names)

	out := make([]sessionState, 0, len(names))
	for _, p := range names {
		path := cfg.SessionPath(p)
		out = append(out, sessionState{Provider: p, Path: path, Present: fileExists(path)})
	}
	return out
}

// redact describes a secret without revealing it: enough to tell "set" from
// "unset", not enough to use.
func redact(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unset (requests accepted without auth)"
	}
	return fmt.Sprintf("set (%d chars)", len(v))
}
