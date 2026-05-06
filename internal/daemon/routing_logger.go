package daemon

import "github.com/Root-IO-Labs/open-agent-teams/internal/logging"

// computeRemovedWorkerModels returns the set of model IDs that were in the
// previous allow-list but are no longer in the new allow-list. Used to detect
// drift after set/remove/clear on AllowedWorkerModels so the daemon can warn
// about workers still running on now-disallowed models.
//
// Treats an empty previous list as "no restriction" in the caller's semantics
// — i.e., if there was no restriction before, nothing has been narrowed, so
// no models are considered removed. Callers that want "everything in the new
// list that wasn't there before" should compute that separately.
func computeRemovedWorkerModels(previous, current []string) map[string]bool {
	if len(previous) == 0 {
		return nil
	}
	currentSet := make(map[string]bool, len(current))
	for _, m := range current {
		currentSet[m] = true
	}
	removed := make(map[string]bool)
	for _, m := range previous {
		if !currentSet[m] {
			removed[m] = true
		}
	}
	if len(removed) == 0 {
		return nil
	}
	return removed
}

// routingLoggerAdapter bridges the routing package's minimal Logger interface
// (Infof/Warnf/Errorf) onto the daemon's structured logging.Logger
// (Info/Warn/Error). It is a thin pass-through — message formatting happens
// in the logging package — so the only job here is to give the routing
// package a seam it can depend on without pulling in daemon internals.
type routingLoggerAdapter struct {
	l *logging.Logger
}

func newRoutingLogger(l *logging.Logger) *routingLoggerAdapter {
	return &routingLoggerAdapter{l: l}
}

func (a *routingLoggerAdapter) Infof(format string, args ...interface{}) {
	a.l.Info(format, args...)
}

func (a *routingLoggerAdapter) Warnf(format string, args ...interface{}) {
	a.l.Warn(format, args...)
}

func (a *routingLoggerAdapter) Errorf(format string, args ...interface{}) {
	a.l.Error(format, args...)
}
