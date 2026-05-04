package routing

import "log"

// Logger is the minimal logging interface the routing package uses to emit
// operator diagnostics (malformed profile skips, missing-field rejections,
// and load-count summaries). It is intentionally small so callers can adapt
// their own structured loggers without pulling in additional dependencies.
type Logger interface {
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// noopLogger is used when NewProfileStore is called without a logger. It
// discards all diagnostics silently, preserving the legacy behavior of the
// routing package for callers (including tests) that don't inject a logger.
type noopLogger struct{}

func (noopLogger) Infof(string, ...interface{})  {}
func (noopLogger) Warnf(string, ...interface{})  {}
func (noopLogger) Errorf(string, ...interface{}) {}

// stdLogger is a fallback implementation that writes to the stdlib log
// package with a severity prefix. It is not used by the daemon (which injects
// its own logger) but is available for consumers that want loud defaults.
type stdLogger struct{}

func (stdLogger) Infof(format string, args ...interface{}) {
	log.Printf("[INFO] routing: "+format, args...)
}

func (stdLogger) Warnf(format string, args ...interface{}) {
	log.Printf("[WARN] routing: "+format, args...)
}

func (stdLogger) Errorf(format string, args ...interface{}) {
	log.Printf("[ERROR] routing: "+format, args...)
}
