package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// streamDebugLogger writes one-line traces of stream events to a debug log
// when OAT_STREAM_DEBUG=1 is set. Logging is opt-in to avoid disk churn
// during normal use; when enabled it captures each hop in the message-
// delivery path so we can pinpoint where lines are dropped.
//
// File: $HOME/.oat/logs/tui-stream.log (created on first write).
var (
	streamDebugInit sync.Once
	streamDebugFile *os.File
	streamDebugMu   sync.Mutex
)

func streamDebugEnabled() bool {
	return os.Getenv("OAT_STREAM_DEBUG") == "1"
}

func streamDebugf(format string, args ...interface{}) {
	if !streamDebugEnabled() {
		return
	}
	streamDebugInit.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir := filepath.Join(home, ".oat", "logs")
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return
		}
		path := filepath.Join(dir, "tui-stream.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		streamDebugFile = f
	})
	streamDebugMu.Lock()
	defer streamDebugMu.Unlock()
	if streamDebugFile == nil {
		return
	}
	fmt.Fprintf(streamDebugFile, "%s "+format+"\n",
		append([]interface{}{time.Now().Format("15:04:05.000")}, args...)...)
}

// streamIdleTimeout is how long the SocketStream waits for data before
// checking if it should close. This catches silent connection drops (daemon
// crash, network flap) that would otherwise block scanner.Scan() forever.
// Set generously so agents that are "thinking" with no output don't trigger it.
const streamIdleTimeout = 2 * time.Minute

// SocketStream connects to the daemon's stream_output endpoint and delivers
// live ANSI-stripped output lines. The rawBroadcaster does basic hygiene
// (blank collapsing, exact consecutive dedup). The TUI's DeduplicateAppend
// handles progressive streaming dedup.
type SocketStream struct {
	client      *socket.Client
	repoName    string
	agentName   string
	filter      *OutputFilter
	skipCatchUp bool // if true, skip ring buffer catch-up (TUI already has content)

	lines      chan string
	typedLines chan TypedLine
	done       chan struct{}
	once       sync.Once
	closed     atomic.Bool

	// Diagnostic counters. Exposed via Stats() for the debug log.
	received    atomic.Uint64 // lines pulled off the daemon socket
	delivered   atomic.Uint64 // lines delivered to the TUI typed channel
	plainDrops  atomic.Uint64 // plain channel sends dropped (TUI ignores it for sockets)
	filterDrops atomic.Uint64 // filtered out by OutputFilter
}

// streamOutputLine matches the daemon's streaming protocol.
type streamOutputLine struct {
	Line     string `json:"line,omitempty"`
	LineType string `json:"line_type,omitempty"` // tool_call, tool_output, thinking, system, user_input, text
	Done     bool   `json:"done,omitempty"`
	Err      string `json:"error,omitempty"`
}

// NewSocketStream creates a socket-based output stream.
// If skipCatchUp is true, the stream tells the daemon to skip ring buffer
// replay (used when reconnecting and the TUI already has prior content).
func NewSocketStream(client *socket.Client, repoName, agentName string, filter *OutputFilter, skipCatchUp bool) *SocketStream {
	return &SocketStream{
		client:      client,
		repoName:    repoName,
		agentName:   agentName,
		filter:      filter,
		skipCatchUp: skipCatchUp,
		lines:       make(chan string, 256),
		typedLines:  make(chan TypedLine, 256),
		done:        make(chan struct{}),
	}
}

// Lines returns the channel of output lines (plain text, no metadata).
func (s *SocketStream) Lines() <-chan string { return s.lines }

// TypedLines returns the channel of typed output lines with daemon metadata.
func (s *SocketStream) TypedLines() <-chan TypedLine { return s.typedLines }

// IsClosed returns true if the stream has ended.
func (s *SocketStream) IsClosed() bool { return s.closed.Load() }

// Start begins streaming in a goroutine.
func (s *SocketStream) Start() {
	go s.run()
}

// Stop signals the stream to stop.
func (s *SocketStream) Stop() {
	s.once.Do(func() { close(s.done) })
}

func (s *SocketStream) run() {
	defer func() {
		s.closed.Store(true)
		close(s.lines)
		close(s.typedLines)
	}()

	args := map[string]interface{}{
		"repo":  s.repoName,
		"agent": s.agentName,
	}
	if s.skipCatchUp {
		args["skip_catchup"] = true
	}

	conn, err := s.client.StreamConnect(socket.Request{
		Command: "stream_output",
		Args:    args,
	})
	if err != nil {
		return // connection failed — TUI will fall back to LogStream
	}
	defer conn.Close()

	// Set initial read deadline so we detect silent connection drops
	// (daemon crash, network flap) instead of blocking forever on Scan().
	_ = conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))

	// Read JSON lines from the stream
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for {
		// Check if we've been told to stop
		select {
		case <-s.done:
			return
		default:
		}

		if !scanner.Scan() {
			// Distinguish timeout (idle stream) from real close/error.
			if err := scanner.Err(); err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					// Deadline fired — check if TUI wants to stop
					select {
					case <-s.done:
						return
					default:
					}
					// Agent may just be thinking with no output. Extend deadline and continue.
					_ = conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))
					// Re-create scanner since the underlying reader hit an error
					scanner = bufio.NewScanner(conn)
					scanner.Buffer(make([]byte, 64*1024), 256*1024)
					continue
				}
			}
			return // real connection close or unrecoverable error
		}

		// Got data — reset the idle deadline
		_ = conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))

		var msg streamOutputLine
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		if msg.Done || msg.Err != "" {
			return
		}

		s.received.Add(1)

		// Apply category filter
		if s.filter != nil && !s.filter.FilterLine(msg.Line) {
			s.filterDrops.Add(1)
			streamDebugf("agent=%s filter-drop line=%q", s.agentName, truncateForLog(msg.Line))
			continue
		}

		tl := TypedLine{Text: msg.Line, LineType: msg.LineType}

		// FIX (Apr 2026): the agent-response-disappears bug.
		//
		// Previously this site did:
		//   select { case s.typedLines <- tl: default: }      // drop on full
		//   select { case s.lines <- msg.Line: case <-s.done: } // BLOCKING
		//
		// The TUI's readStream() prefers TypedLines() and never reads
		// Lines() once a SocketStream is active (see app.go:readStream —
		// `if typedCh != nil` short-circuits the plain channel). With both
		// channels sized 256, the plain channel filled and the goroutine
		// blocked permanently on `s.lines <-`. Subsequent agent output
		// arrived from the daemon but never reached the UI — the chat
		// stream simply went silent, even though the agent was producing
		// a response and the daemon was streaming it. Symptom: user sends
		// a message, the agent replies, the reply never appears.
		//
		// Fix: make the typed-channel send block (it IS consumed) and the
		// plain-channel send non-blocking (no consumer for socket streams,
		// drop is harmless). Both sends honor s.done so Stop() works.
		select {
		case s.typedLines <- tl:
			s.delivered.Add(1)
		case <-s.done:
			return
		}
		select {
		case s.lines <- msg.Line:
		case <-s.done:
			return
		default:
			s.plainDrops.Add(1)
		}

		if d := s.delivered.Load(); d > 0 && d%500 == 0 {
			streamDebugf("agent=%s delivered=%d received=%d filter-drops=%d plain-drops=%d typed-buf=%d/%d",
				s.agentName, d, s.received.Load(), s.filterDrops.Load(),
				s.plainDrops.Load(), len(s.typedLines), cap(s.typedLines))
		}
	}
}

// truncateForLog clips a line to a short prefix for debug logs.
func truncateForLog(s string) string {
	const maxLen = 120
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// Stats returns counters for diagnostics. Useful from tests and from the
// debug log to confirm where lines are being dropped along the pipeline.
func (s *SocketStream) Stats() (received, delivered, filterDrops, plainDrops uint64) {
	return s.received.Load(), s.delivered.Load(), s.filterDrops.Load(), s.plainDrops.Load()
}
