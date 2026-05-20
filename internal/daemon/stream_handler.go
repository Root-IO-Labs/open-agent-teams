package daemon

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
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
// streaming client. Kept short so zombie oat-ui processes (still running but
// not reading) are ejected quickly and don't block daemon event delivery.
const streamWriteTimeout = 5 * time.Second

// HandleStream dispatches streaming commands.
func (sh *streamHandler) HandleStream(req socket.Request, conn net.Conn) {
	switch req.Command {
	case "stream_output":
		sh.handleStreamOutput(req, conn)
	case "stream_events":
		sh.handleStreamEvents(req, conn)
	case "stream_agent_output":
		sh.handleStreamAgentOutput(req, conn)
	case "stream_assistant_turns":
		sh.handleStreamAssistantTurns(req, conn)
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

	// Backend must implement SidecarSubscriber. Backends without
	// sidecar support return a clean error rather than silently
	// producing an empty stream, which would confuse the TUI.
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

// agentOutputBatchInterval is the minimum wall-clock gap between flushes
// to a stream_agent_output client. The first chunk after an idle period
// is delivered immediately, then the handler accumulates any further
// chunks for up to this interval and batches them into a single frame.
//
// 16 ms is the plan's target — fast enough to feel "live" in a debug
// console (~60 Hz), slow enough that a chatty agent emitting many tiny
// chunks doesn't pin the WS connection. Tuning note: smaller values
// increase WS frame count linearly; larger values increase perceived
// latency. The cap mostly matters during streaming token output, which
// is by far the chattiest source of bytes.
const agentOutputBatchInterval = 16 * time.Millisecond

// streamAgentChunkFrame is the wire format for stream_agent_output. Each
// frame is either:
//   - a chunk frame (Chunk set, Gap == 0): base64-encoded bytes from the
//     PTY, optionally batched across multiple PTY reads.
//   - a gap frame (Gap > 0): N bytes were dropped because this subscriber
//     fell behind. Debug view renders this as "[N bytes dropped]"; pretty
//     mode ignores it.
//   - the terminal frame (Done: true): agent exited or broadcaster closed.
//   - an error frame (Err set): unrecoverable problem.
//
// TS is RFC3339 UTC so the client can compute latency without negotiating
// a clock format.
type streamAgentChunkFrame struct {
	Chunk string `json:"chunk,omitempty"`
	Gap   int64  `json:"gap,omitempty"`
	TS    string `json:"ts,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Err   string `json:"error,omitempty"`
}

// handleStreamAgentOutput is the Part-2c side-panel chat output stream.
//
// Identifies the agent by (session, agent_name) — matching the
// OAT_BROWSER_AGENT_SESSION and OAT_BROWSER_AGENT_NAME env vars from
// Part 2a — so the oat-browser-agent bridge doesn't have to reverse-
// resolve the repository name. We still need a state lookup to translate
// agent_name → window_name for the backend's session map (they happen
// to match for browser agents today, but the daemon's contract pins
// state.Agent.WindowName as the canonical key, so we follow it).
//
// Throttling: the daemon batches chunks at agentOutputBatchInterval (16 ms)
// before flushing a frame. This is enforced on the daemon socket boundary
// per the plan, so both the bridge's pretty-mode heartbeat and the
// debug-view raw render benefit from the same backpressure profile.
//
// Backpressure: ChunkFrame.Gap markers surface to the client when the
// subscriber's channel fills — already handled by the chunkBroadcaster.
// The handler re-encodes them as gap frames on the wire so the side
// panel can render an explicit "bytes dropped" indicator.
//
// Protocol:
//  1. Server sends handshake: {"success":true,"stream":true}
//  2. Server streams JSON lines: {"chunk":"<base64>","ts":"..."} or
//     {"gap":N,"ts":"..."} as they arrive (batched at 16 ms minimum).
//  3. On agent exit / cancel: {"done":true}
//  4. On error: {"error":"msg"} then close.
func (sh *streamHandler) handleStreamAgentOutput(req socket.Request, conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)

	sessionName, _ := req.Args["session"].(string)
	agentName, _ := req.Args["agent"].(string)
	if sessionName == "" || agentName == "" {
		enc.Encode(socket.Response{Success: false, Error: "session and agent are required"}) //nolint:errcheck
		return
	}

	directBackend, ok := sh.d.backend.(*backend_pkg.DirectBackend)
	if !ok {
		enc.Encode(socket.Response{Success: false, Error: "raw output streaming not supported for this backend"}) //nolint:errcheck
		return
	}

	// Translate (session, agent_name) → (session, window_name). The bridge
	// addresses agents by the state agent-name; the backend's session map
	// is keyed by window-name. They normally match for browser agents
	// but we don't rely on that invariant here.
	repoName, _, found := sh.d.findRepoBySession(sessionName)
	if !found {
		enc.Encode(socket.Response{Success: false, Error: "no repository is bound to session " + sessionName}) //nolint:errcheck
		return
	}
	agent, exists := sh.d.state.GetAgent(repoName, agentName)
	if !exists {
		enc.Encode(socket.Response{Success: false, Error: "agent '" + agentName + "' not found in session " + sessionName}) //nolint:errcheck
		return
	}
	// Defense in depth: stream_agent_output is intended for the side-panel
	// chat path. Restricting to AgentTypeBrowser matches the agent_input
	// restriction (Part 2b) and prevents a curious or malicious socket
	// client from siphoning raw PTY bytes (which include ANSI / TUI state
	// that other agent types don't expect to be readable in real time)
	// out of the supervisor or a worker. Line-level subscription via the
	// existing stream_output verb stays available for those agent types.
	if agent.Type != state.AgentTypeBrowser {
		enc.Encode(socket.Response{Success: false, Error: "stream_agent_output is restricted to browser-agent type; " + repoName + "/" + agentName + " is " + string(agent.Type)}) //nolint:errcheck
		return
	}

	_, ch, cancel, err := directBackend.SubscribeRawOutput(sessionName, agent.WindowName)
	if err != nil {
		enc.Encode(socket.Response{Success: false, Error: err.Error()}) //nolint:errcheck
		return
	}
	defer cancel()

	// Handshake.
	conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
	if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
		return
	}

	// Disconnect detector — Unix socket reads return when the remote end
	// closes. Same pattern as the existing stream_output handler.
	connDead := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) //nolint:errcheck
		close(connDead)
	}()

	// Pump frames with 16 ms minimum batching. Implementation: on the
	// first frame after an idle period, start a timer; subsequent frames
	// arriving before the timer fires accumulate into a batch; on timer
	// fire, flush the batch as one or more wire frames. Chunk batches
	// concatenate raw bytes (preserves PTY ordering); gap markers stay
	// as separate frames so the client can render them distinctly.
	var pendingBytes []byte
	var pendingChunkTS time.Time
	var pendingGap int64
	var pendingGapTS time.Time
	flushTimer := time.NewTimer(0)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	timerArmed := false

	flush := func() bool {
		if len(pendingBytes) > 0 {
			frame := streamAgentChunkFrame{
				Chunk: base64.StdEncoding.EncodeToString(pendingBytes),
				TS:    pendingChunkTS.Format(time.RFC3339Nano),
			}
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(frame); err != nil {
				sh.d.logger.Debug("stream_agent_output write failed for %s/%s: %v", sessionName, agentName, err)
				return false
			}
			pendingBytes = nil
			pendingChunkTS = time.Time{}
		}
		if pendingGap > 0 {
			frame := streamAgentChunkFrame{
				Gap: pendingGap,
				TS:  pendingGapTS.Format(time.RFC3339Nano),
			}
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(frame); err != nil {
				sh.d.logger.Debug("stream_agent_output gap write failed for %s/%s: %v", sessionName, agentName, err)
				return false
			}
			pendingGap = 0
			pendingGapTS = time.Time{}
		}
		timerArmed = false
		return true
	}

	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				// Broadcaster closed (agent exited). Flush anything
				// queued, then send done.
				if !flush() {
					return
				}
				conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
				enc.Encode(streamAgentChunkFrame{Done: true})             //nolint:errcheck
				return
			}
			if frame.Gap > 0 {
				pendingGap += frame.Gap
				if pendingGapTS.IsZero() {
					pendingGapTS = frame.TS
				}
			} else if len(frame.Chunk) > 0 {
				pendingBytes = append(pendingBytes, frame.Chunk...)
				if pendingChunkTS.IsZero() {
					pendingChunkTS = frame.TS
				}
			}
			if !timerArmed {
				flushTimer.Reset(agentOutputBatchInterval)
				timerArmed = true
			}
		case <-flushTimer.C:
			if !flush() {
				return
			}
		case <-connDead:
			sh.d.logger.Debug("stream_agent_output client disconnected for %s/%s", sessionName, agentName)
			return
		}
	}
}

// handleStreamAssistantTurns is Part 2g's side-panel auto-emit path.
//
// Subscribes the caller to the per-agent assistantTurnTailer's
// broadcaster — every ASSISTANT block the runtime writes to
// OAT_TOOL_LOG becomes a JSON frame on this stream, with a sanitized
// body and a heuristic-detected kind ("final" or "question").
//
// Why two side-panel streams (stream_agent_output AND
// stream_assistant_turns)?
//
//   - stream_agent_output is the byte-level PTY feed — used by
//     debug-mode view, activity heartbeat, and any future "is the
//     model alive?" sensors. It contains TUI repaints, spinners, and
//     all the noise the side-panel pretty mode does NOT want to render
//     as chat bubbles.
//   - stream_assistant_turns is the cleaned, structured turn feed —
//     used by pretty-mode chat. The bridge subscribes here and emits
//     chat_response frames to the side panel automatically, regardless
//     of whether the model called browser_emit_to_user.
//
// Identity model matches stream_agent_output: addressed by
// (session, agent_name) so the bridge doesn't have to reverse-resolve
// the repository name. Restricted to AgentTypeBrowser for the same
// reason the byte-level stream is — the parsed turn feed is intended
// for side-panel chat only and exposing it for other agent types
// would change the audit-surface of those agents.
//
// Protocol:
//  1. Server sends handshake: {"success":true,"stream":true}
//  2. Server streams JSON lines: {"text":"...","kind":"final"|"question","ts":"..."}
//  3. On broadcaster close / daemon shutdown: {"done":true}
//  4. On error: {"error":"msg"} then close.
func (sh *streamHandler) handleStreamAssistantTurns(req socket.Request, conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)

	sessionName, _ := req.Args["session"].(string)
	agentName, _ := req.Args["agent"].(string)
	if sessionName == "" || agentName == "" {
		enc.Encode(socket.Response{Success: false, Error: "session and agent are required"}) //nolint:errcheck
		return
	}

	// Translate (session, agent_name) → state.Agent so we can enforce
	// the AgentTypeBrowser boundary. The error responses mirror
	// stream_agent_output for consistency on the client side.
	repoName, _, found := sh.d.findRepoBySession(sessionName)
	if !found {
		enc.Encode(socket.Response{Success: false, Error: "no repository is bound to session " + sessionName}) //nolint:errcheck
		return
	}
	agent, exists := sh.d.state.GetAgent(repoName, agentName)
	if !exists {
		enc.Encode(socket.Response{Success: false, Error: "agent '" + agentName + "' not found in session " + sessionName}) //nolint:errcheck
		return
	}
	if agent.Type != state.AgentTypeBrowser {
		enc.Encode(socket.Response{Success: false, Error: "stream_assistant_turns is restricted to browser-agent type; " + repoName + "/" + agentName + " is " + string(agent.Type)}) //nolint:errcheck
		return
	}

	broadcaster := sh.d.lookupAssistantTurnBroadcaster(sessionName, agentName)
	if broadcaster == nil {
		// Agent is registered but the tailer hasn't been started
		// (or was already stopped). Refuse rather than blocking
		// forever; the client (bridge) will retry if appropriate.
		sh.d.logger.Info("stream_assistant_turns: no tailer active for %s/%s — bridge will backoff-retry", sessionName, agentName)
		enc.Encode(socket.Response{Success: false, Error: "no assistant-turn tailer active for " + agentName + " in session " + sessionName}) //nolint:errcheck
		return
	}

	ch, cancel := broadcaster.Subscribe()
	defer cancel()
	sh.d.logger.Info("stream_assistant_turns: bridge subscribed for %s/%s", sessionName, agentName)

	// Handshake.
	conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
	if err := enc.Encode(socket.Response{Success: true, Stream: true}); err != nil {
		return
	}

	// Detect client disconnect on idle (same pattern as
	// handleStreamAgentOutput). Without this a side-panel that closes
	// during a long quiet period would hold the goroutine until the
	// next turn arrives.
	connDead := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) //nolint:errcheck
		close(connDead)
	}()

	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
				enc.Encode(assistantTurnFrame{Done: true})                //nolint:errcheck
				return
			}
			conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) //nolint:errcheck
			if err := enc.Encode(frame); err != nil {
				sh.d.logger.Debug("stream_assistant_turns write failed for %s/%s: %v", sessionName, agentName, err)
				return
			}
			if frame.Done {
				return
			}
		case <-connDead:
			sh.d.logger.Debug("stream_assistant_turns client disconnected for %s/%s", sessionName, agentName)
			return
		}
	}
}
