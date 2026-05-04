package daemon

import (
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// streamHandler implements socket.StreamHandler for long-lived streaming connections.
type streamHandler struct {
	d *Daemon
}

// streamOutputLine is a single line sent over the stream.
type streamOutputLine struct {
	Line     string `json:"line,omitempty"`
	LineType string `json:"line_type,omitempty"` // tool_call, tool_output, thinking, system, user_input, text
	Done     bool   `json:"done,omitempty"`
	Err      string `json:"error,omitempty"`
}

// streamEventLine is a single envelope sent over the stream_events stream.
// Either Event is set (a real sidecar event) OR Done/Err is set (end of
// stream). Never both — clients key on presence.
type streamEventLine struct {
	Event *sidecar.Event `json:"event,omitempty"`
	Done  bool           `json:"done,omitempty"`
	Err   string         `json:"error,omitempty"`
}

// classifyLine returns a line_type string for the stream protocol.
// This gives the TUI authoritative metadata so it doesn't have to regex-guess.
func classifyLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "text"
	}
	if strings.HasPrefix(trimmed, "(*) ") || strings.HasPrefix(trimmed, "⏺ ") || strings.HasPrefix(trimmed, "● ") {
		return "tool_call"
	}
	if strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "[Command ") ||
		strings.HasPrefix(line, "  | ") || strings.HasPrefix(line, "  ▎ ") ||
		strings.HasPrefix(trimmed, "Exit code:") {
		return "tool_output"
	}
	if strings.HasPrefix(trimmed, "Thinking...") || trimmed == "Thinking…" {
		return "thinking"
	}
	if strings.Contains(trimmed, "📨 Message from daemon:") || strings.HasPrefix(trimmed, "[daemon]") {
		return "system"
	}
	if strings.HasPrefix(trimmed, "> ") && !strings.HasPrefix(trimmed, "> 📨") {
		return "user_input"
	}
	return "text"
}

// streamWriteTimeout is the maximum time to wait for a single write to the
// streaming client. If the client is hung or the network is broken, we give
// up after this duration to avoid leaking the goroutine.
const streamWriteTimeout = 30 * time.Second

// HandleStream dispatches streaming commands.
func (sh *streamHandler) HandleStream(req socket.Request, conn net.Conn) {
	switch req.Command {
	case "stream_output":
		sh.handleStreamOutput(req, conn)
	case "stream_events":
		sh.handleStreamEvents(req, conn)
	default:
		// Unknown streaming command — send error and close
		resp := socket.Response{Success: false, Error: "unknown stream command: " + req.Command}
		json.NewEncoder(conn).Encode(resp) //nolint:errcheck
		conn.Close()
	}
}

// handleStreamOutput streams live agent output over a long-lived connection.
// Protocol:
//  1. Server sends handshake: {"success":true,"stream":true}
//  2. Server sends JSON lines: {"line":"text"} per line
//  3. On agent exit: {"done":true}
//  4. On error: {"error":"msg"} then close
func (sh *streamHandler) handleStreamOutput(req socket.Request, conn net.Conn) {
	defer conn.Close()

	enc := json.NewEncoder(conn)

	// Extract args
	repoName, _ := req.Args["repo"].(string)
	agentName, _ := req.Args["agent"].(string)
	if repoName == "" || agentName == "" {
		enc.Encode(socket.Response{Success: false, Error: "repo and agent are required"}) //nolint:errcheck
		return
	}

	skipCatchUp, _ := req.Args["skip_catchup"].(bool)

	// Look up the agent's session and subscribe to output
	repo, exists := sh.d.state.GetRepo(repoName)
	if !exists {
		enc.Encode(socket.Response{Success: false, Error: "repo not found"}) //nolint:errcheck
		return
	}

	directBackend, ok := sh.d.backend.(*backend_pkg.DirectBackend)
	if !ok {
		enc.Encode(socket.Response{Success: false, Error: "streaming not supported for this backend"}) //nolint:errcheck
		return
	}

	agent, agentExists := sh.d.state.GetAgent(repoName, agentName)
	if !agentExists {
		enc.Encode(socket.Response{Success: false, Error: "agent not found"}) //nolint:errcheck
		return
	}

	var ch <-chan string
	var cancel func()
	var err error

	if skipCatchUp {
		_, ch, cancel, err = directBackend.SubscribeOutputLive(repo.SessionName, agent.WindowName)
	} else {
		_, ch, cancel, err = directBackend.SubscribeOutput(repo.SessionName, agent.WindowName)
	}
	if err != nil {
		enc.Encode(socket.Response{Success: false, Error: err.Error()}) //nolint:errcheck
		return
	}
	defer cancel()

	// Send handshake with write deadline to detect hung clients early
	conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
	if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
		return
	}

	// Detect client disconnect even when no output is being produced.
	// Spawn a goroutine that reads from the connection — Unix sockets
	// return an error when the remote end closes. When detected, close
	// connDead to unblock the streaming select below.
	connDead := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) //nolint:errcheck // returns when client disconnects
		close(connDead)
	}()

	// Stream lines until channel closes or client disconnects.
	// Each write gets a deadline so hung clients don't leak this goroutine.
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				// Channel closed — agent exited or broadcaster closed
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
				enc.Encode(streamOutputLine{Done: true})               //nolint:errcheck
				return
			}
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(streamOutputLine{Line: line, LineType: classifyLine(line)}); err != nil {
				sh.d.logger.Debug("Stream write failed for %s/%s: %v", repoName, agentName, err)
				return
			}
		case <-connDead:
			sh.d.logger.Debug("Stream client disconnected for %s/%s", repoName, agentName)
			return
		}
	}
}

// handleStreamEvents streams structured sidecar events to a TUI subscriber.
//
// Protocol (mirrors stream_output):
//  1. Server sends handshake: {"success":true,"stream":true}
//  2. Server first flushes any catchup events (one per line) so a
//     mid-session TUI sees recent context immediately.
//  3. Server sends live events as {"event":{...}} per line.
//  4. On agent exit / broadcaster close: {"done":true}
//  5. On error: {"error":"msg"} then close.
//
// Requires the configured backend to implement backend.SidecarSubscriber.
// Returns an error response if the backend is not sidecar-capable or the
// agent doesn't exist. If the sidecar is off for this agent (OAT_USE_SIDECAR
// unset at start time), the subscription still succeeds — the channel
// simply never fires and the client sees only catchup (if any) before
// sitting idle until disconnect.
func (sh *streamHandler) handleStreamEvents(req socket.Request, conn net.Conn) {
	defer conn.Close()

	enc := json.NewEncoder(conn)

	repoName, _ := req.Args["repo"].(string)
	agentName, _ := req.Args["agent"].(string)
	if repoName == "" || agentName == "" {
		enc.Encode(socket.Response{Success: false, Error: "repo and agent are required"}) //nolint:errcheck
		return
	}

	skipCatchUp, _ := req.Args["skip_catchup"].(bool)

	repo, exists := sh.d.state.GetRepo(repoName)
	if !exists {
		enc.Encode(socket.Response{Success: false, Error: "repo not found"}) //nolint:errcheck
		return
	}

	// Backend must implement SidecarSubscriber. Non-sidecar backends
	// (e.g., a future tmux backend) return a clean error rather than
	// silently producing an empty stream, which would confuse the TUI.
	subscriber, ok := sh.d.backend.(backend_pkg.SidecarSubscriber)
	if !ok {
		enc.Encode(socket.Response{Success: false, Error: "event streaming not supported for this backend"}) //nolint:errcheck
		return
	}

	agent, agentExists := sh.d.state.GetAgent(repoName, agentName)
	if !agentExists {
		enc.Encode(socket.Response{Success: false, Error: "agent not found"}) //nolint:errcheck
		return
	}

	_, ch, catchup, cancel, err := subscriber.SubscribeEvents(sh.d.ctx, repo.SessionName, agent.WindowName)
	if err != nil {
		enc.Encode(socket.Response{Success: false, Error: err.Error()}) //nolint:errcheck
		return
	}
	defer cancel()

	// Handshake. Same shape as stream_output so clients can key off the
	// common Response fields.
	conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
	if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
		return
	}

	// Flush catchup events before the live stream so a TUI that joins
	// mid-conversation sees recent turns immediately. skipCatchUp lets a
	// reconnecting client that already has history request live-only.
	if !skipCatchUp {
		for _, ev := range catchup {
			evCopy := ev
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(streamEventLine{Event: &evCopy}); err != nil {
				return
			}
		}
	}

	// Detect client disconnect even when no events are arriving. Same
	// pattern as handleStreamOutput.
	connDead := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) //nolint:errcheck
		close(connDead)
	}()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
				enc.Encode(streamEventLine{Done: true})                //nolint:errcheck
				return
			}
			evCopy := ev
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(streamEventLine{Event: &evCopy}); err != nil {
				sh.d.logger.Debug("Event stream write failed for %s/%s: %v", repoName, agentName, err)
				return
			}
		case <-connDead:
			sh.d.logger.Debug("Event stream client disconnected for %s/%s", repoName, agentName)
			return
		}
	}
}
