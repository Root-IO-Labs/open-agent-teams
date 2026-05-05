package routing

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
