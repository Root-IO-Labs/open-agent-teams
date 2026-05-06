package sidecar

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseEvent_AssistantMessage parses a line crafted to match what
// sidecar_events.py's Event.to_json() would produce for an assistant
// message carrying usage. This is the cross-language parity check at
// the envelope level.
func TestParseEvent_AssistantMessage(t *testing.T) {
	// Exact Python-side output: keys in {v,seq,ts,kind,data,turn_id} order,
	// separators (",", ":"), ensure_ascii=False.
	line := `{"v":1,"seq":42,"ts":1714000000000,"kind":"assistant_message",` +
		`"data":{"content":"hello world","usage":{"input_tokens":100,` +
		`"output_tokens":20,"cache_read_tokens":50,"cache_creation_tokens":0}},` +
		`"turn_id":"abc123"}`

	ev, err := ParseEvent([]byte(line))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.V != 1 {
		t.Errorf("V = %d, want 1", ev.V)
	}
	if ev.Seq != 42 {
		t.Errorf("Seq = %d, want 42", ev.Seq)
	}
	if ev.Kind != KindAssistantMessage {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindAssistantMessage)
	}
	if ev.TurnID != "abc123" {
		t.Errorf("TurnID = %q, want %q", ev.TurnID, "abc123")
	}

	d, err := ev.AsAssistantMessage()
	if err != nil {
		t.Fatalf("AsAssistantMessage: %v", err)
	}
	if d.Content != "hello world" {
		t.Errorf("Content = %q", d.Content)
	}
	if d.Usage == nil {
		t.Fatal("Usage is nil, expected non-nil")
	}
	if d.Usage.InputTokens != 100 || d.Usage.OutputTokens != 20 ||
		d.Usage.CacheReadTokens != 50 {
		t.Errorf("Usage = %+v", *d.Usage)
	}
}

func TestParseEvent_ToolCallPreservesArgs(t *testing.T) {
	// Verify nested JSON in Args (a dict in Python) round-trips as raw bytes.
	line := `{"v":1,"seq":1,"ts":1,"kind":"tool_call",` +
		`"data":{"name":"write_file","args":{"path":"/tmp/x","nested":{"k":[1,2]}},` +
		`"call_id":"c1"}}`

	ev, err := ParseEvent([]byte(line))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	d, err := ev.AsToolCall()
	if err != nil {
		t.Fatalf("AsToolCall: %v", err)
	}
	if d.Name != "write_file" || d.CallID != "c1" {
		t.Errorf("unexpected: %+v", d)
	}
	// Args comes back as RawMessage — verify it parses into a map cleanly.
	var args map[string]any
	if err := json.Unmarshal(d.Args, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args["path"] != "/tmp/x" {
		t.Errorf("args[path] = %v", args["path"])
	}
}

func TestParseEvent_UnknownKindPreserved(t *testing.T) {
	// Forward-compat contract: unknown kinds parse successfully and
	// dispatch is the consumer's responsibility. The envelope's Data
	// stays intact so it can be logged for debugging.
	line := `{"v":1,"seq":7,"ts":1,"kind":"future_kind","data":{"arbitrary":"payload"}}`
	ev, err := ParseEvent([]byte(line))
	if err != nil {
		t.Fatalf("ParseEvent unexpectedly failed for unknown kind: %v", err)
	}
	if ev.Kind != "future_kind" {
		t.Errorf("Kind = %q, want preserved", ev.Kind)
	}
	// Verify Data round-trips as expected for future consumers.
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data["arbitrary"] != "payload" {
		t.Errorf("data = %v", data)
	}
}

func TestParseEvent_MissingRequired(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"empty", ``},
		{"invalid_json", `{`},
		{"missing_v", `{"seq":1,"ts":1,"kind":"turn_end","data":{}}`},
		{"missing_kind", `{"v":1,"seq":1,"ts":1,"data":{}}`},
		{"v_is_zero", `{"v":0,"seq":1,"ts":1,"kind":"x","data":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEvent([]byte(tc.line)); err == nil {
				t.Errorf("expected error for %q", tc.line)
			}
		})
	}
}

func TestParseEvent_OptionalTurnID(t *testing.T) {
	// Pre-turn events (model banner, heartbeat) omit turn_id. The envelope
	// must still parse, with an empty TurnID string.
	line := `{"v":1,"seq":1,"ts":1,"kind":"assistant_delta","data":{"content":"x"}}`
	ev, err := ParseEvent([]byte(line))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.TurnID != "" {
		t.Errorf("TurnID = %q, want empty", ev.TurnID)
	}
}

// TestMarshalEvent_ShapeMatchesPython verifies a Go-produced envelope
// deserializes into the same field set Python's Event.from_json() would
// see. This catches struct-tag typos (the classic "turn_id" vs "turnId"
// bug) before it hits the wire.
func TestMarshalEvent_ShapeMatchesPython(t *testing.T) {
	usage := &Usage{InputTokens: 10, OutputTokens: 5}
	dataBytes, err := json.Marshal(AssistantMessageData{Content: "hi", Usage: usage})
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	ev := Event{
		V: SchemaVersion, Seq: 1, TS: 1714000000000,
		Kind: KindAssistantMessage, TurnID: "t1", Data: dataBytes,
	}
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	s := string(out)

	// These are the exact substrings Python's from_json() keys on. If any
	// is missing, the Go side renamed a field and parity is broken.
	wantSubs := []string{
		`"v":1`,
		`"seq":1`,
		`"ts":1714000000000`,
		`"kind":"assistant_message"`,
		`"turn_id":"t1"`,
		`"content":"hi"`,
		`"input_tokens":10`,
		`"output_tokens":5`,
	}
	for _, want := range wantSubs {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}

	// cache_* fields should be omitted when zero (matches Python's
	// omit-if-zero for nested fields via Usage default).
	if strings.Contains(s, "cache_read_tokens") {
		t.Errorf("cache_read_tokens should be omitted when 0:\n%s", s)
	}
}

// TestTokenUsage_ShapeMatchesOatTokensSentinel verifies the token_usage
// payload is byte-compatible with the existing [OAT_TOKENS] stdout JSON
// the daemon already parses today. If this test breaks, feeding
// handleTokenUsageEvent from the sidecar will also break.
func TestTokenUsage_ShapeMatchesOatTokensSentinel(t *testing.T) {
	// Reference shape: what _emit_oat_tokens currently prints.
	// https://git.example/textual_adapter.py:1171 — "delta_input",
	// "delta_output", "cumulative_input", "cumulative_output", with
	// optional "cache_read" and "cache_creation".
	d := TokenUsageData{
		DeltaInput: 10, DeltaOutput: 5,
		CumulativeInput: 100, CumulativeOutput: 50,
		CacheRead: 20, CacheCreation: 30,
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"delta_input":10`,
		`"delta_output":5`,
		`"cumulative_input":100`,
		`"cumulative_output":50`,
		`"cache_read":20`,
		`"cache_creation":30`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}

	// With cache fields at 0, omitempty must suppress them — the daemon
	// today only emits cache_* lines conditionally.
	d2 := TokenUsageData{DeltaInput: 1, DeltaOutput: 1, CumulativeInput: 1, CumulativeOutput: 1}
	out2, _ := json.Marshal(d2)
	s2 := string(out2)
	if strings.Contains(s2, "cache_read") || strings.Contains(s2, "cache_creation") {
		t.Errorf("cache fields must omit when 0:\n%s", s2)
	}
}

func TestMarshalEvent_TurnIDOmittedWhenEmpty(t *testing.T) {
	// turn_id has `omitempty` — if we don't set it, it must not appear on
	// the wire, otherwise Python sees {"turn_id": ""} instead of the
	// optional-absent semantics.
	ev := Event{V: 1, Seq: 1, TS: 1, Kind: KindTurnEnd, Data: json.RawMessage(`{}`)}
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "turn_id") {
		t.Errorf("turn_id must be omitted when empty:\n%s", out)
	}
}

// TestRoundTrip_AllKinds sanity-checks that each typed payload survives
// marshal → ParseEvent → As*() without losing fields. A broken payload
// struct tag shows up here immediately.
func TestRoundTrip_AllKinds(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		data   any
		verify func(t *testing.T, ev Event)
	}{
		{
			name: "turn_start", kind: KindTurnStart,
			data: TurnStartData{UserInput: "hello"},
			verify: func(t *testing.T, ev Event) {
				d, err := ev.AsTurnStart()
				if err != nil {
					t.Fatal(err)
				}
				if d.UserInput != "hello" {
					t.Errorf("UserInput = %q", d.UserInput)
				}
			},
		},
		{
			name: "tool_result_with_error", kind: KindToolResult,
			data: ToolResultData{CallID: "c1", Content: "", Error: "boom"},
			verify: func(t *testing.T, ev Event) {
				d, err := ev.AsToolResult()
				if err != nil {
					t.Fatal(err)
				}
				if d.Error != "boom" {
					t.Errorf("Error = %q", d.Error)
				}
			},
		},
		{
			name: "interrupt", kind: KindInterrupt,
			data: InterruptData{Kind: "approval", Prompt: "Proceed?"},
			verify: func(t *testing.T, ev Event) {
				d, err := ev.AsInterrupt()
				if err != nil {
					t.Fatal(err)
				}
				if d.Kind != "approval" || d.Prompt != "Proceed?" {
					t.Errorf("%+v", d)
				}
			},
		},
		{
			name: "token_usage", kind: KindTokenUsage,
			data: TokenUsageData{
				DeltaInput: 100, DeltaOutput: 20,
				CumulativeInput: 1000, CumulativeOutput: 200,
				CacheRead: 500,
			},
			verify: func(t *testing.T, ev Event) {
				d, err := ev.AsTokenUsage()
				if err != nil {
					t.Fatal(err)
				}
				if d.DeltaInput != 100 || d.DeltaOutput != 20 ||
					d.CumulativeInput != 1000 || d.CumulativeOutput != 200 ||
					d.CacheRead != 500 || d.CacheCreation != 0 {
					t.Errorf("%+v", d)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataBytes, err := json.Marshal(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			ev := Event{V: 1, Seq: 1, TS: 1, Kind: tc.kind, TurnID: "t",
				Data: dataBytes}
			out, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseEvent(out)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", parsed.Kind, tc.kind)
			}
			tc.verify(t, parsed)
		})
	}
}
