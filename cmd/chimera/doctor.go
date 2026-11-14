package main

// doctor.go — `chimera doctor`: a local health check for a Chimera install.
//
// Everything here runs on local disk and localhost. Only --probe reaches a
// listening gateway's /v1/health/providers (with API_TOKEN when set), and
// only to read it. Sessions are summarized by NAME and EXPIRY only — cookie
// values and tokens are never echoed, which is asserted by test.
//
// Checks report OK / WARN / FIX with a concrete hint; a FIX turns the exit
// status 1 so scripts can gate on it.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/chimera/chimera/internal/sessions"
	"github.com/chimera/chimera/internal/spin"
)

const doctorUsage = `Usage: chimera doctor [--probe] [-dir <sessions-dir>]

Local health check for this Chimera install: binary version, sessions
transcripts, auth_data session files (names and expiry only — secret values
are never printed), gateway ports 8000-8015, pdftotext availability, disk
space, and .env key sanity. Local disk and localhost only.

Flags:
  --probe   additionally ask each listening gateway for /v1/health/providers
            (sends API_TOKEN when it is set) and print per-provider state
  -dir      sessions (transcript) directory (default: $CHIMERA_SESSIONS_DIR,
            else ~/.local/share/chimera/sessions)

Exit status is 1 when any check reports FIX.
`

// checkStatus is the verdict of one doctor check.
type checkStatus string

const (
	statusOK   checkStatus = "OK"
	statusWARN checkStatus = "WARN"
	statusFIX  checkStatus = "FIX"
)

// checkResult is one check's outcome. Detail is safe to print: it must
// never contain credential material (enforced by doctor_test.go).
type checkResult struct {
	Name   string
	Status checkStatus
	Detail string
	Hint   string // concrete next action, for WARN/FIX
}

// doctorOptions carries the tunables so checks stay pure and testable.
type doctorOptions struct {
	Probe       bool
	SessionsDir string
	DotEnvPath  string
	Ports       []int
}

// doctorScanPorts is the gateway port range doctor scans (localhost only).
func doctorScanPorts() []int {
	ports := make([]int, 0, 16)
	for p := 8000; p <= 8015; p++ {
		ports = append(ports, p)
	}
	return ports
}

// runDoctor handles `chimera doctor ...` and exits.
func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	probe := fs.Bool("probe", false, "also query each listening gateway's /v1/health/providers")
	dir := fs.String("dir", spin.DefaultSessionsDir(), "sessions (transcript) directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, doctorUsage)
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", fs.Arg(0), doctorUsage)
		os.Exit(2)
	}

	// config.Load validates env and creates the data directories the same
	// way the gateway would; a failure is itself a finding, not a crash.
	cfg, cfgErr := config.Load()

	results := doctorResults(doctorOptions{
		Probe:       *probe,
		SessionsDir: *dir,
		DotEnvPath:  ".env",
		Ports:       doctorScanPorts(),
	}, cfg, cfgErr)

	fmt.Print(renderDoctor(results))
	for _, r := range results {
		if r.Status == statusFIX {
			os.Exit(1)
		}
	}
}

// doctorResults runs every check. cfg may be nil when loading failed; the
// checks then fall back to the default paths so they can still report.
func doctorResults(opts doctorOptions, cfg *config.Config, cfgErr error) []checkResult {
	var out []checkResult
	out = append(out, checkVersion())

	if cfgErr != nil {
		out = append(out, checkResult{Name: "config", Status: statusFIX,
			Detail: "loading configuration failed: " + cfgErr.Error(),
			Hint:   "fix the offending environment value or .env key, then re-run"})
	}

	authDir, logDir, browserDir := "./auth_data", "./logs", "./browser_data"
	if cfg != nil {
		authDir, logDir, browserDir = cfg.AuthDir, cfg.LogDir, cfg.BrowserDataDir
	}

	out = append(out, checkSessionsDir(opts.SessionsDir))
	out = append(out, checkProviderSessions(authDir, sessionProviderNames())...)
	out = append(out, checkGatewayPorts(opts.Ports, opts.Probe, apiTokenForProbe())...)
	out = append(out, checkPdfTool(exec.LookPath))
	out = append(out, checkDiskSpace("browser_data", browserDir))
	out = append(out, checkDiskSpace("logs", logDir))
	out = append(out, checkEnvFile(opts.DotEnvPath))
	return out
}

// sessionProviderNames is the set of providers whose auth_data session file
// matters: every registered browserless transport plus every provider
// `chimera auth` can log in to. The union is deliberate — internal/providers/
// webapi/all does not blank-import every transport (grok today), and a
// registry-only list would silently skip a session file doctor should see.
func sessionProviderNames() []string {
	seen := map[string]bool{}
	for _, n := range webapi.Registered() {
		seen[n] = true
	}
	for n := range authSpecs {
		seen[n] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// apiTokenForProbe returns the bearer token for --probe, never printing it.
// config.Load has already folded .env into the process env, so both the
// shell export and the .env value are visible here.
func apiTokenForProbe() string {
	return os.Getenv("API_TOKEN")
}

// ── checks ──────────────────────────────────────────────────

func checkVersion() checkResult {
	detail := "chimera " + version
	if version == "" || version == "dev" {
		detail += " (not stamped — source build; install.sh stamps the release version)"
	}
	return checkResult{Name: "version", Status: statusOK, Detail: detail}
}

// checkSessionsDir verifies the transcripts directory is writable and
// counts the recorded sessions.
func checkSessionsDir(dir string) checkResult {
	const name = "sessions"
	if fi, err := os.Stat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return checkResult{name, statusWARN, "cannot stat " + dir + ": " + err.Error(), "check the path: chimera sessions list -dir " + dir}
		}
		return checkResult{name, statusWARN, dir + " does not exist yet",
			"created on the first chimera-spin run; point doctor elsewhere with -dir"}
	} else if !fi.IsDir() {
		return checkResult{name, statusFIX, dir + " exists but is not a directory",
			"move or remove " + dir + " — transcripts live in a directory"}
	}
	probe := filepath.Join(dir, ".doctor-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return checkResult{name, statusFIX, dir + " is not writable: " + err.Error(),
			"chmod u+rwx " + dir}
	}
	f.Close()
	_ = os.Remove(probe)
	sums, err := sessions.List(dir)
	if err != nil {
		return checkResult{name, statusWARN, dir + " is writable but unreadable: " + err.Error(),
			"check the *.jsonl files in " + dir}
	}
	return checkResult{name, statusOK,
		fmt.Sprintf("%s — writable, %d transcript(s)", dir, len(sums)), ""}
}

// checkProviderSessions inspects every browserless transport's session file
// through the same loader the gateway uses. Only counts, paths and expiry
// are reported — LoadSession's errors quote the path, never the values.
func checkProviderSessions(authDir string, names []string) []checkResult {
	if len(names) == 0 {
		return []checkResult{{Name: "auth", Status: statusOK,
			Detail: "no browserless transports registered (DOM-only build)"}}
	}
	out := make([]checkResult, 0, len(names))
	for _, p := range names {
		path := filepath.Join(authDir, p+".json")
		sess, err := webapi.LoadSession(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				out = append(out, checkResult{"auth/" + p, statusWARN,
					"no session file at " + path, "run: chimera auth login " + p})
				continue
			}
			out = append(out, checkResult{"auth/" + p, statusFIX,
				"session file unreadable: " + err.Error(),
				"re-export the session: chimera auth login " + p})
			continue
		}
		st := sess.Status(path)
		cookies := fmt.Sprintf("%d cookie(s)", st.Cookies)
		switch {
		case st.HasExpiry && st.DaysRemain < 0:
			out = append(out, checkResult{"auth/" + p, statusFIX,
				fmt.Sprintf("%s; token expired %.1f day(s) ago", cookies, -st.DaysRemain),
				"run: chimera auth login " + p})
		case st.HasExpiry && st.DaysRemain < 7:
			out = append(out, checkResult{"auth/" + p, statusWARN,
				fmt.Sprintf("%s; token expires %s (%.1f days)", cookies, st.ExpiresAt.Format("2006-01-02"), st.DaysRemain),
				"run: chimera auth login " + p})
		case st.HasExpiry:
			out = append(out, checkResult{"auth/" + p, statusOK,
				fmt.Sprintf("%s; token expires %s (%.0f days)", cookies, st.ExpiresAt.Format("2006-01-02"), st.DaysRemain), ""})
		default:
			out = append(out, checkResult{"auth/" + p, statusOK,
				cookies + "; token expiry unknown (cookie-custody marker)", ""})
		}
	}
	return out
}

// timeouts for the localhost probes; a dead-but-open port must not hang doctor.
const (
	portDialTimeout    = 250 * time.Millisecond
	gatewayHTTPTimeout = 1500 * time.Millisecond
)

// checkGatewayPorts reports which local ports are listening, whether their
// /health answers, and — with probe — each gateway's per-provider state.
// A quiet port is not a finding; the summary line reports the range.
func checkGatewayPorts(ports []int, probe bool, token string) []checkResult {
	if len(ports) == 0 {
		return nil
	}
	var out []checkResult
	listening := 0
	for _, port := range ports {
		res, up := probeGatewayPort(port, probe, token)
		if !up {
			continue
		}
		listening++
		out = append(out, res...)
	}
	if listening == 0 {
		return []checkResult{{Name: "ports", Status: statusOK,
			Detail: fmt.Sprintf("no gateway listening on :%d-%d (start one with `chimera`)", ports[0], ports[len(ports)-1])}}
	}
	return out
}

// probeGatewayPort checks one port. ok=false means nothing was listening.
func probeGatewayPort(port int, doProbe bool, token string) ([]checkResult, bool) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, portDialTimeout)
	if err != nil {
		return nil, false // nothing listening — normal, not a finding
	}
	_ = conn.Close()

	client := &http.Client{Timeout: gatewayHTTPTimeout}
	code, err := httpStatus(client, "http://"+addr+"/health", "")
	if err != nil {
		return []checkResult{{Name: "ports", Status: statusWARN,
			Detail: fmt.Sprintf(":%d listening, but /health unreachable: %v", port, err),
			Hint:   "is " + addr + " really a Chimera gateway? check with: pgrep -fl chimera"}}, true
	}
	var out []checkResult
	if code == "200" {
		out = append(out, checkResult{Name: "ports", Status: statusOK,
			Detail: fmt.Sprintf(":%d listening; /health %s", port, code)})
	} else {
		out = append(out, checkResult{Name: "ports", Status: statusWARN,
			Detail: fmt.Sprintf(":%d listening; /health returned %s", port, code),
			Hint:   fmt.Sprintf("expected a Chimera gateway — check what owns the port: lsof -nP -iTCP:%d -sTCP:LISTEN", port)})
	}
	if doProbe {
		out = append(out, probeProvidersHealth(client, addr, port, token))
	}
	return out, true
}

// probeProvidersHealth reads one gateway's per-provider health (the same
// endpoint status pages use) and renders it. Token usage is silent.
func probeProvidersHealth(client *http.Client, addr string, port int, token string) checkResult {
	const name = "probe"
	req, err := http.NewRequest("GET", "http://"+addr+"/v1/health/providers", nil)
	if err != nil {
		return checkResult{name, statusWARN, fmt.Sprintf(":%d: %v", port, err), ""}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return checkResult{name, statusWARN,
			fmt.Sprintf(":%d /v1/health/providers unreachable: %v", port, err), ""}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return checkResult{name, statusWARN,
			fmt.Sprintf(":%d /v1/health/providers unauthorized", port),
			"export API_TOKEN (the value the gateway was started with) before `chimera doctor --probe`"}
	}
	if resp.StatusCode != 200 {
		return checkResult{name, statusWARN,
			fmt.Sprintf(":%d /v1/health/providers returned %d", port, resp.StatusCode), ""}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return checkResult{name, statusWARN, fmt.Sprintf(":%d reading body: %v", port, err), ""}
	}
	var payload struct {
		Data []struct {
			Provider string `json:"provider"`
			Up       bool   `json:"up"`
			LoggedIn bool   `json:"logged_in"`
			Quota    *struct {
				Remaining int    `json:"remaining"`
				Total     *int   `json:"total"`
				Window    string `json:"window"`
			} `json:"quota"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return checkResult{name, statusWARN,
			fmt.Sprintf(":%d did not return provider_health JSON", port), ""}
	}
	var b strings.Builder
	fmt.Fprintf(&b, ":%d /v1/health/providers — %d provider(s)", port, len(payload.Data))
	for _, d := range payload.Data {
		fmt.Fprintf(&b, "\n  %-10s %s", d.Provider, upDown(d.Up, d.LoggedIn))
		if d.Quota != nil {
			fmt.Fprintf(&b, "  quota remaining=%d", d.Quota.Remaining)
			if d.Quota.Total != nil {
				fmt.Fprintf(&b, "/%d", *d.Quota.Total)
			}
			if d.Quota.Window != "" {
				fmt.Fprintf(&b, " window=%s", d.Quota.Window)
			}
		}
	}
	return checkResult{name, statusOK, b.String(), ""}
}

// upDown renders one provider's probe state.
func upDown(up, loggedIn bool) string {
	switch {
	case up && loggedIn:
		return "up"
	case up:
		return "up (logged_out)"
	default:
		return "down"
	}
}

// httpStatus does a GET and returns the status code string ("200", "404"...).
func httpStatus(client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return strconv.Itoa(resp.StatusCode), nil
}

// checkPdfTool reports pdftotext availability (used by PDF text extraction).
// A missing tool degrades a feature; it never blocks the gateway, so WARN.
func checkPdfTool(lookPath func(string) (string, error)) checkResult {
	const name = "pdftotext"
	path, err := lookPath(name)
	if err != nil {
		return checkResult{name, statusWARN,
			"pdftotext not found — PDF text extraction is unavailable",
			pdfInstallHint(runtime.GOOS)}
	}
	return checkResult{name, statusOK, "found: " + path, ""}
}

// pdfInstallHint is the per-platform install line for poppler's pdftotext.
func pdfInstallHint(goos string) string {
	switch goos {
	case "darwin":
		return "install poppler: brew install poppler"
	case "windows":
		return "install poppler: choco install poppler (or add a poppler release bin/ to PATH)"
	default:
		return "install poppler — Debian/Ubuntu: sudo apt install poppler-utils · Fedora: sudo dnf install poppler-utils · Arch: sudo pacman -S poppler"
	}
}

// disk thresholds: Chromium profiles and logs grow without bound, and a
// full disk reads as mysterious gateway crashes.
const (
	diskFixBytes  = 1 << 30 // 1 GiB
	diskWarnBytes = 5 << 30 // 5 GiB
)

func checkDiskSpace(label, dir string) checkResult {
	const name = "disk"
	if runtime.GOOS == "windows" {
		return checkResult{name, statusWARN, "skipped on windows (not implemented yet)", ""}
	}
	free, err := freeBytesFor(dir)
	if err != nil {
		return checkResult{name, statusWARN,
			fmt.Sprintf("%s (%s): cannot stat: %v", label, dir, err),
			"create the directory first: mkdir -p " + dir}
	}
	r := checkResult{name, statusOK,
		fmt.Sprintf("%s (%s): %s free", label, dir, humanBytes(free)), ""}
	switch {
	case free < diskFixBytes:
		r.Status = statusFIX
		r.Hint = fmt.Sprintf("free space on %s — remove old browser_data profiles or logs, or expand the disk", label)
	case free < diskWarnBytes:
		r.Status = statusWARN
		r.Hint = "under 5 GiB free — browser profiles and logs grow; clean up soon"
	}
	return r
}

// humanBytes renders a byte count for humans.
func humanBytes(n uint64) string {
	const kb, mb, gb = 1 << 10, 1 << 20, 1 << 30
	switch {
	case n >= 10*gb:
		return fmt.Sprintf("%.0f GB", float64(n)/gb)
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.0f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%d KB", n/kb)
	}
}

// knownEnvKeys is every variable name the gateway reads (internal/config,
// the sessions dir override, and the two debug knobs). Doctor compares key
// NAMES only; values are never echoed — several are credentials.
var knownEnvKeys = map[string]bool{
	// provider + browser
	"PROVIDER": true, "BROWSER_DATA_DIR": true, "HEADLESS": true, "SLOW_MO": true,
	// transport + auth
	"TRANSPORT": true, "TRANSPORT_FALLBACK": true, "AUTH_DIR": true,
	// webapi models
	"QWEN_WEB_MODEL": true, "QWEN_WEB_THINKING": true,
	"DEEPSEEK_WEB_MODEL": true, "DEEPSEEK_WEB_THINKING": true,
	"CHATGPT_WEB_MODEL": true,
	"GEMINI_WEB_MODEL":  true, "GEMINI_WEB_THINKING": true,
	"GROK_WEB_MODEL": true,
	"GLM_WEB_MODEL":  true,
	"KIMI_WEB_MODEL": true, "KIMI_WEB_URL": true,
	// provider URLs
	"CHATGPT_URL": true, "CLAUDE_URL": true, "QWEN_URL": true,
	"DEEPSEEK_URL": true, "KIMI_URL": true, "GEMINI_URL": true, "GROK_URL": true,
	"GLM_URL": true,
	// timeouts + human simulation
	"RESPONSE_TIMEOUT": true, "SELECTOR_TIMEOUT": true,
	"TYPING_SPEED_MIN": true, "TYPING_SPEED_MAX": true,
	"THINKING_PAUSE_MIN": true, "THINKING_PAUSE_MAX": true,
	// logging
	"LOG_DIR": true, "LOG_LEVEL": true, "LOG_FORMAT": true, "VERBOSE": true,
	// API server
	"API_HOST": true, "API_PORT": true, "RATE_LIMIT_SECONDS": true,
	"API_TOKEN": true, "API_TOKENS": true,
	"MAX_CONCURRENT_PER_PROVIDER": true, "MAX_PROMPT_CHARS": true,
	// metering
	"METER_DB": true, "QUOTA_MONTHLY_REQUESTS": true,
	// misc
	"VNC_PASSWORD":         true,
	"SESSIONS_DIR":         true,
	"CHIMERA_SESSIONS_DIR": true,
	"CHROME_BIN":           true,
	"CHATGPT_DUMP":         true,
	"DEEPSEEK_SSE_DUMP":    true,
	"QWEN_DUMP":            true,
	"GEMINI_DUMP":          true,
	"GROK_DUMP":            true,
	"GLM_DUMP":             true,
	"KIMI_DUMP":            true,
}

// checkEnvFile reads .env and sanity-checks key names. A typo'd key is
// silently ignored by the gateway — surfacing it here turns a mystery into
// a one-line fix. Values are never echoed.
func checkEnvFile(path string) checkResult {
	const name = "env"
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkResult{name, statusOK, "no .env file (defaults apply)", ""}
		}
		return checkResult{name, statusWARN, "cannot read " + path + ": " + err.Error(),
			"check file permissions on " + path}
	}
	known := 0
	var unknown []string
	for _, line := range strings.Split(string(raw), "\n") {
		key, ok := envKeyOf(line)
		if !ok {
			continue
		}
		if knownEnvKeys[key] {
			known++
		} else {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return checkResult{name, statusWARN,
			fmt.Sprintf("%s: %d known key(s), %d unrecognized: %s (values never echoed)",
				path, known, len(unknown), strings.Join(unknown, ", ")),
			"unrecognized keys are ignored — check spelling against the key list in `chimera doctor` docs or remove them"}
	}
	return checkResult{name, statusOK,
		fmt.Sprintf("%s: %d known key(s), values never echoed", path, known), ""}
}

// envKeyOf extracts the variable name from a .env line, tolerating comments,
// blank lines, an `export ` prefix and quoted names. It never returns a value.
func envKeyOf(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	line = strings.TrimPrefix(line, "export ")
	i := strings.Index(line, "=")
	if i <= 0 {
		return "", false
	}
	key := strings.Trim(strings.TrimSpace(line[:i]), `"'`)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", false
	}
	return key, true
}

// ── rendering ──────────────────────────────────────────────────

// renderDoctor prints one line per check: status, check id, detail; WARN
// and FIX lines get an indented, concrete hint. Multi-line details (the
// probe listing) are embedded in Detail and printed as-is.
func renderDoctor(results []checkResult) string {
	var b strings.Builder
	b.WriteString("chimera doctor\n\n")
	fix, warn := 0, 0
	for _, r := range results {
		fmt.Fprintf(&b, "%-4s %-12s %s\n", string(r.Status), r.Name, r.Detail)
		if r.Hint != "" {
			fmt.Fprintf(&b, "     → %s\n", r.Hint)
		}
		switch r.Status {
		case statusFIX:
			fix++
		case statusWARN:
			warn++
		}
	}
	b.WriteString("\n")
	switch {
	case fix > 0 && warn > 0:
		fmt.Fprintf(&b, "%d fix(es), %d warning(s) — items marked FIX need action.\n", fix, warn)
	case fix > 0:
		fmt.Fprintf(&b, "%d fix(es) — items marked FIX need action.\n", fix)
	case warn > 0:
		fmt.Fprintf(&b, "no fixes needed, %d warning(s).\n", warn)
	default:
		b.WriteString("all checks passed.\n")
	}
	return b.String()
}
