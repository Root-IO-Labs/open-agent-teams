"""Loop-breaker middleware (Phase 10).

Detects when the agent retries the same tool and gets the same error class N
times in a row with no intervening progress — the "keeps getting stuck and never
says why" failure John hit (a REF_STALE spiral after navigation, a repeated
auth/session-expiry error). On tripping, it replaces the identical Nth failure
with a clear directive telling the model to STOP retrying and report to the user
what it tried and the error, instead of silently grinding.

Design decisions (see the plan's "Phase 10" + "Edge cases"):
- **Model-agnostic seat.** It lives in the agent runtime's tool-call loop
  (``wrap_tool_call``), so it protects any model, not just Qwen.
- **No false positives on legitimate polling.** ``browser_wait_for`` and similar
  wait/poll tools repeat by design, so they never trip. The counter also resets
  on ANY successful tool call (progress) or when the (tool, error-code) changes.
- **Advisory, not a hard halt.** Rather than force-ending the graph (risky), the
  tripped result is a strong directive returned as the tool result. Combined
  with the Phase 2 interrupt and the browser.md prompt reinforcement, this turns
  a silent spin into an explicit "I'm stuck because X" message the user sees.
- **Feature-flagged.** ``OAT_LOOP_BREAKER_MAX`` sets the threshold (default 3);
  ``<= 0`` disables the trip entirely (bring-your-own-model / tuning escape
  hatch), matching the ``OAT_MAX_REJECTIONS`` philosophy.
"""

from __future__ import annotations

import logging
import os
import re
from typing import TYPE_CHECKING, Any

from langchain.agents.middleware.types import AgentMiddleware
from langchain_core.messages import ToolMessage

if TYPE_CHECKING:
    from collections.abc import Awaitable, Callable

    from langgraph.prebuilt.tool_node import ToolCallRequest
    from langgraph.types import Command

logger = logging.getLogger(__name__)

# Default trip threshold: 3 identical consecutive same-tool+same-error failures.
_DEFAULT_MAX_REPEATS = 3

# Tools whose repetition is legitimate (polling / waiting) and must never count
# toward the loop-breaker trip.
_DEFAULT_POLL_TOOLS = frozenset({"browser_wait_for"})

# Extracts a bridge-style error code from a tool result body,
# e.g. {"code":"REF_STALE", ...} -> "REF_STALE".
_ERROR_CODE_RE = re.compile(r'"code"\s*:\s*"([A-Z][A-Z0-9_]+)"')


def loop_breaker_max() -> int:
    """Resolve the trip threshold from ``OAT_LOOP_BREAKER_MAX`` (default 3).

    A value ``<= 0`` (or unparseable) disables the trip: the middleware still
    runs but never rewrites a result, so behavior is identical to not having it.

    Returns:
        The number of consecutive identical failures that trips the breaker,
        or ``0`` to disable.
    """
    raw = os.environ.get("OAT_LOOP_BREAKER_MAX")
    if raw is None or raw.strip() == "":
        return _DEFAULT_MAX_REPEATS
    try:
        return int(raw.strip())
    except ValueError:
        logger.warning("Invalid OAT_LOOP_BREAKER_MAX=%r; using default %d", raw, _DEFAULT_MAX_REPEATS)
        return _DEFAULT_MAX_REPEATS


def _error_signature(result: Any) -> str | None:  # noqa: ANN401
    """Return an error-class signature for a tool result, or None if it succeeded.

    A result counts as an error when its ``status`` is ``"error"`` OR its body
    carries a bridge-style ``"code":"SOMETHING"`` field (browser tools return
    REF_STALE / session errors as content even when the call didn't raise). The
    signature is the error code when available, else a generic ``ERROR`` bucket,
    so a repeated same-code failure is detected while a changing error resets.

    Args:
        result: The value returned by the wrapped tool handler.

    Returns:
        An uppercase error-code signature, or ``None`` when the result is a
        success (or a value we can't classify, e.g. a ``Command``).
    """
    # A Command (control-flow return) isn't a classifiable tool failure.
    if not isinstance(result, ToolMessage):
        return None

    content = result.content if isinstance(result.content, str) else str(result.content)
    code_match = _ERROR_CODE_RE.search(content)

    is_error = getattr(result, "status", None) == "error"
    if not is_error and code_match is None:
        return None  # success / progress

    if code_match is not None:
        return code_match.group(1)
    return "ERROR"


class LoopBreakerMiddleware(AgentMiddleware):
    """Stops same-tool+same-error retry spirals and reports instead of grinding.

    Instance state is per-agent (one middleware instance per compiled agent) and
    tool calls in a single reasoning loop are sequential, so the simple counter
    below needs no locking.
    """

    def __init__(
        self,
        *,
        max_repeats: int | None = None,
        poll_tools: frozenset[str] = _DEFAULT_POLL_TOOLS,
    ) -> None:
        """Initialize the loop-breaker.

        Args:
            max_repeats: Trip threshold; defaults to ``OAT_LOOP_BREAKER_MAX`` /
                ``3``. ``<= 0`` disables the trip.
            poll_tools: Tool names that repeat by design and must never trip.
        """
        super().__init__()
        self._max_repeats = max_repeats if max_repeats is not None else loop_breaker_max()
        self._poll_tools = poll_tools
        self._last_key: tuple[str, str] | None = None
        self._count = 0

    def _observe(self, tool_name: str, result: Any) -> ToolMessage | None:  # noqa: ANN401
        """Update the run-length counter and return a directive if we should trip.

        Returns a replacement ``ToolMessage`` when the breaker trips, else
        ``None`` (leave the real result untouched).
        """
        sig = _error_signature(result)
        if sig is None:
            # Progress (or unclassifiable success) — reset the streak.
            self._last_key = None
            self._count = 0
            return None

        key = (tool_name, sig)
        if key == self._last_key:
            self._count += 1
        else:
            self._last_key = key
            self._count = 1

        # Disabled, below threshold, or a legitimate polling tool → never trip.
        if self._max_repeats <= 0 or self._count < self._max_repeats or tool_name in self._poll_tools:
            return None

        # Trip. Reset the count so the model gets a fresh window to change course
        # (we don't want to re-trip on every subsequent call and drown it out).
        self._count = 0
        logger.warning(
            "loop-breaker tripped: tool=%s error=%s repeated %dx with no progress; injecting stop-and-report directive",
            tool_name,
            sig,
            self._max_repeats,
        )
        return self._directive(result, tool_name, sig)

    @staticmethod
    def _directive(result: ToolMessage, tool_name: str, sig: str) -> ToolMessage:
        """Build the stop-and-report directive returned in place of the Nth failure."""
        return ToolMessage(
            content=(
                f"[OAT loop-breaker] The tool `{tool_name}` has failed with the same error "
                f"`{sig}` several times in a row with no progress. STOP calling `{tool_name}` "
                "the same way — do NOT retry it again as-is.\n\n"
                "Instead, do ALL of the following now:\n"
                "1. Tell the user, in plain text, that you are stuck: exactly what you were "
                "trying to do, the action you repeated, and this error.\n"
                "2. If a stale element reference could be the cause, re-observe or re-snapshot "
                "the page to get fresh references before any further action.\n"
                "3. Otherwise, ask the user how to proceed or try a clearly DIFFERENT strategy — "
                "do not repeat the failed action."
            ),
            tool_call_id=result.tool_call_id,
            name=getattr(result, "name", tool_name),
            status="error",
        )

    def wrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], ToolMessage | Command[Any]],
    ) -> ToolMessage | Command[Any]:
        """Sync tool-call interceptor: run the tool, then maybe trip the breaker."""
        result = handler(request)
        tool_name = str(request.tool_call.get("name", ""))
        directive = self._observe(tool_name, result)
        return directive if directive is not None else result

    async def awrap_tool_call(
        self,
        request: ToolCallRequest,
        handler: Callable[[ToolCallRequest], Awaitable[ToolMessage | Command[Any]]],
    ) -> ToolMessage | Command[Any]:
        """Async tool-call interceptor: run the tool, then maybe trip the breaker."""
        result = await handler(request)
        tool_name = str(request.tool_call.get("name", ""))
        directive = self._observe(tool_name, result)
        return directive if directive is not None else result
