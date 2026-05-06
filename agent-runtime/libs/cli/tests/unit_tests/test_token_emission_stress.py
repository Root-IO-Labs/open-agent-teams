"""End-to-end stress and accuracy tests for [OAT_TOKENS] emission.

Exercises the full path from usage_metadata → SpendTracker →
_commit_token_tracking → _emit_oat_tokens → OAT_TOOL_LOG file,
verifying that what the daemon will actually read is accurate under:

- single-turn baseline
- multi-turn tool chains (cumulative correctness)
- cache metric flow (Anthropic-style input_token_details)
- rapid-fire emissions (1000 sequential writes)
- concurrent emissions (POSIX O_APPEND atomicity)
- mixed caching / non-caching turns
"""

import io
import json
import sys
import threading
from pathlib import Path
from unittest.mock import MagicMock

from oat_cli.app import TokenSpendAccumulator
from oat_cli.textual_adapter import _commit_token_tracking, _emit_oat_tokens


def _read_token_payloads(log_file: Path) -> list[dict]:
    """Parse [OAT_TOKENS] lines from a log file into payload dicts."""
    if not log_file.exists():
        return []
    return [
        json.loads(line[len("[OAT_TOKENS] "):])
        for line in log_file.read_text(encoding="utf-8").splitlines()
        if line.startswith("[OAT_TOKENS] ")
    ]


def _suppress_stdout() -> "callable":
    """Redirect __stdout__ to an in-memory buffer; return restore callable."""
    buf = io.StringIO()
    old = sys.__stdout__
    sys.__stdout__ = buf
    del buf
    return lambda: setattr(sys, "__stdout__", old)


class TestSequentialAccuracy:
    def test_single_turn_matches_input(self, tmp_path, monkeypatch):
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        restore = _suppress_stdout()
        try:
            _commit_token_tracking(adapter, 1000, 50, 1000, 50)
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == 1
        p = payloads[0]
        assert p["delta_input"] == 1000
        assert p["delta_output"] == 50
        assert p["cumulative_input"] == 1000
        assert p["cumulative_output"] == 50

    def test_tool_chain_cumulative_matches_sum_of_deltas(self, tmp_path, monkeypatch):
        """Multi-turn chain: final cumulative must equal sum of all deltas."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        # Simulates an agent that ran through 5 model-tool turns.
        # Each tuple is (delta_in, delta_out) for one turn.
        turns = [
            (1000, 50),
            (1200, 100),
            (1500, 120),
            (2000, 200),
            (1800, 150),
        ]
        expected_cum_in = sum(t[0] for t in turns)
        expected_cum_out = sum(t[1] for t in turns)

        restore = _suppress_stdout()
        try:
            for delta_in, delta_out in turns:
                _commit_token_tracking(adapter, delta_in, delta_out, delta_in, delta_out)
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == len(turns)

        # Final line (what daemon treats as authoritative) matches the sum.
        final = payloads[-1]
        assert final["cumulative_input"] == expected_cum_in
        assert final["cumulative_output"] == expected_cum_out

        # Monotonic: cumulative never decreases between turns.
        for i in range(1, len(payloads)):
            assert payloads[i]["cumulative_input"] >= payloads[i - 1]["cumulative_input"]
            assert payloads[i]["cumulative_output"] >= payloads[i - 1]["cumulative_output"]

        # Each line's cumulative_input equals sum of deltas up to and including that turn.
        running_in = 0
        for i, (delta_in, _) in enumerate(turns):
            running_in += delta_in
            assert payloads[i]["cumulative_input"] == running_in, (
                f"turn {i}: expected cum_in={running_in}, got {payloads[i]['cumulative_input']}"
            )


class TestCacheFlow:
    def test_cache_metrics_accumulate_across_turns(self, tmp_path, monkeypatch):
        """Cache_read and cache_creation must sum across all emissions."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        # (delta_in, delta_out, cache_read, cache_creation)
        turns = [
            (1000, 50, 0, 800),    # turn 1: creates cache
            (1200, 100, 700, 0),   # turn 2: cache hit
            (1500, 120, 1000, 0),  # turn 3: bigger cache hit
            (2000, 200, 1500, 400),  # turn 4: partial refresh
            (1800, 150, 1700, 0),  # turn 5: full hit
        ]
        expected_cum_cache_read = sum(t[2] for t in turns)
        expected_cum_cache_creation = sum(t[3] for t in turns)

        restore = _suppress_stdout()
        try:
            for delta_in, delta_out, cache_read, cache_creation in turns:
                _commit_token_tracking(
                    adapter, delta_in, delta_out, delta_in, delta_out,
                    cache_read, cache_creation,
                )
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == len(turns)

        final = payloads[-1]
        assert final["cache_read"] == expected_cum_cache_read
        assert final["cache_creation"] == expected_cum_cache_creation

        # Cumulative cache is monotonic too.
        for i in range(1, len(payloads)):
            assert payloads[i]["cache_read"] >= payloads[i - 1]["cache_read"]
            assert payloads[i]["cache_creation"] >= payloads[i - 1]["cache_creation"]

    def test_mixed_caching_and_non_caching_turns(self, tmp_path, monkeypatch):
        """Turns without cache must not zero out accumulated cache totals."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        restore = _suppress_stdout()
        try:
            # Turn 1: cache creation
            _commit_token_tracking(adapter, 1000, 50, 1000, 50, 0, 500)
            # Turn 2: no cache activity (e.g. non-caching provider chunk)
            _commit_token_tracking(adapter, 800, 40, 800, 40, 0, 0)
            # Turn 3: cache hit again
            _commit_token_tracking(adapter, 1200, 60, 1200, 60, 900, 0)
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == 3

        # Turn 2 wrote no cache delta; cumulative cache_creation must still be 500.
        # Since the emission guard omits cache fields when cumulative is still 0,
        # turn 2 may omit them — but since cache_creation reached 500 in turn 1,
        # turn 2's emission keeps reporting 500.
        assert payloads[0]["cache_creation"] == 500
        assert payloads[1]["cache_creation"] == 500
        assert payloads[2]["cache_creation"] == 500
        assert payloads[2]["cache_read"] == 900


class TestStress:
    def test_1000_sequential_emissions_all_valid_and_ordered(self, tmp_path, monkeypatch):
        """Rapid-fire: every line is valid JSON and cumulative is strictly monotonic."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()

        restore = _suppress_stdout()
        try:
            for _ in range(1000):
                adapter._spend_tracker.record_turn(10, 5)
                _emit_oat_tokens(adapter, 10, 5)
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == 1000

        # Cumulative values are exact and strictly increasing.
        for i, p in enumerate(payloads):
            expected_cum_in = (i + 1) * 10
            expected_cum_out = (i + 1) * 5
            assert p["cumulative_input"] == expected_cum_in
            assert p["cumulative_output"] == expected_cum_out
            assert p["delta_input"] == 10
            assert p["delta_output"] == 5

    def test_concurrent_writers_no_corrupt_lines(self, tmp_path, monkeypatch):
        """POSIX O_APPEND atomicity: N threads each write M lines, no partial or interleaved lines."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        n_threads = 8
        n_writes_per_thread = 50

        def worker(thread_id: int) -> None:
            # Each thread gets its own adapter/tracker so the file-level
            # append atomicity is what's actually under test.
            adapter = MagicMock()
            adapter._spend_tracker = TokenSpendAccumulator()
            for seq in range(n_writes_per_thread):
                # Encode thread_id in delta_input so we can verify every
                # line came intact from a specific thread.
                marker = thread_id * 10000 + seq
                adapter._spend_tracker.record_turn(marker, 1)
                _emit_oat_tokens(adapter, marker, 1)

        restore = _suppress_stdout()
        try:
            threads = [threading.Thread(target=worker, args=(i,)) for i in range(n_threads)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
        finally:
            restore()

        # Every line on disk must be a valid [OAT_TOKENS] JSON.
        raw_lines = log_file.read_text(encoding="utf-8").splitlines()
        assert len(raw_lines) == n_threads * n_writes_per_thread

        seen_markers = set()
        for line in raw_lines:
            assert line.startswith("[OAT_TOKENS] "), f"corrupted prefix: {line!r}"
            payload = json.loads(line[len("[OAT_TOKENS] "):])
            seen_markers.add(payload["delta_input"])

        # Every expected marker appears exactly once: no drops, no duplicates.
        expected_markers = {
            tid * 10000 + seq
            for tid in range(n_threads)
            for seq in range(n_writes_per_thread)
        }
        assert seen_markers == expected_markers


class TestDaemonContract:
    """Validate the on-disk format matches what daemon/handleTokenUsageEvent expects."""

    def test_payload_fields_match_daemon_struct(self, tmp_path, monkeypatch):
        """Emission must produce every field the Go struct unmarshals."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        restore = _suppress_stdout()
        try:
            _commit_token_tracking(adapter, 500, 25, 500, 25, 400, 100)
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == 1
        p = payloads[0]

        # Daemon struct at internal/daemon/daemon.go:4450 unmarshals
        # delta_input, delta_output, cumulative_input, cumulative_output,
        # plus optional cache_read and cache_creation.
        required = {"delta_input", "delta_output", "cumulative_input", "cumulative_output"}
        assert required.issubset(p.keys())

        # Cache fields are surfaced when non-zero.
        assert p.get("cache_read") == 400
        assert p.get("cache_creation") == 100

        # All numeric fields are ints (not floats or strings).
        numeric_keys = (
            "delta_input", "delta_output",
            "cumulative_input", "cumulative_output",
            "cache_read", "cache_creation",
        )
        for key in numeric_keys:
            assert isinstance(p[key], int), f"{key} not int: {type(p[key])}"

    def test_non_caching_provider_payload_compact(self, tmp_path, monkeypatch):
        """When cache is never reported, payload stays 4 fields — no noise."""
        log_file = tmp_path / "agent.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log_file))

        adapter = MagicMock()
        adapter._spend_tracker = TokenSpendAccumulator()
        adapter._token_tracker = None

        restore = _suppress_stdout()
        try:
            _commit_token_tracking(adapter, 500, 25, 500, 25)  # no cache args
        finally:
            restore()

        payloads = _read_token_payloads(log_file)
        assert len(payloads) == 1
        assert set(payloads[0].keys()) == {
            "delta_input", "delta_output", "cumulative_input", "cumulative_output",
        }
