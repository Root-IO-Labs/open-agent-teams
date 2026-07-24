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

from oat_cli.sidecar_client import SidecarClient
from oat_cli.sidecar_events import (
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
        c.emit(
            turn_start(
                seq=_next_seq(),
                turn_id=turn_id,
                user_input=user_input,
            )
        )
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_turn_start failed: %s", e)
    return turn_id


def emit_turn_end() -> None:
    """Emit a turn_end event for the current turn_id, then clear it.

    Also writes a dedicated ``[OAT_TURN_END]`` sentinel to ``OAT_TOOL_LOG``
    (the file the daemon's assistant-turn tailer reads) so the daemon can
    detect the end of a silent, tool-only turn and stop the side panel's
    spinner / run its self-healing recovery ladder. This is DISTINCT from
    ``[OAT_TOKENS]`` on purpose: ``[OAT_TOKENS]`` is also emitted right after
    a mid-turn compaction, so keying turn-end on it would false-fire; this
    sentinel fires only from ``emit_turn_end``, which the adapter calls
    exactly once per turn on every completion path (incl. the app.py
    exception-path finally). The ``tid is not None`` guard makes the write
    exactly-once: ``set_turn_id(None)`` below means the idempotent finally
    call is a no-op.
    """
    # Drop any open arg-generation timer heartbeats so a completed /
    # interrupted turn cannot keep pulsing into the next one.
    stop_all_generating_pulses()
    c = _get_client()
    tid = _turn_id()
    if tid is not None:
        if c is not None:
            try:
                c.emit(turn_end(seq=_next_seq(), turn_id=tid))
            except Exception as e:  # noqa: BLE001
                _log.warning("sidecar_emitter: emit_turn_end failed: %s", e)
        # Best-effort sentinel append; a logging/IO failure must never
        # interrupt the agent (mirrors _emit_oat_tokens' direct-write path).
        log_path = os.environ.get("OAT_TOOL_LOG")
        if log_path:
            try:
                with open(log_path, "a", encoding="utf-8") as f:
                    f.write(f"[OAT_TURN_END] {tid}\n")
                    f.flush()
            except OSError:
                pass
    set_turn_id(None)


# Rate-limit state for [OAT_GENERATING] heartbeats (UI/observability only —
# never a model prompt). Keyed by sanitized tool name.
_gen_last_mono: dict[str, float] = {}
_GEN_MIN_INTERVAL_S = 2.0
_GEN_BYTES_MAX = 50_000_000

# Per-buffer timer heartbeats: some providers buffer entire tool-arg bodies
# with few/no further tool_call_chunks. A background ticker keeps emitting
# [OAT_GENERATING] so the panel/bridge silence clock does not fire "no
# activity" while args are still being generated. Keyed by buffer id/index.
_pulse_lock = threading.Lock()
_pulse_state: dict[str, dict[str, Any]] = {}
_pulse_thread: threading.Thread | None = None
_pulse_wake = threading.Event()


def _valid_gen_tool_name(name: str) -> bool:
    if not name or len(name) > 64:
        return False
    for ch in name:
        if not (ch.isalnum() or ch in "_.-"):
            return False
    return True


def emit_generating(tool: str, byte_count: int = 0) -> None:
    """Append ``[OAT_GENERATING] {"tool","bytes"}`` to ``OAT_TOOL_LOG``.

    UI/observability only: the daemon forwards this so the side panel can
    show elapsed "writing…" progress while tool args are still streaming.
    Never injected into the model context / PTY as a prompt. Rate-limited
    to ~2s per tool name. Best-effort; never raises.
    """
    log_path = os.environ.get("OAT_TOOL_LOG")
    if not log_path:
        return
    try:
        name = str(tool or "").strip()
        if not _valid_gen_tool_name(name):
            return
        try:
            n = int(byte_count)
        except (TypeError, ValueError):
            return
        if n < 0:
            n = 0
        if n > _GEN_BYTES_MAX:
            n = _GEN_BYTES_MAX
        import time as _time

        now = _time.monotonic()
        last = _gen_last_mono.get(name, 0.0)
        if now - last < _GEN_MIN_INTERVAL_S:
            return
        _gen_last_mono[name] = now
        import json as _json

        payload = _json.dumps(
            {"tool": name, "bytes": n},
            ensure_ascii=False,
            separators=(",", ":"),
        )
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(f"[OAT_GENERATING] {payload}\n")
            f.flush()
    except (OSError, TypeError, ValueError) as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: emit_generating failed: %s", e)


def _ensure_pulse_thread_unlocked() -> None:
    """Start the generating-pulse ticker if needed. Caller holds ``_pulse_lock``."""
    global _pulse_thread
    if _pulse_thread is not None and _pulse_thread.is_alive():
        return
    t = threading.Thread(
        target=_generating_pulse_loop,
        name="oat-generating-pulse",
        daemon=True,
    )
    _pulse_thread = t
    t.start()


def _generating_pulse_loop() -> None:
    """Emit ``[OAT_GENERATING]`` every ~2s for open arg buffers until empty."""
    global _pulse_thread
    try:
        while True:
            _pulse_wake.wait(timeout=_GEN_MIN_INTERVAL_S)
            _pulse_wake.clear()
            with _pulse_lock:
                items = [(k, dict(v)) for k, v in _pulse_state.items()]
                if not items:
                    _pulse_thread = None
                    return
            for _key, st in items:
                tool = st.get("tool")
                if not isinstance(tool, str):
                    continue
                try:
                    n = int(st.get("bytes") or 0)
                except (TypeError, ValueError):
                    n = 0
                emit_generating(tool, n)
    except Exception as e:  # noqa: BLE001 — never take down the agent
        _log.warning("sidecar_emitter: generating pulse loop failed: %s", e)
        with _pulse_lock:
            _pulse_thread = None


def start_generating_pulse(key: str | int, tool: str) -> None:
    """Start timer heartbeats for an incomplete tool-arg buffer.

    Call when the tool name is known but args are not yet parsed. Cancel
    with ``stop_generating_pulse`` / ``stop_all_generating_pulses`` when
    args parse, the stream ends, or the turn is interrupted. Best-effort;
    never raises.
    """
    try:
        name = str(tool or "").strip()
        if not _valid_gen_tool_name(name):
            return
        key_s = str(key)
        with _pulse_lock:
            prev = _pulse_state.get(key_s)
            bytes_n = int(prev.get("bytes") or 0) if isinstance(prev, dict) else 0
            _pulse_state[key_s] = {"tool": name, "bytes": bytes_n}
            _ensure_pulse_thread_unlocked()
        _pulse_wake.set()
    except Exception as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: start_generating_pulse failed: %s", e)


def set_generating_pulse_bytes(key: str | int, byte_count: int) -> None:
    """Update the byte count shown on the next timer pulse. Never raises."""
    try:
        key_s = str(key)
        try:
            n = int(byte_count)
        except (TypeError, ValueError):
            return
        if n < 0:
            n = 0
        if n > _GEN_BYTES_MAX:
            n = _GEN_BYTES_MAX
        with _pulse_lock:
            st = _pulse_state.get(key_s)
            if st is None:
                return
            st["bytes"] = n
    except Exception as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: set_generating_pulse_bytes failed: %s", e)


def stop_generating_pulse(key: str | int) -> None:
    """Stop timer heartbeats for one tool-arg buffer. Never raises."""
    try:
        with _pulse_lock:
            _pulse_state.pop(str(key), None)
    except Exception as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: stop_generating_pulse failed: %s", e)


def stop_generating_pulses_for_tool(tool: str) -> None:
    """Stop every generating pulse whose tool name matches.

    Called when a RESULT lands so leftover timer heartbeats cannot
    emit post-RESULT ``[OAT_GENERATING]`` lines (those reopen an
    orphan RUNNING activity row in the side panel). Never raises.
    """
    try:
        name = str(tool or "").strip()
        if not _valid_gen_tool_name(name):
            return
        keys: list[str] = []
        with _pulse_lock:
            keys = [k for k, st in _pulse_state.items() if st.get("tool") == name]
            for k in keys:
                _pulse_state.pop(k, None)
        if keys:
            _pulse_wake.set()
    except Exception as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: stop_generating_pulses_for_tool failed: %s", e)


def stop_all_generating_pulses() -> None:
    """Stop all generating timer heartbeats (stream end / interrupt). Never raises."""
    try:
        with _pulse_lock:
            _pulse_state.clear()
        _pulse_wake.set()
    except Exception as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: stop_all_generating_pulses failed: %s", e)


def emit_todos(todos: Any) -> None:
    """Write a dedicated ``[OAT_TODOS] <json>`` line to ``OAT_TOOL_LOG``.

    Carries the full plan/checklist so the daemon can forward it to the
    side panel's live todo card — the parser's generic tool-arg preview is
    truncated to 200 bytes, which is why the card can't ride the ordinary
    ``TOOL: write_todos`` block. Mirrors ``emit_turn_end``'s best-effort,
    never-raise, direct-append pattern.

    ``todos`` is the raw ``write_todos`` arg dict (``{"todos": [...]}``) or
    a bare list. Items are normalized to ``{content, status, activeForm}``,
    the count is bounded, and each string field is length-capped here so a
    runaway plan can't bloat the log line (the daemon re-bounds on its side
    too — defense in depth). A malformed shape emits nothing.
    """
    log_path = os.environ.get("OAT_TOOL_LOG")
    if not log_path:
        return
    try:
        items = todos.get("todos") if isinstance(todos, dict) else todos
        if not isinstance(items, list):
            return
        max_items = 50
        max_field = 500
        normalized: list[dict[str, str]] = []
        for it in items[:max_items]:
            if not isinstance(it, dict):
                continue
            content = str(it.get("content", ""))[:max_field]
            status = str(it.get("status", "pending"))[:32]
            active_form = str(it.get("activeForm", ""))[:max_field]
            normalized.append(
                {"content": content, "status": status, "activeForm": active_form}
            )
        # An empty list is meaningful (plan cleared) — still emit it. Only a
        # non-list shape bails above.
        import json as _json

        payload = _json.dumps(normalized, ensure_ascii=False, separators=(",", ":"))
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(f"[OAT_TODOS] {payload}\n")
            f.flush()
    except (OSError, TypeError, ValueError) as e:  # noqa: BLE001 — never raise
        _log.warning("sidecar_emitter: emit_todos failed: %s", e)


def emit_assistant_delta(content: str) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(
            assistant_delta(
                seq=_next_seq(),
                turn_id=_turn_id() or "",
                content=content,
            )
        )
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_assistant_delta failed: %s", e)


def emit_assistant_message(
    content: str,
    usage: Optional[Usage] = None,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(
            assistant_message(
                seq=_next_seq(),
                turn_id=_turn_id() or "",
                content=content,
                usage=usage,
            )
        )
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_assistant_message failed: %s", e)


def emit_tool_call(
    name: str,
    args: dict[str, Any],
    call_id: str,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(
            tool_call(
                seq=_next_seq(),
                turn_id=_turn_id() or "",
                name=name,
                args=args,
                call_id=call_id,
            )
        )
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_tool_call failed: %s", e)


def emit_tool_result(
    call_id: str,
    content: str,
    error: Optional[str] = None,
) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(
            tool_result(
                seq=_next_seq(),
                turn_id=_turn_id() or "",
                call_id=call_id,
                content=content,
                error=error,
            )
        )
    except Exception as e:  # noqa: BLE001
        _log.warning("sidecar_emitter: emit_tool_result failed: %s", e)


def emit_interrupt(interrupt_kind: str, prompt: str) -> None:
    c = _get_client()
    if c is None:
        return
    try:
        c.emit(
            make_interrupt(
                seq=_next_seq(),
                turn_id=_turn_id() or "",
                interrupt_kind=interrupt_kind,
                prompt=prompt,
            )
        )
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
