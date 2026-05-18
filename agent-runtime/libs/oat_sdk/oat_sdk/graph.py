"""OAT agents come with planning, filesystem, and subagents."""

from collections.abc import Callable, Sequence
from typing import Any

from langchain.agents import create_agent
from langchain.agents.middleware import HumanInTheLoopMiddleware, InterruptOnConfig, TodoListMiddleware
from langchain.agents.middleware.types import AgentMiddleware
from langchain.agents.structured_output import ResponseFormat
from langchain.chat_models import init_chat_model
from langchain_anthropic import ChatAnthropic
from langchain_anthropic.middleware import AnthropicPromptCachingMiddleware
from langchain_core.language_models import BaseChatModel
from langchain_core.messages import SystemMessage
from langchain_core.tools import BaseTool
from langgraph.cache.base import BaseCache
from langgraph.graph.state import CompiledStateGraph
from langgraph.store.base import BaseStore
from langgraph.types import Checkpointer

from oat_sdk.backends import StateBackend
from oat_sdk.backends.protocol import BackendFactory, BackendProtocol
from oat_sdk.middleware.filesystem import FilesystemMiddleware
from oat_sdk.middleware.memory import MemoryMiddleware
from oat_sdk.middleware.patch_tool_calls import PatchToolCallsMiddleware
from oat_sdk.middleware.skills import SkillsMiddleware
from oat_sdk.middleware.subagents import (
    GENERAL_PURPOSE_SUBAGENT,
    CompiledSubAgent,
    SubAgent,
    SubAgentMiddleware,
)
from oat_sdk.middleware.summarization import create_summarization_middleware

BASE_AGENT_PROMPT = """You are an OAT Agent, an AI assistant that helps users accomplish tasks using tools. You respond with text and tool calls. The user can see your responses and tool outputs in real time.

## Core Behavior

- Be concise and direct. Don't over-explain unless asked.
- NEVER add unnecessary preamble (\"Sure!\", \"Great question!\", \"I'll now...\").
- Don't say \"I'll now do X\" — just do it.
- If the request is ambiguous, ask questions before acting.
- If asked how to approach something, explain first, then act.

## Professional Objectivity

- Prioritize accuracy over validating the user's beliefs
- Disagree respectfully when the user is incorrect
- Avoid unnecessary superlatives, praise, or emotional validation

## Doing Tasks

When the user asks you to do something:

1. **Understand first** — read relevant files, check existing patterns. Quick but thorough — gather enough evidence to start, then iterate.
2. **Act** — implement the solution. Work quickly but accurately.
3. **Verify** — check your work against what was asked, not against your own output. Your first attempt is rarely correct — iterate.

Keep working until the task is fully complete. Don't stop partway and explain what you would do — just do it. Only yield back to the user when the task is done or you're genuinely blocked.

**When things go wrong:**
- If something fails repeatedly, stop and analyze *why* — don't keep retrying the same approach.
- If you're blocked, tell the user what's wrong and ask for guidance.

## Progress Updates

For longer tasks, provide brief progress updates at reasonable intervals — a concise sentence recapping what you've done and what's next."""  # noqa: E501


def _detect_default_model_string() -> str:
    """Detect the best available model based on environment API keys.

    Checks for provider API keys in order of preference and returns
    a provider-prefixed model string for use with init_chat_model.
    Falls back to Anthropic Claude Sonnet 4.6 if no keys are detected
    (init_chat_model will raise a clear error if the key is missing).
    """
    import os

    if os.environ.get("ANTHROPIC_API_KEY"):
        return "anthropic:claude-sonnet-4-6"
    if os.environ.get("OPENAI_API_KEY"):
        return "openai:gpt-4.1"
    if os.environ.get("GOOGLE_API_KEY"):
        return "google_genai:gemini-2.5-pro"
    # Default to Anthropic — init_chat_model will raise if key is missing
    return "anthropic:claude-sonnet-4-6"


def get_default_model() -> BaseChatModel:
    """Get the default model for deep agents.

    Auto-detects the best available provider based on environment API keys.
    Prefers Anthropic > OpenAI > Google, falling back to Anthropic if no
    keys are detected.

    Returns:
        A `BaseChatModel` instance for the detected provider.
    """
    return resolve_model(_detect_default_model_string())


def _is_anthropic_model(model: BaseChatModel) -> bool:
    """Check if a resolved model is an Anthropic model."""
    return isinstance(model, ChatAnthropic)


def resolve_model(model: str | BaseChatModel) -> BaseChatModel:
    """Resolve a model string to a `BaseChatModel` instance.

    If `model` is already a `BaseChatModel`, returns it unchanged.

    String models are resolved via `init_chat_model`, with OpenAI models
    defaulting to the Responses API. See the `create_oat_agent` docstring for
    details on how to customize this behavior.

    Args:
        model: Model name string or pre-configured model instance.

    Returns:
        Resolved `BaseChatModel` instance.
    """
    if isinstance(model, BaseChatModel):
        return model
    if model.startswith("openai:"):
        # Use Responses API by default. To use chat completions, use
        # `model=init_chat_model("openai:...")`
        # To disable data retention with the Responses API, use
        # `model=init_chat_model("openai:...", use_responses_api=True, store=False, include=["reasoning.encrypted_content"])`
        return init_chat_model(model, use_responses_api=True)
    return init_chat_model(model)


def _tool_name(tool: Any) -> str | None:
    """Return the canonical name of a tool, for excluded_tools matching.

    Tools come in three flavours in this SDK:
      - `BaseTool` instances (have `.name`)
      - bare Python callables (use `__name__`)
      - tool spec dicts from LangChain (have a `name` key)
    Returns ``None`` for shapes we cannot classify; the caller treats
    unknown shapes as "not on the deny list" rather than dropping them.
    """
    name = getattr(tool, "name", None)
    if isinstance(name, str) and name:
        return name
    if isinstance(tool, dict):
        n = tool.get("name")
        if isinstance(n, str) and n:
            return n
    fn_name = getattr(tool, "__name__", None)
    if isinstance(fn_name, str) and fn_name:
        return fn_name
    return None


def create_oat_agent(  # noqa: C901, PLR0912, PLR0915  # Complex graph assembly logic with many conditional branches
    model: str | BaseChatModel | None = None,
    tools: Sequence[BaseTool | Callable | dict[str, Any]] | None = None,
    *,
    system_prompt: str | SystemMessage | None = None,
    middleware: Sequence[AgentMiddleware] = (),
    subagents: list[SubAgent | CompiledSubAgent] | None = None,
    skills: list[str] | None = None,
    memory: list[str] | None = None,
    response_format: ResponseFormat | None = None,
    context_schema: type[Any] | None = None,
    checkpointer: Checkpointer | None = None,
    store: BaseStore | None = None,
    backend: BackendProtocol | BackendFactory | None = None,
    interrupt_on: dict[str, bool | InterruptOnConfig] | None = None,
    excluded_tools: set[str] | None = None,
    debug: bool = False,
    name: str | None = None,
    cache: BaseCache | None = None,
) -> CompiledStateGraph:
    """Create a deep agent.

    !!! warning "Deep agents require a LLM that supports tool calling!"

    By default, this agent has access to the following tools:

    - `write_todos`: manage a todo list
    - `ls`, `read_file`, `write_file`, `edit_file`, `glob`, `grep`: file operations
    - `execute`: run shell commands
    - `task`: call subagents

    The `execute` tool allows running shell commands if the backend implements `SandboxBackendProtocol`.
    For non-sandbox backends, the `execute` tool will return an error message.

    Args:
        model: The model to use.

            Defaults to `claude-sonnet-4-6`.

            Use the `provider:model` format (e.g., `openai:gpt-5`) to quickly switch between models.

            If an `openai:` model is used, the agent will use the OpenAI
            Responses API by default. To use OpenAI chat completions instead,
            initialize the model with
            `init_chat_model("openai:...", use_responses_api=False)` and pass
            the initialized model instance here. To disable data retention with
            the Responses API, use
            `init_chat_model("openai:...", use_responses_api=True, store=False, include=["reasoning.encrypted_content"])`
            and pass the initialized model instance here.
        tools: The tools the agent should have access to.

            In addition to custom tools you provide, deep agents include built-in tools for planning,
            file management, and subagent spawning.
        system_prompt: Custom system instructions to prepend before the base deep agent
            prompt.

            If a string, it's concatenated with the base prompt.
        middleware: Additional middleware to apply after the standard middleware stack
            (`TodoListMiddleware`, `FilesystemMiddleware`, `SubAgentMiddleware`,
            `SummarizationMiddleware`, `AnthropicPromptCachingMiddleware`,
            `PatchToolCallsMiddleware`).
        subagents: The subagents to use.

            Each subagent should be a `dict` with the following keys:

            - `name`
            - `description` (used by the main agent to decide whether to call the sub agent)
            - `system_prompt` (used as the system prompt in the subagent)
            - (optional) `tools`
            - (optional) `model` (either a `LanguageModelLike` instance or `dict` settings)
            - (optional) `middleware` (list of `AgentMiddleware`)
        skills: Optional list of skill source paths (e.g., `["/skills/user/", "/skills/project/"]`).

            Paths must be specified using POSIX conventions (forward slashes) and are relative
            to the backend's root. When using `StateBackend` (default), provide skill files via
            `invoke(files={...})`. With `FilesystemBackend`, skills are loaded from disk relative
            to the backend's `root_dir`. Later sources override earlier ones for skills with the
            same name (last one wins).
        memory: Optional list of memory file paths (`AGENTS.md` files) to load
            (e.g., `["/memory/AGENTS.md"]`).

            Display names are automatically derived from paths.

            Memory is loaded at agent startup and added into the system prompt.
        response_format: A structured output response format to use for the agent.
        context_schema: The schema of the deep agent.
        checkpointer: Optional `Checkpointer` for persisting agent state between runs.
        store: Optional store for persistent storage (required if backend uses `StoreBackend`).
        backend: Optional backend for file storage and execution.

            Pass either a `Backend` instance or a callable factory like `lambda rt: StateBackend(rt)`.
            For execution support, use a backend that implements `SandboxBackendProtocol`.
        interrupt_on: Mapping of tool names to interrupt configs.

            Pass to pause agent execution at specified tool calls for human approval or modification.

            Example: `interrupt_on={"edit_file": True}` pauses before every edit.
        excluded_tools: Optional set of tool names to remove from the agent's catalog.

            Filters the `tools` parameter by name, and additionally gates:

            - ``"task"`` skips `SubAgentMiddleware` and the synthesized
              `general_purpose_spec`, removing the subagent-delegation
              tool entirely. (Mirrors the upstream
              `GeneralPurposeSubagentProfile(enabled=False)` knob that
              the oat_sdk fork dropped.)

            Other names match against the `tools` list only (a bare
            callable's ``__name__``, a `BaseTool`'s ``.name``, or a tool
            spec dict's ``"name"`` key). The CLI-level
            ``compact_conversation`` exclusion is handled in
            ``oat_cli/agent.py`` because that's where
            `SummarizationToolMiddleware` (which exposes the tool to the
            LLM) is added — this SDK function only adds the *automatic*
            summarizer, which stays enabled regardless.

            Used by the OAT daemon's browser-agent and assistant spawn
            paths to strip tools that don't make sense for those agent
            types; defense in depth alongside prompt-level guards.
        debug: Whether to enable debug mode. Passed through to `create_agent`.
        name: The name of the agent. Passed through to `create_agent`.
        cache: The cache to use for the agent. Passed through to `create_agent`.

    Returns:
        A configured deep agent.
    """
    model = get_default_model() if model is None else resolve_model(model)

    backend = backend if backend is not None else (StateBackend)

    # Normalize the deny set once. Empty set and None are equivalent;
    # the rest of the function reads `excluded` directly. Keeping a
    # local var (rather than mutating the argument) makes the gating
    # branches readable and the function side-effect-free on the input.
    #
    # NOTE: only `task` is gated at this layer. `compact_conversation`
    # gating happens in `oat_cli/agent.py` because the tool that
    # exposes `compact_conversation` to the LLM is
    # `SummarizationToolMiddleware`, which is added by the CLI — not
    # by this SDK function. `create_summarization_middleware` here
    # adds the *automatic* summarizer (no tool surface) and stays
    # enabled regardless of `excluded_tools` so context limits still
    # get managed in long sessions.
    excluded: frozenset[str] = (
        frozenset(excluded_tools) if excluded_tools else frozenset()
    )
    task_excluded: bool = "task" in excluded

    # Filter user-provided tools by name. We retain anything whose
    # name we cannot determine (defensive: an unknown shape is more
    # likely a third-party tool we should not silently drop than a
    # smuggled `task`/`compact_conversation` we should).
    filtered_tools: list[BaseTool | Callable | dict[str, Any]] = []
    if tools:
        for t in tools:
            n = _tool_name(t)
            if n is None or n not in excluded:
                filtered_tools.append(t)

    # Build general-purpose subagent with default middleware stack.
    # Skipped entirely when `task` is excluded: the GP subagent only
    # exists so the SubAgentMiddleware (also gated below) has something
    # to dispatch to.
    general_purpose_spec: SubAgent | None = None
    if not task_excluded:
        gp_middleware: list[AgentMiddleware[Any, Any, Any]] = [
            TodoListMiddleware(),
            FilesystemMiddleware(backend=backend),
            create_summarization_middleware(model, backend),
        ]
        # Only add Anthropic prompt caching middleware for Anthropic models
        if _is_anthropic_model(model):
            gp_middleware.append(AnthropicPromptCachingMiddleware(unsupported_model_behavior="ignore"))
        gp_middleware.append(PatchToolCallsMiddleware())
        if skills is not None:
            gp_middleware.append(SkillsMiddleware(backend=backend, sources=skills))
        if interrupt_on is not None:
            gp_middleware.append(HumanInTheLoopMiddleware(interrupt_on=interrupt_on))

        general_purpose_spec = {  # ty: ignore[missing-typed-dict-key]
            **GENERAL_PURPOSE_SUBAGENT,
            "model": model,
            "tools": filtered_tools,
            "middleware": gp_middleware,
        }

    # Process user-provided subagents to fill in defaults for model, tools, and middleware
    processed_subagents: list[SubAgent | CompiledSubAgent] = []
    for spec in subagents or []:
        if "runnable" in spec:
            # CompiledSubAgent - use as-is
            processed_subagents.append(spec)
        else:
            # SubAgent - fill in defaults and prepend base middleware
            subagent_model = spec.get("model", model)
            subagent_model = resolve_model(subagent_model)

            # Build middleware: base stack + skills (if specified) + user's middleware.
            subagent_middleware: list[AgentMiddleware[Any, Any, Any]] = [
                TodoListMiddleware(),
                FilesystemMiddleware(backend=backend),
                create_summarization_middleware(subagent_model, backend),
            ]
            if _is_anthropic_model(subagent_model):
                subagent_middleware.append(AnthropicPromptCachingMiddleware(unsupported_model_behavior="ignore"))
            subagent_middleware.append(PatchToolCallsMiddleware())
            subagent_skills = spec.get("skills")
            if subagent_skills:
                subagent_middleware.append(SkillsMiddleware(backend=backend, sources=subagent_skills))
            subagent_middleware.extend(spec.get("middleware", []))

            # Filter the per-subagent tools list too (each spec can
            # override `tools` independently of the main `tools` arg).
            spec_tools_raw = spec.get("tools", filtered_tools)
            spec_tools: list[BaseTool | Callable | dict[str, Any]] = []
            for t in spec_tools_raw or []:
                n = _tool_name(t)
                if n is None or n not in excluded:
                    spec_tools.append(t)

            processed_spec: SubAgent = {  # ty: ignore[missing-typed-dict-key]
                **spec,
                "model": subagent_model,
                "tools": spec_tools,
                "middleware": subagent_middleware,
            }
            processed_subagents.append(processed_spec)

    # Combine GP (if present) with processed user-provided subagents.
    all_subagents: list[SubAgent | CompiledSubAgent] = (
        [general_purpose_spec, *processed_subagents]
        if general_purpose_spec is not None
        else list(processed_subagents)
    )

    # Build main agent middleware stack. `SubAgentMiddleware` is the
    # source of the `task` tool, so we skip it entirely when `task`
    # is excluded — otherwise it would still register `task` even
    # with an empty subagent list.
    oat_sdk_middleware: list[AgentMiddleware[Any, Any, Any]] = [
        TodoListMiddleware(),
    ]
    if memory is not None:
        oat_sdk_middleware.append(MemoryMiddleware(backend=backend, sources=memory))
    if skills is not None:
        oat_sdk_middleware.append(SkillsMiddleware(backend=backend, sources=skills))
    main_stack: list[AgentMiddleware[Any, Any, Any]] = [
        FilesystemMiddleware(backend=backend),
    ]
    if not task_excluded:
        main_stack.append(
            SubAgentMiddleware(
                backend=backend,
                subagents=all_subagents,
            )
        )
    main_stack.append(create_summarization_middleware(model, backend))
    if _is_anthropic_model(model):
        main_stack.append(AnthropicPromptCachingMiddleware(unsupported_model_behavior="ignore"))
    main_stack.append(PatchToolCallsMiddleware())
    oat_sdk_middleware.extend(main_stack)

    if middleware:
        oat_sdk_middleware.extend(middleware)
    if interrupt_on is not None:
        oat_sdk_middleware.append(HumanInTheLoopMiddleware(interrupt_on=interrupt_on))

    # Combine system_prompt with BASE_AGENT_PROMPT
    if system_prompt is None:
        final_system_prompt: str | SystemMessage = BASE_AGENT_PROMPT
    elif isinstance(system_prompt, SystemMessage):
        final_system_prompt = SystemMessage(content_blocks=[*system_prompt.content_blocks, {"type": "text", "text": f"\n\n{BASE_AGENT_PROMPT}"}])
    else:
        # String: simple concatenation
        final_system_prompt = system_prompt + "\n\n" + BASE_AGENT_PROMPT

    return create_agent(
        model,
        system_prompt=final_system_prompt,
        tools=filtered_tools,
        middleware=oat_sdk_middleware,
        response_format=response_format,
        context_schema=context_schema,
        checkpointer=checkpointer,
        store=store,
        debug=debug,
        name=name,
        cache=cache,
    ).with_config({"recursion_limit": 1000})
