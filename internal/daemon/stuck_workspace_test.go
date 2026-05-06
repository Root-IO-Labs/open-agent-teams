package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestCheckWorkspaceHealthSkipsDisabledRepos(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "disabled-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: false,
		Agents: map[string]state.Agent{
			"workspace": {
				Type:      state.AgentTypeWorkspace,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       1,
			},
		},
	})

	d.checkWorkspaceHealth()

	if _, exists := d.workspaceActivity[repoName]; exists {
		t.Error("Should not track activity for repos with workspace stuck detection disabled")
	}
}

func TestCheckWorkspaceHealthSkipsNonWorkspace(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: true,
		Agents: map[string]state.Agent{
			"supervisor": {
				Type:      state.AgentTypeSupervisor,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       1,
			},
		},
	})

	d.checkWorkspaceHealth()

	if _, exists := d.workspaceActivity[repoName]; exists {
		t.Error("Should not track activity for non-workspace agents")
	}
}

func TestCheckWorkspaceHealthTracksRecentActivity(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"

	logFile := d.paths.AgentLogFile(repoName, "workspace", false)
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("Failed to create output log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("ASSISTANT: test output\n"), 0644); err != nil {
		t.Fatalf("Failed to create output log file: %v", err)
	}

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: true,
		Agents: map[string]state.Agent{
			"workspace": {
				Type:      state.AgentTypeWorkspace,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       os.Getpid(),
			},
		},
	})

	d.checkWorkspaceHealth()

	activity, exists := d.workspaceActivity[repoName]
	if !exists {
		t.Fatal("Should track activity for active workspace")
	}
	if !activity.nudgedAt.IsZero() {
		t.Error("Should not be nudged on first check with recent file")
	}
}

func TestCheckWorkspaceHealthSoftTimeout(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	origSoft := workspaceSoftTimeout
	origHard := workspaceHardTimeout
	defer func() {
		workspaceSoftTimeout = origSoft
		workspaceHardTimeout = origHard
	}()
	workspaceSoftTimeout = 1 * time.Millisecond
	workspaceHardTimeout = 1 * time.Hour

	repoName := "test-repo"

	logFile := d.paths.AgentLogFile(repoName, "workspace", false)
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("Failed to create output log dir: %v", err)
	}
	staleTime := time.Now().Add(-20 * time.Minute)
	if err := os.WriteFile(logFile, []byte("ASSISTANT: test output\n"), 0644); err != nil {
		t.Fatalf("Failed to create output log file: %v", err)
	}
	os.Chtimes(logFile, staleTime, staleTime)

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: true,
		Agents: map[string]state.Agent{
			"workspace": {
				Type:      state.AgentTypeWorkspace,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       os.Getpid(),
			},
		},
	})

	d.checkWorkspaceHealth()

	activity := d.workspaceActivity[repoName]
	if activity == nil {
		t.Fatal("Should have activity tracking after first check")
	}

	// Second check: should detect stale file and attempt nudge
	d.checkWorkspaceHealth()

	if activity.lastModTime.IsZero() {
		t.Error("lastModTime should be set")
	}
}

func TestCheckWorkspaceHealthSkipsDeadProcess(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: true,
		Agents: map[string]state.Agent{
			"workspace": {
				Type:      state.AgentTypeWorkspace,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       999999,
			},
		},
	})

	d.checkWorkspaceHealth()

	if _, exists := d.workspaceActivity[repoName]; exists {
		t.Error("Should not track activity for workspace with dead process")
	}
}

func TestWorkspaceActivityResetOnNewOutput(t *testing.T) {
	d, cleanup := setupStuckWorkerTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"

	logFile := d.paths.AgentLogFile(repoName, "workspace", false)
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("Failed to create output log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("ASSISTANT: test output\n"), 0644); err != nil {
		t.Fatalf("Failed to create output log file: %v", err)
	}

	d.state.AddRepo(repoName, &state.Repository{
		GithubURL:               "https://github.com/test/repo",
		SessionName:             "test-session",
		WorkspaceStuckDetection: true,
		Agents: map[string]state.Agent{
			"workspace": {
				Type:      state.AgentTypeWorkspace,
				CreatedAt: time.Now().Add(-60 * time.Minute),
				PID:       os.Getpid(),
			},
		},
	})

	d.checkWorkspaceHealth()

	// Simulate that a nudge was sent (well outside the grace period)
	d.workspaceActivity[repoName].nudgedAt = time.Now().Add(-1 * time.Minute)

	time.Sleep(10 * time.Millisecond)
	os.WriteFile(logFile, []byte("ASSISTANT: test output\nASSISTANT: new output\n"), 0644)

	d.checkWorkspaceHealth()

	activity := d.workspaceActivity[repoName]
	if !activity.nudgedAt.IsZero() {
		t.Error("nudgedAt should be reset after new output detected outside grace period")
	}
}
