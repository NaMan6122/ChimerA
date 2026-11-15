package logging

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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
