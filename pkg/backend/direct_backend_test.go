package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDirectBackend_SessionLifecycle(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	// Create session
	if err := b.CreateSession(ctx, "test-session"); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Verify exists
	exists, err := b.HasSession(ctx, "test-session")
	if err != nil {
		t.Fatalf("HasSession failed: %v", err)
	}
	if !exists {
		t.Fatal("session should exist")
	}

	// List sessions
	sessions, err := b.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0] != "test-session" {
		t.Fatalf("ListSessions = %v, want [test-session]", sessions)
	}

	// Idempotent create
	if err := b.CreateSession(ctx, "test-session"); err != nil {
		t.Fatalf("idempotent CreateSession failed: %v", err)
	}

	// Destroy
	if err := b.DestroySession(ctx, "test-session"); err != nil {
		t.Fatalf("DestroySession failed: %v", err)
	}

	exists, _ = b.HasSession(ctx, "test-session")
	if exists {
		t.Fatal("session should not exist after destroy")
	}

	// Destroy non-existent is not an error
	if err := b.DestroySession(ctx, "no-such-session"); err != nil {
		t.Fatalf("DestroySession of non-existent should not error: %v", err)
	}
}

func TestDirectBackend_StartAndStopAgent(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "agent.log")

	handle, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "test-session",
		AgentName:   "test-agent",
		WorkDir:     tmpDir,
		BinaryPath:  "echo",
		Args:        []string{"hello from agent"},
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}
	if handle.PID == 0 {
		t.Fatal("PID should not be zero")
	}
	if handle.LogFile != logFile {
		t.Fatalf("LogFile = %s, want %s", handle.LogFile, logFile)
	}

	// Session should have been auto-created
	exists, _ := b.HasSession(ctx, "test-session")
	if !exists {
		t.Fatal("session should exist after StartAgent")
	}

	// Wait for the short-lived process to finish writing
	time.Sleep(500 * time.Millisecond)

	// Check log file has output
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello from agent") {
		t.Fatalf("log file should contain agent output, got: %q", string(data))
	}

	// Stop agent
	if err := b.StopAgent(ctx, "test-session", "test-agent"); err != nil {
		t.Fatalf("StopAgent failed: %v", err)
	}

	// Should no longer be alive
	alive, _ := b.IsAgentAlive(ctx, "test-session", "test-agent")
	if alive {
		t.Fatal("agent should not be alive after stop")
	}
}

func TestDirectBackend_IsAgentAlive(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	// Non-existent agent
	alive, err := b.IsAgentAlive(ctx, "no-session", "no-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive {
		t.Fatal("non-existent agent should not be alive")
	}

	// Start a long-running agent
	tmpDir := t.TempDir()
	handle, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	alive, _ = b.IsAgentAlive(ctx, "s", "a")
	if !alive {
		t.Fatal("agent should be alive")
	}

	_ = handle // silence unused
	_ = b.StopAgent(ctx, "s", "a")
}

func TestDirectBackend_SendMessage(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "agent.log")

	// Start cat which will echo what we send
	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "cat",
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send a message
	if err := b.SendMessage(ctx, "s", "a", "hello world"); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	// Stop the agent so the cleanLogWriter flushes its pending buffer to disk.
	// The writer holds the last line until a new unrelated line arrives or the
	// reader goroutine exits (on process termination).
	_ = b.StopAgent(ctx, "s", "a")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("log should contain 'hello world', got: %q", string(data))
	}
}

func TestDirectBackend_SendInterrupt(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()

	// Start a long-running process
	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	// Agent should be alive
	alive, _ := b.IsAgentAlive(ctx, "s", "a")
	if !alive {
		t.Fatal("agent should be alive before interrupt")
	}

	// Send Ctrl-C
	if err := b.SendInterrupt(ctx, "s", "a"); err != nil {
		t.Fatalf("SendInterrupt failed: %v", err)
	}

	// Wait for process to die from the interrupt
	time.Sleep(1 * time.Second)

	// Clean up
	_ = b.StopAgent(ctx, "s", "a")
}

func TestDirectBackend_ConcurrentSendMessage(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()

	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "cat",
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}
	defer b.StopAgent(ctx, "s", "a")

	time.Sleep(200 * time.Millisecond)

	// 10 goroutines sending concurrently — no panics, no data races
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = b.SendMessage(ctx, "s", "a", "msg")
		}(i)
	}
	wg.Wait()
}

func TestDirectBackend_DestroySessionStopsAgents(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()

	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a1",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
	})
	if err != nil {
		t.Fatalf("StartAgent a1 failed: %v", err)
	}

	_, err = b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a2",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
	})
	if err != nil {
		t.Fatalf("StartAgent a2 failed: %v", err)
	}

	// Both should be alive
	alive1, _ := b.IsAgentAlive(ctx, "s", "a1")
	alive2, _ := b.IsAgentAlive(ctx, "s", "a2")
	if !alive1 || !alive2 {
		t.Fatal("both agents should be alive")
	}

	// Destroy session kills both
	if err := b.DestroySession(ctx, "s"); err != nil {
		t.Fatalf("DestroySession failed: %v", err)
	}

	exists, _ := b.HasSession(ctx, "s")
	if exists {
		t.Fatal("session should not exist after destroy")
	}
}

func TestDirectBackend_GetOutputReader(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "agent.log")

	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "echo",
		Args:        []string{"test output"},
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}
	defer b.StopAgent(ctx, "s", "a")

	time.Sleep(500 * time.Millisecond)

	reader, err := b.GetOutputReader(ctx, "s", "a")
	if err != nil {
		t.Fatalf("GetOutputReader failed: %v", err)
	}
	defer reader.Close()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "test output") {
		t.Fatalf("output should contain 'test output', got: %q", string(data))
	}
}

func TestDirectBackend_InitialPrompt(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()

	tmpDir := t.TempDir()

	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName:   "s",
		AgentName:     "a",
		WorkDir:       tmpDir,
		BinaryPath:    "echo",
		Args:          []string{"done"},
		InitialPrompt: "You are a test agent",
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}
	defer b.StopAgent(ctx, "s", "a")

	// Check AGENTS.md was written
	agentsFile := filepath.Join(tmpDir, ".oat", "AGENTS.md")
	data, err := os.ReadFile(agentsFile)
	if err != nil {
		t.Fatalf("failed to read AGENTS.md: %v", err)
	}
	if string(data) != "You are a test agent" {
		t.Fatalf("AGENTS.md = %q, want 'You are a test agent'", string(data))
	}
}

func TestDirectBackend_SessionPersistence(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "agent.log")

	// Create a backend with persistence, start a long-running agent
	b1 := NewDirectBackendWithDataDir(dataDir)

	_, err := b1.StartAgent(ctx, AgentConfig{
		SessionName: "sess",
		AgentName:   "worker",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"300"},
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	// Verify persistence file was written
	persFile := filepath.Join(dataDir, "backend-sessions.json")
	if _, err := os.Stat(persFile); os.IsNotExist(err) {
		t.Fatal("backend-sessions.json should exist after StartAgent")
	}

	// Get the PID for later verification
	proc, _ := b1.getProcess("sess", "worker")
	pid := proc.pid

	// Simulate daemon restart: create a NEW backend from the same dataDir.
	// The old backend's in-memory state is gone, but the agent process lives on.
	b2 := NewDirectBackendWithDataDir(dataDir)

	// The new backend should have re-adopted the session
	exists, err := b2.HasSession(ctx, "sess")
	if err != nil {
		t.Fatalf("HasSession failed: %v", err)
	}
	if !exists {
		t.Fatal("session should be re-adopted after restart")
	}

	// Agent should be seen as alive
	alive, err := b2.IsAgentAlive(ctx, "sess", "worker")
	if err != nil {
		t.Fatalf("IsAgentAlive failed: %v", err)
	}
	if !alive {
		t.Fatal("re-adopted agent should be alive")
	}

	// Agent should be marked as adopted
	adopted, err := b2.IsAdopted(ctx, "sess", "worker")
	if err != nil {
		t.Fatalf("IsAdopted failed: %v", err)
	}
	if !adopted {
		t.Fatal("agent should be marked as adopted")
	}

	// SendMessage should return ErrAgentAdopted
	sendErr := b2.SendMessage(ctx, "sess", "worker", "test")
	if sendErr != ErrAgentAdopted {
		t.Fatalf("SendMessage should return ErrAgentAdopted, got: %v", sendErr)
	}

	// SendInterrupt should work via kill() for adopted processes
	if err := b2.SendInterrupt(ctx, "sess", "worker"); err != nil {
		t.Fatalf("SendInterrupt should work for adopted agents: %v", err)
	}

	// GetOutputReader should still work (reads from log file)
	reader, err := b2.GetOutputReader(ctx, "sess", "worker")
	if err != nil {
		t.Fatalf("GetOutputReader should work for adopted agents: %v", err)
	}
	reader.Close()

	// StopAgent should work for adopted processes
	if err := b2.StopAgent(ctx, "sess", "worker"); err != nil {
		t.Fatalf("StopAgent failed for adopted agent: %v", err)
	}

	// Verify the process was killed
	time.Sleep(100 * time.Millisecond)
	if isAlive := (syscall.Kill(pid, 0) == nil); isAlive {
		t.Fatal("adopted agent should be dead after StopAgent")
	}
}

func TestDirectBackend_PersistenceSkipsDeadProcesses(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	tmpDir := t.TempDir()

	// Start a short-lived agent
	b1 := NewDirectBackendWithDataDir(dataDir)
	_, err := b1.StartAgent(ctx, AgentConfig{
		SessionName: "sess",
		AgentName:   "ephemeral",
		WorkDir:     tmpDir,
		BinaryPath:  "true", // exits immediately
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	// Wait for it to die
	time.Sleep(500 * time.Millisecond)

	// New backend should NOT re-adopt the dead process
	b2 := NewDirectBackendWithDataDir(dataDir)
	exists, _ := b2.HasSession(ctx, "sess")
	if exists {
		// Session might exist but should have no agents
		alive, _ := b2.IsAgentAlive(ctx, "sess", "ephemeral")
		if alive {
			t.Fatal("dead process should not be re-adopted")
		}
	}
}

// TestCleanLogWriter_CarriageReturn verifies that the log writer handles
// \r correctly: \r\n is a normal line ending, bare \r discards the line
// (used by TUI spinners for in-place animation).
func TestCleanLogWriter_CarriageReturn(t *testing.T) {
	var buf strings.Builder
	w := newCleanLogWriter(&buf)

	// Test 1: \r\n is a normal line ending — content preserved
	w.Write([]byte("hello world\r\n"))
	w.Close()
	got := buf.String()
	if !strings.Contains(got, "hello world") {
		t.Errorf("\\r\\n should preserve content, got: %q", got)
	}

	// Test 2: Bare \r followed by new content discards old content
	// Simulates TUI spinner: "frame1\rframe2\rframe3\n"
	buf.Reset()
	w2 := newCleanLogWriter(&buf)
	w2.Write([]byte("frame1\rframe2\rframe3\n"))
	w2.Close()
	got = buf.String()
	if strings.Contains(got, "frame1") {
		t.Errorf("bare \\r should discard overwritten content, got: %q", got)
	}
	if !strings.Contains(got, "frame3") {
		t.Errorf("final frame should be preserved, got: %q", got)
	}

	// Test 3: Spinner countdown noise collapsed
	// "⠋ Thinking...(0s)\r⠙ Thinking...(1s)\r⠹ Thinking...(2s)\n"
	buf.Reset()
	w3 := newCleanLogWriter(&buf)
	w3.Write([]byte("⠋ Thinking...(0s)\r⠙ Thinking...(1s)\r⠹ Thinking...(2s)\n"))
	w3.Close()
	got = buf.String()
	if strings.Contains(got, "(0s)") || strings.Contains(got, "(1s)") {
		t.Errorf("intermediate spinner frames should be discarded, got: %q", got)
	}
	if !strings.Contains(got, "(2s)") {
		t.Errorf("final spinner frame should be preserved, got: %q", got)
	}
}

// syncBuf is a thread-safe string buffer for testing.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCleanLogWriter_PeriodicCommit verifies that pending lines are
// committed within ~5s even when no new line arrives.
func TestCleanLogWriter_PeriodicCommit(t *testing.T) {
	buf := &syncBuf{}
	w := newCleanLogWriter(buf)
	defer w.Close()

	// Write a line that will be buffered as pending
	w.Write([]byte("important status line\n"))

	// Immediately after, pending is NOT committed (no next line arrived)
	got := buf.String()
	if strings.Contains(got, "important status line") {
		t.Fatal("pending should not be committed immediately")
	}

	// After 500ms, should still be pending (timer is 5s)
	time.Sleep(500 * time.Millisecond)
	got = buf.String()
	if strings.Contains(got, "important status line") {
		t.Fatal("pending should not be committed after only 500ms")
	}

	// Wait for the periodic flush (5s + margin)
	time.Sleep(5 * time.Second)

	got = buf.String()
	if !strings.Contains(got, "important status line") {
		t.Errorf("pending should be committed by periodic flush, got: %q", got)
	}
}

// TestCleanLogWriter_StreamingNotPremature verifies that rapid progressive
// writes (< 200ms apart) still collapse correctly — the periodic flush
// should not commit intermediate fragments during fast streaming.
func TestCleanLogWriter_StreamingNotPremature(t *testing.T) {
	var buf strings.Builder
	w := newCleanLogWriter(&buf)

	// Simulate fast token streaming: each write extends the previous.
	// All writes happen within ~50ms — well under the 200ms flush interval.
	w.Write([]byte("The\n"))
	time.Sleep(10 * time.Millisecond)
	w.Write([]byte("The quick\n"))
	time.Sleep(10 * time.Millisecond)
	w.Write([]byte("The quick brown fox\n"))
	time.Sleep(10 * time.Millisecond)

	// Immediately check: intermediate fragments should NOT be in output.
	// Only "The quick brown fox" should eventually appear.
	w.Close()

	got := buf.String()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	// Filter empty lines
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) != 1 {
		t.Errorf("expected 1 line (final extension), got %d: %v", len(nonEmpty), nonEmpty)
	}
	if len(nonEmpty) > 0 && strings.TrimSpace(nonEmpty[0]) != "The quick brown fox" {
		t.Errorf("expected 'The quick brown fox', got %q", nonEmpty[0])
	}
}

// TestCleanLogWriter_CloseStopsGoroutine verifies that Close() is safe to
// call and flushes remaining content.
func TestCleanLogWriter_CloseStopsGoroutine(t *testing.T) {
	var buf strings.Builder
	w := newCleanLogWriter(&buf)

	w.Write([]byte("final line\n"))
	w.Close()

	got := buf.String()
	if !strings.Contains(got, "final line") {
		t.Errorf("Close should flush pending, got: %q", got)
	}
}

// TestCleanLogWriter_StoppingFlag verifies that SendMessage returns an error
// when the agent is being stopped.
func TestDirectBackend_StoppingFlag(t *testing.T) {
	b := NewDirectBackend()
	ctx := context.Background()
	tmpDir := t.TempDir()

	_, err := b.StartAgent(ctx, AgentConfig{
		SessionName: "s",
		AgentName:   "a",
		WorkDir:     tmpDir,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
	})
	if err != nil {
		t.Fatalf("StartAgent failed: %v", err)
	}

	// Get a reference to the process before stopping
	proc, err := b.getProcess("s", "a")
	if err != nil {
		t.Fatalf("getProcess failed: %v", err)
	}

	// Set stopping flag (simulates the beginning of StopAgent)
	proc.stopping.Store(true)

	// SendMessage should fail immediately
	sendErr := b.SendMessage(ctx, "s", "a", "should fail")
	if sendErr == nil {
		t.Fatal("SendMessage should fail when agent is stopping")
	}
	if !strings.Contains(sendErr.Error(), "stopping") {
		t.Errorf("error should mention stopping, got: %v", sendErr)
	}

	// SendInterrupt should also fail when stopping
	intErr := b.SendInterrupt(ctx, "s", "a")
	if intErr == nil {
		t.Fatal("SendInterrupt should fail when agent is stopping")
	}
	if !strings.Contains(intErr.Error(), "stopping") {
		t.Errorf("SendInterrupt error should mention stopping, got: %v", intErr)
	}

	// Clean up
	proc.stopping.Store(false)
	_ = b.StopAgent(ctx, "s", "a")
}
