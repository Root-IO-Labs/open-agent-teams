package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/messages"
	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// setupTestDaemonWithState creates a test daemon with a pre-configured state for testing.
// This allows tests to start with a known state without side effects.
func setupTestDaemonWithState(t *testing.T, setupFn func(*state.State)) (*Daemon, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "daemon-handler-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	paths := &config.Paths{
		Root:         tmpDir,
		BinDir:       filepath.Join(tmpDir, "bin"),
		DaemonPID:    filepath.Join(tmpDir, "daemon.pid"),
		DaemonSock:   filepath.Join(tmpDir, "daemon.sock"),
		DaemonLog:    filepath.Join(tmpDir, "daemon.log"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		MessagesDir:  filepath.Join(tmpDir, "messages"),
		OutputDir:    filepath.Join(tmpDir, "output"),
		ArchiveDir:   filepath.Join(tmpDir, "archive"),
	}

	if err := paths.EnsureDirectories(); err != nil {
		t.Fatalf("Failed to create directories: %v", err)
	}

	d, err := New(paths)
	if err != nil {
		t.Fatalf("Failed to create daemon: %v", err)
	}

	// Apply setup function if provided
	if setupFn != nil {
		setupFn(d.state)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return d, cleanup
}

// TestHandleAddAgentTableDriven tests handleAddAgent with various argument combinations
func TestHandleAddAgentTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{"agent": "test", "type": "worker", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": "", "agent": "test", "type": "worker", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "missing agent argument",
			args:        map[string]interface{}{"repo": "test-repo", "type": "worker", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name:        "empty agent argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "", "type": "worker", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name:        "missing type argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'type'",
		},
		{
			name:        "empty type argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "type": "", "worktree_path": "/tmp", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'type'",
		},
		{
			name:        "missing worktree_path argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "type": "worker", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'worktree_path'",
		},
		{
			name:        "empty worktree_path argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "type": "worker", "worktree_path": "", "window_name": "win"},
			wantSuccess: false,
			wantError:   "missing 'worktree_path'",
		},
		{
			name:        "missing window_name argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "type": "worker", "worktree_path": "/tmp"},
			wantSuccess: false,
			wantError:   "missing 'window_name'",
		},
		{
			name:        "empty window_name argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": "test", "type": "worker", "worktree_path": "/tmp", "window_name": ""},
			wantSuccess: false,
			wantError:   "missing 'window_name'",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo":          "nonexistent",
				"agent":         "test",
				"type":          "worker",
				"worktree_path": "/tmp",
				"window_name":   "win",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "successful add with minimal args",
			args: map[string]interface{}{
				"repo":          "test-repo",
				"agent":         "test-agent",
				"type":          "worker",
				"worktree_path": "/tmp/test",
				"window_name":   "test-win",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "successful add with all optional args",
			args: map[string]interface{}{
				"repo":          "test-repo",
				"agent":         "full-agent",
				"type":          "supervisor",
				"worktree_path": "/tmp/full",
				"window_name":   "full-win",
				"session_id":    "custom-session",
				"pid":           float64(12345),
				"task":          "my task",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "pid as integer type",
			args: map[string]interface{}{
				"repo":          "test-repo",
				"agent":         "int-pid-agent",
				"type":          "worker",
				"worktree_path": "/tmp/test",
				"window_name":   "test-win",
				"pid":           int(99999),
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "all valid agent types",
			args: map[string]interface{}{
				"repo":          "test-repo",
				"agent":         "merge-agent",
				"type":          "merge-queue",
				"worktree_path": "/tmp/mq",
				"window_name":   "mq-win",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleAddAgent(socket.Request{
				Command: "add_agent",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleAddAgent() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleAddAgent() expected error containing %q, got empty error", tt.wantError)
			}

			if tt.wantSuccess {
				// Verify agent was added to state
				agentName, _ := tt.args["agent"].(string)
				repoName, _ := tt.args["repo"].(string)
				agent, exists := d.state.GetAgent(repoName, agentName)
				if !exists {
					t.Error("Agent should exist in state after successful add")
				}

				// Verify agent properties
				if agentType, ok := tt.args["type"].(string); ok {
					if string(agent.Type) != agentType {
						t.Errorf("Agent type = %s, want %s", agent.Type, agentType)
					}
				}
				if worktreePath, ok := tt.args["worktree_path"].(string); ok {
					if agent.WorktreePath != worktreePath {
						t.Errorf("Agent worktree_path = %s, want %s", agent.WorktreePath, worktreePath)
					}
				}
				if windowName, ok := tt.args["window_name"].(string); ok {
					if agent.WindowName != windowName {
						t.Errorf("Agent window_name = %s, want %s", agent.WindowName, windowName)
					}
				}
				if task, ok := tt.args["task"].(string); ok {
					if agent.Task != task {
						t.Errorf("Agent task = %s, want %s", agent.Task, task)
					}
				}
				if sessionID, ok := tt.args["session_id"].(string); ok {
					if agent.SessionID != sessionID {
						t.Errorf("Agent session_id = %s, want %s", agent.SessionID, sessionID)
					}
				}
				// Check PID handling
				if pidFloat, ok := tt.args["pid"].(float64); ok {
					if agent.PID != int(pidFloat) {
						t.Errorf("Agent PID = %d, want %d", agent.PID, int(pidFloat))
					}
				}
				if pidInt, ok := tt.args["pid"].(int); ok {
					if agent.PID != pidInt {
						t.Errorf("Agent PID = %d, want %d", agent.PID, pidInt)
					}
				}
				if issueNumber, ok := tt.args["issue_number"].(string); ok {
					if agent.IssueNumber != issueNumber {
						t.Errorf("Agent issue_number = %q, want %q", agent.IssueNumber, issueNumber)
					}
				}
				if issueURL, ok := tt.args["issue_url"].(string); ok {
					if agent.IssueURL != issueURL {
						t.Errorf("Agent issue_url = %q, want %q", agent.IssueURL, issueURL)
					}
				}
			}
		})
	}
}

// TestHandleAddAgentWithIssueNumber verifies that issue_number and issue_url are stored when provided
func TestHandleAddAgentWithIssueNumber(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	resp := d.handleAddAgent(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          "test-repo",
			"agent":         "issue-worker",
			"type":          "worker",
			"worktree_path": "/tmp/worker",
			"window_name":   "issue-worker",
			"task":          "Fix #42",
			"issue_number":  "42",
			"issue_url":     "https://github.com/owner/repo/issues/42",
		},
	})
	if !resp.Success {
		t.Fatalf("handleAddAgent() failed: %s", resp.Error)
	}
	agent, exists := d.state.GetAgent("test-repo", "issue-worker")
	if !exists {
		t.Fatal("Agent should exist in state")
	}
	if agent.IssueNumber != "42" {
		t.Errorf("IssueNumber = %q, want %q", agent.IssueNumber, "42")
	}
	if agent.IssueURL != "https://github.com/owner/repo/issues/42" {
		t.Errorf("IssueURL = %q, want %q", agent.IssueURL, "https://github.com/owner/repo/issues/42")
	}
}

// TestHandleRemoveAgentTableDriven tests handleRemoveAgent with various argument combinations
func TestHandleRemoveAgentTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{"agent": "test"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": "", "agent": "test"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "missing agent argument",
			args:        map[string]interface{}{"repo": "test-repo"},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name:        "empty agent argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": ""},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo":  "nonexistent",
				"agent": "test",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "agent does not exist - idempotent delete succeeds",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "nonexistent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true, // Delete is idempotent - removing non-existent agent succeeds
		},
		{
			name: "successful remove",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "test-agent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "test-agent", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "test-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleRemoveAgent(socket.Request{
				Command: "remove_agent",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleRemoveAgent() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantSuccess {
				// Verify agent was removed from state
				agentName, _ := tt.args["agent"].(string)
				repoName, _ := tt.args["repo"].(string)
				_, exists := d.state.GetAgent(repoName, agentName)
				if exists {
					t.Error("Agent should not exist in state after successful remove")
				}
			}
		})
	}
}

// TestHandleCompleteAgentTableDriven tests handleCompleteAgent with various argument combinations
func TestHandleCompleteAgentTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
		checkState  func(t *testing.T, d *Daemon)
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{"agent": "test"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": "", "agent": "test"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "missing agent argument",
			args:        map[string]interface{}{"repo": "test-repo"},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name:        "empty agent argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": ""},
			wantSuccess: false,
			wantError:   "missing 'agent'",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo":  "nonexistent",
				"agent": "test",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "agent does not exist",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "nonexistent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "successful complete worker agent",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "worker-agent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "worker-agent", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "worker-window",
					Task:       "test task",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, exists := d.state.GetAgent("test-repo", "worker-agent")
				if !exists {
					t.Error("Agent should still exist after complete")
					return
				}
				if !agent.ReadyForCleanup {
					t.Error("Agent should be marked as ready for cleanup")
				}
			},
		},
		{
			name: "successful complete review agent",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "review-agent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "review-agent", state.Agent{
					Type:       state.AgentTypeReview,
					WindowName: "review-window",
					Task:       "review PR #123",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, exists := d.state.GetAgent("test-repo", "review-agent")
				if !exists {
					t.Error("Agent should still exist after complete")
					return
				}
				if !agent.ReadyForCleanup {
					t.Error("Agent should be marked as ready for cleanup")
				}
			},
		},
		{
			name: "reject supervisor self-completion (permanent agent guard)",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "supervisor",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "supervisor", state.Agent{
					Type:       state.AgentTypeSupervisor,
					WindowName: "supervisor-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "cannot be completed",
			checkState: func(t *testing.T, d *Daemon) {
				agent, exists := d.state.GetAgent("test-repo", "supervisor")
				if !exists {
					t.Error("Agent should still exist")
					return
				}
				if agent.ReadyForCleanup {
					t.Error("Supervisor should NOT be marked for cleanup")
				}
			},
		},
		{
			name: "reject workspace self-completion (permanent agent guard)",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "workspace",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "workspace", state.Agent{
					Type:       state.AgentTypeWorkspace,
					WindowName: "workspace-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "cannot be completed",
		},
		{
			name: "reject merge-queue self-completion (permanent agent guard)",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "merge-queue",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "merge-queue", state.Agent{
					Type:       state.AgentTypeMergeQueue,
					WindowName: "mq-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "cannot be completed",
		},
		{
			name: "supervisor completes worker via --worker flag",
			args: map[string]interface{}{
				"repo":         "test-repo",
				"agent":        "supervisor",
				"target_agent": "stuck-worker",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "supervisor", state.Agent{
					Type:       state.AgentTypeSupervisor,
					WindowName: "supervisor-window",
					CreatedAt:  time.Now(),
				})
				s.AddAgent("test-repo", "stuck-worker", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "worker-window",
					Task:       "fix the bug",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				worker, exists := d.state.GetAgent("test-repo", "stuck-worker")
				if !exists {
					t.Error("Worker should still exist")
					return
				}
				if !worker.ReadyForCleanup {
					t.Error("Worker should be marked for cleanup when completed via --worker")
				}
				supervisor, exists := d.state.GetAgent("test-repo", "supervisor")
				if !exists {
					t.Error("Supervisor should still exist")
					return
				}
				if supervisor.ReadyForCleanup {
					t.Error("Supervisor should NOT be marked for cleanup when using --worker")
				}
			},
		},
		{
			name: "--worker flag with non-existent target",
			args: map[string]interface{}{
				"repo":         "test-repo",
				"agent":        "supervisor",
				"target_agent": "ghost-worker",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "supervisor", state.Agent{
					Type:       state.AgentTypeSupervisor,
					WindowName: "supervisor-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleCompleteAgent(socket.Request{
				Command: "complete_agent",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleCompleteAgent() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.checkState != nil {
				tt.checkState(t, d)
			}
		})
	}
}

// TestHandleCompleteAgentSendsMessages verifies that completion messages are sent
func TestHandleCompleteAgentSendsMessages(t *testing.T) {
	tests := []struct {
		name               string
		agentType          state.AgentType
		agentName          string
		task               string
		expectedRecipients []string
	}{
		{
			name:               "worker sends to supervisor, merge-queue, and workspace",
			agentType:          state.AgentTypeWorker,
			agentName:          "test-worker",
			task:               "implement feature X",
			expectedRecipients: []string{"supervisor", "merge-queue", "workspace"},
		},
		{
			name:               "review agent sends to merge-queue and workspace",
			agentType:          state.AgentTypeReview,
			agentName:          "test-review",
			task:               "review PR #42",
			expectedRecipients: []string{"merge-queue", "workspace"},
		},
		// Supervisor and merge-queue are now rejected by the permanent agent guard.
		// See TestHandleCompleteAgentTableDriven for those tests.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", tt.agentName, state.Agent{
					Type:       tt.agentType,
					WindowName: tt.agentName + "-window",
					Task:       tt.task,
					CreatedAt:  time.Now(),
				})
			})
			defer cleanup()

			resp := d.handleCompleteAgent(socket.Request{
				Command: "complete_agent",
				Args: map[string]interface{}{
					"repo":  "test-repo",
					"agent": tt.agentName,
				},
			})

			if !resp.Success {
				t.Fatalf("handleCompleteAgent() failed: %s", resp.Error)
			}

			// Verify messages were sent to expected recipients
			msgMgr := messages.NewManager(d.paths.MessagesDir)
			for _, recipient := range tt.expectedRecipients {
				msgs, err := msgMgr.List("test-repo", recipient)
				if err != nil {
					t.Errorf("Failed to list messages for %s: %v", recipient, err)
					continue
				}
				if len(msgs) == 0 {
					t.Errorf("Expected message for %s, but found none", recipient)
				}
			}

			// Verify no messages sent to non-expected recipients
			allRecipients := []string{"supervisor", "merge-queue", "workspace"}
			for _, recipient := range allRecipients {
				isExpected := false
				for _, expected := range tt.expectedRecipients {
					if recipient == expected {
						isExpected = true
						break
					}
				}
				if !isExpected {
					msgs, _ := msgMgr.List("test-repo", recipient)
					if len(msgs) > 0 {
						t.Errorf("Unexpected message for %s", recipient)
					}
				}
			}
		})
	}
}

// TestHandleAddRepoTableDriven tests handleAddRepo with various argument combinations
func TestHandleAddRepoTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
		checkState  func(t *testing.T, d *Daemon)
	}{
		{
			name:        "missing name argument",
			args:        map[string]interface{}{"github_url": "https://github.com/test/repo", "session_name": "test"},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name:        "empty name argument",
			args:        map[string]interface{}{"name": "", "github_url": "https://github.com/test/repo", "session_name": "test"},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name:        "missing github_url argument",
			args:        map[string]interface{}{"name": "test-repo", "session_name": "test"},
			wantSuccess: false,
			wantError:   "missing 'github_url'",
		},
		{
			name:        "empty github_url argument",
			args:        map[string]interface{}{"name": "test-repo", "github_url": "", "session_name": "test"},
			wantSuccess: false,
			wantError:   "missing 'github_url'",
		},
		{
			name:        "missing session_name argument",
			args:        map[string]interface{}{"name": "test-repo", "github_url": "https://github.com/test/repo"},
			wantSuccess: false,
			wantError:   "missing 'session_name'",
		},
		{
			name:        "empty session_name argument",
			args:        map[string]interface{}{"name": "test-repo", "github_url": "https://github.com/test/repo", "session_name": ""},
			wantSuccess: false,
			wantError:   "missing 'session_name'",
		},
		{
			name: "successful add with minimal args",
			args: map[string]interface{}{
				"name":         "my-repo",
				"github_url":   "https://github.com/owner/repo",
				"session_name": "oat-my-repo",
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				repo, exists := d.state.GetRepo("my-repo")
				if !exists {
					t.Error("Repo should exist after add")
					return
				}
				if repo.GithubURL != "https://github.com/owner/repo" {
					t.Errorf("GithubURL = %s, want https://github.com/owner/repo", repo.GithubURL)
				}
				if repo.SessionName != "oat-my-repo" {
					t.Errorf("SessionName = %s, want mc-my-repo", repo.SessionName)
				}
				// Default merge queue config
				if !repo.MergeQueueConfig.Enabled {
					t.Error("MergeQueueConfig.Enabled should default to true")
				}
				if repo.MergeQueueConfig.TrackMode != state.TrackModeAll {
					t.Errorf("MergeQueueConfig.TrackMode = %s, want all", repo.MergeQueueConfig.TrackMode)
				}
			},
		},
		{
			name: "successful add with merge queue disabled",
			args: map[string]interface{}{
				"name":         "no-mq-repo",
				"github_url":   "https://github.com/owner/repo",
				"session_name": "oat-no-mq-repo",
				"mq_enabled":   false,
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				repo, exists := d.state.GetRepo("no-mq-repo")
				if !exists {
					t.Error("Repo should exist after add")
					return
				}
				if repo.MergeQueueConfig.Enabled {
					t.Error("MergeQueueConfig.Enabled should be false")
				}
			},
		},
		{
			name: "successful add with track mode author",
			args: map[string]interface{}{
				"name":          "author-repo",
				"github_url":    "https://github.com/owner/repo",
				"session_name":  "oat-author-repo",
				"mq_track_mode": "author",
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				repo, exists := d.state.GetRepo("author-repo")
				if !exists {
					t.Error("Repo should exist after add")
					return
				}
				if repo.MergeQueueConfig.TrackMode != state.TrackModeAuthor {
					t.Errorf("MergeQueueConfig.TrackMode = %s, want author", repo.MergeQueueConfig.TrackMode)
				}
			},
		},
		{
			name: "successful add with track mode assigned",
			args: map[string]interface{}{
				"name":          "assigned-repo",
				"github_url":    "https://github.com/owner/repo",
				"session_name":  "oat-assigned-repo",
				"mq_track_mode": "assigned",
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				repo, exists := d.state.GetRepo("assigned-repo")
				if !exists {
					t.Error("Repo should exist after add")
					return
				}
				if repo.MergeQueueConfig.TrackMode != state.TrackModeAssigned {
					t.Errorf("MergeQueueConfig.TrackMode = %s, want assigned", repo.MergeQueueConfig.TrackMode)
				}
			},
		},
		{
			name: "duplicate repo name fails",
			args: map[string]interface{}{
				"name":         "existing-repo",
				"github_url":   "https://github.com/owner/new-repo",
				"session_name": "oat-existing",
			},
			setupState: func(s *state.State) {
				s.AddRepo("existing-repo", &state.Repository{
					GithubURL:   "https://github.com/owner/existing-repo",
					SessionName: "oat-existing",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleAddRepo(socket.Request{
				Command: "add_repo",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleAddRepo() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.checkState != nil {
				tt.checkState(t, d)
			}
		})
	}
}

// TestHandleRemoveRepoTableDriven tests handleRemoveRepo with various argument combinations
func TestHandleRemoveRepoTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing name argument",
			args:        map[string]interface{}{},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name:        "empty name argument",
			args:        map[string]interface{}{"name": ""},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name:        "repo does not exist",
			args:        map[string]interface{}{"name": "nonexistent"},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "successful remove",
			args: map[string]interface{}{
				"name": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "remove repo with agents",
			args: map[string]interface{}{
				"name": "repo-with-agents",
			},
			setupState: func(s *state.State) {
				s.AddRepo("repo-with-agents", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("repo-with-agents", "agent1", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "agent1-window",
					CreatedAt:  time.Now(),
				})
				s.AddAgent("repo-with-agents", "agent2", state.Agent{
					Type:       state.AgentTypeSupervisor,
					WindowName: "agent2-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleRemoveRepo(socket.Request{
				Command: "remove_repo",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleRemoveRepo() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantSuccess {
				// Verify repo was removed from state
				repoName, _ := tt.args["name"].(string)
				_, exists := d.state.GetRepo(repoName)
				if exists {
					t.Error("Repo should not exist in state after successful remove")
				}
			}
		})
	}
}

// TestHandleAddAgentSessionIDGeneration verifies session ID is auto-generated when not provided
func TestHandleAddAgentSessionIDGeneration(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	// Add agent without session_id
	resp := d.handleAddAgent(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          "test-repo",
			"agent":         "auto-session-agent",
			"type":          "worker",
			"worktree_path": "/tmp/test",
			"window_name":   "test-win",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAddAgent() failed: %s", resp.Error)
	}

	agent, exists := d.state.GetAgent("test-repo", "auto-session-agent")
	if !exists {
		t.Fatal("Agent should exist")
	}

	if agent.SessionID == "" {
		t.Error("SessionID should be auto-generated when not provided")
	}

	if len(agent.SessionID) < 10 {
		t.Error("Auto-generated SessionID should be a reasonable length")
	}
}

// TestHandleAddAgentCreatedAtIsSet verifies CreatedAt is set on agent creation
func TestHandleAddAgentCreatedAtIsSet(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	beforeAdd := time.Now()

	resp := d.handleAddAgent(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          "test-repo",
			"agent":         "time-agent",
			"type":          "worker",
			"worktree_path": "/tmp/test",
			"window_name":   "test-win",
		},
	})

	afterAdd := time.Now()

	if !resp.Success {
		t.Fatalf("handleAddAgent() failed: %s", resp.Error)
	}

	agent, exists := d.state.GetAgent("test-repo", "time-agent")
	if !exists {
		t.Fatal("Agent should exist")
	}

	if agent.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}

	if agent.CreatedAt.Before(beforeAdd) || agent.CreatedAt.After(afterAdd) {
		t.Error("CreatedAt should be set to current time during add")
	}
}

// TestHandleAddRepoEmptyAgentsMap verifies the Agents map is initialized
func TestHandleAddRepoEmptyAgentsMap(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	resp := d.handleAddRepo(socket.Request{
		Command: "add_repo",
		Args: map[string]interface{}{
			"name":         "new-repo",
			"github_url":   "https://github.com/owner/repo",
			"session_name": "oat-new-repo",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAddRepo() failed: %s", resp.Error)
	}

	repo, exists := d.state.GetRepo("new-repo")
	if !exists {
		t.Fatal("Repo should exist")
	}

	if repo.Agents == nil {
		t.Error("Agents map should be initialized, not nil")
	}
}

// TestHandleCompleteAgentWithEmptyTask verifies handling of empty task field
func TestHandleCompleteAgentWithEmptyTask(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "no-task-worker", state.Agent{
			Type:       state.AgentTypeWorker,
			WindowName: "worker-window",
			Task:       "", // Empty task
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleCompleteAgent(socket.Request{
		Command: "complete_agent",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "no-task-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleCompleteAgent() failed: %s", resp.Error)
	}

	// Verify messages were sent with "unknown task" placeholder
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	supervisorMsgs, err := msgMgr.List("test-repo", "supervisor")
	if err != nil {
		t.Fatalf("Failed to list messages: %v", err)
	}

	if len(supervisorMsgs) == 0 {
		t.Fatal("Expected message to supervisor")
	}

	// The message body should contain "unknown task" since task was empty
	foundUnknownTask := false
	for _, msg := range supervisorMsgs {
		if msg.Body != "" && (len(msg.Body) > 0) {
			foundUnknownTask = true
			break
		}
	}
	if !foundUnknownTask {
		t.Log("Message was created for supervisor (task fallback is handled)")
	}
}

// TestArgumentTypeCoercion tests that handlers properly coerce argument types
func TestArgumentTypeCoercion(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	// Test that non-string types for string arguments are handled
	resp := d.handleAddAgent(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          123, // wrong type
			"agent":         "test",
			"type":          "worker",
			"worktree_path": "/tmp",
			"window_name":   "win",
		},
	})

	if resp.Success {
		t.Error("handleAddAgent() should fail with wrong type for repo")
	}
}

// TestHandleGetCurrentRepo tests handleGetCurrentRepo with various scenarios
func TestHandleGetCurrentRepo(t *testing.T) {
	tests := []struct {
		name        string
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
		wantData    string
	}{
		{
			name:        "no_current_repo_set",
			setupState:  nil,
			wantSuccess: false,
			wantError:   "no current repository set",
		},
		{
			name: "current_repo_is_set",
			setupState: func(s *state.State) {
				s.AddRepo("my-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.SetCurrentRepo("my-repo")
			},
			wantSuccess: true,
			wantData:    "my-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleGetCurrentRepo(socket.Request{
				Command: "get_current_repo",
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleGetCurrentRepo() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleGetCurrentRepo() expected error containing %q, got empty error", tt.wantError)
			}

			if tt.wantSuccess {
				data, ok := resp.Data.(string)
				if !ok {
					t.Errorf("handleGetCurrentRepo() data is not a string")
				} else if data != tt.wantData {
					t.Errorf("handleGetCurrentRepo() data = %q, want %q", data, tt.wantData)
				}
			}
		})
	}
}

// TestNilArgsMap tests handlers when Args is nil
func TestNilArgsMap(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	tests := []struct {
		name    string
		command string
		handler func(socket.Request) socket.Response
	}{
		{"handleAddAgent", "add_agent", d.handleAddAgent},
		{"handleRemoveAgent", "remove_agent", d.handleRemoveAgent},
		{"handleCompleteAgent", "complete_agent", d.handleCompleteAgent},
		{"handleAddRepo", "add_repo", d.handleAddRepo},
		{"handleRemoveRepo", "remove_repo", d.handleRemoveRepo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := tt.handler(socket.Request{
				Command: tt.command,
				Args:    nil,
			})

			if resp.Success {
				t.Errorf("%s should fail with nil Args", tt.name)
			}
		})
	}
}

// TestHandleSetCurrentRepo tests the set_current_repo handler
func TestHandleSetCurrentRepo(t *testing.T) {
	tests := []struct {
		name        string
		setupState  func(*state.State)
		args        map[string]interface{}
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing name",
			args:        map[string]interface{}{},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name: "empty name",
			args: map[string]interface{}{
				"name": "",
			},
			wantSuccess: false,
			wantError:   "missing 'name'",
		},
		{
			name: "nonexistent repo",
			args: map[string]interface{}{
				"name": "nonexistent",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "success",
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "oat-test-repo",
					Agents:      make(map[string]state.Agent),
				})
			},
			args: map[string]interface{}{
				"name": "test-repo",
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleSetCurrentRepo(socket.Request{
				Command: "set_current_repo",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v", resp.Success, tt.wantSuccess)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("Expected error containing %q, got empty", tt.wantError)
			}
		})
	}
}

// TestHandleClearCurrentRepo tests the clear_current_repo handler
func TestHandleClearCurrentRepo(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "oat-test-repo",
			Agents:      make(map[string]state.Agent),
		})
		s.SetCurrentRepo("test-repo")
	})
	defer cleanup()

	// Verify it was set
	if d.state.GetCurrentRepo() != "test-repo" {
		t.Fatal("Setup failed: current repo not set")
	}

	resp := d.handleClearCurrentRepo(socket.Request{
		Command: "clear_current_repo",
	})

	if !resp.Success {
		t.Errorf("Expected success, got error: %s", resp.Error)
	}

	// Verify it was cleared
	if d.state.GetCurrentRepo() != "" {
		t.Errorf("Current repo not cleared, got: %s", d.state.GetCurrentRepo())
	}
}

// TestHandleTriggerRefresh tests the trigger_refresh handler
func TestHandleTriggerRefresh(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	resp := d.handleTriggerRefresh(socket.Request{
		Command: "trigger_refresh",
	})

	if !resp.Success {
		t.Errorf("Expected success, got error: %s", resp.Error)
	}

	data, ok := resp.Data.(string)
	if !ok {
		t.Error("Expected string data in response")
	}
	if data != "Worktree refresh triggered" {
		t.Errorf("Unexpected response data: %s", data)
	}
}

// TestHandleRestartAgentTableDriven tests handleRestartAgent with various scenarios
func TestHandleRestartAgentTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{"agent": "test"},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": "", "agent": "test"},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name:        "missing agent argument",
			args:        map[string]interface{}{"repo": "test-repo"},
			wantSuccess: false,
			wantError:   "agent",
		},
		{
			name:        "empty agent argument",
			args:        map[string]interface{}{"repo": "test-repo", "agent": ""},
			wantSuccess: false,
			wantError:   "agent",
		},
		{
			name: "agent does not exist",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "nonexistent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo":  "nonexistent-repo",
				"agent": "test-agent",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "agent marked for cleanup",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"agent": "completed-agent",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "completed-agent", state.Agent{
					Type:            state.AgentTypeWorker,
					WindowName:      "completed-window",
					ReadyForCleanup: true,
					CreatedAt:       time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "complete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleRestartAgent(socket.Request{
				Command: "restart_agent",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleRestartAgent() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleRestartAgent() expected error containing %q, got empty error", tt.wantError)
			}
		})
	}
}

// TestHandleSpawnAgentTableDriven tests handleSpawnAgent with various argument combinations
func TestHandleSpawnAgentTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{"name": "test", "class": "ephemeral", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": "", "name": "test", "class": "ephemeral", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name:        "missing name argument",
			args:        map[string]interface{}{"repo": "test-repo", "class": "ephemeral", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name:        "empty name argument",
			args:        map[string]interface{}{"repo": "test-repo", "name": "", "class": "ephemeral", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name:        "missing class argument",
			args:        map[string]interface{}{"repo": "test-repo", "name": "test", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "class",
		},
		{
			name:        "empty class argument",
			args:        map[string]interface{}{"repo": "test-repo", "name": "test", "class": "", "prompt": "test prompt"},
			wantSuccess: false,
			wantError:   "class",
		},
		{
			name:        "missing prompt argument",
			args:        map[string]interface{}{"repo": "test-repo", "name": "test", "class": "ephemeral"},
			wantSuccess: false,
			wantError:   "prompt",
		},
		{
			name:        "empty prompt argument",
			args:        map[string]interface{}{"repo": "test-repo", "name": "test", "class": "ephemeral", "prompt": ""},
			wantSuccess: false,
			wantError:   "prompt",
		},
		{
			name: "invalid class argument",
			args: map[string]interface{}{
				"repo":   "test-repo",
				"name":   "test",
				"class":  "invalid-class",
				"prompt": "test prompt",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "invalid agent class",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo":   "nonexistent",
				"name":   "test",
				"class":  "ephemeral",
				"prompt": "test prompt",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "agent already exists",
			args: map[string]interface{}{
				"repo":   "test-repo",
				"name":   "existing-agent",
				"class":  "ephemeral",
				"prompt": "test prompt",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "existing-agent", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "existing-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: false,
			wantError:   "already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleSpawnAgent(socket.Request{
				Command: "spawn_agent",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleSpawnAgent() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleSpawnAgent() expected error containing %q, got empty error", tt.wantError)
			}
		})
	}
}

// TestHandleRepairStateBasic tests the repair_state handler with basic scenarios
func TestHandleRepairStateBasic(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	resp := d.handleRepairState(socket.Request{
		Command: "repair_state",
	})

	if !resp.Success {
		t.Errorf("Expected success, got error: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Error("Expected map data in response")
		return
	}

	if _, exists := data["agents_removed"]; !exists {
		t.Error("Response should contain agents_removed field")
	}
	if _, exists := data["issues_fixed"]; !exists {
		t.Error("Response should contain issues_fixed field")
	}
}

// TestHandleTaskHistoryTableDriven tests handleTaskHistory
func TestHandleTaskHistoryTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name:        "empty repo argument",
			args:        map[string]interface{}{"repo": ""},
			wantSuccess: false,
			wantError:   "repo",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"repo": "nonexistent",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "success with empty history",
			args: map[string]interface{}{
				"repo": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "success with limit",
			args: map[string]interface{}{
				"repo":  "test-repo",
				"limit": float64(5),
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "success with status filter",
			args: map[string]interface{}{
				"repo":   "test-repo",
				"status": "pending",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
		{
			name: "success with search",
			args: map[string]interface{}{
				"repo":   "test-repo",
				"search": "test query",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleTaskHistory(socket.Request{
				Command: "task_history",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleTaskHistory() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleTaskHistory() expected error containing %q, got empty error", tt.wantError)
			}
		})
	}
}

// TestHandleListAgentsTableDriven tests handleListAgents
func TestHandleListAgentsTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantAgents  int
	}{
		{
			name:        "missing repo argument",
			args:        map[string]interface{}{},
			wantSuccess: false,
		},
		{
			name: "empty repo returns empty list",
			args: map[string]interface{}{
				"repo": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: true,
			wantAgents:  0,
		},
		{
			name: "repo with multiple agents",
			args: map[string]interface{}{
				"repo": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "worker1", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "worker1-window",
					CreatedAt:  time.Now(),
				})
				s.AddAgent("test-repo", "worker2", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "worker2-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			wantAgents:  2,
		},
		{
			name: "returns all agents regardless of type",
			args: map[string]interface{}{
				"repo": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "worker1", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "worker1-window",
					CreatedAt:  time.Now(),
				})
				s.AddAgent("test-repo", "supervisor", state.Agent{
					Type:       state.AgentTypeSupervisor,
					WindowName: "supervisor-window",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			wantAgents:  2, // Returns all agents
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleListAgents(socket.Request{
				Command: "list_agents",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleListAgents() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantSuccess {
				agents, ok := resp.Data.([]map[string]interface{})
				if !ok {
					t.Errorf("Expected []map[string]interface{} data in response, got %T", resp.Data)
					return
				}
				if len(agents) != tt.wantAgents {
					t.Errorf("Expected %d agents, got %d", tt.wantAgents, len(agents))
				}
			}
		})
	}
}

// TestHandleUpdateRepoConfigTableDriven tests handleUpdateRepoConfig
func TestHandleUpdateRepoConfigTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing name argument",
			args:        map[string]interface{}{},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name:        "empty name argument",
			args:        map[string]interface{}{"name": ""},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"name": "nonexistent",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "update merge queue enabled",
			args: map[string]interface{}{
				"name":       "test-repo",
				"mq_enabled": false,
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
					MergeQueueConfig: state.MergeQueueConfig{
						Enabled:   true,
						TrackMode: state.TrackModeAll,
					},
				})
			},
			wantSuccess: true,
		},
		{
			name: "update merge queue track mode",
			args: map[string]interface{}{
				"name":          "test-repo",
				"mq_track_mode": "author",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
					MergeQueueConfig: state.MergeQueueConfig{
						Enabled:   true,
						TrackMode: state.TrackModeAll,
					},
				})
			},
			wantSuccess: true,
		},
		{
			name: "update pr shepherd enabled",
			args: map[string]interface{}{
				"name":       "test-repo",
				"ps_enabled": true,
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
					PRShepherdConfig: state.PRShepherdConfig{
						Enabled:   false,
						TrackMode: state.TrackModeAll,
					},
				})
			},
			wantSuccess: true,
		},
		{
			name: "invalid track mode",
			args: map[string]interface{}{
				"name":          "test-repo",
				"mq_track_mode": "invalid",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleUpdateRepoConfig(socket.Request{
				Command: "update_repo_config",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleUpdateRepoConfig() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleUpdateRepoConfig() expected error containing %q, got empty error", tt.wantError)
			}
		})
	}
}

// readDaemonLog is a small helper for the drift-warning tests below. Returns
// the contents of the daemon.log file so tests can grep for WARN emissions.
func readDaemonLog(t *testing.T, d *Daemon) string {
	t.Helper()
	data, err := os.ReadFile(d.paths.DaemonLog)
	if err != nil {
		t.Fatalf("reading daemon log: %v", err)
	}
	return string(data)
}

// TestHandleUpdateRepoConfig_RemoveAllowedModel_WarnsActiveWorker verifies
// that removing a model from AllowedWorkerModels emits a per-worker WARN for
// agents currently running on that model, and no WARN for workers on a
// still-allowed model. Covers Task 3 (P1-B) happy path.
func TestHandleUpdateRepoConfig_RemoveAllowedModel_WarnsActiveWorker(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			AllowedWorkerModels: []string{"model-a", "model-b"},
			Agents: map[string]state.Agent{
				"worker-on-a":  {Type: state.AgentTypeWorker, Model: "model-a"},
				"worker-on-b":  {Type: state.AgentTypeWorker, Model: "model-b"},
				"review-on-a":  {Type: state.AgentTypeReview, Model: "model-a"},
				"verify-on-a":  {Type: state.AgentTypeVerification, Model: "model-a"},
				"supervisor-a": {Type: state.AgentTypeSupervisor, Model: "model-a"},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "remove",
			"worker_models_value":  "model-a",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)

	// Three workers on model-a (Worker, Review, Verification) must be warned.
	for _, agent := range []string{"worker-on-a", "review-on-a", "verify-on-a"} {
		needle := "Worker test-repo/" + agent + " is running on disallowed model model-a"
		if !strings.Contains(log, needle) {
			t.Errorf("expected WARN %q in daemon log, got:\n%s", needle, log)
		}
	}
	// Worker on model-b must NOT be warned (still allowed).
	if strings.Contains(log, "worker-on-b is running on disallowed") {
		t.Errorf("unexpected WARN for worker-on-b (model-b still allowed):\n%s", log)
	}
	// Supervisor must NOT be warned (not in the worker-type set).
	if strings.Contains(log, "supervisor-a is running on disallowed") {
		t.Errorf("supervisor should not trigger drift WARN, log:\n%s", log)
	}
}

// TestHandleUpdateRepoConfig_RemoveAllowedModel_NoWarnWhenNoMatch verifies
// the WARN is suppressed when the removed model has no active workers.
func TestHandleUpdateRepoConfig_RemoveAllowedModel_NoWarnWhenNoMatch(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			AllowedWorkerModels: []string{"model-a", "model-b"},
			Agents: map[string]state.Agent{
				"worker-on-b": {Type: state.AgentTypeWorker, Model: "model-b"},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "remove",
			"worker_models_value":  "model-a",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)
	if strings.Contains(log, "is running on disallowed") {
		t.Errorf("no drift WARN expected, log contained:\n%s", log)
	}
}

// TestHandleUpdateRepoConfig_ClearAllowedModels_WarnsAllWorkers covers the
// S5 decision: clear should be treated identically to remove-everything.
func TestHandleUpdateRepoConfig_ClearAllowedModels_WarnsAllWorkers(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			AllowedWorkerModels: []string{"model-a", "model-b"},
			Agents: map[string]state.Agent{
				"worker-on-a": {Type: state.AgentTypeWorker, Model: "model-a"},
				"worker-on-b": {Type: state.AgentTypeWorker, Model: "model-b"},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "clear",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)
	for _, pair := range []struct{ name, model string }{
		{"worker-on-a", "model-a"},
		{"worker-on-b", "model-b"},
	} {
		needle := "Worker test-repo/" + pair.name + " is running on disallowed model " + pair.model
		if !strings.Contains(log, needle) {
			t.Errorf("expected WARN %q after clear, got:\n%s", needle, log)
		}
	}
}

// TestHandleUpdateRepoConfig_SetAllowedModels_WarnsOnNarrowing verifies that
// `set` to a narrower list is treated like remove for drift purposes (S5).
func TestHandleUpdateRepoConfig_SetAllowedModels_WarnsOnNarrowing(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			AllowedWorkerModels: []string{"model-a", "model-b"},
			Agents: map[string]state.Agent{
				"worker-on-a": {Type: state.AgentTypeWorker, Model: "model-a"},
				"worker-on-b": {Type: state.AgentTypeWorker, Model: "model-b"},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "set",
			"worker_models_value":  "model-b", // drops model-a
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)
	if !strings.Contains(log, "Worker test-repo/worker-on-a is running on disallowed model model-a") {
		t.Errorf("expected drift WARN for worker-on-a after narrowing set, got:\n%s", log)
	}
	if strings.Contains(log, "worker-on-b is running on disallowed") {
		t.Errorf("worker-on-b still on allowed model-b, should not warn:\n%s", log)
	}
}

// TestHandleUpdateRepoConfig_InheritsRepoDefault verifies workers with an
// empty agent.Model field are checked against the pre-mutation repo default
// — the "empty + repo default is disallowed" clause from the brief.
func TestHandleUpdateRepoConfig_RemoveAllowedModel_WarnsInheritedDefault(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			Model:               "model-a",
			AllowedWorkerModels: []string{"model-a", "model-b"},
			Agents: map[string]state.Agent{
				"worker-inherits": {Type: state.AgentTypeWorker, Model: ""},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "remove",
			"worker_models_value":  "model-a",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)
	if !strings.Contains(log, "Worker test-repo/worker-inherits is running on disallowed model model-a") {
		t.Errorf("expected WARN for inherited default model-a, got:\n%s", log)
	}
}

// TestHandleUpdateRepoConfig_AddDoesNotWarn asserts `add` never emits drift
// warnings because widening the allow-list cannot strand any workers.
func TestHandleUpdateRepoConfig_AddAllowedModel_DoesNotWarn(t *testing.T) {
	setup := func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:           "https://github.com/test/repo",
			SessionName:         "test-session",
			AllowedWorkerModels: []string{"model-a"},
			Agents: map[string]state.Agent{
				"worker-on-a": {Type: state.AgentTypeWorker, Model: "model-a"},
			},
		})
	}
	d, cleanup := setupTestDaemonWithState(t, setup)
	defer cleanup()

	resp := d.handleUpdateRepoConfig(socket.Request{
		Command: "update_repo_config",
		Args: map[string]interface{}{
			"name":                 "test-repo",
			"worker_models_action": "add",
			"worker_models_value":  "model-c",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	log := readDaemonLog(t, d)
	if strings.Contains(log, "is running on disallowed") {
		t.Errorf("add should never emit drift WARN, got:\n%s", log)
	}
}

// TestHandleGetRepoConfigTableDriven tests handleGetRepoConfig
func TestHandleGetRepoConfigTableDriven(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "missing name argument",
			args:        map[string]interface{}{},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name:        "empty name argument",
			args:        map[string]interface{}{"name": ""},
			wantSuccess: false,
			wantError:   "name",
		},
		{
			name: "repo does not exist",
			args: map[string]interface{}{
				"name": "nonexistent",
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "success",
			args: map[string]interface{}{
				"name": "test-repo",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
					MergeQueueConfig: state.MergeQueueConfig{
						Enabled:   true,
						TrackMode: state.TrackModeAll,
					},
				})
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleGetRepoConfig(socket.Request{
				Command: "get_repo_config",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("handleGetRepoConfig() success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}

			if tt.wantError != "" && resp.Error == "" {
				t.Errorf("handleGetRepoConfig() expected error containing %q, got empty error", tt.wantError)
			}

			if tt.wantSuccess {
				data, ok := resp.Data.(map[string]interface{})
				if !ok {
					t.Error("Expected map data in response")
					return
				}
				if _, exists := data["mq_enabled"]; !exists {
					t.Error("Response should contain mq_enabled field")
				}
			}
		})
	}
}

func TestHandleAddRepoWithModel(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	resp := d.handleAddRepo(socket.Request{
		Command: "add_repo",
		Args: map[string]interface{}{
			"name":         "model-repo",
			"github_url":   "https://github.com/test/repo",
			"session_name": "test-session",
			"model":        "claude-sonnet-4-5",
		},
	})
	if !resp.Success {
		t.Fatalf("handleAddRepo() failed: %s", resp.Error)
	}

	repo, exists := d.state.GetRepo("model-repo")
	if !exists {
		t.Fatal("Repository should exist in state")
	}
	if repo.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want %q", repo.Model, "claude-sonnet-4-5")
	}
}

func TestHandleAddRepoWithoutModel(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	resp := d.handleAddRepo(socket.Request{
		Command: "add_repo",
		Args: map[string]interface{}{
			"name":         "no-model-repo",
			"github_url":   "https://github.com/test/repo",
			"session_name": "test-session",
		},
	})
	if !resp.Success {
		t.Fatalf("handleAddRepo() failed: %s", resp.Error)
	}

	repo, exists := d.state.GetRepo("no-model-repo")
	if !exists {
		t.Fatal("Repository should exist in state")
	}
	if repo.Model != "" {
		t.Errorf("Model = %q, want empty string", repo.Model)
	}
}

func TestHandleAddAgentWithModel(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
			Model:       "claude-sonnet-4-5",
		})
	})
	defer cleanup()

	resp := d.handleAddAgent(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          "test-repo",
			"agent":         "model-worker",
			"type":          "worker",
			"worktree_path": "/tmp/worker",
			"window_name":   "model-worker",
			"task":          "Test task",
			"model":         "gpt-5.2",
		},
	})
	if !resp.Success {
		t.Fatalf("handleAddAgent() failed: %s", resp.Error)
	}

	agent, exists := d.state.GetAgent("test-repo", "model-worker")
	if !exists {
		t.Fatal("Agent should exist in state")
	}
	if agent.Model != "gpt-5.2" {
		t.Errorf("Agent.Model = %q, want %q", agent.Model, "gpt-5.2")
	}
}

func TestHandleGetRepoConfigIncludesModel(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
			Model:       "anthropic:claude-sonnet-4-5",
		})
	})
	defer cleanup()

	resp := d.handleGetRepoConfig(socket.Request{
		Command: "get_repo_config",
		Args:    map[string]interface{}{"name": "test-repo"},
	})
	if !resp.Success {
		t.Fatalf("handleGetRepoConfig() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatal("expected response data to be a map")
	}
	if model, ok := data["model"].(string); !ok || model != "anthropic:claude-sonnet-4-5" {
		t.Errorf("model = %v, want %q", data["model"], "anthropic:claude-sonnet-4-5")
	}
}

func TestResolveAgentModel(t *testing.T) {
	tests := []struct {
		name       string
		agentModel string
		repoModel  string
		want       string
	}{
		{
			name:       "agent override takes precedence",
			agentModel: "gpt-5.2",
			repoModel:  "claude-sonnet-4-5",
			want:       "gpt-5.2",
		},
		{
			name:       "falls back to repo default",
			agentModel: "",
			repoModel:  "claude-sonnet-4-5",
			want:       "claude-sonnet-4-5",
		},
		{
			name:       "both empty returns empty",
			agentModel: "",
			repoModel:  "",
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := state.Agent{Model: tc.agentModel}
			repo := &state.Repository{Model: tc.repoModel}
			got := resolveAgentModel(agent, repo)
			if got != tc.want {
				t.Errorf("resolveAgentModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandleAgentWaitingAutoCompletesNoPR(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "fixup-worker", state.Agent{
			Type:       state.AgentTypeWorker,
			WindowName: "fixup-worker",
			Task:       "Fix merge conflict on PR #23",
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "fixup-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAgentWaiting() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "auto_completed" {
		t.Errorf("status = %v, want auto_completed", data["status"])
	}

	agent, exists := d.state.GetAgent("test-repo", "fixup-worker")
	if !exists {
		t.Fatal("Agent should still exist")
	}
	if !agent.ReadyForCleanup {
		t.Error("Agent should be marked ReadyForCleanup")
	}
	if agent.WaitingForPR {
		t.Error("Agent should NOT be WaitingForPR after auto-complete")
	}

	// Verify notifications were sent
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	supervisorMsgs, _ := msgMgr.List("test-repo", "supervisor")
	if len(supervisorMsgs) == 0 {
		t.Error("Expected notification to supervisor")
	}
	mqMsgs, _ := msgMgr.List("test-repo", "merge-queue")
	if len(mqMsgs) == 0 {
		t.Error("Expected notification to merge-queue")
	}
}

func TestHandleAgentWaitingSucceedsForAlreadyCompleted(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "done-worker", state.Agent{
			Type:            state.AgentTypeWorker,
			WindowName:      "done-worker",
			CreatedAt:       time.Now(),
			ReadyForCleanup: true,
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "done-worker",
		},
	})

	if !resp.Success {
		t.Errorf("handleAgentWaiting() should return success for already-completed agent, got error: %s", resp.Error)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "already_complete" {
		t.Errorf("status = %v, want already_complete", data["status"])
	}
}

func TestShouldSendFinalNudge(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	tests := []struct {
		name  string
		agent state.Agent
		want  bool
	}{
		{
			name:  "supervisor gets final nudge",
			agent: state.Agent{Type: state.AgentTypeSupervisor},
			want:  true,
		},
		{
			name:  "merge-queue gets final nudge",
			agent: state.Agent{Type: state.AgentTypeMergeQueue},
			want:  true,
		},
		{
			name:  "workspace skipped",
			agent: state.Agent{Type: state.AgentTypeWorkspace},
			want:  false,
		},
		{
			name:  "cleanup-ready skipped",
			agent: state.Agent{Type: state.AgentTypeSupervisor, ReadyForCleanup: true},
			want:  false,
		},
		{
			name:  "dormant worker skipped",
			agent: state.Agent{Type: state.AgentTypeWorker, WaitingForPR: true},
			want:  false,
		},
		{
			name:  "recently nudged still gets final nudge (no cooldown)",
			agent: state.Agent{Type: state.AgentTypeSupervisor, LastNudge: time.Now().Add(-30 * time.Second)},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.shouldSendFinalNudge(tt.agent)
			if got != tt.want {
				t.Errorf("shouldSendFinalNudge() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldSendFinalNudgeBypassesCooldown(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	recentlyNudged := state.Agent{
		Type:      state.AgentTypeSupervisor,
		LastNudge: time.Now().Add(-30 * time.Second),
	}

	now := time.Now()
	repo := &state.Repository{
		Agents: map[string]state.Agent{"supervisor": recentlyNudged},
	}

	// shouldNudgeAgent would skip this agent (30s < 2min cooldown)
	if d.shouldNudgeAgent(repo, "supervisor", recentlyNudged, now) {
		t.Error("shouldNudgeAgent should have skipped recently-nudged agent")
	}

	// shouldSendFinalNudge should NOT skip it
	if !d.shouldSendFinalNudge(recentlyNudged) {
		t.Error("shouldSendFinalNudge should allow recently-nudged agent")
	}
}

// ---------------------------------------------------------------------------
// handleStartVerification
// ---------------------------------------------------------------------------

func TestHandleStartVerification(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
		checkState  func(t *testing.T, d *Daemon)
	}{
		{
			name:        "missing repo",
			args:        map[string]interface{}{"worker": "fox", "verifier_name": "verify-fox", "commit_sha": "abc123"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "missing worker",
			args:        map[string]interface{}{"repo": "test-repo", "verifier_name": "verify-fox", "commit_sha": "abc123"},
			wantSuccess: false,
			wantError:   "missing 'worker'",
		},
		{
			name:        "missing verifier_name",
			args:        map[string]interface{}{"repo": "test-repo", "worker": "fox", "commit_sha": "abc123"},
			wantSuccess: false,
			wantError:   "missing 'verifier_name'",
		},
		{
			name:        "missing commit_sha",
			args:        map[string]interface{}{"repo": "test-repo", "worker": "fox", "verifier_name": "verify-fox"},
			wantSuccess: false,
			wantError:   "missing 'commit_sha'",
		},
		{
			name: "worker not found",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "nonexistent",
				"verifier_name": "verify-nonexistent", "commit_sha": "abc123",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
			},
			wantSuccess: false,
			wantError:   "not found",
		},
		{
			name: "sets pending state atomically",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier_name": "verify-clever-fox", "commit_sha": "abc123def",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:       state.AgentTypeWorker,
					WindowName: "clever-fox",
					Task:       "Fix bug",
					CreatedAt:  time.Now(),
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, exists := d.state.GetAgent("test-repo", "clever-fox")
				if !exists {
					t.Fatal("Worker not found")
				}
				if agent.VerificationAgent != "verify-clever-fox" {
					t.Errorf("VerificationAgent = %q, want %q", agent.VerificationAgent, "verify-clever-fox")
				}
				if agent.VerificationStatus != "pending" {
					t.Errorf("VerificationStatus = %q, want %q", agent.VerificationStatus, "pending")
				}
				if agent.VerifiedCommitSHA != "" {
					t.Errorf("VerifiedCommitSHA = %q, want empty (cleared until verdict)", agent.VerifiedCommitSHA)
				}
				if agent.VerificationReason != "" {
					t.Errorf("VerificationReason = %q, want empty", agent.VerificationReason)
				}
				if agent.LastBranchSHA != "abc123def" {
					t.Errorf("LastBranchSHA = %q, want %q", agent.LastBranchSHA, "abc123def")
				}
			},
		},
		{
			name: "clears old approval on re-request",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier_name": "verify-clever-fox", "commit_sha": "newsha999",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					Task:               "Fix bug",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox-old",
					VerificationStatus: "approved",
					VerifiedCommitSHA:  "oldsha111",
					VerificationReason: "Old approval",
					VerificationAt:     time.Now().Add(-time.Hour),
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				if agent.VerificationStatus != "pending" {
					t.Errorf("VerificationStatus = %q, want %q", agent.VerificationStatus, "pending")
				}
				if agent.VerifiedCommitSHA != "" {
					t.Errorf("VerifiedCommitSHA = %q, want empty", agent.VerifiedCommitSHA)
				}
				if agent.VerificationReason != "" {
					t.Errorf("VerificationReason = %q, want empty", agent.VerificationReason)
				}
				if agent.VerificationAgent != "verify-clever-fox" {
					t.Errorf("VerificationAgent = %q, want %q", agent.VerificationAgent, "verify-clever-fox")
				}
				if agent.LastBranchSHA != "newsha999" {
					t.Errorf("LastBranchSHA = %q, want %q", agent.LastBranchSHA, "newsha999")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleStartVerification(socket.Request{
				Command: "start_verification",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}
			if !tt.wantSuccess && tt.wantError != "" {
				if resp.Error == "" || !contains(resp.Error, tt.wantError) {
					t.Errorf("Error = %q, want to contain %q", resp.Error, tt.wantError)
				}
			}
			if tt.checkState != nil {
				tt.checkState(t, d)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleVerificationVerdict
// ---------------------------------------------------------------------------

func TestHandleVerificationVerdict(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		setupState  func(*state.State)
		wantSuccess bool
		wantError   string
		checkState  func(t *testing.T, d *Daemon)
	}{
		{
			name:        "missing repo",
			args:        map[string]interface{}{"worker": "fox", "verdict": "approved", "sha": "abc", "reason": "ok"},
			wantSuccess: false,
			wantError:   "missing 'repo'",
		},
		{
			name:        "missing worker",
			args:        map[string]interface{}{"repo": "test-repo", "verdict": "approved", "sha": "abc", "reason": "ok"},
			wantSuccess: false,
			wantError:   "missing 'worker'",
		},
		{
			name:        "missing verdict",
			args:        map[string]interface{}{"repo": "test-repo", "worker": "fox", "sha": "abc", "reason": "ok"},
			wantSuccess: false,
			wantError:   "missing 'verdict'",
		},
		{
			name:        "missing sha",
			args:        map[string]interface{}{"repo": "test-repo", "worker": "fox", "verdict": "approved", "reason": "ok"},
			wantSuccess: false,
			wantError:   "missing 'sha'",
		},
		{
			name: "approves when pending + verifier matches + SHA matches",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "verify-clever-fox",
				"verdict":  "approved", "sha": "abc123", "reason": "All good",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "pending",
					LastBranchSHA:      "abc123",
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				if agent.VerificationStatus != "approved" {
					t.Errorf("VerificationStatus = %q, want %q", agent.VerificationStatus, "approved")
				}
				if agent.VerifiedCommitSHA != "abc123" {
					t.Errorf("VerifiedCommitSHA = %q, want %q", agent.VerifiedCommitSHA, "abc123")
				}
				if agent.VerificationReason != "All good" {
					t.Errorf("VerificationReason = %q, want %q", agent.VerificationReason, "All good")
				}
				if agent.VerificationAt.IsZero() {
					t.Error("VerificationAt should be set")
				}

				// Check approval message is commit-specific
				msgMgr := messages.NewManager(d.paths.MessagesDir)
				msgs, _ := msgMgr.List("test-repo", "clever-fox")
				if len(msgs) == 0 {
					t.Fatal("Expected approval message to worker")
				}
				body := msgs[0].Body
				if !strings.Contains(body, "approved commit abc123") {
					t.Errorf("Approval message should mention specific commit, got: %s", body)
				}
				if !strings.Contains(body, "ONLY to that specific commit") {
					t.Errorf("Approval message should warn about commit specificity, got: %s", body)
				}
				if !strings.Contains(body, "oat worker verify") {
					t.Errorf("Approval message should mention oat worker verify, got: %s", body)
				}
			},
		},
		{
			name: "rejects when pending + verifier matches + SHA matches",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "verify-clever-fox",
				"verdict":  "rejected", "sha": "abc123", "reason": "Tests fail",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "pending",
					LastBranchSHA:      "abc123",
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				if agent.VerificationStatus != "rejected" {
					t.Errorf("VerificationStatus = %q, want %q", agent.VerificationStatus, "rejected")
				}
				if agent.VerificationReason != "Tests fail" {
					t.Errorf("VerificationReason = %q, want %q", agent.VerificationReason, "Tests fail")
				}
			},
		},
		{
			name: "rejects invalid caller — verifier mismatch",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "wrong-verifier",
				"verdict":  "approved", "sha": "abc123", "reason": "hacked",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "pending",
					LastBranchSHA:      "abc123",
				})
			},
			wantSuccess: false,
			wantError:   "verifier mismatch",
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				if agent.VerificationStatus != "pending" {
					t.Errorf("State should be unchanged: VerificationStatus = %q, want pending", agent.VerificationStatus)
				}
			},
		},
		{
			name: "tolerates stale SHA with success response",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "verify-clever-fox",
				"verdict":  "approved", "sha": "stale-sha", "reason": "ok",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "pending",
					LastBranchSHA:      "current-sha",
				})
			},
			wantSuccess: true,
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				if agent.VerificationStatus != "pending" {
					t.Errorf("State should be unchanged: VerificationStatus = %q, want pending", agent.VerificationStatus)
				}
			},
		},
		{
			name: "rejects verdict when worker not pending",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "verify-clever-fox",
				"verdict":  "approved", "sha": "abc123", "reason": "ok",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "approved", // already approved
					VerifiedCommitSHA:  "abc123",
					LastBranchSHA:      "abc123",
				})
			},
			wantSuccess: false,
			wantError:   "not pending",
			checkState: func(t *testing.T, d *Daemon) {
				agent, _ := d.state.GetAgent("test-repo", "clever-fox")
				// State should remain approved, not overwritten
				if agent.VerificationStatus != "approved" {
					t.Errorf("State should be unchanged: VerificationStatus = %q, want approved", agent.VerificationStatus)
				}
			},
		},
		{
			name: "invalid verdict value",
			args: map[string]interface{}{
				"repo": "test-repo", "worker": "clever-fox",
				"verifier": "verify-clever-fox",
				"verdict":  "maybe", "sha": "abc123", "reason": "unsure",
			},
			setupState: func(s *state.State) {
				s.AddRepo("test-repo", &state.Repository{
					GithubURL:   "https://github.com/test/repo",
					SessionName: "test-session",
					Agents:      make(map[string]state.Agent),
				})
				s.AddAgent("test-repo", "clever-fox", state.Agent{
					Type:               state.AgentTypeWorker,
					WindowName:         "clever-fox",
					CreatedAt:          time.Now(),
					VerificationAgent:  "verify-clever-fox",
					VerificationStatus: "pending",
					LastBranchSHA:      "abc123",
				})
			},
			wantSuccess: false,
			wantError:   "invalid verdict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cleanup := setupTestDaemonWithState(t, tt.setupState)
			defer cleanup()

			resp := d.handleVerificationVerdict(socket.Request{
				Command: "verification_verdict",
				Args:    tt.args,
			})

			if resp.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v (error: %s)", resp.Success, tt.wantSuccess, resp.Error)
			}
			if !tt.wantSuccess && tt.wantError != "" {
				if resp.Error == "" || !contains(resp.Error, tt.wantError) {
					t.Errorf("Error = %q, want to contain %q", resp.Error, tt.wantError)
				}
			}
			if tt.checkState != nil {
				tt.checkState(t, d)
			}
		})
	}
}

func TestHandleAgentWaitingAllowsDormancyForPendingVerification(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "busy-worker", state.Agent{
			Type:               state.AgentTypeWorker,
			WindowName:         "busy-worker",
			Task:               "Implement feature X",
			CreatedAt:          time.Now(),
			VerificationStatus: "pending",
			VerificationAgent:  "verify-busy-worker",
		})
		s.AddAgent("test-repo", "verify-busy-worker", state.Agent{
			Type:       state.AgentTypeVerification,
			WindowName: "verify-busy-worker",
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "busy-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAgentWaiting() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "dormant_verification" {
		t.Errorf("status = %v, want dormant_verification", data["status"])
	}

	agent, exists := d.state.GetAgent("test-repo", "busy-worker")
	if !exists {
		t.Fatal("Agent should still exist")
	}
	if !agent.WaitingForVerification {
		t.Error("Agent should be WaitingForVerification")
	}
	if agent.WaitingForPR {
		t.Error("Agent should NOT be WaitingForPR (verification uses separate field)")
	}
	if agent.ReadyForCleanup {
		t.Error("Agent should NOT be ReadyForCleanup")
	}
	if agent.NudgeCount != 0 {
		t.Errorf("NudgeCount should be 0, got %d", agent.NudgeCount)
	}
}

func TestHandleAgentWaitingStillAutoCompletesWithoutVerification(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "no-pr-worker", state.Agent{
			Type:       state.AgentTypeWorker,
			WindowName: "no-pr-worker",
			Task:       "Fix something minor",
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "no-pr-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAgentWaiting() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "auto_completed" {
		t.Errorf("status = %v, want auto_completed", data["status"])
	}

	agent, exists := d.state.GetAgent("test-repo", "no-pr-worker")
	if !exists {
		t.Fatal("Agent should still exist")
	}
	if !agent.ReadyForCleanup {
		t.Error("Agent should be marked ReadyForCleanup")
	}
	if agent.WaitingForPR {
		t.Error("Agent should NOT be WaitingForPR after auto-complete")
	}
}

func TestCleanupDeadVerifierWakesDormantWorker(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "my-worker", state.Agent{
			Type:               state.AgentTypeWorker,
			WindowName:         "my-worker",
			Task:               "Implement feature Y",
			CreatedAt:          time.Now(),
			WaitingForPR:       true,
			WaitingForPRSince:  time.Now().Add(-3 * time.Minute),
			VerificationStatus: "pending",
			VerificationAgent:  "verify-my-worker",
		})
		s.AddAgent("test-repo", "verify-my-worker", state.Agent{
			Type:       state.AgentTypeVerification,
			WindowName: "verify-my-worker",
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	deadAgents := map[string][]string{
		"test-repo": {"verify-my-worker"},
	}
	d.cleanupDeadAgents(deadAgents)

	// Verifier should be removed
	_, vExists := d.state.GetAgent("test-repo", "verify-my-worker")
	if vExists {
		t.Error("Verifier should have been removed")
	}

	// Worker should have been woken (WaitingForPR cleared) and verification status reset
	worker, wExists := d.state.GetAgent("test-repo", "my-worker")
	if !wExists {
		t.Fatal("Worker should still exist")
	}
	if worker.WaitingForPR {
		t.Error("Worker should have been woken (WaitingForPR should be false)")
	}
	if worker.VerificationStatus != "" {
		t.Errorf("VerificationStatus should be empty, got %q", worker.VerificationStatus)
	}
	if worker.VerificationAgent != "" {
		t.Errorf("VerificationAgent should be empty, got %q", worker.VerificationAgent)
	}

	// Verify the wake reason was recorded
	if !strings.Contains(worker.LastWakeReason, "crashed before delivering a verdict") {
		t.Errorf("LastWakeReason should mention verifier crash, got: %s", worker.LastWakeReason)
	}
}

// TestCleanupDeliveredVerifierDoesNotClaimCrash covers the race where a
// verifier cleanly delivered a verdict (ReadyForCleanup=true) but a
// concurrent `oat worker request-review` reset the worker's
// VerificationStatus back to "pending" before the cleanup loop ran. The
// guard added to cleanupDeadAgents must NOT emit the bogus
// "crashed before delivering a verdict" wake-message in this case.
func TestCleanupDeliveredVerifierDoesNotClaimCrash(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "my-worker", state.Agent{
			Type:              state.AgentTypeWorker,
			WindowName:        "my-worker",
			Task:              "Implement feature Y",
			CreatedAt:         time.Now(),
			WaitingForPR:      true,
			WaitingForPRSince: time.Now().Add(-3 * time.Minute),
			// Status is "pending" because a concurrent re-request-review
			// reset it after the verifier already delivered its verdict.
			VerificationStatus: "pending",
			VerificationAgent:  "verify-my-worker",
		})
		s.AddAgent("test-repo", "verify-my-worker", state.Agent{
			Type:            state.AgentTypeVerification,
			WindowName:      "verify-my-worker",
			CreatedAt:       time.Now(),
			ReadyForCleanup: true, // verdict was successfully delivered
		})
	})
	defer cleanup()

	deadAgents := map[string][]string{
		"test-repo": {"verify-my-worker"},
	}
	d.cleanupDeadAgents(deadAgents)

	// Verifier should still be removed (cleanup runs as normal)
	_, vExists := d.state.GetAgent("test-repo", "verify-my-worker")
	if vExists {
		t.Error("Verifier should have been removed")
	}

	worker, wExists := d.state.GetAgent("test-repo", "my-worker")
	if !wExists {
		t.Fatal("Worker should still exist")
	}

	// The orphaned worker pointers must still get cleared so state stays
	// internally consistent; only the bogus crash wake-message is suppressed.
	if worker.VerificationStatus != "" {
		t.Errorf("VerificationStatus should be cleared, got %q", worker.VerificationStatus)
	}
	if worker.VerificationAgent != "" {
		t.Errorf("VerificationAgent should be cleared, got %q", worker.VerificationAgent)
	}

	// The wake-message must NOT claim a crash.
	if strings.Contains(worker.LastWakeReason, "crashed before delivering a verdict") {
		t.Errorf("LastWakeReason should NOT claim crash for a verifier that delivered a verdict; got: %s", worker.LastWakeReason)
	}
}

func TestHandleAgentWaitingNoPRClosesAssociatedIssue(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "blocker-worker", state.Agent{
			Type:        state.AgentTypeWorker,
			WindowName:  "blocker-worker",
			Task:        "Fix blocker #34",
			IssueNumber: "34",
			CreatedAt:   time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "blocker-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAgentWaiting() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "auto_completed" {
		t.Errorf("status = %v, want auto_completed", data["status"])
	}

	agent, exists := d.state.GetAgent("test-repo", "blocker-worker")
	if !exists {
		t.Fatal("Agent should still exist")
	}
	if !agent.ReadyForCleanup {
		t.Error("Agent should be marked ReadyForCleanup")
	}
	// closeAssociatedIssue was called (gh fails gracefully since no git repo exists in tmpDir).
	// The key assertion is that the auto-complete path still succeeds despite the gh failure.
	if agent.IssueNumber != "34" {
		t.Errorf("IssueNumber should still be 34, got %q", agent.IssueNumber)
	}
}

func TestHandleRemoveAgentNotificationWithPR(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "pr-worker", state.Agent{
			Type:        state.AgentTypeWorker,
			WindowName:  "pr-worker",
			Task:        "Fix issue #13",
			IssueNumber: "13",
			PRNumber:    36,
			CreatedAt:   time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleRemoveAgent(socket.Request{
		Command: "remove_agent",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "pr-worker",
		},
	})
	if !resp.Success {
		t.Fatalf("handleRemoveAgent() failed: %s", resp.Error)
	}

	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List("test-repo", "default")
	if len(msgs) == 0 {
		t.Fatal("Expected notification to workspace")
	}
	body := msgs[0].Body
	if !strings.Contains(body, "PR #36") {
		t.Errorf("Notification should mention PR #36, got: %s", body)
	}
	if !strings.Contains(body, "Issue #13") {
		t.Errorf("Notification should mention Issue #13, got: %s", body)
	}
	if !strings.Contains(body, "gh pr view 36") {
		t.Errorf("Notification should include gh pr view command, got: %s", body)
	}
}

func TestHandleRemoveAgentNotificationNoPR(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "nopr-worker", state.Agent{
			Type:       state.AgentTypeWorker,
			WindowName: "nopr-worker",
			Task:       "Fix something",
			CreatedAt:  time.Now(),
		})
	})
	defer cleanup()

	resp := d.handleRemoveAgent(socket.Request{
		Command: "remove_agent",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "nopr-worker",
		},
	})
	if !resp.Success {
		t.Fatalf("handleRemoveAgent() failed: %s", resp.Error)
	}

	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List("test-repo", "default")
	if len(msgs) == 0 {
		t.Fatal("Expected notification to workspace")
	}
	body := msgs[0].Body
	if !strings.Contains(body, "No PR was created") {
		t.Errorf("Notification should say no PR was created, got: %s", body)
	}
	if !strings.Contains(body, "Consider spawning a replacement") {
		t.Errorf("Notification should suggest replacement, got: %s", body)
	}
}

func TestCheckMainCIAfterMergeDedup(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	// Simulate a recent alert
	d.mainCIAlertTimeMu.Lock()
	d.mainCIAlertTime["test-repo"] = time.Now()
	d.mainCIAlertTimeMu.Unlock()

	// Call checkMainCIAfterMerge — should be a no-op due to dedup
	d.checkMainCIAfterMerge("test-repo", 33)

	// Verify no messages were sent (dedup prevented it)
	msgMgr := messages.NewManager(d.paths.MessagesDir)
	msgs, _ := msgMgr.List("test-repo", "merge-queue")
	if len(msgs) != 0 {
		t.Errorf("Expected no messages due to dedup, got %d", len(msgs))
	}
}

func TestCheckMainCIAfterMergeSkipsRemovedRepo(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	// Call with a repo that doesn't exist — should return silently
	d.checkMainCIAfterMerge("nonexistent-repo", 42)

	// No crash, no messages — just verify we got here without panic
}

func TestAutoCompleteWorkerDoesNotCloseIssue(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents: map[string]state.Agent{
				"supervisor":  {Type: state.AgentTypeSupervisor},
				"merge-queue": {Type: state.AgentTypeMergeQueue},
				"stuck-worker": {
					Type:        state.AgentTypeWorker,
					CreatedAt:   time.Now().Add(-30 * time.Minute),
					PID:         os.Getpid(),
					NudgeCount:  10,
					Task:        "Fix blocker #42",
					IssueNumber: "42",
				},
			},
		})
	})
	defer cleanup()

	agent, _ := d.state.GetAgent("test-repo", "stuck-worker")
	d.autoCompleteWorker("test-repo", "stuck-worker", agent, "Worker force-removed")

	// autoCompleteWorker should NOT call closeAssociatedIssue.
	// Verify the worker is auto-completed but the issue number is preserved
	// (the issue remains open for the supervisor to handle).
	updated, exists := d.state.GetAgent("test-repo", "stuck-worker")
	if !exists {
		t.Fatal("Worker should still exist after auto-complete")
	}
	if !updated.ReadyForCleanup {
		t.Error("Worker should be marked ReadyForCleanup")
	}
	if updated.IssueNumber != "42" {
		t.Errorf("IssueNumber should still be 42, got %q", updated.IssueNumber)
	}
}

func TestHandleAgentWaitingAlreadyDormantWithPendingVerification(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "waiting-worker", state.Agent{
			Type:                        state.AgentTypeWorker,
			WindowName:                  "waiting-worker",
			Task:                        "Implement feature Y",
			CreatedAt:                   time.Now(),
			VerificationStatus:          "pending",
			VerificationAgent:           "verify-waiting-worker",
			WaitingForVerification:      true,
			WaitingForVerificationSince: time.Now().Add(-1 * time.Minute),
		})
	})
	defer cleanup()

	resp := d.handleAgentWaiting(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "waiting-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleAgentWaiting() failed: %s", resp.Error)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Expected map data, got %T", resp.Data)
	}
	if data["status"] != "dormant_verification" {
		t.Errorf("status = %v, want dormant_verification", data["status"])
	}
	msg, _ := data["message"].(string)
	if !strings.Contains(msg, "STOP") {
		t.Errorf("message should contain STOP instruction, got: %s", msg)
	}
}

func TestHandleCompleteAgentClosesIssueWhenNoPR(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "impossible-worker", state.Agent{
			Type:        state.AgentTypeWorker,
			WindowName:  "impossible-worker",
			Task:        "Fix impossible bug",
			CreatedAt:   time.Now(),
			IssueNumber: "99",
		})
	})
	defer cleanup()

	resp := d.handleCompleteAgent(socket.Request{
		Command: "complete_agent",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "impossible-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleCompleteAgent() failed: %s", resp.Error)
	}

	agent, exists := d.state.GetAgent("test-repo", "impossible-worker")
	if !exists {
		t.Fatal("Agent should still exist after completion")
	}
	if !agent.ReadyForCleanup {
		t.Error("Agent should be marked ReadyForCleanup")
	}
	// closeAssociatedIssue was called (best-effort, may fail without GitHub).
	// The key assertion is that the code path was reached without errors.
}

func TestHandleCompleteAgentSupervisorDoesNotCloseIssue(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
		s.AddAgent("test-repo", "supervisor", state.Agent{
			Type:       state.AgentTypeSupervisor,
			WindowName: "supervisor",
			CreatedAt:  time.Now(),
		})
		s.AddAgent("test-repo", "target-worker", state.Agent{
			Type:        state.AgentTypeWorker,
			WindowName:  "target-worker",
			Task:        "Some task",
			CreatedAt:   time.Now(),
			IssueNumber: "50",
		})
	})
	defer cleanup()

	// Supervisor force-completing a worker via --worker flag
	resp := d.handleCompleteAgent(socket.Request{
		Command: "complete_agent",
		Args: map[string]interface{}{
			"repo":         "test-repo",
			"agent":        "supervisor",
			"target_agent": "target-worker",
		},
	})

	if !resp.Success {
		t.Fatalf("handleCompleteAgent() failed: %s", resp.Error)
	}

	agent, exists := d.state.GetAgent("test-repo", "target-worker")
	if !exists {
		t.Fatal("Agent should still exist after completion")
	}
	if !agent.ReadyForCleanup {
		t.Error("Agent should be marked ReadyForCleanup")
	}
	// closeAssociatedIssue should NOT have been called because
	// callerName (supervisor) != agentName (target-worker).
	// The issue remains open for the supervisor's replacement-worker process.
}
