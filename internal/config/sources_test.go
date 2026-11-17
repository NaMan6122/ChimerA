package config

// Resolution tests: which of the four sources wins, where the directories land
// with no configuration at all, and what happens to an install that predates the
// machine-wide layout.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearSettings removes the variables these tests care about, so a developer's
// shell cannot decide the outcome.
func clearSettings(t *testing.T) {
	t.Helper()
	// CHIMERA_HOME is deliberately not cleared: TestMain points it at an empty
	// directory, and unsetting it here would make the test read the developer's
	// real machine-wide file — the exact leak this isolation prevents.
	for _, k := range []string{
		"PROVIDER", "TRANSPORT", "API_HOST", "API_PORT", "API_TOKEN", "AUTH_DIR",
		"BROWSER_DATA_DIR", "LOG_DIR", "LOG_LEVEL", "VERBOSE", "METER_DB", "SESSIONS_DIR",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// writeEnv writes a dotenv file, failing the test if it cannot.
func writeEnv(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPrecedenceEnvOverProjectOverHome is the whole contract of the resolver:
// the process environment beats ./.env, which beats <CHIMERA_HOME>/env, which
// beats the defaults.
func TestPrecedenceEnvOverProjectOverHome(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)

	writeEnv(t, filepath.Join(home, "env"), "PROVIDER=qwen\nAPI_TOKEN=from-home\n")
	writeEnv(t, filepath.Join(work, ".env"), "PROVIDER=deepseek\n")

	// Nothing in the process environment: the project file wins.
	t.Setenv("PROVIDER", "")
	os.Unsetenv("PROVIDER")
	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderDeepSeek {
		t.Fatalf("provider = %q, want the project file's deepseek", cfg.Provider)
	}
	if got := src.Of("API_TOKEN"); got != filepath.Join(home, "env") {
		t.Fatalf("API_TOKEN source = %q, want the home env file", got)
	}

	// An exported variable outranks both files.
	t.Setenv("PROVIDER", ProviderKimi)
	cfg, src, err = LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderKimi {
		t.Fatalf("provider = %q, want the exported kimi", cfg.Provider)
	}
	if got := src.Of("PROVIDER"); got != "environment" {
		t.Fatalf("provider source = %q", got)
	}

	// Without the project file, the machine-wide one decides.
	if err := os.Remove(filepath.Join(work, ".env")); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("PROVIDER")
	cfg, _, err = LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderQwen {
		t.Fatalf("provider = %q, want the home file's qwen", cfg.Provider)
	}
}

// TestDefaultsLiveUnderChimeraHome: with no configuration at all, state belongs
// in one machine-wide directory rather than wherever the process was started.
func TestDefaultsLiveUnderChimeraHome(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)

	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"AUTH_DIR", cfg.AuthDir, filepath.Join(home, "auth_data")},
		{"BROWSER_DATA_DIR", cfg.BrowserDataDir, filepath.Join(home, "browser_data")},
		{"LOG_DIR", cfg.LogDir, filepath.Join(home, "logs")},
		{"METER_DB", cfg.MeterDB, filepath.Join(home, "logs", "usage.db")},
		// Session transcripts follow the same root: a relocated install must not
		// keep its transcripts in the default location while everything else
		// moved.
		{"SESSIONS_DIR", cfg.SessionsDir, filepath.Join(home, "sessions")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
		if got := src.Of(c.name); got != "default" {
			t.Errorf("%s source = %q, want default", c.name, got)
		}
	}
	// The directories the gateway would create are created by Load itself.
	if _, err := os.Stat(filepath.Join(home, "auth_data")); err != nil {
		t.Errorf("auth dir was not created: %v", err)
	}
}

// TestLegacyCwdLayoutIsKept: an install that has been running from a directory
// keeps using its ./auth_data until the machine-wide copy exists, so an upgrade
// does not look like every session was lost.
func TestLegacyCwdLayoutIsKept(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)

	if err := os.Mkdir("./auth_data", 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != "./auth_data" {
		t.Fatalf("AuthDir = %q, want the legacy ./auth_data", cfg.AuthDir)
	}
	if got := src.Of("AUTH_DIR"); !strings.Contains(got, "legacy") {
		t.Fatalf("source = %q, want it named as the legacy layout", got)
	}

	// Once the machine-wide directory exists, it wins: the fallback is only for
	// a layout that has not been adopted yet.
	if err := os.MkdirAll(filepath.Join(home, "auth_data"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != filepath.Join(home, "auth_data") {
		t.Fatalf("AuthDir = %q, want %q once the home copy exists", cfg.AuthDir, filepath.Join(home, "auth_data"))
	}
}

// TestExplicitPathAlwaysWins: a value in the environment or a file is used as
// written, legacy directory or not.
func TestExplicitPathAlwaysWins(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)
	if err := os.Mkdir("./auth_data", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_DIR", "/tmp/explicit-auth")

	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != "/tmp/explicit-auth" {
		t.Fatalf("AuthDir = %q, want the explicit value", cfg.AuthDir)
	}
	if got := src.Of("AUTH_DIR"); got != "environment" {
		t.Fatalf("source = %q", got)
	}
}

// TestLoadReadOnlyCreatesNothing: an inspection command must not change the
// thing it inspects, and must work when the filesystem is not writable.
func TestLoadReadOnlyCreatesNothing(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)

	cfg, _, err := LoadReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != filepath.Join(home, "auth_data") {
		t.Fatalf("AuthDir = %q", cfg.AuthDir)
	}
	for _, sub := range []string{"auth_data", "logs", "browser_data"} {
		if _, err := os.Stat(filepath.Join(home, sub)); err == nil {
			t.Errorf("LoadReadOnly created %s", sub)
		}
	}

	// And the writing path still does create them.
	if _, _, err := LoadWithSources(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth_data")); err != nil {
		t.Errorf("LoadWithSources did not create the auth dir: %v", err)
	}
}

// TestReloadSeesFileEdits: a value copied out of a settings file must not become
// indistinguishable from an exported one, or editing the file would appear to do
// nothing on the next load — and provenance would blame the environment.
func TestReloadSeesFileEdits(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(t.TempDir())
	envPath := filepath.Join(home, "env")

	writeEnv(t, envPath, "PROVIDER=qwen\n")
	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderQwen {
		t.Fatalf("first load provider = %q", cfg.Provider)
	}
	if got := src.Of("PROVIDER"); got != envPath {
		t.Fatalf("provenance = %q, want the file", got)
	}

	writeEnv(t, envPath, "PROVIDER=kimi\n")
	cfg, src, err = LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderKimi {
		t.Fatalf("second load provider = %q, want the edited kimi", cfg.Provider)
	}
	if got := src.Of("PROVIDER"); got != envPath {
		t.Fatalf("second load provenance = %q, want the file, not the environment", got)
	}
}

// TestDirRejectsAFileNamedLikeTheDirectory: a regular file named ./auth_data must
// not be selected, or Load dies with "not a directory" while the report calls it
// a healthy setting.
func TestDirRejectsAFileNamedLikeTheDirectory(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(work)

	if err := os.WriteFile("./auth_data", []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadWithSources()
	if err != nil {
		t.Fatalf("Load with a file named auth_data: %v", err)
	}
	if cfg.AuthDir != filepath.Join(home, "auth_data") {
		t.Fatalf("AuthDir = %q, want the home directory", cfg.AuthDir)
	}
}

// TestRemovedFileValueDoesNotLinger: after a key is deleted from the settings
// file, the next load must fall back to the default instead of serving the value
// of a file that no longer sets it.
func TestRemovedFileValueDoesNotLinger(t *testing.T) {
	clearSettings(t)
	home := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Chdir(t.TempDir())
	envPath := filepath.Join(home, "env")

	writeEnv(t, envPath, "PROVIDER=qwen\nLOG_LEVEL=warn\n")
	if cfg, _, err := LoadWithSources(); err != nil || cfg.Provider != ProviderQwen {
		t.Fatalf("first load: provider=%v err=%v", cfg.Provider, err)
	}

	writeEnv(t, envPath, "LOG_LEVEL=warn\n")
	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderChatGPT {
		t.Fatalf("provider = %q, want the compiled default once the file stopped setting it", cfg.Provider)
	}
	if got := src.Of("PROVIDER"); got != "default" {
		t.Fatalf("source = %q, want default", got)
	}
	// The surviving key is still honoured.
	if cfg.LogLevel != "warn" {
		t.Fatalf("log level = %q", cfg.LogLevel)
	}
}

// TestChimeraHomeFromTheProjectFile: CHIMERA_HOME may itself come from ./.env, so
// the machine-wide file is located after the project file is applied — otherwise
// the previous home's settings are read next to state resolved under the new one.
func TestChimeraHomeFromTheProjectFile(t *testing.T) {
	clearSettings(t)
	work := t.TempDir()
	projectHome := t.TempDir()
	t.Chdir(work)

	// TestMain exports CHIMERA_HOME for isolation, and an exported value rightly
	// outranks ./.env. Unset it so the project file is what decides here, then
	// put the isolation back for the next test.
	isolated := os.Getenv("CHIMERA_HOME")
	if err := os.Unsetenv("CHIMERA_HOME"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("CHIMERA_HOME", isolated) }()

	writeEnv(t, filepath.Join(projectHome, "env"), "PROVIDER=kimi\n")
	writeEnv(t, filepath.Join(work, ".env"), "CHIMERA_HOME="+projectHome+"\n")

	cfg, src, err := LoadWithSources()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != ProviderKimi {
		t.Fatalf("provider = %q, want kimi from %s/env", cfg.Provider, projectHome)
	}
	if cfg.AuthDir != filepath.Join(projectHome, "auth_data") {
		t.Fatalf("AuthDir = %q, want it under the project-declared home", cfg.AuthDir)
	}
	if got := src.Of("PROVIDER"); got != filepath.Join(projectHome, "env") {
		t.Fatalf("source = %q, want the new home's file", got)
	}
}

// TestHomeDirHonoursChimeraHome pins the one place the directory is decided.
func TestHomeDirHonoursChimeraHome(t *testing.T) {
	t.Setenv("CHIMERA_HOME", "/opt/chimera")
	if got := HomeDir(); got != "/opt/chimera" {
		t.Fatalf("HomeDir() = %q", got)
	}

	// Unset, it falls back to the XDG data dir — and TestMain's value is put
	// back afterwards, so the next test does not start reading this machine's
	// real settings file.
	isolated := os.Getenv("CHIMERA_HOME")
	os.Unsetenv("CHIMERA_HOME")
	defer os.Setenv("CHIMERA_HOME", isolated)

	got := HomeDir()
	if !strings.HasSuffix(got, filepath.Join(".local", "share", "chimera")) {
		t.Fatalf("HomeDir() = %q, want the XDG data dir by default", got)
	}
}
