#!/usr/bin/env python3
"""OAT Model Capability Probe — onboard any model by fingerprinting its capabilities.

Usage:
    # Anthropic
    python benchmarks/probe-model.py anthropic:claude-sonnet-4-6

    # OpenAI (reasoning)
    python benchmarks/probe-model.py openai:o4-mini

    # Ollama (local)
    python benchmarks/probe-model.py ollama:qwen2.5:3b

    # Google
    python benchmarks/probe-model.py google_genai:gemini-2.5-flash

    # OpenRouter
    OPENROUTER_API_KEY=... python benchmarks/probe-model.py openrouter:deepseek/deepseek-v3-0324

    # Save report
    python benchmarks/probe-model.py anthropic:claude-sonnet-4-6 --output results/probe-sonnet.json

This script uses OAT's own model resolution (init_chat_model + resolve_model)
so the probe tests the exact same code path agents use. Results are a structured
JSON report that can be committed to the repo as a model capability card.

Probes:
  1. basic_inference        — Can the model respond at all? Checks instruction following.
  2. tool_calling           — Can it emit tool_use blocks via function calling API? Checks arg correctness.
  3. streaming              — Does astream() work? Measures TTFT (time to first token).
  4. token_reporting        — Does usage_metadata contain input/output tokens? Sanity checks ranges.
  5. streaming_tokens       — Does streaming deliver usage_metadata per-chunk?
  6. multi_turn             — Can it handle conversation history + tool results across many turns?
  7. large_output           — Can it produce structurally complete multi-class code?
  8. shell_roundtrip        — Full-loop: emit tool call -> execute -> act on result.
  9. shell_failure_recovery — Can it recover from multiple failure types?
  10. file_write_via_tool   — Can it write files with correct args?
  11. reasoning_effort      — Does the model accept reasoning effort parameters?
  12. routing_decision      — Can the model correctly assign tasks to different models? (supervisor signal)
  13. context_profile       — What does the model profile report for context window?
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import traceback
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from typing import Any

# Add OAT's Python paths
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_SCRIPT_DIR)
_AGENT_RUNTIME = os.path.join(_REPO_ROOT, "agent-runtime")
for subdir in ("libs/cli", "libs/deepagents", "libs/acp"):
    path = os.path.join(_AGENT_RUNTIME, subdir)
    if path not in sys.path:
        sys.path.insert(0, path)


@dataclass
class ProbeResult:
    name: str
    passed: bool
    score: int  # 0-100
    details: dict[str, Any] = field(default_factory=dict)
    error: str | None = None
    duration_ms: int = 0


@dataclass
class ModelReport:
    model_string: str
    provider: str
    model_name: str
    timestamp: str
    probes: list[ProbeResult] = field(default_factory=list)
    overall_score: int = 0
    oat_compatible: bool = False
    warnings: list[str] = field(default_factory=list)
    recommendations: list[str] = field(default_factory=list)
    probe_set: str = "default"
    # True when _resolve_model had to fall back from create_model() to
    # init_chat_model() because the former raised. Surfaced in YAML so
    # operators can see the model wasn't resolved via the canonical path.
    resolution_fallback: bool = False


def _resolve_model(model_string: str, no_fallback: bool = False) -> tuple[Any, bool]:
    """Resolve model using OAT's exact resolution path.

    Returns ``(model, used_fallback)``. ``used_fallback`` is ``True`` when the
    primary path (``deepagents_cli.config.create_model``) raised and we had to
    fall back to ``langchain.chat_models.init_chat_model``.

    The primary path reads ``~/.oat/config.toml`` and supports custom providers
    (e.g. openrouter). The fallback is used when the CLI package is not
    installed OR when ``create_model`` raises.

    Error handling (PR #2 P0-B):

    - ``ImportError`` from the CLI package → WARN and fall through silently;
      this is the "running probe-model.py outside an OAT checkout" case.
    - Any other exception from ``create_model`` → WARN with type + message so
      config.toml typos surface, then try ``init_chat_model``.
    - If both paths fail, re-raise the FIRST exception (the ``create_model``
      one, not the ``init_chat_model`` one) so operators see the real cause.
    - ``no_fallback=True`` skips the second path entirely for debugging.
    """
    first_exc: Exception | None = None

    try:
        from deepagents_cli.config import create_model
    except ImportError as exc:
        # Only WARN once — this is the common case when running the probe
        # outside an OAT checkout. Fallback is expected.
        print(f"WARN: deepagents_cli.config unavailable ({type(exc).__name__}): {exc}",
              file=sys.stderr)
        first_exc = exc
    else:
        try:
            return create_model(model_string).model, False
        except Exception as exc:  # noqa: BLE001 — we re-raise below
            print(f"WARN: create_model failed ({type(exc).__name__}): {exc}",
                  file=sys.stderr)
            first_exc = exc
            if no_fallback:
                raise

    from langchain.chat_models import init_chat_model

    try:
        if model_string.startswith("openai:"):
            model = init_chat_model(model_string, use_responses_api=True)
        else:
            model = init_chat_model(model_string)
    except Exception as exc:  # noqa: BLE001
        # Prefer surfacing the FIRST exception — operators care more about a
        # config.toml typo than a generic "Unknown model" from init_chat_model.
        if first_exc is not None:
            raise first_exc from exc
        raise

    return model, True


def _get_provider(model_string: str) -> tuple[str, str]:
    """Extract provider and model name from model string."""
    if ":" in model_string:
        parts = model_string.split(":", 1)
        return parts[0], parts[1]
    return "unknown", model_string


def _timed(func):
    """Run a function and return (result, duration_ms)."""
    start = time.monotonic()
    try:
        result = func()
        return result, int((time.monotonic() - start) * 1000)
    except Exception as e:
        return e, int((time.monotonic() - start) * 1000)


async def _timed_async(coro):
    """Run a coroutine and return (result, duration_ms)."""
    start = time.monotonic()
    try:
        result = await coro
        return result, int((time.monotonic() - start) * 1000)
    except Exception as e:
        return e, int((time.monotonic() - start) * 1000)


def _extract_content(result) -> str:
    """Safely extract text content from any LangChain message."""
    content = getattr(result, "content", "")
    if isinstance(content, list):
        content = " ".join(
            b.get("text", "") if isinstance(b, dict) else str(b) for b in content
        )
    return content


# ---------------------------------------------------------------------------
# Probe 1: Basic Inference
# Added 20-point instruction-following bonus for ~3 word response.
# ---------------------------------------------------------------------------

def probe_basic_inference(model) -> ProbeResult:
    """Can the model respond to a simple prompt? Does it follow instructions?"""
    result, ms = _timed(lambda: model.invoke("Say hello in exactly 3 words."))
    if isinstance(result, Exception):
        return ProbeResult(
            name="basic_inference", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )
    content = _extract_content(result)

    if not content.strip():
        return ProbeResult(
            name="basic_inference", passed=False, score=0,
            details={"response": "", "response_length": 0},
            duration_ms=ms,
        )

    # Base: 80 for any non-empty response (gate check)
    score = 80

    # Instruction-following bonus: did it actually produce ~3 words?
    word_count = len(content.strip().split())
    if word_count == 3:
        score += 20  # Perfect instruction following
    elif word_count in (2, 4):
        score += 10  # Close enough

    return ProbeResult(
        name="basic_inference", passed=True, score=min(score, 100),
        details={
            "response": content[:100],
            "response_length": len(content),
            "word_count": word_count,
            "instruction_followed": word_count == 3,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 2: Tool Calling
# Added arg correctness check (+10 for city=="Tokyo").
# ---------------------------------------------------------------------------

def probe_tool_calling(model) -> ProbeResult:
    """Can the model emit proper tool_use blocks with correct arguments?"""
    from langchain_core.tools import tool

    @tool
    def get_weather(city: str) -> str:
        """Get current weather for a city."""
        return f"Sunny, 72°F in {city}"

    bound = model.bind_tools([get_weather])
    result, ms = _timed(lambda: bound.invoke("What's the weather in Tokyo?"))
    if isinstance(result, Exception):
        return ProbeResult(
            name="tool_calling", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )

    tool_calls = getattr(result, "tool_calls", [])
    content = _extract_content(result)

    if tool_calls:
        call = tool_calls[0]
        tool_name = call.get("name", "?")
        tool_args = call.get("args", {})
        city_arg = tool_args.get("city", "")

        # Base 70 for emitting a tool call at all
        score = 70
        # +20 for correct tool name
        if tool_name == "get_weather":
            score += 20
        # +10 for correct argument value
        if city_arg.lower().strip() == "tokyo":
            score += 10

        return ProbeResult(
            name="tool_calling", passed=True, score=score,
            details={
                "tool_name": tool_name,
                "tool_args": tool_args,
                "num_tool_calls": len(tool_calls),
                "arg_correct": city_arg.lower().strip() == "tokyo",
            },
            duration_ms=ms,
        )

    # Check if the model wrote a tool call as plaintext
    plaintext_indicators = ["get_weather", "tool_use", "function_call", '"city"', "Tokyo"]
    plaintext_hits = sum(1 for ind in plaintext_indicators if ind.lower() in content.lower())

    if plaintext_hits >= 2:
        return ProbeResult(
            name="tool_calling", passed=False, score=20,
            details={"issue": "plaintext_tool_call", "response_excerpt": content[:200]},
            error="Model wrote tool call as text instead of using function calling API",
            duration_ms=ms,
        )

    return ProbeResult(
        name="tool_calling", passed=False, score=0,
        details={"response_excerpt": content[:200]},
        error="No tool_calls in response and no plaintext tool pattern detected",
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 3: Streaming + TTFT
# Records time-to-first-token (TTFT). Scored on gradient instead of binary.
# ---------------------------------------------------------------------------

async def probe_streaming(model) -> ProbeResult:
    """Does astream() work? What's the time-to-first-token (TTFT)?"""
    chunks = []
    first_content_ms = None
    start = time.monotonic()
    try:
        async for chunk in model.astream("Count from 1 to 5, one number per line."):
            now = time.monotonic()
            chunks.append(chunk)
            # Record TTFT: first chunk with actual content
            if first_content_ms is None:
                chunk_content = _extract_content(chunk)
                if chunk_content.strip():
                    first_content_ms = int((now - start) * 1000)
            if len(chunks) > 50:
                break  # Safety cap
        ms = int((time.monotonic() - start) * 1000)
    except Exception as e:
        ms = int((time.monotonic() - start) * 1000)
        return ProbeResult(
            name="streaming", passed=False, score=0,
            error=str(e)[:200], duration_ms=ms,
        )

    if len(chunks) == 0:
        return ProbeResult(
            name="streaming", passed=False, score=0,
            error="No chunks received", duration_ms=ms,
        )

    # Graduated scoring based on TTFT
    # 50 base for streaming working at all
    # +25 for TTFT < 3000ms
    # +25 for TTFT < 1000ms
    score = 50
    if first_content_ms is not None:
        if first_content_ms < 3000:
            score += 25
        if first_content_ms < 1000:
            score += 25

    return ProbeResult(
        name="streaming", passed=True, score=score,
        details={
            "num_chunks": len(chunks),
            "first_chunk_type": type(chunks[0]).__name__,
            "ttft_ms": first_content_ms,
            "total_stream_ms": ms,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 4: Token Reporting
# Added sanity check on token count ranges.
# ---------------------------------------------------------------------------

def probe_token_reporting(model) -> ProbeResult:
    """Does usage_metadata contain input/output tokens? Are counts sane?"""
    result, ms = _timed(lambda: model.invoke("What is 2+2?"))
    if isinstance(result, Exception):
        return ProbeResult(
            name="token_reporting", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )

    usage = getattr(result, "usage_metadata", None)
    if usage is None:
        return ProbeResult(
            name="token_reporting", passed=False, score=0,
            error="No usage_metadata on response",
            details={"has_response_metadata": bool(getattr(result, "response_metadata", None))},
            duration_ms=ms,
        )

    input_tokens = usage.get("input_tokens", 0)
    output_tokens = usage.get("output_tokens", 0)
    total_tokens = usage.get("total_tokens", 0)
    input_details = usage.get("input_token_details", {})
    output_details = usage.get("output_token_details", {})

    has_basic = input_tokens > 0 and output_tokens > 0
    has_total = total_tokens > 0

    score = 0
    if has_basic:
        score += 70
    if has_total:
        score += 15
    if input_details:
        score += 10
    if output_details:
        score += 5

    # Sanity check: for "What is 2+2?", input should be < 200 tokens
    # and output should be < 500 tokens. Flag but don't gate on this.
    sane_input = input_tokens < 200
    sane_output = output_tokens < 500
    counts_sane = sane_input and sane_output

    return ProbeResult(
        name="token_reporting", passed=has_basic, score=score,
        details={
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "input_token_details": dict(input_details) if input_details else None,
            "output_token_details": dict(output_details) if output_details else None,
            "counts_sane": counts_sane,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 5: Streaming Token Reporting (unchanged — fine for its weight)
# ---------------------------------------------------------------------------

async def probe_streaming_tokens(model) -> ProbeResult:
    """Does streaming deliver usage_metadata on final chunk?"""
    chunks_with_usage = []
    all_chunks = 0
    last_usage = None
    start = time.monotonic()
    try:
        async for chunk in model.astream("What is 7*8?"):
            all_chunks += 1
            if hasattr(chunk, "usage_metadata") and chunk.usage_metadata:
                chunks_with_usage.append(chunk.usage_metadata)
                last_usage = chunk.usage_metadata
            if all_chunks > 50:
                break
        ms = int((time.monotonic() - start) * 1000)
    except Exception as e:
        ms = int((time.monotonic() - start) * 1000)
        return ProbeResult(
            name="streaming_tokens", passed=False, score=0,
            error=str(e)[:200], duration_ms=ms,
        )

    if not chunks_with_usage:
        return ProbeResult(
            name="streaming_tokens", passed=False, score=30,
            details={"total_chunks": all_chunks, "chunks_with_usage": 0},
            error="Streaming works but no chunk carried usage_metadata",
            duration_ms=ms,
        )

    return ProbeResult(
        name="streaming_tokens", passed=True, score=100,
        details={
            "total_chunks": all_chunks,
            "chunks_with_usage": len(chunks_with_usage),
            "final_usage": dict(last_usage) if last_usage else None,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 6: Multi-Turn with Tool Results
# Two-stage test. Stage 1 is the original 3-turn gate (quick, binary).
# Stage 2 is 8 turns with accumulating state — 3 tool calls, cross-turn
# reference check, final synthesis. This is the probe that gates supervisor.
# ---------------------------------------------------------------------------

def probe_multi_turn(model) -> ProbeResult:
    """Can the model handle multi-turn conversation with tool results and state tracking?"""
    from langchain_core.messages import AIMessage, HumanMessage, ToolMessage

    # ---------------------------------------------------------------
    # Stage 1: Basic 3-turn gate (same as before but scored as gate)
    # ---------------------------------------------------------------
    gate_messages = [
        HumanMessage(content="What's the weather?"),
        AIMessage(
            content="",
            tool_calls=[{"name": "get_weather", "args": {"city": "NYC"}, "id": "call_1"}],
        ),
        ToolMessage(content="Sunny, 72°F", tool_call_id="call_1"),
    ]

    result, ms_gate = _timed(lambda: model.invoke(gate_messages))
    if isinstance(result, Exception):
        error_str = str(result)[:200]
        if "tool" in error_str.lower():
            return ProbeResult(
                name="multi_turn", passed=False, score=20,
                error="Model can't process tool messages in history (may need tool binding)",
                duration_ms=ms_gate,
            )
        return ProbeResult(
            name="multi_turn", passed=False, score=0,
            error=error_str, duration_ms=ms_gate,
        )

    gate_content = _extract_content(result)
    gate_passed = bool(gate_content.strip()) and any(
        w in gate_content.lower() for w in ["sunny", "72", "weather", "nyc"]
    )

    if not gate_passed:
        # Can't even handle 3 turns with a tool result — fail early
        return ProbeResult(
            name="multi_turn", passed=False,
            score=30 if gate_content.strip() else 0,
            details={
                "stage": "gate",
                "response_excerpt": gate_content[:150],
                "references_tool_result": False,
            },
            duration_ms=ms_gate,
        )

    # ---------------------------------------------------------------
    # Stage 2: Extended multi-turn with accumulating state
    # Simulates a realistic worker conversation: 3 tool calls spread
    # across turns, follow-up questions that require combining results,
    # and a final synthesis that requires referencing early turns.
    # ---------------------------------------------------------------
    extended_messages = [
        # Turn 1: user asks about a project
        HumanMessage(content="I need a status report on our deployment. First check the server health."),
        # Turn 2: model called health check
        AIMessage(
            content="Let me check the server health.",
            tool_calls=[{"name": "check_health", "args": {"target": "prod-server-1"}, "id": "call_h1"}],
        ),
        # Turn 3: tool result - server healthy, 94% memory
        ToolMessage(
            content='{"status": "healthy", "cpu": "23%", "memory": "94%", "disk": "67%"}',
            tool_call_id="call_h1",
        ),
        # Turn 4: model summarizes and user asks for logs
        AIMessage(content="Server is healthy. CPU at 23%, memory at 94%, disk at 67%. Memory is running high. Let me check the deployment logs."),
        HumanMessage(content="Yes, pull the last deployment log to see if the memory spike is from the latest release."),
        # Turn 5: model called log retrieval
        AIMessage(
            content="",
            tool_calls=[{"name": "get_logs", "args": {"service": "api-gateway", "lines": 50}, "id": "call_l1"}],
        ),
        # Turn 6: tool result - logs show memory leak indicator
        ToolMessage(
            content='[2025-06-01 14:32] Deployed v2.8.1\n[2025-06-01 14:35] Memory usage increased from 71% to 94%\n[2025-06-01 14:40] GC pressure elevated\n[2025-06-01 15:00] OOM killer triggered on worker-3',
            tool_call_id="call_l1",
        ),
        # Turn 7: user asks for synthesis referencing earlier data
        HumanMessage(
            content="OK based on everything so far — the health check AND the logs — "
            "what's causing the memory issue and what should we do? "
            "Also remind me what the disk usage was."
        ),
    ]

    result2, ms_ext = _timed(lambda: model.invoke(extended_messages))
    total_ms = ms_gate + ms_ext

    if isinstance(result2, Exception):
        # Stage 1 passed but stage 2 errored — partial credit
        return ProbeResult(
            name="multi_turn", passed=True, score=40,
            details={
                "stage": "extended_errored",
                "gate_passed": True,
                "error_stage2": str(result2)[:200],
            },
            duration_ms=total_ms,
        )

    ext_content = _extract_content(result2)
    ext_lower = ext_content.lower()

    # Check cross-turn references:
    # 1. Does it reference the deployment version from turn 6? (v2.8.1)
    references_version = "2.8.1" in ext_content
    # 2. Does it connect memory spike to the deployment? (causal reasoning)
    connects_cause = any(phrase in ext_lower for phrase in [
        "deploy", "release", "v2.8", "after deploy", "caused by",
        "memory leak", "oom", "since the deploy", "latest release",
    ])
    # 3. Does it recall disk usage from turn 3? (67%)
    recalls_disk = "67" in ext_content
    # 4. Does it suggest a recovery action?
    suggests_action = any(phrase in ext_lower for phrase in [
        "rollback", "roll back", "revert", "restart", "investigate",
        "scale", "fix", "patch", "downgrade", "previous version",
    ])

    # Scoring: 30 gate + up to 70 for extended
    score = 30  # Gate passed
    if ext_content.strip():
        score += 10  # Responded to extended at all
    if references_version:
        score += 15  # Cross-turn detail recall
    if connects_cause:
        score += 15  # Causal reasoning across turns
    if recalls_disk:
        score += 15  # References data from early turn when asked
    if suggests_action:
        score += 15  # Actionable synthesis

    return ProbeResult(
        name="multi_turn", passed=True, score=min(score, 100),
        details={
            "stage": "extended",
            "gate_passed": True,
            "references_version": references_version,
            "connects_cause": connects_cause,
            "recalls_disk": recalls_disk,
            "suggests_action": suggests_action,
            "response_excerpt": ext_content[:300],
        },
        duration_ms=total_ms,
    )


# ---------------------------------------------------------------------------
# Probe 7: Large Structured Output
# Harder prompt (multi-class LRU cache with TTL), structural completeness
# checks (balanced parens/braces, no truncation), length gradient.
# ---------------------------------------------------------------------------

def probe_large_output(model) -> ProbeResult:
    """Can the model produce structurally complete multi-component code?"""
    result, ms = _timed(lambda: model.invoke(
        "Write a Python module with the following:\n"
        "1. A class `CacheEntry` that stores a value, creation timestamp, and TTL in seconds.\n"
        "2. A class `LRUCache` that implements a least-recently-used cache with TTL support. "
        "It should have methods: get(key), put(key, value, ttl=None), evict_expired(), and a "
        "configurable max_size.\n"
        "3. A utility function `make_cache(max_size=128, default_ttl=300)` that returns a "
        "configured LRUCache instance.\n\n"
        "Include docstrings. Use only the standard library. Return ONLY the code, no explanation."
    ))
    if isinstance(result, Exception):
        return ProbeResult(
            name="large_output", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )

    content = _extract_content(result)

    # Strip markdown code fences if present
    if "```python" in content:
        content = content.split("```python", 1)[1]
        if "```" in content:
            content = content.rsplit("```", 1)[0]
    elif "```" in content:
        parts = content.split("```")
        if len(parts) >= 3:
            content = parts[1]

    lines = content.strip().split("\n")
    line_count = len(lines)

    # Structural checks
    has_cache_entry = "class CacheEntry" in content or "class cacheentry" in content.lower()
    has_lru_cache = "class LRUCache" in content or "class lrucache" in content.lower()
    has_make_cache = "def make_cache" in content or "def make_cache" in content.lower()
    has_docstring = '"""' in content or "'''" in content
    has_return = "return" in content

    # Structural completeness: balanced braces/parens (catches truncation)
    open_parens = content.count("(") - content.count(")")
    open_brackets = content.count("[") - content.count("]")
    open_braces = content.count("{") - content.count("}")
    structurally_complete = (
        abs(open_parens) <= 1 and abs(open_brackets) <= 1 and abs(open_braces) <= 1
    )

    # Check for truncation indicators
    not_truncated = not content.rstrip().endswith(("...", "# ...", "# etc"))

    score = 0
    # Component checks (10 each = 30)
    if has_cache_entry:
        score += 10
    if has_lru_cache:
        score += 10
    if has_make_cache:
        score += 10
    # Quality checks
    if has_docstring:
        score += 10
    if has_return:
        score += 5
    # Structural completeness (important — truncated code = broken worker output)
    if structurally_complete:
        score += 15
    if not_truncated:
        score += 10
    # Length gradient
    if line_count >= 20:
        score += 10
    if line_count >= 50:
        score += 10

    passed = has_lru_cache and has_return and structurally_complete

    return ProbeResult(
        name="large_output", passed=passed, score=min(score, 100),
        details={
            "line_count": line_count,
            "has_cache_entry": has_cache_entry,
            "has_lru_cache": has_lru_cache,
            "has_make_cache": has_make_cache,
            "has_docstring": has_docstring,
            "structurally_complete": structurally_complete,
            "not_truncated": not_truncated,
            "unbalanced_parens": open_parens,
            "unbalanced_brackets": open_brackets,
            "unbalanced_braces": open_braces,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 8: Shell Roundtrip (replaces old shell_execution)
# Full-loop test: emit tool call -> feed back result -> model acts on output.
# ---------------------------------------------------------------------------

def probe_shell_roundtrip(model) -> ProbeResult:
    """Full-loop: model emits shell command, sees output, acts on it."""
    from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
    from langchain_core.tools import tool

    @tool
    def execute(command: str) -> str:
        """Execute a shell command and return its output."""
        return "(simulated output)"

    # Phase 1: Does the model emit a tool call?
    bound = model.bind_tools([execute])
    result, ms1 = _timed(lambda: bound.invoke(
        "Run: echo hello_from_probe"
    ))
    if isinstance(result, Exception):
        return ProbeResult(
            name="shell_roundtrip", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms1,
        )

    tool_calls = getattr(result, "tool_calls", [])
    if not tool_calls:
        return ProbeResult(
            name="shell_roundtrip", passed=False, score=0,
            error="Model did not emit a tool call for shell execution",
            duration_ms=ms1,
        )

    call = tool_calls[0]
    cmd = call.get("args", {}).get("command", "")
    has_echo = "echo" in cmd.lower()
    has_marker = "hello_from_probe" in cmd

    # Phase 2: Feed back the tool result, ask model to act on it.
    # This tests the full loop: model sees output and responds intelligently.
    roundtrip_messages = [
        HumanMessage(content="Run 'ls -la /app/src/' and tell me how many Python files there are."),
        AIMessage(
            content="",
            tool_calls=[{"name": "execute", "args": {"command": "ls -la /app/src/"}, "id": "call_ls"}],
        ),
        ToolMessage(
            content=(
                "total 48\n"
                "drwxr-xr-x  2 root root 4096 Jun  1 12:00 .\n"
                "drwxr-xr-x  5 root root 4096 Jun  1 12:00 ..\n"
                "-rw-r--r--  1 root root 1234 Jun  1 12:00 main.py\n"
                "-rw-r--r--  1 root root 2345 Jun  1 12:00 utils.py\n"
                "-rw-r--r--  1 root root 3456 Jun  1 12:00 config.yaml\n"
                "-rw-r--r--  1 root root 4567 Jun  1 12:00 test_main.py\n"
                "-rw-r--r--  1 root root  890 Jun  1 12:00 README.md\n"
            ),
            tool_call_id="call_ls",
        ),
    ]

    result2, ms2 = _timed(lambda: model.invoke(roundtrip_messages))
    total_ms = ms1 + ms2

    if isinstance(result2, Exception):
        # Phase 1 passed but phase 2 errored — partial credit
        score = 40
        if has_echo:
            score += 10
        if has_marker:
            score += 10
        return ProbeResult(
            name="shell_roundtrip", passed=True, score=score,
            details={"phase1_cmd": cmd, "phase2_error": str(result2)[:200]},
            duration_ms=total_ms,
        )

    content2 = _extract_content(result2)
    content2_lower = content2.lower()

    # Check if model correctly interpreted the ls output
    # There are 3 Python files: main.py, utils.py, test_main.py
    mentions_count = any(w in content2_lower for w in ["3 python", "three python", "3 .py", "three .py"])
    identifies_files = sum(1 for f in ["main.py", "utils.py", "test_main.py"] if f in content2_lower)

    score = 30  # Phase 1: emitted tool call
    if has_echo:
        score += 10
    if has_marker:
        score += 10
    # Phase 2: acted on tool output
    if content2.strip():
        score += 15  # Responded at all
    if mentions_count:
        score += 20  # Correct count
    if identifies_files >= 2:
        score += 15  # Named the files

    return ProbeResult(
        name="shell_roundtrip", passed=bool(tool_calls) and bool(content2.strip()),
        score=min(score, 100),
        details={
            "phase1_cmd": cmd,
            "phase1_has_echo": has_echo,
            "phase1_has_marker": has_marker,
            "phase2_response": content2[:200],
            "phase2_mentions_count": mentions_count,
            "phase2_identifies_files": identifies_files,
        },
        duration_ms=total_ms,
    )


# ---------------------------------------------------------------------------
# Probe 9: Shell Failure Recovery
# Tests 3 failure scenarios (file not found, command not found, permission
# denied). Checks for corrective tool call, not just text.
# ---------------------------------------------------------------------------

def probe_shell_failure_recovery(model) -> ProbeResult:
    """Can the model recover from multiple types of shell failures?"""
    from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
    from langchain_core.tools import tool

    @tool
    def execute(command: str) -> str:
        """Execute a shell command and return its output."""
        return "(simulated)"

    bound = model.bind_tools([execute])

    scenarios = [
        {
            "name": "file_not_found",
            "request": "Read the config file at /etc/app/config.yaml",
            "command": "cat /etc/app/config.yaml",
            "error_output": "cat: /etc/app/config.yaml: No such file or directory\n[exit code 1]",
            "acknowledge_keywords": ["not found", "doesn't exist", "does not exist", "no such file", "missing"],
            "recovery_keywords": ["find", "locate", "ls", "search", "look for", "check if", "alternative path", "let me"],
        },
        {
            "name": "command_not_found",
            "request": "Use ripgrep to search for 'TODO' in the codebase.",
            "command": "rg TODO /app/src/",
            "error_output": "bash: rg: command not found\n[exit code 127]",
            "acknowledge_keywords": ["command not found", "not installed", "not available", "doesn't exist"],
            "recovery_keywords": ["grep", "install", "apt", "pip", "alternative", "instead", "use grep", "fall back"],
        },
        {
            "name": "permission_denied",
            "request": "Write the deployment config to /etc/app/deploy.conf",
            "command": "echo 'env=prod' > /etc/app/deploy.conf",
            "error_output": "bash: /etc/app/deploy.conf: Permission denied\n[exit code 1]",
            "acknowledge_keywords": ["permission denied", "not permitted", "access denied", "cannot write", "don't have permission"],
            "recovery_keywords": ["sudo", "chmod", "chown", "different location", "tmp", "home", "write to", "permissions"],
        },
    ]

    total_score = 0
    scenario_results = {}
    total_ms = 0
    any_corrective_tool_call = False

    for scenario in scenarios:
        messages = [
            HumanMessage(content=scenario["request"]),
            AIMessage(
                content="",
                tool_calls=[{
                    "name": "execute",
                    "args": {"command": scenario["command"]},
                    "id": f"call_{scenario['name']}",
                }],
            ),
            ToolMessage(
                content=scenario["error_output"],
                tool_call_id=f"call_{scenario['name']}",
            ),
        ]

        result, ms = _timed(lambda: bound.invoke(messages))
        total_ms += ms

        if isinstance(result, Exception):
            scenario_results[scenario["name"]] = {
                "error": str(result)[:100],
                "acknowledges": False,
                "recovers": False,
                "corrective_tool_call": False,
            }
            continue

        content = _extract_content(result)
        content_lower = content.lower()

        acknowledges = any(k in content_lower for k in scenario["acknowledge_keywords"])
        recovers = any(k in content_lower for k in scenario["recovery_keywords"])

        # Check if model emitted a corrective tool call (much better than just text)
        tool_calls = getattr(result, "tool_calls", [])
        has_corrective_call = len(tool_calls) > 0
        if has_corrective_call:
            any_corrective_tool_call = True

        scenario_score = 0
        if acknowledges:
            scenario_score += 1
        if recovers:
            scenario_score += 1
        if has_corrective_call:
            scenario_score += 1  # Bonus for actual action, not just words

        total_score += scenario_score

        scenario_results[scenario["name"]] = {
            "acknowledges": acknowledges,
            "recovers": recovers,
            "corrective_tool_call": has_corrective_call,
            "corrective_cmd": tool_calls[0].get("args", {}).get("command", "") if has_corrective_call else None,
            "response_excerpt": content[:150],
        }

    # Max possible: 3 scenarios * 3 points = 9
    # Scale to 100
    score = int((total_score / 9) * 100)

    # Passed if at least 2/3 scenarios acknowledged AND at least 1 recovery
    scenarios_acknowledged = sum(
        1 for s in scenario_results.values()
        if isinstance(s, dict) and s.get("acknowledges", False)
    )
    scenarios_recovered = sum(
        1 for s in scenario_results.values()
        if isinstance(s, dict) and s.get("recovers", False)
    )
    passed = scenarios_acknowledged >= 2 and scenarios_recovered >= 1

    return ProbeResult(
        name="shell_failure_recovery", passed=passed, score=score,
        details={
            "scenarios": scenario_results,
            "total_points": total_score,
            "max_points": 9,
            "scenarios_acknowledged": scenarios_acknowledged,
            "scenarios_recovered": scenarios_recovered,
            "any_corrective_tool_call": any_corrective_tool_call,
        },
        duration_ms=total_ms,
    )


# ---------------------------------------------------------------------------
# Probe 10: File Write via Tool (unchanged — solid for its purpose)
# ---------------------------------------------------------------------------

def probe_file_write_via_tool(model) -> ProbeResult:
    """Can the model use a write_file tool to create a file?"""
    from langchain_core.tools import tool

    @tool
    def write_file(path: str, content: str) -> str:
        """Write content to a file at the given path."""
        return f"Successfully wrote {len(content)} bytes to {path}"

    bound = model.bind_tools([write_file])
    result, ms = _timed(lambda: bound.invoke(
        "Create a file called hello.txt containing the text: Hello World"
    ))
    if isinstance(result, Exception):
        return ProbeResult(
            name="file_write_via_tool", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )

    tool_calls = getattr(result, "tool_calls", [])
    if not tool_calls:
        return ProbeResult(
            name="file_write_via_tool", passed=False, score=0,
            error="Model did not emit a write_file tool call",
            duration_ms=ms,
        )

    call = tool_calls[0]
    args = call.get("args", {})
    path = args.get("path", "")
    file_content = args.get("content", "")

    has_path = bool(path)
    has_content = bool(file_content)
    path_correct = "hello" in path.lower() and path.endswith(".txt")
    content_correct = "hello" in file_content.lower() and "world" in file_content.lower()

    score = 30  # Got a tool call
    if has_path:
        score += 15
    if has_content:
        score += 15
    if path_correct:
        score += 20
    if content_correct:
        score += 20

    return ProbeResult(
        name="file_write_via_tool", passed=has_path and has_content, score=score,
        details={
            "path": path,
            "content_length": len(file_content),
            "content_preview": file_content[:100],
            "path_correct": path_correct,
            "content_correct": content_correct,
        },
        duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 11: Reasoning Effort Support
# Returns score 0 (not 50) for untested providers. Added Google support.
# ---------------------------------------------------------------------------

def probe_reasoning_effort(model, provider: str, model_name: str) -> ProbeResult:
    """Does the model accept reasoning effort parameters?"""
    from langchain.chat_models import init_chat_model

    results = {}
    supported_levels = []

    if provider == "openai":
        for level in ("low", "medium", "high"):
            try:
                m = init_chat_model(
                    f"openai:{model_name}",
                    use_responses_api=True,
                    reasoning_effort=level,
                )
                r = m.invoke("Say OK")
                results[level] = {"status": "ok", "output_tokens": r.usage_metadata.get("output_tokens", 0) if r.usage_metadata else 0}
                supported_levels.append(level)
            except Exception as e:
                err = str(e)[:100]
                if "Unrecognized" in err or "not support" in err.lower():
                    results[level] = {"status": "unsupported"}
                else:
                    results[level] = {"status": "error", "error": err}

    elif provider == "anthropic":
        # Test thinking budget (effort field doesn't work on current models)
        for budget in (5000, 10000):
            label = f"thinking_budget_{budget}"
            try:
                m = init_chat_model(
                    f"anthropic:{model_name}",
                    thinking={"type": "enabled", "budget_tokens": budget},
                    max_tokens=budget + 8000,
                )
                r = m.invoke("Say OK")
                output = r.usage_metadata.get("output_tokens", 0) if r.usage_metadata else 0
                results[label] = {"status": "ok", "output_tokens": output}
                supported_levels.append(label)
            except Exception as e:
                err = str(e)[:100]
                results[label] = {"status": "error", "error": err}

    elif provider == "google_genai":
        # Google Gemini thinking support
        for budget in (5000, 10000):
            label = f"thinking_budget_{budget}"
            try:
                m = init_chat_model(
                    f"google_genai:{model_name}",
                    thinking={"type": "enabled", "budget_tokens": budget},
                )
                r = m.invoke("Say OK")
                results[label] = {"status": "ok"}
                supported_levels.append(label)
            except Exception as e:
                err = str(e)[:100]
                if "not support" in err.lower() or "invalid" in err.lower():
                    results[label] = {"status": "unsupported"}
                else:
                    results[label] = {"status": "error", "error": err}

    else:
        # Return 0, not 50. "Unknown" is not "partial".
        return ProbeResult(
            name="reasoning_effort", passed=False, score=0,
            details={"provider": provider, "note": "No reasoning effort API known for this provider"},
            duration_ms=0,
        )

    has_support = len(supported_levels) > 0
    score = 100 if has_support else 0

    return ProbeResult(
        name="reasoning_effort", passed=has_support, score=score,
        details={"provider": provider, "tested_levels": results, "supported": supported_levels},
        duration_ms=0,
    )


# ---------------------------------------------------------------------------
# Probe 12: Routing Decision (supervisor quality signal)
# 5 tasks instead of 3 for better signal. Added cost-sensitive and
# large-context tasks. Profile roster can be injected dynamically.
# ---------------------------------------------------------------------------

# Default profiles — used if no dynamic roster is available.
_DEFAULT_ROUTING_PROFILES = """Available models:
1. anthropic:claude-sonnet-4-6 — Score 99, reasoning controls, 200K context, $3/M input, all tools 100% reliable
2. google_genai:gemini-2.5-flash — Score 95, no reasoning, 1M context, $0.15/M input, shell recovery 70%
3. ollama:qwen2.5:3b — Score 90, no reasoning, 32K context, FREE (local), all tools reliable
4. ollama:gemma3:1b — Score 50, RESTRICTED, cannot use tools, cannot run shell, NOT eligible for workers
"""

_ROUTING_TASKS = [
    {
        "task": "Refactor the authentication system across 12 files to support OAuth2 and SAML federation",
        "expected_tier": "strong",
        "best_picks": ["anthropic:claude-sonnet-4-6"],
        "bad_picks": ["ollama:gemma3:1b"],
        "points": 20,
    },
    {
        "task": "Fix typo: change 'recieve' to 'receive' in the error message at api/handler.go line 42",
        "expected_tier": "any_eligible",
        "best_picks": ["ollama:qwen2.5:3b", "google_genai:gemini-2.5-flash"],  # Cost-efficient picks
        "bad_picks": ["ollama:gemma3:1b"],
        "points": 15,
    },
    {
        "task": "Add comprehensive integration tests for the payment pipeline, requires running shell commands and validating CI output",
        "expected_tier": "shell_capable",
        "best_picks": ["anthropic:claude-sonnet-4-6"],
        "bad_picks": ["ollama:gemma3:1b"],
        "points": 20,
    },
    {
        "task": "Add a one-line copyright header to each of the 30 source files in /app/src/",
        "expected_tier": "cost_efficient",
        "best_picks": ["ollama:qwen2.5:3b"],  # Free/local model for bulk repetitive work
        "bad_picks": ["ollama:gemma3:1b"],
        "points": 20,
    },
    {
        "task": "Analyze a 400K-token codebase to identify all usages of deprecated API methods and suggest replacements",
        "expected_tier": "large_context",
        "best_picks": ["google_genai:gemini-2.5-flash"],  # 1M context
        "bad_picks": ["ollama:gemma3:1b", "ollama:qwen2.5:3b"],  # 32K can't fit 400K
        "points": 25,
    },
]


def probe_routing_decision(model, profiles_text: str | None = None) -> ProbeResult:
    """Can this model make good task-to-model assignment decisions?"""
    profiles = profiles_text or _DEFAULT_ROUTING_PROFILES
    eligible_models = [
        "anthropic:claude-sonnet-4-6",
        "google_genai:gemini-2.5-flash",
        "ollama:qwen2.5:3b",
        "ollama:gemma3:1b",
    ]

    task_lines = "\n".join(
        f"Task {i+1}: {t['task']}" for i, t in enumerate(_ROUTING_TASKS)
    )

    prompt = f"""You are selecting which AI model should work on each task.

{profiles}

Rules:
- NEVER assign a RESTRICTED model (ollama:gemma3:1b) to any task
- Complex multi-file work needs the strongest model
- Simple/repetitive tasks should use the cheapest capable model
- Shell-heavy tasks need models with good shell reliability
- Large-context tasks need models with sufficient context windows
- Consider cost: prefer cheaper models when quality difference is minimal

For each task, respond with ONLY the model ID (e.g., "anthropic:claude-sonnet-4-6"), one per line.

{task_lines}

Respond with exactly {len(_ROUTING_TASKS)} lines, one model ID per line. No explanations."""

    result, ms = _timed(lambda: model.invoke(prompt))
    if isinstance(result, Exception):
        return ProbeResult(
            name="routing_decision", passed=False, score=0,
            error=str(result)[:200], duration_ms=ms,
        )

    content = _extract_content(result)

    # Parse assignments
    lines = [l.strip() for l in content.strip().split("\n") if l.strip()]
    assignments = []
    for line in lines:
        # Strip common prefixes
        for prefix in [f"Task {i}:" for i in range(1, 10)] + \
                      [f"{i}." for i in range(1, 10)] + \
                      [f"{i}:" for i in range(1, 10)] + ["-", "*"]:
            if line.startswith(prefix):
                line = line[len(prefix):].strip()
        for mid in eligible_models:
            if mid in line:
                assignments.append(mid)
                break

    if len(assignments) < len(_ROUTING_TASKS):
        return ProbeResult(
            name="routing_decision", passed=False, score=10,
            details={"raw_response": content[:400], "parsed_assignments": assignments},
            error=f"Could not parse {len(_ROUTING_TASKS)} assignments (got {len(assignments)})",
            duration_ms=ms,
        )

    score = 0
    task_details = {}

    for i, (task, assignment) in enumerate(zip(_ROUTING_TASKS, assignments)):
        task_key = f"task{i+1}"
        task_details[f"{task_key}_assignment"] = assignment
        task_details[f"{task_key}_tier"] = task["expected_tier"]

        if assignment in task["bad_picks"]:
            # Critical failure: assigned restricted or incapable model
            task_details[f"{task_key}_correct"] = False
            task_details[f"{task_key}_critical_fail"] = True
        elif assignment in task["best_picks"]:
            # Optimal pick
            score += task["points"]
            task_details[f"{task_key}_correct"] = True
            task_details[f"{task_key}_optimal"] = True
        else:
            # Acceptable but not optimal (eligible model, just not the best fit)
            score += int(task["points"] * 0.5)
            task_details[f"{task_key}_correct"] = True
            task_details[f"{task_key}_optimal"] = False

    # Check for critical failure: any restricted model assigned
    any_restricted = any(a == "ollama:gemma3:1b" for a in assignments)
    if any_restricted:
        score = max(score - 30, 0)  # Heavy penalty

    # Check distribution: not all tasks on the same model
    unique_models = len(set(assignments))
    if unique_models >= 3:
        task_details["good_distribution"] = True
    elif unique_models == 1:
        score = max(score - 20, 0)  # Penalty for dumping everything on one model
        task_details["good_distribution"] = False

    correct_count = sum(1 for k, v in task_details.items() if k.endswith("_correct") and v)
    passed = correct_count >= 3 and not any_restricted
    task_details["correct_count"] = correct_count
    task_details["any_restricted_assigned"] = any_restricted
    task_details["unique_models_used"] = unique_models
    task_details["raw_response"] = content[:400]

    return ProbeResult(
        name="routing_decision", passed=passed, score=min(score, 100),
        details=task_details, duration_ms=ms,
    )


# ---------------------------------------------------------------------------
# Probe 13: Context Profile (unchanged — metadata check, not a capability test)
# ---------------------------------------------------------------------------

_openrouter_models_cache: list[dict] | None = None

_OPENROUTER_ROUTING_SUFFIXES = (":nitro", ":extended", ":free", ":floor")


def _fetch_openrouter_context_length(model_id: str) -> int | None:
    """Query the OpenRouter full models list for a model's context_length.

    Strips known routing suffixes (:nitro, :extended, :free, :floor) before
    searching.  Caches the full model list at module level to avoid redundant
    fetches within the same probe session.

    Returns the context window size in tokens, or None on any failure.
    The endpoint is public (no auth required).
    """
    import urllib.request
    import urllib.error

    global _openrouter_models_cache

    # Strip known routing suffixes to get canonical model ID
    model_id_to_search = model_id
    for suffix in _OPENROUTER_ROUTING_SUFFIXES:
        if model_id_to_search.endswith(suffix):
            model_id_to_search = model_id_to_search[: -len(suffix)]
            break

    if _openrouter_models_cache is None:
        url = "https://openrouter.ai/api/v1/models"
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "oat-probe/1.0"})
            with urllib.request.urlopen(req, timeout=15) as resp:
                data = json.loads(resp.read())
            _openrouter_models_cache = data.get("data", [])
        except (urllib.error.URLError, OSError, json.JSONDecodeError, KeyError):
            _openrouter_models_cache = []
            return None

    for model_data in _openrouter_models_cache:
        if model_data.get("id") == model_id_to_search:
            ctx = model_data.get("context_length")
            if isinstance(ctx, int) and ctx > 0:
                return ctx
            break

    # If suffix-stripped ID didn't match, try the original as-is
    if model_id_to_search != model_id:
        for model_data in _openrouter_models_cache:
            if model_data.get("id") == model_id:
                ctx = model_data.get("context_length")
                if isinstance(ctx, int) and ctx > 0:
                    return ctx
                break

    return None


def _fetch_openai_context_length(model_id: str) -> int | None:
    """Query the OpenAI models API for a model's context window.

    Requires OPENAI_API_KEY to be set. Returns the context window size in
    tokens, or None on any failure (missing key, network error, etc.).
    """
    import urllib.request
    import urllib.error

    api_key = os.environ.get("OPENAI_API_KEY", "")
    if not api_key:
        return None

    url = f"https://api.openai.com/v1/models/{model_id}"
    try:
        req = urllib.request.Request(
            url,
            headers={
                "Authorization": f"Bearer {api_key}",
                "User-Agent": "oat-probe/1.0",
            },
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            data = json.loads(resp.read())
        ctx = data.get("context_window")
        if isinstance(ctx, int) and ctx > 0:
            return ctx
    except (urllib.error.URLError, OSError, json.JSONDecodeError, KeyError):
        pass
    return None


def probe_context_profile(model, *, provider: str = "", model_name: str = "") -> ProbeResult:
    """What does the model's LangChain profile report?"""
    profile = getattr(model, "profile", None)

    if profile is None or not isinstance(profile, dict):
        return ProbeResult(
            name="context_profile", passed=False, score=30,
            details={"has_profile": False},
            error="No model profile available — context limits unknown. Set via config.toml.",
        )

    context_limit = profile.get("max_input_tokens")
    tool_calling = profile.get("tool_calling")
    supports_thinking = profile.get("supports_thinking")

    # Fallback: fetch context window from provider API if not in profile
    if not context_limit and model_name:
        if provider == "openrouter":
            fetched = _fetch_openrouter_context_length(model_name)
            if fetched:
                context_limit = fetched
        elif provider == "openai":
            fetched = _fetch_openai_context_length(model_name)
            if fetched:
                context_limit = fetched

    score = 30  # base for having a profile
    if context_limit and isinstance(context_limit, int):
        score += 40
    if tool_calling is True:
        score += 20
    if tool_calling is False:
        score = 0  # Critical failure

    details = {
        "has_profile": True,
        "max_input_tokens": context_limit,
        "tool_calling": tool_calling,
    }
    if supports_thinking is not None:
        details["supports_thinking"] = supports_thinking

    return ProbeResult(
        name="context_profile", passed=tool_calling is not False,
        score=min(score, 100), details=details,
    )


# ---------------------------------------------------------------------------
# Report Generation
# ---------------------------------------------------------------------------

def _generate_recommendations(report: ModelReport) -> None:
    """Add actionable recommendations based on probe results."""
    probe_map = {p.name: p for p in report.probes}

    # Critical failures
    if not probe_map.get("basic_inference", ProbeResult("", False, 0)).passed:
        report.recommendations.append(
            "BLOCKER: Model cannot produce basic responses. Check API key, model name, and provider connectivity."
        )
        return

    if not probe_map.get("tool_calling", ProbeResult("", False, 0)).passed:
        tc = probe_map.get("tool_calling")
        if tc and tc.details.get("issue") == "plaintext_tool_call":
            report.recommendations.append(
                "BLOCKER: Model writes tool calls as plaintext — cannot use function calling API. "
                "If using OpenRouter, check if the provider enables server-side tool call parsing."
            )
            report.warnings.append("Tool calling broken — OAT agents will not function")
        else:
            report.recommendations.append(
                "BLOCKER: Model does not emit tool_use blocks. OAT requires tool calling."
            )
            report.warnings.append("Tool calling not supported — OAT incompatible")

    # Token reporting
    if not probe_map.get("token_reporting", ProbeResult("", False, 0)).passed:
        report.warnings.append("No token reporting — token tracking will show '—' in TUI")
        report.recommendations.append(
            "Token reporting unavailable. Budget enforcement (--max-tokens) will not work."
        )

    # Token sanity
    tr = probe_map.get("token_reporting")
    if tr and tr.details.get("counts_sane") is False:
        report.warnings.append("Token counts outside expected range — budget tracking may be inaccurate")

    # TTFT from streaming
    streaming = probe_map.get("streaming")
    if streaming and streaming.passed:
        ttft = streaming.details.get("ttft_ms")
        if ttft is not None and ttft > 3000:
            report.warnings.append(f"High TTFT ({ttft}ms) — consider for batch tasks only, not interactive")
        elif ttft is not None and ttft < 1000:
            report.recommendations.append(f"Fast TTFT ({ttft}ms) — good candidate for interactive/streaming tasks")

    # Streaming tokens
    st = probe_map.get("streaming_tokens")
    if st and not st.passed and st.score > 0:
        report.warnings.append("Streaming works but doesn't report usage_metadata per-chunk")

    # Context profile
    cp = probe_map.get("context_profile")
    if cp and not cp.details.get("max_input_tokens"):
        report.recommendations.append(
            "No context window limit in model profile. Add to config.toml:\n"
            f'  [models.providers.{report.provider}.profile."{report.model_name}"]\n'
            "  max_input_tokens = <context_window_size>"
        )

    # Reasoning effort
    re_probe = probe_map.get("reasoning_effort")
    if re_probe and re_probe.passed:
        report.recommendations.append(
            f"Reasoning effort supported ({', '.join(re_probe.details.get('supported', []))}). "
            "Consider setting in config.toml for quality improvement."
        )

    # Shell roundtrip
    sr = probe_map.get("shell_roundtrip")
    if sr and not sr.passed:
        report.warnings.append("Shell roundtrip failed — workers cannot execute and act on command output")

    # File write
    if not probe_map.get("file_write_via_tool", ProbeResult("", False, 0)).passed:
        report.warnings.append("File write via tool not working — workers cannot create/edit files")

    # Shell recovery
    sfr = probe_map.get("shell_failure_recovery")
    if sfr and not sfr.passed:
        report.warnings.append("Model does not recover from shell failures — may get stuck on errors")
    if sfr and sfr.details.get("any_corrective_tool_call"):
        report.recommendations.append("Model emits corrective tool calls on failure — strong worker signal")

    # Multi-turn
    mt = probe_map.get("multi_turn")
    if mt and mt.passed and mt.score < 60:
        report.warnings.append("Multi-turn works but weak cross-turn state tracking — poor supervisor candidate")

    # OAT compatibility verdict
    core_probes = ["basic_inference", "tool_calling", "streaming",
                   "shell_roundtrip", "file_write_via_tool"]
    report.oat_compatible = all(
        probe_map[name].passed
        for name in core_probes
        if name in probe_map
    )


def _print_report(report: ModelReport) -> None:
    """Print a human-readable report to stderr."""
    print(f"\n{'=' * 70}", file=sys.stderr)
    print(f"  OAT Model Probe Report: {report.model_string}", file=sys.stderr)
    print(f"  {report.timestamp}", file=sys.stderr)
    print(f"{'=' * 70}\n", file=sys.stderr)

    for p in report.probes:
        status = "PASS" if p.passed else "FAIL"
        icon = "✓" if p.passed else "✗"
        time_str = f"({p.duration_ms}ms)" if p.duration_ms else ""
        print(f"  {icon} {p.name:24s}  {status:4s}  {p.score:3d}/100  {time_str}", file=sys.stderr)
        if p.error:
            print(f"    └─ {p.error[:70]}", file=sys.stderr)
        if p.details:
            for k, v in p.details.items():
                if v is not None and k not in ("response_excerpt", "tested_levels",
                                                "scenarios", "raw_response",
                                                "phase2_response"):
                    print(f"       {k}: {v}", file=sys.stderr)

    print(f"\n{'─' * 70}", file=sys.stderr)
    print(f"  Overall Score:    {report.overall_score}/100", file=sys.stderr)
    print(f"  OAT Compatible:  {'YES' if report.oat_compatible else 'NO'}", file=sys.stderr)

    if report.warnings:
        print(f"\n  Warnings:", file=sys.stderr)
        for w in report.warnings:
            print(f"    ⚠ {w}", file=sys.stderr)

    if report.recommendations:
        print(f"\n  Recommendations:", file=sys.stderr)
        for r in report.recommendations:
            for i, line in enumerate(r.split("\n")):
                prefix = "  → " if i == 0 else "    "
                print(f"  {prefix}{line}", file=sys.stderr)

    print(f"\n{'=' * 70}\n", file=sys.stderr)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

# Rebalanced weights to match routing importance.
PROBE_WEIGHTS = {
    "basic_inference": 15,        # Gate check — correct
    "tool_calling": 15,           # Core capability — correct
    "shell_roundtrip": 8,         # Reduced from 15: was redundant, now full-loop but still overlaps
    "shell_failure_recovery": 15, # Increased from 10: most predictive of worker success
    "file_write_via_tool": 10,    # Correct
    "streaming": 5,               # Correct
    "token_reporting": 10,        # Correct — budget enforcement depends on this
    "streaming_tokens": 3,        # Correct — low-priority nice-to-have
    "multi_turn": 12,             # Increased from 5: gates supervisor, was dangerously low
    "large_output": 4,            # Correct — test is harder now but weight is fine
    "reasoning_effort": 3,        # Correct
    "routing_decision": 5,        # Correct
    "context_profile": 5,         # Correct
}

# Probe sets: which probes to run at each level.
PROBE_SETS = {
    "minimum": {
        "basic_inference", "tool_calling", "shell_roundtrip",
        "file_write_via_tool", "token_reporting", "context_profile",
    },
    "default": None,  # None = run all probes
}


async def _invoke_probe_with_timeout(probe_fn, timeout: int):
    """Invoke a probe function (sync or async) with a hard deadline.

    Sync probes are dispatched via ``asyncio.to_thread`` so the event loop
    stays responsive even if the probe's internal HTTP client blocks. If the
    probe returns a coroutine (the streaming probes), it is awaited
    afterwards. The whole dispatch is wrapped in
    ``asyncio.wait_for(..., timeout=timeout)`` so a single hung API call
    can't block the onboarding run (PR #2 P0-A).

    Thread-leak caveat (Q1): ``asyncio.to_thread`` has no way to cancel a
    blocked sync HTTP call. When a probe times out the ``wait_for`` returns
    but the worker thread keeps running until the underlying HTTP client
    gives up on its own (provider-side timeout, typically 60-120 s more).
    This is accepted here because the alternative — subprocess-per-probe —
    is disproportionate complexity for a bench tool. Operators running many
    onboard attempts against a hanging provider may see thread/memory
    growth; restart the process if it becomes a problem.
    """
    async def _dispatch():
        result = await asyncio.to_thread(probe_fn)
        if asyncio.iscoroutine(result):
            return await result
        return result

    return await asyncio.wait_for(_dispatch(), timeout=timeout)


async def run_probes(
    model_string: str,
    probe_set: str = "default",
    profiles_path: str | None = None,
    per_probe_timeout: int = 60,
    no_fallback: bool = False,
) -> ModelReport:
    provider, model_name = _get_provider(model_string)

    print(f"Resolving model: {model_string}...", file=sys.stderr)
    used_fallback = False
    try:
        model, used_fallback = _resolve_model(model_string, no_fallback=no_fallback)
    except Exception as e:
        report = ModelReport(
            model_string=model_string,
            provider=provider,
            model_name=model_name,
            timestamp=datetime.now(timezone.utc).isoformat(),
        )
        report.probes.append(ProbeResult(
            name="model_resolution", passed=False, score=0,
            error=f"Failed to resolve model: {e}",
        ))
        report.warnings.append(f"Cannot resolve model: {e}")
        return report

    # Read custom profiles text for routing_decision probe if provided
    profiles_text = None
    if profiles_path:
        with open(profiles_path) as f:
            profiles_text = f.read()

    report = ModelReport(
        model_string=model_string,
        provider=provider,
        model_name=model_name,
        timestamp=datetime.now(timezone.utc).isoformat(),
        probe_set=probe_set,
        resolution_fallback=used_fallback,
    )

    sync_probes: list[tuple[str, Any]] = [
        ("basic_inference", lambda: probe_basic_inference(model)),
        ("tool_calling", lambda: probe_tool_calling(model)),
        ("token_reporting", lambda: probe_token_reporting(model)),
        ("shell_roundtrip", lambda: probe_shell_roundtrip(model)),
        ("shell_failure_recovery", lambda: probe_shell_failure_recovery(model)),
        ("file_write_via_tool", lambda: probe_file_write_via_tool(model)),
        ("multi_turn", lambda: probe_multi_turn(model)),
        ("large_output", lambda: probe_large_output(model)),
        ("reasoning_effort", lambda: probe_reasoning_effort(model, provider, model_name)),
        ("routing_decision", lambda: probe_routing_decision(model, profiles_text)),
        ("context_profile", lambda: probe_context_profile(model, provider=provider, model_name=model_name)),
    ]

    async_probes: list[tuple[str, Any]] = [
        ("streaming", lambda: probe_streaming(model)),
        ("streaming_tokens", lambda: probe_streaming_tokens(model)),
    ]

    all_probes = sync_probes[:3] + async_probes + sync_probes[3:]

    # Filter by probe set
    allowed = PROBE_SETS.get(probe_set)
    if allowed is not None:
        all_probes = [(n, fn) for n, fn in all_probes if n in allowed]

    for name, probe_fn in all_probes:
        print(f"  Running {name}...", file=sys.stderr, end="", flush=True)
        try:
            result = await _invoke_probe_with_timeout(probe_fn, per_probe_timeout)
            report.probes.append(result)
            status = "✓" if result.passed else "✗"
            print(f" {status} ({result.score}/100)", file=sys.stderr)
        except asyncio.TimeoutError:
            # Per-probe timeout (PR #2 P0-A). Record as a failed probe with
            # score=0 and continue to the next probe so one hung API call
            # doesn't block the whole run. See _invoke_probe_with_timeout for
            # the thread-leak caveat.
            print(f" TIMEOUT", file=sys.stderr)
            print(f"[PROBE] {name} TIMEOUT after {per_probe_timeout}s", file=sys.stderr)
            report.probes.append(ProbeResult(
                name=name, passed=False, score=0,
                error=f"timeout after {per_probe_timeout}s",
            ))
        except Exception:
            print(f" ERROR", file=sys.stderr)
            report.probes.append(ProbeResult(
                name=name, passed=False, score=0,
                error=f"Probe crashed: {traceback.format_exc()[-200:]}",
            ))

        # If basic inference fails (including timeout), skip remaining probes.
        # Consistent with Q3 — treat timeout as failure for the gate check.
        if name == "basic_inference" and not report.probes[-1].passed:
            print("  Skipping remaining probes (basic inference failed)", file=sys.stderr)
            break

    # Weighted scoring
    if report.probes:
        total_weight = 0
        weighted_score = 0
        for p in report.probes:
            w = PROBE_WEIGHTS.get(p.name, 5)
            weighted_score += p.score * w
            total_weight += w
        report.overall_score = int(weighted_score / total_weight) if total_weight > 0 else 0

    _generate_recommendations(report)
    return report


def _generate_yaml_profile(report: ModelReport) -> str:
    """Generate a YAML capability profile from probe results."""
    probe_map = {p.name: p for p in report.probes}

    def _probed(name: str) -> bool:
        """Return True if this probe was actually run."""
        return name in probe_map

    def _score(name: str) -> float:
        p = probe_map.get(name)
        return round(p.score / 100.0, 2) if p else 0.0

    def _score_or_default(name: str, default: float = 1.0) -> float:
        """Return probe score if tested, or a safe default if not tested."""
        if _probed(name):
            return _score(name)
        return default

    # Derive context class from profile
    ctx = probe_map.get("context_profile", ProbeResult("", False, 0))
    max_input = (ctx.details.get("max_input_tokens") or 0) if ctx.details else 0
    if max_input >= 500000:
        context_class = "large"
    elif max_input >= 100000:
        context_class = "medium"
    elif max_input > 0:
        context_class = "small"
    else:
        context_class = "unknown"

    # Derive reasoning controls
    re_probe = probe_map.get("reasoning_effort")
    if re_probe and re_probe.passed:
        reasoning_controls = ", ".join(re_probe.details.get("supported", []))
    elif not _probed("reasoning_effort"):
        reasoning_controls = "not_tested"
    else:
        reasoning_controls = "none"

    # Derive autonomy tier
    score = report.overall_score
    if score >= 90:
        autonomy_tier = "full"
    elif score >= 75:
        autonomy_tier = "standard"
    elif score >= 60:
        autonomy_tier = "limited"
    else:
        autonomy_tier = "restricted"

    # Worker/orchestrator eligibility
    worker_eligible = report.oat_compatible
    routing_score = _score("routing_decision")
    orchestrator_eligible = (
        report.oat_compatible
        and (_score("shell_failure_recovery") >= 0.7 or not _probed("shell_failure_recovery"))
        and (_score("multi_turn") >= 0.7 or not _probed("multi_turn"))
        and (_score("routing_decision") >= 0.5 or not _probed("routing_decision"))
        and score >= 80
    )

    # TTFT from streaming probe
    streaming_probe = probe_map.get("streaming")
    ttft_ms = None
    if streaming_probe and streaming_probe.details:
        ttft_ms = streaming_probe.details.get("ttft_ms")

    # Compute latency from probe durations
    latency_probes = [(p.name, p.duration_ms) for p in report.probes if p.duration_ms > 0]
    avg_latency_ms = int(sum(ms for _, ms in latency_probes) / len(latency_probes)) if latency_probes else 0

    # Track which probes were skipped (minimum probe set)
    all_known_probes = {
        "basic_inference", "tool_calling", "streaming", "streaming_tokens",
        "token_reporting", "multi_turn", "large_output", "shell_roundtrip",
        "shell_failure_recovery", "file_write_via_tool", "reasoning_effort",
        "routing_decision", "context_profile",
    }
    probes_skipped = sorted(all_known_probes - set(probe_map.keys()))

    # Use the probe set that was actually requested, not inferred from count
    probe_set_name = report.probe_set

    lines = [
        f"# OAT Model Capability Profile",
        f"# Generated by: oat model onboard / probe-model.py",
        f"# Probe set: {probe_set_name}",
        f"",
        f"model_id: \"{report.model_string}\"",
        f"status: {'known' if report.oat_compatible else 'restricted'}",
        f"source: onboarded",
        f"onboarded_at: \"{report.timestamp[:10]}\"",
        f"",
        f"provider:",
        f"  name: {report.provider}",
        f"",
        f"capabilities:",
        f"  tool_reliability: {_score('tool_calling')}",
        f"  shell_roundtrip: {_score('shell_roundtrip')}",
        f"  shell_recovery: {_score_or_default('shell_failure_recovery')}",
        f"  file_write_reliability: {_score('file_write_via_tool')}",
        f"  token_reporting: {_score('token_reporting')}",
        f"  streaming: {_score_or_default('streaming')}",
        f"  multi_turn: {_score_or_default('multi_turn')}",
        f"  large_output: {_score_or_default('large_output')}",
        f"  effective_context_class: {context_class}",
        f"  max_input_tokens: {max_input if max_input else 'unknown'}",
        f"  reasoning_controls: \"{reasoning_controls}\"",
        f"",
    ]

    # Latency section (from probe durations)
    if latency_probes or ttft_ms is not None:
        lines.append("latency:")
        if ttft_ms is not None:
            lines.append(f"  ttft_ms: {ttft_ms}")
        for probe_name, ms in latency_probes:
            lines.append(f"  {probe_name}_ms: {ms}")
        lines.append(f"  avg_ms: {avg_latency_ms}")
        lines.append("")

    lines.extend([
        f"routing:",
        f"  autonomy_tier: {autonomy_tier}",
        f"  overall_score: {report.overall_score}",
        f"",
        f"contract:",
        f"  onboarding_passed: {str(report.oat_compatible).lower()}",
        f"  worker_eligible: {str(worker_eligible).lower()}",
        f"  orchestrator_eligible: {str(orchestrator_eligible).lower()}",
        f"",
        f"evidence:",
        f"  probe_version: 2",
        f"  probe_set: {probe_set_name}",
        f"  probes_run: {len(report.probes)}",
        f"  probes_passed: {sum(1 for p in report.probes if p.passed)}",
        f"  resolution_fallback: {str(report.resolution_fallback).lower()}",
    ])

    if probes_skipped:
        lines.append(f"  probes_skipped:")
        for s in probes_skipped:
            lines.append(f"    - \"{s}\"")

    # Emit probe_errors map when any probe recorded an error (timeout, crash,
    # or expected failure) — operators use this to diagnose why a probe
    # scored zero without grepping stderr. Q18: nested YAML map.
    probe_errors = [(p.name, p.error) for p in report.probes if p.error]
    if probe_errors:
        lines.append(f"  probe_errors:")
        for name, err in probe_errors:
            # Truncate to 200 chars to keep YAML readable; quote the value so
            # colons/newlines don't break the flat-key parser.
            truncated = err.replace("\n", " ").replace('"', "'")[:200]
            lines.append(f"    {name}: \"{truncated}\"")

    if report.warnings:
        lines.append("")
        lines.append("warnings:")
        for w in report.warnings:
            lines.append(f"  - \"{w}\"")

    return "\n".join(lines) + "\n"


def _save_profile(report: ModelReport, yaml_content: str) -> str:
    """Save YAML profile to model-routing/profiles/ and return the path."""
    import shutil

    safe_name = report.model_string.replace(":", "__").replace("/", "__")
    profile_dir = os.path.join(_REPO_ROOT, "model-routing", "profiles")
    os.makedirs(profile_dir, exist_ok=True)
    filepath = os.path.join(profile_dir, f"{safe_name}.yaml")

    if os.path.exists(filepath):
        try:
            with open(filepath) as f:
                old_content = f.read()
            old_score = 0
            for line in old_content.split("\n"):
                if "overall_score:" in line:
                    old_score = int(line.split(":")[1].strip())
                    break
            if report.overall_score < old_score:
                backup_path = filepath + ".bak"
                shutil.copy2(filepath, backup_path)
                print(f"  Backed up previous profile (score {old_score}) to {backup_path}", file=sys.stderr)
                print(f"  New score: {report.overall_score} (downgrade detected — backup preserved)", file=sys.stderr)
        except Exception:
            pass

    with open(filepath, "w") as f:
        f.write(yaml_content)
    return filepath


def main():
    parser = argparse.ArgumentParser(
        description="OAT Model Capability Probe — onboard any model",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("model", help="Model string (e.g., anthropic:claude-sonnet-4-6)")
    parser.add_argument("--output", "-o", help="Save JSON report to file")
    parser.add_argument(
        "--probe-set", choices=["minimum", "default"], default="default",
        help="Probe set: minimum (fast gate ~1 min) or default (full ~3 min)",
    )
    parser.add_argument(
        "--save", action="store_true",
        help="Save YAML capability profile to model-routing/profiles/",
    )
    parser.add_argument(
        "--profiles", default=None,
        help="Path to custom model profiles text for routing_decision probe",
    )
    parser.add_argument(
        "--per-probe-timeout", type=int, default=60,
        help=(
            "Max seconds per probe (default: 60). On timeout the probe is "
            "recorded score=0 with error='timeout after Xs' and the run "
            "continues. Cold-start local models (first Ollama invocation) "
            "may need a higher value."
        ),
    )
    parser.add_argument(
        "--no-fallback", action="store_true",
        help=(
            "Skip the init_chat_model fallback when create_model fails. "
            "Useful for debugging ~/.oat/config.toml issues — without this "
            "flag, a config typo would fall through to a generic 'unknown "
            "model' error."
        ),
    )
    args = parser.parse_args()

    report = asyncio.run(run_probes(
        args.model,
        probe_set=args.probe_set,
        profiles_path=args.profiles,
        per_probe_timeout=args.per_probe_timeout,
        no_fallback=args.no_fallback,
    ))
    _print_report(report)

    report_dict = asdict(report)
    json_str = json.dumps(report_dict, indent=2)

    if args.output:
        os.makedirs(os.path.dirname(args.output) or ".", exist_ok=True)
        with open(args.output, "w") as f:
            f.write(json_str)
        print(f"Report saved to {args.output}", file=sys.stderr)
    else:
        print(json_str)

    if args.save:
        probes_passed = sum(1 for p in report.probes if p.passed)
        if report.overall_score == 0 and probes_passed == 0:
            print(f"NOT saving profile — all probes failed (likely a dependency or API key issue).", file=sys.stderr)
            print(f"Fix the issue and re-run: oat model onboard {args.model}", file=sys.stderr)
            sys.exit(1)
        else:
            yaml_content = _generate_yaml_profile(report)
            filepath = _save_profile(report, yaml_content)
            print(f"Profile saved to {filepath}", file=sys.stderr)
            print(f"\nProfile contents:", file=sys.stderr)
            print(yaml_content, file=sys.stderr)


if __name__ == "__main__":
    main()
