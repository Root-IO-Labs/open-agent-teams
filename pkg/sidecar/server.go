package sidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// MaxLineSize bounds the largest single event we'll accept from the client.
// Tool-result payloads can be large (file reads, grep output), but not
// arbitrarily so — a 4 MiB ceiling catches pathological inputs without
// bottlenecking normal traffic. The Python client is expected to stay well
// under this via its own emitter logic (large tool results are offloaded
// to the filesystem; the event only carries a pointer).
const MaxLineSize = 4 * 1024 * 1024

// Server listens on a Unix domain socket and parses newline-delimited
// events from a single connected client (one Python agent → one Server).
//
// Lifecycle:
//
//	srv := NewServer(path)
//	srv.OnEvent = func(Event) { ... }     // required
//	if err := srv.Start(ctx); err != nil { ... }
//	defer srv.Close()
//
// Start is non-blocking. An accept goroutine runs until Close is called or
// the context is canceled. The socket file is removed on Close. Only one
// connection is handled at a time; if the client reconnects, the accept
// loop picks up the new connection automatically.
type Server struct {
	path string

	// OnEvent is called for each successfully parsed event in the order
	// they were received. Must NOT block — the reader goroutine invokes
	// it synchronously; blocking here stalls the stream. If callers need
	// to do heavy work, buffer to a channel inside OnEvent.
	OnEvent func(Event)

	// OnGap is called when the seq numbers skip. A gap indicates the
	// Python emitter dropped events from its bounded queue under
	// backpressure. Optional; unset = silent.
	OnGap func(expected, got uint64)

	// OnParseError is called for lines that fail to parse. Optional.
	// The line is discarded either way — the stream continues.
	OnParseError func(line []byte, err error)

	// OnClientClose is called when the current connection ends (EOF,
	// error, or peer disconnect). The Server is still running and will
	// accept another connection unless Server.Close() was called.
	OnClientClose func(err error)

	ln       net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   atomic.Bool
	started  atomic.Bool
	lastSeq  atomic.Uint64
	hasFirst atomic.Bool
}

// NewServer prepares a Server bound to the given Unix socket path. It does
// NOT create the socket — Start does. The path is removed on Close.
func NewServer(path string) *Server {
	return &Server{path: path}
}

// Start binds the listener and launches the accept goroutine. Returns an
// error if the socket cannot be created (e.g. path collision with a live
// listener the caller didn't unlink). Idempotent per instance: calling
// Start twice returns ErrAlreadyStarted.
func (s *Server) Start(ctx context.Context) error {
	if s.OnEvent == nil {
		return errors.New("sidecar.Server: OnEvent is required")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("sidecar.Server: already started")
	}

	// Remove any stale socket file from a crashed previous run. If the
	// path is actually in use by a live process, Listen will fail below
	// and we'll surface that error.
	_ = os.Remove(s.path)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", s.path)
	if err != nil {
		return fmt.Errorf("sidecar.Server: listen %s: %w", s.path, err)
	}
	s.ln = ln

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return nil
}

// Close stops the accept loop, closes any active connection, removes the
// socket file, and waits for the accept goroutine to exit. Safe to call
// multiple times; only the first call does work.
func (s *Server) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	_ = os.Remove(s.path)
	s.wg.Wait()
	return nil
}

// Path returns the socket path this server is bound to.
func (s *Server) Path() string { return s.path }

func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Accept fails after Close() closes the listener — that's
			// the expected exit path. Any other error also exits;
			// the server's Close() contract is "one listener, one life".
			if ctx.Err() != nil || s.closed.Load() {
				return
			}
			// Transient accept errors are rare for Unix sockets; log-worthy
			// but not fatal. Back off briefly and keep going.
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}

		// Block here — only one client at a time. When the reader
		// returns (EOF, error, Close), we loop and accept a reconnect.
		s.readConn(ctx, conn)
	}
}

func (s *Server) readConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Bubble up cancellation to the blocked Read in Scanner.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.(*net.UnixConn).CloseRead()
		case <-done:
		}
	}()

	scanner := bufio.NewScanner(conn)
	// Default buffer is 64 KiB; bump to MaxLineSize so large tool-result
	// payloads don't cause bufio.ErrTooLong. Scanner enforces this as a
	// hard cap — oversized lines are dropped with an OnParseError.
	scanner.Buffer(make([]byte, 64*1024), MaxLineSize)

	var readErr error
	for scanner.Scan() {
		line := scanner.Bytes()
		// Scanner reuses its buffer across iterations. We must copy bytes
		// we want to retain — but ParseEvent unmarshals synchronously
		// into our own struct, so no retention is needed here.
		if len(line) == 0 {
			continue
		}
		ev, err := ParseEvent(line)
		if err != nil {
			if s.OnParseError != nil {
				// Give the handler a stable copy — scanner.Bytes is
				// invalidated on the next Scan().
				cp := make([]byte, len(line))
				copy(cp, line)
				s.OnParseError(cp, err)
			}
			continue
		}
		s.trackSeq(ev.Seq)
		s.OnEvent(ev)
	}

	if err := scanner.Err(); err != nil {
		readErr = err
	} else if !errors.Is(readErr, io.EOF) {
		// scanner.Err() returns nil on EOF — that's the normal close.
		readErr = io.EOF
	}

	// Reset sequence tracking so a reconnect starts fresh. The emitter
	// begins each connection at seq=0 anyway; carrying lastSeq across
	// reconnects would produce a spurious gap.
	s.lastSeq.Store(0)
	s.hasFirst.Store(false)

	if s.OnClientClose != nil {
		s.OnClientClose(readErr)
	}
}

func (s *Server) trackSeq(got uint64) {
	if !s.hasFirst.Load() {
		s.hasFirst.Store(true)
		s.lastSeq.Store(got)
		return
	}
	expected := s.lastSeq.Load() + 1
	if got != expected && s.OnGap != nil {
		s.OnGap(expected, got)
	}
	s.lastSeq.Store(got)
}
