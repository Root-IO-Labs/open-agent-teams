package backend

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// TestSubscribeEvents_EndToEndThroughBridge verifies the full fan-out
// from the sidecar socket, through the bridge, into the event
// broadcaster, out to a SubscribeEvents consumer. It exercises every
// layer introduced in Day 3b and Day 4a at once.
//
// Wire layout:
//
//	fake client → Unix socket → sidecar.Server → bridge OnEvent →
//	  {log file for token_usage, eventBroadcaster for all kinds} →
//	  SubscribeEvents consumer
func TestSubscribeEvents_EndToEndThroughBridge(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sub-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	socketPath := filepath.Join(dir, "s")
	logPath := filepath.Join(dir, "agent.log")
	bc := newEventBroadcaster()
	defer bc.Close()

	srv := newSidecarServerForAgent(socketPath, logPath, bc)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Subscribe before anyone publishes.
	_, ch, catchup, cancel := bc.Subscribe()
	defer cancel()
	if len(catchup) != 0 {
		t.Fatalf("expected empty catchup, got %d", len(catchup))
	}

	// Act like Python: open the socket, emit a token_usage and an
	// assistant_message. Both must reach the broadcaster; only the
	// token_usage must also reach the log.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	events := []sidecar.Event{
		{
			V: 1, Seq: 1, TS: 1, Kind: sidecar.KindTokenUsage, TurnID: "t",
			Data: json.RawMessage(`{"delta_input":10,"delta_output":5,` +
				`"cumulative_input":100,"cumulative_output":50}`),
		},
		{
			V: 1, Seq: 2, TS: 1, Kind: sidecar.KindAssistantMessage, TurnID: "t",
			Data: json.RawMessage(`{"content":"hello"}`),
		},
	}
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		conn.Write(append(raw, '\n'))
	}

	// Collect on the subscriber channel.
	var got []sidecar.Event
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timeout at %d events", len(got))
		}
	}

	// Order + kinds preserved.
	if got[0].Kind != sidecar.KindTokenUsage {
		t.Errorf("[0].Kind = %q, want token_usage", got[0].Kind)
	}
	if got[1].Kind != sidecar.KindAssistantMessage {
		t.Errorf("[1].Kind = %q, want assistant_message", got[1].Kind)
	}

	// Log file should contain the [OAT_TOKENS] line for token_usage
	// but NOT the assistant_message payload (bridge gates by kind).
	deadline = time.After(1 * time.Second)
	for time.Now().Before(time.Now().Add(-time.Second)) || time.Now().Sub(time.Now()) < time.Second {
		// loop guard replaced below
		break
	}
	// Wait for the bridge's log-file write to land.
	var logContent []byte
	for i := 0; i < 100; i++ {
		logContent, _ = os.ReadFile(logPath)
		if len(logContent) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := string(logContent)
	if !strings.Contains(s, "[OAT_TOKENS]") {
		t.Errorf("log missing [OAT_TOKENS]:\n%s", s)
	}
	if strings.Contains(s, "assistant_message") || strings.Contains(s, `"content":"hello"`) {
		t.Errorf("log leaked assistant_message content:\n%s", s)
	}
	_ = deadline
}

// TestSubscribeEvents_UnknownAgentErrors ensures the public API
// surface returns a clear error rather than panicking when the
// caller asks for an agent that doesn't exist.
func TestSubscribeEvents_UnknownAgentErrors(t *testing.T) {
	b := NewDirectBackend()
	_, _, _, _, err := b.SubscribeEvents(context.Background(), "no-such-session", "no-such-agent")
	if err == nil {
		t.Fatal("expected error for unknown session/agent")
	}
}

// TestSubscribeEvents_BackendImplementsInterface is a compile-time
// assertion: DirectBackend must be assignable to SidecarSubscriber.
// If a future change accidentally breaks the method signature, this
// test refuses to compile, flagging the regression at build time
// rather than via a runtime type-assertion failure in the daemon.
func TestSubscribeEvents_BackendImplementsInterface(t *testing.T) {
	var _ SidecarSubscriber = (*DirectBackend)(nil)
}
