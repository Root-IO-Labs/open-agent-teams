"""ChatOpenAI subclass that rescues reasoning-only responses.

Some OpenAI-compatible servers -- notably vLLM with a reasoning parser such as
``nemotron_v3`` -- put a turn's entire answer in the response's ``reasoning``
(a.k.a. ``reasoning_content``) field and leave ``content`` empty, even for a
final user-facing answer that contains no tool call. This shows up most often
in multi-turn conversations that already include tool history.

Base ``ChatOpenAI`` does not extract that field (see the langchain_openai
docstring: "Custom provider fields ... are not extracted. Use a
provider-specific subclass"). The non-streaming result path
(``_convert_dict_to_message``) keeps only ``content``, ``tool_calls``,
``function_call`` and ``audio`` -- so the reasoning text is dropped and the
agent emits an empty turn. The OAT agents bind tools and run with
``disable_streaming="tool_calling"``, so they always hit this non-streaming
path.

This subclass captures the reasoning trace into ``additional_kwargs`` and,
when a turn has empty content AND no tool calls, promotes the reasoning to
``content`` so the answer is not lost. Turns that already have content or a
tool call are left untouched, so well-behaved models and tool-calling turns
are unaffected and structured tool calls stay structured.
"""

from __future__ import annotations

from typing import Any

from langchain_core.messages import AIMessage
from langchain_core.outputs import ChatResult
from langchain_openai import ChatOpenAI

# Keys OpenAI-compatible reasoning servers use for the reasoning trace. vLLM's
# newer parsers emit ``reasoning``; DeepSeek-style servers use
# ``reasoning_content``. Checked in order.
_REASONING_KEYS = ("reasoning", "reasoning_content")


def _is_blank(content: Any) -> bool:
    """Whether message content carries no user-visible text.

    Handles the str form and the list-of-blocks form ``ChatOpenAI`` can emit.
    """
    if content is None:
        return True
    if isinstance(content, str):
        return not content.strip()
    if isinstance(content, list):
        for block in content:
            if isinstance(block, str) and block.strip():
                return False
            if isinstance(block, dict) and str(block.get("text", "")).strip():
                return False
        return True
    return not bool(content)


def _extract_reasoning(raw_message: dict[str, Any]) -> str | None:
    """Return the reasoning trace from a raw response message dict, if any."""
    for key in _REASONING_KEYS:
        value = raw_message.get(key)
        if isinstance(value, str) and value.strip():
            return value
    return None


class ReasoningFallbackChatOpenAI(ChatOpenAI):
    """``ChatOpenAI`` that promotes reasoning-only turns to message content.

    Drop-in replacement for ``langchain_openai:ChatOpenAI``: point a provider's
    ``class_path`` at ``oat_cli.reasoning_fallback:ReasoningFallbackChatOpenAI``
    to enable the rescue behavior. All other behavior is inherited unchanged.
    """

    def _create_chat_result(
        self,
        response: Any,
        generation_info: dict | None = None,
    ) -> ChatResult:
        result = super()._create_chat_result(response, generation_info)

        response_dict = (
            response if isinstance(response, dict) else response.model_dump()
        )
        choices = response_dict.get("choices") or []

        # super() builds one generation per choice in order, so zip aligns them.
        for generation, choice in zip(result.generations, choices):
            message = generation.message
            if not isinstance(message, AIMessage):
                continue
            raw_message = (choice or {}).get("message") or {}
            reasoning = _extract_reasoning(raw_message)
            if not reasoning:
                continue
            # Preserve the trace so downstream code/UI can still surface it.
            message.additional_kwargs.setdefault("reasoning", reasoning)
            # Only rescue genuinely empty, non-tool turns: never overwrite a
            # real answer or clobber a (structured) tool call.
            has_tool_calls = bool(
                getattr(message, "tool_calls", None)
                or getattr(message, "invalid_tool_calls", None)
            )
            if _is_blank(message.content) and not has_tool_calls:
                message.content = reasoning

        return result
