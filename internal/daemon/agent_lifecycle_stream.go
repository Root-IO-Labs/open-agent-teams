// Package daemon — agent_lifecycle_stream.go is the daemon→bridge→
// extension push channel for Part 7 Commit 7.4. It tells the side-
// panel about agent add/start/stop/remove events without polling, so
// the new Status-tab per-agent cards refresh reactively.
//
// Wire shape: a long-lived `stream_agent_lifecycle` socket
// subscription. ONE global broadcaster (no (session, agent) keying)
// because the side-panel renderer wants a single feed covering
// every agent across every repo — keyed per-(repo, agent) only at
// the consumer side via the frame's Repo+Agent fields.
//
// Frame kinds (stable strings on the wire — DO NOT rename without a
// coordinated cross-repo bump):
//
//   - "snapshot"        — replayed at subscribe time, one per
//     existing agent, so a freshly-connected panel
//     learns the full set without an extra
//     list_agents round-trip.
//   - "agent_added"     — handleAddAgent / start path created a
//     record.
//   - "agent_started"   — PID transitioned 0 → >0 (start / restart
//     completed successfully).
//   - "agent_stopped"   — handleStopAgent set PID to 0 with the
//     LastError marker.
//   - "agent_removed"   — handleRemoveAgent deleted the record.
//   - "emergency_stop"  — (Part 7 panic-redesign slice 3a, 2026-05-28)
//     global emergency-stop fired. Every bridge that sees this
//     frame must immediately invoke its local panicState.trigger(),
//     blocking all in-flight + future tool calls at the MCP layer.
//     Carries no per-agent identity — it's a process-wide signal,
//     not an agent-specific one. Optional Reason field describes
//     why (e.g. "sidepanel button"). Companion to the per-agent
//     PTY notice injection so even an LLM that ignores the bridge
//     block sees an explicit "user halted you — await new
//     instructions" message in its conversation context.
//   - "emergency_resume" — (Part 7 panic-redesign slice 3a)
//     global emergency-stop cleared. Bridges call panicState.resume()
//     to re-enable tool dispatch. NOT a license to retry — slice 1's
//     AGENT_PANIC error directive + slice 3a's PTY notice both tell
//     the LLM to wait for explicit user instructions before acting.
//
// Mirrors the structural pattern of capacityBroadcaster in
// context_capacity_stream.go — same Subscribe/Publish/Close API,
// same drop-when-full back-pressure policy. Kept structurally
// identical so a future "broadcaster framework" refactor can
// absorb both behind one interface.

package daemon

import (
	"sync"
	"time"
)

// Frame kind constants. Stable strings on the wire — the bridge
// and the extension match against them by string compare.
const (
	lifecycleKindSnapshot        = "snapshot"
	lifecycleKindAgentAdded      = "agent_added"
	lifecycleKindAgentStarted    = "agent_started"
	lifecycleKindAgentStopped    = "agent_stopped"
	lifecycleKindAgentRemoved    = "agent_removed"
	lifecycleKindEmergencyStop   = "emergency_stop"
	lifecycleKindEmergencyResume = "emergency_resume"
)

// agentLifecycleFrame is the wire shape sent over
// `stream_agent_lifecycle`. One frame per lifecycle event (plus
// the snapshot replay on connect).
//
// Either the agent fields are set (the normal case: Kind names a
// lifecycle event) or Done/Err is set (terminal frame). Never both.
//
// PID is 0 for "agent_stopped" and "agent_removed" — by definition
// no live process — and >0 for "agent_started" / "agent_added"
// with a backing process. The side panel uses this to drive the
// running/stopped/dead pill colour without a second daemon call.
//
// LastError carries the Part 7 Commit 7.1 "stopped by user" marker
// when Kind == "agent_stopped" with a user-initiated cause, OR a
// free-form crash-cause string when Kind == "agent_stopped"
// because the process died. The side panel pattern-matches the
// "stopped by user" literal to suppress the auto-restore-on-
// reload prompt.
type agentLifecycleFrame struct {
	Kind      string `json:"kind"`
	Repo      string `json:"repo,omitempty"`
	Agent     string `json:"agent,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Model     string `json:"model,omitempty"`
	LastError string `json:"last_error,omitempty"`
	TS        string `json:"ts,omitempty"`
	// Reason is set only on Kind == "emergency_stop" /
	// "emergency_resume" frames. Free-form short string surfaced
	// in audit logs + the bridge's panicState.reason. Omitted
	// (zero string elided by omitempty) on every other kind. The
	// extension may render this on the side-panel banner so users
	// see "stopped via side panel" vs "stopped via CLI".
	Reason string `json:"reason,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Err    string `json:"error,omitempty"`
}

// lifecycleSubscriberBuf is the per-subscriber channel buffer.
// Lifecycle events are rare (handful per session) so even a
// small buffer is fine; same trade-off as capacityBroadcaster.
const lifecycleSubscriberBuf = 16

// agentLifecycleBroadcaster fans out lifecycle frames from the
// publish call sites to many subscribers (each
// stream_agent_lifecycle connection from the bridge). Structurally
// identical to capacityBroadcaster — see that file's doc comment
// for the future-framework-refactor note.
type agentLifecycleBroadcaster struct {
	mu          sync.Mutex
	closed      bool
	subscribers map[int]chan agentLifecycleFrame
	nextID      int
	logf        func(format string, args ...any)
}

func newAgentLifecycleBroadcaster(logf func(format string, args ...any)) *agentLifecycleBroadcaster {
	return &agentLifecycleBroadcaster{
		subscribers: make(map[int]chan agentLifecycleFrame),
		logf:        logf,
	}
}

// Subscribe returns a channel that receives every subsequent
// lifecycle frame plus a cancel func the caller must invoke when
// done. No replay of past frames here — the stream handler
// synthesises a per-agent "snapshot" frame for every existing
// agent on connect (see handleStreamAgentLifecycle).
func (b *agentLifecycleBroadcaster) Subscribe() (<-chan agentLifecycleFrame, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan agentLifecycleFrame)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan agentLifecycleFrame, lifecycleSubscriberBuf)
	id := b.nextID
	b.nextID++
	b.subscribers[id] = ch
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(existing)
		}
	}
	return ch, cancel
}

// Publish broadcasts a lifecycle frame to all current
// subscribers. A slow subscriber whose buffer is full has the
// frame dropped; the producer never blocks. Drops are logged at
// Debug since the side panel periodically reconnects and gets a
// fresh snapshot.
func (b *agentLifecycleBroadcaster) Publish(frame agentLifecycleFrame) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := make([]chan agentLifecycleFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
			if b.logf != nil {
				b.logf("agentLifecycleBroadcaster: dropped frame for slow subscriber (kind=%s %s/%s)",
					frame.Kind, frame.Repo, frame.Agent)
			}
		}
	}
}

// Close terminates all subscriptions. Sends a Done:true frame
// where possible. Safe to call multiple times. Invoked from
// daemon Shutdown so connected bridges learn the daemon is going
// away rather than seeing a TCP-reset surprise.
func (b *agentLifecycleBroadcaster) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := make([]chan agentLifecycleFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.subscribers = make(map[int]chan agentLifecycleFrame)
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- agentLifecycleFrame{Kind: lifecycleKindSnapshot, Done: true}:
		default:
		}
		close(ch)
	}
}

// publishAgentLifecycle is the convenience wrapper every handler
// uses to emit a lifecycle frame. Stamps ts in RFC3339Nano so the
// extension can render absolute timestamps without doing its own
// clock-skew correction. Best-effort: nil broadcaster is a no-op
// (test daemons may construct without one).
//
// Why a wrapper instead of direct Publish at each call site: the
// frame has 5+ fields and call sites repeatedly forget one
// (model, last_error). Centralising the construction means a
// regression in one handler can't accidentally produce a frame
// missing a field — the wrapper signature lists every input.
func (d *Daemon) publishAgentLifecycle(
	kind, repo, agent, agentType string,
	pid int,
	model, lastError string,
) {
	if d.agentLifecycleBroadcaster == nil {
		return
	}
	d.agentLifecycleBroadcaster.Publish(agentLifecycleFrame{
		Kind:      kind,
		Repo:      repo,
		Agent:     agent,
		AgentType: agentType,
		PID:       pid,
		Model:     model,
		LastError: lastError,
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	})
}
