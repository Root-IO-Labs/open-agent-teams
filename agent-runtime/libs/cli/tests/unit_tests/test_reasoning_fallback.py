"""Tests for ReasoningFallbackChatOpenAI reasoning->content rescue."""

from __future__ import annotations

from typing import Any

import pytest
from langchain_core.messages import AIMessage

from oat_cli.reasoning_fallback import (
    ReasoningFallbackChatOpenAI,
    _extract_reasoning,
    _is_blank,
)


def _make_model() -> ReasoningFallbackChatOpenAI:
    """Construct the model without touching the network."""
    return ReasoningFallbackChatOpenAI(
        model="test-model",
        api_key="EMPTY",
        base_url="http://localhost:9/v1",
    )


def _response(message: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": "chatcmpl-test",
        "model": "test-model",
        "choices": [{"index": 0, "finish_reason": "stop", "message": message}],
    }


def _message(result: Any) -> AIMessage:
    msg = result.generations[0].message
    assert isinstance(msg, AIMessage)
    return msg


@pytest.mark.parametrize("key", ["reasoning", "reasoning_content"])
def test_empty_content_no_tools_promotes_reasoning(key: str) -> None:
    """The canonical bug: answer trapped in reasoning, content empty, no tools."""
    model = _make_model()
    answer = "There are 2 python files."
    result = model._create_chat_result(
        _response({"role": "assistant", "content": "", key: answer})
    )
    msg = _message(result)
    assert msg.content == answer
    assert msg.additional_kwargs.get("reasoning") == answer


def test_content_present_is_not_overwritten() -> None:
    """A real answer in content must never be clobbered by reasoning."""
    model = _make_model()
    result = model._create_chat_result(
        _response(
            {
                "role": "assistant",
                "content": "The real answer.",
                "reasoning": "internal scratchpad",
            }
        )
    )
    msg = _message(result)
    assert msg.content == "The real answer."
    # Trace is still preserved for downstream visibility.
    assert msg.additional_kwargs.get("reasoning") == "internal scratchpad"


def test_tool_call_is_not_clobbered() -> None:
    """An empty-content turn that carries a tool call stays a tool call."""
    model = _make_model()
    result = model._create_chat_result(
        _response(
            {
                "role": "assistant",
                "content": "",
                "reasoning": "I should call the tool",
                "tool_calls": [
                    {
                        "id": "call_1",
                        "type": "function",
                        "function": {
                            "name": "get_weather",
                            "arguments": '{"city": "Tokyo"}',
                        },
                    }
                ],
            }
        )
    )
    msg = _message(result)
    assert msg.content == ""
    assert len(msg.tool_calls) == 1
    assert msg.tool_calls[0]["name"] == "get_weather"
    assert msg.additional_kwargs.get("reasoning") == "I should call the tool"


def test_empty_content_no_reasoning_stays_empty() -> None:
    """Without a reasoning trace there is nothing to rescue."""
    model = _make_model()
    result = model._create_chat_result(
        _response({"role": "assistant", "content": ""})
    )
    msg = _message(result)
    assert msg.content == ""
    assert "reasoning" not in msg.additional_kwargs


def test_whitespace_reasoning_is_ignored() -> None:
    """A blank/whitespace reasoning field is treated as absent."""
    model = _make_model()
    result = model._create_chat_result(
        _response({"role": "assistant", "content": "", "reasoning": "   \n  "})
    )
    msg = _message(result)
    assert msg.content == ""
    assert "reasoning" not in msg.additional_kwargs


def test_is_blank() -> None:
    assert _is_blank(None)
    assert _is_blank("")
    assert _is_blank("   \n")
    assert _is_blank([])
    assert _is_blank([{"type": "text", "text": "  "}])
    assert not _is_blank("hi")
    assert not _is_blank([{"type": "text", "text": "hi"}])
    assert not _is_blank(["hi"])


def test_extract_reasoning_prefers_reasoning_key() -> None:
    assert _extract_reasoning({"reasoning": "a", "reasoning_content": "b"}) == "a"
    assert _extract_reasoning({"reasoning_content": "b"}) == "b"
    assert _extract_reasoning({"content": "x"}) is None
    assert _extract_reasoning({"reasoning": "   "}) is None
