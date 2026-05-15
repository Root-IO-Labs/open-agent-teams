#!/usr/bin/env python3
"""OAT benchmark LLM helper — provider-agnostic single-shot completion.

Used by ``benchmarks/summarize.sh`` and ``benchmarks/judge-blackbox.sh`` so
they can target any model OAT supports (anthropic, openai, google_genai,
openrouter, deepseek, ollama, custom config.toml providers, ...) instead of
calling the Anthropic REST API directly with curl.

Resolution path mirrors ``benchmarks/probe-model.py``: try
``oat_cli.config.create_model`` first (honors ``~/.oat/config.toml``
custom providers), then fall back to ``langchain.chat_models.init_chat_model``.

Usage::

    python benchmarks/llm_call.py \\
        --model anthropic:claude-sonnet-4-6 \\
        --payload /tmp/prompt.json

Payload JSON::

    {
        "system": "...optional system prompt...",
        "messages": [{"role": "user", "content": "..."}],
        "max_tokens": 8192
    }

Output (single JSON line on stdout)::

    {"text": "...", "input_tokens": 1234, "output_tokens": 567,
     "model": "anthropic:claude-sonnet-4-6", "provider": "anthropic"}

All progress / log / warning lines go to stderr. The bash side can capture
stdout with ``result=$(python benchmarks/llm_call.py ... 2>>logfile)``
without contamination.

Exit codes:
    0  success
    2  no API key for resolved provider (and provider isn't local)
    3  provider call failed (network error, rate limit, etc.) after retries
    4  model resolution failed (unknown provider, bad config.toml, ...)
    1  other / unexpected error
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
from typing import Any

# Add OAT's Python paths so we can import oat_cli.config and langchain
# without requiring the caller to activate the venv. Mirrors probe-model.py
# lines 56-63 — keep these in sync.
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_SCRIPT_DIR)
_AGENT_RUNTIME = os.path.join(_REPO_ROOT, "agent-runtime")
for _subdir in ("libs/cli", "libs/oat_sdk"):
    _path = os.path.join(_AGENT_RUNTIME, _subdir)
    if _path not in sys.path:
        sys.path.insert(0, _path)


# Exit codes — keep stable; bash callers branch on these.
EXIT_OK = 0
EXIT_GENERIC_ERROR = 1
EXIT_NO_API_KEY = 2
EXIT_PROVIDER_ERROR = 3
EXIT_RESOLUTION_ERROR = 4

# Map provider prefix → required env var. Local providers map to None.
# `None` means "no key required". Order matters only for the resolved
# provider lookup; we compare exact provider strings here.
PROVIDER_ENV_VARS: dict[str, str | None] = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google_genai": "GOOGLE_API_KEY",
    "google": "GOOGLE_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "groq": "GROQ_API_KEY",
    "mistralai": "MISTRAL_API_KEY",
    "cohere": "COHERE_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
    "together": "TOGETHER_API_KEY",
    "ollama": None,
}


def _log(msg: str) -> None:
    """Stderr-only log so stdout stays clean for the JSON result."""
    print(msg, file=sys.stderr, flush=True)


def _split_provider(model_string: str) -> tuple[str, str]:
    """Split ``provider:model`` into ``(provider, model_name)``.

    Bare strings (no colon) default to ``anthropic`` to preserve the
    benchmark scripts' historical behavior — every script today defaults to
    ``claude-sonnet-4-6`` without a prefix.
    """
    if ":" in model_string:
        provider, name = model_string.split(":", 1)
        return provider, name
    return "anthropic", model_string


def _resolve_model(model_string: str) -> tuple[Any, str]:
    """Resolve a model string into a ready-to-call LangChain chat model.

    Returns ``(model, resolved_string)`` where ``resolved_string`` is the
    canonical ``provider:model`` form — bare names are normalized to
    ``anthropic:<name>`` so callers (and ``gate.json`` / ``summary.md``
    provenance headers) record the actual provider that was used.

    Two-path resolution mirrors ``probe-model.py``:

    1. ``oat_cli.config.create_model`` — honors ``~/.oat/config.toml``
       custom providers.
    2. ``langchain.chat_models.init_chat_model`` — fallback for installations
       without the OAT CLI venv (e.g. in CI).
    """
    provider, model_name = _split_provider(model_string)
    canonical = f"{provider}:{model_name}"

    # Path 1: OAT's create_model (preferred — supports config.toml).
    try:
        from oat_cli.config import create_model  # type: ignore[import-not-found]
    except ImportError as exc:
        _log(f"WARN: oat_cli.config unavailable ({type(exc).__name__}): {exc}")
    else:
        try:
            wrapper = create_model(canonical)
            return wrapper.model, canonical
        except Exception as exc:  # noqa: BLE001
            _log(f"WARN: create_model failed ({type(exc).__name__}): {exc}")

    # Path 2: vanilla init_chat_model.
    from langchain.chat_models import init_chat_model  # type: ignore[import-not-found]

    if canonical.startswith("openai:"):
        model = init_chat_model(canonical, use_responses_api=True)
    else:
        model = init_chat_model(canonical)
    return model, canonical


def _check_api_key(provider: str) -> str | None:
    """Return None if the provider has its key (or doesn't need one), else
    a human-readable error message naming the missing env var."""
    env_var = PROVIDER_ENV_VARS.get(provider)
    if env_var is None:
        if provider not in PROVIDER_ENV_VARS:
            # Unknown provider: don't block — let the actual API call surface
            # the real error, since custom providers via config.toml may use
            # arbitrary credential schemes.
            return None
        # Local provider (ollama) — no key required.
        return None
    if not os.environ.get(env_var):
        return (
            f"Missing API key: {env_var} is not set. "
            f"Provider '{provider}' requires this environment variable."
        )
    return None


def _build_messages(payload: dict[str, Any]) -> list[Any]:
    """Convert a payload's ``messages`` list into LangChain message objects.

    Accepts the same shape both summarize.sh and judge-blackbox.sh build:
    a list of ``{"role": "...", "content": "..."}`` dicts, plus an optional
    top-level ``system`` field.
    """
    from langchain_core.messages import (  # type: ignore[import-not-found]
        AIMessage,
        HumanMessage,
        SystemMessage,
    )

    out: list[Any] = []
    system = payload.get("system")
    if system:
        out.append(SystemMessage(content=str(system)))

    for msg in payload.get("messages", []):
        role = msg.get("role", "user").lower()
        content = msg.get("content", "")
        if role == "system":
            out.append(SystemMessage(content=str(content)))
        elif role == "assistant":
            out.append(AIMessage(content=str(content)))
        else:
            out.append(HumanMessage(content=str(content)))

    return out


def _extract_text(response: Any) -> str:
    """Flatten LangChain message content into a plain string.

    Anthropic returns a list of content blocks (``[{"type": "text",
    "text": "..."}]``); OpenAI returns a string. Normalize both.
    """
    content = getattr(response, "content", "")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, dict):
                # Anthropic-style {"type": "text", "text": "..."}
                if "text" in block:
                    parts.append(str(block["text"]))
                elif block.get("type") == "text":
                    parts.append(str(block.get("text", "")))
            else:
                parts.append(str(block))
        return "".join(parts)
    return str(content)


def _extract_tokens(response: Any) -> tuple[int, int]:
    """Normalize provider-specific usage shapes to ``(input, output)``.

    LangChain exposes a uniform ``usage_metadata`` dict with
    ``input_tokens`` / ``output_tokens`` keys, but some older providers fall
    back to ``response_metadata['token_usage']`` with OpenAI-style keys
    (``prompt_tokens`` / ``completion_tokens``) or Google's
    ``usage_metadata`` with prompt/candidate keys. Try them in order.
    """
    usage = getattr(response, "usage_metadata", None)
    if usage:
        in_t = usage.get("input_tokens")
        out_t = usage.get("output_tokens")
        if in_t is None:
            in_t = usage.get("prompt_tokens", 0) or usage.get("prompt_token_count", 0)
        if out_t is None:
            out_t = (
                usage.get("completion_tokens", 0)
                or usage.get("candidates_token_count", 0)
            )
        return int(in_t or 0), int(out_t or 0)

    meta = getattr(response, "response_metadata", None) or {}
    token_usage = meta.get("token_usage") or meta.get("usage") or {}
    in_t = (
        token_usage.get("input_tokens")
        or token_usage.get("prompt_tokens")
        or token_usage.get("prompt_token_count")
        or 0
    )
    out_t = (
        token_usage.get("output_tokens")
        or token_usage.get("completion_tokens")
        or token_usage.get("candidates_token_count")
        or 0
    )
    return int(in_t or 0), int(out_t or 0)


async def _invoke_with_retries(
    model: Any,
    messages: list[Any],
    max_tokens: int | None,
    max_attempts: int = 3,
    initial_backoff: float = 5.0,
) -> Any:
    """Call ``model.ainvoke`` with provider-uniform retry/backoff.

    Each langchain provider has its own retry behavior; this wraps them with
    a known-good 3-attempt exponential backoff so summarize.sh /
    judge-blackbox.sh see consistent semantics regardless of the model.
    """
    last_exc: Exception | None = None
    backoff = initial_backoff
    invoke_kwargs: dict[str, Any] = {}
    if max_tokens is not None:
        invoke_kwargs["max_tokens"] = max_tokens

    for attempt in range(1, max_attempts + 1):
        try:
            if invoke_kwargs:
                return await model.ainvoke(messages, **invoke_kwargs)
            return await model.ainvoke(messages)
        except Exception as exc:  # noqa: BLE001 — surface any provider error
            last_exc = exc
            if attempt < max_attempts:
                _log(
                    f"Attempt {attempt}/{max_attempts} failed "
                    f"({type(exc).__name__}: {exc}). "
                    f"Retrying in {backoff:.0f}s..."
                )
                await asyncio.sleep(backoff)
                backoff *= 2
            else:
                _log(
                    f"Attempt {attempt}/{max_attempts} failed "
                    f"({type(exc).__name__}: {exc}). Giving up."
                )

    assert last_exc is not None
    raise last_exc


def main() -> int:
    parser = argparse.ArgumentParser(
        description="OAT benchmark LLM helper (langchain-backed)",
    )
    parser.add_argument(
        "--model",
        required=True,
        help="Model string, e.g. anthropic:claude-sonnet-4-6 or openai:gpt-5.2",
    )
    parser.add_argument(
        "--payload",
        required=True,
        help="Path to JSON file with {system, messages, max_tokens}",
    )
    parser.add_argument(
        "--max-attempts",
        type=int,
        default=3,
        help="Retry attempts for provider call (default: 3)",
    )
    args = parser.parse_args()

    if not os.path.isfile(args.payload):
        _log(f"Error: payload file not found: {args.payload}")
        return EXIT_GENERIC_ERROR

    try:
        with open(args.payload) as f:
            payload = json.load(f)
    except json.JSONDecodeError as exc:
        _log(f"Error: payload is not valid JSON: {exc}")
        return EXIT_GENERIC_ERROR

    if not isinstance(payload, dict):
        _log("Error: payload must be a JSON object")
        return EXIT_GENERIC_ERROR

    provider, _ = _split_provider(args.model)
    key_err = _check_api_key(provider)
    if key_err:
        _log(f"Error: {key_err}")
        return EXIT_NO_API_KEY

    _log(f"Resolving model: {args.model}...")
    try:
        model, resolved_string = _resolve_model(args.model)
    except Exception as exc:  # noqa: BLE001
        _log(f"Error: failed to resolve model '{args.model}': "
             f"{type(exc).__name__}: {exc}")
        return EXIT_RESOLUTION_ERROR

    resolved_provider, _ = _split_provider(resolved_string)

    # Re-check the env var if resolution changed the provider (bare name
    # got prefixed). This catches the case where the user passes
    # `claude-sonnet-4-6` with no key set — we want NO_API_KEY, not a
    # confusing provider error.
    if resolved_provider != provider:
        key_err = _check_api_key(resolved_provider)
        if key_err:
            _log(f"Error: {key_err}")
            return EXIT_NO_API_KEY

    messages = _build_messages(payload)
    if not messages:
        _log("Error: payload contained no messages")
        return EXIT_GENERIC_ERROR

    max_tokens = payload.get("max_tokens")
    if max_tokens is not None:
        try:
            max_tokens = int(max_tokens)
        except (TypeError, ValueError):
            _log(f"Warning: ignoring non-integer max_tokens: {max_tokens!r}")
            max_tokens = None

    _log(f"Calling {resolved_string}...")
    start = time.monotonic()
    try:
        response = asyncio.run(
            _invoke_with_retries(
                model,
                messages,
                max_tokens=max_tokens,
                max_attempts=args.max_attempts,
            )
        )
    except Exception as exc:  # noqa: BLE001
        _log(f"Error: provider call failed: {type(exc).__name__}: {exc}")
        return EXIT_PROVIDER_ERROR

    elapsed_ms = int((time.monotonic() - start) * 1000)

    text = _extract_text(response)
    in_tokens, out_tokens = _extract_tokens(response)

    if not text:
        _log("Error: provider returned an empty response")
        return EXIT_PROVIDER_ERROR

    # Truncation hint for reasoning models that may consume max_tokens on
    # hidden thinking and clip the visible JSON the judge expects.
    if max_tokens and out_tokens and out_tokens >= max_tokens - 16:
        _log(
            f"Warning: output_tokens ({out_tokens}) is at/near max_tokens "
            f"({max_tokens}); response may be truncated."
        )

    _log(
        f"Done in {elapsed_ms}ms — input={in_tokens} output={out_tokens} tokens"
    )

    result = {
        "text": text,
        "input_tokens": in_tokens,
        "output_tokens": out_tokens,
        "model": resolved_string,
        "provider": resolved_provider,
    }
    print(json.dumps(result), flush=True)
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
