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

	if got, gen := tokenFor("", existing); got != "already-here" || gen {
		t.Errorf("tokenFor(re-run) = %q generated=%v, want the existing token", got, gen)
	}
	if got, gen := tokenFor("typed", existing); got != "typed" || gen {
		t.Errorf("tokenFor(explicit) = %q generated=%v", got, gen)
	}
	got, gen := tokenFor("", map[string]string{})
	if !gen || len(got) < 32 {
		t.Errorf("tokenFor(fresh) = %q generated=%v, want a minted token", got, gen)
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
