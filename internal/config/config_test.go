package config

import (
	"os"
	"path/filepath"
	"testing"
)

func testEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PROVIDER", "chatgpt")
	t.Setenv("BROWSER_DATA_DIR", filepath.Join(dir, "browser_data"))
	t.Setenv("LOG_DIR", filepath.Join(dir, "logs"))
	t.Setenv("API_TOKEN", "")
	t.Setenv("API_TOKENS", "")
	t.Setenv("METER_DB", "")
}

func TestLoadParsesTenantKeys(t *testing.T) {
	dir := t.TempDir()
	testEnv(t, dir)
	t.Setenv("API_TOKEN", "sek")
	t.Setenv("API_TOKENS", "acme=a1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeys == nil || cfg.APIKeys.IsOpen() {
		t.Fatal("expected closed registry")
	}
}

func TestLoadRejectsMalformedTokens(t *testing.T) {
	dir := t.TempDir()
	testEnv(t, dir)
	t.Setenv("API_TOKENS", "acme") // missing =token
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to fail on malformed API_TOKENS")
	}
}

func TestLoadMeteringDefaults(t *testing.T) {
	dir := t.TempDir()
	testEnv(t, dir)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeterDB != "" {
		t.Fatalf("expected metering disabled, got %q", cfg.MeterDB)
	}
	if cfg.QuotaMonthlyRequests != 0 {
		t.Fatalf("expected unlimited quota, got %d", cfg.QuotaMonthlyRequests)
	}
	if !cfg.APIKeys.IsOpen() {
		t.Fatal("expected open gateway with no tokens")
	}

	// Unset → default path under the log dir.
	if err := os.Unsetenv("METER_DB"); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg2.LogDir, "usage.db")
	if cfg2.MeterDB != want {
		t.Fatalf("default MeterDB = %q, want %q", cfg2.MeterDB, want)
	}
}
