package main

// `chimera up` — start one backend, or every backend you have a session for,
// without remembering PROVIDER/TRANSPORT/API_PORT.
//
// The gateway serves exactly one provider per process, and chimera-chat
// discovers backends by probing localhost:8000-8011. So "all backends" means
// one child process per provider, each on the first free port in that range —
// which is also the range the TUI already scans, so nothing downstream changes.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// The port range chimera-chat probes (cmd/chimera-chat portRange(8000, 8011)).
const (
	upPortLo = 8000
	upPortHi = 8011
)

// providerAliases maps the short names a user is likely to type onto the
// provider identifiers. "gpt" exists because `chimera-chatgpt` sits one
// keystroke away from `chimera-chat` (the TUI) — the short form is safer.
var providerAliases = map[string]string{
	"gpt":     config.ProviderChatGPT,
	"chatgpt": config.ProviderChatGPT,
	"chat":    config.ProviderChatGPT, // only reachable via `chimera up chat`
}

// runUp handles `chimera up [provider|all]`. It returns true when the caller
// should continue into the normal gateway path (a single provider, with the
// environment prepared), and false when it already did the work.
func runUp(args []string) bool {
	target := "all"
	if len(args) > 0 {
		target = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if target == "all" {
		runUpAll()
		return false
	}

	name := resolveProviderName(target)
	if name == "" {
		fmt.Fprintf(os.Stderr, "unknown provider %q\n\n", target)
		fmt.Fprintf(os.Stderr, "  chimera up            start every backend with a session\n")
		fmt.Fprintf(os.Stderr, "  chimera up <provider> start one: %s\n", strings.Join(upProviderNames(), ", "))
		os.Exit(2)
	}

	// A single provider runs in this process: no supervision needed.
	port, err := freePort(upPortLo, upPortHi, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chimera up: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("PROVIDER", name)
	if os.Getenv("API_PORT") == "" {
		os.Setenv("API_PORT", strconv.Itoa(port))
	}
	// Prefer the browserless transport when one exists; DOM-only providers
	// (claude) keep the default so they still launch a browser.
	if _, ok := webapi.Lookup(name); ok && os.Getenv("TRANSPORT") == "" {
		os.Setenv("TRANSPORT", "webapi")
	}
	return true
}

// runUpAll starts one child gateway per provider that has both a browserless
// transport and an exported session, then supervises them until interrupted.
func runUpAll() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "chimera up: %v\n", err)
		os.Exit(1)
	}
	names := upCandidates(cfg)
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "chimera up: no backend has an exported session")
		fmt.Fprintln(os.Stderr, "  export one first:  chimera auth login <provider>")
		os.Exit(1)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "chimera up: cannot locate own binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("==> starting %d backend(s)\n", len(names))
	var (
		children []*exec.Cmd
		used     = map[int]bool{}
	)
	for _, name := range names {
		port, err := freePort(upPortLo, upPortHi, used)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-10s skipped: %v\n", name, err)
			continue
		}
		used[port] = true
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(),
			"PROVIDER="+name,
			"API_PORT="+strconv.Itoa(port),
			"TRANSPORT=webapi",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "  %-10s failed to start: %v\n", name, err)
			continue
		}
		children = append(children, cmd)
		fmt.Printf("    %-10s :%d  %s\n", name, port, "chimera-"+name)
	}
	if len(children) == 0 {
		fmt.Fprintln(os.Stderr, "chimera up: nothing started")
		os.Exit(1)
	}
	fmt.Printf("\n  chimera-chat            # the TUI discovers all of them\n")
	fmt.Printf("  Ctrl-C                  # stops every backend\n\n")

	// Report a child that dies on its own (bad session, port stolen) instead
	// of letting the supervisor sit there looking healthy.
	exited := make(chan string, len(children))
	for i, cmd := range children {
		go func(name string, cmd *exec.Cmd) {
			_ = cmd.Wait()
			exited <- name
		}(names[i], cmd)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		fmt.Println("\n==> stopping backends")
	case name := <-exited:
		fmt.Printf("\n==> backend %q exited; stopping the rest\n", name)
	}
	for _, cmd := range children {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
	}
	// Give them a moment to close cleanly, then insist.
	done := make(chan struct{})
	go func() {
		for range children {
			<-exited
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		for _, cmd := range children {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}
}

// upCandidates lists the providers `chimera up` can start: a registered
// browserless transport plus an exported session file.
func upCandidates(cfg *config.Config) []string {
	var out []string
	for _, name := range config.SupportedProviders {
		if name == config.ProviderAll {
			continue
		}
		if _, ok := webapi.Lookup(name); !ok {
			continue // DOM-only: needs a browser, not part of the quick start
		}
		if _, err := os.Stat(cfg.SessionPath(name)); err != nil {
			continue
		}
		out = append(out, name)
	}
	return out
}

// upProviderNames lists the names `chimera up <provider>` accepts.
func upProviderNames() []string {
	var out []string
	for _, name := range config.SupportedProviders {
		if name != config.ProviderAll {
			out = append(out, name)
		}
	}
	return out
}

// resolveProviderName maps a user-typed name (or alias) onto a provider id.
func resolveProviderName(arg string) string {
	if alias, ok := providerAliases[arg]; ok {
		return alias
	}
	for _, name := range config.SupportedProviders {
		if name == arg && name != config.ProviderAll {
			return name
		}
	}
	return ""
}

// freePort returns the lowest port in [lo, hi] that is not already reserved
// and can be bound right now. It probes the WILDCARD address because that is
// what the gateway binds: on macOS a listener on 0.0.0.0:8000 does not stop
// 127.0.0.1:8000 from binding, so a loopback probe happily hands out a port
// that is already serving.
func freePort(lo, hi int, used map[int]bool) (int, error) {
	for p := lo; p <= hi; p++ {
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", lo, hi)
}

// providerFromArgv0 turns `chimera-kimi` (a symlink to this binary) into
// "kimi". It returns "" for the plain `chimera` name and for the sibling
// binaries, which are separate programs and never dispatch here.
func providerFromArgv0(argv0 string) string {
	base := filepath.Base(argv0)
	if !strings.HasPrefix(base, "chimera-") {
		return ""
	}
	suffix := strings.TrimPrefix(base, "chimera-")
	switch suffix {
	case "", "chat", "spin": // chimera-chat and chimera-spin are their own binaries
		return ""
	}
	return resolveProviderName(suffix)
}
