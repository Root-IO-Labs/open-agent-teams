// Tests for the Part 5e Slice B context-capacity streaming pieces:
// tierForPct truth table, roundPct, capacityBroadcaster lifecycle,
// publishCapacityFrameIfTierChanged dedupe semantics, and the
// snapshot frame helper. The socket-level handler is covered by
// integration tests in the bridge repo (oat-browser-agent's
// daemon-socket-client + context-capacity-stream tests) which talks
// to a real daemon socket -- duplicating that here would require
// stubbing the streamHandler's net.Conn input.

package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// tierForPct is the wire-stable mapping from pct → tier name. The
// boundaries are inclusive at the low end (`>=`), so 0.75 → "hint",
// 0.85 → "amber", 0.90 → "banner", 0.95 → "safety_net". Anything
// below 75% is "ok" (clears the UI). This test is the contract:
// changing a tier name OR a threshold without updating the
// corresponding extension UI is a cross-repo break, so the
// truth-table failure here is exactly the warning we want.
func TestTierForPct_Part5eSliceB(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{-0.5, capacityTierOK},
		{0, capacityTierOK},
		{0.10, capacityTierOK},
		{0.749999, capacityTierOK},
		{0.75, capacityTierHint},
		{0.80, capacityTierHint},
		{0.849999, capacityTierHint},
		{0.85, capacityTierAmber},
		{0.86, capacityTierAmber},
		{0.899999, capacityTierAmber},
		{0.90, capacityTierBanner},
		{0.94, capacityTierBanner},
		{0.949999, capacityTierBanner},
		{0.95, capacityTierSafetyNet},
		{0.99, capacityTierSafetyNet},
		{1.0, capacityTierSafetyNet},
		{1.5, capacityTierSafetyNet},
	}
	for _, tc := range cases {
		if got := tierForPct(tc.pct); got != tc.want {
			t.Errorf("tierForPct(%.6f) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

// roundPct keeps the wire shape stable: 4 decimal digits,
// clamped to [0, 1]. The side panel renders pct*100 as an integer
// percent, so beyond 4 decimals doesn't matter; rounding ensures
// the wire is byte-stable for testers + metric consumers that
// might diff successive frames.
func TestRoundPct_Part5eSliceB(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{-0.1, 0},
		{0, 0},
		{0.123456789, 0.1235}, // rounded up (round-half-up)
		{0.12344, 0.1234},     // rounded down
		{0.75, 0.75},
		{0.85, 0.85},
		{1.0, 1.0},
		{1.5, 1.0},
	}
	for _, tc := range cases {
		if got := roundPct(tc.in); got != tc.want {
			t.Errorf("roundPct(%.9f) = %.9f, want %.9f", tc.in, got, tc.want)
		}
	}
}

// capacitySnapshotFrame populates the wire shape from raw inputs.
// Pins: tier derivation matches tierForPct, pct gets rounded, used
// + limit are plumbed verbatim, TS is non-empty (RFC3339Nano).
func TestCapacitySnapshotFrame_Part5eSliceB(t *testing.T) {
	frame := capacitySnapshotFrame(0.876, 56_321, 64_000)
	if frame.Tier != capacityTierAmber {
		t.Errorf("Tier = %q, want %q (87.6%% → amber)", frame.Tier, capacityTierAmber)
	}
	if frame.Pct != 0.876 {
		t.Errorf("Pct = %v, want 0.876 (input is already at 3-decimal precision)", frame.Pct)
	}
	if frame.Used != 56_321 {
		t.Errorf("Used = %d, want 56321", frame.Used)
	}
	if frame.Limit != 64_000 {
		t.Errorf("Limit = %d, want 64000", frame.Limit)
	}
	if frame.TS == "" {
		t.Error("TS is empty -- should be RFC3339Nano timestamp")
	}
	if _, err := time.Parse(time.RFC3339Nano, frame.TS); err != nil {
		t.Errorf("TS does not parse as RFC3339Nano: %v (got %q)", err, frame.TS)
	}
	if frame.Done {
		t.Error("Done = true -- snapshot frames must never be terminal")
	}
	if frame.Err != "" {
		t.Errorf("Err = %q -- snapshot frames must never be error frames", frame.Err)
	}
}

// capacityBroadcaster lifecycle: Subscribe returns a fresh channel
// per call; Publish reaches every subscriber; Close terminates all
// subscriptions with a Done:true frame (best-effort) AND closes the
// channels; subsequent Subscribe returns an already-closed channel.
func TestCapacityBroadcaster_Lifecycle_Part5eSliceB(t *testing.T) {
	b := newCapacityBroadcaster(nil)

	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel1()
	defer cancel2()

	frame := contextCapacityFrame{Pct: 0.80, Tier: capacityTierHint, Used: 51_200, Limit: 64_000}
	b.Publish(frame)

	for i, ch := range []<-chan contextCapacityFrame{ch1, ch2} {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Errorf("ch%d: channel closed before receiving frame", i+1)
				continue
			}
			if got.Tier != capacityTierHint || got.Pct != 0.80 {
				t.Errorf("ch%d: got %+v, want tier=hint pct=0.80", i+1, got)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("ch%d: did not receive frame within 100ms", i+1)
		}
	}

	b.Close()

	// After Close, every existing subscriber should observe channel
	// close. The Done:true frame is best-effort (sent when buffer
	// has room); the close itself is guaranteed.
	for i, ch := range []<-chan contextCapacityFrame{ch1, ch2} {
		drained := false
		for !drained {
			select {
			case _, ok := <-ch:
				if !ok {
					drained = true
				}
			case <-time.After(100 * time.Millisecond):
				t.Errorf("ch%d: channel not drained/closed after Close()", i+1)
				drained = true
			}
		}
	}

	// Subscribe after Close returns an already-closed channel so
	// the caller's select doesn't deadlock.
	ch3, _ := b.Subscribe()
	select {
	case _, ok := <-ch3:
		if ok {
			t.Error("Subscribe after Close: channel delivered a value, want already-closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Subscribe after Close: channel is not closed (would deadlock callers)")
	}
}

// Cancel removes the subscriber so subsequent Publish does not reach
// it. Pins the leak-prevention contract (a side panel that closes
// must not keep the broadcaster's map growing).
func TestCapacityBroadcaster_Cancel_Part5eSliceB(t *testing.T) {
	b := newCapacityBroadcaster(nil)

	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	cancel1()

	// Drain any Done frame Close might have queued -- cancel1 just
	// closes ch1's channel, no Done frame is sent.
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("ch1: received a frame after cancel -- should be closed-only")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("ch1: channel not closed after cancel")
	}

	b.Publish(contextCapacityFrame{Pct: 0.90, Tier: capacityTierBanner})

	select {
	case got := <-ch2:
		if got.Tier != capacityTierBanner {
			t.Errorf("ch2: got %+v, want tier=banner (ch1 was cancelled but ch2 should still receive)", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("ch2: did not receive frame within 100ms")
	}
}

// Slow-subscriber drop: a subscriber whose buffer is full has the
// frame dropped -- producer never blocks. Pins the back-pressure
// contract called out in capacityBroadcaster docs.
func TestCapacityBroadcaster_DropOnSlowSubscriber_Part5eSliceB(t *testing.T) {
	logged := make([]string, 0, 4)
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged = append(logged, format)
	}
	b := newCapacityBroadcaster(logf)
	defer b.Close()

	ch, cancel := b.Subscribe()
	defer cancel()

	// capacitySubscriberBuf is 4. Fill the buffer + one extra,
	// without draining. The extra Publish MUST NOT block, and the
	// drop logger MUST fire at least once.
	for i := 0; i < capacitySubscriberBuf+3; i++ {
		done := make(chan struct{})
		go func() {
			b.Publish(contextCapacityFrame{Pct: 0.80, Tier: capacityTierHint})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Publish #%d blocked > 200ms -- producer should never block on a slow subscriber", i+1)
		}
	}

	// Now drain to confirm the channel is at capacity (not deadlocked).
	drained := 0
loop:
	for drained < capacitySubscriberBuf {
		select {
		case <-ch:
			drained++
		case <-time.After(100 * time.Millisecond):
			break loop
		}
	}
	if drained != capacitySubscriberBuf {
		t.Errorf("drained %d frames, want %d (buffer should have been full)", drained, capacitySubscriberBuf)
	}

	logMu.Lock()
	count := len(logged)
	logMu.Unlock()
	if count == 0 {
		t.Error("logger never fired -- drop path is not actually being exercised")
	}
}

// publishCapacityFrameIfTierChanged is the dedupe layer that ensures
// the wire only emits frames when the tier ACTUALLY changes. Pins:
//   - First call for an agent publishes (any tier, including "ok").
//   - Subsequent call at the same tier does NOT publish.
//   - Call at a DIFFERENT tier (in either direction: climbing or
//     dropping back) DOES publish. The drop-back case is the
//     "clear the UI" signal after a successful compact.
//   - The broadcaster is created lazily on first publish; per-agent
//     state is isolated (one agent crossing a tier does not affect
//     another).
func TestPublishCapacityFrameIfTierChanged_Part5eSliceB(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// First call at 80% (hint) for agent A. Should publish.
	_, sub1, sub1Cancel := subscribeForTest(t, d, "sessA", "agentA")
	defer sub1Cancel()

	tier, published := d.publishCapacityFrameIfTierChanged("repoA", "agentA", "sessA", 0.80, 51_200, 64_000)
	if tier != capacityTierHint || !published {
		t.Errorf("first call: tier=%q published=%v, want tier=hint published=true", tier, published)
	}
	expectFrameWithin(t, sub1, "first call (hint)", 100*time.Millisecond, capacityTierHint, 0.80, 51_200, 64_000)

	// Same-tier follow-up call: must NOT publish.
	tier, published = d.publishCapacityFrameIfTierChanged("repoA", "agentA", "sessA", 0.82, 52_500, 64_000)
	if tier != capacityTierHint || published {
		t.Errorf("same-tier call: tier=%q published=%v, want tier=hint published=false", tier, published)
	}
	expectNoFrameWithin(t, sub1, "same-tier (hint→hint)", 50*time.Millisecond)

	// Tier climb to amber: must publish.
	tier, published = d.publishCapacityFrameIfTierChanged("repoA", "agentA", "sessA", 0.87, 55_680, 64_000)
	if tier != capacityTierAmber || !published {
		t.Errorf("amber climb: tier=%q published=%v, want tier=amber published=true", tier, published)
	}
	expectFrameWithin(t, sub1, "amber climb", 100*time.Millisecond, capacityTierAmber, 0.87, 55_680, 64_000)

	// Tier drop back to ok (post-compact): must publish. This is
	// the load-bearing "clear the UI" signal -- without it the
	// side panel would keep showing the amber pill after the
	// assistant successfully compacted.
	tier, published = d.publishCapacityFrameIfTierChanged("repoA", "agentA", "sessA", 0.30, 19_200, 64_000)
	if tier != capacityTierOK || !published {
		t.Errorf("drop to ok: tier=%q published=%v, want tier=ok published=true", tier, published)
	}
	expectFrameWithin(t, sub1, "drop to ok", 100*time.Millisecond, capacityTierOK, 0.30, 19_200, 64_000)

	// Per-agent isolation: a SECOND agent's first call publishes
	// even though agent A is already at "ok" (the agent A tier
	// state must not leak into agent B's dedupe).
	_, sub2, sub2Cancel := subscribeForTest(t, d, "sessB", "agentB")
	defer sub2Cancel()
	tier, published = d.publishCapacityFrameIfTierChanged("repoB", "agentB", "sessB", 0.92, 58_880, 64_000)
	if tier != capacityTierBanner || !published {
		t.Errorf("agent B isolation: tier=%q published=%v, want tier=banner published=true", tier, published)
	}
	expectFrameWithin(t, sub2, "agent B isolation", 100*time.Millisecond, capacityTierBanner, 0.92, 58_880, 64_000)

	// Agent A's subscriber must NOT have seen agent B's frame.
	expectNoFrameWithin(t, sub1, "cross-agent isolation", 50*time.Millisecond)
}

// publishCapacityFrameIfTierChanged is a no-op when contextCap is
// nil (defensive -- ensures a not-fully-initialized daemon can't
// panic on this path).
func TestPublishCapacityFrameIfTierChanged_NilContextCap_Part5eSliceB(t *testing.T) {
	d := &Daemon{} // intentionally minimal -- no contextCap
	tier, published := d.publishCapacityFrameIfTierChanged("r", "a", "s", 0.80, 1, 2)
	if tier != "" || published {
		t.Errorf("nil contextCap: tier=%q published=%v, want \"\" / false (defensive no-op)", tier, published)
	}
}

// lookupCapacityBroadcaster returns nil if no publish or subscribe
// has happened for the (session, agent) -- the create-on-demand
// pattern lives in lookupOrCreate, not lookup.
func TestLookupCapacityBroadcaster_NilBeforeUse_Part5eSliceB(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	if got := d.lookupCapacityBroadcaster("sessUnused", "agentUnused"); got != nil {
		t.Errorf("lookupCapacityBroadcaster before use = %v, want nil", got)
	}

	b1 := d.lookupOrCreateCapacityBroadcaster("sessUsed", "agentUsed")
	if b1 == nil {
		t.Fatal("lookupOrCreate returned nil -- it MUST create on first call")
	}

	// Subsequent lookup-only returns the same broadcaster.
	if got := d.lookupCapacityBroadcaster("sessUsed", "agentUsed"); got != b1 {
		t.Errorf("lookupCapacityBroadcaster after create = %v, want same instance %v", got, b1)
	}

	// Repeated lookupOrCreate returns the same instance (no churn).
	b2 := d.lookupOrCreateCapacityBroadcaster("sessUsed", "agentUsed")
	if b2 != b1 {
		t.Errorf("lookupOrCreate is not idempotent: first=%v second=%v", b1, b2)
	}
}

// maybeNudgeContextCapacity now invokes publishCapacityFrameIfTierChanged
// even for sub-75% pcts (the "clear the UI" signal). This test verifies
// the integration: an assistant going from 80% → 30% emits BOTH the
// climb-to-hint frame AND the drop-to-ok frame on the wire. Without
// the early-return relaxation in Slice B this would skip the second.
func TestMaybeNudgeContextCapacity_EmitsClearFrame_Part5eSliceB(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	const repo = "_assistant-personal"
	const agent = "personal"
	if err := d.state.AddRepo(repo, &state.Repository{SessionName: repo, Agents: map[string]state.Agent{}}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeAssistant, PID: 1234}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	_, sub, subCancel := subscribeForTest(t, d, repo, agent)
	defer subCancel()

	// Climb to hint (80%).
	d.maybeNudgeContextCapacity(repo, agent, state.Agent{
		Type:        state.AgentTypeAssistant,
		TotalTokens: 26_000,
		Model:       "", // forces 32K fallback → 80%
	})
	expectFrameWithin(t, sub, "climb to hint", 100*time.Millisecond, capacityTierHint, 0, 26_000, 32_000)

	// Drop back to 20% (after a successful compact). MUST emit a
	// clear-the-UI frame even though it's below the contextTierHint
	// threshold the PTY-hint path uses for its own gating.
	d.maybeNudgeContextCapacity(repo, agent, state.Agent{
		Type:        state.AgentTypeAssistant,
		TotalTokens: 6_400,
		Model:       "",
	})
	expectFrameWithin(t, sub, "drop to ok", 100*time.Millisecond, capacityTierOK, 0, 6_400, 32_000)
}

// subscribeForTest wires a fresh subscriber to the (session, agent)
// broadcaster. Returns the broadcaster, the channel, and a cancel
// fn the caller MUST defer.
func subscribeForTest(t *testing.T, d *Daemon, session, agent string) (*capacityBroadcaster, <-chan contextCapacityFrame, func()) {
	t.Helper()
	b := d.lookupOrCreateCapacityBroadcaster(session, agent)
	ch, cancel := b.Subscribe()
	return b, ch, cancel
}

// expectFrameWithin reads the next frame off ch with a timeout and
// asserts tier + used + limit. Skips pct check when wantPct==0 so
// the maybeNudge test (where pct is derived from model + tokens)
// can pass without coupling to floating-point computation.
func expectFrameWithin(t *testing.T, ch <-chan contextCapacityFrame, label string, timeout time.Duration, wantTier string, wantPct float64, wantUsed, wantLimit int64) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Errorf("%s: channel closed before frame", label)
			return
		}
		if got.Tier != wantTier {
			t.Errorf("%s: Tier = %q, want %q", label, got.Tier, wantTier)
		}
		if wantPct != 0 && got.Pct != wantPct {
			t.Errorf("%s: Pct = %v, want %v", label, got.Pct, wantPct)
		}
		if got.Used != wantUsed {
			t.Errorf("%s: Used = %d, want %d", label, got.Used, wantUsed)
		}
		if got.Limit != wantLimit {
			t.Errorf("%s: Limit = %d, want %d", label, got.Limit, wantLimit)
		}
	case <-time.After(timeout):
		t.Errorf("%s: no frame received within %s", label, timeout)
	}
}

func expectNoFrameWithin(t *testing.T, ch <-chan contextCapacityFrame, label string, timeout time.Duration) {
	t.Helper()
	select {
	case got := <-ch:
		t.Errorf("%s: unexpected frame %+v -- dedupe should have suppressed", label, got)
	case <-time.After(timeout):
	}
}
