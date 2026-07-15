package daemon

import (
	"fmt"
	"strconv"
)

// This file owns the daemon-side lifecycle helpers for the per-agent
// assistantTurnTailer + turnBroadcaster pair. Kept separate from
// daemon.go so the bulk of daemon.go stays readable and Part 2g's
// concerns are easy to find in a code review.

// buildActiveTabPrefix returns the optional `[active-tab-id: <N>] `
// fragment the daemon inserts between sidePanelInputSentinel and the
// user's text on side-panel chat input (Part 4.K). Accepts the raw
// `active_tab_id` argument value as it arrives from the socket layer
// (interface{}: float64 from JSON, int / int64 from in-process tests,
// string from the dispatch table's stringy fallback). Anything else,
// or a non-positive id, returns "" — silent fall-through so an older
// side-panel build without the field continues to work.
//
// Kept in this file (not daemon.go) so all the side-panel-chat
// lifecycle helpers are colocated; the parser/tailer files import
// nothing from daemon.go.
func buildActiveTabPrefix(raw interface{}) string {
	if raw == nil {
		return ""
	}
	var tabID int64
	switch v := raw.(type) {
	case float64:
		tabID = int64(v)
	case int:
		tabID = int64(v)
	case int64:
		tabID = v
	case string:
		if parsed, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			tabID = parsed
		}
	}
	if tabID <= 0 {
		return ""
	}
	return fmt.Sprintf("[active-tab-id: %d] ", tabID)
}

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
//
// emitToolEvents should be true ONLY for AgentTypeAssistant: it makes
// the tailer publish TOOL/RESULT blocks as tool_start/tool_end
// activity frames. Browser agents already surface tool rows via the
// bridge's MCP hooks, so enabling it for them would double-render.
func (d *Daemon) startAssistantTurnTailer(repoName, sessionName, agentName, logPath string, emitToolEvents bool) {
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
	tailer := newAssistantTurnTailer(logPath, broadcaster, emitToolEvents, d.logger.Info)
	// Wire Layer 2 auto-recovery ONLY for assistants. emitToolEvents is
	// true iff agent.Type == AgentTypeAssistant (see the call sites), so
	// it doubles as the assistant gate here. Browser agents get a nil
	// callback (turn_end still publishes, just with recovering=false).
	if emitToolEvents {
		tailer.onTurnEnd = func(info turnEndInfo) bool {
			return d.maybeRecoverAssistantTurn(repoName, sessionName, agentName, info)
		}
	}
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

// armSidePanelAutoEmit flips the (session, agent) tailer's side-panel
// auto-emit flag on at message-delivery time. Called from the daemon's
// side-panel input paths (handleAgentInput + handleRouteUserMessage) the
// instant a user message is handed to the agent, so the agent's replies
// render even if the `[SIDE-PANEL CHAT]` sentinel lands in the agent's
// log out of order (relying on the sentinel alone can suppress a busy
// agent's turns). No-op if no tailer is registered yet — the
// sentinel-parse path still covers that case.
//
// resetRecovery: when true (default for new-work messages), clears the
// Layer-2 recovery budget. Pure status pings pass false so "are you
// stuck?" does not wipe the one remaining auto-nudge slot.
func (d *Daemon) armSidePanelAutoEmit(sessionName, agentName string, resetRecovery bool) {
	key := turnKey(sessionName, agentName)
	d.assistantTurnTailersMu.Lock()
	t := d.assistantTurnTailers[key]
	d.assistantTurnTailersMu.Unlock()
	if t != nil {
		t.markSidePanelActive()
	}
	if resetRecovery {
		// A genuine new-work user message ends any in-progress stuck
		// sequence. Recovery re-prompts are injected via backend.SendMessage
		// (NOT this path), so they never reset the budget.
		d.assistantRecovery.resetForUser(sessionName, agentName)
	}
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
