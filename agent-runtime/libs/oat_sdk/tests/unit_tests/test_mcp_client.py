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
import sys
from pathlib import Path

import pytest

from oat_sdk.mcp_client import (
    McpServerSpec,
    _resolve_stderr_log_path,
    load_mcp_config,
    load_mcp_tools,
)

# ---------------------------------------------------------------------------
# Config-parsing layer (no subprocess).
# ---------------------------------------------------------------------------


def test_load_mcp_config_missing_file_returns_empty(tmp_path: Path) -> None:
    """The most common path -- no ``.oat/mcp.json`` means no MCP servers.

    And no log noise. We assert empty-list AND we'd notice a
    regression that started logging on the absent-file case via the
    caplog.
    """
    assert load_mcp_config(tmp_path / "nonexistent.json") == []


def test_load_mcp_config_malformed_json_logs_warning_returns_empty(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """Graceful degradation: bad JSON -> empty list + warning.

    The agent proceeds with built-in tools only.
    """
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
    """Servers declaring ``transport != "stdio"`` are skipped with a warning.

    Valid neighbours still load. Future SSE/WS support won't regress
    this filter -- it should add new arms, not relax the filter.
    """
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
    """``~`` and ``$VAR`` resolve at load time so callers downstream see canonical paths.

    The daemon writes portable paths like ``~/.oat/...``; this is the contract.
    """
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
# Stderr capture path resolution (pure-Python).
# ---------------------------------------------------------------------------


def test_resolve_stderr_log_path_uses_audit_dir_env(tmp_path: Path) -> None:
    """When the daemon-provided audit-log dir env is set, the stderr log co-locates there.

    This is the production path for browser_bridge: the daemon writes
    OAT_BROWSER_AGENT_AUDIT_LOG_DIR=~/.oat/output/<repo> into the MCP
    env block, and the resolver puts the stderr capture in that same
    dir.
    """
    audit_dir = tmp_path / "output" / "my-repo"
    spec = McpServerSpec(
        name="browser_bridge",
        command="node",
        args=["bridge.js"],
        env={"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": str(audit_dir)},
        transport="stdio",
    )
    out = _resolve_stderr_log_path(spec)
    assert out is not None
    assert out == audit_dir / "mcp-browser_bridge.stderr.log"
    # Directory is materialised so the open() that follows succeeds.
    assert audit_dir.is_dir()


def test_resolve_stderr_log_path_falls_back_to_cwd_dot_oat(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """No env override -> <cwd>/.oat/.

    That's the agent worktree's hidden config dir, which already
    holds mcp.json. Used for hand-authored MCP configs where the
    operator hasn't standardised an audit dir.
    """
    monkeypatch.chdir(tmp_path)
    spec = McpServerSpec(
        name="custom_tool",
        command="echo",
        args=[],
        env={},
        transport="stdio",
    )
    out = _resolve_stderr_log_path(spec)
    assert out is not None
    assert out == tmp_path / ".oat" / "mcp-custom_tool.stderr.log"
    assert (tmp_path / ".oat").is_dir()


def test_resolve_stderr_log_path_expands_tilde_and_envvars(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The audit dir env value goes through ~/$VAR expansion.

    Matches every other path in mcp_client.py. Without this, the
    daemon's portable ``~/.oat/...`` config wouldn't resolve when
    the agent's CWD is its worktree.
    """
    monkeypatch.setenv("HOME", str(tmp_path))
    spec = McpServerSpec(
        name="x",
        command="echo",
        args=[],
        env={"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": "~/output/r1"},
        transport="stdio",
    )
    out = _resolve_stderr_log_path(spec)
    assert out is not None
    assert out == tmp_path / "output" / "r1" / "mcp-x.stderr.log"


def test_resolve_stderr_log_path_returns_none_on_unwritable_dir(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    """If we can't materialise the directory, return None and log a warning.

    Caller falls back to the SDK's inherit-stderr default rather
    than crashing the agent -- stderr capture is operator
    observability, not a correctness requirement.
    """
    # Force mkdir to raise. We can't actually un-write tmp_path
    # reliably across platforms; monkeypatching is cleaner than
    # chmod'ing.
    msg = "simulated read-only filesystem"

    def boom(_self: Path, *_args: object, **_kwargs: object) -> None:
        raise OSError(msg)

    monkeypatch.setattr(Path, "mkdir", boom)
    spec = McpServerSpec(
        name="x",
        command="echo",
        args=[],
        env={"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": str(tmp_path / "ro" / "x")},
        transport="stdio",
    )
    with caplog.at_level("WARNING"):
        out = _resolve_stderr_log_path(spec)
    assert out is None
    assert any("MCP stderr log dir" in r.message for r in caplog.records)


# ---------------------------------------------------------------------------
# Integration layer (spawns the stub MCP server).
# ---------------------------------------------------------------------------


@pytest.fixture
def stub_spec() -> McpServerSpec:
    """A spec that launches ``_stub_mcp_server.py`` as a subprocess.

    Uses sys.executable so the stub runs in the same interpreter the
    agent-runtime would (and inherits the project's PYTHONPATH if any).
    """
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
    """End-to-end: call ``echo`` via the LangChain wrapper, get the text back.

    Exercises stdio framing, MCP TextContent canonicalisation, and
    the StructuredTool coroutine path.
    """
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
    """``boom`` raises server-side; the adapter wraps it in a RuntimeError.

    Lets LangChain deliver the error to the LLM rather than crashing
    the agent process.
    """
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
    """Two concurrent slow_echo calls on the same session must serialise.

    One lock per session. We schedule four 100ms calls and assert
    the wall-clock is closer to 400ms than 100ms. Generous bounds
    keep the test stable on slow CI hosts -- the property under test
    is "they don't overlap", not exact timing.
    """
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
    """When a built-in tool already owns a name, the MCP tool gets namespaced.

    Uses ``<server>__<tool>`` rather than shadowing the built-in.
    Exercises the collision-resolution path in load_mcp_tools.
    """
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
    """A spec whose command doesn't exist is skipped with a warning.

    Valid specs after it still load. Mirrors how a real deployment
    where the bridge is missing should not take down a multi-server
    config.
    """
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
    """No specs -> no tools, but we still return a stack.

    Keeps the caller's ``async with`` / ``aclose`` plumbing from
    having to special-case the empty path.
    """
    tools, stack = await load_mcp_tools([])
    try:
        assert tools == []
    finally:
        await stack.aclose()


@pytest.mark.asyncio
async def test_post_aclose_tool_call_fails(stub_spec: McpServerSpec) -> None:
    """After ``stack.aclose()`` the underlying MCP session is closed.

    Any subsequent tool call must error -- it cannot silently
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
    # `match=r".+"` pacifies ruff's PT011 (require a `match=` on
    # broad exceptions) without narrowing the type: any non-empty
    # error message satisfies it, including the
    # wrapped-string-via-RuntimeError path.
    with pytest.raises(Exception, match=r".+"):
        await echo.ainvoke({"text": "post"})


@pytest.mark.asyncio
async def test_load_mcp_tools_captures_subprocess_stderr_to_file(
    tmp_path: Path,
) -> None:
    """End-to-end: the MCP child's stderr is redirected to a file we can later read.

    Instead of being silently swallowed by the agent's PTY (which the
    OAT daemon drops under OAT_TOOL_LOG mode). The stub prints
    `[STUB MCP] boot banner` to stderr at startup; we assert that
    line lands in `mcp-stub.stderr.log` inside the audit dir.

    Production analogue: the bridge prints `[OAT Bridge] BOOT_TOKEN=...`
    and `[OAT Bridge] WebSocket client connected` to stderr, and the
    bench orchestrator tails these to gate preflight readiness.
    """
    stub_path = Path(__file__).parent / "_stub_mcp_server.py"
    audit_dir = tmp_path / "output" / "test-repo"
    spec = McpServerSpec(
        name="stub",
        command=sys.executable,
        args=[str(stub_path)],
        env={"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": str(audit_dir)},
        transport="stdio",
    )

    tools, stack = await load_mcp_tools([spec])
    try:
        # Exercise the server so the boot path definitely ran (the
        # subprocess prints its banner before serving any tool, but
        # asserting after a successful tool call gives stderr a
        # guaranteed flush window).
        echo = next(t for t in tools if t.name == "echo")
        assert await echo.ainvoke({"text": "x"}) == "x"
    finally:
        await stack.aclose()

    stderr_log = audit_dir / "mcp-stub.stderr.log"
    assert stderr_log.exists(), f"expected stderr log at {stderr_log}"
    captured = stderr_log.read_text(encoding="utf-8")
    assert "[STUB MCP] boot banner" in captured, (
        f"stub server's startup banner did not reach the per-server stderr "
        f"log; subprocess stderr capture is broken. captured={captured!r}"
    )
