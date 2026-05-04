// Package sidecar defines the wire schema for events emitted by the Python
// agent over the astream sidecar socket. Both sides must stay in sync with
// agent-runtime/libs/cli/deepagents_cli/sidecar_events.py — change one,
// change the other, add tests on both sides.
//
// Design notes:
//
//   - Envelope is generic: V, Seq, TS, Kind, TurnID, Data. Data is held as
//     json.RawMessage so unknown kinds parse successfully and the consumer
//     can log+skip instead of crashing. This is the forward-compat contract.
//
//   - Seq is monotonic per-connection (not per-turn). The receiver should
//     detect gaps and log them; gaps mean the Python emitter dropped events
//     from its bounded queue under backpressure.
//
//   - Typed payload structs (TurnStartData, AssistantMessageData, etc.) are
//     unmarshalled lazily via the As*() methods on Event.
package sidecar

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is the current wire-schema major version. Every envelope
// MUST include this field. Consumers should log+skip envelopes whose V
// differs from what they support (future migration path).
const SchemaVersion = 1

// Event kind constants — mirror sidecar_events.py. Keep alphabetized
// beyond the turn-boundary pair.
const (
	KindTurnStart        = "turn_start"
	KindTurnEnd          = "turn_end"
	KindAssistantDelta   = "assistant_delta"
	KindAssistantMessage = "assistant_message"
	KindToolCall         = "tool_call"
	KindToolResult       = "tool_result"
	KindInterrupt        = "interrupt"
	KindTokenUsage       = "token_usage"
)

// Event is the on-wire envelope. Data is kept as json.RawMessage so the
// caller can dispatch on Kind and parse into the correct payload type.
// Unknown kinds are preserved for logging instead of crashing the parser.
type Event struct {
	V      int             `json:"v"`
	Seq    uint64          `json:"seq"`
	TS     int64           `json:"ts"`
	TurnID string          `json:"turn_id,omitempty"`
	Kind   string          `json:"kind"`
	Data   json.RawMessage `json:"data"`
}

// Usage mirrors Python's Usage dataclass. The cache-* fields are omitempty
// to match Python's default of 0 when no cache was used.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

// --- payload types, one per Kind ---

type TurnStartData struct {
	UserInput string `json:"user_input"`
}

// TurnEndData is intentionally empty — the envelope's TurnID carries the
// correlation, and a turn is "over" when this event arrives.
type TurnEndData struct{}

type AssistantDeltaData struct {
	Content string `json:"content"`
}

type AssistantMessageData struct {
	Content string `json:"content"`
	Usage   *Usage `json:"usage,omitempty"`
}

type ToolCallData struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	CallID string          `json:"call_id"`
}

type ToolResultData struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

type InterruptData struct {
	Kind   string `json:"kind"`
	Prompt string `json:"prompt"`
}

// TokenUsageData mirrors the payload of the existing [OAT_TOKENS] stdout
// sentinel 1:1 so the daemon's handleTokenUsageEvent can be fed from a
// sidecar source with no adaptation. See sidecar_events.py:token_usage
// for the emission-side rationale. CacheRead / CacheCreation use
// omitempty to match the sentinel's conditional-emit behavior.
type TokenUsageData struct {
	DeltaInput       int `json:"delta_input"`
	DeltaOutput      int `json:"delta_output"`
	CumulativeInput  int `json:"cumulative_input"`
	CumulativeOutput int `json:"cumulative_output"`
	CacheRead        int `json:"cache_read,omitempty"`
	CacheCreation    int `json:"cache_creation,omitempty"`
}

// ParseEvent unmarshals one JSON line into an Event envelope.
//
// Returns an error for malformed JSON or a missing required field
// (v, seq, kind). Does NOT validate Kind — unknown kinds succeed here
// and dispatch happens one layer up. This preserves the forward-compat
// contract stated at the top of the file.
func ParseEvent(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, fmt.Errorf("sidecar: parse envelope: %w", err)
	}
	if ev.V == 0 {
		return ev, fmt.Errorf("sidecar: envelope missing required field v")
	}
	if ev.Kind == "" {
		return ev, fmt.Errorf("sidecar: envelope missing required field kind")
	}
	return ev, nil
}

// --- typed payload accessors — use after dispatching on Event.Kind ---

func (e Event) AsTurnStart() (TurnStartData, error) {
	var d TurnStartData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsTurnEnd() (TurnEndData, error) {
	var d TurnEndData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsAssistantDelta() (AssistantDeltaData, error) {
	var d AssistantDeltaData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsAssistantMessage() (AssistantMessageData, error) {
	var d AssistantMessageData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsToolCall() (ToolCallData, error) {
	var d ToolCallData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsToolResult() (ToolResultData, error) {
	var d ToolResultData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsInterrupt() (InterruptData, error) {
	var d InterruptData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}

func (e Event) AsTokenUsage() (TokenUsageData, error) {
	var d TokenUsageData
	err := json.Unmarshal(e.Data, &d)
	return d, err
}
