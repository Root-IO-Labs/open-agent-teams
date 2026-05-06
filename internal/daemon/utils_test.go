package daemon

import (
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Tests for getRequiredStringArg helper function

func TestGetRequiredStringArg(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		key         string
		description string
		wantValue   string
		wantOK      bool
	}{
		{
			name:        "valid string",
			args:        map[string]interface{}{"name": "test-value"},
			key:         "name",
			description: "name is required",
			wantValue:   "test-value",
			wantOK:      true,
		},
		{
			name:        "missing key",
			args:        map[string]interface{}{"other": "value"},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "empty string",
			args:        map[string]interface{}{"name": ""},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "wrong type - int",
			args:        map[string]interface{}{"name": 123},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "wrong type - bool",
			args:        map[string]interface{}{"name": true},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "nil value",
			args:        map[string]interface{}{"name": nil},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "nil args map",
			args:        nil,
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
		{
			name:        "whitespace only string",
			args:        map[string]interface{}{"name": "   "},
			key:         "name",
			description: "name is required",
			wantValue:   "   ",
			wantOK:      true, // Note: whitespace strings are technically valid
		},
		{
			name:        "float type",
			args:        map[string]interface{}{"name": 3.14},
			key:         "name",
			description: "name is required",
			wantValue:   "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, resp, ok := getRequiredStringArg(tt.args, tt.key, tt.description)

			if ok != tt.wantOK {
				t.Errorf("getRequiredStringArg() ok = %v, want %v", ok, tt.wantOK)
			}

			if value != tt.wantValue {
				t.Errorf("getRequiredStringArg() value = %q, want %q", value, tt.wantValue)
			}

			if !ok && resp.Success {
				t.Error("Response should indicate failure when ok=false")
			}

			if !ok && resp.Error == "" {
				t.Error("Response should contain error message when ok=false")
			}
		})
	}
}

// Tests for recordTaskHistory function

func TestRecordTaskHistory(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Add a test repo
	repo := &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}
	if err := d.state.AddRepo("test-repo", repo); err != nil {
		t.Fatalf("Failed to add repo: %v", err)
	}

	// Test recording task history
	createdAt := time.Now().Add(-1 * time.Hour)
	agent := state.Agent{
		Type:         state.AgentTypeWorker,
		WorktreePath: "/tmp/test-worker",
		WindowName:   "test-window",
		Task:         "implement feature X",
		Summary:      "completed successfully",
		CreatedAt:    createdAt,
	}

	d.recordTaskHistory("test-repo", "test-worker", agent)

	// Verify task was recorded
	history, err := d.state.GetTaskHistory("test-repo", 10)
	if err != nil {
		t.Fatalf("GetTaskHistory() failed: %v", err)
	}

	if len(history) == 0 {
		t.Fatal("Expected task history entry")
	}

	entry := history[0]
	if entry.Name != "test-worker" {
		t.Errorf("Entry name = %q, want %q", entry.Name, "test-worker")
	}
	if entry.Task != "implement feature X" {
		t.Errorf("Entry task = %q, want %q", entry.Task, "implement feature X")
	}
	if entry.Summary != "completed successfully" {
		t.Errorf("Entry summary = %q, want %q", entry.Summary, "completed successfully")
	}
	// Status should be unknown since there's no failure reason
	if entry.Status != state.TaskStatusUnknown {
		t.Errorf("Entry status = %q, want %q", entry.Status, state.TaskStatusUnknown)
	}
}

func TestRecordTaskHistoryWithFailure(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Add a test repo
	repo := &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}
	if err := d.state.AddRepo("test-repo", repo); err != nil {
		t.Fatalf("Failed to add repo: %v", err)
	}

	// Test recording failed task
	agent := state.Agent{
		Type:          state.AgentTypeWorker,
		WindowName:    "failed-window",
		Task:          "broken feature",
		FailureReason: "tests failed",
		CreatedAt:     time.Now().Add(-30 * time.Minute),
	}

	d.recordTaskHistory("test-repo", "failed-worker", agent)

	history, err := d.state.GetTaskHistory("test-repo", 10)
	if err != nil {
		t.Fatalf("GetTaskHistory() failed: %v", err)
	}

	if len(history) == 0 {
		t.Fatal("Expected task history entry")
	}

	entry := history[0]
	if entry.Status != state.TaskStatusFailed {
		t.Errorf("Entry status = %q, want %q", entry.Status, state.TaskStatusFailed)
	}
	if entry.FailureReason != "tests failed" {
		t.Errorf("Entry failure_reason = %q, want %q", entry.FailureReason, "tests failed")
	}
}

func TestRecordTaskHistoryBranchFromWorktree(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Add a test repo
	repo := &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}
	if err := d.state.AddRepo("test-repo", repo); err != nil {
		t.Fatalf("Failed to add repo: %v", err)
	}

	// Test with empty worktree path - should use fallback branch name
	agent := state.Agent{
		Type:         state.AgentTypeWorker,
		WorktreePath: "", // Empty worktree path
		WindowName:   "orphan-window",
		Task:         "orphan task",
		CreatedAt:    time.Now(),
	}

	d.recordTaskHistory("test-repo", "orphan-worker", agent)

	history, err := d.state.GetTaskHistory("test-repo", 10)
	if err != nil {
		t.Fatalf("GetTaskHistory() failed: %v", err)
	}

	if len(history) == 0 {
		t.Fatal("Expected task history entry")
	}

	entry := history[0]
	// With empty worktree path, branch should be empty
	if entry.Branch != "" {
		t.Errorf("Entry branch = %q, want empty for no worktree", entry.Branch)
	}
}

// Tests for linkGlobalCredentials

// Edge case tests for isLogFile

func TestIsLogFileEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "just filename .log with 4 chars",
			path: ".log",
			want: false, // Too short (len <= 4)
		},
		{
			name: "exactly 5 chars ending in .log",
			path: "x.log",
			want: true,
		},
		{
			name: "path with .log in middle",
			path: "/path.log/file.txt",
			want: false, // Base name is "file.txt"
		},
		{
			name: "uppercase LOG extension",
			path: "/path/to/file.LOG",
			want: false, // Case sensitive
		},
		{
			name: "mixed case extension",
			path: "/path/to/file.Log",
			want: false,
		},
		{
			name: "double extension .log.log",
			path: "/path/to/file.log.log",
			want: true, // Ends in .log
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLogFile(tt.path)
			if got != tt.want {
				t.Errorf("isLogFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
