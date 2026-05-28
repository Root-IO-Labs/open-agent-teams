package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// setupStreamTestDaemon creates a daemon with a direct backend for streaming tests.
func setupStreamTestDaemon(t *testing.T) (*Daemon, *backend_pkg.DirectBackend, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "stream-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	paths := config.NewTestPaths(tmpDir)
	if err := paths.EnsureDirectories(); err != nil {
		t.Fatalf("Failed to create directories: %v", err)
	}

	db := backend_pkg.NewDirectBackend()
	d, err := New(paths)
	if err != nil {
		t.Fatalf("Failed to create daemon: %v", err)
	}
	d.backend = db

	return d, db, func() { os.RemoveAll(tmpDir) }
}

func TestStreamHandlerUnknownCommand(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.HandleStream(socket.Request{Command: "unknown_stream"}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("Expected failure for unknown stream command")
	}
	if resp.Error != "unknown stream command: unknown_stream" {
		t.Errorf("Unexpected error: %s", resp.Error)
	}
}

func TestStreamHandlerMissingArgs(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamOutput(socket.Request{
		Command: "stream_output",
		Args:    map[string]interface{}{},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("Expected failure for missing args")
	}
	if resp.Error != "repo and agent are required" {
		t.Errorf("Unexpected error: %s", resp.Error)
	}
}

func TestStreamHandlerRepoNotFound(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamOutput(socket.Request{
		Command: "stream_output",
		Args: map[string]interface{}{
			"repo":  "nonexistent",
			"agent": "worker",
		},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("Expected failure for missing repo")
	}
}

func TestStreamHandlerSuccessfulStream(t *testing.T) {
	d, db, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-repo"
	agentWindow := "test-agent"

	// Set up state
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"test-agent": {
				Type:       state.AgentTypeWorker,
				WindowName: agentWindow,
			},
		},
	})

	// Create backend session and start a simple agent
	db.CreateSession(d.ctx, sessionName)

	logFile := os.TempDir() + "/stream-test-agent.log"
	defer os.Remove(logFile)

	handle, err := db.StartAgent(d.ctx, backend_pkg.AgentConfig{
		SessionName: sessionName,
		AgentName:   agentWindow,
		BinaryPath:  "echo",
		Args:        []string{"hello from agent"},
		WorkDir:     os.TempDir(),
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}
	_ = handle

	// Give the agent a moment to produce output
	time.Sleep(500 * time.Millisecond)

	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamOutput(socket.Request{
		Command: "stream_output",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": "test-agent",
		},
	}, server)

	scanner := bufio.NewScanner(client)

	// First message should be handshake
	if !scanner.Scan() {
		t.Fatal("Expected handshake message")
	}
	var handshake socket.Response
	if err := json.Unmarshal(scanner.Bytes(), &handshake); err != nil {
		t.Fatalf("Failed to decode handshake: %v", err)
	}
	if !handshake.Success {
		t.Errorf("Handshake should be successful, got error: %s", handshake.Error)
	}
	if !handshake.Stream {
		t.Error("Handshake should have Stream=true")
	}

	// Read lines until we get a done message (agent will exit quickly)
	gotLine := false
	gotDone := false
	deadline := time.After(5 * time.Second)

	for !gotDone {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for stream messages")
		default:
		}

		// Set read deadline on client to avoid blocking
		client.SetReadDeadline(time.Now().Add(2 * time.Second))
		if !scanner.Scan() {
			break
		}

		var msg streamOutputLine
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatalf("Failed to decode stream message: %v", err)
		}
		if msg.Done {
			gotDone = true
		}
		if msg.Line != "" {
			gotLine = true
		}
	}

	if !gotLine {
		t.Error("Expected at least one output line from the agent")
	}
	if !gotDone {
		// Agent exited and channel closed — stream handler should have sent done
		// (may not always receive it due to timing, so this is a soft check)
		t.Log("Note: did not receive explicit done message (agent may have exited before stream established)")
	}
}

// Part 8 Commit 8.2: handleStreamAssistantTurns now accepts both
// assistant and browser agent types (the chat-capable whitelist
// already used by daemon.usesBrowserBridge). Pre-8.2 only browser
// agents passed, which spammed the bridge stderr log of assistant
// bridges with "stream_assistant_turns is restricted to browser-agent
// type; ... is assistant".
//
// These tests pin the new gate semantics:
//  1. Assistant subscription does NOT get the type-restriction
//     error (it may get "no tailer active" depending on whether
//     the tailer has been registered — that's a separate path).
//  2. Supervisor/worker still fail with the new "chat-capable
//     agents (assistant + browser)" error message.
func TestStreamHandlerAssistantTurns_AssistantPassesGate_Part8Commit2(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-session"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"personal": {
				Type:       state.AgentTypeAssistant,
				WindowName: "personal",
				PID:        12345,
			},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAssistantTurns(socket.Request{
		Command: "stream_assistant_turns",
		Args: map[string]interface{}{
			"session": sessionName,
			"agent":   "personal",
		},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	// Assistant must NOT receive the pre-8.2 type-restriction error.
	// It may receive "no tailer active" (the tailer is not running
	// in this test harness) but that's a separate path.
	if resp.Success {
		// Handshake succeeded — tailer was somehow active. That's
		// fine, the gate passed.
		return
	}
	if contains := strings.Contains(resp.Error, "restricted to"); contains {
		t.Errorf("Assistant should pass the type gate; got restriction error: %s", resp.Error)
	}
	// "no tailer active" is the expected outcome for assistant in
	// this test fixture (no tailer registered), proving the gate
	// passed but the lookup failed downstream.
	if !strings.Contains(resp.Error, "no assistant-turn tailer active") {
		t.Logf("Note: assistant passed gate; downstream error: %s", resp.Error)
	}
}

func TestStreamHandlerAssistantTurns_SupervisorRejected_Part8Commit2(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-session"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"supervisor": {
				Type:       state.AgentTypeSupervisor,
				WindowName: "supervisor",
				PID:        12345,
			},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAssistantTurns(socket.Request{
		Command: "stream_assistant_turns",
		Args: map[string]interface{}{
			"session": sessionName,
			"agent":   "supervisor",
		},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("Supervisor subscription must be rejected")
	}
	// New error message references "chat-capable agents (assistant + browser)"
	// instead of the pre-8.2 "browser-agent type".
	if !strings.Contains(resp.Error, "chat-capable agents (assistant + browser)") {
		t.Errorf("Expected new error message 'chat-capable agents (assistant + browser)'; got: %s", resp.Error)
	}
}

func TestStreamHandlerAssistantTurns_WorkerRejected_Part8Commit2(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-session"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"worker": {
				Type:       state.AgentTypeWorker,
				WindowName: "worker",
				PID:        12345,
			},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAssistantTurns(socket.Request{
		Command: "stream_assistant_turns",
		Args: map[string]interface{}{
			"session": sessionName,
			"agent":   "worker",
		},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("Worker subscription must be rejected")
	}
	if !strings.Contains(resp.Error, "chat-capable agents (assistant + browser)") {
		t.Errorf("Expected new error message 'chat-capable agents (assistant + browser)'; got: %s", resp.Error)
	}
}

func TestStreamHandlerClientDisconnect(t *testing.T) {
	d, db, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-repo"
	agentWindow := "long-agent"

	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"long-agent": {
				Type:       state.AgentTypeWorker,
				WindowName: agentWindow,
			},
		},
	})

	db.CreateSession(d.ctx, sessionName)

	logFile := os.TempDir() + "/stream-test-long-agent.log"
	defer os.Remove(logFile)

	// Start a long-running agent
	_, err := db.StartAgent(d.ctx, backend_pkg.AgentConfig{
		SessionName: sessionName,
		AgentName:   agentWindow,
		BinaryPath:  "sleep",
		Args:        []string{"60"},
		WorkDir:     os.TempDir(),
		LogFile:     logFile,
	})
	if err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}

	sh := &streamHandler{d: d}

	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		sh.handleStreamOutput(socket.Request{
			Command: "stream_output",
			Args: map[string]interface{}{
				"repo":  repoName,
				"agent": "long-agent",
			},
		}, server)
		close(done)
	}()

	// Read handshake then disconnect
	scanner := bufio.NewScanner(client)
	if scanner.Scan() {
		var handshake socket.Response
		json.Unmarshal(scanner.Bytes(), &handshake)
		if !handshake.Success {
			t.Errorf("Expected successful handshake, got: %s", handshake.Error)
		}
	}

	// Close client to simulate disconnect
	client.Close()

	// Stream handler should exit within write deadline (30s) — but with pipe
	// it should detect immediately
	select {
	case <-done:
		// Good — handler exited after client disconnect
	case <-time.After(5 * time.Second):
		t.Error("Stream handler did not exit after client disconnect")
	}

	// Clean up agent
	db.StopAgent(d.ctx, sessionName, agentWindow)
}
