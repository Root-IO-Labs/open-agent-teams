"""Minimal Langfuse ingestion client used by ``LangfuseMiddleware``.

Mirrors the Go-side implementation in ``internal/telemetry/langfuse.go``:

- Reads ``LANGFUSE_PUBLIC_KEY``, ``LANGFUSE_SECRET_KEY``, ``LANGFUSE_HOST``,
  and ``OAT_TRACE_ID`` from the environment. The Go daemon injects all four
  when it spawns an agent with telemetry enabled.

- One module-level singleton per process. ``get_client()`` returns it (or
  ``None`` when telemetry is off so callers can early-return).

- Events are JSON envelopes pushed onto a bounded queue. A daemon writer
  thread batches and POSTs to ``{host}/api/public/ingestion`` using basic
  auth. Failures degrade silently after one warning — the Python agent's
  critical path must never block on telemetry.

The reason for not depending on the official ``langfuse`` pip package is
intentional: that package pulls in Pydantic v2 and other transitive deps,
and we want telemetry to be a pure no-op when off (no import cost, no
dependency conflicts). Direct HTTP also keeps the Python and Go sides
emitting through the exact same wire protocol, which makes the two
trace trees correlate cleanly.
"""
from __future__ import annotations

import atexit
import base64
import json
import logging
import os
import queue
import secrets
import threading
import time
import urllib.request
from datetime import datetime, timezone
from typing import Any, Optional

logger = logging.getLogger("oat.telemetry")

_QUEUE_CAPACITY = 4096
_BATCH_SIZE = 64
_FLUSH_INTERVAL_S = 3.0
_HTTP_TIMEOUT_S = 10.0


def _iso_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.") + f"{datetime.now(timezone.utc).microsecond // 1000:03d}Z"


def _new_id() -> str:
    return secrets.token_hex(16)


class LangfuseClient:
    """Lazy, thread-safe Langfuse client.

    The first call to ``emit_*`` starts the background writer thread and
    registers an ``atexit`` flush. All ``emit_*`` methods are non-blocking;
    they enqueue and return.
    """

    def __init__(self, public_key: str, secret_key: str, host: str, trace_id: str):
        self.public_key = public_key
        self.secret_key = secret_key
        self.host = host.rstrip("/")
        self.trace_id = trace_id
        self._queue: queue.Queue[dict[str, Any]] = queue.Queue(maxsize=_QUEUE_CAPACITY)
        self._stop = threading.Event()
        self._warned = False
        self._thread = threading.Thread(target=self._run, name="oat-langfuse", daemon=True)
        self._thread.start()
        atexit.register(self.close)

    # ─── Public emit API ─────────────────────────────────────────────

    def emit_generation(self, *, name: str, model: str, input_tokens: int, output_tokens: int,
                        start_time: str, end_time: str, level: str = "DEFAULT",
                        metadata: Optional[dict[str, Any]] = None) -> None:
        body: dict[str, Any] = {
            "id": _new_id(),
            "traceId": self.trace_id,
            "name": name,
            "model": model,
            "startTime": start_time,
            "endTime": end_time,
            "usage": {
                "input": input_tokens,
                "output": output_tokens,
                "total": input_tokens + output_tokens,
            },
            "level": level,
        }
        if metadata:
            body["metadata"] = metadata
        self._enqueue("generation-create", body)

    def emit_tool_span(self, *, name: str, start_time: str, end_time: str,
                       level: str = "DEFAULT", metadata: Optional[dict[str, Any]] = None) -> None:
        body: dict[str, Any] = {
            "id": _new_id(),
            "traceId": self.trace_id,
            "name": f"tool:{name}",
            "startTime": start_time,
            "endTime": end_time,
            "level": level,
        }
        if metadata:
            body["metadata"] = metadata
        self._enqueue("span-create", body)

    # ─── Internals ───────────────────────────────────────────────────

    def _enqueue(self, event_type: str, body: dict[str, Any]) -> None:
        envelope = {
            "id": _new_id(),
            "type": event_type,
            "timestamp": _iso_now(),
            "body": body,
        }
        try:
            self._queue.put_nowait(envelope)
        except queue.Full:
            if not self._warned:
                logger.warning("oat telemetry queue full, dropping events (Langfuse slow or unreachable)")
                self._warned = True

    def _run(self) -> None:
        batch: list[dict[str, Any]] = []
        last_flush = time.monotonic()
        while not self._stop.is_set():
            timeout = max(0.05, _FLUSH_INTERVAL_S - (time.monotonic() - last_flush))
            try:
                ev = self._queue.get(timeout=timeout)
                batch.append(ev)
                if len(batch) >= _BATCH_SIZE:
                    self._flush(batch)
                    batch = []
                    last_flush = time.monotonic()
            except queue.Empty:
                if batch:
                    self._flush(batch)
                    batch = []
                last_flush = time.monotonic()
        # Drain on shutdown.
        while True:
            try:
                batch.append(self._queue.get_nowait())
            except queue.Empty:
                break
            if len(batch) >= _BATCH_SIZE:
                self._flush(batch)
                batch = []
        if batch:
            self._flush(batch)

    def _flush(self, batch: list[dict[str, Any]]) -> None:
        if not batch:
            return
        payload = json.dumps({"batch": batch}).encode("utf-8")
        creds = f"{self.public_key}:{self.secret_key}".encode("utf-8")
        auth = "Basic " + base64.b64encode(creds).decode("ascii")
        req = urllib.request.Request(
            self.host + "/api/public/ingestion",
            data=payload,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Authorization": auth,
                "User-Agent": "oat-telemetry-py/1",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=_HTTP_TIMEOUT_S) as resp:
                # Drain body so the connection can be reused.
                resp.read(1024)
        except Exception as e:  # noqa: BLE001 — never propagate telemetry failures
            if not self._warned:
                logger.warning("oat telemetry send failed: %s (further errors suppressed this session)", e)
                self._warned = True

    def close(self, timeout: float = 2.0) -> None:
        if self._stop.is_set():
            return
        self._stop.set()
        self._thread.join(timeout=timeout)


# Module-level singleton + lock so concurrent first-emit calls share one client.
_client: Optional[LangfuseClient] = None
_client_lock = threading.Lock()
_disabled: bool = False  # True once we've confirmed env is incomplete; skip rechecks.


def get_client() -> Optional[LangfuseClient]:
    """Return the active client, or ``None`` if telemetry is off.

    Off means: any of ``LANGFUSE_PUBLIC_KEY``, ``LANGFUSE_SECRET_KEY``, or
    ``OAT_TRACE_ID`` is empty/unset. The daemon either injects all of them
    or none, so this check is binary.
    """
    global _client, _disabled
    if _disabled:
        return None
    if _client is not None:
        return _client
    with _client_lock:
        if _client is not None:
            return _client
        public = os.environ.get("LANGFUSE_PUBLIC_KEY", "")
        secret = os.environ.get("LANGFUSE_SECRET_KEY", "")
        host = os.environ.get("LANGFUSE_HOST", "https://cloud.langfuse.com")
        trace = os.environ.get("OAT_TRACE_ID", "")
        if not public or not secret or not trace:
            _disabled = True
            return None
        _client = LangfuseClient(public_key=public, secret_key=secret, host=host, trace_id=trace)
        return _client


__all__ = ["LangfuseClient", "get_client"]
