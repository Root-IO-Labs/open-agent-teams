package daemon

import (
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestNudgeAgentsInRepo_ChatCapableSkip confirms that the Part 8
// Commit 8.7 defense-in-depth early-skip guard is reached for
// AgentTypeBrowser + AgentTypeAssistant, and that the one-time
// debug-log tracking map records both. The wake loop's pre-existing
// `default: continue` switch arm already excluded these types; this
// test pins the explicit early-skip path so a future refactor that
// inadvertently widens the default arm still has unit-test coverage
// for the chat-capable exclusion.
func TestNudgeAgentsInRepo_ChatCapableSkip(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	const repoName = "repo-chat"
	now := time.Now()
	addTestRepo(t, d, repoName, map[string]state.Agent{
		"firefly-browser":  {Type: state.AgentTypeBrowser, WindowName: "w1", CreatedAt: now, PID: 1},
		"helper-assistant": {Type: state.AgentTypeAssistant, WindowName: "w2", CreatedAt: now, PID: 2},
		"alpha-fox-worker": {Type: state.AgentTypeWorker, WindowName: "w3", CreatedAt: now, PID: 3},
	})

	repo, ok := d.state.GetRepo(repoName)
	if !ok {
		t.Fatalf("GetRepo(%q): not found", repoName)
	}

	// First sweep — the skip path fires for both chat-capable agents
	// AND populates the dedup map.
	d.nudgeAgentsInRepo(repoName, repo, now)

	d.chatCapableNudgeSkipLoggedMu.Lock()
	firstSweep := len(d.chatCapableNudgeSkipLogged)
	gotBrowser := d.chatCapableNudgeSkipLogged[repoName+"/firefly-browser"]
	gotAssistant := d.chatCapableNudgeSkipLogged[repoName+"/helper-assistant"]
	gotWorker := d.chatCapableNudgeSkipLogged[repoName+"/alpha-fox-worker"]
	d.chatCapableNudgeSkipLoggedMu.Unlock()

	if firstSweep != 2 {
		t.Errorf("expected 2 chat-capable agents logged after first sweep, got %d", firstSweep)
	}
	if !gotBrowser {
		t.Error("browser agent not in chatCapableNudgeSkipLogged after sweep")
	}
	if !gotAssistant {
		t.Error("assistant agent not in chatCapableNudgeSkipLogged after sweep")
	}
	if gotWorker {
		t.Error("worker agent should NOT be in chatCapableNudgeSkipLogged (only chat-capable types route through the skip log)")
	}

	// Second sweep — the dedup map prevents a second log entry from
	// being recorded. Map size stays at 2 (no re-entries), confirming
	// the "once per agent per daemon process lifetime" semantics.
	d.nudgeAgentsInRepo(repoName, repo, now.Add(2*time.Minute))

	d.chatCapableNudgeSkipLoggedMu.Lock()
	secondSweep := len(d.chatCapableNudgeSkipLogged)
	d.chatCapableNudgeSkipLoggedMu.Unlock()
	if secondSweep != 2 {
		t.Errorf("expected dedup to keep map size at 2 across sweeps, got %d", secondSweep)
	}
}

// TestMaybeLogChatCapableNudgeSkip_PerAgentDedup pins the dedup
// helper's behaviour directly: each (repo, agent) pair gets exactly
// one entry no matter how many times it's invoked; distinct pairs
// each get their own entry.
func TestMaybeLogChatCapableNudgeSkip_PerAgentDedup(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	d.maybeLogChatCapableNudgeSkip("repoA", "fireflyA", state.AgentTypeBrowser)
	d.maybeLogChatCapableNudgeSkip("repoA", "fireflyA", state.AgentTypeBrowser) // dedup hit
	d.maybeLogChatCapableNudgeSkip("repoA", "fireflyA", state.AgentTypeBrowser) // dedup hit
	d.maybeLogChatCapableNudgeSkip("repoA", "helperA", state.AgentTypeAssistant)
	d.maybeLogChatCapableNudgeSkip("repoB", "fireflyA", state.AgentTypeBrowser) // different repo

	d.chatCapableNudgeSkipLoggedMu.Lock()
	defer d.chatCapableNudgeSkipLoggedMu.Unlock()
	if got, want := len(d.chatCapableNudgeSkipLogged), 3; got != want {
		t.Errorf("expected %d unique entries (firefly@A, helper@A, firefly@B), got %d", want, got)
	}
	if !d.chatCapableNudgeSkipLogged["repoA/fireflyA"] {
		t.Error("missing repoA/fireflyA")
	}
	if !d.chatCapableNudgeSkipLogged["repoA/helperA"] {
		t.Error("missing repoA/helperA")
	}
	if !d.chatCapableNudgeSkipLogged["repoB/fireflyA"] {
		t.Error("missing repoB/fireflyA (same agent name, different repo, should be separate entry)")
	}
}
