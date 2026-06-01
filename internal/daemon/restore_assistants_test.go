// Part 9 R2 sanity test (2026-05-29 smoke-test follow-up).
//
// Pins the existing behavior that a removed assistant stays
// removed across a daemon-state reload. The 2026-05-29 smoke
// report initially suggested deleted assistants were
// resurrecting on daemon restart; the follow-up investigation
// confirmed they were NOT — what made it LOOK that way was
// stopped (not removed) assistants lingering in STOPPED state.
// The fix in 9.0 wired up generic `oat agent remove` + the
// extension Delete button so the cards clear correctly.
//
// This test exists to make a future regression here loud:
// suppose someone adds a "rehydrate-known-assistants" startup
// loop that consults a registry / config file / etc. and
// re-creates state.Repository + state.Agent records for known
// assistant names. That would silently undo `oat assistant
// remove`. The pin: after a remove and a fresh state Load,
// neither the virtual repo nor the agent record reappears.
//
// Test surface deliberately stays at the state layer (rather
// than spinning up a full daemon) — the property under test
// is "removed things stay removed across persistence," and
// that's where the bug would land. A full e2e (oat CLI ->
// daemon socket -> CLI) is the manual smoke-test step 9 of
// the Part 9.5 acceptance gate.

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestAssistantRemoveStaysRemovedAcrossReload_R2(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "oat-r2-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	statePath := filepath.Join(tmpDir, "state.json")

	const (
		assistantName = "smoke-test-victim"
		virtualRepo   = "_assistant-smoke-test-victim"
	)

	// Phase 1: build the world a successful `oat assistant
	// start` would have left behind — a virtual repo + an
	// AgentTypeAssistant record under it.
	s := state.New(statePath)
	repo := &state.Repository{
		SessionName: virtualRepo,
		IsVirtual:   true,
		Agents:      map[string]state.Agent{},
	}
	if err := s.AddRepo(virtualRepo, repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := s.AddAgent(virtualRepo, assistantName, state.Agent{
		Type:       state.AgentTypeAssistant,
		WindowName: assistantName,
	}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Sanity: the assistant exists pre-remove.
	if r, ok := s.GetRepo(virtualRepo); !ok {
		t.Fatalf("virtual repo missing before remove")
	} else if _, exists := r.Agents[assistantName]; !exists {
		t.Fatalf("agent missing before remove")
	}

	// Phase 2: simulate `oat assistant remove smoke-test-victim`.
	// The CLI tears down BOTH the agent record AND the virtual
	// repo; both removals must happen for the pin to be
	// meaningful — leaving the virtual repo behind would let a
	// future "rehydrate registered assistants" loop bring back
	// the agent.
	if err := s.RemoveAgent(virtualRepo, assistantName); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if err := s.RemoveRepo(virtualRepo); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	// Save is implicit (RemoveAgent/RemoveRepo persist), but
	// be explicit so a future change to those methods doesn't
	// silently break the pin.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Phase 3: simulate `oat daemon restart` by Load()ing a
	// fresh State from the same path. The assistant must NOT
	// reappear.
	reloaded, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, ok := reloaded.GetRepo(virtualRepo); ok {
		t.Fatalf("virtual repo %q reappeared after reload: %+v", virtualRepo, r)
	}
	for repoName, r := range reloaded.GetAllRepos() {
		if _, exists := r.Agents[assistantName]; exists {
			t.Fatalf("agent %q reappeared in repo %q after reload", assistantName, repoName)
		}
	}
}
