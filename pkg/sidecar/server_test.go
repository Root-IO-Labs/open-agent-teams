package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sockPath returns a per-test Unix socket path that's short enough to fit
// in macOS's 104-byte sun_path limit. t.TempDir() under /var/folders/...
// is often too long once the test name is appended, so we allocate under
// /tmp and register a cleanup for removal. Bytes matter: "/tmp/sc-XXXXXX/s"
// is ~16 chars — plenty of headroom.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sc-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// writeLine is a tiny helper for tests that act as the Python client —
// writes one event as newline-delimited JSON.
func writeLine(t *testing.T, c net.Conn, ev Event) {
	t.Helper()
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := c.Write(append(out, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func makeEvent(seq uint64, kind, turnID string, dataJSON string) Event {
	return Event{
		V: 1, Seq: seq, TS: 1, Kind: kind, TurnID: turnID,
		Data: json.RawMessage(dataJSON),
	}
}

func TestServer_Start_RequiresOnEvent(t *testing.T) {
	s := NewServer(sockPath(t))
	defer s.Close()
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected error when OnEvent is nil")
	}
}

func TestServer_Start_RejectsDoubleStart(t *testing.T) {
	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) {}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer s.Close()
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected error on second start")
	}
}

func TestServer_Start_RemovesStaleSocket(t *testing.T) {
	// Simulate a previous crashed run leaving a stale socket file.
	path := sockPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(path)
	s.OnEvent = func(Event) {}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start should have cleaned stale socket: %v", err)
	}
	defer s.Close()
}

func TestServer_ReceivesEvents(t *testing.T) {
	var (
		mu       sync.Mutex
		received []Event
		done     = make(chan struct{})
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(ev Event) {
		mu.Lock()
		received = append(received, ev)
		n := len(received)
		mu.Unlock()
		if n == 3 {
			close(done)
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	writeLine(t, c, makeEvent(1, KindTurnStart, "t", `{"user_input":"hi"}`))
	writeLine(t, c, makeEvent(2, KindAssistantDelta, "t", `{"content":"he"}`))
	writeLine(t, c, makeEvent(3, KindAssistantDelta, "t", `{"content":"hel"}`))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 3 events")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("got %d events", len(received))
	}
	if received[0].Kind != KindTurnStart || received[2].Seq != 3 {
		t.Errorf("unexpected events: %+v", received)
	}
}

func TestServer_DetectsGap(t *testing.T) {
	var (
		gaps   []struct{ expected, got uint64 }
		gapsMu sync.Mutex
		events = make(chan struct{}, 10)
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) { events <- struct{}{} }
	s.OnGap = func(expected, got uint64) {
		gapsMu.Lock()
		gaps = append(gaps, struct{ expected, got uint64 }{expected, got})
		gapsMu.Unlock()
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	writeLine(t, c, makeEvent(10, KindTurnStart, "t", `{"user_input":"a"}`))
	writeLine(t, c, makeEvent(11, KindTurnStart, "t", `{"user_input":"b"}`))
	writeLine(t, c, makeEvent(15, KindTurnStart, "t", `{"user_input":"c"}`)) // gap: 12,13,14 missing
	writeLine(t, c, makeEvent(16, KindTurnStart, "t", `{"user_input":"d"}`))

	for i := 0; i < 4; i++ {
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}

	gapsMu.Lock()
	defer gapsMu.Unlock()
	if len(gaps) != 1 {
		t.Fatalf("expected 1 gap, got %d: %+v", len(gaps), gaps)
	}
	if gaps[0].expected != 12 || gaps[0].got != 15 {
		t.Errorf("gap = %+v", gaps[0])
	}
}

func TestServer_ParseError_ContinuesStream(t *testing.T) {
	var (
		perr      []string
		perrMu    sync.Mutex
		goodCount int
		goodMu    sync.Mutex
		recvd     = make(chan struct{}, 10)
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) {
		goodMu.Lock()
		goodCount++
		goodMu.Unlock()
		recvd <- struct{}{}
	}
	s.OnParseError = func(line []byte, err error) {
		perrMu.Lock()
		perr = append(perr, string(line))
		perrMu.Unlock()
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Good, bad, good, bad, good — server must deliver all 3 good events
	// and call OnParseError for the 2 malformed lines.
	writeLine(t, c, makeEvent(1, KindTurnStart, "t", `{"user_input":"a"}`))
	c.Write([]byte("{not valid json\n"))
	writeLine(t, c, makeEvent(2, KindTurnStart, "t", `{"user_input":"b"}`))
	c.Write([]byte(`{"v":1,"seq":1,"ts":1,"data":{}}` + "\n")) // missing kind
	writeLine(t, c, makeEvent(3, KindTurnStart, "t", `{"user_input":"c"}`))

	for i := 0; i < 3; i++ {
		select {
		case <-recvd:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout at good event %d", i)
		}
	}

	// Give the parse-error callback a moment after the final good event
	// lands (handlers run synchronously in the reader goroutine so the
	// order is deterministic, but the test fixture polls with a timeout).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		perrMu.Lock()
		n := len(perr)
		perrMu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	perrMu.Lock()
	defer perrMu.Unlock()
	if len(perr) != 2 {
		t.Errorf("expected 2 parse errors, got %d: %v", len(perr), perr)
	}
}

func TestServer_OversizedLine_DroppedGracefully(t *testing.T) {
	// Scanner caps lines at MaxLineSize. A line larger than that is
	// reported via scanner.Err() = bufio.ErrTooLong and currently ends
	// the connection (documented limitation; Python client is expected
	// to stay under this via its own logic).
	var closeErr error
	closed := make(chan struct{})

	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) {
		t.Error("should not receive an oversized event")
	}
	s.OnClientClose = func(err error) {
		closeErr = err
		close(closed)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Write a line longer than MaxLineSize without a newline. Scanner
	// will read until its buffer is full, then error out.
	huge := strings.Repeat("x", MaxLineSize+1024)
	c.Write([]byte(huge))

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("OnClientClose not called")
	}
	if closeErr == nil {
		t.Error("expected non-nil close error for oversized line")
	}
}

func TestServer_ReconnectAccepted(t *testing.T) {
	var (
		seen = make(chan Event, 10)
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(ev Event) { seen <- ev }
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// First client: send one, close.
	c1, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, c1, makeEvent(1, KindTurnStart, "t", `{"user_input":"first"}`))
	c1.Close()

	select {
	case ev := <-seen:
		if ev.Seq != 1 {
			t.Errorf("first client event seq = %d", ev.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first event")
	}

	// Second client: connects after first closed. No gap should be
	// reported because server resets its sequence tracker on disconnect.
	var gapSeen bool
	s.OnGap = func(uint64, uint64) { gapSeen = true }

	c2, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	writeLine(t, c2, makeEvent(1, KindTurnStart, "t", `{"user_input":"second"}`))
	writeLine(t, c2, makeEvent(2, KindTurnStart, "t", `{"user_input":"third"}`))

	for i := 0; i < 2; i++ {
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout at reconnect event %d", i)
		}
	}
	if gapSeen {
		t.Error("gap falsely reported across reconnect")
	}
}

func TestServer_Close_Idempotent(t *testing.T) {
	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) {}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Closing twice must be safe — callers often defer Close after
	// explicit shutdown.
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestServer_Close_RemovesSocketFile(t *testing.T) {
	path := sockPath(t)
	s := NewServer(path)
	s.OnEvent = func(Event) {}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file not created: %v", err)
	}
	s.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still exists after Close: err=%v", err)
	}
}

func TestServer_ContextCancel_StopsAccept(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(sockPath(t))
	s.OnEvent = func(Event) {}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cancel()
	// Close waits for accept goroutine. Without the cancel handling
	// via context, this would hang until the test timeout.
	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after ctx cancel")
	}
}
