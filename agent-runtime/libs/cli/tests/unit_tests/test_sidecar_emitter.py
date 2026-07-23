"""Tests for the sidecar_emitter singleton.

Covers:
- Flag-off path: every emit_* is a no-op, no socket created, no errors.
- Flag-on path: client lazy-inits, events reach the listening server.
- Fail-soft: a missing socket does NOT raise from emit_*.
- Turn correlation: turn_id threads through subsequent events.
- Metrics: get_metrics() reflects state accurately.
- Idempotent shutdown: multiple _shutdown / _reset cycles are safe.
"""

from __future__ import annotations

import os
import socket
import tempfile
import threading
import time

import pytest

from oat_cli import sidecar_emitter
from oat_cli.sidecar_events import Event, Usage


def _short_sock_dir() -> str:
    return tempfile.mkdtemp(prefix="se-", dir="/tmp")


class _ListeningServer:
    """Tiny test-local server that accepts one connection and collects
    newline-delimited lines. Mirrors the one in test_sidecar_client.py so
    these tests stay self-contained."""

    def __init__(self):
        d = _short_sock_dir()
        self.path = os.path.join(d, "s")
        self._srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._srv.bind(self.path)
        self._srv.listen(1)
        self._srv.settimeout(2.0)
        self.received: list[bytes] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def _run(self):
        while not self._stop.is_set():
            try:
                conn, _ = self._srv.accept()
            except (socket.timeout, OSError):
                continue
            conn.settimeout(0.5)
            buf = b""
            try:
                while not self._stop.is_set():
                    try:
                        chunk = conn.recv(4096)
                    except socket.timeout:
                        continue
                    if not chunk:
                        break
                    buf += chunk
                    while b"\n" in buf:
                        line, _, buf = buf.partition(b"\n")
                        with self._lock:
                            self.received.append(line)
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

    def stop(self):
        self._stop.set()
        try:
            self._srv.close()
        except OSError:
            pass
        try:
            os.remove(self.path)
        except OSError:
            pass
        try:
            os.rmdir(os.path.dirname(self.path))
        except OSError:
            pass

    def wait_for(self, n: int, timeout: float = 3.0) -> list[Event]:
        deadline = time.time() + timeout
        while time.time() < deadline:
            with self._lock:
                if len(self.received) >= n:
                    return [Event.from_json(b.decode("utf-8")) for b in self.received]
            time.sleep(0.02)
        with self._lock:
            return [Event.from_json(b.decode("utf-8")) for b in self.received]


@pytest.fixture(autouse=True)
def _reset_between_tests(monkeypatch):
    """Every test starts with a clean emitter state and no env var.
    Tests that want the emitter on set OAT_SIDECAR_SOCKET themselves."""
    monkeypatch.delenv("OAT_SIDECAR_SOCKET", raising=False)
    sidecar_emitter._reset_for_tests()
    yield
    sidecar_emitter._reset_for_tests()


# --- disabled-path (no env var set) ---


class TestDisabledPath:
    def test_emit_token_usage_is_noop(self):
        # No env var → no-op. Must not raise, must not create a client.
        sidecar_emitter.emit_token_usage(10, 5, 100, 50)
        metrics = sidecar_emitter.get_metrics()
        assert metrics["active"] is False
        assert metrics["socket"] is None

    def test_all_emit_helpers_are_noop(self):
        # Sanity: every public helper is safe to call with the flag off.
        sidecar_emitter.emit_turn_start("hi")
        sidecar_emitter.emit_assistant_delta("x")
        sidecar_emitter.emit_assistant_message("y", usage=Usage(1, 1))
        sidecar_emitter.emit_tool_call("n", {}, "c1")
        sidecar_emitter.emit_tool_result("c1", "ok")
        sidecar_emitter.emit_interrupt("approval", "?")
        sidecar_emitter.emit_token_usage(1, 1, 1, 1)
        sidecar_emitter.emit_turn_end()
        # None of those should have started a client.
        assert sidecar_emitter._client is None

    def test_new_turn_id_returns_id_even_when_disabled(self):
        # Callers that want a correlation id for their own use should get
        # one even if the sidecar isn't emitting — stdout sentinels may
        # still want it.
        tid = sidecar_emitter.new_turn_id()
        assert len(tid) == 12
        assert sidecar_emitter._turn_id() == tid


class TestFailSoftOnBadSocket:
    def test_unreachable_socket_does_not_raise(self, monkeypatch, tmp_path):
        # Point at a path that doesn't exist and never will — the client
        # will retry connect, exhaust its backoff, and drop events. The
        # emitter must not surface that failure to the caller.
        monkeypatch.setenv("OAT_SIDECAR_SOCKET", str(tmp_path / "nope"))
        # With this path non-existent, SidecarClient's retry logic will
        # give up after ~3s. We don't wait that long — we just verify
        # that emit_token_usage returns without raising.
        sidecar_emitter.emit_token_usage(10, 5, 100, 50)
        # Must have attempted to init.
        assert sidecar_emitter._initialized is True


# --- enabled-path (env var set, server listening) ---


class TestEnabledPath:
    def test_token_usage_reaches_server(self, monkeypatch):
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            sidecar_emitter.emit_token_usage(
                delta_input=10,
                delta_output=5,
                cumulative_input=100,
                cumulative_output=50,
                cache_read=20,
            )
            # Give the writer a moment to drain.
            events = srv.wait_for(1, timeout=3.0)
            assert len(events) == 1
            ev = events[0]
            assert ev.kind == "token_usage"
            assert ev.data["cumulative_input"] == 100
            assert ev.data["cache_read"] == 20
            assert "cache_creation" not in ev.data  # omitempty at zero
        finally:
            srv.stop()

    def test_turn_id_threads_through_events(self, monkeypatch):
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            tid = sidecar_emitter.emit_turn_start("hi there")
            assert tid is not None
            # All subsequent emits inherit that turn_id via the module
            # state — this is how the Go side correlates a stream of
            # events to one chat turn.
            sidecar_emitter.emit_assistant_delta("par")
            sidecar_emitter.emit_assistant_message("partial", usage=Usage(10, 5))
            sidecar_emitter.emit_token_usage(10, 5, 100, 50)
            sidecar_emitter.emit_turn_end()

            events = srv.wait_for(5, timeout=3.0)
            assert len(events) == 5
            # Every event should carry the same turn_id.
            turn_ids = {e.turn_id for e in events}
            assert turn_ids == {tid}
            kinds = [e.kind for e in events]
            assert kinds == [
                "turn_start",
                "assistant_delta",
                "assistant_message",
                "token_usage",
                "turn_end",
            ]
        finally:
            srv.stop()

    def test_turn_end_clears_turn_id(self, monkeypatch):
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            sidecar_emitter.emit_turn_start("a")
            sidecar_emitter.emit_turn_end()
            assert sidecar_emitter._turn_id() is None
        finally:
            srv.stop()

    def test_metrics_reflect_emits(self, monkeypatch):
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            for i in range(5):
                sidecar_emitter.emit_token_usage(i, i, i * 10, i * 5)
            # Let the writer drain.
            srv.wait_for(5, timeout=3.0)
            metrics = sidecar_emitter.get_metrics()
            assert metrics["active"] is True
            assert metrics["emitted"] == 5
            assert metrics["dropped_queue_full"] == 0
        finally:
            srv.stop()

    def test_sequence_numbers_are_monotonic(self, monkeypatch):
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            for i in range(3):
                sidecar_emitter.emit_token_usage(i, i, i * 10, i * 5)
            events = srv.wait_for(3, timeout=3.0)
            seqs = [e.seq for e in events]
            # Monotonic and contiguous (no drops in this small test).
            assert seqs == sorted(seqs)
            assert len(set(seqs)) == 3

        finally:
            srv.stop()


# --- lifecycle ---


class TestLifecycle:
    def test_reset_is_idempotent(self):
        # Double _reset from tests must not raise.
        sidecar_emitter._reset_for_tests()
        sidecar_emitter._reset_for_tests()

    def test_init_is_lazy(self, monkeypatch):
        # Setting the env var alone doesn't start the client — only the
        # first emit_* call does. This keeps the disabled path zero-cost.
        srv = _ListeningServer()
        try:
            monkeypatch.setenv("OAT_SIDECAR_SOCKET", srv.path)
            assert sidecar_emitter._client is None
            sidecar_emitter.emit_token_usage(1, 1, 1, 1)
            assert sidecar_emitter._client is not None
        finally:
            srv.stop()


# --- [OAT_TURN_END] tool-log sentinel ---


class TestTurnEndSentinel:
    """emit_turn_end() writes a dedicated ``[OAT_TURN_END] <turn_id>`` line
    to OAT_TOOL_LOG (the file the daemon's assistant-turn tailer reads) so
    the daemon can detect the end of a silent, tool-only turn. This is the
    signal that stops the side-panel spinner and drives the self-healing
    recovery ladder — distinct from ``[OAT_TOKENS]`` on purpose, since that
    also fires after a mid-turn compaction."""

    def test_sentinel_written_once_per_turn(self, monkeypatch, tmp_path):
        sidecar_emitter._reset_for_tests()
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        # No sidecar socket needed — the sentinel write is a best-effort
        # direct append independent of the socket client.
        tid = sidecar_emitter.new_turn_id()
        sidecar_emitter.set_turn_id(tid)

        sidecar_emitter.emit_turn_end()

        contents = log.read_text(encoding="utf-8")
        assert contents.count("[OAT_TURN_END]") == 1
        assert f"[OAT_TURN_END] {tid}" in contents
        # emit_turn_end clears the turn_id, so a second (idempotent finally)
        # call must NOT write a duplicate sentinel.
        sidecar_emitter.emit_turn_end()
        assert log.read_text(encoding="utf-8").count("[OAT_TURN_END]") == 1

    def test_no_sentinel_without_turn_id(self, monkeypatch, tmp_path):
        sidecar_emitter._reset_for_tests()
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        # No active turn -> emit_turn_end is a no-op, no sentinel written.
        sidecar_emitter.set_turn_id(None)
        sidecar_emitter.emit_turn_end()
        assert not log.exists() or log.read_text(encoding="utf-8") == ""

    def test_sentinel_noop_when_tool_log_unset(self, monkeypatch):
        # No OAT_TOOL_LOG in the environment -> emit_turn_end must not raise
        # even though a turn is active (the write is best-effort).
        sidecar_emitter._reset_for_tests()
        monkeypatch.delenv("OAT_TOOL_LOG", raising=False)
        sidecar_emitter.set_turn_id(sidecar_emitter.new_turn_id())
        sidecar_emitter.emit_turn_end()  # must not raise


# --- [OAT_TODOS] tool-log sentinel ---


class TestTodosSentinel:
    """emit_todos() writes a dedicated ``[OAT_TODOS] <json>`` line to
    OAT_TOOL_LOG carrying the full plan/checklist, so the daemon can
    forward it to the side panel's live todo card. The full list bypasses
    the parser's 200-byte tool-arg preview cap."""

    def test_writes_json_line_from_todos_arg(self, monkeypatch, tmp_path):
        import json

        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_todos(
            {
                "todos": [
                    {"content": "A", "status": "completed", "activeForm": "Doing A"},
                    {"content": "B", "status": "in_progress", "activeForm": "Doing B"},
                ]
            }
        )
        contents = log.read_text(encoding="utf-8")
        assert contents.count("[OAT_TODOS]") == 1
        payload = contents.split("[OAT_TODOS]", 1)[1].strip()
        parsed = json.loads(payload)
        assert len(parsed) == 2
        assert parsed[0] == {"content": "A", "status": "completed", "activeForm": "Doing A"}
        assert parsed[1]["status"] == "in_progress"

    def test_accepts_bare_list(self, monkeypatch, tmp_path):
        import json

        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_todos([{"content": "X", "status": "pending"}])
        payload = log.read_text(encoding="utf-8").split("[OAT_TODOS]", 1)[1].strip()
        parsed = json.loads(payload)
        assert parsed == [{"content": "X", "status": "pending", "activeForm": ""}]

    def test_empty_list_still_emits(self, monkeypatch, tmp_path):
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_todos({"todos": []})
        assert "[OAT_TODOS] []" in log.read_text(encoding="utf-8")

    def test_non_list_shape_writes_nothing(self, monkeypatch, tmp_path):
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_todos({"not_todos": 1})
        assert not log.exists() or log.read_text(encoding="utf-8") == ""

    def test_item_count_and_fields_are_bounded(self, monkeypatch, tmp_path):
        import json

        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        big = "Z" * 5000
        items = [{"content": big, "status": "pending"} for _ in range(200)]
        sidecar_emitter.emit_todos({"todos": items})
        payload = log.read_text(encoding="utf-8").split("[OAT_TODOS]", 1)[1].strip()
        parsed = json.loads(payload)
        assert len(parsed) <= 50
        assert all(len(it["content"]) <= 500 for it in parsed)

    def test_noop_when_tool_log_unset(self, monkeypatch):
        monkeypatch.delenv("OAT_TOOL_LOG", raising=False)
        sidecar_emitter.emit_todos({"todos": [{"content": "A", "status": "pending"}]})  # no raise


class TestEmitGenerating:
    """[OAT_GENERATING] heartbeats — UI only, rate-limited, never raise."""

    def setup_method(self):
        sidecar_emitter.stop_all_generating_pulses()
        sidecar_emitter._gen_last_mono.clear()

    def teardown_method(self):
        sidecar_emitter.stop_all_generating_pulses()
        sidecar_emitter._gen_last_mono.clear()

    def test_writes_sentinel_with_tool_and_bytes(self, monkeypatch, tmp_path):
        import json

        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_generating("write_file", 12345)
        text = log.read_text(encoding="utf-8")
        assert text.startswith("[OAT_GENERATING] ")
        payload = json.loads(text.split("[OAT_GENERATING]", 1)[1].strip())
        assert payload == {"tool": "write_file", "bytes": 12345}

    def test_rate_limits_same_tool(self, monkeypatch, tmp_path):
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_generating("write_file", 1)
        sidecar_emitter.emit_generating("write_file", 2)
        assert log.read_text(encoding="utf-8").count("[OAT_GENERATING]") == 1

    def test_rejects_invalid_tool_name(self, monkeypatch, tmp_path):
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.emit_generating("write file!", 1)
        assert not log.exists() or log.read_text(encoding="utf-8") == ""

    def test_noop_when_tool_log_unset(self, monkeypatch):
        monkeypatch.delenv("OAT_TOOL_LOG", raising=False)
        sidecar_emitter.emit_generating("write_file", 1)  # no raise

    def test_timer_pulse_without_further_chunks(self, monkeypatch, tmp_path):
        """Buffered arg generation: timer alone keeps [OAT_GENERATING] alive."""
        import time

        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        monkeypatch.setattr(sidecar_emitter, "_GEN_MIN_INTERVAL_S", 0.05)
        sidecar_emitter.start_generating_pulse("buf-0", "write_file")
        # First emit may be rate-limited against an empty last_mono; clear so
        # the ticker can write promptly.
        sidecar_emitter._gen_last_mono.clear()
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            if log.exists() and log.read_text(encoding="utf-8").count(
                "[OAT_GENERATING]"
            ) >= 1:
                break
            time.sleep(0.02)
        assert log.exists()
        assert "[OAT_GENERATING]" in log.read_text(encoding="utf-8")
        sidecar_emitter.set_generating_pulse_bytes("buf-0", 42000)
        sidecar_emitter._gen_last_mono.clear()
        n_before = log.read_text(encoding="utf-8").count("[OAT_GENERATING]")
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            if log.read_text(encoding="utf-8").count("[OAT_GENERATING]") > n_before:
                break
            time.sleep(0.02)
        assert log.read_text(encoding="utf-8").count("[OAT_GENERATING]") > n_before
        assert '"bytes":42000' in log.read_text(encoding="utf-8")
        sidecar_emitter.stop_generating_pulse("buf-0")

    def test_stop_all_clears_pulses(self, monkeypatch, tmp_path):
        log = tmp_path / "tool.log"
        monkeypatch.setenv("OAT_TOOL_LOG", str(log))
        sidecar_emitter.start_generating_pulse(0, "write_file")
        sidecar_emitter.stop_all_generating_pulses()
        assert sidecar_emitter._pulse_state == {}
