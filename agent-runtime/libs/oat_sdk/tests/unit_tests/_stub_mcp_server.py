"""Stub MCP server used by mcp_client tests.

A minimal stdio MCP server that exposes two tools:
- ``echo``: returns the same text it received.
- ``boom``: always raises, used to verify error-path sidecar emission.

Implemented with the same official ``mcp`` SDK the agent-runtime uses,
so the wire shape is guaranteed to match. Kept tiny: a few tools is
enough to exercise discovery, dispatch, lock serialisation, and error
paths. New tools should NOT be added here unless a test needs them --
the fixture's value is its small footprint.
"""

from __future__ import annotations

import asyncio
import sys

from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.types import TextContent, Tool


def build_server() -> Server:
    server: Server = Server("oat-test-stub")

    @server.list_tools()
    async def _list() -> list[Tool]:
        return [
            Tool(
                name="echo",
                description="Echo back the text payload (used to verify MCP round-trips).",
                inputSchema={
                    "type": "object",
                    "properties": {"text": {"type": "string"}},
                    "required": ["text"],
                },
            ),
            Tool(
                name="boom",
                description="Always raises (used to verify MCP error surfaces).",
                inputSchema={"type": "object", "properties": {}},
            ),
            Tool(
                name="slow_echo",
                description="Echo with an async sleep (used to verify the per-session asyncio.Lock serialises parallel calls).",
                inputSchema={
                    "type": "object",
                    "properties": {"text": {"type": "string"}, "ms": {"type": "number"}},
                    "required": ["text"],
                },
            ),
        ]

    @server.call_tool()
    async def _call(name: str, arguments: dict | None) -> list[TextContent]:
        args = arguments or {}
        if name == "echo":
            return [TextContent(type="text", text=args.get("text", ""))]
        if name == "boom":
            raise RuntimeError("intentional stub failure")
        if name == "slow_echo":
            await asyncio.sleep(float(args.get("ms", 50)) / 1000.0)
            return [TextContent(type="text", text=args.get("text", ""))]
        raise ValueError(f"unknown tool: {name}")

    return server


async def _main() -> None:
    server = build_server()
    async with stdio_server() as (read_stream, write_stream):
        await server.run(read_stream, write_stream, server.create_initialization_options())


if __name__ == "__main__":
    try:
        asyncio.run(_main())
    except KeyboardInterrupt:
        sys.exit(0)
