package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// TestSocketStream_NoDeadlockWhenPlainChannelUnread is the regression test for
// the "agent did not respond" bug observed Apr 2026.
//
// Before the fix, run() did `s.lines <- msg.Line` as a BLOCKING send, but the
// TUI never consumes Lines() once a SocketStream is active (it prefers
// TypedLines()). Once the plain-channel buffer (256) filled, the goroutine
// blocked permanently and no further lines reached TypedLines() — manifesting
// as the agent's response never appearing in the chat stream.
//
// This test simulates that exact condition: a fake daemon streams more lines
// than fit in either channel, and the test only consumes TypedLines(). With
// the fix in place all lines arrive at TypedLines() within a reasonable time.
// Without the fix, this test deadlocks at ~256 lines and times out.
func TestSocketStream_NoDeadlockWhenPlainChannelUnread(t *testing.T) {
	const totalLines = 1500 // well above the 256 plain-channel buffer

	srv := startFakeStreamServer(t, totalLines)
	defer srv.close()

	client := socket.NewClient(srv.path)
	stream := NewSocketStream(client, "repo", "agent", nil, false)
	stream.Start()
	defer stream.Stop()

	// "Healthy" threshold: substantially more than the channel buffers (256
	// each). The exact pre-fix failure ceiling was ~512 lines (typed buffer
	// + plain buffer) before the goroutine deadlocked. Anything well above
	// that proves both channels stay drained under burst load. We don't
	// require exactly totalLines because TCP/Scanner edge effects at the
	// connection's tail can swallow the last line or two even in healthy
	// runs — the bug we're guarding against drops orders of magnitude more.
	const healthyMin = 1200

	deadline := time.After(5 * time.Second)
	got := 0
loop:
	for {
		select {
		case tl, ok := <-stream.TypedLines():
			if !ok {
				break loop
			}
			if tl.Text == "" {
				continue
			}
			got++
			if got >= healthyMin {
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	recv, deliv, _, plainDrop := stream.Stats()
	if got < healthyMin {
		t.Fatalf("only got %d/%d lines (healthy min=%d, received=%d delivered=%d plainDrops=%d) — likely the pre-fix deadlock at ~512 lines",
			got, totalLines, healthyMin, recv, deliv, plainDrop)
	}
}

// fakeSocketStreamServer is a minimal Unix-socket server that emulates the
// daemon's stream_output protocol: handshake, then `count` JSON-encoded
// lines. Distinct from event_stream_test.go's fakeStreamServer (which serves
// event-stream JSON over a TCP listener for the event_stream.go consumer);
// keeping them as separate types avoids a compile collision in the package
// and makes the call-site intent obvious.
type fakeSocketStreamServer struct {
	path string
	ln   net.Listener
	done chan struct{} // closed when close() is called — signals the goroutine to exit
	exit chan struct{} // closed when the goroutine actually exits
}

func startFakeStreamServer(t *testing.T, count int) *fakeSocketStreamServer {
	t.Helper()

	// macOS Unix sockets cap path length at ~104 bytes. t.TempDir() under
	// /var/folders/... already eats most of the budget, so use a short
	// path under the system temp root and clean up explicitly.
	f, err := os.CreateTemp("", "oats*.sock")
	if err != nil {
		t.Fatalf("create temp sock: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path) //nolint:errcheck — net.Listen wants an absent path

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &fakeSocketStreamServer{
		path: path,
		ln:   ln,
		done: make(chan struct{}),
		exit: make(chan struct{}),
	}

	go func() {
		defer close(srv.exit)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read the request (we don't validate it — just consume the line).
		buf := make([]byte, 4096)
		conn.Read(buf) //nolint:errcheck

		enc := json.NewEncoder(conn)

		// Handshake.
		if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
			return
		}

		// Burst all `count` lines as fast as the socket allows. The point
		// is to overflow channel buffers on the consumer side.
		for i := 0; i < count; i++ {
			line := streamOutputLine{
				Line:     fmt.Sprintf("line-%04d", i),
				LineType: "text",
			}
			if err := enc.Encode(line); err != nil {
				return
			}
		}
		// Hold the connection open. The test calls stream.Stop() when
		// done; closing here would race with the consumer and trigger
		// in-flight EOF before all lines drain through bufio.Scanner.
		<-srv.done
	}()

	// Allow the listener to settle.
	time.Sleep(20 * time.Millisecond)
	return srv
}

func (s *fakeSocketStreamServer) close() {
	close(s.done)
	s.ln.Close()
	os.Remove(s.path) //nolint:errcheck
	<-s.exit
}
