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

// handleStreamAssistantTurns accepts both assistant and browser
// agent types (the chat-capable whitelist already used by
// daemon.usesBrowserBridge). An earlier gate only allowed browser
// agents and spammed the bridge stderr log of assistant bridges
// with "stream_assistant_turns is restricted to browser-agent
// type; ... is assistant".
//
// These tests pin the current gate semantics:
//  1. Assistant subscription does NOT get the type-restriction
//     error (it may get "no tailer active" depending on whether
//     the tailer has been registered — that's a separate path).
//  2. Supervisor/worker still fail with the "chat-capable agents
//     (assistant + browser)" error message.
func TestStreamHandlerAssistantTurns_AssistantPassesGate(t *testing.T) {
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
	// Assistant must NOT receive the type-restriction error. It
	// may receive "no tailer active" (the tailer is not running in
	// this test harness) but that's a separate path.
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

// Regression: the AssistantTurnMultiplexer addresses subscriptions by
// `repo` (the canonical OAT key from lifecycle frames) rather than
// `session` (tmux session name; unknown to the multiplexer). The
// handler must accept either; previously it required `session`,
// findRepoBySession returned "not found" for the multiplexer-supplied
// repo value, and every chat-capable subscription failed its handshake
// silently — no agent replies ever reached the side panel.
func TestStreamHandlerAssistantTurns_AcceptsRepoArg(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "_assistant-personal"
	sessionName := "oat-_assistant-personal"
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
			"repo":  repoName,
			"agent": "personal",
		},
	}, server)

	var resp socket.Response
	dec := json.NewDecoder(client)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		// Handshake succeeded — gate passed. Done.
		return
	}
	if strings.Contains(resp.Error, "not found") || strings.Contains(resp.Error, "no repository is bound") {
		t.Fatalf("repo arg should resolve directly without findRepoBySession; got: %s", resp.Error)
	}
	if strings.Contains(resp.Error, "restricted to") {
		t.Fatalf("Assistant should pass the type gate; got restriction error: %s", resp.Error)
	}
	// "no assistant-turn tailer active" is the expected downstream
	// outcome in this fixture — proves the repo arg resolved and the
	// gate passed; only the tailer (out of scope here) was missing.
	if !strings.Contains(resp.Error, "no assistant-turn tailer active") {
		t.Logf("Note: repo arg resolved + gate passed; downstream error: %s", resp.Error)
	}
}

// Regression: when both `repo` and `session` are supplied and they
// disagree (e.g. a multiplexer subscription supplying a target repo
// alongside the bridge's bonded env session), `repo` MUST win. The
// bridge's bonded `OAT_BROWSER_AGENT_SESSION` names one agent; the
// multiplexer subscribes to many, and a buggy fallback that mixed
// the two would resolve the broadcaster to the wrong tailer
// (handshake then fails with "no tailer active for X in session Y"
// even though X is in a DIFFERENT session Z). Defensive on the
// daemon side because the bridge can't always strip the stale arg.
func TestStreamHandlerAssistantTurns_RepoWinsOverConflictingSession(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	// Two repos; the bonded session names repo A, the multiplexer
	// target names repo B (the agent only exists in B).
	d.state.AddRepo("_assistant-personal", &state.Repository{
		SessionName: "oat-_assistant-personal",
		Agents: map[string]state.Agent{
			"personal": {Type: state.AgentTypeAssistant, WindowName: "personal", PID: 1},
		},
	})
	d.state.AddRepo("oat-browser-test", &state.Repository{
		SessionName: "oat-oat-browser-test",
		Agents: map[string]state.Agent{
			"browser-agent": {Type: state.AgentTypeBrowser, WindowName: "browser-agent", PID: 2},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	// Bridge sends both: bonded session=oat-_assistant-personal,
	// multiplexer-supplied repo=oat-browser-test, agent=browser-agent.
	go sh.handleStreamAssistantTurns(socket.Request{
		Command: "stream_assistant_turns",
		Args: map[string]interface{}{
			"session": "oat-_assistant-personal",
			"repo":    "oat-browser-test",
			"agent":   "browser-agent",
		},
	}, server)

	var resp socket.Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must NOT report "agent 'browser-agent' not found in session
	// oat-_assistant-personal" — that's the bug this test guards.
	// Acceptable downstream errors: "no tailer active" (no tailer
	// registered in fixture) or success.
	if strings.Contains(resp.Error, "not found in session oat-_assistant-personal") {
		t.Fatalf("daemon used stale session arg instead of repo-derived session; got: %s", resp.Error)
	}
	if strings.Contains(resp.Error, "restricted to") {
		t.Fatalf("type gate should pass for browser; got: %s", resp.Error)
	}
}

// Regression: missing `agent` is rejected with a clear message even
// when `repo` is supplied (and vice versa). The new acceptance
// criterion is "agent AND (session OR repo)".
func TestStreamHandlerAssistantTurns_RejectsMissingArgs(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"no args", map[string]interface{}{}, "agent and (session or repo) are required"},
		{"only session", map[string]interface{}{"session": "oat-x"}, "agent and (session or repo) are required"},
		{"only repo", map[string]interface{}{"repo": "x"}, "agent and (session or repo) are required"},
		{"only agent", map[string]interface{}{"agent": "a"}, "agent and (session or repo) are required"},
		{"unknown repo", map[string]interface{}{"repo": "no-such-repo", "agent": "a"}, "repository 'no-such-repo' not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := &streamHandler{d: d}
			server, client := net.Pipe()
			defer client.Close()
			go sh.handleStreamAssistantTurns(socket.Request{
				Command: "stream_assistant_turns",
				Args:    tc.args,
			}, server)
			var resp socket.Response
			if err := json.NewDecoder(client).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Success {
				t.Fatalf("expected rejection for %s", tc.name)
			}
			if !strings.Contains(resp.Error, tc.want) {
				t.Errorf("error = %q, want substring %q", resp.Error, tc.want)
			}
		})
	}
}

func TestStreamHandlerAssistantTurns_SupervisorRejected(t *testing.T) {
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
	// Error message references "chat-capable agents (assistant + browser)"
	// (was "browser-agent type" before the gate widened).
	if !strings.Contains(resp.Error, "chat-capable agents (assistant + browser)") {
		t.Errorf("Expected new error message 'chat-capable agents (assistant + browser)'; got: %s", resp.Error)
	}
}

// The tailer broadcaster fans out a fixture turn to a real
// subscriber once the chat-capable gate allows the assistant
// subscription through. Injects a fake tailer with a working
// turnBroadcaster directly into d.assistantTurnTailers, subscribes
// via the socket, publishes a fixture AssistantTurn, and asserts
// the wire frame arrives on the subscriber connection. This proves
// the daemon pipeline end-to-end for the assistant lift, not just
// the gate.
func TestStreamHandlerAssistantTurns_AssistantBroadcasterFanout(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "_assistant-personal"
	sessionName := "oat-_assistant-personal"
	agentName := "personal"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			agentName: {
				Type:       state.AgentTypeAssistant,
				WindowName: agentName,
				PID:        12345,
			},
		},
	})

	// Manually register a tailer + broadcaster for this assistant.
	// The real startAssistantTurnTailer opens an OAT_TOOL_LOG file;
	// we don't need the tailer goroutine running for this test —
	// just a broadcaster wired into the lookup path.
	broadcaster := newTurnBroadcaster(d.logger.Info)
	d.assistantTurnTailersMu.Lock()
	d.assistantTurnTailers[turnKey(sessionName, agentName)] = &assistantTurnTailer{
		broadcaster: broadcaster,
	}
	d.assistantTurnTailersMu.Unlock()
	defer broadcaster.Close()

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAssistantTurns(socket.Request{
		Command: "stream_assistant_turns",
		Args: map[string]interface{}{
			"session": sessionName,
			"agent":   agentName,
		},
	}, server)

	dec := json.NewDecoder(client)

	// Read handshake.
	var handshake socket.Response
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := dec.Decode(&handshake); err != nil {
		t.Fatalf("Failed to decode handshake: %v", err)
	}
	if !handshake.Success {
		t.Fatalf("Assistant handshake should have succeeded; got error: %s", handshake.Error)
	}
	if !handshake.Stream {
		t.Fatal("Handshake should have Stream=true")
	}

	// Publish a fixture turn. The broadcaster's subscriber-buf is
	// 16 so we don't need to race the reader.
	broadcaster.Publish(AssistantTurn{
		SanitizedText: "hello from the fixture tailer",
		Kind:          "final",
	})

	// Read the wire frame.
	var frame assistantTurnFrame
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := dec.Decode(&frame); err != nil {
		t.Fatalf("Failed to decode turn frame: %v", err)
	}
	if frame.Text != "hello from the fixture tailer" {
		t.Errorf("Unexpected frame text: %q", frame.Text)
	}
	if frame.Kind != "final" {
		t.Errorf("Unexpected frame kind: %q", frame.Kind)
	}
}

func TestStreamHandlerAssistantTurns_WorkerRejected(t *testing.T) {
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

// handleStreamContextCapacity gate widened from assistant-only to any
// chat-capable agent (usesBrowserBridge: assistant + browser) so the
// side-panel ring meter can follow whichever agent the chat picker has
// selected. A browser-agent subscription must now handshake OK (it was
// rejected with "restricted to assistant agent type" before).
func TestStreamHandlerContextCapacity_BrowserPassesGate(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "my-repo"
	sessionName := "oat-my-repo"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"browser-agent": {
				Type:       state.AgentTypeBrowser,
				WindowName: "browser-agent",
				PID:        12345,
			},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamContextCapacity(socket.Request{
		Command: "stream_context_capacity",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": "browser-agent",
		},
	}, server)

	dec := json.NewDecoder(client)
	var resp socket.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode handshake: %v", err)
	}
	if !resp.Success {
		t.Fatalf("browser agent should pass the widened gate; got error: %s", resp.Error)
	}
	// Drain the snapshot frame so the handler doesn't block writing it
	// on the synchronous pipe.
	var snap contextCapacityFrame
	if err := dec.Decode(&snap); err != nil {
		t.Fatalf("Failed to decode snapshot frame: %v", err)
	}
}

// Mirror of the turns verb: handleStreamContextCapacity resolves a
// `repo` arg directly (the CapacityMultiplexer addresses subscriptions
// by repo, not the tmux session name).
func TestStreamHandlerContextCapacity_AcceptsRepoArg(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "_assistant-personal"
	sessionName := "oat-_assistant-personal"
	d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			"personal": {
				Type:                state.AgentTypeAssistant,
				WindowName:          "personal",
				PID:                 12345,
				ContextWindowTokens: 64_000,
			},
		},
	})

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamContextCapacity(socket.Request{
		Command: "stream_context_capacity",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": "personal",
		},
	}, server)

	dec := json.NewDecoder(client)
	var resp socket.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode handshake: %v", err)
	}
	if !resp.Success {
		if strings.Contains(resp.Error, "not found") || strings.Contains(resp.Error, "no repository is bound") {
			t.Fatalf("repo arg should resolve directly; got: %s", resp.Error)
		}
		t.Fatalf("assistant via repo arg should handshake OK; got: %s", resp.Error)
	}
	var snap contextCapacityFrame
	if err := dec.Decode(&snap); err != nil {
		t.Fatalf("Failed to decode snapshot frame: %v", err)
	}
}

// Non-chat-capable agents (supervisor/worker/etc.) are still rejected
// by the capacity verb, with the same chat-capable wording the turns
// verb uses.
func TestStreamHandlerContextCapacity_SupervisorRejected(t *testing.T) {
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

	go sh.handleStreamContextCapacity(socket.Request{
		Command: "stream_context_capacity",
		Args: map[string]interface{}{
			"session": sessionName,
			"agent":   "supervisor",
		},
	}, server)

	var resp socket.Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("supervisor capacity subscription must be rejected")
	}
	if !strings.Contains(resp.Error, "chat-capable agents (assistant + browser)") {
		t.Errorf("expected chat-capable rejection wording; got: %s", resp.Error)
	}
}
