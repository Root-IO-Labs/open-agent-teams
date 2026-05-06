package tui

import (
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// fakeStreamServer is a minimal stand-in for the daemon that spawns a
// goroutine on each connection and follows the stream_events protocol:
// handshake first, then per-line envelopes.
type fakeStreamServer struct {
	ln net.Listener
	// Called with the server-side connection for each new client. The
	// handler drives the protocol (handshake, writes, close).
	handler func(conn net.Conn)
}

func startFakeServer(t *testing.T, handler func(conn net.Conn)) (*fakeStreamServer, string) {
	t.Helper()
	// /tmp-rooted path for macOS sun_path limit.
	sockPath := tempSockPathTUI(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeStreamServer{ln: ln, handler: handler}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return srv, sockPath
}

func tempSockPathTUI(t *testing.T) string {
	t.Helper()
	// macOS sun_path caps at 104 bytes; t.TempDir() under /var/folders/...
	// routinely exceeds that once the test name is appended. Use a short
	// /tmp-rooted dir instead.
	dir, err := os.MkdirTemp("/tmp", "es-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir + "/s"
}

// TestEventStream_DeliversEventsToConsumer is the happy path: server
// sends the handshake then two event envelopes; EventStream receives
// them in order on the Events() channel.
func TestEventStream_DeliversEventsToConsumer(t *testing.T) {
	srv, sockPath := startFakeServer(t, func(conn net.Conn) {
		defer conn.Close()
		enc := json.NewEncoder(conn)

		_, _ = conn.Read(make([]byte, 1024))

		if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
			return
		}

		// Tiny pause lets the client's StreamConnect consume the
		// handshake and hand off the raw conn to the EventStream scanner
		// before the next bytes arrive. In production the daemon's
		// subscription setup provides this gap naturally; tests need an
		// explicit one or bufio.Reader buffering in StreamConnect
		// swallows subsequent lines.
		time.Sleep(20 * time.Millisecond)

		e1 := sidecar.Event{
			V: 1, Seq: 1, TS: 1, Kind: sidecar.KindAssistantDelta, TurnID: "t",
			Data: json.RawMessage(`{"content":"hello"}`),
		}
		e2 := sidecar.Event{
			V: 1, Seq: 2, TS: 2, Kind: sidecar.KindAssistantMessage, TurnID: "t",
			Data: json.RawMessage(`{"content":"hello world"}`),
		}
		_ = enc.Encode(streamEventLine{Event: &e1})
		_ = enc.Encode(streamEventLine{Event: &e2})
		_ = enc.Encode(streamEventLine{Done: true})
	})
	_ = srv

	client := socket.NewClient(sockPath)
	es := NewEventStream(client, "repo", "agent", false)
	es.Start()
	defer es.Stop()

	var got []sidecar.Event
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case ev, ok := <-es.Events():
			if !ok {
				goto done
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timeout at %d events", len(got))
		}
	}
done:
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Kind != sidecar.KindAssistantDelta || got[1].Kind != sidecar.KindAssistantMessage {
		t.Errorf("kinds = [%q, %q], want [assistant_delta, assistant_message]",
			got[0].Kind, got[1].Kind)
	}
	if es.Received() != 2 {
		t.Errorf("Received() = %d, want 2", es.Received())
	}
}

// TestEventStream_DoneMessageEndsStream verifies that a {"done":true}
// envelope closes the consumer channel. This is how the daemon signals
// "agent exited, no more events coming".
func TestEventStream_DoneMessageEndsStream(t *testing.T) {
	_, sockPath := startFakeServer(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = conn.Read(make([]byte, 1024))
		enc := json.NewEncoder(conn)
		_ = enc.Encode(socket.Response{Success: true, Stream: true})
		_ = enc.Encode(streamEventLine{Done: true})
	})

	client := socket.NewClient(sockPath)
	es := NewEventStream(client, "repo", "agent", false)
	es.Start()
	defer es.Stop()

	// Channel must close without any events.
	select {
	case _, ok := <-es.Events():
		if ok {
			t.Error("received an event when expecting channel close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel never closed on Done message")
	}
	if !es.IsClosed() {
		t.Error("IsClosed() should be true after Done")
	}
}

// TestEventStream_MalformedLineIsSkipped asserts forward-compat: a
// non-JSON line mid-stream does NOT kill the subscription; the next
// valid envelope still lands.
func TestEventStream_MalformedLineIsSkipped(t *testing.T) {
	_, sockPath := startFakeServer(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = conn.Read(make([]byte, 1024))
		enc := json.NewEncoder(conn)
		_ = enc.Encode(socket.Response{Success: true, Stream: true})
		// See TestEventStream_DeliversEventsToConsumer — small pause so
		// StreamConnect's bufio.Reader doesn't eat subsequent bytes.
		time.Sleep(20 * time.Millisecond)

		_, _ = conn.Write([]byte("{ not json\n"))
		ev := sidecar.Event{
			V: 1, Seq: 1, TS: 1, Kind: sidecar.KindTurnStart,
			Data: json.RawMessage(`{"user_input":"x"}`),
		}
		_ = enc.Encode(streamEventLine{Event: &ev})
		_ = enc.Encode(streamEventLine{Done: true})
	})

	client := socket.NewClient(sockPath)
	es := NewEventStream(client, "repo", "agent", false)
	es.Start()
	defer es.Stop()

	select {
	case ev := <-es.Events():
		if ev.Kind != sidecar.KindTurnStart {
			t.Errorf("Kind = %q, want turn_start", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid event after malformed line was not delivered")
	}
}

// TestEventStream_StopClosesChannel ensures a consumer that calls Stop
// sees the channel close promptly (no goroutine leak, no hang).
func TestEventStream_StopClosesChannel(t *testing.T) {
	_, sockPath := startFakeServer(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = conn.Read(make([]byte, 1024))
		enc := json.NewEncoder(conn)
		_ = enc.Encode(socket.Response{Success: true, Stream: true})
		// Keep connection open without sending anything — simulates a
		// silent agent. Stop() must close the consumer side regardless.
		time.Sleep(10 * time.Second)
	})

	client := socket.NewClient(sockPath)
	es := NewEventStream(client, "repo", "agent", false)
	es.Start()

	// Stop immediately; channel should close within a moment.
	es.Stop()

	select {
	case _, ok := <-es.Events():
		if ok {
			t.Error("expected channel close after Stop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel did not close after Stop")
	}
}

// TestEventStream_StopIsIdempotent mirrors SocketStream.Stop semantics:
// calling Stop twice must not panic.
func TestEventStream_StopIsIdempotent(t *testing.T) {
	client := socket.NewClient("/tmp/does-not-exist.sock")
	es := NewEventStream(client, "r", "a", false)
	es.Start()
	es.Stop()
	es.Stop() // must not panic
}
