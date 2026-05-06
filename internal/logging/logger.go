package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Level controls the minimum severity that gets written.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Logger provides structured logging
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	logger *log.Logger
	level  Level
}

// New creates a new logger that writes to the given writer.
// Default level is LevelDebug (all messages).
func New(w io.Writer) *Logger {
	return &Logger{
		writer: w,
		logger: log.New(w, "", log.LstdFlags),
		level:  LevelDebug,
	}
}

// SetLevel changes the minimum log level.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// NewFile creates a logger that writes to a file
func NewFile(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	return New(f), nil
}

// Info logs an informational message
func (l *Logger) Info(format string, args ...interface{}) {
	l.logAt(LevelInfo, "INFO", format, args...)
}

// Warn logs a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	l.logAt(LevelWarn, "WARN", format, args...)
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	l.logAt(LevelError, "ERROR", format, args...)
}

// Debug logs a debug message
func (l *Logger) Debug(format string, args ...interface{}) {
	l.logAt(LevelDebug, "DEBUG", format, args...)
}

// logAt writes a message if the logger's level permits it.
func (l *Logger) logAt(lvl Level, label, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if lvl < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.logger.Printf("[%s] %s", label, msg)
}

// Close closes the logger (if backed by a file)
func (l *Logger) Close() error {
	if f, ok := l.writer.(*os.File); ok {
		return f.Close()
	}
	return nil
}
