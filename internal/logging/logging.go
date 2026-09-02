// Package logging provides structured logging for Chimera.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
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
type Logger struct {
	name  string
	level Level
	debug *log.Logger
	info  *log.Logger
	warn  *log.Logger
	err   *log.Logger
}

// New creates a new Logger writing to the given directory.
func New(name, logDir, level string, verbose bool) *Logger {
	lvl := parseLevel(level)

	var debugW, infoW, warnW, errW io.Writer

	if verbose {
		debugW = io.MultiWriter(os.Stderr, &noopWriter{})
		infoW = io.MultiWriter(os.Stderr, &noopWriter{})
		warnW = io.MultiWriter(os.Stderr, &noopWriter{})
		errW = io.MultiWriter(os.Stderr, &noopWriter{})
	} else {
		debugW = &noopWriter{}
		infoW = &noopWriter{}
		warnW = &noopWriter{}
		errW = &noopWriter{}
	}

	// Also write to log files
	logFile := filepath.Join(logDir, name+".log")
	if f, ferr := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		debugW = io.MultiWriter(debugW, f)
		infoW = io.MultiWriter(infoW, f)
		warnW = io.MultiWriter(warnW, f)
		errW = io.MultiWriter(errW, f)
	}

	return &Logger{
		name:  name,
		level: lvl,
		debug: log.New(debugW, fmt.Sprintf("[%s] DEBUG ", name), log.Ltime),
		info:  log.New(infoW, fmt.Sprintf("[%s] INFO  ", name), log.Ltime),
		warn:  log.New(warnW, fmt.Sprintf("[%s] WARN  ", name), log.Ltime),
		err:   log.New(errW, fmt.Sprintf("[%s] ERROR ", name), log.Ltime),
	}
}

func (l *Logger) Debugf(format string, args ...any) {
	if l.level <= LevelDebug {
		l.debug.Printf(format, args...)
	}
}

func (l *Logger) Infof(format string, args ...any) {
	if l.level <= LevelInfo {
		l.info.Printf(format, args...)
	}
}

func (l *Logger) Warnf(format string, args ...any) {
	if l.level <= LevelWarn {
		l.warn.Printf(format, args...)
	}
}

func (l *Logger) Errorf(format string, args ...any) {
	if l.level <= LevelError {
		l.err.Printf(format, args...)
	}
}

func (l *Logger) Debug(args ...any) {
	if l.level <= LevelDebug {
		l.debug.Print(args...)
	}
}

func (l *Logger) Info(args ...any) {
	if l.level <= LevelInfo {
		l.info.Print(args...)
	}
}

func (l *Logger) Warn(args ...any) {
	if l.level <= LevelWarn {
		l.warn.Print(args...)
	}
}

func (l *Logger) Error(args ...any) {
	if l.level <= LevelError {
		l.err.Print(args...)
	}
}

// noopWriter discards everything.
type noopWriter struct{}

func (n *noopWriter) Write(p []byte) (int, error) { return len(p), nil }
