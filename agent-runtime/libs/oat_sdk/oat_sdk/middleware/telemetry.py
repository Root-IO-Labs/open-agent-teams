"""Langfuse telemetry middleware for the OAT Python agent runtime.

Wraps each model call with a Langfuse ``generation`` event and emits a
``span`` event for every tool call observed in the model's response. All
events attach to the trace ID the Go daemon assigned at spawn time
(``OAT_TRACE_ID``), so the resulting Langfuse trace tree contains:

- router decision           (emitted by Go daemon)
- agent_start / agent_end   (emitted by Go daemon)
- one generation per LLM call    ← this middleware
- one span per tool call         ← this middleware

When telemetry is off (no creds / no trace ID), every hook is a no-op:
``get_client()`` returns ``None`` and we pass the request through
unchanged with zero allocation overhead.

Failure policy: telemetry never throws. Any exception in the
instrumentation path is caught and logged at debug, and the original
model call is preserved. The agent must keep running even if Langfuse
is unreachable, misconfigured, or returning 5xx.
"""
from __future__ import annotations

import logging
import time
from datetime import datetime, timezone
from typing import Any, Callable

from langchain.agents.middleware import AgentMiddleware, AgentState
from langgraph.runtime import Runtime

from oat_sdk.middleware._langfuse_client import get_client

logger = logging.getLogger("oat.telemetry")


def _iso_now() -> str:
    now = datetime.now(timezone.utc)
    return now.strftime("%Y-%m-%dT%H:%M:%S.") + f"{now.microsecond // 1000:03d}Z"


def _extract_usage(response: Any) -> tuple[int, int, str]:
    """Pull ``(input_tokens, output_tokens, model_name)`` from a model response.

    LangChain providers attach token usage in slightly different shapes:
    Anthropic exposes ``usage_metadata`` on the AIMessage, OpenAI uses
    ``response_metadata['token_usage']``, others vary. We try the common
    shapes and fall back to zeros. Model name comes from ``response_metadata``
    or the message's ``id``.
    """
    input_tokens = 0
    output_tokens = 0
    model = ""

    # ModelResponse typically has .result with the AIMessage(s).
    messages = getattr(response, "result", None)
    if messages is None:
        messages = [response]
    elif not isinstance(messages, list):
        messages = [messages]

    for msg in messages:
        # langchain v1 AIMessage exposes usage_metadata directly.
        usage = getattr(msg, "usage_metadata", None) or {}
        if usage:
            input_tokens += int(usage.get("input_tokens", 0) or 0)
            output_tokens += int(usage.get("output_tokens", 0) or 0)
        meta = getattr(msg, "response_metadata", {}) or {}
        tu = meta.get("token_usage") or meta.get("usage") or {}
        if tu and not usage:
            input_tokens += int(tu.get("prompt_tokens", tu.get("input_tokens", 0)) or 0)
            output_tokens += int(tu.get("completion_tokens", tu.get("output_tokens", 0)) or 0)
        if not model:
            model = (
                meta.get("model_name")
                or meta.get("model")
                or getattr(msg, "name", "")
                or ""
            )
    return input_tokens, output_tokens, model


def _extract_tool_calls(response: Any) -> list[dict[str, Any]]:
    """Return the list of tool_calls seen on the AIMessage(s) in the response."""
    calls: list[dict[str, Any]] = []
    messages = getattr(response, "result", None)
    if messages is None:
        messages = [response]
    elif not isinstance(messages, list):
        messages = [messages]
    for msg in messages:
        tc = getattr(msg, "tool_calls", None) or []
        for c in tc:
            calls.append(c if isinstance(c, dict) else dict(c))
    return calls


class LangfuseMiddleware(AgentMiddleware):
    """Emit Langfuse generations and tool spans on the trace the daemon opened.

    Added to the agent middleware stack in ``create_oat_agent``. Cheap to
    construct: just instantiates the singleton client lazily on first emit.
    When telemetry env vars are missing, the middleware is effectively a
    pass-through.
    """

    def wrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        client = get_client()
        if client is None:
            return handler(request)

        start_time = _iso_now()
        wall_start = time.monotonic()
        try:
            response = handler(request)
        except Exception as e:  # noqa: BLE001 — re-raised after telemetry
            end_time = _iso_now()
            try:
                client.emit_generation(
                    name="llm_call",
                    model="unknown",
                    input_tokens=0,
                    output_tokens=0,
                    start_time=start_time,
                    end_time=end_time,
                    level="ERROR",
                    metadata={
                        "error": type(e).__name__,
                        "wall_ms": int((time.monotonic() - wall_start) * 1000),
                    },
                )
            except Exception:  # noqa: BLE001
                logger.debug("telemetry emit_generation (error path) failed", exc_info=True)
            raise

        end_time = _iso_now()
        wall_ms = int((time.monotonic() - wall_start) * 1000)
        try:
            input_tokens, output_tokens, model = _extract_usage(response)
            client.emit_generation(
                name="llm_call",
                model=model,
                input_tokens=input_tokens,
                output_tokens=output_tokens,
                start_time=start_time,
                end_time=end_time,
                metadata={"wall_ms": wall_ms},
            )
            # Tool calls discovered in the LLM response — emit one span per call.
            for tc in _extract_tool_calls(response):
                name = tc.get("name") or "unknown"
                client.emit_tool_span(
                    name=name,
                    start_time=end_time,
                    end_time=end_time,
                    metadata={
                        "tool_call_id": tc.get("id", ""),
                        "arg_count": len(tc.get("args", {}) or {}),
                    },
                )
        except Exception:  # noqa: BLE001 — telemetry never breaks the agent
            logger.debug("telemetry emit failed (non-fatal)", exc_info=True)
        return response

    async def awrap_model_call(self, request: Any, handler: Callable[[Any], Any]) -> Any:
        # The async path mirrors the sync path. We don't try to keep them
        # in lockstep via shared helpers because the handler signature can
        # differ (Awaitable vs plain Callable) and dispatch cost matters here.
        client = get_client()
        if client is None:
            return await handler(request)

        start_time = _iso_now()
        wall_start = time.monotonic()
        try:
            response = await handler(request)
        except Exception as e:  # noqa: BLE001
            end_time = _iso_now()
            try:
                client.emit_generation(
                    name="llm_call",
                    model="unknown",
                    input_tokens=0,
                    output_tokens=0,
                    start_time=start_time,
                    end_time=end_time,
                    level="ERROR",
                    metadata={
                        "error": type(e).__name__,
                        "wall_ms": int((time.monotonic() - wall_start) * 1000),
                    },
                )
            except Exception:  # noqa: BLE001
                logger.debug("telemetry emit_generation (error path) failed", exc_info=True)
            raise

        end_time = _iso_now()
        wall_ms = int((time.monotonic() - wall_start) * 1000)
        try:
            input_tokens, output_tokens, model = _extract_usage(response)
            client.emit_generation(
                name="llm_call",
                model=model,
                input_tokens=input_tokens,
                output_tokens=output_tokens,
                start_time=start_time,
                end_time=end_time,
                metadata={"wall_ms": wall_ms},
            )
            for tc in _extract_tool_calls(response):
                name = tc.get("name") or "unknown"
                client.emit_tool_span(
                    name=name,
                    start_time=end_time,
                    end_time=end_time,
                    metadata={
                        "tool_call_id": tc.get("id", ""),
                        "arg_count": len(tc.get("args", {}) or {}),
                    },
                )
        except Exception:  # noqa: BLE001
            logger.debug("telemetry emit failed (non-fatal)", exc_info=True)
        return response


__all__ = ["LangfuseMiddleware"]
