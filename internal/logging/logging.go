// Package logging provides structured logging for Chimera.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = [...]string{"DEBUG", "INFO", "WARN", "ERROR"}

func parseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelDebug
	}
}

// target is where a logger writes and at which level. It is shared through a
// pointer because WithRequestID copies the Logger, not the target: a
// request-scoped copy has to follow Configure like the original.
type target struct {
	// mu is held for reading for the whole of one line (check, format, write)
	// and exclusively to re-point the target, so Configure can close the
	// previous file without a write racing into it. format lives here rather
	// than on Logger for the same reason: it is read while formatting a line,
	// so it has to move with the target and sit under the lock that guards it.
	mu      sync.RWMutex
	level   Level
	dir     string
	verbose bool
	format  string
	file    *os.File  // nil when the log file could not be opened
	out     io.Writer // stderr-if-verbose plus the file, or a discard
}

// Logger is a simple leveled logger.
// Use WithRequestID to tag all lines from one HTTP request (chi RequestID).
type Logger struct {
	name  string
	reqID string
	t     *target
}

// New creates a new Logger writing to stderr (if verbose) and a log file.
// The arguments are pre-config defaults: Configure re-points every logger New
// has returned, and a logger built after Configure is immediately re-pointed
// onto that configuration, so a package-level logger built at init still
// honours the resolved config once the config is loaded.
func New(name, logDir, level string, verbose bool) *Logger {
	l := &Logger{
		name: name,
		t: &target{
			level:   parseLevel(level),
			dir:     logDir,
			verbose: verbose,
			format:  logFormat(),
		},
	}
	l.t.file = openLogFile(name, logDir)
	l.t.out = l.t.buildOut()
	register(l)
	return l
}

// logFormat reads LOG_FORMAT; anything but "json" is text. New and Configure
// both call it, so Configure can fix the loggers built before LOG_FORMAT was
// visible instead of leaving them on the format they happened to see at init.
func logFormat() string {
	if strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) == "json" {
		return "json"
	}
	return "text"
}

// openLogFile returns nil when the path cannot be opened (missing directory,
// read-only fs): losing the file must not lose the log line entirely.
func openLogFile(name, dir string) *os.File {
	f, err := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// buildOut composes stderr (when verbose) with the log file, if any.
func (t *target) buildOut() io.Writer {
	var out io.Writer = &noopWriter{}
	if t.verbose {
		out = os.Stderr
	}
	if t.file != nil {
		out = io.MultiWriter(out, t.file)
	}
	return out
}

var (
	// regMu guards the logger registry, which exists so Configure can reach
	// loggers that were built at init, long before config.Load ran. It also
	// guards applied so the two move together: see register.
	regMu   sync.Mutex
	loggers []*Logger
	applied *config

	// configureMu serializes Configure calls: half-applied re-pointing would
	// leave some loggers on ./logs and some on LOG_DIR.
	configureMu sync.Mutex
)

// config is one resolved Configure call, kept package-wide so a logger created
// afterwards does not keep whatever arguments its New call happened to pass.
type config struct {
	dir     string
	level   Level
	verbose bool
	format  string
}

// register adds l to the registry and replays the last Configure onto it.
//
// Both steps happen against regMu, which is what Configure holds while it
// records the new config and copies the registry: a logger either sees the
// recorded config here, or was already registered when Configure took its
// snapshot. There is no window where it misses both. Configure itself is not
// called, because Configure blocks all loggers and would deadlock here.
func register(l *Logger) {
	regMu.Lock()
	loggers = append(loggers, l)
	last := applied
	regMu.Unlock()

	// Re-point outside regMu: reconfigure takes the target lock, and a slow
	// filesystem must not block New on another goroutine.
	if last != nil {
		l.reconfigure(last.dir, last.level, last.verbose, last.format)
	}
}

// Configure makes dir, level and verbose authoritative for every logger this
// package has created so far, including the package-level ones built at init
// with hardcoded defaults. Call it once config.Load() has resolved LOG_DIR,
// LOG_LEVEL and VERBOSE. It is idempotent and safe before anything logs.
func Configure(dir, level string, verbose bool) {
	lvl := parseLevel(level)

	configureMu.Lock()
	defer configureMu.Unlock()

	regMu.Lock()
	// Record the config under the same lock that serialises the snapshot, so
	// a concurrent New cannot miss it. LOG_FORMAT is read here rather than per
	// logger, so one call fixes the formatting of the loggers built at init.
	applied = &config{dir: dir, level: lvl, verbose: verbose, format: logFormat()}
	targets := make([]*Logger, len(loggers))
	copy(targets, loggers)
	cfg := *applied
	regMu.Unlock()

	// Re-point outside regMu: each logger takes its own target lock, and a
	// slow filesystem must not block New on another goroutine.
	for _, l := range targets {
		l.reconfigure(cfg.dir, cfg.level, cfg.verbose, cfg.format)
	}
}

// reconfigure moves one logger onto a new target. The new file is opened
// before the old handle is closed, so a failed open degrades instead of
// losing lines; the close happens under the exclusive lock, so no line can
// land in the old directory after Configure returns.
func (l *Logger) reconfigure(dir string, lvl Level, verbose bool, format string) {
	t := l.t
	t.mu.Lock()
	defer t.mu.Unlock()

	// file==nil belongs in the guard: a logger whose first open failed would
	// otherwise never retry, because its arguments already match the config.
	if t.dir == dir && t.level == lvl && t.verbose == verbose && t.format == format && t.file != nil {
		return // already configured; reopening would only churn descriptors
	}

	f := openLogFile(l.name, dir)
	if f == nil {
		// Keep the working handle and the writer that wraps it. Swapping in
		// buildOut() here would hand back a discard whenever VERBOSE=false -
		// the production setting - and every line for the rest of the process
		// would vanish silently. t.dir is left alone so the next Configure
		// retries the open rather than believing it already succeeded.
		t.level, t.verbose, t.format = lvl, verbose, format
		t.out = t.buildOut()
		fmt.Fprintf(os.Stderr, "[%s] WARN   log directory %q could not be opened; logging continues to %q\n", l.name, dir, t.dir)
		return
	}

	old := t.file
	t.dir, t.level, t.verbose, t.format = dir, lvl, verbose, format
	t.file = f
	t.out = t.buildOut()
	if old != nil {
		_ = old.Close()
	}
}

// WithRequestID returns a copy that tags lines with the request ID.
// Returns the same logger when id is empty.
func (l *Logger) WithRequestID(id string) *Logger {
	if id == "" {
		return l
	}
	c := *l
	c.reqID = id
	return &c
}

// emit writes one line if lvl passes the configured level. The check, the
// formatting and the write share the read lock, so a concurrent Configure
// cannot switch the target in the middle of a line.
func (l *Logger) emit(lvl Level, level string, args ...any) {
	t := l.t
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.level > lvl {
		return
	}
	l.write(t.out, level, fmt.Sprint(args...))
}

// emitf is emit with a format string. Formatting happens after the level
// check, so suppressed debug lines cost nothing.
func (l *Logger) emitf(lvl Level, level, format string, args ...any) {
	t := l.t
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.level > lvl {
		return
	}
	l.write(t.out, level, fmt.Sprintf(format, args...))
}

// write formats one record onto out. The caller holds t.mu for reading, which
// is what makes reading t.format here safe against a concurrent Configure.
func (l *Logger) write(out io.Writer, level, msg string) {
	if l.t.format == "json" {
		rec := map[string]string{
			"ts":     time.Now().Format(time.RFC3339),
			"level":  strings.ToLower(level),
			"logger": l.name,
			"msg":    msg,
		}
		if l.reqID != "" {
			rec["req_id"] = l.reqID
		}
		_ = json.NewEncoder(out).Encode(rec)
		return
	}
	// Text format mirrors the historic "[name] LEVEL HH:MM:SS msg" layout.
	ts := time.Now().Format("15:04:05")
	if l.reqID != "" {
		fmt.Fprintf(out, "[%s] %-6s%s [req=%s] %s\n", l.name, level, ts, l.reqID, msg)
		return
	}
	fmt.Fprintf(out, "[%s] %-6s%s %s\n", l.name, level, ts, msg)
}

func (l *Logger) Debugf(format string, args ...any) {
	l.emitf(LevelDebug, "DEBUG", format, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.emitf(LevelInfo, "INFO", format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.emitf(LevelWarn, "WARN", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.emitf(LevelError, "ERROR", format, args...)
}

func (l *Logger) Debug(args ...any) {
	l.emit(LevelDebug, "DEBUG", args...)
}

func (l *Logger) Info(args ...any) {
	l.emit(LevelInfo, "INFO", args...)
}

func (l *Logger) Warn(args ...any) {
	l.emit(LevelWarn, "WARN", args...)
}

func (l *Logger) Error(args ...any) {
	l.emit(LevelError, "ERROR", args...)
}

// noopWriter discards everything.
type noopWriter struct{}

func (n *noopWriter) Write(p []byte) (int, error) { return len(p), nil }
