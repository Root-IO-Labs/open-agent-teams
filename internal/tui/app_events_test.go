package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// mkEvent is a local builder so tests stay compact.
func mkEvent(kind string, dataJSON string) sidecar.Event {
	return sidecar.Event{
		V: 1, Seq: 1, TS: 1, Kind: kind, TurnID: "t",
		Data: json.RawMessage(dataJSON),
	}
}

// Tests for eventToLine — the event → (text, line_type, render?) mapping
// that drives chat rendering from sidecar events.

func TestEventToLine_AssistantMessageRendersAsText(t *testing.T) {
	ev := mkEvent(sidecar.KindAssistantMessage, `{"content":"Hello, world!"}`)
	line, lineType, ok := eventToLine(ev)
	if !ok {
		t.Fatal("assistant_message should render")
	}
	if line != "Hello, world!" {
		t.Errorf("line = %q, want %q", line, "Hello, world!")
	}
	if lineType != "text" {
		t.Errorf("lineType = %q, want %q", lineType, "text")
	}
}

func TestEventToLine_EmptyAssistantMessageSkipped(t *testing.T) {
	// Whitespace-only messages are suppressed on the Python side too
	// (`if pending_text.strip()`). Mirror that here so the TUI never
	// shows a blank line.
	for _, body := range []string{
		`{"content":""}`,
		`{"content":"   "}`,
		`{"content":"\n\n"}`,
	} {
		ev := mkEvent(sidecar.KindAssistantMessage, body)
		_, _, ok := eventToLine(ev)
		if ok {
			t.Errorf("expected non-rendering for %s", body)
		}
	}
}

func TestEventToLine_ToolCallRendersWithPrefix(t *testing.T) {
	// "● <name>" is the visual prefix LineRenderer already recognizes
	// as tool_call — matches the PTY-scraped format so styling is
	// identical whether the source is sidecar or PTY.
	ev := mkEvent(sidecar.KindToolCall,
		`{"name":"read_file","args":{"path":"/tmp/x"},"call_id":"c1"}`)
	line, lineType, ok := eventToLine(ev)
	if !ok {
		t.Fatal("tool_call should render")
	}
	if !strings.HasPrefix(line, "●") {
		t.Errorf("line missing tool_call prefix: %q", line)
	}
	if !strings.Contains(line, "read_file") {
		t.Errorf("line missing tool name: %q", line)
	}
	if lineType != "tool_call" {
		t.Errorf("lineType = %q", lineType)
	}
}

func TestEventToLine_ToolResultUsesOutputType(t *testing.T) {
	ev := mkEvent(sidecar.KindToolResult,
		`{"call_id":"c1","content":"file contents here"}`)
	line, lineType, ok := eventToLine(ev)
	if !ok {
		t.Fatal("tool_result should render")
	}
	if !strings.HasPrefix(line, "⎿") {
		t.Errorf("line missing tool_output prefix: %q", line)
	}
	if !strings.Contains(line, "file contents here") {
		t.Errorf("line missing content: %q", line)
	}
	if lineType != "tool_output" {
		t.Errorf("lineType = %q", lineType)
	}
}

func TestEventToLine_ToolResultWithErrorMarkedAsError(t *testing.T) {
	ev := mkEvent(sidecar.KindToolResult,
		`{"call_id":"c1","content":"","error":"permission denied"}`)
	line, _, ok := eventToLine(ev)
	if !ok {
		t.Fatal("errored tool_result should render")
	}
	if !strings.Contains(line, "error") {
		t.Errorf("error marker missing from line: %q", line)
	}
	if !strings.Contains(line, "permission denied") {
		t.Errorf("error message missing: %q", line)
	}
}

func TestEventToLine_SkipsNonChatKinds(t *testing.T) {
	// Kinds that shouldn't reach the chat viewport: deltas (duplicate
	// the final message), turn boundaries (control-plane), tokens
	// (separate accounting), interrupts (HITL flow).
	for _, kind := range []string{
		sidecar.KindAssistantDelta,
		sidecar.KindTurnStart,
		sidecar.KindTurnEnd,
		sidecar.KindTokenUsage,
		sidecar.KindInterrupt,
	} {
		t.Run(kind, func(t *testing.T) {
			ev := mkEvent(kind, `{}`)
			_, _, ok := eventToLine(ev)
			if ok {
				t.Errorf("%s should not render as a chat line", kind)
			}
		})
	}
}

func TestEventToLine_MalformedDataSafelySkips(t *testing.T) {
	// Forward-compat: an event with invalid JSON payload for its kind
	// must not crash eventToLine — just return render=false.
	for _, kind := range []string{
		sidecar.KindAssistantMessage,
		sidecar.KindToolCall,
		sidecar.KindToolResult,
	} {
		ev := sidecar.Event{V: 1, Seq: 1, TS: 1, Kind: kind,
			Data: json.RawMessage(`not valid json`)}
		_, _, ok := eventToLine(ev)
		if ok {
			t.Errorf("%s with bad JSON should not render", kind)
		}
	}
}

// Tests for the PTY-suppression classifier — decides which PTY line
// types get dropped when the sidecar is authoritative for chat.

func TestIsChatContentLineType_ChatTypesSuppressed(t *testing.T) {
	// Every line type the sidecar replaces authoritatively — assistant
	// text, tool calls + results, user-input echoes (from turn_start),
	// and the thinking spinner (now driven by turnInFlight directly).
	for _, lt := range []string{"text", "tool_call", "tool_output", "user_input", "thinking"} {
		if !isChatContentLineType(lt) {
			t.Errorf("%q should be classified as chat content (gets suppressed)", lt)
		}
	}
}

func TestIsChatContentLineType_SystemAndUnknownPassThrough(t *testing.T) {
	// "system" carries daemon-injected messages ("[daemon] ...", merge-
	// queue events, notifications) — the sidecar doesn't emit these
	// and operators need to see them. Unknown/empty types pass through
	// as forward-compat for future PTY line classifications.
	for _, lt := range []string{"system", "", "unknown_future_type"} {
		if isChatContentLineType(lt) {
			t.Errorf("%q should NOT be suppressed (operator signal / forward-compat)", lt)
		}
	}
}
