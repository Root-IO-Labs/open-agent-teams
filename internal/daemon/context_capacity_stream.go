// Package daemon — context_capacity_stream.go is the
// daemon→bridge→extension push channel for Part 5e Slice B (the 85%
// pill + 90% banner tiers). It complements context_capacity.go's
// Slice A (the 75% silent PTY hint + 95% synthetic-inject safety net)
// by giving the side panel a way to learn about capacity in real time
// without polling.
//
// Wire shape: a long-lived `stream_context_capacity` socket
// subscription scoped by (session, agent), mirroring the
// stream_assistant_turns pattern in assistant_turn_tailer.go +
// stream_handler.go. The bridge opens the subscription at startup
// (same lifecycle as streamAssistantTurns) and fans each frame out to
// connected side-panel WS clients via a new `context_capacity` WS
// frame.
//
// Tier semantics:
//
//	"ok"          → pct <  75 %   (clears any UI indicator)
//	"hint"        → pct >= 75 %   (silent PTY hint already sent in Slice A)
//	"amber"       → pct >= 85 %   (Status-tab amber pill)
//	"banner"      → pct >= 90 %   (user-visible banner w/ Compact/Reset)
//	"safety_net"  → pct >= 95 %   (synthetic inject auto-fires)
//
// Tier-crossing rule: a frame is only published when the agent's tier
// actually CHANGES. Going from "ok" → "hint" emits one frame; staying
// in "hint" emits nothing more. Going from "hint" → "ok" (after a
// successful compact_conversation) emits a clear-the-UI frame. This
// keeps the wire quiet during normal operation and ensures the side
// panel never sees stale UI state — every transition is reported.
//
// Why a separate stream from stream_assistant_turns / stream_agent_output:
//   - The frame cadence is dramatically different (one per tier
//     crossing, not one per turn / chunk).
//   - The semantic content is different (capacity, not text).
//   - Future capacity tiers (e.g. provider-specific cache-aware
//     budgets) extend this stream's tier enum without touching the
//     turn-feed schema.
//
// Restricted to AgentTypeAssistant by the socket handler — the
// workflow-helper AgentTypeBrowser doesn't surface side-panel chat
// UI, so a capacity pill would be meaningless there.

package daemon

import (
	"sync"
	"time"
)

// contextCapacityFrame is the wire shape sent over
// `stream_context_capacity`. One frame per tier crossing.
//
// Either the capacity fields are set (the normal case: Tier is
// non-empty) or Done/Err is set (terminal frame). Never both.
//
// Pct is a fraction in [0, 1], rounded to 4 decimals on the wire so
// the side panel can render "%d%%" without re-rounding. Used and
// Limit are the raw token counts the percentage was computed from --
// surfaced so the side panel can show "47,123 / 64,000 tokens" in
// the connection-details strip without a second daemon call.
type contextCapacityFrame struct {
	Pct   float64 `json:"pct,omitempty"`
	Tier  string  `json:"tier,omitempty"`
	Used  int64   `json:"used,omitempty"`
	Limit int64   `json:"limit,omitempty"`
	TS    string  `json:"ts,omitempty"`
	Done  bool    `json:"done,omitempty"`
	Err   string  `json:"error,omitempty"`
}

// Tier name constants. Stable strings on the wire — the extension
// and the bridge match against them by string compare. DO NOT
// rename without a coordinated cross-repo bump (the matching enum
// lives in oat-browser-agent's extension/src/types.ts / side-panel
// renderer).
const (
	capacityTierOK        = "ok"
	capacityTierHint      = "hint"
	capacityTierAmber     = "amber"
	capacityTierBanner    = "banner"
	capacityTierSafetyNet = "safety_net"
)

// Tier-crossing fractional thresholds. Mirrors the constants in
// context_capacity.go and ADDS the 85% / 90% thresholds that V1
// shipped as silent boundaries (used internally for tier name lookup
// even though no PTY action was wired to them in Slice A).
const (
	capacityThresholdHint      = 0.75
	capacityThresholdAmber     = 0.85
	capacityThresholdBanner    = 0.90
	capacityThresholdSafetyNet = 0.95
)

// tierForPct returns the tier name for a given fraction. Ordered
// boundary checks ensure each pct maps to exactly one tier (e.g.
// 0.95 → "safety_net", 0.86 → "amber"). The "ok" tier is the
// default below the lowest threshold and is what the side panel
// renders as "no indicator visible".
func tierForPct(pct float64) string {
	switch {
	case pct >= capacityThresholdSafetyNet:
		return capacityTierSafetyNet
	case pct >= capacityThresholdBanner:
		return capacityTierBanner
	case pct >= capacityThresholdAmber:
		return capacityTierAmber
	case pct >= capacityThresholdHint:
		return capacityTierHint
	default:
		return capacityTierOK
	}
}

// Per-subscriber buffer for the capacity stream. Capacity events are
// FAR less frequent than turn frames (one per tier crossing, maybe a
// handful per assistant session) so even a tiny buffer is fine.
// Drops at the buffer limit are logged but do not block the producer
// -- the side panel will catch up at the next tier crossing anyway.
const capacitySubscriberBuf = 4

// capacityBroadcaster fans out contextCapacityFrame values from the
// publish call site (driven by token-usage events in the daemon) to
// many subscribers (each stream_context_capacity connection from the
// bridge). Same shape as turnBroadcaster in assistant_turn_tailer.go;
// kept structurally identical so a future "broadcaster framework"
// refactor can absorb both behind one interface.
type capacityBroadcaster struct {
	mu          sync.Mutex
	closed      bool
	subscribers map[int]chan contextCapacityFrame
	nextID      int
	logf        func(format string, args ...any)
}

func newCapacityBroadcaster(logf func(format string, args ...any)) *capacityBroadcaster {
	return &capacityBroadcaster{
		subscribers: make(map[int]chan contextCapacityFrame),
		logf:        logf,
	}
}

// Subscribe returns a channel that receives every subsequent capacity
// frame plus a cancel func the caller must invoke when done. No replay
// of past frames -- a freshly-connected side panel either gets the
// NEXT tier crossing or learns the current tier via the synthesised
// "snapshot" frame the handler sends on connect (see
// publishSnapshot below).
func (b *capacityBroadcaster) Subscribe() (<-chan contextCapacityFrame, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan contextCapacityFrame)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan contextCapacityFrame, capacitySubscriberBuf)
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

// Publish broadcasts a capacity frame to all current subscribers.
// A slow subscriber whose buffer is full has the frame dropped; the
// producer never blocks. Drops are logged at Debug since the next
// tier crossing will re-deliver the equivalent state anyway.
func (b *capacityBroadcaster) Publish(frame contextCapacityFrame) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := make([]chan contextCapacityFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
			if b.logf != nil {
				b.logf("capacityBroadcaster: dropped frame for slow subscriber (tier=%s pct=%.2f)", frame.Tier, frame.Pct)
			}
		}
	}
}

// Close terminates all subscriptions. Sends a `Done: true` frame
// where possible (each subscriber gets one). Safe to call multiple
// times.
func (b *capacityBroadcaster) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := make([]chan contextCapacityFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.subscribers = make(map[int]chan contextCapacityFrame)
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- contextCapacityFrame{Done: true}:
		default:
		}
		close(ch)
	}
}

// lookupOrCreateCapacityBroadcaster returns the broadcaster for the
// (sessionName, agentName) tuple, creating it on first use. Stored
// on the Daemon (capacityBroadcasters map) keyed by the same composite
// the rest of the daemon uses elsewhere (sessionName/agentName).
//
// Unlike the assistant-turn tailer, capacity broadcasters do not
// require an active background goroutine -- the producer is the
// token-usage event handler which always exists when the assistant
// is alive. So we lazily create on first publish OR first subscribe
// and never tear down (the entries are tiny: a mutex + an empty map).
// They get GC'd when the daemon process exits.
func (d *Daemon) lookupOrCreateCapacityBroadcaster(sessionName, agentName string) *capacityBroadcaster {
	key := sessionName + "/" + agentName
	d.capacityBroadcastersMu.Lock()
	defer d.capacityBroadcastersMu.Unlock()
	if existing, ok := d.capacityBroadcasters[key]; ok {
		return existing
	}
	b := newCapacityBroadcaster(d.logger.Debug)
	d.capacityBroadcasters[key] = b
	return b
}

// lookupCapacityBroadcaster returns the broadcaster for the
// (sessionName, agentName) tuple WITHOUT creating one. Used by the
// stream handler so the broadcaster exists iff the publisher has
// actually fired at least once for this agent -- avoids creating a
// broadcaster for an agent that may not be an assistant.
func (d *Daemon) lookupCapacityBroadcaster(sessionName, agentName string) *capacityBroadcaster {
	key := sessionName + "/" + agentName
	d.capacityBroadcastersMu.Lock()
	defer d.capacityBroadcastersMu.Unlock()
	return d.capacityBroadcasters[key]
}

// publishCapacityFrameIfTierChanged builds a contextCapacityFrame
// from the supplied state and broadcasts it ONLY if the tier
// differs from the previously-observed tier for this agent. Returns
// (newTier, published). The lastTier map lives on
// contextCapacityState so the dedupe state is shared with the
// rest of the capacity machinery; this function locks the same mutex.
//
// Called from maybeNudgeContextCapacity (after the existing 75 %
// dedupe / PTY hint paths) so the wire stays in lockstep with the
// daemon's internal view of capacity. Calling more often than that
// is fine -- the tier-crossing dedupe means redundant calls are no-
// ops on the wire.
func (d *Daemon) publishCapacityFrameIfTierChanged(repoName, agentName, sessionName string, pct float64, used, limit int64) (newTier string, published bool) {
	if d.contextCap == nil {
		return "", false
	}
	tier := tierForPct(pct)
	key := agentKey(repoName, agentName)

	d.contextCap.mu.Lock()
	previous, _ := d.contextCap.lastTier[key]
	if previous == tier {
		d.contextCap.mu.Unlock()
		return tier, false
	}
	d.contextCap.lastTier[key] = tier
	d.contextCap.mu.Unlock()

	frame := contextCapacityFrame{
		Pct:   roundPct(pct),
		Tier:  tier,
		Used:  used,
		Limit: limit,
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	b := d.lookupOrCreateCapacityBroadcaster(sessionName, agentName)
	b.Publish(frame)
	return tier, true
}

// publishCapacitySnapshot pushes the CURRENT capacity state to one
// specific subscriber channel ONLY (via the broadcaster's Subscribe
// path that the caller already has). The stream handler invokes this
// right after the handshake so a freshly-connected side panel learns
// its starting tier without waiting for the next tier crossing.
//
// IMPORTANT: this is the ONE call site that publishes regardless of
// tier change -- specifically to seed a new subscriber. publishCapacity
// FrameIfTierChanged still owns the global dedupe; this just bypasses
// it for the snapshot edge case.
//
// Used + Limit are the snapshot values at the time of the call.
// Returns the constructed frame so the handler can write it directly
// on its own connection (avoids a producer→subscriber→handler hop
// for what's just a synchronous reply).
func capacitySnapshotFrame(pct float64, used, limit int64) contextCapacityFrame {
	return contextCapacityFrame{
		Pct:   roundPct(pct),
		Tier:  tierForPct(pct),
		Used:  used,
		Limit: limit,
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// roundPct rounds to 4 decimals so the wire shape is stable. Side-
// panel rendering reads pct*100 → integer, so the exact fractional
// value beyond 1% doesn't matter; we keep 4 decimals for consumers
// that might want sub-percent precision (e.g. metrics).
func roundPct(pct float64) float64 {
	if pct < 0 {
		return 0
	}
	if pct > 1 {
		return 1
	}
	// Round-half-up to 4 decimals.
	return float64(int64(pct*10000+0.5)) / 10000
}
