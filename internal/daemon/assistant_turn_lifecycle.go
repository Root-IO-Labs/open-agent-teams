package daemon

// This file owns the daemon-side lifecycle helpers for the per-agent
// assistantTurnTailer + turnBroadcaster pair. Kept separate from
// daemon.go so the bulk of daemon.go stays readable and Part 2g's
// concerns are easy to find in a code review.

// sidePanelInputSentinel is the prefix the daemon prepends to text
// injected via handleAgentInput before it reaches the agent's PTY.
// The browser.md prompt is taught to key off this exact byte string
// when deciding "is this the side-panel user talking to me?" — the
// prefix is deliberately load-bearing ONLY for prompt context, not
// for safety/sanitization. The auto-emit path (Part 2g Option E) is
// what guarantees side-panel replies render; the sentinel is the
// model's hint that it can speak more conversationally.
//
// Format kept stable across releases: changing it requires a matching
// browser.md update and would break in-flight chat sessions.
const sidePanelInputSentinel = "[SIDE-PANEL CHAT] "

// turnKey is the canonical map key used by both
// assistantTurnTailers (registered tailers) and the
// stream_assistant_turns socket verb (subscription lookup). Mirrors
// the (session, agent_name) identity model from Part 2a.
func turnKey(sessionName, agentName string) string {
	return sessionName + "/" + agentName
}

// startAssistantTurnTailer registers and starts an assistant-turn
// tailer for (session, agent) bound to logPath. Idempotent: a second
// call for the same (session, agent) replaces the existing tailer,
// stopping the old one first.
//
// Called from startRegisteredAgent's AgentTypeBrowser branch (the same
// spot that builds the MCP config). The tailer must outlive the agent
// briefly so any in-flight ASSISTANT block can be flushed; Stop() is
// called when the agent process exits or when the daemon shuts down.
func (d *Daemon) startAssistantTurnTailer(sessionName, agentName, logPath string) {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	hadExisting := false
	if existing, ok := d.assistantTurnTailers[key]; ok {
		// Stop the prior tailer outside the lock to avoid holding
		// the map mutex across a 2s-bounded wait.
		hadExisting = true
		d.assistantTurnTailersMu.Unlock()
		existing.Stop()
		d.assistantTurnTailersMu.Lock()
	}
	// Use Info-level logging for the tailer's own diagnostics. The
	// smoke-test regression (replies invisible in side panel) was
	// untraceable from daemon.log because every internal hook was at
	// Debug. Info logs are bounded (~one publish per agent turn) and
	// directly answer the operator question "did the daemon see the
	// reply / hand it to the bridge?".
	broadcaster := newTurnBroadcaster(d.logger.Info)
	tailer := newAssistantTurnTailer(logPath, broadcaster, d.logger.Info)
	d.assistantTurnTailers[key] = tailer
	d.assistantTurnTailersMu.Unlock()
	tailer.Start(d.ctx)
	if hadExisting {
		d.logger.Info("assistantTurnTailer: replaced tailer for %s/%s (log=%s)", sessionName, agentName, logPath)
	} else {
		d.logger.Info("assistantTurnTailer: started tailer for %s/%s (log=%s)", sessionName, agentName, logPath)
	}
}

// stopAssistantTurnTailer stops and removes the tailer for
// (session, agent). No-op if none exists.
func (d *Daemon) stopAssistantTurnTailer(sessionName, agentName string) {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	tailer, ok := d.assistantTurnTailers[key]
	if ok {
		delete(d.assistantTurnTailers, key)
	}
	d.assistantTurnTailersMu.Unlock()
	if tailer != nil {
		tailer.Stop()
	}
}

// lookupAssistantTurnBroadcaster returns the broadcaster registered for
// (session, agent), or nil if none is active. Used by the
// stream_assistant_turns socket handler.
func (d *Daemon) lookupAssistantTurnBroadcaster(sessionName, agentName string) *turnBroadcaster {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	defer d.assistantTurnTailersMu.Unlock()
	if t, ok := d.assistantTurnTailers[key]; ok {
		return t.broadcaster
	}
	return nil
}

// stopAllAssistantTurnTailers tears down every active tailer.
// Called during daemon shutdown so the broadcasters close cleanly
// and any subscribed stream_assistant_turns clients see a Done frame
// instead of a hung connection.
func (d *Daemon) stopAllAssistantTurnTailers() {
	d.assistantTurnTailersMu.Lock()
	tailers := d.assistantTurnTailers
	d.assistantTurnTailers = make(map[string]*assistantTurnTailer)
	d.assistantTurnTailersMu.Unlock()
	for _, t := range tailers {
		t.Stop()
	}
}
