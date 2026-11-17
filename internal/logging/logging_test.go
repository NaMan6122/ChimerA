package logging

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetForTest clears the registry and the recorded Configure. Both are
// process-wide by design, so without this one test's loggers and directory
// would be re-pointed by the next test's Configure - and a logger built after
// that Configure would adopt the previous test's configuration.
func resetForTest(t *testing.T) {
	t.Helper()
	clear := func() {
		regMu.Lock()
		loggers = nil
		applied = nil
		regMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// readLog returns the contents of <dir>/<name>.log, empty when absent.
func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading log: %v", err)
	}
	return string(b)
}

// captureStderr points os.Stderr at a temp file; the returned func restores it
// and yields what was written. New and Configure both capture os.Stderr, so
// the swap has to cover the whole test body.
func captureStderr(t *testing.T) (*os.File, func() string) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	old := os.Stderr
	os.Stderr = tmp
	return tmp, func() string {
		os.Stderr = old
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("seek stderr: %v", err)
		}
		b, err := io.ReadAll(tmp)
		if err != nil {
			t.Fatalf("read stderr: %v", err)
		}
		return string(b)
	}
}

// (a) Configure redirects new lines into the configured directory and stops
// writing to the directory New opened.
func TestConfigureRedirectsOutput(t *testing.T) {
	resetForTest(t)
	before, after := t.TempDir(), t.TempDir()

	l := New("redirect", before, "debug", false)
	l.Info("pre-configure")

	Configure(after, "debug", false)
	l.Info("post-configure")

	if got := readLog(t, after, "redirect"); !strings.Contains(got, "post-configure") {
		t.Fatalf("configured dir missing post-configure line, got %q", got)
	}
	// The old handle must be closed: nothing else may reach the old file.
	old := readLog(t, before, "redirect")
	if strings.Contains(old, "post-configure") {
		t.Fatalf("old dir kept receiving lines: %q", old)
	}
	if !strings.Contains(old, "pre-configure") {
		t.Fatalf("pre-configure line lost from old dir: %q", old)
	}
}

// (b) The configured level actually filters: LOG_LEVEL=error suppresses debug
// and info.
func TestConfigureLevelFiltersDebug(t *testing.T) {
	resetForTest(t)
	dir := t.TempDir()
	l := New("filter", dir, "debug", false)
	l.Debug("debug-before")

	Configure(dir, "error", false)
	l.Debug("debug-after")
	l.Infof("info-after")
	l.Errorf("error-after")

	got := readLog(t, dir, "filter")
	for _, suppressed := range []string{"debug-after", "info-after"} {
		if strings.Contains(got, suppressed) {
			t.Fatalf("level=error let %q through: %q", suppressed, got)
		}
	}
	if !strings.Contains(got, "debug-before") {
		t.Fatalf("pre-configure debug line missing: %q", got)
	}
	if !strings.Contains(got, "error-after") {
		t.Fatalf("error line suppressed: %q", got)
	}
}

// (c) verbose=false stops the stderr copy while the file keeps the line.
func TestConfigureVerboseFalseSilencesStderr(t *testing.T) {
	resetForTest(t)
	dir := t.TempDir()
	_, stderr := captureStderr(t)

	l := New("verbosity", dir, "debug", true)
	l.Info("to-stderr")

	Configure(dir, "debug", false)
	l.Info("file-only")

	errOut := stderr()
	if !strings.Contains(errOut, "to-stderr") {
		t.Fatalf("verbose logger did not reach stderr: %q", errOut)
	}
	if strings.Contains(errOut, "file-only") {
		t.Fatalf("verbose=false still wrote to stderr: %q", errOut)
	}
	got := readLog(t, dir, "verbosity")
	if !strings.Contains(got, "file-only") || !strings.Contains(got, "to-stderr") {
		t.Fatalf("file missing lines after verbose change: %q", got)
	}
}

// (d) Every logger created before Configure follows it, including
// request-scoped copies made with WithRequestID.
func TestConfigureReachesLoggersCreatedBeforeIt(t *testing.T) {
	resetForTest(t)
	before, after := t.TempDir(), t.TempDir()

	one := New("first", before, "debug", false)
	two := New("second", before, "debug", false)
	scoped := one.WithRequestID("req-1")

	Configure(after, "debug", false)

	one.Info("one-line")
	two.Info("two-line")
	scoped.Info("scoped-line")

	for name, want := range map[string]string{
		"first":  "one-line",
		"second": "two-line",
	} {
		if got := readLog(t, after, name); !strings.Contains(got, want) {
			t.Fatalf("%s did not follow Configure, got %q", name, got)
		}
	}
	// A request-scoped copy shares the target, so it lands in the new
	// directory with its request tag intact.
	if got := readLog(t, after, "first"); !strings.Contains(got, "scoped-line") || !strings.Contains(got, "[req=req-1]") {
		t.Fatalf("WithRequestID copy did not follow Configure: %q", got)
	}
}

// (e) Logging from many goroutines while Configure re-points the target must
// not race or panic (run with -race). Torn lines are possible only in the
// same way two loggers can interleave, never a lost or crashed write.
func TestConcurrentLoggingDuringConfigure(t *testing.T) {
	resetForTest(t)
	dirs := []string{t.TempDir(), t.TempDir()}
	l := New("concurrent", dirs[0], "debug", false)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				l.Infof("goroutine %d line %d", id, n)
				l.Debugf("goroutine %d debug %d", id, n)
			}
		}(i)
	}

	for i := 0; i < 20; i++ {
		// verbose stays false here: the point is the fd swap under
		// concurrent writes, and stderr noise would bury the test output.
		Configure(dirs[i%2], "info", false)
	}
	close(stop)
	wg.Wait()

	// A final Configure fixes where the tail of the log lives; both files must
	// still be readable and at least one line must have survived.
	Configure(dirs[0], "debug", false)
	l.Info("after-configure")
	got := readLog(t, dirs[0], "concurrent")
	if !strings.Contains(got, "after-configure") {
		t.Fatalf("post-Configure line missing: %q", got)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "concurrent.log")); err != nil {
			t.Fatalf("log file in %s: %v", d, err)
		}
	}
}

// blockedDir returns a path that cannot be opened as a directory, plus the
// parent to repair it in later. The parent of the path is a regular FILE, so
// the open fails with ENOTDIR: unlike chmod 0500 this also fails for root, and
// nothing needs restoring.
func blockedDir(t *testing.T) (path, parent string) {
	t.Helper()
	parent = t.TempDir()
	path = filepath.Join(parent, "logs")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("blocking file: %v", err)
	}
	return path, parent
}

// (f) An unwritable LOG_DIR must not cost a single line. With VERBOSE=false the
// replacement writer would be a discard, so the old handle has to stay open.
func TestConfigureKeepsLoggingWhenNewDirUnwritable(t *testing.T) {
	resetForTest(t)
	good := t.TempDir()
	blocked, _ := blockedDir(t)
	_, stderr := captureStderr(t)

	l := New("unwritable", good, "debug", false)
	l.Info("pre-failure")

	// verbose=false is the production setting, and info is still a real level
	// change: both must take effect without touching the working handle.
	Configure(blocked, "info", false)
	l.Info("after-failure")
	l.Debug("debug-after-failure")

	got := readLog(t, good, "unwritable")
	if !strings.Contains(got, "pre-failure") {
		t.Fatalf("pre-failure line lost: %q", got)
	}
	if !strings.Contains(got, "after-failure") {
		t.Fatalf("line discarded after a failed open: %q", got)
	}
	if strings.Contains(got, "debug-after-failure") {
		t.Fatalf("level change not applied on the failed open: %q", got)
	}
	if strings.Contains(got, "could not be opened") {
		t.Fatalf("warning leaked into the log file: %q", got)
	}
	if out := stderr(); !strings.Contains(out, "could not be opened") {
		t.Fatalf("no warning on stderr: %q", out)
	}
}

// (g) A failed first open must be retried by a later Configure with the SAME
// arguments, otherwise a process that boots before LOG_DIR exists never logs.
func TestConfigureRetriesFailedFirstOpen(t *testing.T) {
	resetForTest(t)
	blocked, _ := blockedDir(t)
	_, stderr := captureStderr(t)

	l := New("retry", blocked, "debug", false)
	l.Info("line-before-dir-existed")

	// Identical to the New arguments: without the file==nil term in the
	// idempotence guard this returns early and never opens anything.
	Configure(blocked, "debug", false)

	// The directory appears, as `mkdir -p "$LOG_DIR"` in the entrypoint would.
	if err := os.Remove(blocked); err != nil {
		t.Fatalf("unblocking: %v", err)
	}
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("creating log dir: %v", err)
	}
	Configure(blocked, "debug", false)
	l.Info("line-after-dir-appeared")

	got := readLog(t, blocked, "retry")
	if !strings.Contains(got, "line-after-dir-appeared") {
		t.Fatalf("failed first open was not retried: %q", got)
	}
	if out := stderr(); !strings.Contains(out, "could not be opened") {
		t.Fatalf("no warning while the directory was missing: %q", out)
	}
}

// (h) LOG_FORMAT=json has to reach the loggers built at init, which captured
// text before the environment was resolved.
func TestConfigureSwitchesToJSONFormat(t *testing.T) {
	resetForTest(t)
	dir := t.TempDir()
	t.Setenv("LOG_FORMAT", "") // what a package-level logger sees with no env

	l := New("format", dir, "debug", false)
	l.Info("text-line")

	t.Setenv("LOG_FORMAT", "json")
	Configure(dir, "debug", false)
	l.Info("json-line")

	got := readLog(t, dir, "format")
	if !strings.Contains(got, "[format] INFO") {
		t.Fatalf("pre-configure line is not text: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	var rec map[string]string
	last := lines[len(lines)-1]
	if err := json.Unmarshal([]byte(last), &rec); err != nil {
		t.Fatalf("post-configure line is not JSON: %q (%v)", last, err)
	}
	if rec["logger"] != "format" || rec["msg"] != "json-line" || rec["level"] != "info" {
		t.Fatalf("unexpected JSON record: %v", rec)
	}
}

// (i) A logger created after Configure follows it instead of keeping the
// arguments its New call passed in.
func TestLoggerCreatedAfterConfigureHonoursConfig(t *testing.T) {
	resetForTest(t)
	configured, stale := t.TempDir(), t.TempDir()
	_, stderr := captureStderr(t)

	Configure(configured, "error", false)

	// Pre-config arguments say: ./stale, debug, verbose. The recorded
	// configuration says otherwise on all three.
	l := New("late", stale, "debug", true)
	l.Info("late-info")
	l.Error("late-error")

	got := readLog(t, configured, "late")
	if !strings.Contains(got, "late-error") {
		t.Fatalf("logger created after Configure did not follow it: %q", got)
	}
	if strings.Contains(got, "late-info") {
		t.Fatalf("configured level not applied to a late logger: %q", got)
	}
	if old := readLog(t, stale, "late"); old != "" {
		t.Fatalf("late logger kept writing to its New directory: %q", old)
	}
	if out := stderr(); strings.Contains(out, "late-error") {
		t.Fatalf("verbose from the New arguments leaked to stderr: %q", out)
	}
}

// (j) Repeated Configure with identical arguments must not reopen the file:
// O_APPEND would keep the content, so the file identity and the lines are what
// prove nothing was swapped underneath a writer.
func TestRepeatedConfigureIsANoOp(t *testing.T) {
	resetForTest(t)
	first, second := t.TempDir(), t.TempDir()

	l := New("idem", first, "debug", false)
	l.Info("line-one")
	Configure(second, "debug", false)
	l.Info("line-two")

	path := filepath.Join(second, "idem.log")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	for i := 0; i < 3; i++ {
		Configure(second, "debug", false)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("repeated Configure replaced the log file")
	}

	l.Info("line-three")
	got := readLog(t, second, "idem")
	for _, want := range []string{"line-two", "line-three"} {
		if !strings.Contains(got, want) {
			t.Fatalf("line lost across repeated Configure: %q", got)
		}
	}
	if n := strings.Count(got, "line-two"); n != 1 {
		t.Fatalf("line-two appears %d times: %q", n, got)
	}
	if old := readLog(t, first, "idem"); strings.Contains(old, "line-three") {
		t.Fatalf("line-three reached the previous directory: %q", old)
	}
}
