package main

// Tests for `chimera up`: name resolution, argv[0] dispatch, port selection,
// and which providers count as startable.

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/chimera/chimera/internal/config"
)

func TestResolveProviderName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"kimi", "kimi"},
		{"glm", "glm"},
		{"KIMI", ""}, // callers lowercase; the resolver does not guess
		{"chatgpt", "chatgpt"},
		{"gpt", "chatgpt"}, // the short alias that avoids chimera-chat confusion
		{"chat", "chatgpt"},
		{"all", ""}, // "all" is handled before resolution, never a provider
		{"nope", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := resolveProviderName(tc.in); got != tc.want {
			t.Errorf("resolveProviderName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProviderFromArgv0(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/usr/local/bin/chimera-kimi", "kimi"},
		{"/usr/local/bin/chimera-gpt", "chatgpt"},
		{"/usr/local/bin/chimera-glm", "glm"},
		// The plain gateway and the sibling binaries must never dispatch here:
		// chimera-chat is the TUI and chimera-spin is the harness.
		{"/usr/local/bin/chimera", ""},
		{"/usr/local/bin/chimera-chat", ""},
		{"/usr/local/bin/chimera-spin", ""},
		{"/usr/local/bin/chimera-nope", ""},
		{"chimera-kimi", "kimi"}, // relative invocation
	}
	for _, tc := range cases {
		if got := providerFromArgv0(tc.in); got != tc.want {
			t.Errorf("providerFromArgv0(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFreePortSkipsBusyAndReserved(t *testing.T) {
	const lo, hi = 18400, 18403
	// Occupy the first port in the range.
	ln, err := net.Listen("tcp", ":18400")
	if err != nil {
		t.Skipf("cannot bind %d to set up the test: %v", lo, err)
	}
	defer ln.Close()

	got, err := freePort(lo, hi, nil)
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if got == lo {
		t.Errorf("freePort returned the busy port %d", lo)
	}

	// A reserved port must be skipped even though it is free.
	got2, err := freePort(lo, hi, map[int]bool{got: true})
	if err != nil {
		t.Fatalf("freePort with reservation: %v", err)
	}
	if got2 == got {
		t.Errorf("freePort returned the reserved port %d", got)
	}
}

func TestFreePortExhausted(t *testing.T) {
	// A one-port range that is already reserved must report exhaustion rather
	// than silently handing back a port that is in use.
	if _, err := freePort(18410, 18410, map[int]bool{18410: true}); err == nil {
		t.Error("freePort returned no error for an exhausted range")
	}
}

func TestUpCandidatesNeedsSessionAndTransport(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir}

	// No sessions at all: nothing is startable.
	if got := upCandidates(cfg); len(got) != 0 {
		t.Errorf("upCandidates with no sessions = %v, want none", got)
	}

	// A session for a browserless provider counts; one for a DOM-only provider
	// (claude) does not, because `up` is the no-browser quick start.
	for _, name := range []string{"kimi", "glm", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := upCandidates(cfg)
	want := map[string]bool{"kimi": true, "glm": true}
	if len(got) != len(want) {
		t.Fatalf("upCandidates = %v, want kimi and glm only", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("upCandidates included %q (no browserless transport)", name)
		}
	}
}
