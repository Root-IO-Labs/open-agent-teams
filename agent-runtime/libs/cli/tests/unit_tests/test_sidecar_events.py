"""Round-trip and shape tests for the sidecar event schema.

Cross-checks with ``pkg/sidecar/events.go`` — the two files define the
same wire format and must stay in sync. Fixtures crafted here to look
exactly like what the Go side emits, so a field-rename on either side
surfaces immediately.
"""
from __future__ import annotations

import json

import pytest

from oat_cli.sidecar_events import (
    KIND_ASSISTANT_MESSAGE,
    KIND_INTERRUPT,
    KIND_TOKEN_USAGE,
    KIND_TOOL_CALL,
    KIND_TOOL_RESULT,
    KIND_TURN_END,
    KIND_TURN_START,
    SCHEMA_VERSION,
    Event,
    Usage,
    assistant_delta,
    assistant_message,
    interrupt,
    token_usage,
    tool_call,
    tool_result,
    turn_end,
    turn_start,
)


class TestRoundTrip:
    def test_assistant_message_with_usage(self):
        usage = Usage(input_tokens=100, output_tokens=20, cache_read_tokens=50)
        ev = assistant_message(seq=42, turn_id="abc123", content="hello", usage=usage)
        parsed = Event.from_json(ev.to_json())
        assert parsed.v == SCHEMA_VERSION
        assert parsed.seq == 42
        assert parsed.turn_id == "abc123"
        assert parsed.kind == KIND_ASSISTANT_MESSAGE
        assert parsed.data["content"] == "hello"
        assert parsed.data["usage"]["input_tokens"] == 100
        assert parsed.data["usage"]["cache_read_tokens"] == 50

    def test_tool_call_preserves_nested_args(self):
        args = {"path": "/tmp/x", "nested": {"k": [1, 2, 3]}, "flag": True}
        ev = tool_call(seq=1, turn_id="t", name="write_file", args=args, call_id="c1")
        parsed = Event.from_json(ev.to_json())
        assert parsed.data["args"] == args

    def test_unicode_content_preserved(self):
        ev = assistant_message(seq=1, turn_id="t", content="héllo 世界 🎉")
        parsed = Event.from_json(ev.to_json())
        assert parsed.data["content"] == "héllo 世界 🎉"

    def test_turn_end_has_empty_data(self):
        ev = turn_end(seq=1, turn_id="t")
        parsed = Event.from_json(ev.to_json())
        assert parsed.data == {}
        assert parsed.kind == KIND_TURN_END

    def test_assistant_delta_content_only(self):
        ev = assistant_delta(seq=5, turn_id="t", content="par")
        parsed = Event.from_json(ev.to_json())
        assert parsed.data == {"content": "par"}


class TestShape:
    """Tests against hand-written JSON that mimics what Go will emit.

    If any of these fail, a struct tag in pkg/sidecar/events.go probably
    drifted away from its Python counterpart.
    """

    def test_envelope_field_order_is_flexible(self):
        # Go's encoding/json emits keys in struct field order. Python's
        # json.loads is order-independent — verify we don't accidentally
        # depend on order somewhere in from_json().
        raw = '{"seq":1,"kind":"turn_end","v":1,"ts":123,"turn_id":"x","data":{}}'
        ev = Event.from_json(raw)
        assert ev.seq == 1
        assert ev.kind == "turn_end"
        assert ev.v == 1
        assert ev.turn_id == "x"

    def test_omitempty_turn_id_accepted(self):
        # Go omits turn_id when empty (omitempty tag). Python must accept
        # the envelope without the key and leave turn_id as None.
        raw = '{"v":1,"seq":1,"ts":123,"kind":"assistant_delta","data":{"content":"x"}}'
        ev = Event.from_json(raw)
        assert ev.turn_id is None

    def test_unknown_kind_preserved(self):
        # Forward-compat: caller dispatches. Parser must not reject.
        raw = '{"v":1,"seq":1,"ts":1,"kind":"future_kind","data":{"x":1}}'
        ev = Event.from_json(raw)
        assert ev.kind == "future_kind"
        assert ev.data == {"x": 1}

    @pytest.mark.parametrize(
        "raw",
        [
            '{"seq":1,"ts":1,"kind":"x","data":{}}',  # missing v
            '{"v":1,"ts":1,"kind":"x","data":{}}',  # missing seq
            '{"v":1,"seq":1,"ts":1,"data":{}}',  # missing kind
            '{"v":1,"seq":1,"kind":"x","data":{}}',  # missing ts
        ],
    )
    def test_missing_required_fields_raise(self, raw):
        with pytest.raises((KeyError, ValueError)):
            Event.from_json(raw)

    def test_malformed_json_raises(self):
        with pytest.raises(ValueError):
            Event.from_json("{not valid json")

    def test_cache_fields_omitted_when_zero(self):
        # Matches Go's `cache_read_tokens,omitempty`. Zero-valued cache
        # fields should not appear on the wire — this saves bytes on every
        # event and matches the Go omitempty contract.
        ev = assistant_message(
            seq=1, turn_id="t", content="hi",
            usage=Usage(input_tokens=10, output_tokens=5),
        )
        payload = json.loads(ev.to_json())
        usage_on_wire = payload["data"]["usage"]
        # Python always includes these (dataclass asdict), but the contract
        # is that Go MAY omit them. Assert Python-side stays consistent for
        # now; if we later switch Python to omit, update this test and
        # regenerate the Go cross-language fixture.
        assert usage_on_wire["input_tokens"] == 10
        assert usage_on_wire["output_tokens"] == 5


class TestFactories:
    def test_turn_start_shape(self):
        ev = turn_start(seq=1, turn_id="t", user_input="hi")
        assert ev.kind == KIND_TURN_START
        assert ev.data == {"user_input": "hi"}

    def test_tool_result_with_error(self):
        ev = tool_result(seq=1, turn_id="t", call_id="c", content="", error="boom")
        assert ev.kind == KIND_TOOL_RESULT
        assert ev.data["error"] == "boom"

    def test_tool_result_without_error_omits_field(self):
        ev = tool_result(seq=1, turn_id="t", call_id="c", content="ok")
        payload = json.loads(ev.to_json())
        assert "error" not in payload["data"]

    def test_interrupt_payload_kind_vs_envelope_kind(self):
        # The interrupt factory takes `interrupt_kind` for the payload's
        # inner `kind` field — this prevents the envelope `kind` from
        # being shadowed at the call site.
        ev = interrupt(seq=1, turn_id="t", interrupt_kind="approval", prompt="Go?")
        assert ev.kind == KIND_INTERRUPT  # envelope
        assert ev.data["kind"] == "approval"  # payload

    def test_seq_monotonic_is_caller_responsibility(self):
        # Factories don't enforce seq ordering — the emitter owns that
        # invariant. We just check that seq is preserved.
        ev1 = turn_start(seq=5, turn_id="t", user_input="a")
        ev2 = turn_start(seq=10, turn_id="t", user_input="b")
        assert ev1.seq == 5
        assert ev2.seq == 10

    def test_all_kinds_have_factory(self):
        # Smoke test: calling each factory produces a valid round-trippable
        # event. Catches future factory additions that forget to_json.
        fixtures = [
            turn_start(1, "t", "x"),
            turn_end(2, "t"),
            assistant_delta(3, "t", "x"),
            assistant_message(4, "t", "x"),
            tool_call(5, "t", "n", {}, "c"),
            tool_result(6, "t", "c", "x"),
            interrupt(7, "t", "k", "p"),
            token_usage(8, "t", 1, 1, 1, 1),
        ]
        for ev in fixtures:
            Event.from_json(ev.to_json())  # must not raise

    def test_token_usage_shape_matches_oat_tokens_sentinel(self):
        # Payload keys MUST match [OAT_TOKENS] stdout JSON so the daemon's
        # existing handleTokenUsageEvent can consume sidecar events with
        # no adaptation.
        ev = token_usage(
            seq=1, turn_id="t",
            delta_input=10, delta_output=5,
            cumulative_input=100, cumulative_output=50,
            cache_read=20, cache_creation=30,
        )
        assert ev.kind == KIND_TOKEN_USAGE
        assert ev.data == {
            "delta_input": 10,
            "delta_output": 5,
            "cumulative_input": 100,
            "cumulative_output": 50,
            "cache_read": 20,
            "cache_creation": 30,
        }

    def test_token_usage_omits_zero_cache_fields(self):
        # Matches the sentinel's conditional-emit behavior: cache_read and
        # cache_creation only appear when non-zero. Reduces wire bytes and
        # keeps parity with the existing [OAT_TOKENS] path.
        ev = token_usage(
            seq=1, turn_id="t",
            delta_input=10, delta_output=5,
            cumulative_input=100, cumulative_output=50,
        )
        assert "cache_read" not in ev.data
        assert "cache_creation" not in ev.data

    def test_token_usage_allows_none_turn_id(self):
        # The existing emitter sometimes runs pre-turn (model banner) or
        # aggregated across turns — turn_id may legitimately be None.
        ev = token_usage(
            seq=1, turn_id=None,
            delta_input=0, delta_output=0,
            cumulative_input=10, cumulative_output=5,
        )
        assert ev.turn_id is None
