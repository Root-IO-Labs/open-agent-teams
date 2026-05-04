package tui

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

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
	conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))

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
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Deadline fired — check if TUI wants to stop
					select {
					case <-s.done:
						return
					default:
					}
					// Agent may just be thinking with no output. Extend deadline and continue.
					conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))
					// Re-create scanner since the underlying reader hit an error
					scanner = bufio.NewScanner(conn)
					scanner.Buffer(make([]byte, 64*1024), 256*1024)
					continue
				}
			}
			return // real connection close or unrecoverable error
		}

		// Got data — reset the idle deadline
		conn.SetReadDeadline(time.Now().Add(streamIdleTimeout))

		var msg streamOutputLine
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		if msg.Done || msg.Err != "" {
			return
		}

		// Apply category filter
		if s.filter != nil && !s.filter.FilterLine(msg.Line) {
			continue
		}

		tl := TypedLine{Text: msg.Line, LineType: msg.LineType}
		select {
		case s.typedLines <- tl:
		default:
		}
		select {
		case s.lines <- msg.Line:
		case <-s.done:
			return
		}
	}
}
