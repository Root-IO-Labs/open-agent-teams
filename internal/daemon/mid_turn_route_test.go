package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// TestInterruptMidTurnThenRoute pins Pillar B: while a turn is open,
// route_user_message interrupts first, then delivers the user text.
// Stuck wording adds diagnose prefix only (interrupt already decided).
func TestInterruptMidTurnThenRoute(t *testing.T) {
	prevSettle := midTurnInterruptSettle
	midTurnInterruptSettle = 20 * time.Millisecond
	t.Cleanup(func() { midTurnInterruptSettle = prevSettle })
	prevRate := routeRateLimitWindow
	routeRateLimitWindow = 0
	t.Cleanup(func() { routeRateLimitWindow = prevRate })

	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	fake := &routeTestBackend{}
	d.backend = fake

	repo := &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "sess-mid",
		Agents:      make(map[string]state.Agent),
	}
	if err := d.state.AddRepo("repo-mid", repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	ag := state.Agent{
		Type:       state.AgentTypeAssistant,
		WindowName: "win-asst",
		CreatedAt:  time.Now(),
		PID:        4242,
	}
	if err := d.state.AddAgent("repo-mid", "asst", ag); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Register a live-ish tailer and mark a turn in flight (no need to
	// Start — we only read the atomic latch).
	logPath := filepath.Join(t.TempDir(), "tool.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("touch log: %v", err)
	}
	tailer := newAssistantTurnTailer(logPath, newTurnBroadcaster(func(string, ...any) {}), true, nil)
	tailer.markSidePanelActive()
	key := turnKey(repo.SessionName, "asst")
	d.assistantTurnTailersMu.Lock()
	d.assistantTurnTailers[key] = tailer
	d.assistantTurnTailersMu.Unlock()
	t.Cleanup(func() {
		d.assistantTurnTailersMu.Lock()
		delete(d.assistantTurnTailers, key)
		d.assistantTurnTailersMu.Unlock()
	})

	t.Run("mid-turn hello interrupts then routes", func(t *testing.T) {
		fake.mu.Lock()
		fake.sent = nil
		fake.interrupts = 0
		fake.mu.Unlock()
		tailer.turnInFlight.Store(true)

		resp := d.handleRouteUserMessage(socket.Request{
			Command: "route_user_message",
			Args: map[string]interface{}{
				"repo":  "repo-mid",
				"agent": "asst",
				"text":  "hello?",
			},
		})
		if !resp.Success {
			t.Fatalf("route failed: %v", resp.Error)
		}
		if fake.interruptCount() != 1 {
			t.Fatalf("expected 1 interrupt, got %d", fake.interruptCount())
		}
		calls := fake.calls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 SendMessage, got %d", len(calls))
		}
		if !strings.Contains(calls[0].Message, interruptContinuePrefix) &&
			!strings.Contains(calls[0].Message, statusDiagnosePrefix) {
			// hello? is pure status → diagnose prefix (not interruptContinue)
			t.Fatalf("message missing expected prefix: %q", calls[0].Message)
		}
		if !strings.Contains(calls[0].Message, "hello?") {
			t.Fatalf("user text missing: %q", calls[0].Message)
		}
	})

	t.Run("stuck wording uses diagnose prefix (still interrupts)", func(t *testing.T) {
		fake.mu.Lock()
		fake.sent = nil
		fake.interrupts = 0
		fake.mu.Unlock()
		tailer.turnInFlight.Store(true)

		resp := d.handleRouteUserMessage(socket.Request{
			Command: "route_user_message",
			Args: map[string]interface{}{
				"repo":  "repo-mid",
				"agent": "asst",
				"text":  "you seem stuck",
			},
		})
		if !resp.Success {
			t.Fatalf("route failed: %v", resp.Error)
		}
		if fake.interruptCount() != 1 {
			t.Fatalf("expected interrupt, got %d", fake.interruptCount())
		}
		msg := fake.calls()[0].Message
		if !strings.HasPrefix(msg, statusDiagnosePrefix) {
			t.Fatalf("stuck ask should get diagnose prefix only; got %q", msg)
		}
		if strings.Contains(msg, interruptContinuePrefix) {
			t.Fatalf("stuck ask should not also get interruptContinuePrefix")
		}
	})

	t.Run("idle (no turn) does not interrupt", func(t *testing.T) {
		fake.mu.Lock()
		fake.sent = nil
		fake.interrupts = 0
		fake.mu.Unlock()
		tailer.turnInFlight.Store(false)

		resp := d.handleRouteUserMessage(socket.Request{
			Command: "route_user_message",
			Args: map[string]interface{}{
				"repo":  "repo-mid",
				"agent": "asst",
				"text":  "also map the Feed tab",
			},
		})
		if !resp.Success {
			t.Fatalf("route failed: %v", resp.Error)
		}
		if fake.interruptCount() != 0 {
			t.Fatalf("idle send must not interrupt; got %d", fake.interruptCount())
		}
		msg := fake.calls()[0].Message
		if strings.Contains(msg, interruptContinuePrefix) {
			t.Fatalf("idle non-stuck send must not get interruptContinuePrefix")
		}
	})

	t.Run("mid-turn add-on gets interruptContinuePrefix", func(t *testing.T) {
		fake.mu.Lock()
		fake.sent = nil
		fake.interrupts = 0
		fake.mu.Unlock()
		tailer.turnInFlight.Store(true)

		resp := d.handleRouteUserMessage(socket.Request{
			Command: "route_user_message",
			Args: map[string]interface{}{
				"repo":  "repo-mid",
				"agent": "asst",
				"text":  "also map the Feed tab",
			},
		})
		if !resp.Success {
			t.Fatalf("route failed: %v", resp.Error)
		}
		if fake.interruptCount() != 1 {
			t.Fatalf("expected interrupt, got %d", fake.interruptCount())
		}
		msg := fake.calls()[0].Message
		if !strings.HasPrefix(msg, interruptContinuePrefix) {
			t.Fatalf("add-on mid-turn should get interruptContinuePrefix; got %q", msg)
		}
	})
}
