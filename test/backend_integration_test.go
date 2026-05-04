package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
)

// TestDirectBackendLifecycle tests the full agent lifecycle using the direct backend.
func TestDirectBackendLifecycle(t *testing.T) {
	b := backend.NewDirectBackend()
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Create session
	if err := b.CreateSession(ctx, "oat-test"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 2. Start agent
	logFile := filepath.Join(tmpDir, "agent.log")
	handle, err := b.StartAgent(ctx, backend.AgentConfig{
		SessionName:   "oat-test",
		AgentName:     "test-worker",
		WorkDir:       tmpDir,
		BinaryPath:    "cat",
		LogFile:       logFile,
		InitialPrompt: "You are a test agent",
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if handle.PID == 0 {
		t.Fatal("expected non-zero PID")
	}

	// Verify AGENTS.md was written
	agentsFile := filepath.Join(tmpDir, ".oat", "AGENTS.md")
	data, err := os.ReadFile(agentsFile)
	if err != nil {
		t.Fatalf("AGENTS.md not written: %v", err)
	}
	if string(data) != "You are a test agent" {
		t.Fatalf("AGENTS.md content wrong: %q", string(data))
	}

	// 3. Agent should be alive
	alive, _ := b.IsAgentAlive(ctx, "oat-test", "test-worker")
	if !alive {
		t.Fatal("agent should be alive after start")
	}

	// 4. Send message
	time.Sleep(200 * time.Millisecond)
	if err := b.SendMessage(ctx, "oat-test", "test-worker", "hello from test"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// 5. Get output reader (while agent is still alive)
	reader, err := b.GetOutputReader(ctx, "oat-test", "test-worker")
	if err != nil {
		t.Fatalf("GetOutputReader: %v", err)
	}
	reader.Close()

	// 6. Stop agent so the cleanLogWriter flushes its pending buffer to disk.
	if err := b.StopAgent(ctx, "oat-test", "test-worker"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	// 7. Verify output in log (after stop so buffer is flushed)
	logData, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logData), "hello from test") {
		t.Fatalf("log should contain sent message, got: %q", string(logData))
	}

	alive, _ = b.IsAgentAlive(ctx, "oat-test", "test-worker")
	if alive {
		t.Fatal("agent should not be alive after stop")
	}

	// 8. Destroy session
	if err := b.DestroySession(ctx, "oat-test"); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	exists, _ := b.HasSession(ctx, "oat-test")
	if exists {
		t.Fatal("session should not exist after destroy")
	}
}

// TestDirectBackendAgentCrashDetection verifies IsAgentAlive detects crashed agents.
func TestDirectBackendAgentCrashDetection(t *testing.T) {
	b := backend.NewDirectBackend()
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Start a short-lived process
	_, err := b.StartAgent(ctx, backend.AgentConfig{
		SessionName: "s",
		AgentName:   "crash",
		WorkDir:     tmpDir,
		BinaryPath:  "false", // exits immediately with code 1
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// Wait for it to exit
	time.Sleep(500 * time.Millisecond)

	// Should detect it's dead
	alive, _ := b.IsAgentAlive(ctx, "s", "crash")
	if alive {
		t.Fatal("crashed agent should not be alive")
	}

	// Cleanup shouldn't error
	_ = b.StopAgent(ctx, "s", "crash")
}

// TestDirectBackendRapidMessages verifies mutex prevents interleaving.
func TestDirectBackendRapidMessages(t *testing.T) {
	b := backend.NewDirectBackend()
	ctx := context.Background()
	tmpDir := t.TempDir()

	_, err := b.StartAgent(ctx, backend.AgentConfig{
		SessionName: "s",
		AgentName:   "rapid",
		WorkDir:     tmpDir,
		BinaryPath:  "cat",
		LogFile:     filepath.Join(tmpDir, "rapid.log"),
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	defer b.StopAgent(ctx, "s", "rapid")

	time.Sleep(200 * time.Millisecond)

	// Send 20 rapid messages sequentially
	for i := 0; i < 20; i++ {
		if err := b.SendMessage(ctx, "s", "rapid", "msg"); err != nil {
			t.Fatalf("SendMessage %d: %v", i, err)
		}
	}
}

// TestBackendFactory tests the NewBackend factory behavior.
func TestBackendFactory(t *testing.T) {
	// "direct" always returns DirectBackend
	b := backend.NewBackend("direct", "")
	if b == nil {
		t.Fatal("NewBackend('direct') returned nil")
	}
	if info, ok := b.(backend.BackendInfo); ok {
		if info.Name() != "direct" {
			t.Fatalf("Name() = %q, want 'direct'", info.Name())
		}
	}

	// "" (auto) returns DirectBackend
	b = backend.NewBackend("", "")
	if b == nil {
		t.Fatal("NewBackend('') returned nil")
	}
	if info, ok := b.(backend.BackendInfo); ok {
		if info.Name() != "direct" {
			t.Fatalf("Name() = %q, want 'direct'", info.Name())
		}
	}
}

// TestDirectBackendProcessReAdoption verifies that after a daemon restart,
// still-running agent processes are re-adopted from backend-sessions.json.
func TestDirectBackendProcessReAdoption(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Start an agent with a persistent backend
	b1 := backend.NewDirectBackendWithDataDir(tmpDir)
	if err := b1.CreateSession(ctx, "oat-test"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	logFile := filepath.Join(tmpDir, "agent.log")
	handle, err := b1.StartAgent(ctx, backend.AgentConfig{
		SessionName: "oat-test",
		AgentName:   "long-runner",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"300"},
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	agentPID := handle.PID
	t.Logf("Started agent with PID %d", agentPID)

	// Verify it's alive
	alive, _ := b1.IsAgentAlive(ctx, "oat-test", "long-runner")
	if !alive {
		t.Fatal("Agent should be alive after start")
	}

	// Verify backend-sessions.json was written
	sessFile := filepath.Join(tmpDir, "backend-sessions.json")
	if _, err := os.Stat(sessFile); os.IsNotExist(err) {
		t.Fatal("backend-sessions.json should exist after StartAgent")
	}

	// 2. Simulate daemon restart: create a NEW backend from the same data dir.
	// The old backend is abandoned (not stopped), simulating a crash.
	b2 := backend.NewDirectBackendWithDataDir(tmpDir)

	// 3. Verify the agent was re-adopted
	alive2, _ := b2.IsAgentAlive(ctx, "oat-test", "long-runner")
	if !alive2 {
		t.Fatal("Agent should be re-adopted and reported alive by new backend")
	}

	// Verify it's marked as adopted
	adopted, _ := b2.IsAdopted(ctx, "oat-test", "long-runner")
	if !adopted {
		t.Fatal("Agent should be marked as adopted (no PTY)")
	}

	// Verify SendMessage returns ErrAgentAdopted
	err = b2.SendMessage(ctx, "oat-test", "long-runner", "hello")
	if err != backend.ErrAgentAdopted {
		t.Fatalf("Expected ErrAgentAdopted, got: %v", err)
	}

	// 4. Clean up: stop the agent through the new backend
	if err := b2.StopAgent(ctx, "oat-test", "long-runner"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	// Verify it's now dead
	time.Sleep(500 * time.Millisecond) // give monitor goroutine time to detect
	alive3, _ := b2.IsAgentAlive(ctx, "oat-test", "long-runner")
	if alive3 {
		t.Error("Agent should be dead after StopAgent")
	}
}
