"""Schema for events emitted over the agent astream sidecar socket.

The Python agent taps LangGraph's ``astream`` loop and emits structured
events to a Unix socket. The Go daemon subscribes and forwards them to
the TUI. This replaces PTY-scraping for chat content, so the 512-slot
broadcaster channel can no longer silently drop assistant responses
under chrome-redraw flood.

Wire format: newline-delimited JSON. Each line is a complete envelope::

    {"v":1,"seq":42,"ts":1714000000000,"turn_id":"abc123",
     "kind":"assistant_message",
     "data":{"content":"hello","usage":{...}}}

Forward-compat contract: any consumer that doesn't recognize ``kind``
MUST log and skip, never raise. The Go side follows the same rule — see
``pkg/sidecar/events.go``. Both sides must evolve in lockstep.
"""
from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass, field
from typing import Any, Optional

SCHEMA_VERSION = 1

KIND_TURN_START = "turn_start"
KIND_TURN_END = "turn_end"
KIND_ASSISTANT_DELTA = "assistant_delta"
KIND_ASSISTANT_MESSAGE = "assistant_message"
KIND_TOOL_CALL = "tool_call"
KIND_TOOL_RESULT = "tool_result"
KIND_INTERRUPT = "interrupt"
KIND_TOKEN_USAGE = "token_usage"


@dataclass
class Usage:
    """Token usage for an assistant message. Mirrors LangGraph ``usage_metadata``."""

    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    cache_creation_tokens: int = 0


@dataclass
class Event:
    """Envelope for a single sidecar event.

    ``seq`` is monotonic per-connection (NOT per-turn) — gaps indicate drops
    on the wire. ``ts`` is unix milliseconds for easy cross-language diff.
    ``turn_id`` is optional because a few events (e.g., model banners, idle
    heartbeats) are not scoped to a turn.
    """

    seq: int
    kind: str
    data: dict[str, Any]
    ts: int = field(default_factory=lambda: int(time.time() * 1000))
    turn_id: Optional[str] = None
    v: int = SCHEMA_VERSION

    def to_json(self) -> str:
        """Serialize to a single line of JSON (no trailing newline).

        Caller is responsible for appending the framing ``\\n``. Keys are
        emitted in a stable order so byte-for-byte parity tests work.
        """
        payload: dict[str, Any] = {
            "v": self.v,
            "seq": self.seq,
            "ts": self.ts,
            "kind": self.kind,
            "data": self.data,
        }
        if self.turn_id is not None:
            payload["turn_id"] = self.turn_id
        return json.dumps(payload, ensure_ascii=False, separators=(",", ":"))

    @classmethod
    def from_json(cls, s: str) -> Event:
        """Parse a JSON line into an Event.

        Raises ``ValueError`` for malformed JSON and ``KeyError`` if any of
        the required envelope fields (``v``, ``seq``, ``ts``, ``kind``) is
        missing. Unknown ``kind`` values succeed here — dispatch happens
        above this layer so forward-compat is preserved.
        """
        d = json.loads(s)
        return cls(
            v=d["v"],
            seq=d["seq"],
            ts=d["ts"],
            kind=d["kind"],
            data=d.get("data", {}),
            turn_id=d.get("turn_id"),
        )


# ---------- factory helpers ----------
#
# One per kind. Keep these thin — they exist to (a) set the right ``kind``
# string and (b) enforce the payload shape so callers can't typo a key.


def turn_start(seq: int, turn_id: str, user_input: str) -> Event:
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_TURN_START,
        data={"user_input": user_input},
    )


def turn_end(seq: int, turn_id: str) -> Event:
    return Event(seq=seq, turn_id=turn_id, kind=KIND_TURN_END, data={})


def assistant_delta(seq: int, turn_id: str, content: str) -> Event:
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_ASSISTANT_DELTA,
        data={"content": content},
    )


def assistant_message(
    seq: int, turn_id: str, content: str, usage: Optional[Usage] = None,
) -> Event:
    data: dict[str, Any] = {"content": content}
    if usage is not None:
        data["usage"] = asdict(usage)
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_ASSISTANT_MESSAGE, data=data,
    )


def tool_call(
    seq: int, turn_id: str, name: str, args: dict[str, Any], call_id: str,
) -> Event:
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_TOOL_CALL,
        data={"name": name, "args": args, "call_id": call_id},
    )


def tool_result(
    seq: int, turn_id: str, call_id: str, content: str,
    error: Optional[str] = None,
) -> Event:
    data: dict[str, Any] = {"call_id": call_id, "content": content}
    if error is not None:
        data["error"] = error
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_TOOL_RESULT, data=data,
    )


def interrupt(
    seq: int, turn_id: str, interrupt_kind: str, prompt: str,
) -> Event:
    """Human-in-the-loop interrupt event.

    ``interrupt_kind`` is the payload's ``kind`` field (e.g., "approval",
    "hitl_input"). Renamed from ``kind`` to avoid shadowing the envelope
    field of the same name.
    """
    return Event(
        seq=seq, turn_id=turn_id, kind=KIND_INTERRUPT,
        data={"kind": interrupt_kind, "prompt": prompt},
    )


def token_usage(
    seq: int,
    turn_id: Optional[str],
    delta_input: int,
    delta_output: int,
    cumulative_input: int,
    cumulative_output: int,
    cache_read: int = 0,
    cache_creation: int = 0,
) -> Event:
    """Authoritative token-usage update for the daemon's accounting path.

    Payload shape is 1:1 with the existing ``[OAT_TOKENS]`` stdout sentinel
    (see ``_emit_oat_tokens`` in ``textual_adapter.py``), so the Go daemon's
    existing ``handleTokenUsageEvent`` can be fed from this sidecar source
    with no schema adaptation. The cache_* fields are omitempty to match
    the sentinel's existing conditional-emit behavior.

    Why this matters: the PTY-borne ``[OAT_TOKENS]`` line currently rides
    through the 512-slot rawBroadcaster channel that silently drops under
    chrome flood. Emitting the same payload via the sidecar (which uses
    blocking-send-with-timeout, never silent-drop) gives the daemon a
    lossless source for cost accounting. During migration both paths
    coexist; after sidecar-on is the default, the PTY sentinel can be
    retired in a follow-up.

    ``turn_id`` may be None for pre-turn or aggregated emissions.
    """
    data: dict[str, Any] = {
        "delta_input": delta_input,
        "delta_output": delta_output,
        "cumulative_input": cumulative_input,
        "cumulative_output": cumulative_output,
    }
    if cache_read:
        data["cache_read"] = cache_read
    if cache_creation:
        data["cache_creation"] = cache_creation
    return Event(seq=seq, turn_id=turn_id, kind=KIND_TOKEN_USAGE, data=data)
