"""Unix-socket client that emits sidecar events from the Python agent.

Design contract (must stay in sync with ``pkg/sidecar/server.go``):

- Newline-delimited JSON on the wire.
- One socket per agent. Go listens, Python connects.
- ``emit()`` is non-blocking: it enqueues onto a bounded ``queue.Queue``
  and returns. The agent's critical path (astream loop) never blocks on
  socket I/O or a slow TUI consumer — if we blocked, a disconnected TUI
  would freeze agent progress.
- On queue-full, we drop the OLDEST event (not newest). Newest is what
  the user is watching right now; oldest is already stale UI state.
  Drops are counted for observability.
- A background writer thread drains the queue to the socket. It retries
  the initial connect with exponential backoff (10 attempts, ~3s total).
  On post-connect disconnect (EPIPE / ConnectionResetError), it reconnects
  the same way while preserving queued events.
- ``close()`` signals stop, lets the writer drain for up to ``timeout``
  seconds, then force-closes. Remaining queued events are reported in
  ``dropped_on_close``.

Failure-mode policy: the agent is the producer of truth. If the sidecar is
unreachable the agent MUST continue running (ConversationLogger still writes
to disk); only the live TUI view is degraded.
"""
from __future__ import annotations

import errno
import logging
import os
import queue
import socket
import sys
import threading
import time
from typing import Optional

from oat_cli.sidecar_events import (
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

_log = logging.getLogger(__name__)

# --- tunables ---
DEFAULT_QUEUE_SIZE = 1000
# Connect retry schedule. Total wall-clock ≈ sum(schedule) ≈ 3s.
_CONNECT_BACKOFF_S = (0.05, 0.1, 0.2, 0.3, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5)
# Drain interval for writer thread when queue is empty. Short enough to
# flush tail events promptly on close, long enough to avoid busy-loop.
_IDLE_POLL_S = 0.05


class SidecarClient:
    """Thread-safe, non-blocking producer for the sidecar socket.

    Usage::

        client = SidecarClient("/tmp/oat-agent-123.sock")
        client.start()
        try:
            client.emit(turn_start(seq=0, turn_id="t", user_input="hi"))
            ...
        finally:
            client.close(timeout=2.0)

    ``emit`` is thread-safe. ``start`` and ``close`` should be called from
    the owning thread exactly once each.
    """

    def __init__(
        self,
        socket_path: str,
        queue_size: int = DEFAULT_QUEUE_SIZE,
        connect_backoff: Optional[tuple[float, ...]] = None,
    ):
        if queue_size < 1:
            raise ValueError("queue_size must be >= 1")
        self.socket_path = socket_path
        self._queue: queue.Queue[Event] = queue.Queue(maxsize=queue_size)
        self._backoff = connect_backoff if connect_backoff is not None else _CONNECT_BACKOFF_S
        self._stop = threading.Event()
        self._sock: Optional[socket.socket] = None
        self._writer: Optional[threading.Thread] = None
        # Observability counters — read from any thread via the properties
        # below. All counters are monotonic.
        self._lock = threading.Lock()
        self._dropped_queue_full = 0
        self._dropped_on_close = 0
        self._emitted = 0
        self._reconnects = 0
        self._connected = False

    # --- public API ---

    def start(self) -> None:
        """Start the writer thread. Does NOT block waiting for connection;
        the writer handles connect retry asynchronously. Idempotent."""
        if self._writer is not None:
            return
        self._writer = threading.Thread(
            target=self._writer_loop, name="sidecar-writer", daemon=True,
        )
        self._writer.start()

    def emit(self, event: Event) -> None:
        """Enqueue an event. Non-blocking.

        If the queue is full, drops the OLDEST event and increments
        ``dropped_queue_full``. Newest events always win — the user is
        watching the live TUI, not the one from 3s ago.
        """
        try:
            self._queue.put_nowait(event)
        except queue.Full:
            # Drop oldest to make room. Race window: another emit may sneak
            # in between get_nowait and put_nowait and fill the queue
            # again. That's fine — we'll drop another on the next emit.
            try:
                _ = self._queue.get_nowait()
                with self._lock:
                    self._dropped_queue_full += 1
            except queue.Empty:
                pass
            try:
                self._queue.put_nowait(event)
            except queue.Full:
                # Extreme contention: give up, count it.
                with self._lock:
                    self._dropped_queue_full += 1

    def close(self, timeout: float = 2.0) -> None:
        """Signal the writer to drain and exit. Blocks up to ``timeout``
        seconds waiting for the queue to empty. Remaining events are
        counted in ``dropped_on_close``.
        """
        self._stop.set()
        if self._writer is not None:
            self._writer.join(timeout=timeout)
        if self._sock is not None:
            try:
                self._sock.shutdown(socket.SHUT_WR)
            except OSError:
                pass
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None
        # Count any events the writer couldn't drain in time.
        remaining = 0
        while True:
            try:
                self._queue.get_nowait()
                remaining += 1
            except queue.Empty:
                break
        with self._lock:
            self._dropped_on_close += remaining

    # --- metrics (read-only, safe from any thread) ---

    @property
    def connected(self) -> bool:
        return self._connected

    @property
    def emitted(self) -> int:
        with self._lock:
            return self._emitted

    @property
    def dropped_queue_full(self) -> int:
        with self._lock:
            return self._dropped_queue_full

    @property
    def dropped_on_close(self) -> int:
        with self._lock:
            return self._dropped_on_close

    @property
    def reconnects(self) -> int:
        with self._lock:
            return self._reconnects

    # --- writer thread internals ---

    def _writer_loop(self) -> None:
        while not self._stop.is_set():
            if self._sock is None:
                if not self._connect():
                    # Connect failed after retries — drop any events waiting
                    # in the queue to avoid indefinite memory growth, then
                    # exit the writer. The agent keeps running; sidecar is
                    # a non-critical path.
                    self._drain_on_connect_failure()
                    return

            # Drain queue to socket until it's empty or stop is set. An
            # individual write failure bounces us back out to reconnect.
            try:
                ev = self._queue.get(timeout=_IDLE_POLL_S)
            except queue.Empty:
                continue

            if not self._send(ev):
                # Put the event back at the front? queue.Queue is FIFO,
                # so we can't. Best effort: put it back at the tail —
                # worse case it's seen slightly out of order after a
                # reconnect, and the gap detector will fire. For now we
                # drop it; the seq gap is a clear observability signal.
                with self._lock:
                    self._dropped_queue_full += 1
                self._disconnect()

        # Drain remaining events up to stop deadline (caller sets the
        # deadline via close's join timeout).
        while True:
            try:
                ev = self._queue.get_nowait()
            except queue.Empty:
                return
            if self._sock is None or not self._send(ev):
                # Can't drain further — stop trying.
                return

    def _connect(self) -> bool:
        for i, delay in enumerate(self._backoff):
            if self._stop.is_set():
                return False
            try:
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.connect(self.socket_path)
                self._sock = s
                self._connected = True
                if i > 0:
                    with self._lock:
                        self._reconnects += 1
                return True
            except (FileNotFoundError, ConnectionRefusedError):
                time.sleep(delay)
            except OSError as e:
                # Unix sockets can return EAGAIN/EINTR — treat as retryable.
                if e.errno in (errno.EAGAIN, errno.EINTR):
                    time.sleep(delay)
                    continue
                _log.warning("sidecar connect failed: %s", e)
                time.sleep(delay)
        return False

    def _disconnect(self) -> None:
        self._connected = False
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None

    def _send(self, ev: Event) -> bool:
        if self._sock is None:
            return False
        try:
            line = ev.to_json() + "\n"
            # Use sendall — partial writes on stream sockets would corrupt
            # the newline framing.
            self._sock.sendall(line.encode("utf-8"))
            with self._lock:
                self._emitted += 1
            return True
        except (BrokenPipeError, ConnectionResetError):
            return False
        except OSError as e:
            _log.warning("sidecar send failed: %s", e)
            return False

    def _drain_on_connect_failure(self) -> None:
        remaining = 0
        while True:
            try:
                self._queue.get_nowait()
                remaining += 1
            except queue.Empty:
                break
        if remaining:
            _log.warning(
                "sidecar connect retries exhausted; %d queued events dropped",
                remaining,
            )
            with self._lock:
                self._dropped_queue_full += remaining


# ---------- CLI entry for integration testing ----------
#
# Usage: python -m oat_cli.sidecar_client <socket_path> <count> [mode]
#
# Connects to <socket_path>, emits <count> events of varied kinds (covering
# every factory), then closes. Exit code 0 on clean close, 1 if any events
# were dropped. Used by pkg/sidecar/integration_test.go to drive an end-to-
# end cross-language verification.
#
# mode="mixed" (default) cycles through all kinds. mode="deltas" emits only
# assistant_delta, used by the throughput / backpressure test.


def _cli_main(argv: list[str]) -> int:
    if len(argv) < 3:
        print(
            "usage: python -m oat_cli.sidecar_client "
            "<socket> <count> [mixed|deltas] [queue_size]",
            file=sys.stderr,
        )
        return 2
    socket_path = argv[1]
    count = int(argv[2])
    mode = argv[3] if len(argv) > 3 else "mixed"
    # Queue size override for stress tests. Production default stays at
    # DEFAULT_QUEUE_SIZE; tests that burst events faster than LLM cadence
    # bump this to avoid spurious drops.
    queue_size = int(argv[4]) if len(argv) > 4 else DEFAULT_QUEUE_SIZE

    client = SidecarClient(socket_path, queue_size=queue_size)
    client.start()

    turn_id = "itest"
    for i in range(count):
        if mode == "deltas":
            ev = assistant_delta(seq=i, turn_id=turn_id, content=f"d{i}")
        else:
            # Cycle through every kind so the test covers all parsers.
            kind_idx = i % 8
            if kind_idx == 0:
                ev = turn_start(seq=i, turn_id=turn_id, user_input=f"u{i}")
            elif kind_idx == 1:
                ev = assistant_delta(seq=i, turn_id=turn_id, content=f"d{i}")
            elif kind_idx == 2:
                ev = assistant_message(
                    seq=i, turn_id=turn_id, content=f"msg{i}",
                    usage=Usage(input_tokens=10 + i, output_tokens=5),
                )
            elif kind_idx == 3:
                ev = tool_call(
                    seq=i, turn_id=turn_id, name="x", args={"i": i}, call_id=f"c{i}",
                )
            elif kind_idx == 4:
                ev = tool_result(seq=i, turn_id=turn_id, call_id=f"c{i-1}", content="ok")
            elif kind_idx == 5:
                ev = interrupt(seq=i, turn_id=turn_id, interrupt_kind="approval", prompt="?")
            elif kind_idx == 6:
                ev = token_usage(
                    seq=i, turn_id=turn_id,
                    delta_input=10 + i, delta_output=5,
                    cumulative_input=100 + i * 10, cumulative_output=50 + i * 5,
                    cache_read=i if i % 2 == 0 else 0,
                )
            else:
                ev = turn_end(seq=i, turn_id=turn_id)
        client.emit(ev)

    # Allow generous time for the writer to drain. The integration test
    # times us out externally so we don't hang forever.
    client.close(timeout=10.0)

    # Report back on stderr so the test can parse if needed.
    print(
        f"sidecar client: emitted={client.emitted} "
        f"dropped_qfull={client.dropped_queue_full} "
        f"dropped_close={client.dropped_on_close} "
        f"reconnects={client.reconnects}",
        file=sys.stderr,
    )
    total_sent = client.emitted
    if total_sent != count:
        # Not a hard error — the integration test decides pass/fail based
        # on what the Go side received. But non-zero exit surfaces oddness.
        return 1
    return 0


if __name__ == "__main__":
    # Propagate errors to the outer test runner via non-zero exit.
    try:
        sys.exit(_cli_main(sys.argv))
    except Exception as e:  # noqa: BLE001 — top-level harness
        print(f"sidecar client fatal: {e}", file=sys.stderr)
        sys.exit(3)
