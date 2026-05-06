package backend

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// appendOatTokensSentinel writes a [OAT_TOKENS] line to the given log file
// using the same JSON shape the Python runtime emits. OutputWatcher then
// fires an EventTokenUsage which the daemon's handleTokenUsageEvent consumes.
//
// Why we re-emit onto the log file rather than call the daemon handler
// directly: the existing pipeline (OutputWatcher → handleTokenUsageEvent →
// monotonicity guard) is proven and already handles "same cumulative arrives
// twice" gracefully. Stdout [OAT_TOKENS] and sidecar token_usage therefore
// merge at the log tail: whichever arrives first wins, the second is a no-op
// at the monotonicity check. No new race conditions; no duplicate handler
// code; no new guard logic.
//
// O_APPEND on writes smaller than PIPE_BUF (512 bytes typical) is atomic on
// Linux/macOS so interleaved writes from Python + this bridge stay line-
// clean. A single [OAT_TOKENS] line is well under that.
//
// Best-effort: errors opening or writing the log file are swallowed. The
// stdout path is still carrying the same data; dropping the sidecar copy
// is strictly not worse than today's behavior.
func appendOatTokensSentinel(logFile string, data sidecar.TokenUsageData) {
	if logFile == "" {
		return
	}
	payload := map[string]any{
		"delta_input":       data.DeltaInput,
		"delta_output":      data.DeltaOutput,
		"cumulative_input":  data.CumulativeInput,
		"cumulative_output": data.CumulativeOutput,
	}
	// Match Python's conditional-emit: cache fields only appear on the
	// wire when non-zero. Keeps the dedup test (daemon comparing old vs
	// new cumulative) simple and avoids flipping cache totals to 0 when
	// a non-cache-aware provider's sidecar emission arrives between two
	// cache-aware stdout emissions.
	if data.CacheRead > 0 || data.CacheCreation > 0 {
		payload["cache_read"] = data.CacheRead
		payload["cache_creation"] = data.CacheCreation
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	line := fmt.Sprintf("[OAT_TOKENS] %s\n", encoded)

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// newSidecarServerForAgent builds a sidecar.Server wired to an agent's log
// file AND its event broadcaster. Called from StartAgent when
// AgentConfig.SidecarPath is set.
//
// OnEvent fans out to two sinks per event:
//
//  1. For KindTokenUsage: also append [OAT_TOKENS] to logFile so the
//     existing OutputWatcher → handleTokenUsageEvent pipeline updates
//     the daemon's token state. The monotonicity guard dedupes against
//     the same delta arriving via the stdout path.
//
//  2. For EVERY kind (including token_usage): publish to eventBc so
//     TUI subscribers receive the full structured event stream. Day 4b
//     wires the TUI to this broadcaster for chat-content rendering.
//
// If eventBc is nil, the fan-out to (2) is skipped — lets unit tests
// exercise the token-only path in isolation.
//
// The returned server is NOT yet started — caller invokes Start after
// optional callback override.
func newSidecarServerForAgent(
	socketPath, logFile string, eventBc *eventBroadcaster,
) *sidecar.Server {
	srv := sidecar.NewServer(socketPath)
	srv.OnEvent = func(ev sidecar.Event) {
		// Fan-out 1: token accounting via log file. Gated on kind so
		// non-token events don't pollute the log (OutputWatcher parses
		// every line).
		if ev.Kind == sidecar.KindTokenUsage {
			if data, err := ev.AsTokenUsage(); err == nil {
				appendOatTokensSentinel(logFile, data)
			}
		}
		// Fan-out 2: TUI event stream. Publish every kind so the TUI
		// can build up chat history, live token displays, tool-call
		// panels, etc. Token events are intentionally delivered to
		// BOTH sinks — the log path owns daemon-side accounting; the
		// broadcaster path owns live UI updates without polling.
		if eventBc != nil {
			eventBc.Publish(ev)
		}
	}
	return srv
}
