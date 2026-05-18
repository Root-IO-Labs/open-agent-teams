package daemon

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
)

// Pre-backend validation gates for stream_agent_output. The verb runs four
// validations before subscribing: session+agent presence, session→repo
// mapping, agent existence within that repo, and the AgentTypeBrowser
// security boundary. Failing any returns a Response with Success=false on
// the wire (no Stream handshake). Tests exercise each in isolation.

// readFirstFrame reads one JSON line from the connection and decodes it
// into a Response. Useful for asserting the pre-handshake failure path
// where the server writes a single frame and closes.
func readFirstFrame(t *testing.T, conn net.Conn) socket.Response {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected one frame, got EOF/err: %v", scanner.Err())
	}
	var resp socket.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("decode frame: %v (raw: %q)", err, scanner.Bytes())
	}
	return resp
}

func TestStreamAgentOutput_MissingArgs(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"empty", map[string]interface{}{}, "session and agent are required"},
		{"only_session", map[string]interface{}{"session": "s"}, "session and agent are required"},
		{"only_agent", map[string]interface{}{"agent": "a"}, "session and agent are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := &streamHandler{d: d}
			server, client := net.Pipe()
			defer client.Close()
			go sh.handleStreamAgentOutput(socket.Request{
				Command: "stream_agent_output",
				Args:    tc.args,
			}, server)

			resp := readFirstFrame(t, client)
			if resp.Success {
				t.Fatalf("expected failure, got success: %+v", resp)
			}
			if resp.Error != tc.want {
				t.Errorf("error = %q, want %q", resp.Error, tc.want)
			}
		})
	}
}

func TestStreamAgentOutput_SessionNotFound(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()
	go sh.handleStreamAgentOutput(socket.Request{
		Command: "stream_agent_output",
		Args:    map[string]interface{}{"session": "ghost", "agent": "browser-agent"},
	}, server)

	resp := readFirstFrame(t, client)
	if resp.Success {
		t.Fatalf("expected failure for unknown session, got success")
	}
	if !strings.Contains(resp.Error, "no repository is bound to session") {
		t.Errorf("error = %q, want substring 'no repository is bound to session'", resp.Error)
	}
}

func TestStreamAgentOutput_AgentNotFound(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	d.state.AddRepo("my-repo", &state.Repository{SessionName: "my-session"}) //nolint:errcheck

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()
	go sh.handleStreamAgentOutput(socket.Request{
		Command: "stream_agent_output",
		Args:    map[string]interface{}{"session": "my-session", "agent": "nope"},
	}, server)

	resp := readFirstFrame(t, client)
	if resp.Success {
		t.Fatalf("expected failure for unknown agent, got success")
	}
	if !strings.Contains(resp.Error, "agent 'nope' not found") {
		t.Errorf("error = %q, want substring", resp.Error)
	}
}

// SECURITY: stream_agent_output mirrors the AgentTypeBrowser restriction
// from agent_input (Part 2b). A non-browser agent — even one that exists
// in state with a valid session — must be rejected before the subscription
// is opened. Iterating every other AgentType keeps the boundary explicit
// if a new type is ever added.
func TestStreamAgentOutput_RestrictedToBrowserType(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()
	d.state.AddRepo("my-repo", &state.Repository{SessionName: "my-session"}) //nolint:errcheck

	otherTypes := []state.AgentType{
		state.AgentTypeWorker,
		state.AgentTypeSupervisor,
		state.AgentTypeMergeQueue,
		state.AgentTypeReview,
		state.AgentTypeVerification,
		state.AgentTypePRShepherd,
		state.AgentTypeWorkspace,
		state.AgentTypeAgentBuilder,
		state.AgentTypeGenericPersistent,
	}
	for _, agentType := range otherTypes {
		t.Run(string(agentType), func(t *testing.T) {
			name := "victim-" + string(agentType)
			if err := d.state.AddAgent("my-repo", name, state.Agent{
				Type:       agentType,
				WindowName: name,
			}); err != nil {
				t.Fatalf("AddAgent: %v", err)
			}
			sh := &streamHandler{d: d}
			server, client := net.Pipe()
			defer client.Close()
			go sh.handleStreamAgentOutput(socket.Request{
				Command: "stream_agent_output",
				Args:    map[string]interface{}{"session": "my-session", "agent": name},
			}, server)

			resp := readFirstFrame(t, client)
			if resp.Success {
				t.Fatalf("stream_agent_output must NOT accept non-browser agent type %s", agentType)
			}
			if !strings.Contains(resp.Error, "restricted to browser-agent type") {
				t.Errorf("error = %q, want substring 'restricted to browser-agent type'", resp.Error)
			}
		})
	}
}

// End-to-end happy path: a real PTY agent (we use `echo` like the existing
// stream_output test) writes bytes; the handler streams them as base64
// chunk frames batched at 16 ms minimum, then a Done frame on agent exit.
// We don't assert exact framing because batching is timing-dependent, but
// the concatenated decoded chunk bytes must contain the agent's stdout.
func TestStreamAgentOutput_SuccessfulStream(t *testing.T) {
	d, db, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "test-repo"
	sessionName := "oat-test-repo"
	agentName := "test-browser"
	windowName := agentName // browser agents use the same name in state and backend

	if err := d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			agentName: {
				Type:       state.AgentTypeBrowser,
				WindowName: windowName,
			},
		},
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	db.CreateSession(d.ctx, sessionName) //nolint:errcheck
	logFile := os.TempDir() + "/stream-agent-output-test.log"
	defer os.Remove(logFile)

	if _, err := db.StartAgent(d.ctx, backend_pkg.AgentConfig{
		SessionName: sessionName,
		AgentName:   windowName,
		BinaryPath:  "echo",
		Args:        []string{"hello from chunk stream"},
		WorkDir:     os.TempDir(),
		LogFile:     logFile,
	}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAgentOutput(socket.Request{
		Command: "stream_agent_output",
		Args:    map[string]interface{}{"session": sessionName, "agent": agentName},
	}, server)

	scanner := bufio.NewScanner(client)
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)

	// First line: handshake.
	client.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if !scanner.Scan() {
		t.Fatalf("expected handshake, got err: %v", scanner.Err())
	}
	var handshake socket.Response
	if err := json.Unmarshal(scanner.Bytes(), &handshake); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if !handshake.Success || !handshake.Stream {
		t.Fatalf("handshake = %+v, want Success && Stream", handshake)
	}

	// Then chunk frames + Done. Concatenate every chunk's decoded payload
	// and assert the agent's stdout appears somewhere in it.
	var allBytes []byte
	gotDone := false
	deadline := time.After(5 * time.Second)
	for !gotDone {
		select {
		case <-deadline:
			t.Fatalf("timed out before Done; collected so far: %q", string(allBytes))
		default:
		}
		client.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
		if !scanner.Scan() {
			break
		}
		var frame streamAgentChunkFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("decode frame: %v (raw=%q)", err, scanner.Bytes())
		}
		if frame.Done {
			gotDone = true
			break
		}
		if frame.Err != "" {
			t.Fatalf("got error frame: %s", frame.Err)
		}
		if frame.Chunk != "" {
			decoded, err := base64.StdEncoding.DecodeString(frame.Chunk)
			if err != nil {
				t.Fatalf("base64 decode: %v", err)
			}
			allBytes = append(allBytes, decoded...)
		}
		// Gap frames are valid but shouldn't appear here under a normal
		// load — we don't assert anything about them.
	}

	if !strings.Contains(string(allBytes), "hello from chunk stream") {
		t.Errorf("decoded chunks did not contain agent stdout; got %q", string(allBytes))
	}
	if !gotDone {
		t.Log("note: Done frame not observed; agent may have exited before broadcaster closed")
	}
}

// The 16 ms batching threshold is the plan-mandated WS rate limit. We test
// it indirectly: write a burst of small chunks within one batch window and
// assert they coalesce into a small number of frames (much smaller than the
// number of chunks written). Exact frame count varies with scheduling, but
// the upper bound is "one frame per 16 ms", so 100 ms of input must
// produce at most ~7 frames even if 50 chunks were written.
func TestStreamAgentOutput_BatchesChunksWithin16ms(t *testing.T) {
	d, db, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "burst-repo"
	sessionName := "oat-burst-repo"
	agentName := "burst-browser"

	if err := d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			agentName: {Type: state.AgentTypeBrowser, WindowName: agentName},
		},
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	db.CreateSession(d.ctx, sessionName) //nolint:errcheck
	logFile := os.TempDir() + "/stream-agent-output-burst.log"
	defer os.Remove(logFile)

	// `yes` produces a continuous high-rate stream of "y\n" — many tiny
	// chunks per second from the PTY reader's perspective.
	if _, err := db.StartAgent(d.ctx, backend_pkg.AgentConfig{
		SessionName: sessionName,
		AgentName:   agentName,
		BinaryPath:  "yes",
		WorkDir:     os.TempDir(),
		LogFile:     logFile,
	}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	defer db.StopAgent(d.ctx, sessionName, agentName) //nolint:errcheck

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamAgentOutput(socket.Request{
		Command: "stream_agent_output",
		Args:    map[string]interface{}{"session": sessionName, "agent": agentName},
	}, server)

	scanner := bufio.NewScanner(client)
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)

	// Drain the handshake.
	client.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if !scanner.Scan() {
		t.Fatalf("expected handshake: %v", scanner.Err())
	}

	// Read frames for a 200 ms window. Upper bound at 16 ms/frame ≈ 13
	// frames; we give plenty of slack (cap 50) since the test
	// scheduler might be jittery.
	stop := time.After(200 * time.Millisecond)
	frameCount := 0
	chunkBytes := 0
loop:
	for {
		select {
		case <-stop:
			break loop
		default:
		}
		client.SetReadDeadline(time.Now().Add(150 * time.Millisecond)) //nolint:errcheck
		if !scanner.Scan() {
			break
		}
		frameCount++
		var frame streamAgentChunkFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err == nil && frame.Chunk != "" {
			decoded, _ := base64.StdEncoding.DecodeString(frame.Chunk)
			chunkBytes += len(decoded)
		}
	}
	if frameCount == 0 {
		t.Fatalf("got 0 frames in 200 ms — handler isn't streaming")
	}
	if frameCount > 50 {
		t.Errorf("got %d frames in 200 ms (cap 50) — batching is broken; chunkBytes=%d", frameCount, chunkBytes)
	}
	if chunkBytes < 10 {
		t.Errorf("got %d bytes of chunk payload in 200 ms — `yes` should have produced thousands", chunkBytes)
	}
}

// Adopted processes have no chunk broadcaster. Subscribing must fail with
// a clean error frame rather than hanging or panicking. The fail is from
// SubscribeRawOutput inside the handler — we just verify the error
// surfaces to the wire properly.
func TestStreamAgentOutput_AgentWithoutBroadcaster(t *testing.T) {
	d, db, cleanup := setupStreamTestDaemon(t)
	defer cleanup()

	repoName := "adopted-repo"
	sessionName := "oat-adopted-repo"
	agentName := "adopted-browser"

	if err := d.state.AddRepo(repoName, &state.Repository{
		SessionName: sessionName,
		Agents: map[string]state.Agent{
			agentName: {Type: state.AgentTypeBrowser, WindowName: agentName},
		},
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	db.CreateSession(d.ctx, sessionName) //nolint:errcheck
	// Note: we deliberately do NOT call StartAgent. The backend's session
	// map has no entry for this agent name, so SubscribeRawOutput returns
	// "agent not found in session" — which is the same code path adopted
	// agents (without a broadcaster) take, since by the time the handler
	// asks for it the proc map either lacks the key or the broadcaster
	// is nil.

	sh := &streamHandler{d: d}
	server, client := net.Pipe()
	defer client.Close()
	go sh.handleStreamAgentOutput(socket.Request{
		Command: "stream_agent_output",
		Args:    map[string]interface{}{"session": sessionName, "agent": agentName},
	}, server)

	resp := readFirstFrame(t, client)
	if resp.Success {
		t.Fatalf("expected failure when agent has no live PTY, got success")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("error = %q, want substring 'not found'", resp.Error)
	}
}
