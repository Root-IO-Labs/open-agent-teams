package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// Tests for the stream_events streaming command. Pattern mirrors
// stream_handler_test.go so reviewers can eyeball the parallels.

func TestStreamEvents_MissingArgs(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()
	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamEvents(socket.Request{
		Command: "stream_events",
		Args:    map[string]interface{}{},
	}, server)

	var resp socket.Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Success {
		t.Error("expected failure for missing args")
	}
	if resp.Error != "repo and agent are required" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
}

func TestStreamEvents_RepoNotFound(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()
	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.handleStreamEvents(socket.Request{
		Command: "stream_events",
		Args: map[string]interface{}{
			"repo":  "no-such-repo",
			"agent": "no-such-agent",
		},
	}, server)

	var resp socket.Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Success {
		t.Error("expected failure for unknown repo")
	}
	if resp.Error != "repo not found" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
}

// TestStreamEvents_RoutesThroughDispatch checks that the HandleStream
// dispatcher routes "stream_events" to handleStreamEvents. Unknown
// command would return a different error message.
func TestStreamEvents_RoutesThroughDispatch(t *testing.T) {
	d, _, cleanup := setupStreamTestDaemon(t)
	defer cleanup()
	sh := &streamHandler{d: d}

	server, client := net.Pipe()
	defer client.Close()

	go sh.HandleStream(socket.Request{
		Command: "stream_events",
		Args:    map[string]interface{}{}, // missing repo/agent → expected error path
	}, server)

	// Read with a timeout so a dispatch bug doesn't hang the test forever.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp socket.Response
	br := bufio.NewReader(client)
	if err := json.NewDecoder(br).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// If dispatch went to the right handler, we get the
	// "repo and agent are required" message. If it fell into the
	// default branch we'd see "unknown stream command: stream_events".
	if resp.Error != "repo and agent are required" {
		t.Errorf("dispatch took the wrong branch; error was: %s", resp.Error)
	}
}
