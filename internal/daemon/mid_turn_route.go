package daemon

import (
	"sync"
	"time"
)

// midTurnInterruptSettle is how long we wait after Ctrl-C for the
// runtime to emit [OAT_TURN_END] (and clear turnInFlight) before we
// deliver the interrupting user message anyway. Package var so tests
// can shrink it without sleeping ~1s per case.
var midTurnInterruptSettle = 800 * time.Millisecond

// interruptContinuePrefix is prepended when a mid-turn side-panel send
// interrupted an open turn and the user text is NOT stuck-flavored
// (stuck-flavored messages already get statusDiagnosePrefix). Code-only —
// does not echo user text.
const interruptContinuePrefix = "[OAT-system] Your previous turn was interrupted by a new side-panel message. " +
	"Reply visibly in chat this turn (brief concrete status if useful) before or alongside tools. " +
	"Continue unfinished prior work from history unless the user cancelled it. " +
	"If a file write was interrupted, mention the path may be partially written on disk unless they asked to delete/discard it.\n"

// midTurnRouteLock returns a per-target mutex so concurrent mid-turn
// sends serialize (no stacked Ctrl-C storms).
func (d *Daemon) midTurnRouteLock(key string) *sync.Mutex {
	d.routeMidTurnMu.Lock()
	defer d.routeMidTurnMu.Unlock()
	if d.routeMidTurnLocks == nil {
		d.routeMidTurnLocks = make(map[string]*sync.Mutex)
	}
	m, ok := d.routeMidTurnLocks[key]
	if !ok {
		m = &sync.Mutex{}
		d.routeMidTurnLocks[key] = m
	}
	return m
}

// isAssistantTurnInFlight reports whether the (session, agent) tailer
// believes a side-panel turn is still open (armed and not yet turn_end).
func (d *Daemon) isAssistantTurnInFlight(sessionName, agentName string) bool {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	t := d.assistantTurnTailers[key]
	d.assistantTurnTailersMu.Unlock()
	if t == nil {
		return false
	}
	return t.isTurnInFlight()
}

// clearAssistantTurnInFlight clears the open-turn latch (used after an
// interrupt attempt so a missed turn_end cannot sticky-interrupt forever).
func (d *Daemon) clearAssistantTurnInFlight(sessionName, agentName string) {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	t := d.assistantTurnTailers[key]
	d.assistantTurnTailersMu.Unlock()
	if t != nil {
		t.clearTurnInFlight()
	}
}

// suppressAssistantTurnEndClear ignores a delayed turn_end clear so the
// newly armed turn stays in-flight after interrupt-then-route.
func (d *Daemon) suppressAssistantTurnEndClear(sessionName, agentName string, durr time.Duration) {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	t := d.assistantTurnTailers[key]
	d.assistantTurnTailersMu.Unlock()
	if t != nil {
		t.suppressNextTurnEndClear(durr)
	}
}

// waitAssistantTurnIdle polls until turnInFlight clears or timeout.
func (d *Daemon) waitAssistantTurnIdle(sessionName, agentName string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !d.isAssistantTurnInFlight(sessionName, agentName) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// interruptMidTurnIfNeeded fires Ctrl-C when a side-panel turn is open,
// waits briefly for cancel/history repair, then returns true if an
// interrupt was sent. Caller must still deliver the user text afterward.
// Serialized per target. No-op when no turn is in flight.
func (d *Daemon) interruptMidTurnIfNeeded(sessionName, windowName, agentName, repoName string) bool {
	key := repoName + "/" + agentName
	lock := d.midTurnRouteLock(key)
	lock.Lock()
	defer lock.Unlock()

	if !d.isAssistantTurnInFlight(sessionName, agentName) {
		return false
	}

	if err := d.backend.SendInterrupt(d.ctx, sessionName, windowName); err != nil {
		d.logger.Warn(
			"mid-turn interrupt failed for %s/%s (continuing to route message): %v",
			repoName, agentName, err,
		)
		// Still clear the latch + mark interrupted so recovery does not
		// fight the new message, and so we do not retry forever.
		d.assistantRecovery.markInterrupted(sessionName, agentName)
		d.suppressAssistantTurnEndClear(sessionName, agentName, 3*time.Second)
		d.clearAssistantTurnInFlight(sessionName, agentName)
		return true
	}
	d.assistantRecovery.markInterrupted(sessionName, agentName)
	d.logger.Info("mid-turn interrupt-then-route: interrupted %s/%s before new side-panel message", repoName, agentName)
	d.waitAssistantTurnIdle(sessionName, agentName, midTurnInterruptSettle)
	// Suppress a delayed turn_end from the cancelled turn, then clear
	// any sticky latch so idle sends do not spuriously interrupt.
	d.suppressAssistantTurnEndClear(sessionName, agentName, 3*time.Second)
	d.clearAssistantTurnInFlight(sessionName, agentName)
	return true
}
