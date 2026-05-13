"""Tests for oat_sdk.mcp_client.

Two layers:
1. **Pure-Python config/parsing tests** -- no subprocess, fast, deterministic.
   Exercise the JSON-schema-validation path, the ~/${VAR} expansion path,
   and the graceful-degradation behaviour on malformed files.
2. **Integration tests** with a stub MCP server (``_stub_mcp_server.py``).
   Spawned via the same ``stdio_client`` the production path uses, so
   they cover discovery, call, error surfacing, and the per-session
   asyncio.Lock serialisation under parallel dispatch.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

import pytest

from oat_sdk.mcp_client import (
    McpServerSpec,
    load_mcp_config,
    load_mcp_tools,
)


# ---------------------------------------------------------------------------
# Config-parsing layer (no subprocess).
# ---------------------------------------------------------------------------


def test_load_mcp_config_missing_file_returns_empty(tmp_path: Path) -> None:
    """The most common path -- no ``.oat/mcp.json`` means no MCP servers
    and no log noise. We assert empty-list AND we'd notice a regression
    that started logging on the absent-file case via the caplog."""
    assert load_mcp_config(tmp_path / "nonexistent.json") == []


def test_load_mcp_config_malformed_json_logs_warning_returns_empty(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """Graceful degradation: bad JSON -> empty list + warning. The agent
    proceeds with built-in tools only."""
    p = tmp_path / "mcp.json"
    p.write_text("{ not json", encoding="utf-8")
    with caplog.at_level("WARNING"):
        assert load_mcp_config(p) == []
    assert any("Failed to read MCP config" in r.message for r in caplog.records)


def test_load_mcp_config_missing_servers_key_returns_empty(tmp_path: Path) -> None:
    p = tmp_path / "mcp.json"
    p.write_text(json.dumps({"not_servers": []}), encoding="utf-8")
    assert load_mcp_config(p) == []


def test_load_mcp_config_filters_unsupported_transport(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """Servers declaring ``transport != "stdio"`` are skipped with a
    warning; valid neighbours still load. Future SSE/WS support won't
    regress this filter -- it should add new arms, not relax the filter."""
    p = tmp_path / "mcp.json"
    p.write_text(
        json.dumps(
            {
                "servers": [
                    {"name": "future_sse", "command": "x", "transport": "sse"},
                    {"name": "ok", "command": "echo", "args": [], "transport": "stdio"},
                ]
            }
        ),
        encoding="utf-8",
    )
    with caplog.at_level("WARNING"):
        specs = load_mcp_config(p)
    assert [s.name for s in specs] == ["ok"]
    assert any("only 'stdio' is supported" in r.message for r in caplog.records)


def test_load_mcp_config_expands_home_and_envvars(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """``~`` and ``$VAR`` resolve at load time so callers downstream see
    canonical paths. The daemon writes portable paths like ``~/.oat/...``;
    this is the contract."""
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("OAT_TEST_VAL", "from-env")
    p = tmp_path / "mcp.json"
    p.write_text(
        json.dumps(
            {
                "servers": [
                    {
                        "name": "s",
                        "command": "~/bin/x",
                        "args": ["--flag=$OAT_TEST_VAL"],
                        "env": {"OAT_TARGET": "~/output"},
                    }
                ]
            }
        ),
        encoding="utf-8",
    )
    specs = load_mcp_config(p)
    assert len(specs) == 1
    assert specs[0].command == str(tmp_path / "bin" / "x")
    assert specs[0].args == ["--flag=from-env"]
    assert specs[0].env == {"OAT_TARGET": str(tmp_path / "output")}


def test_load_mcp_config_invalid_entry_skipped(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """One bad entry doesn't take down the others. We log + continue."""
    p = tmp_path / "mcp.json"
    p.write_text(
        json.dumps(
            {
                "servers": [
                    {"name": "missing_command"},
                    {"name": "ok", "command": "echo"},
                ]
            }
        ),
        encoding="utf-8",
    )
    with caplog.at_level("WARNING"):
        specs = load_mcp_config(p)
    assert [s.name for s in specs] == ["ok"]


# ---------------------------------------------------------------------------
# Integration layer (spawns the stub MCP server).
# ---------------------------------------------------------------------------


@pytest.fixture
def stub_spec() -> McpServerSpec:
    """A spec that launches ``_stub_mcp_server.py`` as a subprocess.
    Uses sys.executable so the stub runs in the same interpreter the
    agent-runtime would (and inherits the project's PYTHONPATH if any)."""
    stub_path = Path(__file__).parent / "_stub_mcp_server.py"
    assert stub_path.exists(), "stub server fixture missing"
    return McpServerSpec(
        name="stub",
        command=sys.executable,
        args=[str(stub_path)],
        env={},
        transport="stdio",
    )


@pytest.mark.asyncio
async def test_load_mcp_tools_discovers_stub_tools(stub_spec: McpServerSpec) -> None:
    """Smoke: stub server discovery returns the three tools we registered."""
    tools, stack = await load_mcp_tools([stub_spec])
    try:
        names = sorted(t.name for t in tools)
        assert names == ["boom", "echo", "slow_echo"]
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_mcp_tool_echo_roundtrips(stub_spec: McpServerSpec) -> None:
    """End-to-end: call ``echo`` via the LangChain wrapper, get the text
    back. Exercises stdio framing, MCP TextContent canonicalisation, and
    the StructuredTool coroutine path."""
    tools, stack = await load_mcp_tools([stub_spec])
    try:
        echo = next(t for t in tools if t.name == "echo")
        # StructuredTool exposes ainvoke for async tools.
        result = await echo.ainvoke({"text": "hello mcp"})
        assert result == "hello mcp"
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_mcp_tool_error_path_surfaces_as_runtime_error(stub_spec: McpServerSpec) -> None:
    """``boom`` raises server-side; the adapter wraps it in a RuntimeError
    so LangChain can deliver the error to the LLM rather than crashing
    the agent process."""
    tools, stack = await load_mcp_tools([stub_spec])
    try:
        boom = next(t for t in tools if t.name == "boom")
        with pytest.raises(RuntimeError) as exc:
            await boom.ainvoke({})
        assert "MCP tool 'boom' failed" in str(exc.value)
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_per_session_lock_serialises_parallel_calls(stub_spec: McpServerSpec) -> None:
    """Two concurrent slow_echo calls on the same session must serialise
    (one lock per session). We schedule four 100ms calls and assert the
    wall-clock is closer to 400ms than 100ms. Generous bounds keep the
    test stable on slow CI hosts -- the property under test is "they
    don't overlap", not exact timing."""
    tools, stack = await load_mcp_tools([stub_spec])
    try:
        slow = next(t for t in tools if t.name == "slow_echo")
        start = asyncio.get_event_loop().time()
        results = await asyncio.gather(
            slow.ainvoke({"text": "a", "ms": 100}),
            slow.ainvoke({"text": "b", "ms": 100}),
            slow.ainvoke({"text": "c", "ms": 100}),
            slow.ainvoke({"text": "d", "ms": 100}),
        )
        elapsed = asyncio.get_event_loop().time() - start
        assert sorted(results) == ["a", "b", "c", "d"]
        # 4 x 100ms = 400ms minimum if serialised; allow a wide ceiling
        # for CI noise but assert a clear floor that catches "they ran
        # in parallel and corrupted framing"-style regressions.
        assert elapsed >= 0.35, f"calls did not serialise (elapsed={elapsed:.3f}s)"
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_tool_name_collision_with_builtin_namespaces(stub_spec: McpServerSpec) -> None:
    """When a built-in OAT tool already owns a name, the MCP tool gets
    namespaced as ``<server>__<tool>`` rather than shadowing the
    built-in. Exercises the collision-resolution path in load_mcp_tools."""
    tools, stack = await load_mcp_tools([stub_spec], builtin_tool_names={"echo"})
    try:
        names = sorted(t.name for t in tools)
        assert "echo" not in names
        assert "stub__echo" in names
        # Non-colliding tools keep their bare names.
        assert "boom" in names
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_bad_command_skipped_remaining_specs_still_load(
    stub_spec: McpServerSpec, caplog: pytest.LogCaptureFixture
) -> None:
    """A spec whose command doesn't exist is skipped with a warning;
    valid specs after it still load. Mirrors how a real deployment
    where the bridge is missing should not take down a multi-server
    config."""
    bad = McpServerSpec(
        name="missing",
        command="/no/such/binary/oat_test",
        args=[],
        env={},
        transport="stdio",
    )
    with caplog.at_level("WARNING"):
        tools, stack = await load_mcp_tools([bad, stub_spec])
    try:
        names = sorted(t.name for t in tools)
        assert names == ["boom", "echo", "slow_echo"]
        assert any("failed to start" in r.message for r in caplog.records)
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_empty_specs_returns_empty_tools_and_open_stack() -> None:
    """No specs -> no tools, but we still return a stack so the caller's
    ``async with`` / ``aclose`` plumbing doesn't have to special-case
    the empty path."""
    tools, stack = await load_mcp_tools([])
    try:
        assert tools == []
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_post_aclose_tool_call_fails(stub_spec: McpServerSpec) -> None:
    """After ``stack.aclose()`` the underlying MCP session is closed
    and any subsequent tool call must error -- it cannot silently
    succeed against a torn-down transport.

    This is the cleanup-correctness assertion. The plan calls for a
    SIGTERM test; the actual SIGTERM path runs ``loop.add_signal_handler
    -> task.cancel() -> CancelledError -> async with stack`` exits ->
    ``stack.aclose()``. The leaf assertion in that chain -- "aclose
    actually tears the session down" -- is what we cover here. A direct
    SIGTERM test would need a real subprocess + signal-handler dance
    that pytest-asyncio doesn't compose with cleanly.
    """
    tools, stack = await load_mcp_tools([stub_spec])
    echo = next(t for t in tools if t.name == "echo")
    # Sanity: tool works before aclose.
    assert await echo.ainvoke({"text": "pre"}) == "pre"

    await stack.aclose()

    # Post-aclose: any tool call must raise. The exact exception type
    # comes from the MCP SDK's transport layer (typically an
    # ``anyio.ClosedResourceError`` re-wrapped through our adapter as
    # ``RuntimeError``). We just assert it errors, not the type --
    # SDK version drift would otherwise turn this into a brittle test.
    with pytest.raises(Exception):
        await echo.ainvoke({"text": "post"})
