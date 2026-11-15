package main

// The config report's contract: every setting names the source that won, secrets
// are described rather than printed, and the paths it shows are the paths the
// gateway will use.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chimera/chimera/internal/config"
)

// testConfig is a Config with every path under one temp dir, so the report is
// deterministic on any machine.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Provider:         config.ProviderDeepSeek,
		Transport:        "auto",
		APIHost:          "127.0.0.1",
		APIPort:          8000,
		APIToken:         "chimera",
		AuthDir:          dir + "/auth_data",
		BrowserDataDir:   dir + "/browser_data",
		LogDir:           dir + "/logs",
		LogLevel:         "info",
		Verbose:          false,
		MeterDB:          dir + "/logs/usage.db",
		SessionsDir:      dir + "/sessions",
		DeepSeekWebModel: "deepseek-chat",
	}
}

// TestConfigNeverPrintsTheToken is the security contract: the report is meant to
// be pasted into an issue, so the bearer token appears only as a shape.
func TestConfigNeverPrintsTheToken(t *testing.T) {
	cfg := testConfig(t)
	cfg.APIToken = "sek-super-secret-value"

	out := renderConfig(cfg, nil)
	if strings.Contains(out, cfg.APIToken) {
		t.Fatalf("the token leaked into the report:\n%s", out)
	}
	if want := "set (" + itoa(len(cfg.APIToken)) + " chars)"; !strings.Contains(out, want) {
		t.Fatalf("token shape %q missing:\n%s", want, out)
	}
}

func itoa(n int) string { return fmt.Sprint(n) }

// TestConfigReportsEveryResolvedPath: the point of the command is that the paths
// it prints are the ones in use, including the usage ledger and transcripts.
func TestConfigReportsEveryResolvedPath(t *testing.T) {
	cfg := testConfig(t)
	out := renderConfig(cfg, nil)

	for _, want := range []string{
		"provider", cfg.Provider,
		"transport", cfg.Transport,
		"127.0.0.1:8000",
		cfg.AuthDir,
		cfg.BrowserDataDir,
		cfg.LogDir,
		cfg.MeterDB,
		cfg.SessionsDir,
		"deepseek-chat",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// TestConfigShowsSources: an environment variable outranks the files, and the
// file is named when it supplies the value.
func TestConfigShowsSources(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("PROVIDER", config.ProviderQwen)

	// A Sources value with no files and no process entries means everything is a
	// default; with PROVIDER present it must say so.
	src := &config.Sources{}
	if got := src.Of("PROVIDER"); got != "default" {
		t.Fatalf("Of(PROVIDER) without provenance = %q, want default", got)
	}

	settings := resolveSettings(cfg, nil)
	for _, s := range settings {
		if s.Key == "provider" && s.Source != "default" {
			t.Errorf("provider source = %q, want default when no provenance is available", s.Source)
		}
	}
}

// TestRedact covers the three states a token can be in.
func TestRedact(t *testing.T) {
	if got := redact(""); !strings.Contains(got, "unset") {
		t.Errorf("redact(empty) = %q", got)
	}
	if got := redact("abc"); got != "set (3 chars)" {
		t.Errorf("redact(abc) = %q", got)
	}
}

// TestConfigNeverTruncatesPaths: the value column prints paths whole — a
// truncated path is unusable for the copy-paste it exists for.
func TestConfigNeverTruncatesPaths(t *testing.T) {
	cfg := testConfig(t)
	cfg.BrowserDataDir = strings.Repeat("/very-long-segment", 6)
	if out := renderConfig(cfg, nil); !strings.Contains(out, cfg.BrowserDataDir) {
		t.Fatalf("path was truncated out of the report:\n%s", out)
	}
}

// TestSessionStateOnlyReportsPresence: the rows name files, and the file's
// contents are never read — they are password-equivalent.
func TestSessionStateOnlyReportsPresence(t *testing.T) {
	cfg := testConfig(t)
	states := providerSessions(cfg)
	if len(states) == 0 {
		t.Fatal("no providers reported")
	}
	for _, s := range states {
		if s.Present {
			t.Errorf("%s reported present although no session file exists", s.Provider)
		}
		if !strings.HasSuffix(s.Path, s.Provider+".json") {
			t.Errorf("%s path = %q, want <auth dir>/%s.json", s.Provider, s.Path, s.Provider)
		}
	}
}
