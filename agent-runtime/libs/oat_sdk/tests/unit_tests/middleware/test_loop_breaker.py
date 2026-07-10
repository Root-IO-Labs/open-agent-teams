"""Unit tests for the Phase 10 loop-breaker middleware."""

from __future__ import annotations

from types import SimpleNamespace
from typing import TYPE_CHECKING, cast

from langchain_core.messages import ToolMessage

from oat_sdk.middleware.loop_breaker import LoopBreakerMiddleware, loop_breaker_max

if TYPE_CHECKING:
    from langgraph.prebuilt.tool_node import ToolCallRequest


def _req(tool_name: str) -> ToolCallRequest:
    """Minimal stand-in for ToolCallRequest (only .tool_call is read)."""
    return cast(
        "ToolCallRequest",
        SimpleNamespace(tool_call={"name": tool_name, "id": "call_1", "args": {}}),
    )


def _err(code: str | None = "REF_STALE", tool_call_id: str = "call_1") -> ToolMessage:
    body = f'{{"code":"{code}","message":"boom","retryable":true}}' if code else "boom"
    return ToolMessage(content=body, tool_call_id=tool_call_id, name="t", status="error")


def _ok(tool_call_id: str = "call_1") -> ToolMessage:
    return ToolMessage(content='{"ok":true}', tool_call_id=tool_call_id, name="t")


def _run(mw: LoopBreakerMiddleware, tool_name: str, result: ToolMessage) -> ToolMessage:
    return mw.wrap_tool_call(_req(tool_name), lambda _r: result)


def test_trips_after_threshold_identical_failures() -> None:
    mw = LoopBreakerMiddleware(max_repeats=3)
    # First two identical failures pass through unchanged.
    assert _run(mw, "browser_click", _err()).content.startswith('{"code"')
    assert _run(mw, "browser_click", _err()).content.startswith('{"code"')
    # Third trips: the directive replaces the raw error.
    tripped = _run(mw, "browser_click", _err())
    assert "[OAT loop-breaker]" in tripped.content
    assert "browser_click" in tripped.content
    assert tripped.status == "error"


def test_success_resets_the_streak() -> None:
    mw = LoopBreakerMiddleware(max_repeats=3)
    _run(mw, "browser_click", _err())
    _run(mw, "browser_click", _err())
    # A success in the middle resets progress...
    _run(mw, "browser_click", _ok())
    # ...so two more failures still do NOT trip.
    assert "[OAT loop-breaker]" not in _run(mw, "browser_click", _err()).content
    assert "[OAT loop-breaker]" not in _run(mw, "browser_click", _err()).content


def test_different_error_code_resets_streak() -> None:
    mw = LoopBreakerMiddleware(max_repeats=3)
    _run(mw, "browser_click", _err("REF_STALE"))
    _run(mw, "browser_click", _err("REF_STALE"))
    # A different error code is a different signature → counter resets to 1.
    assert "[OAT loop-breaker]" not in _run(mw, "browser_click", _err("SESSION_EXPIRED")).content


def test_polling_tool_never_trips() -> None:
    mw = LoopBreakerMiddleware(max_repeats=3)
    for _ in range(10):
        out = _run(mw, "browser_wait_for", _err("TIMEOUT"))
        assert "[OAT loop-breaker]" not in out.content


def test_disabled_when_max_le_zero() -> None:
    mw = LoopBreakerMiddleware(max_repeats=0)
    for _ in range(10):
        out = _run(mw, "browser_click", _err())
        assert "[OAT loop-breaker]" not in out.content


def test_retrips_after_reset_window() -> None:
    mw = LoopBreakerMiddleware(max_repeats=2)
    _run(mw, "browser_click", _err())
    assert "[OAT loop-breaker]" in _run(mw, "browser_click", _err()).content
    # After a trip the counter resets, giving the model a fresh window; if it
    # STILL repeats the same failure twice more, it trips again.
    _run(mw, "browser_click", _err())
    assert "[OAT loop-breaker]" in _run(mw, "browser_click", _err()).content


def test_loop_breaker_max_env(monkeypatch) -> None:
    monkeypatch.setenv("OAT_LOOP_BREAKER_MAX", "5")
    assert loop_breaker_max() == 5
    monkeypatch.setenv("OAT_LOOP_BREAKER_MAX", "garbage")
    assert loop_breaker_max() == 3  # falls back to default
    monkeypatch.delenv("OAT_LOOP_BREAKER_MAX", raising=False)
    assert loop_breaker_max() == 3


async def test_async_wrap_trips() -> None:
    mw = LoopBreakerMiddleware(max_repeats=2)

    async def handler(_r: ToolCallRequest) -> ToolMessage:
        return _err()

    await mw.awrap_tool_call(_req("browser_click"), handler)
    tripped = await mw.awrap_tool_call(_req("browser_click"), handler)
    assert "[OAT loop-breaker]" in tripped.content
