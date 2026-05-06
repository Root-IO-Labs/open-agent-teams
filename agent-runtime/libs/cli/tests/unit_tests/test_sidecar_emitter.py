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
                    return [Event.from_json(b.decode("utf-8"))
                            for b in self.received]
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
                delta_input=10, delta_output=5,
                cumulative_input=100, cumulative_output=50,
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
                "turn_start", "assistant_delta", "assistant_message",
                "token_usage", "turn_end",
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
