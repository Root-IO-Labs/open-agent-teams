package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// startEventStreamFor opens a stream_events subscription for an agent.
// Fail-soft: if the daemon's backend doesn't implement SidecarSubscriber
// or the sidecar is off for this agent, StreamConnect simply returns an
// error and EventStream.run() exits without ever delivering — the TUI
// keeps rendering via SocketStream as today.
//
// The decision to enable the sidecar lives on the DAEMON
// (OAT_USE_SIDECAR env var when starting the daemon). The TUI always
// tries to subscribe; that way a user who sets OAT_USE_SIDECAR=1 on the
// daemon but forgets to export it for `oat ui` still gets the
// structured event stream.
//
// No drain goroutine is spawned — the Update loop pulls events
// cooperatively via readEventStream (scheduled from the streamTickMsg
// handler). That keeps all state mutation single-threaded inside
// Bubble Tea's Update.
func (a *App) startEventStreamFor(agent string) {
	if existing, ok := a.eventStreams[agent]; ok && !existing.IsClosed() {
		return
	}

	// skipCatchUp=false so a TUI joining mid-session receives the last
	// ~256 events from the broadcaster's ring buffer before live
	// deliveries start. On a fresh TUI boot, catchup is empty anyway.
	es := NewEventStream(a.client, a.repoName, agent, false)
	es.Start()
	a.eventStreams[agent] = es
}

// stopEventStreamFor tears down an agent's EventStream if one exists.
// Safe to call for agents that never had a stream started.
func (a *App) stopEventStreamFor(agent string) {
	if es, ok := a.eventStreams[agent]; ok {
		es.Stop()
		delete(a.eventStreams, agent)
	}
	delete(a.chatFromEvents, agent)
}

// readEventStream drains any buffered sidecar events for an agent and
// returns them as a streamBatchMsg (same shape as readStream's output)
// so the existing Update path appends them to outputContent/outputTypes
// with zero new branches.
//
// Event → line mapping:
//
//   - assistant_message → content as a "text" line
//   - tool_call         → formatted name+args line with "tool_call" type
//   - tool_result       → content line with "tool_output" type
//     (error field prefixed when present)
//   - assistant_delta, turn_start, turn_end, token_usage, interrupt →
//     skipped. Deltas would visually duplicate the final
//     assistant_message; turn markers and token updates aren't chat
//     content; interrupts are handled via a separate HITL code path.
//
// Also flips chatFromEvents[agent] to true on the FIRST chat-content
// event, which activates PTY-suppression in the streamBatchMsg
// handler so the user doesn't see the same text twice.
func (a *App) readEventStream(agent string, es *EventStream) tea.Cmd {
	return func() tea.Msg {
		var lines, lineTypes []string
		var sawStart, sawEnd bool
		ch := es.Events()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					// Channel closed — flush whatever we have + any
					// turn-boundary signals we observed so the thinking
					// indicator flips correctly before the agent goes away.
					if len(lines) > 0 || sawStart || sawEnd {
						return streamBatchMsg{
							agent:        agent,
							lines:        lines,
							lineTypes:    lineTypes,
							fromSidecar:  true,
							sawTurnStart: sawStart,
							sawTurnEnd:   sawEnd,
						}
					}
					return nil
				}
				// Track turn boundaries separately from renderable content.
				// These don't produce a line but flip the in-flight flag
				// that drives the thinking indicator.
				switch ev.Kind {
				case sidecar.KindTurnStart:
					sawStart = true
				case sidecar.KindTurnEnd:
					sawEnd = true
				}
				line, kind, render := eventToLine(ev)
				if render {
					lines = append(lines, line)
					lineTypes = append(lineTypes, kind)
					if len(lines) >= 100 {
						return streamBatchMsg{
							agent:        agent,
							lines:        lines,
							lineTypes:    lineTypes,
							fromSidecar:  true,
							sawTurnStart: sawStart,
							sawTurnEnd:   sawEnd,
						}
					}
				}
			default:
				if len(lines) > 0 || sawStart || sawEnd {
					return streamBatchMsg{
						agent:        agent,
						lines:        lines,
						lineTypes:    lineTypes,
						fromSidecar:  true,
						sawTurnStart: sawStart,
						sawTurnEnd:   sawEnd,
					}
				}
				return nil
			}
		}
	}
}

// eventToLine converts a sidecar.Event into a (text, line_type) pair
// matching the existing line classification. Returns render=false for
// kinds that shouldn't appear in the chat viewport.
//
// The formatting here is the contract between the producer (Python
// sidecar_emitter) and the visual layer (TUI). Keep it deliberately
// simple: plain content for text, name only for tool calls (args
// available in data but too noisy for default display), content for
// tool results. The existing LineRenderer handles color/icon/prefix
// based on line_type.
func eventToLine(ev sidecar.Event) (line, lineType string, render bool) {
	switch ev.Kind {
	case sidecar.KindAssistantMessage:
		d, err := ev.AsAssistantMessage()
		if err != nil || strings.TrimSpace(d.Content) == "" {
			return "", "", false
		}
		return d.Content, "text", true

	case sidecar.KindToolCall:
		d, err := ev.AsToolCall()
		if err != nil || d.Name == "" {
			return "", "", false
		}
		// Mirror the PTY "● <name>" shape LineRenderer already recognizes
		// as a tool_call prefix. Keeps coloring consistent regardless of
		// whether the line comes via PTY or sidecar.
		return fmt.Sprintf("● %s", d.Name), "tool_call", true

	case sidecar.KindToolResult:
		d, err := ev.AsToolResult()
		if err != nil {
			return "", "", false
		}
		body := d.Content
		if d.Error != "" {
			body = fmt.Sprintf("[error] %s", d.Error)
		}
		if body == "" {
			return "", "", false
		}
		return fmt.Sprintf("⎿ %s", body), "tool_output", true
	}

	// assistant_delta, turn_start, turn_end, token_usage, interrupt:
	// not rendered as lines. (Deltas would double-render the final
	// assistant_message; the others are control-plane.)
	return "", "", false
}

// isChatContentLineType returns true if a PTY-originated line_type is
// something the sidecar event stream replaces authoritatively. When
// chatFromEvents is set for an agent, the streamBatchMsg handler drops
// PTY batches of these types to avoid visible duplication and to honor
// the "don't show PTY if not needed" UX goal.
//
// Currently suppressed: every chat-facing line type. The sidecar
// delivers assistant text (text), tool calls (tool_call), tool
// results (tool_output), user echoes (user_input via turn_start), and
// thinking/spinner state (via turn_start/turn_end — the
// thinkingIndicator reads turnInFlight directly, so PTY "thinking"
// lines are no longer needed).
//
// The one type we keep: "system". Daemon-injected messages like
// "[daemon] merge queue error" or "📨 Message from daemon" come from
// the daemon itself, not the agent, and the sidecar doesn't emit
// them. Dropping them would hide operator signals. Unknown/empty
// line types also pass through — forward-compat if a new PTY type
// shows up before a sidecar event kind covers it.
func isChatContentLineType(lineType string) bool {
	switch lineType {
	case "text", "tool_call", "tool_output", "user_input", "thinking":
		return true
	}
	return false
}
