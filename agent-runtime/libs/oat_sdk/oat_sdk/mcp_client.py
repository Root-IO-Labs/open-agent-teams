"""MCP (Model Context Protocol) client adapter for OAT agent-runtime.

Loads MCP servers declared in ``<cwd>/.oat/mcp.json`` at agent startup and
exposes their tools as LangChain ``BaseTool`` instances that can be merged
straight into the existing ``create_cli_agent(tools=...)`` call.

This module is a thin custom adapter -- we own four concerns directly:

1. **Concurrency**: each ``ClientSession`` is wrapped with an
   ``asyncio.Lock``. LangGraph dispatches tool calls in parallel; a
   single stdio JSON-RPC stream interleaving two requests would
   corrupt the framing.

2. **Sidecar wiring**: every MCP tool call emits ``KIND_TOOL_CALL`` /
   ``KIND_TOOL_RESULT`` sidecar events on both success AND error paths,
   feeding OAT's existing observability (cost tracking, model-bench,
   debugger UI). Failures still get a bounded ``duration_ms``.

3. **Result-type handling**: MCP servers can return text, image, or
   embedded-resource content blocks. We canonicalise to a LangChain
   tool-message-compatible representation; the image path emits the
   multimodal content block shape LangChain expects.

4. **Graceful degradation**: a malformed ``mcp.json``, a server that
   refuses to start, or an unreachable transport never crashes the
   agent. We log a warning and proceed with no MCP tools (mirrors how
   ``oat-browser-agent``'s bridge handles a malformed ``config.toml``).

Lifetime: ``load_mcp_tools`` returns the tools plus an ``AsyncExitStack``.
The caller owns the stack and MUST close it on shutdown so the bridge
stdio child is reaped cleanly. ``oat_cli/main.py`` wires this through
the same ``async with`` scope as the checkpointer.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import time
import uuid
from contextlib import AsyncExitStack
from pathlib import Path
from typing import Any, TextIO

from langchain_core.tools import StructuredTool
from pydantic import BaseModel, Field

logger = logging.getLogger(__name__)

# Conservative timeout for MCP session initialisation. The bridge handshake
# (BOOT_TOKEN handoff + WebSocket-handshake probe to the extension) is the
# slow path here and finishes well under 30s in practice; 30s gives the
# Chrome extension time to wake on a cold start.
_INIT_TIMEOUT_SECONDS = 30.0


class McpServerSpec(BaseModel):
    """One MCP server config entry from ``.oat/mcp.json``.

    Stdio is the only transport we support today -- it's what the
    oat-browser-agent bridge speaks, and it's the only transport that
    composes naturally with the OAT daemon spawn model (one child
    process per spec, cleaned up via SIGTERM through the exit stack).
    Adding SSE or WebSocket-server modes is a future extension; the
    current shape leaves room for a ``transport: "sse" | "ws"`` variant.
    """

    name: str = Field(..., description="Unique server identifier; used as a namespace for collision resolution")
    command: str = Field(..., description="Executable to spawn (e.g. 'node', '/path/to/bin')")
    args: list[str] = Field(default_factory=list, description="Argv to pass to the executable")
    env: dict[str, str] = Field(default_factory=dict, description="Env vars to merge into the spawned process")
    transport: str = Field(default="stdio", description="Wire transport (only 'stdio' supported)")


def _expand_path(value: str) -> str:
    """Apply ``~`` + env-var expansion on a string path.

    Used on every command/arg/env value before we hand it to the
    stdio spawner. Without this, configs written by the daemon
    (``~/.oat/...``) wouldn't resolve when the agent's CWD is the
    worktree. Uses ``Path.expanduser`` for the tilde-expansion arm
    (Pathlib-ish) but falls back to ``os.path.expandvars`` for
    the ``$VAR`` arm since pathlib lacks a Path-native equivalent.
    """
    return os.path.expandvars(str(Path(value).expanduser()))


def _resolve_stderr_log_path(spec: McpServerSpec) -> Path | None:
    """Decide where to capture a stdio MCP server's stderr.

    Resolution order:

    1. ``spec.env["OAT_BROWSER_AGENT_AUDIT_LOG_DIR"]`` if set -- the
       OAT daemon sets this to the canonical per-repo output dir
       (``~/.oat/output/<repo>``) for the browser_bridge spec, so
       co-locating the stderr capture there keeps every per-run log
       in one place (matches Part 4 canonicalization).
    2. ``<cwd>/.oat/`` -- the agent's worktree's hidden config dir,
       which already exists (it's where ``mcp.json`` lives). Used
       when the daemon didn't provide an explicit audit dir, e.g.
       hand-authored ``mcp.json`` configs for non-browser MCP
       servers or local development.

    File name is ``mcp-<spec.name>.stderr.log`` so two MCP servers in
    the same config don't collide. Append mode is used so a daemon
    restart of an opted-in agent preserves the prior boot's lines
    until the operator chooses to rotate.

    Returns ``None`` if the path can't be created -- caller falls back
    to the SDK default (inherit-stderr) rather than crashing the
    agent. Stderr capture is operator observability, not a correctness
    requirement.
    """
    audit_dir = spec.env.get("OAT_BROWSER_AGENT_AUDIT_LOG_DIR") if spec.env else None
    base = Path(_expand_path(audit_dir)) if audit_dir else Path.cwd() / ".oat"
    try:
        base.mkdir(parents=True, exist_ok=True)
    except OSError as e:
        logger.warning(
            "Could not create MCP stderr log dir %s for server %r (%s); "
            "subprocess stderr will be inherited (PTY-dropped under OAT_TOOL_LOG)",
            base,
            spec.name,
            e,
        )
        return None
    return base / f"mcp-{spec.name}.stderr.log"


def _open_stderr_log_for_spec(
    spec: McpServerSpec, stack: AsyncExitStack
) -> TextIO | None:
    """Open the per-server stderr log file and register its close on ``stack``.

    Returns the open file (suitable for passing as ``errlog`` to
    ``stdio_client``) or ``None`` if path resolution or open failed --
    caller then falls back to the SDK default (inherit-stderr from
    the Python parent).

    Path is computed by ``_resolve_stderr_log_path``. We register
    cleanup via ``stack.callback`` instead of using a ``with`` block
    because the file needs to stay open for the lifetime of the MCP
    session: the subprocess writes to it asynchronously, and the
    session shares the same exit stack we're building here.
    """
    log_path = _resolve_stderr_log_path(spec)
    if log_path is None:
        return None
    try:
        # Line-buffered append text mode. Append so a daemon-restart of
        # an opted-in browser-agent preserves the prior session's
        # diagnostics; an operator who wants a clean slate can rotate
        # or truncate the file out-of-band. We intentionally don't
        # wrap this in a ``with`` block (the file's lifetime is the
        # MCP session's, owned by ``stack`` via the callback below).
        errlog_file = log_path.open("a", encoding="utf-8", buffering=1)
    except OSError as e:
        logger.warning(
            "Could not open MCP stderr log %s for server %r (%s); "
            "subprocess stderr will be inherited",
            log_path,
            spec.name,
            e,
        )
        return None
    stack.callback(errlog_file.close)
    return errlog_file


def load_mcp_config(config_path: Path) -> list[McpServerSpec]:
    """Read ``<cwd>/.oat/mcp.json`` and return a list of validated specs.

    Returns ``[]`` for any failure path -- missing file, invalid JSON,
    schema validation error, unsupported transport. Logs a warning when
    the file exists but couldn't be parsed; silently returns ``[]`` when
    the file is absent (the common "no MCP configured" case shouldn't
    spam the log).

    The accepted file shape is:

    .. code-block:: json

        {
          "servers": [
            {
              "name": "browser_bridge",
              "command": "node",
              "args": ["/path/to/dist/bridge/index.js"],
              "env": {"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": "~/.oat/output/<repo>"},
              "transport": "stdio"
            }
          ]
        }
    """
    if not config_path.exists():
        return []
    try:
        with config_path.open("r", encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        logger.warning("Failed to read MCP config at %s: %s -- continuing with no MCP tools", config_path, e)
        return []

    raw_servers = data.get("servers") if isinstance(data, dict) else None
    if not isinstance(raw_servers, list):
        logger.warning("MCP config at %s missing 'servers' list -- continuing with no MCP tools", config_path)
        return []

    specs: list[McpServerSpec] = []
    for idx, raw in enumerate(raw_servers):
        try:
            spec = McpServerSpec.model_validate(raw)
        # pydantic raises several distinct exception classes for
        # validation failures (ValidationError, plus various
        # ValueError subclasses depending on field type). We collapse
        # them all here because the "skip this bad spec and keep the
        # rest" policy doesn't depend on which kind of failure it was.
        except Exception as e:  # noqa: BLE001
            logger.warning("MCP config entry %d invalid: %s -- skipping", idx, e)
            continue
        if spec.transport != "stdio":
            logger.warning(
                "MCP server '%s' uses transport=%r; only 'stdio' is supported -- skipping",
                spec.name,
                spec.transport,
            )
            continue
        # Expand ~ and $VARs in command, args, and env values. This lets
        # the daemon write portable paths like '~/.oat/...' without each
        # consumer reimplementing the expansion.
        spec = McpServerSpec(
            name=spec.name,
            command=_expand_path(spec.command),
            args=[_expand_path(a) for a in spec.args],
            env={k: _expand_path(v) for k, v in spec.env.items()},
            transport=spec.transport,
        )
        specs.append(spec)

    return specs


def _emit_sidecar_tool_call(name: str, args: dict[str, Any], call_id: str) -> None:
    """Wrap the sidecar emitter import + call in a try/except.

    A missing or broken sidecar never crashes the MCP tool execution
    path. The deferred import keeps a hard dependency on ``oat_cli``
    out of this module's import graph; the blind-Exception catch is
    intentional because any sidecar failure is non-fatal.
    """
    try:
        from oat_cli.sidecar_emitter import emit_tool_call  # noqa: PLC0415

        emit_tool_call(name, args, call_id)
    except Exception:  # noqa: BLE001
        logger.debug("sidecar emit_tool_call failed (non-fatal)", exc_info=True)


def _emit_sidecar_tool_result(call_id: str, content: str, error: str | None = None) -> None:
    try:
        from oat_cli.sidecar_emitter import emit_tool_result  # noqa: PLC0415

        emit_tool_result(call_id, content, error)
    except Exception:  # noqa: BLE001
        logger.debug("sidecar emit_tool_result failed (non-fatal)", exc_info=True)


def _stringify_mcp_result(result: Any) -> tuple[str, list[dict[str, Any]] | None]:  # noqa: ANN401
    """Canonicalise an MCP ``CallToolResult`` to LangChain-friendly types.

    Returns ``(text_repr, multimodal_blocks_or_None)``.

    - All ``TextContent`` blocks join into the text repr (newline-joined).
    - ``ImageContent`` blocks become multimodal content entries
      (``{type: 'image', source: {type: 'base64', media_type, data}}``).
      LangChain's tool-message handling accepts this shape; the text
      repr gets a marker line so plain-text consumers know there were
      images.
    - ``EmbeddedResource`` blocks fall back to a tagged string
      (``[mcp:resource type=<mime> uri=<uri>]``) -- agents that genuinely
      need the bytes can call the server again with a resource-fetch tool.

    The MCP wire types live in ``mcp.types``; we attribute-probe rather
    than ``isinstance`` to stay tolerant of MCP SDK version drift (the
    type names have shuffled between 0.x and 1.x).
    """
    if result is None:
        return ("", None)

    content = getattr(result, "content", None)
    if content is None:
        # Some MCP versions surface the result body directly on the object.
        return (str(result), None)

    text_parts: list[str] = []
    multimodal: list[dict[str, Any]] = []
    has_image = False

    for block in content:
        block_type = getattr(block, "type", None)
        if block_type == "text":
            text_parts.append(getattr(block, "text", "") or "")
            continue
        if block_type == "image":
            has_image = True
            data = getattr(block, "data", None)
            media_type = getattr(block, "mimeType", None) or "image/png"
            if data:
                multimodal.append(
                    {
                        "type": "image",
                        "source": {"type": "base64", "media_type": media_type, "data": data},
                    }
                )
            text_parts.append(f"[mcp:image media_type={media_type}]")
            continue
        if block_type == "resource":
            resource = getattr(block, "resource", None)
            mime = getattr(resource, "mimeType", None) or "application/octet-stream"
            uri = getattr(resource, "uri", None) or "<no-uri>"
            text_parts.append(f"[mcp:resource type={mime} uri={uri}]")
            continue
        # Unknown block type: stringify as a structural marker. Don't
        # silently drop -- the agent should see SOMETHING is in the
        # response, and the marker lets a maintainer notice unknown
        # block kinds in the future without a crash.
        text_parts.append(f"[mcp:unknown_block type={block_type!r}]")

    text_repr = "\n".join(text_parts) if text_parts else "(empty MCP result)"
    return (text_repr, multimodal if has_image else None)


def _build_input_schema(mcp_tool: Any) -> dict[str, Any] | None:  # noqa: ANN401
    """Pull the MCP tool's JSON Schema into a LangChain-compatible dict.

    LangChain's ``StructuredTool.from_function(args_schema=...)`` accepts
    either a pydantic model class or a JSON Schema dict (in recent
    versions). For now we pass the JSON Schema directly; if older
    LangChain versions reject it, the call site falls back to
    ``StructuredTool.from_function`` without an explicit schema and
    relies on the LLM to read the description.
    """
    schema = getattr(mcp_tool, "inputSchema", None)
    if isinstance(schema, dict):
        return schema
    return None


async def _make_tool_wrapper(
    *,
    session: Any,  # noqa: ANN401
    session_lock: asyncio.Lock,
    server_name: str,
    mcp_tool: Any,  # noqa: ANN401
    public_name: str,
) -> StructuredTool:
    """Build a LangChain ``StructuredTool`` that proxies into an MCP session.

    The wrapper is the only LangChain-visible artefact; everything else
    (session, lock, raw MCP tool descriptor) is captured in the closure
    and never leaks to the agent.
    """
    description = getattr(mcp_tool, "description", None) or f"MCP tool '{mcp_tool.name}' on server '{server_name}'."
    args_schema = _build_input_schema(mcp_tool)

    raw_name = mcp_tool.name

    async def _coroutine(**kwargs: Any) -> str:
        call_id = f"mcp-{uuid.uuid4().hex[:12]}"
        start = time.monotonic()
        _emit_sidecar_tool_call(public_name, kwargs, call_id)
        try:
            async with session_lock:
                result = await session.call_tool(raw_name, kwargs)
            text_repr, _multimodal = _stringify_mcp_result(result)
            # MCP servers surface user-visible errors via
            # ``CallToolResult.isError=True`` rather than raising over
            # the wire (the server-side exception is caught and folded
            # into the result). Treat that as an error path so the LLM
            # sees a tool-failure rather than a "successful" result
            # containing an error string.
            if getattr(result, "isError", False):
                elapsed_ms = int((time.monotonic() - start) * 1000)
                err_msg = f"{text_repr} (after {elapsed_ms}ms)"
                _emit_sidecar_tool_result(call_id, text_repr, error=err_msg)
                # Raising inside the try is intentional: the outer
                # `except RuntimeError: raise` arm re-propagates this
                # unmodified, while the `except Exception` arm wraps
                # other failures. Moving this raise outside the try
                # (TRY301) would skip that ordering. The matching
                # return below stays in the try for the same reason
                # (TRY300 would move it to an `else:` block, but that
                # forces a re-indent of the entire happy-path body).
                wrapped = f"MCP tool '{public_name}' failed: {err_msg}"
                raise RuntimeError(wrapped)  # noqa: TRY301
            _emit_sidecar_tool_result(call_id, text_repr, error=None)
            return text_repr  # noqa: TRY300
        except RuntimeError:
            # Already wrapped (isError branch above). Don't re-wrap.
            raise
        # Catching `Exception` is intentional: MCP tool dispatch is a
        # plugin boundary -- any failure here must surface to the
        # LLM as a tool error, not crash the agent process. Narrowing
        # would let SDK-version-specific exception classes leak out.
        # (ruff's BLE001 does not fire here because of the explicit
        # `raise ... from e` re-wrap below.)
        except Exception as e:
            elapsed_ms = int((time.monotonic() - start) * 1000)
            err_msg = f"{type(e).__name__}: {e} (after {elapsed_ms}ms)"
            _emit_sidecar_tool_result(call_id, "", error=err_msg)
            # Re-raise as a tool-call error LangChain can surface to the
            # model. Do NOT crash the agent process.
            wrapped = f"MCP tool '{public_name}' failed: {err_msg}"
            raise RuntimeError(wrapped) from e

    if args_schema is not None:
        try:
            return StructuredTool.from_function(
                name=public_name,
                description=description,
                coroutine=_coroutine,
                args_schema=args_schema,
            )
        # Blind Exception catch is intentional: LangChain versions
        # vary on what they raise for unsupported JSON Schema shapes
        # (TypeError, ValueError, jsonschema-specific errors, pydantic
        # validation failures...). The fallback path is the same
        # regardless, so we don't gain anything by enumerating types.
        except Exception:  # noqa: BLE001
            # Older LangChain versions choke on raw JSON Schema dicts;
            # fall through to the schema-less variant.
            logger.debug(
                "StructuredTool.from_function rejected MCP JSON schema for %s; falling back to schema-less",
                public_name,
                exc_info=True,
            )
    return StructuredTool.from_function(
        name=public_name,
        description=description,
        coroutine=_coroutine,
    )


async def load_mcp_tools(
    specs: list[McpServerSpec],
    *,
    builtin_tool_names: set[str] | None = None,
) -> tuple[list[StructuredTool], AsyncExitStack]:
    """Spawn each MCP server, discover its tools, and return adapters.

    The returned ``AsyncExitStack`` owns the lifetime of all spawned
    stdio children plus their ``ClientSession``s. The caller MUST keep
    the stack open until the agent shuts down, then ``aclose()`` it
    (e.g. via an ``async with`` block, or an explicit ``await
    stack.aclose()`` in a SIGTERM handler).

    Tool-name collisions:
      - Built-in OAT tool names (passed via ``builtin_tool_names``)
        always win. A colliding MCP tool is exposed as
        ``<server_name>__<tool>``. Double underscore is visually clear
        in LLM tool-call traces and avoids shell-quoting issues.
      - MCP-to-MCP collisions are handled the same way (namespace the
        loser, log it once).

    A spec that fails to start (subprocess exec error, init timeout,
    transport handshake failure) is skipped with a warning; remaining
    specs still load. We never abort the agent on a single bad spec.
    """
    stack = AsyncExitStack()
    out_tools: list[StructuredTool] = []
    seen_names = set(builtin_tool_names or set())

    if not specs:
        return (out_tools, stack)

    try:
        # Deferred import: the SDK pulls in pydantic-settings, starlette,
        # sse_starlette, ... we don't want every oat_sdk import path to
        # pay that cost. Bringing it in here means agents without an
        # ``mcp.json`` skip the cost entirely. PLC0415 is suppressed
        # for the same reason: top-level would defeat the deferral.
        from mcp import ClientSession, StdioServerParameters  # noqa: PLC0415
        from mcp.client.stdio import stdio_client  # noqa: PLC0415
    except ImportError as e:
        logger.warning("MCP client SDK not importable (%s); skipping all MCP servers", e)
        return (out_tools, stack)

    for spec in specs:
        try:
            params = StdioServerParameters(
                command=spec.command,
                args=list(spec.args),
                env={**os.environ, **spec.env} if spec.env else None,
            )
            # Capture the subprocess's stderr to a file so the bridge
            # (or any other MCP server) has somewhere to emit
            # connection-level diagnostics. Without this, stderr
            # flows up to the Python parent's PTY, where the OAT
            # daemon drops it because OAT_TOOL_LOG is set (the
            # daemon defers to Python for log writing under that
            # mode, but Python's conversation log only captures
            # LLM/tool events -- not the MCP child's startup banner,
            # WebSocket-client-connected events, token-handshake
            # rejections, etc.). The MCP SDK's stdio_client accepts
            # an `errlog: TextIO`; we provide a per-server file when
            # we can compute one, else fall back to the SDK default.
            errlog_file = _open_stderr_log_for_spec(spec, stack)
            stdio_kwargs = {"errlog": errlog_file} if errlog_file is not None else {}
            # Enter the stdio_client context (spawns the subprocess +
            # opens stdin/stdout streams) on the exit stack so the
            # subprocess is reaped when the stack closes.
            read_stream, write_stream = await stack.enter_async_context(
                stdio_client(params, **stdio_kwargs)
            )
            session = await stack.enter_async_context(ClientSession(read_stream, write_stream))
            await asyncio.wait_for(session.initialize(), timeout=_INIT_TIMEOUT_SECONDS)

            list_response = await session.list_tools()
            mcp_tools = getattr(list_response, "tools", []) or []
        # Blind Exception catch is intentional: an MCP server is a
        # third-party plugin and the failure modes are unbounded
        # (subprocess spawn errors, transport-level disconnects,
        # SDK-version-specific exception classes, asyncio timeouts).
        # Policy is "one bad server doesn't break the agent"; we log
        # and move on so the remaining specs still load.
        except Exception as e:  # noqa: BLE001
            logger.warning(
                "MCP server '%s' failed to start (%s: %s); continuing without it",
                spec.name,
                type(e).__name__,
                e,
            )
            continue

        # One lock per session. LangGraph dispatches tool calls in
        # parallel and a single stdio JSON-RPC stream can't multiplex
        # two requests; the lock serialises at the adapter layer.
        session_lock = asyncio.Lock()

        for mcp_tool in mcp_tools:
            raw_name = getattr(mcp_tool, "name", None)
            if not raw_name:
                continue
            public_name = raw_name
            if public_name in seen_names:
                namespaced = f"{spec.name}__{raw_name}"
                logger.info(
                    "MCP tool name collision: %r already provided; exposing as %r",
                    raw_name,
                    namespaced,
                )
                public_name = namespaced
                if public_name in seen_names:
                    logger.warning(
                        "Namespaced MCP tool %r still collides; skipping",
                        public_name,
                    )
                    continue
            seen_names.add(public_name)
            tool = await _make_tool_wrapper(
                session=session,
                session_lock=session_lock,
                server_name=spec.name,
                mcp_tool=mcp_tool,
                public_name=public_name,
            )
            out_tools.append(tool)

    return (out_tools, stack)
