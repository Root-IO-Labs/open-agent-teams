#!/usr/bin/env python3
"""Tests for benchmarks/llm_call.py.

Fully mocked — no network calls, no API keys, no token cost. Validates the
shell-facing contract that summarize.sh and judge-blackbox.sh depend on:

  1. Bare model strings vs ``provider:model`` strings parse the same way.
  2. Missing API key for the resolved provider returns exit 2 with a clear
     stderr message and writes nothing to stdout.
  3. Stdout contains exactly one JSON object; stderr contains the log lines.
  4. Token-usage normalization handles Anthropic-shape, OpenAI-shape, and
     Google-shape responses identically.
  5. Provider call failures and resolution failures map to distinct exit
     codes (3 vs 4) so bash callers can branch on them.

Pattern follows benchmarks/test_probe_model.py — same MockMessage / MockModel
helpers and same importlib trick to load the underscore-named module.
"""

from __future__ import annotations

import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch


# Add the benchmarks dir to sys.path so we can import llm_call.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import llm_call  # noqa: E402


# ---------------------------------------------------------------------------
# Mock helpers — minimal LangChain response surface used by llm_call.py
# ---------------------------------------------------------------------------


class MockResponse:
    """Minimal stand-in for a LangChain AIMessage."""

    def __init__(
        self,
        content: Any = "",
        usage_metadata: dict[str, Any] | None = None,
        response_metadata: dict[str, Any] | None = None,
    ):
        self.content = content
        self.usage_metadata = usage_metadata
        self.response_metadata = response_metadata or {}


class MockChatModel:
    """Stand-in for a LangChain chat model with an async ``ainvoke``."""

    def __init__(self, response: Any = None, raises: Exception | None = None):
        self._response = response
        self._raises = raises
        self.ainvoke = AsyncMock(side_effect=self._ainvoke)
        self.last_kwargs: dict[str, Any] = {}

    async def _ainvoke(self, messages: Any, **kwargs: Any) -> Any:
        self.last_kwargs = kwargs
        if self._raises is not None:
            raise self._raises
        return self._response


def _write_payload(tmpdir: str, payload: dict[str, Any]) -> str:
    """Write a payload JSON file under ``tmpdir`` and return its path."""
    path = os.path.join(tmpdir, "payload.json")
    with open(path, "w") as f:
        json.dump(payload, f)
    return path


def _run_main(argv: list[str], env: dict[str, str] | None = None) -> tuple[int, str, str]:
    """Run llm_call.main() with patched argv/env, capture stdout+stderr.

    Returns (exit_code, stdout, stderr).
    """
    stdout_buf = io.StringIO()
    stderr_buf = io.StringIO()

    full_env = dict(os.environ)
    if env is not None:
        # Wipe and replace so missing keys actually look missing.
        full_env = dict(env)

    with patch.object(sys, "argv", ["llm_call.py"] + argv), \
            patch.dict(os.environ, full_env, clear=True), \
            redirect_stdout(stdout_buf), \
            redirect_stderr(stderr_buf):
        exit_code = llm_call.main()

    return exit_code, stdout_buf.getvalue(), stderr_buf.getvalue()


# ---------------------------------------------------------------------------
# _split_provider — bare names default to anthropic
# ---------------------------------------------------------------------------


class TestSplitProvider(unittest.TestCase):
    def test_bare_name_defaults_to_anthropic(self):
        provider, name = llm_call._split_provider("claude-sonnet-4-6")
        self.assertEqual(provider, "anthropic")
        self.assertEqual(name, "claude-sonnet-4-6")

    def test_provider_prefix_parsed(self):
        provider, name = llm_call._split_provider("openai:gpt-5.2")
        self.assertEqual(provider, "openai")
        self.assertEqual(name, "gpt-5.2")

    def test_openrouter_with_slash_in_name(self):
        provider, name = llm_call._split_provider("openrouter:meta-llama/llama-4-scout")
        self.assertEqual(provider, "openrouter")
        self.assertEqual(name, "meta-llama/llama-4-scout")

    def test_ollama_local(self):
        provider, name = llm_call._split_provider("ollama:qwen2.5:3b")
        self.assertEqual(provider, "ollama")
        # Note: split(":", 1) keeps the second colon in the name.
        self.assertEqual(name, "qwen2.5:3b")


# ---------------------------------------------------------------------------
# _check_api_key — provider-aware env var lookup
# ---------------------------------------------------------------------------


class TestCheckApiKey(unittest.TestCase):
    def test_missing_anthropic_key_returns_message(self):
        with patch.dict(os.environ, {}, clear=True):
            err = llm_call._check_api_key("anthropic")
        self.assertIsNotNone(err)
        self.assertIn("ANTHROPIC_API_KEY", err)

    def test_present_anthropic_key_returns_none(self):
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "sk-test"}, clear=True):
            err = llm_call._check_api_key("anthropic")
        self.assertIsNone(err)

    def test_missing_openai_key(self):
        with patch.dict(os.environ, {}, clear=True):
            err = llm_call._check_api_key("openai")
        self.assertIsNotNone(err)
        self.assertIn("OPENAI_API_KEY", err)

    def test_ollama_needs_no_key(self):
        with patch.dict(os.environ, {}, clear=True):
            err = llm_call._check_api_key("ollama")
        self.assertIsNone(err)

    def test_unknown_provider_passes_through(self):
        # Unknown providers (e.g. custom config.toml ones) should not be
        # blocked here — let the actual call surface the real error.
        with patch.dict(os.environ, {}, clear=True):
            err = llm_call._check_api_key("custom_unknown_provider")
        self.assertIsNone(err)


# ---------------------------------------------------------------------------
# _extract_text — flatten string OR list-of-blocks content
# ---------------------------------------------------------------------------


class TestExtractText(unittest.TestCase):
    def test_string_content(self):
        resp = MockResponse(content="hello world")
        self.assertEqual(llm_call._extract_text(resp), "hello world")

    def test_anthropic_list_of_text_blocks(self):
        resp = MockResponse(content=[
            {"type": "text", "text": "hello "},
            {"type": "text", "text": "world"},
        ])
        self.assertEqual(llm_call._extract_text(resp), "hello world")

    def test_list_with_plain_text_field(self):
        resp = MockResponse(content=[{"text": "foo"}, {"text": "bar"}])
        self.assertEqual(llm_call._extract_text(resp), "foobar")

    def test_empty_content(self):
        resp = MockResponse(content="")
        self.assertEqual(llm_call._extract_text(resp), "")


# ---------------------------------------------------------------------------
# _extract_tokens — provider-shape normalization
# ---------------------------------------------------------------------------


class TestExtractTokens(unittest.TestCase):
    def test_anthropic_shape(self):
        # LangChain's normalized usage_metadata for Anthropic.
        resp = MockResponse(usage_metadata={
            "input_tokens": 1234,
            "output_tokens": 567,
            "total_tokens": 1801,
        })
        self.assertEqual(llm_call._extract_tokens(resp), (1234, 567))

    def test_openai_shape_via_usage_metadata(self):
        # When LangChain hasn't normalized (older OpenAI integration),
        # token_usage shows up in response_metadata with prompt/completion keys.
        resp = MockResponse(
            usage_metadata=None,
            response_metadata={
                "token_usage": {"prompt_tokens": 200, "completion_tokens": 80}
            },
        )
        self.assertEqual(llm_call._extract_tokens(resp), (200, 80))

    def test_google_shape(self):
        # Google Gemini uses prompt_token_count / candidates_token_count.
        resp = MockResponse(usage_metadata={
            "prompt_token_count": 50,
            "candidates_token_count": 25,
        })
        self.assertEqual(llm_call._extract_tokens(resp), (50, 25))

    def test_no_usage_returns_zeros(self):
        resp = MockResponse(content="hi")
        self.assertEqual(llm_call._extract_tokens(resp), (0, 0))


# ---------------------------------------------------------------------------
# main() — exit-code mapping + stdout/stderr discipline
# ---------------------------------------------------------------------------


class TestMainExitCodes(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="llm-call-test-")
        self.payload_path = _write_payload(self.tmpdir, {
            "system": "You are a test judge.",
            "messages": [{"role": "user", "content": "Score: 75/100"}],
            "max_tokens": 100,
        })

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_missing_api_key_exits_2(self):
        # No env vars set at all → bare anthropic name → ANTHROPIC_API_KEY
        # missing → exit 2 with clear stderr, nothing on stdout.
        exit_code, stdout, stderr = _run_main(
            ["--model", "anthropic:claude-sonnet-4-6",
             "--payload", self.payload_path],
            env={},
        )
        self.assertEqual(exit_code, llm_call.EXIT_NO_API_KEY)
        self.assertEqual(stdout, "", "stdout must be empty when key is missing")
        self.assertIn("ANTHROPIC_API_KEY", stderr)

    def test_bare_name_missing_anthropic_key_exits_2(self):
        # Bare name should default to anthropic, then trip the same check.
        exit_code, stdout, stderr = _run_main(
            ["--model", "claude-sonnet-4-6",
             "--payload", self.payload_path],
            env={},
        )
        self.assertEqual(exit_code, llm_call.EXIT_NO_API_KEY)
        self.assertIn("ANTHROPIC_API_KEY", stderr)

    def test_resolution_failure_exits_4(self):
        # Patch _resolve_model to raise — simulates unknown provider /
        # bad config.toml. Use OPENAI to bypass the API key gate first.
        with patch.object(
            llm_call, "_resolve_model",
            side_effect=ValueError("Unknown provider 'fictional'"),
        ):
            exit_code, stdout, stderr = _run_main(
                ["--model", "openai:gpt-5.2",
                 "--payload", self.payload_path],
                env={"OPENAI_API_KEY": "sk-test"},
            )
        self.assertEqual(exit_code, llm_call.EXIT_RESOLUTION_ERROR)
        self.assertEqual(stdout, "")
        self.assertIn("failed to resolve model", stderr)

    def test_provider_error_exits_3(self):
        # Resolution succeeds but the API call raises every retry.
        bad_model = MockChatModel(raises=RuntimeError("rate limited"))
        with patch.object(
            llm_call, "_resolve_model",
            return_value=(bad_model, "openai:gpt-5.2"),
        ):
            exit_code, stdout, stderr = _run_main(
                ["--model", "openai:gpt-5.2",
                 "--payload", self.payload_path,
                 "--max-attempts", "1"],
                env={"OPENAI_API_KEY": "sk-test"},
            )
        self.assertEqual(exit_code, llm_call.EXIT_PROVIDER_ERROR)
        self.assertEqual(stdout, "")
        self.assertIn("provider call failed", stderr)
        self.assertIn("rate limited", stderr)

    def test_payload_not_found_exits_1(self):
        exit_code, stdout, stderr = _run_main(
            ["--model", "anthropic:claude-sonnet-4-6",
             "--payload", "/nonexistent/path.json"],
            env={"ANTHROPIC_API_KEY": "sk-test"},
        )
        self.assertEqual(exit_code, llm_call.EXIT_GENERIC_ERROR)
        self.assertIn("payload file not found", stderr)

    def test_payload_invalid_json_exits_1(self):
        bad_path = os.path.join(self.tmpdir, "bad.json")
        with open(bad_path, "w") as f:
            f.write("{not valid json}")
        exit_code, stdout, stderr = _run_main(
            ["--model", "anthropic:claude-sonnet-4-6",
             "--payload", bad_path],
            env={"ANTHROPIC_API_KEY": "sk-test"},
        )
        self.assertEqual(exit_code, llm_call.EXIT_GENERIC_ERROR)
        self.assertIn("not valid JSON", stderr)


class TestMainSuccessPath(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="llm-call-test-")
        self.payload_path = _write_payload(self.tmpdir, {
            "system": "Be terse.",
            "messages": [{"role": "user", "content": "Reply 'ok'"}],
            "max_tokens": 64,
        })

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_anthropic_success_emits_one_json_line(self):
        good_response = MockResponse(
            content=[{"type": "text", "text": "ok"}],
            usage_metadata={
                "input_tokens": 12, "output_tokens": 1, "total_tokens": 13,
            },
        )
        good_model = MockChatModel(response=good_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(good_model, "anthropic:claude-sonnet-4-6"),
        ):
            exit_code, stdout, stderr = _run_main(
                ["--model", "anthropic:claude-sonnet-4-6",
                 "--payload", self.payload_path],
                env={"ANTHROPIC_API_KEY": "sk-test"},
            )

        self.assertEqual(exit_code, llm_call.EXIT_OK)
        # Exactly one JSON object on stdout, no extra log lines.
        stdout_lines = [l for l in stdout.split("\n") if l.strip()]
        self.assertEqual(len(stdout_lines), 1, f"stdout had {len(stdout_lines)} non-empty lines")
        result = json.loads(stdout_lines[0])
        self.assertEqual(result["text"], "ok")
        self.assertEqual(result["input_tokens"], 12)
        self.assertEqual(result["output_tokens"], 1)
        self.assertEqual(result["model"], "anthropic:claude-sonnet-4-6")
        self.assertEqual(result["provider"], "anthropic")
        # Logs went to stderr.
        self.assertIn("Resolving model", stderr)
        self.assertIn("Done in", stderr)

    def test_openai_normalizes_to_uniform_token_shape(self):
        good_response = MockResponse(
            content="ok",
            usage_metadata=None,
            response_metadata={
                "token_usage": {"prompt_tokens": 50, "completion_tokens": 25},
            },
        )
        good_model = MockChatModel(response=good_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(good_model, "openai:gpt-5.2"),
        ):
            exit_code, stdout, _ = _run_main(
                ["--model", "openai:gpt-5.2",
                 "--payload", self.payload_path],
                env={"OPENAI_API_KEY": "sk-test"},
            )

        self.assertEqual(exit_code, llm_call.EXIT_OK)
        result = json.loads([l for l in stdout.split("\n") if l.strip()][0])
        self.assertEqual(result["input_tokens"], 50)
        self.assertEqual(result["output_tokens"], 25)
        self.assertEqual(result["provider"], "openai")

    def test_google_normalizes_to_uniform_token_shape(self):
        good_response = MockResponse(
            content="ok",
            usage_metadata={
                "prompt_token_count": 100,
                "candidates_token_count": 50,
            },
        )
        good_model = MockChatModel(response=good_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(good_model, "google_genai:gemini-2.5-flash"),
        ):
            exit_code, stdout, _ = _run_main(
                ["--model", "google_genai:gemini-2.5-flash",
                 "--payload", self.payload_path],
                env={"GOOGLE_API_KEY": "sk-test"},
            )

        self.assertEqual(exit_code, llm_call.EXIT_OK)
        result = json.loads([l for l in stdout.split("\n") if l.strip()][0])
        self.assertEqual(result["input_tokens"], 100)
        self.assertEqual(result["output_tokens"], 50)
        self.assertEqual(result["provider"], "google_genai")

    def test_bare_name_resolves_to_anthropic_in_output(self):
        # Bare model strings should be normalized to provider:model in the
        # output JSON so gate.json / summary.md provenance is unambiguous.
        good_response = MockResponse(
            content="ok",
            usage_metadata={"input_tokens": 1, "output_tokens": 1},
        )
        good_model = MockChatModel(response=good_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(good_model, "anthropic:claude-sonnet-4-6"),
        ):
            exit_code, stdout, _ = _run_main(
                ["--model", "claude-sonnet-4-6",
                 "--payload", self.payload_path],
                env={"ANTHROPIC_API_KEY": "sk-test"},
            )

        self.assertEqual(exit_code, llm_call.EXIT_OK)
        result = json.loads([l for l in stdout.split("\n") if l.strip()][0])
        self.assertEqual(result["model"], "anthropic:claude-sonnet-4-6")
        self.assertEqual(result["provider"], "anthropic")

    def test_empty_response_exits_3(self):
        empty_response = MockResponse(content="")
        empty_model = MockChatModel(response=empty_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(empty_model, "anthropic:claude-sonnet-4-6"),
        ):
            exit_code, stdout, stderr = _run_main(
                ["--model", "anthropic:claude-sonnet-4-6",
                 "--payload", self.payload_path],
                env={"ANTHROPIC_API_KEY": "sk-test"},
            )

        self.assertEqual(exit_code, llm_call.EXIT_PROVIDER_ERROR)
        self.assertEqual(stdout, "")
        self.assertIn("empty response", stderr)

    def test_max_tokens_passed_to_model(self):
        good_response = MockResponse(
            content="ok",
            usage_metadata={"input_tokens": 1, "output_tokens": 1},
        )
        good_model = MockChatModel(response=good_response)

        with patch.object(
            llm_call, "_resolve_model",
            return_value=(good_model, "anthropic:claude-sonnet-4-6"),
        ):
            _run_main(
                ["--model", "anthropic:claude-sonnet-4-6",
                 "--payload", self.payload_path],
                env={"ANTHROPIC_API_KEY": "sk-test"},
            )

        # Payload had max_tokens=64; helper must forward it to ainvoke.
        self.assertIn("max_tokens", good_model.last_kwargs)
        self.assertEqual(good_model.last_kwargs["max_tokens"], 64)


# ---------------------------------------------------------------------------
# _build_messages — system + user wiring
# ---------------------------------------------------------------------------


class TestBuildMessages(unittest.TestCase):
    def test_system_then_user(self):
        msgs = llm_call._build_messages({
            "system": "be terse",
            "messages": [{"role": "user", "content": "hi"}],
        })
        # Exactly two messages, system first.
        self.assertEqual(len(msgs), 2)
        # Avoid importing langchain_core to keep test deps light — just
        # check the class names that LangChain uses.
        self.assertEqual(type(msgs[0]).__name__, "SystemMessage")
        self.assertEqual(type(msgs[1]).__name__, "HumanMessage")

    def test_no_system(self):
        msgs = llm_call._build_messages({
            "messages": [{"role": "user", "content": "hi"}],
        })
        self.assertEqual(len(msgs), 1)
        self.assertEqual(type(msgs[0]).__name__, "HumanMessage")

    def test_assistant_message_recognized(self):
        msgs = llm_call._build_messages({
            "messages": [
                {"role": "user", "content": "hi"},
                {"role": "assistant", "content": "hello"},
                {"role": "user", "content": "again"},
            ],
        })
        self.assertEqual(len(msgs), 3)
        self.assertEqual(type(msgs[0]).__name__, "HumanMessage")
        self.assertEqual(type(msgs[1]).__name__, "AIMessage")
        self.assertEqual(type(msgs[2]).__name__, "HumanMessage")


if __name__ == "__main__":
    unittest.main(verbosity=2)
