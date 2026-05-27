// Part 7 Commit 7.4: agentLifecycleBroadcaster + the handler-side
// publish hooks. Mirrors the structural shape of the existing
// context_capacity broadcaster tests but pins the
// add/start/stop/remove event flow that the side-panel Status-tab
// cards consume.

package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// drain returns every frame currently buffered on the channel.
// Bounded loop so a stuck producer can't hang the test.
func drainLifecycleChannel(ch <-chan agentLifecycleFrame, deadline time.Duration) []agentLifecycleFrame {
	var out []agentLifecycleFrame
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-time.After(20 * time.Millisecond):
			// No more frames within a short quiet window — treat
			// the queue as drained. This is enough quiet time
			// even on a CI box; the producer side is synchronous
			// in handleAddAgent / handleStopAgent so by the time
			// the handler returned the frame is on the channel.
			return out
		case <-timeout.C:
			return out
		}
	}
}

// TestAgentLifecycleBroadcaster_BasicPublish_Part7Commit4 pins the
// Subscribe/Publish/Close contract without any daemon scaffolding.
func TestAgentLifecycleBroadcaster_BasicPublish_Part7Commit4(t *testing.T) {
	b := newAgentLifecycleBroadcaster(nil)
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(agentLifecycleFrame{
		Kind:  lifecycleKindAgentAdded,
		Repo:  "test-repo",
		Agent: "personal",
	})
	frames := drainLifecycleChannel(ch, 100*time.Millisecond)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d: %+v", len(frames), frames)
	}
	if frames[0].Kind != lifecycleKindAgentAdded {
		t.Errorf("kind = %q, want %q", frames[0].Kind, lifecycleKindAgentAdded)
	}
	if frames[0].Repo != "test-repo" {
		t.Errorf("repo = %q, want test-repo", frames[0].Repo)
	}
}

// TestAgentLifecycleBroadcaster_MultiSubscriber_Part7Commit4 pins
// the fan-out: N subscribers all see each frame.
func TestAgentLifecycleBroadcaster_MultiSubscriber_Part7Commit4(t *testing.T) {
	b := newAgentLifecycleBroadcaster(nil)
	const N = 4
	chans := make([]<-chan agentLifecycleFrame, N)
	cancels := make([]func(), N)
	for i := 0; i < N; i++ {
		chans[i], cancels[i] = b.Subscribe()
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	b.Publish(agentLifecycleFrame{Kind: lifecycleKindAgentStarted, Agent: "shared"})

	for i, ch := range chans {
		frames := drainLifecycleChannel(ch, 100*time.Millisecond)
		if len(frames) != 1 || frames[0].Agent != "shared" {
			t.Errorf("subscriber %d: expected one 'shared' frame, got %+v", i, frames)
		}
	}
}

// TestAgentLifecycleBroadcaster_DropsOnSlow_Part7Commit4 pins the
// non-blocking-producer contract: a subscriber that never reads
// has its frames silently dropped after its buffer fills. The
// fast subscriber is drained concurrently so its channel never
// overflows, proving that one slow subscriber does NOT starve
// others. This is the contract the real bridge-side stream
// reader relies on — a single misbehaving WS client mustn't
// drop frames for the well-behaved ones.
func TestAgentLifecycleBroadcaster_DropsOnSlow_Part7Commit4(t *testing.T) {
	dropCount := 0
	var dropMu sync.Mutex
	logf := func(format string, args ...any) {
		dropMu.Lock()
		defer dropMu.Unlock()
		if strings.Contains(format, "dropped frame") {
			dropCount++
		}
	}
	b := newAgentLifecycleBroadcaster(logf)
	slowCh, slowCancel := b.Subscribe()
	defer slowCancel()
	fastCh, fastCancel := b.Subscribe()
	defer fastCancel()
	_ = slowCh // intentionally never read

	// Concurrent draining of the fast subscriber so its channel
	// never overflows during publish. Use a count + done channel
	// instead of WaitGroup so the goroutine can exit cleanly when
	// the test concludes.
	const total = lifecycleSubscriberBuf + 8
	fastSeen := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for fastSeen < total {
			select {
			case <-fastCh:
				fastSeen++
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()

	for i := 0; i < total; i++ {
		b.Publish(agentLifecycleFrame{Kind: lifecycleKindAgentAdded})
		// Give the fast consumer goroutine a chance to drain
		// between publishes. The producer is otherwise so much
		// faster than the channel receiver that the fast
		// subscriber's buffer fills BEFORE the goroutine wakes,
		// which would make the test flake in the same way as
		// the slow subscriber. A sub-millisecond yield is plenty.
		time.Sleep(time.Millisecond)
	}

	<-done
	if fastSeen != total {
		t.Errorf("fast subscriber missing frames: got %d, want %d", fastSeen, total)
	}
	dropMu.Lock()
	defer dropMu.Unlock()
	if dropCount == 0 {
		t.Error("expected logf to record at least one drop for the slow subscriber")
	}
}

// TestAgentLifecycleBroadcaster_Close_Part7Commit4 pins the
// terminal Done:true frame + channel close on broadcaster
// shutdown.
func TestAgentLifecycleBroadcaster_Close_Part7Commit4(t *testing.T) {
	b := newAgentLifecycleBroadcaster(nil)
	ch, _ := b.Subscribe()
	b.Close()
	// First frame should be the Done sentinel; second receive
	// should observe the close (ok=false).
	frame, ok := <-ch
	if !ok {
		t.Fatal("channel closed before Done frame was delivered")
	}
	if !frame.Done {
		t.Errorf("expected Done frame, got %+v", frame)
	}
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after Done frame")
	}

	// Close is idempotent.
	b.Close()
}

// TestHandleStopAgent_PublishesLifecycle_Part7Commit4 confirms
// handleStopAgent emits an agent_stopped frame after mutating
// state. This is the cross-section that proves the broadcaster
// wiring works end-to-end through the real handler.
func TestHandleStopAgent_PublishesLifecycle_Part7Commit4(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	if err := d.state.AddRepo("test-repo", &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := d.state.AddAgent("test-repo", "personal", state.Agent{
		Type:       state.AgentTypeAssistant,
		WindowName: "personal",
		PID:        12345,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	ch, cancel := d.agentLifecycleBroadcaster.Subscribe()
	defer cancel()

	resp := d.handleStopAgent(socket.Request{
		Command: "stop_agent",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "personal",
		},
	})
	if !resp.Success {
		t.Fatalf("handleStopAgent: %s", resp.Error)
	}

	frames := drainLifecycleChannel(ch, 200*time.Millisecond)
	var stopped *agentLifecycleFrame
	for i := range frames {
		if frames[i].Kind == lifecycleKindAgentStopped {
			stopped = &frames[i]
			break
		}
	}
	if stopped == nil {
		t.Fatalf("expected agent_stopped frame, got: %+v", frames)
	}
	if stopped.Repo != "test-repo" || stopped.Agent != "personal" {
		t.Errorf("stopped frame mismatch: %+v", stopped)
	}
	if stopped.PID != 0 {
		t.Errorf("stopped frame PID = %d, want 0", stopped.PID)
	}
	if stopped.LastError != "stopped by user" {
		t.Errorf("stopped frame LastError = %q, want %q", stopped.LastError, "stopped by user")
	}
}

// TestHandleRemoveAgent_PublishesLifecycle_Part7Commit4 confirms
// handleRemoveAgent emits an agent_removed frame with the reason
// encoded into LastError so the side panel can distinguish a
// user-initiated cleanup from a daemon-driven removal.
func TestHandleRemoveAgent_PublishesLifecycle_Part7Commit4(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	if err := d.state.AddRepo("test-repo", &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := d.state.AddAgent("test-repo", "personal", state.Agent{
		Type:       state.AgentTypeAssistant,
		WindowName: "personal",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	ch, cancel := d.agentLifecycleBroadcaster.Subscribe()
	defer cancel()

	resp := d.handleRemoveAgent(socket.Request{
		Command: "remove_agent",
		Args: map[string]interface{}{
			"repo":   "test-repo",
			"agent":  "personal",
			"reason": RemovalReasonUserCleanupAfterPause,
		},
	})
	if !resp.Success {
		t.Fatalf("handleRemoveAgent: %s", resp.Error)
	}

	frames := drainLifecycleChannel(ch, 200*time.Millisecond)
	var removed *agentLifecycleFrame
	for i := range frames {
		if frames[i].Kind == lifecycleKindAgentRemoved {
			removed = &frames[i]
			break
		}
	}
	if removed == nil {
		t.Fatalf("expected agent_removed frame, got: %+v", frames)
	}
	if !strings.Contains(removed.LastError, RemovalReasonUserCleanupAfterPause) {
		t.Errorf("agent_removed frame should carry reason in LastError; got %q", removed.LastError)
	}
}
