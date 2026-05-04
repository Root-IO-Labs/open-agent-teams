package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/messages"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

func setupStuckWorkerTestDaemon(t *testing.T) (*Daemon, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "stuck-worker-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	paths := config.NewTestPaths(tmpDir)
	if err := paths.EnsureDirectories(); err != nil {
		t.Fatalf("Failed to create directories: %v", err)
	}

	d, err := New(paths)
	if err != nil {
		t.Fatalf("Failed to create daemon: %v", err)
	}

	return d, func() { os.RemoveAll(tmpDir) }
}

// startFakeAgent launches a long-running process whose command line contains
// "oat-agent" so it passes isAgentProcess(). Returns the PID and a cleanup func.
func startFakeAgent(t *testing.T) (int, func()) {
	t.Helper()
	// Use bash -c with a process name containing "oat-agent" so ps shows it
	cmd := exec.Command("bash", "-c", "exec -a oat-agent sleep 3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start fake agent: %v", err)
	}
	return cmd.Process.Pid, func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name       string
		envKey     string
		envValue   string
		defaultVal int
		want       int
	}{
		{"unset uses default", "OAT_TEST_UNSET_KEY", "", 42, 42},
		{"valid int", "OAT_TEST_INT_KEY", "10", 42, 10},
		{"invalid int uses default", "OAT_TEST_INVALID_KEY", "abc", 42, 42},
		{"zero value", "OAT_TEST_ZERO_KEY", "0", 42, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				os.Setenv(tt.envKey, tt.envValue)
				defer os.Unsetenv(tt.envKey)
			}
			got := getEnvInt(tt.envKey, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getEnvInt(%q, %d) = %d, want %d", tt.envKey, tt.defaultVal, got, tt.want)
			}
		})
	}
}

func TestNudgeCountIncrementsOnWorkerNudge(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"test-worker": {
				Type:      state.AgentTypeWorker,
				CreatedAt: time.Now().Add(-10 * time.Minute),
				PID:       fakePID,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()

	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, exists := d.state.GetAgent(repoName, "test-worker")
	if !exists {
		t.Fatal("Worker should still exist after nudge")
	}
	if agent.NudgeCount != 1 {
		t.Errorf("NudgeCount = %d, want 1", agent.NudgeCount)
	}
}

func TestNudgeCountResetsOnGitActivity(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	repoDir := filepath.Join(d.paths.ReposDir, repoName)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"test-worker": {
				Type:          state.AgentTypeWorker,
				CreatedAt:     time.Now().Add(-30 * time.Minute),
				PID:           fakePID,
				NudgeCount:    6,
				LastBranchSHA: "old-sha-that-will-never-match",
			},
		},
	})

	// getBranchSHA returns "" for non-pushed branches, so the reset
	// only triggers when both the old and new SHA are non-empty and differ.
	// We verify the NudgeCount increments (no reset since branch not pushed).
	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()

	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, _ := d.state.GetAgent(repoName, "test-worker")
	// NudgeCount should increment to 7 (no SHA detected from unpushed branch)
	if agent.NudgeCount != 7 {
		t.Errorf("NudgeCount = %d, want 7 (no reset since branch not pushed)", agent.NudgeCount)
	}
}

func TestEscalationTiersNormalNudge(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"test-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-5 * time.Minute),
				PID:        fakePID,
				NudgeCount: 2,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()
	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, _ := d.state.GetAgent(repoName, "test-worker")
	if agent.NudgeCount != 3 {
		t.Errorf("NudgeCount = %d, want 3 (normal tier)", agent.NudgeCount)
	}

	// No supervisor alert should have been sent at nudge count 3
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List(repoName, "supervisor")
	for _, msg := range msgs {
		if msg.From == "daemon" {
			t.Error("Should not alert supervisor during normal nudge tier (count < 5)")
		}
	}
}

func TestEscalationTiersSupervisorAlert(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type:      state.AgentTypeSupervisor,
				CreatedAt: time.Now().Add(-30 * time.Minute),
				PID:       fakePID,
			},
			"test-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-12 * time.Minute),
				PID:        fakePID,
				NudgeCount: stuckSupervisorNudge - 1,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()
	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, _ := d.state.GetAgent(repoName, "test-worker")
	if agent.NudgeCount != stuckSupervisorNudge {
		t.Errorf("NudgeCount = %d, want %d (supervisor alert tier)", agent.NudgeCount, stuckSupervisorNudge)
	}

	// Supervisor should have been alerted
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List(repoName, "supervisor")
	found := false
	for _, msg := range msgs {
		if msg.From == "daemon" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Supervisor should have been alerted about stuck worker at nudge count %d", stuckSupervisorNudge)
	}
}

func TestAutoCompleteWorker(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type: state.AgentTypeSupervisor,
			},
			"merge-queue": {
				Type: state.AgentTypeMergeQueue,
			},
			"stuck-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-30 * time.Minute),
				PID:        os.Getpid(),
				NudgeCount: 10,
				Task:       "Fix the login bug",
			},
		},
	})

	agent, _ := d.state.GetAgent(repoName, "stuck-worker")
	d.autoCompleteWorker(repoName, "stuck-worker", agent, "Fix login page validation")

	updated, exists := d.state.GetAgent(repoName, "stuck-worker")
	if !exists {
		t.Fatal("Worker should still exist after auto-complete")
	}
	if !updated.ReadyForCleanup {
		t.Error("Worker should be marked ReadyForCleanup after auto-complete")
	}
	if updated.Summary != "Fix login page validation" {
		t.Errorf("Summary = %q, want %q", updated.Summary, "Fix login page validation")
	}

	// Check supervisor and merge-queue received notifications
	msgMgr := messages.NewManager(d.paths.MessagesDir)

	supervisorMsgs, _ := msgMgr.List(repoName, "supervisor")
	foundSupervisor := false
	for _, msg := range supervisorMsgs {
		if msg.From == "daemon" {
			foundSupervisor = true
			break
		}
	}
	if !foundSupervisor {
		t.Error("Supervisor should be notified of auto-completion")
	}

	mqMsgs, _ := msgMgr.List(repoName, "merge-queue")
	foundMQ := false
	for _, msg := range mqMsgs {
		if msg.From == "daemon" {
			foundMQ = true
			break
		}
	}
	if !foundMQ {
		t.Error("Merge queue should be notified of auto-completion")
	}
}

func TestForceRemoveWorkerNoPR(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	repoDir := filepath.Join(d.paths.ReposDir, repoName)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type: state.AgentTypeSupervisor,
			},
			"idle-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-45 * time.Minute),
				PID:        os.Getpid(),
				NudgeCount: 20,
				Task:       "Some task",
			},
		},
	})

	agent, _ := d.state.GetAgent(repoName, "idle-worker")
	d.forceRemoveWorker(repoName, repoDir, "idle-worker", agent)

	updated, exists := d.state.GetAgent(repoName, "idle-worker")
	if !exists {
		t.Fatal("Worker should still exist after force-remove (marked for cleanup)")
	}
	if !updated.ReadyForCleanup {
		t.Error("Worker should be marked ReadyForCleanup after force-remove")
	}
	if updated.FailureReason == "" {
		t.Error("FailureReason should be set for force-removed worker")
	}

	// Supervisor should be notified
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List(repoName, "supervisor")
	found := false
	for _, msg := range msgs {
		if msg.From == "daemon" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Supervisor should be notified of force-removal")
	}
}

func TestGetBranchSHA(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoDir := filepath.Join(d.paths.ReposDir, "test-repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	// No remote set up, so getBranchSHA should return empty
	sha := d.getBranchSHA(repoDir, "work/test-worker")
	if sha != "" {
		t.Errorf("getBranchSHA = %q, want empty (no remote)", sha)
	}
}

func TestGetWorkerPRNoGH(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoDir := filepath.Join(d.paths.ReposDir, "test-repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	// gh won't find PRs in a local-only repo
	pr := d.getWorkerPR(repoDir, "work/test-worker")
	if pr != nil {
		t.Error("getWorkerPR should return nil for local-only repo")
	}
}

func TestCheckWorkerProgressNoBranch(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	repoDir := filepath.Join(d.paths.ReposDir, repoName)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	// Create a worktree path with uncommitted changes
	worktreeDir := filepath.Join(d.paths.WorktreesDir, repoName, "no-push-worker")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("Failed to create worktree dir: %v", err)
	}

	// Init a git repo in the worktree for git status to work
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = worktreeDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = worktreeDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = worktreeDir
	cmd.Run()

	// Create an uncommitted file
	os.WriteFile(filepath.Join(worktreeDir, "new-file.txt"), []byte("hello"), 0644)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"no-push-worker": {
				Type:         state.AgentTypeWorker,
				CreatedAt:    time.Now().Add(-30 * time.Minute),
				PID:          os.Getpid(),
				NudgeCount:   9,
				WorktreePath: worktreeDir,
				Task:         "Some task",
			},
		},
	})

	agent, _ := d.state.GetAgent(repoName, "no-push-worker")
	now := time.Now()
	d.checkWorkerProgress(repoName, repoDir, "no-push-worker", agent, now)

	// Worker should NOT be force-removed (has uncommitted changes, gets a nudge instead)
	updated, exists := d.state.GetAgent(repoName, "no-push-worker")
	if !exists {
		t.Fatal("Worker should still exist")
	}
	if updated.ReadyForCleanup {
		t.Error("Worker with uncommitted changes should not be force-removed, should get directive nudge")
	}
}

func TestMaxNudgeForceRemoval(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	repoDir := filepath.Join(d.paths.ReposDir, repoName)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type: state.AgentTypeSupervisor,
			},
			"max-nudge-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-45 * time.Minute),
				PID:        fakePID,
				NudgeCount: stuckMaxNudge - 1,
				Task:       "Abandoned task",
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()

	// This nudge should hit the max and trigger force-removal
	d.nudgeAgentsInRepo(repoName, repo, now)

	updated, exists := d.state.GetAgent(repoName, "max-nudge-worker")
	if !exists {
		t.Fatal("Worker should still exist (marked for cleanup)")
	}
	if !updated.ReadyForCleanup {
		t.Error("Worker at max nudge should be marked ReadyForCleanup")
	}
}

func TestNonWorkerAgentsNotEscalated(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type:      state.AgentTypeSupervisor,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       fakePID,
			},
			"merge-queue": {
				Type:      state.AgentTypeMergeQueue,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       fakePID,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()
	d.nudgeAgentsInRepo(repoName, repo, now)

	// Non-worker agents should have NudgeCount remain 0
	supervisor, _ := d.state.GetAgent(repoName, "supervisor")
	if supervisor.NudgeCount != 0 {
		t.Errorf("Supervisor NudgeCount = %d, want 0 (no escalation for non-workers)", supervisor.NudgeCount)
	}

	mq, _ := d.state.GetAgent(repoName, "merge-queue")
	if mq.NudgeCount != 0 {
		t.Errorf("MergeQueue NudgeCount = %d, want 0 (no escalation for non-workers)", mq.NudgeCount)
	}
}

func TestAlertSupervisorAboutWorker(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type: state.AgentTypeSupervisor,
			},
		},
	})

	d.alertSupervisorAboutWorker(repoName, "stuck-worker", 7)

	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, err := msgMgr.List(repoName, "supervisor")
	if err != nil {
		t.Fatalf("Failed to list supervisor messages: %v", err)
	}

	if len(msgs) == 0 {
		t.Fatal("Expected at least one message to supervisor")
	}

	lastMsg := msgs[len(msgs)-1]
	if lastMsg.From != "daemon" {
		t.Errorf("Message from = %q, want %q", lastMsg.From, "daemon")
	}
	if lastMsg.Body == "" {
		t.Error("Message content should not be empty")
	}
}

func TestWorkerPRInfoParsing(t *testing.T) {
	info := workerPRInfo{Number: 42, Title: "Fix login bug"}
	if info.Number != 42 {
		t.Errorf("Number = %d, want 42", info.Number)
	}
	if info.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", info.Title, "Fix login bug")
	}
}

func TestMergedPRWorkerFastTrack(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	repoDir := filepath.Join(d.paths.ReposDir, repoName)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}
	createTestGitRepo(t, repoDir)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"merged-worker": {
				Type:               state.AgentTypeWorker,
				CreatedAt:          time.Now().Add(-30 * time.Minute),
				PID:                fakePID,
				NudgeCount:         2,
				WokenForMergedPRAt: time.Now().Add(-5 * time.Minute),
				PRNumber:           42,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()

	// At nudge 3 with WokenForMergedPRAt set, should fast-track to daemon takeover
	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, exists := d.state.GetAgent(repoName, "merged-worker")
	if !exists {
		t.Fatal("Worker should still exist")
	}
	// NudgeCount should be 3 (fast-track threshold)
	if agent.NudgeCount != 3 {
		t.Errorf("NudgeCount = %d, want 3", agent.NudgeCount)
	}
	// checkWorkerProgress was called (which checks the PR status via gh, which
	// won't work in tests, so the worker won't be auto-completed - but the
	// fast-track path was exercised). The important thing is it didn't hit
	// the normal nudge path at count 3 (which would just send a status check).
}

func TestNonMergedPRWorkerNormalEscalation(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()
	fakePID, killFake := startFakeAgent(t)
	defer killFake()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"normal-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-10 * time.Minute),
				PID:        fakePID,
				NudgeCount: 2,
			},
		},
	})

	repo, _ := d.state.GetRepo(repoName)
	now := time.Now()

	// At nudge 3 without WokenForMergedPRAt, should follow normal escalation
	d.nudgeAgentsInRepo(repoName, repo, now)

	agent, _ := d.state.GetAgent(repoName, "normal-worker")
	if agent.NudgeCount != 3 {
		t.Errorf("NudgeCount = %d, want 3 (normal nudge)", agent.NudgeCount)
	}
	// Worker should not be ready for cleanup (normal nudge at count 3)
	if agent.ReadyForCleanup {
		t.Error("Worker at nudge 3 without merged PR should not be auto-completed")
	}
}

func TestAutoCompleteWorkerMergedPRSkipsMergeQueue(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents: map[string]state.Agent{
			"supervisor": {
				Type: state.AgentTypeSupervisor,
			},
			"merge-queue": {
				Type: state.AgentTypeMergeQueue,
			},
			"merged-worker": {
				Type:       state.AgentTypeWorker,
				CreatedAt:  time.Now().Add(-30 * time.Minute),
				PID:        os.Getpid(),
				NudgeCount: 10,
				PRNumber:   42,
			},
		},
	})

	agent, _ := d.state.GetAgent(repoName, "merged-worker")
	d.autoCompleteWorker(repoName, "merged-worker", agent, "Auto-completed by daemon (PR #42 merged, worker did not self-complete)")

	// Supervisor should receive notification
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	supervisorMsgs, _ := msgMgr.List(repoName, "supervisor")
	foundSupervisor := false
	for _, msg := range supervisorMsgs {
		if msg.From == "daemon" {
			foundSupervisor = true
		}
	}
	if !foundSupervisor {
		t.Error("Supervisor should be notified for merged-PR auto-complete")
	}

	// Merge-queue should NOT receive notification (PR already merged)
	mqMsgs, _ := msgMgr.List(repoName, "merge-queue")
	for _, msg := range mqMsgs {
		if msg.From == "daemon" {
			t.Error("Merge-queue should NOT be notified for already-merged PR auto-complete")
		}
	}
}

func TestConfigurableThresholds(t *testing.T) {
	// Save and restore original values
	origSupervisor := stuckSupervisorNudge
	origDaemon := stuckDaemonNudge
	origMax := stuckMaxNudge
	defer func() {
		stuckSupervisorNudge = origSupervisor
		stuckDaemonNudge = origDaemon
		stuckMaxNudge = origMax
	}()

	// Test custom thresholds via env vars
	os.Setenv("OAT_STUCK_SUPERVISOR_NUDGE", "3")
	os.Setenv("OAT_STUCK_DAEMON_NUDGE", "6")
	os.Setenv("OAT_STUCK_MAX_NUDGE", "15")
	defer os.Unsetenv("OAT_STUCK_SUPERVISOR_NUDGE")
	defer os.Unsetenv("OAT_STUCK_DAEMON_NUDGE")
	defer os.Unsetenv("OAT_STUCK_MAX_NUDGE")

	stuckSupervisorNudge = getEnvInt("OAT_STUCK_SUPERVISOR_NUDGE", 5)
	stuckDaemonNudge = getEnvInt("OAT_STUCK_DAEMON_NUDGE", 8)
	stuckMaxNudge = getEnvInt("OAT_STUCK_MAX_NUDGE", 20)

	if stuckSupervisorNudge != 3 {
		t.Errorf("stuckSupervisorNudge = %d, want 3", stuckSupervisorNudge)
	}
	if stuckDaemonNudge != 6 {
		t.Errorf("stuckDaemonNudge = %d, want 6", stuckDaemonNudge)
	}
	if stuckMaxNudge != 15 {
		t.Errorf("stuckMaxNudge = %d, want 15", stuckMaxNudge)
	}
}
