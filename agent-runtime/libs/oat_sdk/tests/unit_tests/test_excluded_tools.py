"""Tests for the ``excluded_tools`` kwarg added to ``create_oat_agent``.

Background: the upstream LangChain ``deepagents`` package has a
``HarnessProfile.excluded_tools`` + ``GeneralPurposeSubagentProfile(enabled=False)``
mechanism for stripping baked-in tools from an agent's catalog. The
``oat_sdk`` fork dropped those knobs, so by default every agent gets
``task`` (subagent delegation) and ``compact_conversation`` (manual
context compaction) whether they need them or not.

The OAT daemon's ``browser-agent`` and (future) ``assistant`` agent
types neither need nor benefit from those tools — they were observed
in May 2026 testing to be a prompt-injection vector (the browser-agent
agent delegated work to a subagent via ``task`` and got wedged on a
``CDP_TIMEOUT``). ``excluded_tools`` is the runtime guard rail.

These tests intercept the call to ``langchain.agents.create_agent``
(the underlying graph builder ``create_oat_agent`` delegates to) so
we can inspect the assembled tool list and middleware stack without
needing a live LLM.
"""
from __future__ import annotations

from typing import Any
from unittest.mock import MagicMock, patch

from langchain_core.tools import tool

from oat_sdk.graph import create_oat_agent
from oat_sdk.middleware.subagents import SubAgentMiddleware


def _capture_create_agent_kwargs() -> tuple[dict[str, Any], Any, Any]:
    """Patch ``langchain.agents.create_agent`` and return a tuple of
    ``(captured, fake_create_agent, sentinel)``.

    The sentinel is a ``MagicMock`` so ``create_oat_agent``'s trailing
    ``.with_config({"recursion_limit": 1000})`` chain works on the
    returned value (otherwise the test would fail with
    ``AttributeError: 'object' object has no attribute 'with_config'``).
    Tests assert against ``captured["kwargs"]`` to introspect the
    tool/middleware decisions without compiling a real graph.
    """
    captured: dict[str, Any] = {}
    sentinel = MagicMock(name="create_agent_return")

    def fake_create_agent(*args: Any, **kwargs: Any) -> Any:
        # ``create_agent`` is invoked with `model` as positional arg 0,
        # rest as kwargs (see graph.py near the bottom of ``create_oat_agent``).
        captured["args"] = args
        captured["kwargs"] = kwargs
        return sentinel

    return captured, fake_create_agent, sentinel


@tool
def safe_user_tool(x: int) -> int:
    """A harmless user-supplied tool that should never be filtered."""
    return x + 1


@tool
def http_request_lookalike(url: str) -> str:
    """A user tool whose name happens to match the built-in http_request.

    Excluding ``http_request`` should still drop this tool because the
    filter is name-based. (The daemon's deny list for browser-agents
    actually targets the SDK's http_request — but a name collision in
    user-supplied tools is treated identically; safer to over-strip
    than to let a smuggled tool through.)
    """
    return url


@tool
def task(prompt: str) -> str:  # noqa: D401 — minimal placeholder, deliberate name
    """A user-supplied tool literally named ``task`` (also the baked-in subagent tool).

    When ``"task" in excluded_tools`` both the SDK's `SubAgentMiddleware`
    AND this user tool should be removed.
    """
    return prompt


class TestExcludedToolsFilteringUserTools:
    """``excluded_tools`` filters the user-supplied ``tools=`` list by name."""

    def test_no_exclusions_keeps_all_tools(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool, http_request_lookalike],
            )
        passed_tools = captured["kwargs"]["tools"]
        names = {t.name for t in passed_tools}
        assert names == {"safe_user_tool", "http_request_lookalike"}

    def test_excludes_named_tool(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool, http_request_lookalike],
                excluded_tools={"http_request_lookalike"},
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}

    def test_unknown_excluded_name_is_a_noop(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool],
                excluded_tools={"nonexistent_tool_name"},
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}

    def test_empty_set_is_a_noop(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool],
                excluded_tools=set(),
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}

    def test_none_is_a_noop(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool],
                excluded_tools=None,
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}


class TestExcludedToolsGatesSubAgentMiddleware:
    """``"task" in excluded_tools`` strips the subagent middleware + tool entirely."""

    def test_task_default_keeps_subagent_middleware(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(model="anthropic:claude-sonnet-4-6")
        middleware = captured["kwargs"]["middleware"]
        # SubAgentMiddleware is what registers the `task` tool; default
        # behaviour (no exclusion) must keep it in the stack.
        assert any(isinstance(m, SubAgentMiddleware) for m in middleware)

    def test_task_excluded_drops_subagent_middleware(self) -> None:
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                excluded_tools={"task"},
            )
        middleware = captured["kwargs"]["middleware"]
        # Without SubAgentMiddleware, the `task` tool simply does not
        # exist in the runtime catalog. Mirrors upstream's
        # `GeneralPurposeSubagentProfile(enabled=False)` semantics.
        assert not any(isinstance(m, SubAgentMiddleware) for m in middleware)

    def test_task_excluded_also_filters_user_task_named_tool(self) -> None:
        # User tools with the same name should be dropped too — otherwise
        # excluding "task" would still leak a tool by that name into
        # the catalog and confuse models that hallucinate-call by name.
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[task, safe_user_tool],
                excluded_tools={"task"},
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}


class TestExcludedToolsCombined:
    """The browser-agent / assistant production case excludes four names."""

    def test_browser_agent_deny_list_strips_all_four_at_sdk_layer(self) -> None:
        # At the SDK layer the four-tool browser-agent deny list affects:
        #   * `task` -> SubAgentMiddleware skipped
        #   * `http_request` / `fetch_url` / `compact_conversation` ->
        #     filtered out of the user-provided `tools=` list (but the
        #     SDK doesn't add any of these as built-ins; the CLI does
        #     for the first two, and SummarizationToolMiddleware in the
        #     CLI adds `compact_conversation`).
        # So at this layer we only assert the SubAgent + tool-filter
        # behaviour. The CLI-layer test covers the additional filtering
        # of the CLI's built-in `http_request` / `fetch_url` / the
        # `SummarizationToolMiddleware` add at oat_cli/agent.py:602.
        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool, task, http_request_lookalike],
                excluded_tools={
                    "task",
                    "http_request",
                    "fetch_url",
                    "compact_conversation",
                },
            )
        middleware = captured["kwargs"]["middleware"]
        assert not any(isinstance(m, SubAgentMiddleware) for m in middleware)
        # Automatic summarization stays (no `SummarizationToolMiddleware`
        # at this layer, only the auto-summarizer). The CLI test
        # asserts the tool-exposure middleware is gated correctly.
        names = {t.name for t in captured["kwargs"]["tools"]}
        # `task` dropped; `http_request_lookalike` survives (its name
        # does not match the deny set); `safe_user_tool` survives.
        assert names == {"safe_user_tool", "http_request_lookalike"}

    def test_browser_agent_deny_list_filters_user_tool_by_canonical_name(self) -> None:
        # Defense in depth: if a user (or a hostile MCP server) supplied
        # a tool literally named `http_request` or `fetch_url`, exclusion
        # should drop it too.
        @tool
        def http_request(url: str) -> str:  # noqa: D401
            """Renamed to literally match the SDK built-in name."""
            return url

        @tool
        def fetch_url(url: str) -> str:  # noqa: D401
            """Same, for fetch_url."""
            return url

        captured, fake, _ = _capture_create_agent_kwargs()
        with patch("oat_sdk.graph.create_agent", fake):
            create_oat_agent(
                model="anthropic:claude-sonnet-4-6",
                tools=[safe_user_tool, http_request, fetch_url],
                excluded_tools={"http_request", "fetch_url"},
            )
        names = {t.name for t in captured["kwargs"]["tools"]}
        assert names == {"safe_user_tool"}
