package tui

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// eventStreamIdleTimeout is how long EventStream waits for data before
// checking whether it should close. Matches SocketStream's streamIdleTimeout
// so a thinking agent doesn't trip an idle disconnect on either stream.
const eventStreamIdleTimeout = 2 * time.Minute

// EventStream connects to the daemon's stream_events endpoint and delivers
// structured sidecar events (assistant_message, tool_call, token_usage,
// etc.) as a typed channel. Use alongside SocketStream: SocketStream
// carries the PTY-scraped chrome/spinners/text; EventStream carries the
// authoritative chat payload.
//
// Compared to SocketStream, EventStream is:
//
//   - Lossless for chat content (Go-side eventBroadcaster uses blocking
//     send with timeout, not silent-drop).
//   - Typed — consumers dispatch on Event.Kind instead of regex-guessing.
//   - Catchup-aware — a late subscriber receives the last ~256 events
//     from the broadcaster's ring buffer before live deliveries start.
//
// If the backend doesn't support sidecar subscription (non-direct
// backend) or OAT_USE_SIDECAR is off for the target agent, the
// connection still succeeds but the channel stays quiet — the caller
// should treat that as "fall back to SocketStream for this agent".
type EventStream struct {
	client      *socket.Client
	repoName    string
	agentName   string
	skipCatchUp bool

	events chan sidecar.Event
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool

	// Observability: cumulative count of events delivered.
	received atomic.Uint64
}

// streamEventLine matches the daemon's stream_events wire format.
// Kept in sync with internal/daemon/stream_handler.go:streamEventLine.
type streamEventLine struct {
	Event *sidecar.Event `json:"event,omitempty"`
	Done  bool           `json:"done,omitempty"`
	Err   string         `json:"error,omitempty"`
}

// NewEventStream creates an event-based output stream. skipCatchUp = true
// tells the daemon to skip the ring-buffer replay (useful when the TUI
// is reconnecting and already has prior events in hand).
func NewEventStream(client *socket.Client, repoName, agentName string, skipCatchUp bool) *EventStream {
	return &EventStream{
		client:      client,
		repoName:    repoName,
		agentName:   agentName,
		skipCatchUp: skipCatchUp,
		events:      make(chan sidecar.Event, 256),
		done:        make(chan struct{}),
	}
}

// Events returns the channel of incoming sidecar events.
func (s *EventStream) Events() <-chan sidecar.Event { return s.events }

// IsClosed returns true once the stream has ended.
func (s *EventStream) IsClosed() bool { return s.closed.Load() }

// Received returns the cumulative count of events delivered to the
// consumer — useful for debugging and for UI-side "saw N events" hints.
func (s *EventStream) Received() uint64 { return s.received.Load() }

// Start begins streaming in a goroutine.
func (s *EventStream) Start() {
	go s.run()
}

// Stop signals the stream to stop. Idempotent.
func (s *EventStream) Stop() {
	s.once.Do(func() { close(s.done) })
}

func (s *EventStream) run() {
	defer func() {
		s.closed.Store(true)
		close(s.events)
	}()

	args := map[string]interface{}{
		"repo":  s.repoName,
		"agent": s.agentName,
	}
	if s.skipCatchUp {
		args["skip_catchup"] = true
	}

	conn, err := s.client.StreamConnect(socket.Request{
		Command: "stream_events",
		Args:    args,
	})
	if err != nil {
		// Connection failed. Caller falls back to SocketStream for this
		// agent. This is expected for non-sidecar backends or agents
		// started before OAT_USE_SIDECAR was enabled.
		return
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(eventStreamIdleTimeout))

	scanner := bufio.NewScanner(conn)
	// Events can carry large tool-result payloads — match the server's
	// 4 MiB ceiling so we don't drop legitimate large events.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for {
		select {
		case <-s.done:
			return
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Idle timeout — agent may just be thinking. Check
					// done, extend deadline, rebuild scanner.
					select {
					case <-s.done:
						return
					default:
					}
					conn.SetReadDeadline(time.Now().Add(eventStreamIdleTimeout))
					scanner = bufio.NewScanner(conn)
					scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
					continue
				}
			}
			return
		}

		conn.SetReadDeadline(time.Now().Add(eventStreamIdleTimeout))

		var msg streamEventLine
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			// Malformed line: skip and keep reading. The server shouldn't
			// send bad JSON but we'd rather lose one line than drop the
			// whole subscription.
			continue
		}

		if msg.Done || msg.Err != "" {
			return
		}
		if msg.Event == nil {
			continue
		}

		s.received.Add(1)
		select {
		case s.events <- *msg.Event:
		case <-s.done:
			return
		}
	}
}
