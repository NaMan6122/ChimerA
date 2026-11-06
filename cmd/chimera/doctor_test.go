package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// ── sessions dir check ──────────────────────────────────────────────────

func TestDoctorSessionsDirMissingIsWarn(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	r := checkSessionsDir(missing)
	if r.Status != statusWARN {
		t.Fatalf("missing dir should WARN, got %s (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Hint, "chimera-spin") {
		t.Fatalf("hint should name the creating command, got %q", r.Hint)
	}
}

func TestDoctorSessionsDirEmptyIsOKZero(t *testing.T) {
	r := checkSessionsDir(t.TempDir())
	if r.Status != statusOK || !strings.Contains(r.Detail, "0 transcript(s)") {
		t.Fatalf("empty dir should be OK with 0 transcripts, got %s (%s)", r.Status, r.Detail)
	}
}

func TestDoctorSessionsDirCountsTranscripts(t *testing.T) {
	dir := t.TempDir()
	line := `{"ts":"2026-01-01T00:00:00Z","kind":"session","session":"s1","model":"chimera-x"}`
	for _, id := range []string{"s1", "s2"} {
		body := strings.Replace(line, "s1", id, 1) + "\n"
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-transcript.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkSessionsDir(dir)
	if r.Status != statusOK || !strings.Contains(r.Detail, "2 transcript(s)") {
		t.Fatalf("expected OK with 2 transcripts, got %s (%s)", r.Status, r.Detail)
	}
}

func TestDoctorSessionsDirUnwritableIsFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anything; chmod-based test would not hold")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	r := checkSessionsDir(dir)
	if r.Status != statusFIX {
		t.Fatalf("unwritable dir should be FIX, got %s (%s)", r.Status, r.Detail)
	}
	if r.Hint == "" {
		t.Fatal("FIX must carry a concrete hint")
	}
}

// ── auth session checks: shapes + secret hygiene ────────────────────────

// jwtWithExp builds an unsigned JWT whose payload carries only exp — enough
// for webapi.tokenExpiry to read the claim.
func jwtWithExp(exp int64) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return head + "." + payload + "."
}

func TestDoctorSessionExpirySurfaced(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	cases := []struct {
		name string
		tok  string
		want checkStatus
	}{
		{"expired", jwtWithExp(now - 72*3600), statusFIX},
		{"expiring soon", jwtWithExp(now + 48*3600), statusWARN},
		{"healthy", jwtWithExp(now + 30*24*3600), statusOK},
	}
	for i, tc := range cases {
		provider := fmt.Sprintf("p%d", i)
		sess := &webapi.Session{AccessToken: tc.tok, Cookies: map[string]string{"sso": "x"}}
		if err := webapi.WriteSession(sess, filepath.Join(dir, provider+".json")); err != nil {
			t.Fatal(err)
		}
		results := checkProviderSessions(dir, []string{provider})
		if len(results) != 1 || results[0].Status != tc.want {
			t.Fatalf("%s: want %s, got %+v", tc.name, tc.want, results)
		}
		if results[0].Status != statusOK && results[0].Hint == "" {
			t.Fatalf("%s: WARN/FIX must carry a hint", tc.name)
		}
	}
}

func TestDoctorSessionMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "qwen.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := checkProviderSessions(dir, []string{"qwen", "grok"})
	byName := map[string]checkResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName["auth/qwen"]; r.Status != statusFIX || r.Hint == "" {
		t.Fatalf("unparseable session should FIX with hint, got %+v", r)
	}
	if r := byName["auth/grok"]; r.Status != statusWARN || !strings.Contains(r.Hint, "chimera auth login grok") {
		t.Fatalf("missing session should WARN with login hint, got %+v", r)
	}
}

// The hard rule: no secret material in any doctor output, ever.
func TestDoctorOutputNeverContainsSecrets(t *testing.T) {
	const (
		secretCookie = "SECRET-SSO-VALUE-do-not-leak"
		secretCF     = "SECRET-CF-BM-VALUE"
		secretToken  = "SECRET-BEARER-LEAK-MARKER"
	)
	dir := t.TempDir()
	sess := &webapi.Session{
		AccessToken: secretToken,
		Cookies:     map[string]string{"sso": secretCookie, "__cf_bm": secretCF},
	}
	if err := webapi.WriteSession(sess, filepath.Join(dir, "grok.json")); err != nil {
		t.Fatal(err)
	}

	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("API_TOKEN=tok-"+secretToken+"\nMYSTERY_KEY=val-"+secretCookie+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var all []checkResult
	all = append(all, checkVersion())
	all = append(all, checkSessionsDir(dir))
	all = append(all, checkProviderSessions(dir, []string{"grok", "qwen"})...)
	all = append(all, checkEnvFile(envPath))

	out := renderDoctor(all)
	for _, secret := range []string{secretCookie, secretCF, secretToken, "tok-" + secretToken, "val-" + secretCookie} {
		if strings.Contains(out, secret) {
			t.Fatalf("doctor output leaked secret material:\n%s", out)
		}
	}
	// The unknown key NAME is surfaced (names are not secrets); values are not.
	if !strings.Contains(out, "MYSTERY_KEY") {
		t.Fatalf("unknown env key should be named (name only):\n%s", out)
	}
}

// ── ports ──────────────────────────────────────────────────

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctorPortCheckListeningHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	results := checkGatewayPorts([]int{port}, false, "")
	if len(results) != 1 || results[0].Status != statusOK {
		t.Fatalf("want one OK, got %+v", results)
	}
	if !strings.Contains(results[0].Detail, fmt.Sprintf(":%d", port)) || !strings.Contains(results[0].Detail, "/health 200") {
		t.Fatalf("detail should name the port and health code, got %q", results[0].Detail)
	}
}

func TestDoctorPortCheckListeningNoHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // a listener that is not a Chimera gateway
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	results := checkGatewayPorts([]int{port}, false, "")
	if len(results) != 1 || results[0].Status != statusWARN {
		t.Fatalf("non-chimera listener should WARN, got %+v", results)
	}
	if !strings.Contains(results[0].Detail, "404") {
		t.Fatalf("detail should carry the health status, got %q", results[0].Detail)
	}
}

func TestDoctorPortCheckNoneListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // free again: doctor must report the range, not a finding

	results := checkGatewayPorts([]int{port}, false, "")
	if len(results) != 1 || results[0].Status != statusOK {
		t.Fatalf("quiet port is not a finding; want single OK, got %+v", results)
	}
	if !strings.Contains(results[0].Detail, "no gateway listening") {
		t.Fatalf("summary should say nothing is listening, got %q", results[0].Detail)
	}
}

// ── --probe ──────────────────────────────────────────────────

func TestDoctorProbeProvidersHealth(t *testing.T) {
	gotAuth := &atomicString{ch: make(chan string, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record the credential only for the probe endpoint: the /health
		// request doctor sends first carries no token and must not clobber
		// the capture slot.
		if r.URL.Path == "/v1/health/providers" {
			gotAuth.set(r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/health/providers" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"object":"provider_health","data":[`+
			`{"provider":"grok","model":"chimera-grok","up":true,"logged_in":true,"quota":{"remaining":21,"total":30,"window":"5h"}},`+
			`{"provider":"qwen","model":"chimera-qwen","up":true,"logged_in":false}]}`)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	results := checkGatewayPorts([]int{port}, true, "tok-123")
	out := renderDoctor(results)
	if gotAuth.get() != "Bearer tok-123" {
		t.Fatalf("probe must send the token, got %q", gotAuth.get())
	}
	for _, want := range []string{"grok", "up", "quota remaining=21/30 window=5h", "qwen", "up (logged_out)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("probe output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorProbeUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer right" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"object":"provider_health","data":[]}`)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	results := checkGatewayPorts([]int{port}, true, "wrong")
	if len(results) != 2 { // ports OK + probe WARN
		t.Fatalf("want ports+probe results, got %+v", results)
	}
	last := results[len(results)-1]
	if last.Status != statusWARN || !strings.Contains(last.Hint, "API_TOKEN") {
		t.Fatalf("401 should WARN with a token hint, got %+v", last)
	}
}

// atomicString is a tiny race-free capture slot for handler-side assertions.
type atomicString struct {
	ch chan string
}

func newAtomicString() *atomicString { return &atomicString{ch: make(chan string, 1)} }

func (s *atomicString) set(v string) {
	select {
	case s.ch <- v:
	default:
	}
}

func (s *atomicString) get() string {
	select {
	case v := <-s.ch:
		return v
	default:
		return ""
	}
}

// ── pdftotext ──────────────────────────────────────────────────

func TestDoctorPdfToolCheck(t *testing.T) {
	found := checkPdfTool(func(string) (string, error) { return "/usr/bin/pdftotext", nil })
	if found.Status != statusOK || !strings.Contains(found.Detail, "/usr/bin/pdftotext") {
		t.Fatalf("found tool should be OK, got %+v", found)
	}
	missing := checkPdfTool(func(string) (string, error) { return "", errors.New("exec: not found") })
	if missing.Status != statusWARN || missing.Hint == "" {
		t.Fatalf("missing tool should WARN with install hint, got %+v", missing)
	}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		if pdfInstallHint(goos) == "" {
			t.Fatalf("install hint for %s must not be empty", goos)
		}
	}
}

// ── disk ──────────────────────────────────────────────────

func TestDoctorDiskCheckTempDir(t *testing.T) {
	r := checkDiskSpace("browser_data", t.TempDir())
	if r.Status != statusOK || !strings.Contains(r.Detail, "free") {
		t.Fatalf("temp dir should have plenty of space, got %+v", r)
	}
	// A missing directory resolves to its existing ancestor instead of failing.
	r = checkDiskSpace("logs", filepath.Join(t.TempDir(), "logs"))
	if r.Status != statusOK {
		t.Fatalf("missing subdir should fall back to ancestor fs, got %+v", r)
	}
}

func TestDoctorHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		512: "0 KB", 5 << 20: "5 MB", 3 << 30: "3.0 GB", 42 << 30: "42 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// ── env ──────────────────────────────────────────────────

func TestDoctorEnvCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\n\nPROVIDER=grok\nAPI_TOKEN=never-shown\nexport LOG_DIR=./logs\n\"QUOTED_KEY\"=value\nnot a pair\nBAD NAME=x\nMYSTERY_KEY=hidden\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkEnvFile(path)
	if r.Status != statusWARN {
		t.Fatalf("unknown keys should WARN, got %+v", r)
	}
	// PROVIDER, API_TOKEN, LOG_DIR, MYSTERY_KEY (+ one of the two malformed
	// lines is rejected — neither may be counted as known).
	if !strings.Contains(r.Detail, "3 known key(s)") {
		t.Fatalf("expected 3 known keys, got %q", r.Detail)
	}
	if !strings.Contains(r.Detail, "MYSTERY_KEY") || !strings.Contains(r.Detail, "QUOTED_KEY") {
		t.Fatalf("unknown keys should be listed by name, got %q", r.Detail)
	}
	if strings.Contains(r.Detail, "hidden") || strings.Contains(r.Detail, "never-shown") {
		t.Fatalf("values must never be echoed, got %q", r.Detail)
	}
}

func TestDoctorEnvCheckMissingFile(t *testing.T) {
	r := checkEnvFile(filepath.Join(t.TempDir(), ".env"))
	if r.Status != statusOK {
		t.Fatalf("missing .env is fine (defaults), got %+v", r)
	}
}

// ── rendering ──────────────────────────────────────────────────

func TestRenderDoctorSummaryCounts(t *testing.T) {
	results := []checkResult{
		{"a", statusOK, "fine", ""},
		{"b", statusWARN, "hmm", "do the warn thing"},
		{"c", statusFIX, "bad", "run: fixit"},
	}
	out := renderDoctor(results)
	for _, want := range []string{"OK", "WARN", "FIX", "1 fix(es), 1 warning(s)", "→ do the warn thing", "→ run: fixit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(renderDoctor([]checkResult{{"a", statusOK, "fine", ""}}), "warning") {
		t.Fatal("clean run should not mention warnings")
	}
}

// ── the whole run against a temp tree ──────────────────────────────────

// TestDoctorResultsOnTempDirs runs the full check set against a temp
// install and asserts the OK/WARN/FIX shapes end to end. Network-facing
// checks are pinned: a closed port for the ports summary, a temp .env that
// does not exist (defaults), temp paths everywhere.
func TestDoctorResultsOnTempDirs(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		AuthDir:        filepath.Join(root, "auth_data"),
		LogDir:         filepath.Join(root, "logs"),
		BrowserDataDir: filepath.Join(root, "browser_data"),
	}
	sess := &webapi.Session{AccessToken: "cookie-session", Cookies: map[string]string{"sso": "x"}}
	if err := webapi.WriteSession(sess, filepath.Join(cfg.AuthDir, "grok.json")); err != nil {
		t.Fatal(err)
	}

	// A port that is listening-free by construction.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quietPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	results := doctorResults(doctorOptions{
		SessionsDir: filepath.Join(root, "sessions"),
		DotEnvPath:  filepath.Join(root, ".env"),
		Ports:       []int{quietPort},
	}, cfg, nil)

	byName := map[string]checkResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName["version"]; r.Status != statusOK {
		t.Fatalf("version: %+v", r)
	}
	if r := byName["auth/grok"]; r.Status != statusOK || !strings.Contains(r.Detail, "expiry unknown") {
		t.Fatalf("grok session (cookie-custody): %+v", r)
	}
	if r := byName["sessions"]; r.Status != statusWARN {
		t.Fatalf("missing sessions dir should WARN: %+v", r)
	}
	if r := byName["env"]; r.Status != statusOK || !strings.Contains(r.Detail, "no .env") {
		t.Fatalf("missing .env should be OK: %+v", r)
	}
	if r := byName["ports"]; r.Status != statusOK || !strings.Contains(r.Detail, "no gateway listening") {
		t.Fatalf("quiet port should summarize as OK: %+v", r)
	}
	if r := byName["disk"]; r.Detail == "" {
		t.Fatal("disk check should always produce a detail line")
	}
}
