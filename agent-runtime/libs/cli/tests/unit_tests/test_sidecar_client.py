"""Unit tests for the Python sidecar client.

Scope: behavior of ``SidecarClient`` in isolation — queue bounds, drop
policy, connect retry, disconnect resilience. End-to-end wire interop with
the Go server is tested by ``pkg/sidecar/integration_test.go``.
"""
from __future__ import annotations

import os
import socket
import tempfile
import threading
import time

import pytest

from oat_cli.sidecar_client import (
    DEFAULT_QUEUE_SIZE,
    SidecarClient,
)
from oat_cli.sidecar_events import (
    Event,
    assistant_delta,
    turn_start,
)


# ---------- helpers ----------


def _short_sock() -> str:
    """Return a sock path safely under macOS's 104-byte sun_path limit."""
    d = tempfile.mkdtemp(prefix="sct-", dir="/tmp")
    return os.path.join(d, "s")


class _TestServer:
    """Tiny Unix-socket server for tests. Accepts one connection at a time,
    reads newline-delimited lines, exposes them via ``received``."""

    def __init__(self):
        self.path = _short_sock()
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

    def wait_for(self, n: int, timeout: float = 3.0) -> list[bytes]:
        deadline = time.time() + timeout
        while time.time() < deadline:
            with self._lock:
                if len(self.received) >= n:
                    return list(self.received)
            time.sleep(0.02)
        with self._lock:
            return list(self.received)


# ---------- queue / drop policy ----------


class TestQueueBehavior:
    def test_rejects_zero_queue_size(self):
        with pytest.raises(ValueError):
            SidecarClient(_short_sock(), queue_size=0)

    def test_drops_oldest_on_full_queue_without_server(self):
        # No server listening — writer thread can't drain. Queue fills,
        # subsequent emits drop the oldest entry.
        c = SidecarClient(_short_sock(), queue_size=3)
        # Don't start the writer; that way nothing drains and we can
        # observe the drop counter deterministically.
        c.emit(turn_start(1, "t", "a"))
        c.emit(turn_start(2, "t", "b"))
        c.emit(turn_start(3, "t", "c"))
        assert c.dropped_queue_full == 0
        c.emit(turn_start(4, "t", "d"))  # should drop seq=1
        assert c.dropped_queue_full == 1
        c.emit(turn_start(5, "t", "e"))
        assert c.dropped_queue_full == 2
        # Close is idempotent and should tally remaining queued events
        # as dropped_on_close.
        c.close(timeout=0.1)
        assert c.dropped_on_close >= 1  # 3 remaining in queue


# ---------- connect / reconnect ----------


class TestConnectRetry:
    def test_connect_succeeds_when_server_appears_late(self):
        # Start client pointing at a socket that doesn't exist yet. The
        # writer loop retries with backoff; we bring the server up during
        # the retry window and verify emitted events arrive.
        path = _short_sock()
        c = SidecarClient(path, connect_backoff=(0.05,) * 20)
        c.start()
        c.emit(turn_start(1, "t", "a"))

        # Race: create the server after a short delay.
        time.sleep(0.1)
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(path)
        srv.listen(1)
        srv.settimeout(2.0)

        try:
            conn, _ = srv.accept()
        except socket.timeout:
            c.close(timeout=0.1)
            pytest.fail("client never connected")

        # Drain one line (the first event).
        conn.settimeout(1.0)
        buf = b""
        while b"\n" not in buf:
            try:
                chunk = conn.recv(4096)
            except socket.timeout:
                break
            if not chunk:
                break
            buf += chunk

        c.close(timeout=1.0)
        srv.close()
        os.remove(path)
        os.rmdir(os.path.dirname(path))

        assert b'"seq":1' in buf

    def test_gives_up_after_backoff_exhausted(self):
        # Very short backoff schedule. No server ever appears. Writer must
        # give up, drain pending queue, and exit without hanging.
        c = SidecarClient(
            _short_sock(),
            queue_size=10,
            connect_backoff=(0.01, 0.01, 0.01),
        )
        c.emit(turn_start(1, "t", "a"))
        c.emit(turn_start(2, "t", "b"))
        c.start()
        # Give the writer enough time to exhaust its backoff.
        time.sleep(0.5)
        c.close(timeout=1.0)

        # Events should have been dropped since connect never succeeded.
        assert c.emitted == 0
        assert c.dropped_queue_full >= 2


# ---------- send path ----------


class TestSendPath:
    def test_events_reach_server_in_order(self):
        srv = _TestServer()
        try:
            c = SidecarClient(srv.path)
            c.start()
            for i in range(5):
                c.emit(turn_start(i, "t", f"u{i}"))
            # Close with generous drain timeout so the writer flushes.
            c.close(timeout=3.0)

            lines = srv.wait_for(5, timeout=3.0)
            assert len(lines) == 5, f"got {len(lines)} lines"
            for i, line in enumerate(lines):
                ev = Event.from_json(line.decode("utf-8"))
                assert ev.seq == i
                assert ev.kind == "turn_start"
        finally:
            srv.stop()

    def test_close_is_idempotent(self):
        # Calling close twice should not raise.
        c = SidecarClient(_short_sock(), queue_size=1)
        c.start()
        c.close(timeout=0.1)
        c.close(timeout=0.1)

    def test_emit_thread_safe(self):
        # Multiple producer threads emit concurrently. All their events
        # should reach the server (up to queue bounds) with no corruption
        # (every line must be valid JSON).
        srv = _TestServer()
        try:
            c = SidecarClient(srv.path, queue_size=2000)
            c.start()
            n_threads = 8
            per_thread = 50

            def producer(base: int):
                for i in range(per_thread):
                    c.emit(assistant_delta(base + i, "t", f"x{base+i}"))

            threads = [
                threading.Thread(target=producer, args=(t * per_thread,))
                for t in range(n_threads)
            ]
            for th in threads:
                th.start()
            for th in threads:
                th.join()

            c.close(timeout=5.0)
            expected = n_threads * per_thread
            lines = srv.wait_for(expected, timeout=5.0)
            # All received lines must parse cleanly. Some may be lost if
            # the queue fills, but nothing may be corrupted.
            assert len(lines) >= expected * 0.8, (
                f"received only {len(lines)} of {expected}"
            )
            for line in lines:
                ev = Event.from_json(line.decode("utf-8"))
                assert ev.kind in ("assistant_delta",)
        finally:
            srv.stop()


# ---------- default params ----------


class TestDefaults:
    def test_default_queue_size_sensible(self):
        # Guard against accidental drops-to-zero regressions.
        assert DEFAULT_QUEUE_SIZE >= 100
