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

// TestRestoreVirtualRepoSurvivesDaemonRestart_2026_06_02 pins the
// inverse property of TestAssistantRemoveStaysRemovedAcrossReload_R2:
// an ACTIVE virtual repo + assistant agent record must survive a
// daemon restart (i.e., a restoreRepoAgents call against a freshly-
// loaded state).
//
// Pre-fix bug (reported 2026-06-02: "when I reinstall and reload the
// extension and restart the daemon, test1 disappears for some reason"):
// the standard restoreRepoAgents path assumed every repo had a git
// directory on disk + a supervisor + worktrees. A virtual repo
// (state.Repository.IsVirtual=true, from `oat assistant start`) has
// none of those. The os.Stat(repoPath) check returned ENOENT, the
// function returned an error, the health-check loop retried until
// fetchFailureThreshold, then it called RemoveAgent on every agent in
// the repo. The user's assistant was silently deleted on every
// daemon restart with no visible failure narrative beyond the
// daemon log's "failed to restore" warnings.
//
// Post-fix: restoreRepoAgents branches on repo.IsVirtual and calls
// restoreVirtualRepoAgents instead. That path only creates the
// backend session and re-spawns assistant agents (skipping user-
// stopped ones); it does NOT touch git, supervisor, or worktrees,
// and it does NOT remove "stale" agents.
func TestRestoreVirtualRepoSurvivesDaemonRestart_2026_06_02(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Force OAT_TEST_MODE so startRegisteredAgent's spawn branch
	// is skipped (no real oat-agent binary in the test sandbox).
	t.Setenv("OAT_TEST_MODE", "1")

	const (
		assistantName = "post-restart-survivor"
		virtualRepo   = "_assistant-post-restart-survivor"
	)

	// Build the world an in-progress assistant would have:
	// virtual repo + AgentTypeAssistant record with no live PID
	// (simulating the post-daemon-restart state where the in-
	// process backend has lost its sessions).
	repo := &state.Repository{
		SessionName: "oat-" + virtualRepo,
		IsVirtual:   true,
		Agents:      map[string]state.Agent{},
	}
	if err := d.state.AddRepo(virtualRepo, repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	// Worktree path must exist for startRegisteredAgent's
	// downstream prompt-file write etc. The virtual-repo create
	// path in CLI assistantStart mkdirs this; mirror that here.
	wtPath := d.paths.AgentWorktree(virtualRepo, assistantName)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := d.state.AddAgent(virtualRepo, assistantName, state.Agent{
		Type:         state.AgentTypeAssistant,
		WorktreePath: wtPath,
		WindowName:   assistantName,
		// PID=0 (no live process) — matches the post-restart
		// reality of the in-process DirectBackend.
	}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Re-read repo from state with the agent we just added.
	gotRepo, ok := d.state.GetRepo(virtualRepo)
	if !ok {
		t.Fatalf("repo %q missing from state immediately after AddRepo+AddAgent", virtualRepo)
	}

	// THE PIN: restoreRepoAgents on a virtual repo must NOT return
	// an error and must NOT remove the agent. (Pre-fix this
	// returned "repository path does not exist" → after enough
	// retries the agent was deleted via the appendToSliceMap →
	// deadAgents → RemoveAgent path in checkAgentHealth.)
	if err := d.restoreRepoAgents(virtualRepo, gotRepo); err != nil {
		t.Fatalf("restoreRepoAgents on virtual repo returned error: %v", err)
	}

	// Repo + agent must still exist after restore.
	postRepo, ok := d.state.GetRepo(virtualRepo)
	if !ok {
		t.Fatalf("virtual repo %q was removed during restoreRepoAgents", virtualRepo)
	}
	if _, exists := postRepo.Agents[assistantName]; !exists {
		t.Fatalf("agent %q was removed during restoreRepoAgents (this is the 2026-06-02 disappearing-assistant bug)", assistantName)
	}

	// User-stopped agents must NOT be respawned. Mirror the
	// stop_agent verb's LastError marker; restoreVirtualRepoAgents
	// should leave the PID at 0.
	stoppedAgent := postRepo.Agents[assistantName]
	stoppedAgent.LastError = "stopped by user"
	stoppedAgent.PID = 0
	if err := d.state.UpdateAgent(virtualRepo, assistantName, stoppedAgent); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	stoppedRepo, _ := d.state.GetRepo(virtualRepo)
	if err := d.restoreRepoAgents(virtualRepo, stoppedRepo); err != nil {
		t.Fatalf("restoreRepoAgents on virtual repo (user-stopped agent) returned error: %v", err)
	}
	finalRepo, _ := d.state.GetRepo(virtualRepo)
	finalAgent, exists := finalRepo.Agents[assistantName]
	if !exists {
		t.Fatalf("user-stopped agent %q was removed during restoreRepoAgents", assistantName)
	}
	if finalAgent.PID != 0 {
		t.Errorf("user-stopped agent was respawned by restoreVirtualRepoAgents (PID=%d); should have been left STOPPED", finalAgent.PID)
	}
}

// TestStartRegisteredAgentPreservesExistingSessionID_2026_06_02 pins
// the SessionID-stability contract that startRegisteredAgent must NOT
// generate a fresh session ID when the agent record already carries
// one. SessionID stability is the foundation of the assistant memory-
// continuity fix shipped 2026-06-03: the daemon passes
// `--thread-id <agent.SessionID>` on every assistant spawn, and
// langgraph (oat-cli's checkpointer) keys conversation history by
// thread_id. Regenerate the SessionID across restarts and you orphan
// the prior langgraph thread → assistant has no memory of prior
// turns. This test guards only the state half of that contract; the
// arg-construction half (—> --thread-id ends up in the spawn args)
// is gated behind OAT_TEST_MODE=1's spawn-skip, so it lives in the
// manual smoke test step.
//
// Background (2026-06-02 follow-up): the first attempt at memory
// continuity stat'd `~/.claude/projects/.../<SessionID>.jsonl` to
// decide whether to pass `--resume`. That path is Claude CLI's
// convention; oat-cli stores threads in `~/.oat/sessions.db` via
// langgraph checkpoints. The stat always failed, --resume was
// never passed, and the user-visible behavior remained "fresh
// spawn re-injects the system prompt on every restart." The
// 06-03 rework switched to --thread-id (which langgraph treats
// idempotently — create on first call, resume on subsequent ones)
// and suppressed the -m prompt re-injection when SessionID was
// already populated.
func TestStartRegisteredAgentPreservesExistingSessionID_2026_06_02(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	t.Setenv("OAT_TEST_MODE", "1")

	const (
		assistantName = "memory-keeper"
		virtualRepo   = "_assistant-memory-keeper"
		priorSession  = "00000000-0000-0000-0000-aaaaaaaaaaaa"
	)

	repo := &state.Repository{
		SessionName: "oat-" + virtualRepo,
		IsVirtual:   true,
		Agents:      map[string]state.Agent{},
	}
	if err := d.state.AddRepo(virtualRepo, repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	wtPath := d.paths.AgentWorktree(virtualRepo, assistantName)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	// Pre-existing SessionID — the post-daemon-restart shape.
	if err := d.state.AddAgent(virtualRepo, assistantName, state.Agent{
		Type:         state.AgentTypeAssistant,
		WorktreePath: wtPath,
		WindowName:   assistantName,
		SessionID:    priorSession,
	}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	gotRepo, _ := d.state.GetRepo(virtualRepo)
	gotAgent := gotRepo.Agents[assistantName]
	if _, err := d.startRegisteredAgent(virtualRepo, gotRepo, assistantName, gotAgent, nil); err != nil {
		t.Fatalf("startRegisteredAgent: %v", err)
	}

	postRepo, _ := d.state.GetRepo(virtualRepo)
	postAgent := postRepo.Agents[assistantName]
	if postAgent.SessionID != priorSession {
		t.Fatalf("SessionID was regenerated: got %q want %q (memory continuity broken)", postAgent.SessionID, priorSession)
	}

	// Companion case: empty SessionID → new one assigned (fresh-
	// create path). Pin so a future "preserve unconditionally"
	// refactor doesn't leave fresh-create agents with no
	// SessionID at all (which breaks --thread-id on the very
	// first spawn — langgraph wouldn't have anything to key on).
	const freshName = "fresh-create"
	if err := d.state.AddAgent(virtualRepo, freshName, state.Agent{
		Type:         state.AgentTypeAssistant,
		WorktreePath: wtPath,
		WindowName:   freshName,
	}); err != nil {
		t.Fatalf("AddAgent fresh: %v", err)
	}
	freshRepo, _ := d.state.GetRepo(virtualRepo)
	freshAgent := freshRepo.Agents[freshName]
	if _, err := d.startRegisteredAgent(virtualRepo, freshRepo, freshName, freshAgent, nil); err != nil {
		t.Fatalf("startRegisteredAgent fresh: %v", err)
	}
	postFreshRepo, _ := d.state.GetRepo(virtualRepo)
	postFreshAgent := postFreshRepo.Agents[freshName]
	if postFreshAgent.SessionID == "" {
		t.Fatalf("fresh-create agent has empty SessionID after spawn — should have been generated")
	}
	if postFreshAgent.SessionID == priorSession {
		t.Fatalf("fresh-create agent collided with prior assistant's SessionID (%q)", priorSession)
	}
}

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
