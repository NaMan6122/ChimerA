package main

// init's contract: it writes one settings file in the place the binary already
// looks, absolute paths inside it, a token that is never silently rotated, and
// it touches nothing that already existed in the working directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvFileBodyIsAbsoluteAndComplete(t *testing.T) {
	home := t.TempDir()
	body := envFileBody(initPlan{Home: home, EnvPath: filepath.Join(home, "env"),
		Provider: "deepseek", Token: "tok-123", Port: 8010})

	for _, want := range []string{
		"PROVIDER=deepseek",
		"API_PORT=8010",
		"API_TOKEN=tok-123",
		filepath.Join(home, "auth_data"),
		filepath.Join(home, "browser_data"),
		filepath.Join(home, "logs"),
		filepath.Join(home, "usage.db"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings file missing %q:\n%s", want, body)
		}
	}
	// Relative paths are the bug this file exists to remove: the settings mean
	// the same thing from every directory, or they mean nothing.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "AUTH_DIR=") && !strings.HasPrefix(line, "BROWSER_DATA_DIR=") &&
			!strings.HasPrefix(line, "LOG_DIR=") && !strings.HasPrefix(line, "METER_DB=") {
			continue
		}
		if strings.Contains(line, "=./") || strings.HasSuffix(strings.TrimSpace(line), "=") {
			t.Errorf("relative path in the settings file: %q", line)
		}
	}
	if !strings.Contains(body, "API_HOST=127.0.0.1") {
		t.Error("a new install should not be reachable from the network")
	}
}

// TestWritePlanCreatesAndProtects: the tree exists afterwards, and the settings
// file is owner-only because it holds a credential.
func TestWritePlanCreatesAndProtects(t *testing.T) {
	home := t.TempDir()
	plan := initPlan{Home: home, EnvPath: filepath.Join(home, "env"),
		Provider: "deepseek", Token: "tok", Port: 8000}
	if err := writePlan(plan); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []string{"auth_data", "logs", "browser_data"} {
		if fi, err := os.Stat(filepath.Join(home, sub)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", sub, err)
		}
	}
	fi, err := os.Stat(plan.EnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings file mode = %v, want 0600 (it holds the API token)", perm)
	}

	// Re-running with the same plan is idempotent: same bytes, no error.
	before, _ := os.ReadFile(plan.EnvPath)
	if err := writePlan(plan); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(plan.EnvPath)
	if string(before) != string(after) {
		t.Error("re-running init changed the settings file")
	}
}

// TestTokenForNeverRotatesAWorkingCredential is the rule that keeps existing
// clients working: only a fresh install mints a token.
func TestTokenForNeverRotatesAWorkingCredential(t *testing.T) {
	existing := map[string]string{"API_TOKEN": "already-here"}

	if got, src := tokenFor("", existing); got != "already-here" || src != "kept" {
		t.Errorf("tokenFor(re-run) = %q source=%q, want the existing token", got, src)
	}
	// A token the operator passed is reported as theirs, not as a generated one
	// to be printed back: printing a typed secret adds nothing and copies it
	// into the terminal's scrollback.
	if got, src := tokenFor("typed", existing); got != "typed" || src != "flag" {
		t.Errorf("tokenFor(explicit) = %q source=%q", got, src)
	}
	got, src := tokenFor("", map[string]string{})
	if src != "generated" || len(got) < 32 {
		t.Errorf("tokenFor(fresh) = %q source=%q, want a minted token", got, src)
	}
	if again, _ := tokenFor("", map[string]string{}); again == got {
		t.Error("two minted tokens were identical")
	}
}

// TestProviderWithSession keeps re-initialising from changing what the machine
// serves: one exported session decides, two do not.
func TestProviderWithSession(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(home, "auth_data")
	if err := os.MkdirAll(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := providerWithSession(home); got != "" {
		t.Fatalf("providerWithSession(empty) = %q", got)
	}

	if err := os.WriteFile(filepath.Join(auth, "deepseek.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := providerWithSession(home); got != "deepseek" {
		t.Fatalf("providerWithSession(one) = %q, want deepseek", got)
	}

	if err := os.WriteFile(filepath.Join(auth, "qwen.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := providerWithSession(home); got != "" {
		t.Fatalf("providerWithSession(two) = %q, want no inference", got)
	}
}

// TestReadEnvMapIgnoresCommentsAndSpacing: the file is meant to be edited by
// hand, so parsing it must survive a human.
func TestReadEnvMapIgnoresCommentsAndSpacing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "# a comment\n\nPROVIDER = deepseek\nAPI_TOKEN=abc  \nnonsense line\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := readEnvMap(path)
	if !ok {
		t.Fatal("readEnvMap reported the file missing")
	}
	if got["PROVIDER"] != "deepseek" {
		t.Errorf("PROVIDER = %q", got["PROVIDER"])
	}
	if got["API_TOKEN"] != "abc" {
		t.Errorf("API_TOKEN = %q", got["API_TOKEN"])
	}
	if _, ok := readEnvMap(filepath.Join(dir, "absent")); ok {
		t.Error("readEnvMap reported a missing file as present")
	}
}

// TestPortFromKeepsTheRunningPort: -force regenerates the file, it does not move
// the gateway off the port clients already point at.
func TestPortFromKeepsTheRunningPort(t *testing.T) {
	existing := map[string]string{"API_PORT": "8010"}
	if got := portFrom(existing, 0); got != 8010 {
		t.Errorf("portFrom(no flag) = %d, want the existing 8010", got)
	}
	if got := portFrom(existing, 8020); got != 8020 {
		t.Errorf("portFrom(flag) = %d, want the flag's 8020", got)
	}
	for _, bad := range []string{"", "not-a-port", "0", "99999"} {
		if got := portFrom(map[string]string{"API_PORT": bad}, 0); got != 8000 {
			t.Errorf("portFrom(%q) = %d, want the 8000 default", bad, got)
		}
	}
}

// TestExtraBodyPreservesUnmanagedSettings is the -force safety net: dropping
// API_TOKENS would reopen a gateway that was authenticated, and dropping a quota
// would lift a limit somebody set deliberately.
func TestExtraBodyPreservesUnmanagedSettings(t *testing.T) {
	if got := extraBody(nil); got != "" {
		t.Errorf("extraBody(nil) = %q, want nothing", got)
	}
	body := extraBody(map[string]string{
		"API_TOKENS":             "acme=a1",
		"DEEPSEEK_WEB_MODEL":     "deepseek-reasoner",
		"QUOTA_MONTHLY_REQUESTS": "500",
	})
	for _, want := range []string{"API_TOKENS=acme=a1", "DEEPSEEK_WEB_MODEL=deepseek-reasoner", "QUOTA_MONTHLY_REQUESTS=500", "does not manage"} {
		if !strings.Contains(body, want) {
			t.Errorf("preserved block missing %q:\n%s", want, body)
		}
	}
	// Sorted, so two rewrites of the same file produce the same bytes.
	if strings.Index(body, "API_TOKENS") > strings.Index(body, "DEEPSEEK_WEB_MODEL") {
		t.Error("preserved keys are not sorted")
	}
}

// TestAdoptLegacyStateCopiesWorkingDirectory: writing absolute home paths ends
// the legacy fallback, so init must carry the sessions across or an upgrade
// silently loses them. Copies, never moves: a gateway may still be running.
func TestAdoptLegacyStateCopiesWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	t.Chdir(work)

	if err := os.MkdirAll("./auth_data", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("./auth_data/deepseek.json", []byte(`{"token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := "./browser_data/deepseek/Default/Local Storage"
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "state.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("./browser_data/deepseek/DevToolsActivePort", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	adopted := adoptLegacyState(initPlan{Home: home, Provider: "deepseek"})
	if len(adopted) != 2 {
		t.Fatalf("adopted = %v, want a session and a profile", adopted)
	}

	dst := filepath.Join(home, "auth_data", "deepseek.json")
	if raw, err := os.ReadFile(dst); err != nil || string(raw) != `{"token":"t"}` {
		t.Fatalf("session not copied: %v", err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("copied session mode = %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, "browser_data", "deepseek", "Default", "Local Storage", "state.log")); err != nil {
		t.Errorf("profile not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "browser_data", "deepseek", "DevToolsActivePort")); err == nil {
		t.Error("stale DevToolsActivePort travelled with the profile")
	}
	// The source is left alone: the old install keeps working.
	if _, err := os.Stat("./auth_data/deepseek.json"); err != nil {
		t.Errorf("source session was removed: %v", err)
	}

	// Re-running adopts nothing twice and does not overwrite what is there.
	if got := adoptLegacyState(initPlan{Home: home, Provider: "deepseek"}); len(got) != 0 {
		t.Errorf("second adopt = %v, want nothing", got)
	}
}

// TestWritePlanTightensAnExistingFile: os.WriteFile keeps the mode of a file that
// already exists, so a world-readable settings file would stay world-readable
// while holding a token.
func TestWritePlanTightensAnExistingFile(t *testing.T) {
	home := t.TempDir()
	plan := initPlan{Home: home, EnvPath: filepath.Join(home, "env"),
		Provider: "deepseek", Token: "tok", Port: 8000}
	if err := os.WriteFile(plan.EnvPath, []byte("OLD=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plan.EnvPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePlan(plan); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(plan.EnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestProviderListCoversEveryServableProvider: init must not offer "all", which
// is a routing mode rather than something auth can log in to.
func TestProviderListCoversEveryServableProvider(t *testing.T) {
	list := providerList()
	if strings.Contains(list, "all") {
		t.Errorf("provider list offers the 'all' routing mode: %s", list)
	}
	for _, p := range []string{"chatgpt", "deepseek", "qwen"} {
		if !strings.Contains(list, p) {
			t.Errorf("provider list missing %q: %s", p, list)
		}
	}
	if !isSupportedProvider("DEEPSEEK") {
		t.Error("provider names should compare case-insensitively")
	}
}
