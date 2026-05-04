"""Tests for the token tracking system (Path A + Path B).

Covers:
- TextualTokenTracker (Path A: context window, overwrite semantics)
- TokenSpendAccumulator (Path B: cumulative spend, accumulate semantics)
- _commit_token_tracking (commit logic)
- _emit_oat_tokens (IPC payload)
- _is_summarization_chunk (classification)
- Edge cases: no usage_metadata, zero values, interrupted requests
"""

import json
from unittest.mock import MagicMock, patch

import pytest

from deepagents_cli.app import TextualTokenTracker, TokenSpendAccumulator
from deepagents_cli.textual_adapter import (
    _commit_token_tracking,
    _emit_oat_tokens,
    _is_summarization_chunk,
)


# ---------------------------------------------------------------------------
# Path A: TextualTokenTracker (context window)
# ---------------------------------------------------------------------------


class TestTextualTokenTracker:
    def test_add_overwrites_context(self):
        """Each add() overwrites — does not accumulate."""
        called = []
        tracker = TextualTokenTracker(lambda x: called.append(x))

        tracker.add(1000)
        tracker.add(2000)

        assert tracker.current_context == 2000
        assert called == [1000, 2000]

    def test_reset_clears_context(self):
        called = []
        tracker = TextualTokenTracker(lambda x: called.append(x))
        tracker.add(1500)
        called.clear()

        tracker.reset()

        assert tracker.current_context == 0
        assert called == [0]

    def test_hide_and_show(self):
        hide_called = []
        called = []
        tracker = TextualTokenTracker(
            lambda x: called.append(x),
            hide_callback=lambda: hide_called.append(True),
        )
        tracker.add(500)
        called.clear()

        tracker.hide()
        assert hide_called == [True]

        tracker.show()
        assert called == [500]


# ---------------------------------------------------------------------------
# Path B: TokenSpendAccumulator (cumulative spend)
# ---------------------------------------------------------------------------


class TestTokenSpendAccumulator:
    def test_record_turn_accumulates(self):
        acc = TokenSpendAccumulator()
        acc.record_turn(100, 50)
        acc.record_turn(200, 80)

        assert acc.total_input == 300
        assert acc.total_output == 130
        assert acc.total == 430

    def test_starts_at_zero(self):
        acc = TokenSpendAccumulator()
        assert acc.total_input == 0
        assert acc.total_output == 0
        assert acc.total == 0

    def test_no_reset_method(self):
        """TokenSpendAccumulator intentionally has no reset method."""
        acc = TokenSpendAccumulator()
        assert not hasattr(acc, "reset")

    def test_zero_tokens_accepted(self):
        """record_turn(0, 0) is valid — provider reported zero usage."""
        acc = TokenSpendAccumulator()
        acc.record_turn(0, 0)
        assert acc.total == 0

        acc.record_turn(100, 50)
        assert acc.total == 150


# ---------------------------------------------------------------------------
# Path A vs Path B separation
# ---------------------------------------------------------------------------


class TestPathSeparation:
    def test_clear_resets_path_a_not_path_b(self):
        """Simulates /clear: Path A resets, Path B unchanged."""
        path_a = TextualTokenTracker(lambda _: None)
        path_b = TokenSpendAccumulator()

        # Simulate a turn
        path_a.add(1000)
        path_b.record_turn(800, 200)

        # /clear
        path_a.reset()

        assert path_a.current_context == 0
        assert path_b.total_input == 800
        assert path_b.total_output == 200
        assert path_b.total == 1000


# ---------------------------------------------------------------------------
# _commit_token_tracking
# ---------------------------------------------------------------------------


class TestCommitTokenTracking:
    def _make_adapter(self):
        adapter = MagicMock()
        adapter._token_tracker = TextualTokenTracker(lambda _: None)
        adapter._spend_tracker = TokenSpendAccumulator()
        return adapter

    def test_normal_commit(self):
        adapter = self._make_adapter()
        _commit_token_tracking(adapter, 800, 200, 900, 250)

        assert adapter._token_tracker.current_context == 1000  # 800+200
        assert adapter._spend_tracker.total_input == 900
        assert adapter._spend_tracker.total_output == 250

    def test_no_context_restores_display(self):
        """When no main-agent context captured, show() restores previous value."""
        adapter = self._make_adapter()
        adapter._token_tracker.add(500)  # previous value

        _commit_token_tracking(adapter, 0, 0, 100, 50)

        # Path A: restored to 500 (show, not overwrite)
        assert adapter._token_tracker.current_context == 500
        # Path B: still accumulated
        assert adapter._spend_tracker.total == 150

    def test_no_spend_skips_emit(self):
        """When no spend captured, no emission to daemon."""
        adapter = self._make_adapter()
        with patch(
            "deepagents_cli.textual_adapter._emit_oat_tokens"
        ) as mock_emit:
            _commit_token_tracking(adapter, 0, 0, 0, 0)
            mock_emit.assert_not_called()

    def test_summarization_spend_without_context(self):
        """Summarization-only turn: Path B gets spend, Path A unchanged."""
        adapter = self._make_adapter()
        adapter._token_tracker.add(2000)  # prior context

        # Summarization contributed to spend but not context
        _commit_token_tracking(adapter, 0, 0, 500, 100)

        assert adapter._token_tracker.current_context == 2000  # unchanged
        assert adapter._spend_tracker.total == 600

    def test_interrupted_request_counts_spend(self):
        """Interrupted/failed work still accumulates in Path B."""
        adapter = self._make_adapter()

        # Simulate partial work before interrupt
        _commit_token_tracking(adapter, 300, 100, 300, 100)

        assert adapter._spend_tracker.total_input == 300
        assert adapter._spend_tracker.total_output == 100


# ---------------------------------------------------------------------------
# _emit_oat_tokens (IPC payload)
# ---------------------------------------------------------------------------


class TestEmitOatTokens:
    def _capture_emit(self, adapter, delta_in, delta_out):
        """Call _emit_oat_tokens and capture its output.

        _emit_oat_tokens writes to sys.__stdout__ to bypass Textual's
        redirection. We temporarily replace __stdout__ with a StringIO
        to capture the output.
        """
        import io
        import sys

        buf = io.StringIO()
        old = getattr(sys, "__stdout__", sys.stdout)
        sys.__stdout__ = buf
        try:
            _emit_oat_tokens(adapter, delta_in, delta_out)
        finally:
            sys.__stdout__ = old
        return buf.getvalue().strip()

    def test_payload_structure(self):
        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._spend_tracker.record_turn(800, 200)

        line = self._capture_emit(adapter, 400, 100)
        assert line.startswith("[OAT_TOKENS] ")
        payload = json.loads(line[len("[OAT_TOKENS] "):])

        assert payload["delta_input"] == 400
        assert payload["delta_output"] == 100
        assert payload["cumulative_input"] == 800
        assert payload["cumulative_output"] == 200

    def test_no_spend_tracker_emits_zeros(self):
        adapter = MagicMock()
        adapter._spend_tracker = None

        line = self._capture_emit(adapter, 100, 50)
        payload = json.loads(line[len("[OAT_TOKENS] "):])
        assert payload["cumulative_input"] == 0
        assert payload["cumulative_output"] == 0

    def test_no_old_field_names(self):
        """Old field names (input, output, total, cumulative_input as alias) are gone."""
        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()

        line = self._capture_emit(adapter, 100, 50)
        payload = json.loads(line[len("[OAT_TOKENS] "):])
        # Only new honest field names
        assert set(payload.keys()) == {
            "delta_input",
            "delta_output",
            "cumulative_input",
            "cumulative_output",
        }

    def test_writes_directly_to_oat_tool_log(self, tmp_path, monkeypatch):
        """When OAT_TOOL_LOG is set, [OAT_TOKENS] is appended to that file.

        The direct-backend ``hasToolLog`` guard suppresses PTY capture to
        the log file whenever OAT_TOOL_LOG is set. Without this direct
        write, the daemon's OutputWatcher (which tails the log file)
        would never see token lines. Lock the behavior in.
        """
        log_file = tmp_path / "agent.log"
        log_file.write_text("")  # pre-existing content is preserved (append)
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._spend_tracker.record_turn(500, 120)

        self._capture_emit(adapter, 500, 120)

        contents = log_file.read_text().splitlines()
        assert len(contents) == 1
        assert contents[0].startswith("[OAT_TOKENS] ")
        payload = json.loads(contents[0][len("[OAT_TOKENS] "):])
        assert payload["cumulative_input"] == 500

    def test_oat_tool_log_missing_is_noop(self, tmp_path, monkeypatch):
        """Unset OAT_TOOL_LOG must not crash; stdout emission still works."""
        monkeypatch.delenv("OAT_TOOL_LOG", raising=False)

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()

        line = self._capture_emit(adapter, 10, 5)
        assert line.startswith("[OAT_TOKENS] ")

    def test_oat_tool_log_write_error_is_swallowed(self, tmp_path, monkeypatch):
        """Unwritable OAT_TOOL_LOG must not raise — stdout path is primary."""
        bad_path = tmp_path / "nonexistent-dir" / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(bad_path))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()

        # Should not raise
        line = self._capture_emit(adapter, 10, 5)
        assert line.startswith("[OAT_TOKENS] ")

    def test_cache_fields_emitted_when_nonzero(self):
        """Anthropic-style cache metrics are surfaced in the payload."""
        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._spend_tracker.record_turn(1000, 200, cache_read=700, cache_creation=100)

        line = self._capture_emit(adapter, 1000, 200)
        payload = json.loads(line[len("[OAT_TOKENS] "):])
        assert payload["cache_read"] == 700
        assert payload["cache_creation"] == 100

    def test_cache_fields_omitted_when_zero(self):
        """Non-caching providers keep payloads compact."""
        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._spend_tracker.record_turn(1000, 200)

        line = self._capture_emit(adapter, 1000, 200)
        payload = json.loads(line[len("[OAT_TOKENS] "):])
        assert "cache_read" not in payload
        assert "cache_creation" not in payload


# ---------------------------------------------------------------------------
# _is_summarization_chunk
# ---------------------------------------------------------------------------


class TestIsSummarizationChunk:
    def test_summarization_detected(self):
        assert _is_summarization_chunk({"lc_source": "summarization"}) is True

    def test_normal_chunk_not_summarization(self):
        assert _is_summarization_chunk({"langgraph_node": "model"}) is False

    def test_none_metadata(self):
        assert _is_summarization_chunk(None) is False

    def test_empty_metadata(self):
        assert _is_summarization_chunk({}) is False


# ---------------------------------------------------------------------------
# Provider edge cases
# ---------------------------------------------------------------------------


class TestProviderEdgeCases:
    def test_no_usage_metadata_no_crash(self):
        """Provider emits no usage_metadata → no crash, Path B unchanged."""
        acc = TokenSpendAccumulator()
        tracker = TextualTokenTracker(lambda _: None)

        # Simulate: no usage captured at all (provider didn't report)
        _adapter = MagicMock()
        _adapter._token_tracker = tracker
        _adapter._spend_tracker = acc

        _commit_token_tracking(_adapter, 0, 0, 0, 0)

        assert acc.total == 0
        assert tracker.current_context == 0

    def test_zero_input_tokens_recorded_correctly(self):
        """usage_metadata with input_tokens=0 records 0, not previous value."""
        acc = TokenSpendAccumulator()
        acc.record_turn(500, 200)

        # New turn with 0 input (e.g., cached response)
        acc.record_turn(0, 100)

        assert acc.total_input == 500  # not overwritten, accumulated
        assert acc.total_output == 300

    def test_multi_invocation_accumulation(self):
        """Request with summarization + main response counts both in Path B."""
        acc = TokenSpendAccumulator()

        # Simulate extraction: summarization spend = 500 input, 100 output
        # Main model spend = 800 input, 200 output
        # Both contribute to spend_*_delta via += in the extraction loop
        total_spend_input = 500 + 800
        total_spend_output = 100 + 200

        acc.record_turn(total_spend_input, total_spend_output)

        assert acc.total_input == 1300
        assert acc.total_output == 300
        assert acc.total == 1600
