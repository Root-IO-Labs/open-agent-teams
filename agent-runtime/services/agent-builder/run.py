#!/usr/bin/env python3
"""Interactive CLI for the agent-builder service.

Usage:
    uv run --no-sync run.py
    uv run --no-sync run.py --thread-id <id>   # resume a session
"""
from __future__ import annotations

import argparse
import asyncio
import sys
from pathlib import Path
from uuid import uuid4

from dotenv import load_dotenv

# Load .env if present (OAT provides env vars via its own .env system,
# but a local .env can be useful for standalone testing)
_env_file = Path(__file__).parent / ".env"
if _env_file.exists():
    load_dotenv(_env_file)

# ── ANSI colours ──────────────────────────────────────────────────────────────
RESET  = "\033[0m"
BOLD   = "\033[1m"
DIM    = "\033[2m"
CYAN   = "\033[36m"
GREEN  = "\033[32m"
YELLOW = "\033[33m"
RED    = "\033[31m"
BLUE   = "\033[34m"

def _c(code: str, text: str) -> str:
    return f"{code}{text}{RESET}"


# ── Paths ─────────────────────────────────────────────────────────────────────
_HERE      = Path(__file__).parent
_MAIN_YAML = _HERE / "agent-builder" / "main.yaml"


# ── Streaming helpers ─────────────────────────────────────────────────────────

def _text_from_chunk(msg_chunk: object) -> str:
    """Extract plain text from an AIMessageChunk, ignoring tool-call parts."""
    content = getattr(msg_chunk, "content", "")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                parts.append(block.get("text", ""))
            elif isinstance(block, str):
                parts.append(block)
        return "".join(parts)
    return ""


async def _stream_turn(runtime, messages: list[dict], thread_id: str) -> str:
    """Stream one turn and return the full assistant text."""
    full_text = ""
    in_response = False

    async for chunk in runtime.stream(
        messages,
        thread_id=thread_id,
        stream_mode="messages",
    ):
        # stream_mode="messages" yields (BaseMessage | BaseMessageChunk, metadata)
        if not isinstance(chunk, tuple) or len(chunk) != 2:
            continue
        msg_chunk, _meta = chunk

        # Skip user messages and tool messages; only print AI output
        msg_type = type(msg_chunk).__name__
        if "HumanMessage" in msg_type or "ToolMessage" in msg_type:
            continue

        text = _text_from_chunk(msg_chunk)
        if not text:
            continue

        if not in_response:
            print(f"\n{_c(BOLD + CYAN, 'Agent Builder')}: ", end="", flush=True)
            in_response = True

        print(text, end="", flush=True)
        full_text += text

    if in_response:
        print()  # newline after streaming ends

    return full_text


# ── REPL ──────────────────────────────────────────────────────────────────────

_BANNER = f"""
{_c(BOLD + CYAN, '╔══════════════════════════════════════╗')}
{_c(BOLD + CYAN, '║')}  {_c(BOLD, 'OAT Agent Builder')}  {_c(DIM, '— deepagents')}  {_c(BOLD + CYAN, '║')}
{_c(BOLD + CYAN, '╚══════════════════════════════════════╝')}
{_c(DIM, "Describe the agent service you want to build.")}
{_c(DIM, "Type 'exit' or press Ctrl-C to quit.")}
"""

_EXIT_CMDS = {"exit", "quit", "q", ":q"}


async def run_repl(thread_id: str) -> None:
    from deep_loom_kit import create_runtime, load_service

    print(_BANNER)
    print(f"{_c(DIM, 'Loading service...')}", end="\r", flush=True)

    service = await load_service(_MAIN_YAML)

    print(f"{_c(DIM, 'Starting runtime... ')}", end="\r", flush=True)

    async with await create_runtime(service) as runtime:
        print(f"{_c(GREEN, '✓ Ready')}  {_c(DIM, f'session: {thread_id}')}\n")

        while True:
            try:
                user_input = input(f"{_c(BOLD + YELLOW, 'You')}: ").strip()
            except (EOFError, KeyboardInterrupt):
                print(f"\n{_c(DIM, 'Goodbye.')}")
                break

            if not user_input:
                continue
            if user_input.lower() in _EXIT_CMDS:
                print(_c(DIM, "Goodbye."))
                break

            try:
                await _stream_turn(
                    runtime,
                    [{"role": "user", "content": user_input}],
                    thread_id=thread_id,
                )
            except KeyboardInterrupt:
                print(f"\n{_c(DIM, '[interrupted]')}")
            except Exception as exc:  # noqa: BLE001
                print(f"\n{_c(RED, f'Error: {exc}')}\n", file=sys.stderr)


# ── Entry point ───────────────────────────────────────────────────────────────

def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="OAT Agent Builder — interactively create new agent types.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--thread-id",
        default=None,
        metavar="ID",
        help="Resume an existing session by thread ID (default: new UUID).",
    )
    return parser.parse_args()


def main() -> None:
    args = _parse_args()
    thread_id = args.thread_id or str(uuid4())
    asyncio.run(run_repl(thread_id))


if __name__ == "__main__":
    main()
