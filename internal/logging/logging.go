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

// Logger is a simple leveled logger.
// Format is chosen once via LOG_FORMAT=json (default: text). Use
// WithRequestID to tag all lines from one HTTP request (chi RequestID).
type Logger struct {
	name   string
	level  Level
	out    io.Writer
	format string
	reqID  string
	mu     *sync.Mutex
}

// New creates a new Logger writing to stderr (if verbose) and a log file.
func New(name, logDir, level string, verbose bool) *Logger {
	lvl := parseLevel(level)

	var out io.Writer = &noopWriter{}
	if verbose {
		out = os.Stderr
	}

	// Also write to log files
	logFile := filepath.Join(logDir, name+".log")
	if f, ferr := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		out = io.MultiWriter(out, f)
	}

	format := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT")))
	if format != "json" {
		format = "text"
	}

	return &Logger{name: name, level: lvl, out: out, format: format, mu: &sync.Mutex{}}
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

func (l *Logger) output(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == "json" {
		rec := map[string]string{
			"ts":     time.Now().Format(time.RFC3339),
			"level":  strings.ToLower(level),
			"logger": l.name,
			"msg":    msg,
		}
		if l.reqID != "" {
			rec["req_id"] = l.reqID
		}
		_ = json.NewEncoder(l.out).Encode(rec)
		return
	}
	// Text format mirrors the historic "[name] LEVEL HH:MM:SS msg" layout.
	ts := time.Now().Format("15:04:05")
	if l.reqID != "" {
		fmt.Fprintf(l.out, "[%s] %-6s%s [req=%s] %s\n", l.name, level, ts, l.reqID, msg)
		return
	}
	fmt.Fprintf(l.out, "[%s] %-6s%s %s\n", l.name, level, ts, msg)
}

func (l *Logger) Debugf(format string, args ...any) {
	if l.level <= LevelDebug {
		l.output("DEBUG", fmt.Sprintf(format, args...))
	}
}

func (l *Logger) Infof(format string, args ...any) {
	if l.level <= LevelInfo {
		l.output("INFO", fmt.Sprintf(format, args...))
	}
}

func (l *Logger) Warnf(format string, args ...any) {
	if l.level <= LevelWarn {
		l.output("WARN", fmt.Sprintf(format, args...))
	}
}

func (l *Logger) Errorf(format string, args ...any) {
	if l.level <= LevelError {
		l.output("ERROR", fmt.Sprintf(format, args...))
	}
}

func (l *Logger) Debug(args ...any) {
	if l.level <= LevelDebug {
		l.output("DEBUG", fmt.Sprint(args...))
	}
}

func (l *Logger) Info(args ...any) {
	if l.level <= LevelInfo {
		l.output("INFO", fmt.Sprint(args...))
	}
}

func (l *Logger) Warn(args ...any) {
	if l.level <= LevelWarn {
		l.output("WARN", fmt.Sprint(args...))
	}
}

func (l *Logger) Error(args ...any) {
	if l.level <= LevelError {
		l.output("ERROR", fmt.Sprint(args...))
	}
}

// noopWriter discards everything.
type noopWriter struct{}

func (n *noopWriter) Write(p []byte) (int, error) { return len(p), nil }
