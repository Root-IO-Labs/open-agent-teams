"""Process-level singleton for emitting sidecar events from inside
``textual_adapter``.

Why a singleton: the astream loop, token commit, and turn boundaries all
need to emit — giving each call site its own ``SidecarClient`` would pile
up writer threads and socket connections. One client per process, lazy-
initialized on first emit, cleaned up at process exit.

Feature gating is entirely env-driven:

- If ``OAT_SIDECAR_SOCKET`` is unset or empty, every ``emit_*`` call is a
  fast no-op (no lock, no client creation, no logging). The agent runs
  exactly as today.

- If set, the module lazy-creates a ``SidecarClient`` pointed at that
  path, starts its writer thread, and registers an ``atexit`` hook so the
  client drains cleanly on a normal process shutdown.

Observability: ``get_metrics()`` returns client counters (emitted,
drops, reconnects) or ``{"active": False}`` when the sidecar is off.
Useful for agent diagnostics and for tests that want to verify the
emitter ran.

Safety invariants:

1. ``emit_*`` MUST NOT raise. Sidecar failures must never interrupt the
   agent; the stdout ``[OAT_TOKENS]`` path is the source of truth for
   accounting and continues to work regardless.

2. The module's state is thread-local via a lock. Callers from any thread
   may emit concurrently; the underlying ``SidecarClient.emit`` is
   already thread-safe.

3. Sequence numbers are module-monotonic across the process lifetime. If
   the client reconnects, the server resets its own tracker on disconnect
   (see pkg/sidecar/server.go trackSeq).
"""
from __future__ import annotations

import atexit
import itertools
import logging
import os
import threading
import uuid
from typing import Any, Optional

from deepagents_cli.sidecar_client import SidecarClient
from deepagents_cli.sidecar_events import (
    assistant_delta,
    assistant_message,
    interrupt as make_interrupt,
    token_usage,
    tool_call,
    tool_result,
    turn_end,
    turn_start,
    Usage,
)

_log = logging.getLogger(__name__)

# Module state, guarded by _lock.
_lock = threading.Lock()
_client: Optional[SidecarClient] = None
_initialized: bool = False
_seq_counter = itertools.count()
_current_turn_id: Optional[str] = None


def _env_socket_path() -> Optional[str]:
    path = os.environ.get("OAT_SIDECAR_SOCKET")
    if path:
        return path
    return None


def _get_client() -> Optional[SidecarClient]:
    """Return the singleton client or None if the sidecar is disabled.

    Lazy-initializes on first call; subsequent calls are lock-free after
    ``_initialized`` is set. The bias is toward speed on the disabled path
    — production agents run with the flag off by default, and we don't
    want a lock contention on every token commit.
    """
    global _client, _initialized
    # Fast path: no env var → disabled forever for this process.
    path = _env_socket_path()
    if path is None:
        return None
    # Slow path: lazy init.
    if _initialized:
        return _client
    with _lock:
        if _initialized:
            return _client
        try:
            _client = SidecarClient(path)
            _client.start()
            atexit.register(_shutdown)
            _log.info("sidecar_emitter: client started, socket=%s", path)
        except Exception as e:  # noqa: BLE001 — emitter must not raise
            _log.warning("sidecar_emitter: init failed: %s", e)
            _client = None
        _initialized = True
    return _client


def _shutdown() -> None:
    """Close the client on process exit, draining up to 2s.

    Registered via ``atexit`` on successful init. Safe if called twice.
    """
    global _client
    with _lock:
        if _client is not None:
            try:
                _client.close(timeout=2.0)
            except Exception as e:  # noqa: BLE001
                _log.warning("sidecar_emitter: shutdown error: %s", e)
            _client = None


def _next_seq() -> int:
    return next(_seq_counter)


# --- turn correlation ---


def set_turn_id(turn_id: Optional[str]) -> None:
    """Set the turn_id stamped on subsequent events. None = unscoped.

    Called by the adapter at the start of every ``execute_task_textual``
    invocation. The value threads through into every event's envelope
    until the next call. Safe to call from any thread.
    """
    global _current_turn_id
    with _lock:
        _current_turn_id = turn_id


def new_turn_id() -> str:
    """Generate and set a fresh turn_id. Returns the id so the caller
    can also stamp it on stdout sentinels for parity if desired."""
    tid = uuid.uuid4().hex[:12]
    set_turn_id(tid)
    return tid


def _turn_id() -> Optional[str]:
    # Read under the lock to guarantee ordering with set_turn_id, but
    # string reads are atomic in CPython so the lock is cheap.
    with _lock:
        return _current_turn_id


# --- emit helpers — one per event kind ---


def emit_token_usage(
    delta_input: int,
    delta_output: int,
    cumulative_input: int,
    cumulative_output: int,
    cache_read: int = 0,
    cache_creation: int = 0,
) -> None:
    """Mirror of ``_emit_oat_tokens``' stdout payload onto the sidecar.

    Called right after the stdout sentinel is written. The daemon's
    monotonicity guard deduplicates: whichever arrives first wins; the
    second one has an equal cumulative and is a no-op.
    """
    c = _get_client()
    if c is None:
        return
    try:
        ev = token_usage(
            seq=_next_seq(),
            turn_id=_turn_id(),
            delta_input=delta_input,
            delta_output=delta_output,
            cumulative_input=cumulative_input,
            cumulative_output=cumulative_output,
            cache_read=cache_read,
            cache_creation=cache_creation,
        )
        c.emit(ev)
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_token_usage failed: %s", e)


def emit_turn_start(user_input: str, turn_id: Optional[str] = None) -> Optional[str]:
    """Emit a turn_start event and set the active turn_id.

    If ``turn_id`` is None, a fresh one is generated and returned so the
    caller can stamp it on stdout sentinels too for cross-path parity.
    Returns the turn_id even when the sidecar is disabled (so callers
    needing a correlation id still get one).
    """
    if turn_id is None:
        turn_id = uuid.uuid4().hex[:12]
    set_turn_id(turn_id)
    c = _get_client()
    if c is None:
        return turn_id
    try:
        c.emit(turn_start(
            seq=_next_seq(), turn_id=turn_id, user_input=user_input,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_turn_start failed: %s", e)
    return turn_id


def emit_turn_end() -> None:
    """Emit a turn_end event for the current turn_id, then clear it."""
    c = _get_client()
    tid = _turn_id()
    if c is not None and tid is not None:
        try:
            c.emit(turn_end(seq=_next_seq(), turn_id=tid))
        except Exception as e:  # noqa: BLE001
            _log.warning("sidecar_emitter: emit_turn_end failed: %s", e)
    set_turn_id(None)


def emit_assistant_delta(content: str) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(assistant_delta(
            seq=_next_seq(), turn_id=_turn_id() or "", content=content,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_assistant_delta failed: %s", e)


def emit_assistant_message(
    content: str, usage: Optional[Usage] = None,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(assistant_message(
            seq=_next_seq(), turn_id=_turn_id() or "",
            content=content, usage=usage,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_assistant_message failed: %s", e)


def emit_tool_call(
    name: str, args: dict[str, Any], call_id: str,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(tool_call(
            seq=_next_seq(), turn_id=_turn_id() or "",
            name=name, args=args, call_id=call_id,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_tool_call failed: %s", e)


def emit_tool_result(
    call_id: str, content: str, error: Optional[str] = None,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(tool_result(
            seq=_next_seq(), turn_id=_turn_id() or "",
            call_id=call_id, content=content, error=error,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_tool_result failed: %s", e)


def emit_interrupt(interrupt_kind: str, prompt: str) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(make_interrupt(
            seq=_next_seq(), turn_id=_turn_id() or "",
            interrupt_kind=interrupt_kind, prompt=prompt,
        ))
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_interrupt failed: %s", e)


# --- observability ---


def get_metrics() -> dict[str, Any]:
    """Return current emitter metrics or a disabled stub.

    Intended for debugging: a test can call this after emitting events to
    verify the sidecar actually ran, or an operator can include it in
    agent diagnostic dumps. Never raises; always returns a dict.
    """
    c = _client  # volatile read; lock not required for informational data
    if c is None:
        return {"active": False, "socket": _env_socket_path()}
    return {
        "active": True,
        "socket": _env_socket_path(),
        "emitted": c.emitted,
        "dropped_queue_full": c.dropped_queue_full,
        "dropped_on_close": c.dropped_on_close,
        "reconnects": c.reconnects,
    }


# --- test hook: reset module state between tests ---


def _reset_for_tests() -> None:
    """Force the module back to un-initialized. Tests use this to exercise
    the lazy-init path repeatedly with different env settings. Do NOT call
    from production code."""
    global _client, _initialized, _seq_counter, _current_turn_id
    with _lock:
        if _client is not None:
            try:
                _client.close(timeout=0.5)
            except Exception:  # noqa: BLE001
                pass
        _client = None
        _initialized = False
        _seq_counter = itertools.count()
        _current_turn_id = None
