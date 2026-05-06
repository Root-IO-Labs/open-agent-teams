"""Textual UI adapter for agent execution."""
# This module has complex streaming logic ported from execution.py

from __future__ import annotations

import asyncio
import json
import logging
import re
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from collections.abc import Awaitable, Callable

from langchain.agents.middleware.human_in_the_loop import (
    ApproveDecision,
    EditDecision,
    HITLRequest,
    HITLResponse,
    RejectDecision,
)
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from langgraph.types import Command, Interrupt
from pydantic import TypeAdapter, ValidationError

from oat_cli import sidecar_emitter
from oat_cli.file_ops import FileOpTracker
from oat_cli.image_utils import create_multimodal_content
from oat_cli.input import ImageTracker, parse_file_mentions
from oat_cli.tool_display import format_tool_message_content
from oat_cli.widgets.messages import (
    AppMessage,
    AssistantMessage,
    DiffMessage,
    SummarizationMessage,
    ToolCallMessage,
)

logger = logging.getLogger(__name__)

# Type alias matching HITLResponse["decisions"] element type
HITLDecision = ApproveDecision | EditDecision | RejectDecision

_HITL_REQUEST_ADAPTER = TypeAdapter(HITLRequest)


def _build_stream_config(
    thread_id: str,
    assistant_id: str | None,
) -> dict[str, Any]:
    """Build the LangGraph stream config dict.

    The `thread_id` in `configurable` is automatically propagated as run
    metadata by LangGraph, so it can be used for LangSmith filtering without
    a separate metadata key.

    Args:
        thread_id: The CLI session thread identifier.
        assistant_id: The agent/assistant identifier, if any.

    Returns:
        Config dict with `configurable` and `metadata` keys.
    """
    metadata: dict[str, str] = {}
    if assistant_id:
        metadata.update(
            {
                "assistant_id": assistant_id,
                "agent_name": assistant_id,
                "updated_at": datetime.now(UTC).isoformat(),
            }
        )
    return {
        "configurable": {"thread_id": thread_id},
        "metadata": metadata,
    }


def _is_summarization_chunk(metadata: dict | None) -> bool:
    """Check if a message chunk is from summarization middleware.

    The summarization model is invoked with
    `config={"metadata": {"lc_source": "summarization"}}`
    (see `langchain.agents.middleware.summarization`), which
    LangChain's callback system merges into the stream metadata dict.

    Args:
        metadata: The metadata dict from the stream chunk.

    Returns:
        Whether the chunk is from summarization and should be filtered.
    """
    if metadata is None:
        return False
    return metadata.get("lc_source") == "summarization"


class TextualUIAdapter:
    """Adapter for rendering agent output to Textual widgets.

    This adapter provides an abstraction layer between the agent execution and the
    Textual UI, allowing streaming output to be rendered as widgets.
    """

    _mount_message: Callable[..., Awaitable[None]]
    """Async callback to mount a message widget to the chat."""

    _update_status: Callable[[str], None]
    """Callback to update the status bar text."""

    _request_approval: Callable[..., Awaitable[Any]]
    """Async callback that returns a Future for HITL approval."""

    _on_auto_approve_enabled: Callable[[], None] | None
    """Callback invoked when auto-approve is enabled via the HITL approval menu.

    Fired when the user selects "Auto-approve all" from an approval dialog,
    allowing the app to sync its status bar and session state.
    """

    _scroll_to_bottom: Callable[[], None] | None
    """Callback to scroll chat to bottom."""

    _set_spinner: Callable[[str | None], Awaitable[None]] | None
    """Callback to show/hide loading spinner.

    Pass `None` to hide, or a status string to show.
    """

    _set_active_message: Callable[[str | None], None] | None
    """Callback to set the active streaming message ID (pass `None` to clear)."""

    _sync_message_content: Callable[[str, str], None] | None
    """Callback to sync final message content back to the store after streaming."""

    _current_tool_messages: dict[str, ToolCallMessage]
    """Map of tool call IDs to their message widgets."""

    _token_tracker: Any
    """Path A: context window tracker (overwrite semantics)."""

    _spend_tracker: Any
    """Path B: cumulative spend tracker (accumulate semantics)."""

    def __init__(
        self,
        mount_message: Callable[..., Awaitable[None]],
        update_status: Callable[[str], None],
        request_approval: Callable[..., Awaitable[Any]],
        on_auto_approve_enabled: Callable[[], None] | None = None,
        scroll_to_bottom: Callable[[], None] | None = None,
        set_spinner: Callable[[str | None], Awaitable[None]] | None = None,
        set_active_message: Callable[[str | None], None] | None = None,
        sync_message_content: Callable[[str, str], None] | None = None,
    ) -> None:
        """Initialize the adapter.

        Args:
            mount_message: Async callable to mount a message widget.
            update_status: Callable to update the status bar message.
            request_approval: Async callable that returns a Future for HITL approval.
            on_auto_approve_enabled: Callback fired when the user selects
                "Auto-approve all" from an approval dialog.

                Used by the app to sync the status bar indicator and session state.
            scroll_to_bottom: Callback to scroll chat to bottom.
            set_spinner: Callback to show/hide loading spinner (pass `None` to hide).
            set_active_message: Callback to set the active streaming message ID.
            sync_message_content: Callback to sync final content back to the
                message store after streaming completes.
        """
        self._mount_message = mount_message
        self._update_status = update_status
        self._request_approval = request_approval
        self._on_auto_approve_enabled = on_auto_approve_enabled
        self._scroll_to_bottom = scroll_to_bottom
        self._set_spinner = set_spinner
        self._set_active_message = set_active_message
        self._sync_message_content = sync_message_content

        # State tracking
        self._current_tool_messages: dict[str, ToolCallMessage] = {}
        self._token_tracker: Any = None
        self._spend_tracker: Any = None

    def set_token_tracker(self, tracker: Any) -> None:  # noqa: ANN401  # Dynamic tracker type from Textual
        """Set the context window tracker (Path A)."""
        self._token_tracker = tracker

    def set_spend_tracker(self, tracker: Any) -> None:  # noqa: ANN401  # Dynamic tracker type from Textual
        """Set the cumulative spend tracker (Path B)."""
        self._spend_tracker = tracker

    def finalize_pending_tools_with_error(self, error: str) -> None:
        """Mark all pending/running tool widgets as error and clear tracking.

        This is used as a safety net when an unexpected exception aborts
        streaming before matching `ToolMessage` results are received.

        Args:
            error: Error text to display in each pending tool widget.
        """
        for tool_msg in list(self._current_tool_messages.values()):
            tool_msg.set_error(error)
        self._current_tool_messages.clear()

        # Clear active streaming message to avoid stale "active" state in the store.
        if self._set_active_message:
            self._set_active_message(None)


def _build_interrupted_ai_message(
    pending_text_by_namespace: dict[tuple, str],
    current_tool_messages: dict[str, Any],
) -> AIMessage | None:
    """Build an AIMessage capturing interrupted state (text + tool calls).

    Args:
        pending_text_by_namespace: Dict of accumulated text by namespace
        current_tool_messages: Dict of tool_id -> ToolCallMessage widget

    Returns:
        AIMessage with accumulated content and tool calls, or None if empty.
    """
    main_ns_key = ()
    accumulated_text = pending_text_by_namespace.get(main_ns_key, "").strip()

    # Reconstruct tool_calls from displayed tool messages
    tool_calls = []
    for tool_id, tool_widget in list(current_tool_messages.items()):
        tool_calls.append(
            {
                "id": tool_id,
                "name": tool_widget._tool_name,
                "args": tool_widget._args,
            }
        )

    if not accumulated_text and not tool_calls:
        return None

    return AIMessage(
        content=accumulated_text,
        tool_calls=tool_calls or [],
    )


_ANSI_RE = re.compile(r"\x1b\[[0-9;]*[a-zA-Z]")


class ConversationLogger:
    """Writes a human-readable conversation log with full tool call/result content.

    Produces output suitable for LLM-assisted analysis of agent runs. ANSI
    escape codes are stripped; the format uses timestamps and labeled sections.
    """

    def __init__(self, path: str | Path) -> None:
        self._path = Path(path)
        self._path.parent.mkdir(parents=True, exist_ok=True)
        self._file = self._path.open("a", encoding="utf-8")

    def _ts(self) -> str:
        return datetime.now(UTC).strftime("%H:%M:%S")

    def _strip_ansi(self, text: str) -> str:
        return _ANSI_RE.sub("", text)

    def log_user(self, text: str) -> None:
        text = self._strip_ansi(text)
        self._file.write(f"[{self._ts()}] USER:\n")
        for line in text.splitlines():
            self._file.write(f"  {line}\n")
        self._file.write("\n")
        self._file.flush()

    def log_assistant(self, text: str) -> None:
        text = self._strip_ansi(text)
        self._file.write(f"[{self._ts()}] ASSISTANT:\n")
        for line in text.splitlines():
            self._file.write(f"  {line}\n")
        self._file.write("\n")
        self._file.flush()

    def log_tool_call(self, name: str, args: dict[str, Any]) -> None:
        self._file.write(f"[{self._ts()}] TOOL: {name}\n")
        for k, v in args.items():
            v_str = self._strip_ansi(str(v))
            self._file.write(f"  {k}: {v_str}\n")
        self._file.write("\n")
        self._file.flush()

    def log_tool_result(self, name: str, content: str, status: str = "success") -> None:
        content = self._strip_ansi(content)
        label = f"RESULT: {name}"
        if status != "success":
            label += f" ({status})"
        self._file.write(f"[{self._ts()}] {label}\n")
        for line in content.splitlines():
            self._file.write(f"  {line}\n")
        self._file.write("\n")
        self._file.flush()

    def close(self) -> None:
        self._file.close()


async def execute_task_textual(
    user_input: str,
    agent: Any,  # noqa: ANN401  # Dynamic agent graph type
    assistant_id: str | None,
    session_state: Any,  # noqa: ANN401  # Dynamic session state type
    adapter: TextualUIAdapter,
    backend: Any = None,  # noqa: ANN401  # Dynamic backend type
    image_tracker: ImageTracker | None = None,
    conversation_log_path: str | None = None,
) -> None:
    """Execute a task with output directed to Textual UI.

    This is the Textual-compatible version of execute_task() that uses
    the TextualUIAdapter for all UI operations.

    Args:
        user_input: The user's input message
        agent: The LangGraph agent to execute
        assistant_id: The agent identifier
        session_state: Session state with auto_approve flag
        adapter: The TextualUIAdapter for UI operations
        backend: Optional backend for file operations
        image_tracker: Optional tracker for images
        conversation_log_path: Optional path for full-content conversation log.
            When set, writes a human-readable log with full tool call/result
            content (what the LLM sees, not the 4-line UI preview).

    Raises:
        ValidationError: If HITL request validation fails (re-raised).
    """
    # Parse file mentions and inject content if any
    prompt_text, mentioned_files = parse_file_mentions(user_input)

    # Max file size to embed inline (256KB, matching mistral-vibe)
    # Larger files get a reference instead - use read_file tool to view them
    max_embed_bytes = 256 * 1024

    if mentioned_files:
        context_parts = [prompt_text, "\n\n## Referenced Files\n"]
        for file_path in mentioned_files:
            try:
                file_size = file_path.stat().st_size
                if file_size > max_embed_bytes:
                    # File too large - include reference instead of content
                    size_kb = file_size // 1024
                    context_parts.append(
                        f"\n### {file_path.name}\n"
                        f"Path: `{file_path}`\n"
                        f"Size: {size_kb}KB (too large to embed, "
                        "use read_file tool to view)"
                    )
                else:
                    content = file_path.read_text()
                    context_parts.append(
                        f"\n### {file_path.name}\n"
                        f"Path: `{file_path}`\n```\n{content}\n```"
                    )
            except Exception as e:  # noqa: BLE001  # Resilient adapter error handling
                context_parts.append(
                    f"\n### {file_path.name}\n[Error reading file: {e}]"
                )
        final_input = "\n".join(context_parts)
    else:
        final_input = prompt_text

    # Include images in the message content
    images_to_send = []
    if image_tracker:
        images_to_send = image_tracker.get_images()
    if images_to_send:
        message_content = create_multimodal_content(final_input, images_to_send)
    else:
        message_content = final_input

    thread_id = session_state.thread_id
    config = _build_stream_config(thread_id, assistant_id)

    # Path A capture: latest main-agent context window (overwrite per turn)
    latest_main_context_input = 0
    latest_main_context_output = 0

    # Path B capture: all spend this request, including summarization (accumulate)
    spend_input_delta = 0
    spend_output_delta = 0
    # Prompt-cache metrics (Anthropic input_token_details). Stay 0 for
    # providers that don't report caching.
    spend_cache_read_delta = 0
    spend_cache_creation_delta = 0

    # Show spinner
    if adapter._set_spinner:
        await adapter._set_spinner("Thinking")

    # Hide token display during streaming (will be shown with accurate count at end)
    if adapter._token_tracker:
        adapter._token_tracker.hide()

    conv_log: ConversationLogger | None = None
    if conversation_log_path:
        conv_log = ConversationLogger(conversation_log_path)
        conv_log.log_user(user_input)

    # Open the sidecar turn. emit_turn_start sets the process-level
    # turn_id that every subsequent emit_* call in this invocation
    # inherits, so the Go side can correlate assistant/tool events back
    # to this specific user message. Unconditional — sidecar is independent
    # of conv_log so it also fires when conversation logging is disabled.
    sidecar_emitter.emit_turn_start(user_input)

    _emit_oat_model()

    file_op_tracker = FileOpTracker(assistant_id=assistant_id, backend=backend)
    displayed_tool_ids: set[str] = set()
    tool_call_buffers: dict[str | int, dict] = {}

    # Track pending text and assistant messages PER NAMESPACE to avoid interleaving
    # when multiple subagents stream in parallel
    pending_text_by_namespace: dict[tuple, str] = {}
    assistant_message_by_namespace: dict[tuple, Any] = {}

    # Clear images from tracker after creating the message
    if image_tracker:
        image_tracker.clear()

    stream_input: dict | Command = {
        "messages": [{"role": "user", "content": message_content}]
    }

    # Track summarization lifecycle so spinner status and notification stay in sync.
    summarization_in_progress = False

    try:
        while True:
            interrupt_occurred = False
            hitl_response: dict[str, HITLResponse] = {}
            suppress_resumed_output = False
            pending_interrupts: dict[str, HITLRequest] = {}

            async for chunk in agent.astream(
                stream_input,
                stream_mode=["messages", "updates"],
                subgraphs=True,
                config=config,
                durability="exit",
            ):
                if not isinstance(chunk, tuple) or len(chunk) != 3:  # noqa: PLR2004  # Retry count threshold
                    continue

                namespace, current_stream_mode, data = chunk

                # Convert namespace to hashable tuple for dict keys
                ns_key = tuple(namespace) if namespace else ()

                # Filter out subagent outputs - only show main agent (empty
                # namespace). Subagents run via Task tool and should only
                # report back to the main agent
                is_main_agent = ns_key == ()

                # Handle UPDATES stream - for interrupts and todos
                if current_stream_mode == "updates":
                    if not isinstance(data, dict):
                        continue

                    # Check for interrupts
                    if "__interrupt__" in data:
                        interrupts: list[Interrupt] = data["__interrupt__"]
                        if interrupts:
                            for interrupt_obj in interrupts:
                                try:
                                    validated_request = (
                                        _HITL_REQUEST_ADAPTER.validate_python(
                                            interrupt_obj.value
                                        )
                                    )
                                    pending_interrupts[interrupt_obj.id] = (
                                        validated_request
                                    )
                                    interrupt_occurred = True
                                except ValidationError:  # noqa: TRY203  # Re-raise preserves exception context in handler
                                    raise

                    # Check for todo updates (not yet implemented in Textual UI)
                    chunk_data = next(iter(data.values())) if data else None
                    if (
                        chunk_data
                        and isinstance(chunk_data, dict)
                        and "todos" in chunk_data
                    ):
                        pass  # Future: render todo list widget

                # Handle MESSAGES stream - for content and tool calls
                elif current_stream_mode == "messages":
                    # Skip subagent outputs - only render main agent content in chat
                    if not is_main_agent:
                        continue

                    if not isinstance(data, tuple) or len(data) != 2:  # noqa: PLR2004  # Tool call part index
                        continue

                    message, metadata = data

                    # --- Token usage extraction (BEFORE summarization skip) ---
                    # Extract from ALL usage-bearing chunks so Path B
                    # (cumulative spend) counts summarization, interrupted,
                    # and failed work.  Path A (context window) is only
                    # updated from main non-summarization turns.
                    #
                    # LangChain guarantees incremental per-chunk delivery:
                    # Anthropic/OpenAI send usage on a single final chunk;
                    # Google sends incremental per-chunk (after internal
                    # subtract_usage conversion).  += is correct for all.
                    if hasattr(message, "usage_metadata") and message.usage_metadata:
                        usage = message.usage_metadata
                        chunk_input = 0
                        chunk_output = 0
                        if "input_tokens" in usage:
                            chunk_input = int(usage["input_tokens"])
                        if "output_tokens" in usage:
                            chunk_output = int(usage["output_tokens"])

                        # Path B: accumulate ALL spend
                        spend_input_delta += chunk_input
                        spend_output_delta += chunk_output

                        # Cache metrics (Anthropic, some OpenAI paths).
                        # Other providers don't populate input_token_details
                        # — fields stay 0.
                        details = usage.get("input_token_details") or {}
                        if isinstance(details, dict):
                            spend_cache_read_delta += int(
                                details.get("cache_read", 0) or 0
                            )
                            spend_cache_creation_delta += int(
                                details.get("cache_creation", 0) or 0
                            )

                        # Path A: overwrite from main non-summarization only
                        if not _is_summarization_chunk(metadata):
                            latest_main_context_input = chunk_input
                            latest_main_context_output = chunk_output

                        # Diagnostic: warn if a single chunk carries values
                        # suggesting cumulative (not incremental) delivery.
                        # This would mean a provider is violating LangChain's
                        # per-chunk contract.
                        if (
                            chunk_input > (spend_input_delta - chunk_input) * 4
                            and (spend_input_delta - chunk_input) > 0
                        ):
                            logger.warning(
                                "usage_metadata may be cumulative, not incremental "
                                "— token spend for this turn may be overcounted "
                                "(chunk_input=%d, prior_spend=%d)",
                                chunk_input,
                                spend_input_delta - chunk_input,
                            )

                    # Filter out summarization model output, but keep UI feedback.
                    # The summarization model streams AIMessage chunks tagged
                    # with lc_source="summarization" in the callback metadata.
                    # These are hidden from the user; only the spinner and a
                    # notification widget provide feedback.
                    if _is_summarization_chunk(metadata):
                        if not summarization_in_progress:
                            summarization_in_progress = True
                            if adapter._set_spinner:
                                await adapter._set_spinner("Summarizing")
                        continue

                    # Regular (non-summarization) chunks resumed — summarization
                    # has finished. Mount the notification and reset the spinner.
                    if summarization_in_progress:
                        summarization_in_progress = False
                        try:
                            await adapter._mount_message(SummarizationMessage())
                        except Exception:
                            logger.debug(
                                "Failed to mount summarization notification",
                                exc_info=True,
                            )
                        if adapter._set_spinner:
                            await adapter._set_spinner("Thinking")

                    if isinstance(message, HumanMessage):
                        content = message.text
                        # Flush pending text for this namespace
                        pending_text = pending_text_by_namespace.get(ns_key, "")
                        if content and pending_text:
                            await _flush_assistant_text_ns(
                                adapter,
                                pending_text,
                                ns_key,
                                assistant_message_by_namespace,
                            )
                            pending_text_by_namespace[ns_key] = ""
                        continue

                    if isinstance(message, ToolMessage):
                        tool_name = getattr(message, "name", "")
                        tool_status = getattr(message, "status", "success")
                        tool_content = format_tool_message_content(message.content)
                        record = file_op_tracker.complete_with_message(message)
                        # Used by both the conv_log path and the sidecar emit
                        # below — hoisted out of the `if conv_log:` block so
                        # the sidecar still fires when conversation logging
                        # is disabled.
                        tool_content_str = str(tool_content) if tool_content else ""
                        sidecar_call_id = getattr(message, "tool_call_id", "") or ""

                        if conv_log:
                            conv_log.log_tool_result(
                                tool_name, tool_content_str, tool_status,
                            )
                        sidecar_emitter.emit_tool_result(
                            call_id=sidecar_call_id,
                            content=tool_content_str,
                            error=None if tool_status == "success" else tool_status,
                        )

                        # Reshow spinner after tool result
                        if adapter._set_spinner:
                            await adapter._set_spinner("Thinking")

                        # Update tool call status with output
                        tool_id = getattr(message, "tool_call_id", None)
                        if tool_id and tool_id in adapter._current_tool_messages:
                            tool_msg = adapter._current_tool_messages[tool_id]
                            output_str = str(tool_content) if tool_content else ""
                            if tool_status == "success":
                                tool_msg.set_success(output_str)
                            else:
                                tool_msg.set_error(output_str or "Error")
                            # Clean up - remove from tracking dict after status update
                            adapter._current_tool_messages.pop(tool_id, None)

                        # Show file operation results - always show diffs in chat
                        if record:
                            pending_text = pending_text_by_namespace.get(ns_key, "")
                            if pending_text:
                                await _flush_assistant_text_ns(
                                    adapter,
                                    pending_text,
                                    ns_key,
                                    assistant_message_by_namespace,
                                )
                                pending_text_by_namespace[ns_key] = ""
                            if record.diff:
                                await adapter._mount_message(
                                    DiffMessage(record.diff, record.display_path)
                                )
                        continue

                    # Check if this is an AIMessageChunk with content
                    if not hasattr(message, "content_blocks"):
                        continue

                    # Process content blocks
                    for block in message.content_blocks:
                        block_type = block.get("type")

                        if block_type == "text":
                            text = block.get("text", "")
                            if text:
                                # Track accumulated text for reference
                                pending_text = pending_text_by_namespace.get(ns_key, "")
                                pending_text += text
                                pending_text_by_namespace[ns_key] = pending_text

                                # Get or create assistant message for this namespace
                                current_msg = assistant_message_by_namespace.get(ns_key)
                                if current_msg is None:
                                    # Hide spinner when assistant starts responding
                                    if adapter._set_spinner:
                                        await adapter._set_spinner(None)
                                    msg_id = f"asst-{uuid.uuid4().hex[:8]}"
                                    # Mark active BEFORE mounting so pruning
                                    # (triggered by mount) won't remove it
                                    # (_mount_message can trigger
                                    # _prune_old_messages if the window exceeds
                                    # WINDOW_SIZE.)
                                    if adapter._set_active_message:
                                        adapter._set_active_message(msg_id)
                                    current_msg = AssistantMessage(id=msg_id)
                                    await adapter._mount_message(current_msg)
                                    assistant_message_by_namespace[ns_key] = current_msg

                                # Append just the new text chunk for smoother
                                # streaming (uses MarkdownStream internally for
                                # better performance)
                                await current_msg.append_content(text)

                                # Sticky scroll: scroll to bottom only if user is
                                # near bottom. This lets users scroll away and
                                # stay where they are
                                if adapter._scroll_to_bottom:
                                    adapter._scroll_to_bottom()

                        elif block_type in {"tool_call_chunk", "tool_call"}:
                            chunk_name = block.get("name")
                            chunk_args = block.get("args")
                            chunk_id = block.get("id")
                            chunk_index = block.get("index")

                            buffer_key: str | int
                            if chunk_index is not None:
                                buffer_key = chunk_index
                            elif chunk_id is not None:
                                buffer_key = chunk_id
                            else:
                                buffer_key = f"unknown-{len(tool_call_buffers)}"

                            buffer = tool_call_buffers.setdefault(
                                buffer_key,
                                {
                                    "name": None,
                                    "id": None,
                                    "args": None,
                                    "args_parts": [],
                                },
                            )

                            if chunk_name:
                                buffer["name"] = chunk_name
                            if chunk_id:
                                buffer["id"] = chunk_id

                            if isinstance(chunk_args, dict):
                                buffer["args"] = chunk_args
                                buffer["args_parts"] = []
                            elif isinstance(chunk_args, str):
                                if chunk_args:
                                    parts: list[str] = buffer.setdefault(
                                        "args_parts", []
                                    )
                                    if not parts or chunk_args != parts[-1]:
                                        parts.append(chunk_args)
                                    buffer["args"] = "".join(parts)
                            elif chunk_args is not None:
                                buffer["args"] = chunk_args

                            buffer_name = buffer.get("name")
                            buffer_id = buffer.get("id")
                            if buffer_name is None:
                                continue

                            parsed_args = buffer.get("args")
                            if isinstance(parsed_args, str):
                                if not parsed_args:
                                    continue
                                try:
                                    parsed_args = json.loads(parsed_args)
                                except json.JSONDecodeError:
                                    continue
                            elif parsed_args is None:
                                continue

                            if not isinstance(parsed_args, dict):
                                parsed_args = {"value": parsed_args}

                            # Flush pending text before tool call
                            pending_text = pending_text_by_namespace.get(ns_key, "")
                            if pending_text:
                                if pending_text.strip():
                                    if conv_log:
                                        conv_log.log_assistant(pending_text)
                                    sidecar_emitter.emit_assistant_message(pending_text)
                                await _flush_assistant_text_ns(
                                    adapter,
                                    pending_text,
                                    ns_key,
                                    assistant_message_by_namespace,
                                )
                                pending_text_by_namespace[ns_key] = ""
                                assistant_message_by_namespace.pop(ns_key, None)

                            if (
                                buffer_id is not None
                                and buffer_id not in displayed_tool_ids
                            ):
                                displayed_tool_ids.add(buffer_id)
                                file_op_tracker.start_operation(
                                    buffer_name, parsed_args, buffer_id
                                )

                                if conv_log:
                                    conv_log.log_tool_call(buffer_name, parsed_args)
                                sidecar_emitter.emit_tool_call(
                                    name=buffer_name,
                                    args=parsed_args if isinstance(parsed_args, dict) else {"value": parsed_args},
                                    call_id=str(buffer_id) if buffer_id is not None else "",
                                )

                                # Hide spinner before showing tool call
                                if adapter._set_spinner:
                                    await adapter._set_spinner(None)

                                # Mount tool call message
                                tool_msg = ToolCallMessage(buffer_name, parsed_args)
                                await adapter._mount_message(tool_msg)
                                adapter._current_tool_messages[buffer_id] = tool_msg

                                # Sticky scroll after tool call is shown
                                if adapter._scroll_to_bottom:
                                    adapter._scroll_to_bottom()

                            tool_call_buffers.pop(buffer_key, None)

                    if getattr(message, "chunk_position", None) == "last":
                        pending_text = pending_text_by_namespace.get(ns_key, "")
                        if pending_text:
                            if pending_text.strip():
                                if conv_log:
                                    conv_log.log_assistant(pending_text)
                                sidecar_emitter.emit_assistant_message(pending_text)
                            await _flush_assistant_text_ns(
                                adapter,
                                pending_text,
                                ns_key,
                                assistant_message_by_namespace,
                            )
                            pending_text_by_namespace[ns_key] = ""
                            assistant_message_by_namespace.pop(ns_key, None)

            # Reset summarization state if stream ended mid-summarization
            # (e.g. middleware error, stream exhausted before regular chunks).
            if summarization_in_progress:
                summarization_in_progress = False
                try:
                    await adapter._mount_message(SummarizationMessage())
                except Exception:
                    logger.debug(
                        "Failed to mount summarization notification",
                        exc_info=True,
                    )
                if adapter._set_spinner:
                    await adapter._set_spinner("Thinking")

            # Flush any remaining text from all namespaces
            for ns_key, pending_text in list(pending_text_by_namespace.items()):
                if pending_text:
                    if pending_text.strip():
                        if conv_log:
                            conv_log.log_assistant(pending_text)
                        sidecar_emitter.emit_assistant_message(pending_text)
                    await _flush_assistant_text_ns(
                        adapter, pending_text, ns_key, assistant_message_by_namespace
                    )
            pending_text_by_namespace.clear()
            assistant_message_by_namespace.clear()

            # Handle HITL after stream completes
            if interrupt_occurred:
                any_rejected = False

                for interrupt_id, hitl_request in list(pending_interrupts.items()):
                    action_requests = hitl_request["action_requests"]

                    if session_state.auto_approve:
                        # Auto-approve silently - start running animation
                        decisions: list[HITLDecision] = [
                            ApproveDecision(type="approve") for _ in action_requests
                        ]
                        hitl_response[interrupt_id] = {"decisions": decisions}
                        # Mark all tools as running
                        for tool_msg in list(adapter._current_tool_messages.values()):
                            tool_msg.set_running()
                    else:
                        # Batch approval - one dialog for all parallel tool calls
                        future = await adapter._request_approval(
                            action_requests, assistant_id
                        )
                        decision = await future

                        # Handle the batch decision
                        if isinstance(decision, dict):
                            decision_type = decision.get("type")

                            if decision_type == "auto_approve_all":
                                # Enable auto-approve for session
                                session_state.auto_approve = True
                                if adapter._on_auto_approve_enabled:
                                    adapter._on_auto_approve_enabled()
                                # Approve all
                                decisions = [
                                    ApproveDecision(type="approve")
                                    for _ in action_requests
                                ]
                                tool_msgs = list(
                                    adapter._current_tool_messages.values()
                                )
                                for tool_msg in tool_msgs:
                                    tool_msg.set_running()
                                # Mark file ops as approved
                                for action_request in action_requests:
                                    tool_name = action_request.get("name")
                                    if tool_name in {"write_file", "edit_file"}:
                                        args = action_request.get("args", {})
                                        if isinstance(args, dict):
                                            file_op_tracker.mark_hitl_approved(
                                                tool_name, args
                                            )

                            elif decision_type == "approve":
                                # Approve all
                                decisions = [
                                    ApproveDecision(type="approve")
                                    for _ in action_requests
                                ]
                                tool_msgs = list(
                                    adapter._current_tool_messages.values()
                                )
                                for tool_msg in tool_msgs:
                                    tool_msg.set_running()
                                # Mark file ops as approved
                                for action_request in action_requests:
                                    tool_name = action_request.get("name")
                                    if tool_name in {"write_file", "edit_file"}:
                                        args = action_request.get("args", {})
                                        if isinstance(args, dict):
                                            file_op_tracker.mark_hitl_approved(
                                                tool_name, args
                                            )

                            elif decision_type == "reject":
                                # Reject all
                                decisions = [
                                    RejectDecision(type="reject")
                                    for _ in action_requests
                                ]
                                tool_msgs = list(
                                    adapter._current_tool_messages.values()
                                )
                                for tool_msg in tool_msgs:
                                    tool_msg.set_rejected()
                                adapter._current_tool_messages.clear()
                                any_rejected = True
                            else:
                                logger.warning(
                                    "Unexpected HITL decision type: %s",
                                    decision_type,
                                )
                                decisions = [
                                    RejectDecision(type="reject")
                                    for _ in action_requests
                                ]
                                for tool_msg in list(
                                    adapter._current_tool_messages.values()
                                ):
                                    tool_msg.set_rejected()
                                adapter._current_tool_messages.clear()
                                any_rejected = True
                        else:
                            logger.warning(
                                "HITL decision was not a dict: %s",
                                type(decision).__name__,
                            )
                            decisions = [
                                RejectDecision(type="reject") for _ in action_requests
                            ]
                            for tool_msg in list(
                                adapter._current_tool_messages.values()
                            ):
                                tool_msg.set_rejected()
                            adapter._current_tool_messages.clear()
                            any_rejected = True

                        hitl_response[interrupt_id] = {"decisions": decisions}

                        if any_rejected:
                            break

                suppress_resumed_output = any_rejected

            if interrupt_occurred and hitl_response:
                if suppress_resumed_output:
                    await adapter._mount_message(
                        AppMessage(
                            "Command rejected. Tell the agent what you'd like instead."
                        )
                    )
                    return

                stream_input = Command(resume=hitl_response)
            else:
                break

    except asyncio.CancelledError:
        # Clear active message immediately so it won't block pruning
        # If we don't do this, the store still thinks it's actice and protects
        # from pruning, which breaks get_messages_to_prune(), potentially
        # blocking all future pruning
        if adapter._set_active_message:
            adapter._set_active_message(None)

        # Hide spinner (may still show "Summarizing" if interrupted mid-summary)
        if adapter._set_spinner:
            await adapter._set_spinner(None)

        await adapter._mount_message(AppMessage("Interrupted by user"))

        # Save accumulated state before marking tools as rejected (best-effort)
        # State update failures shouldn't prevent cleanup
        try:
            interrupted_msg = _build_interrupted_ai_message(
                pending_text_by_namespace,
                adapter._current_tool_messages,
            )
            if interrupted_msg:
                await agent.aupdate_state(config, {"messages": [interrupted_msg]})

            cancellation_msg = HumanMessage(
                content="[SYSTEM] Task interrupted by user. "
                "Previous operation was cancelled."
            )
            await agent.aupdate_state(config, {"messages": [cancellation_msg]})
        except Exception:
            logger.debug("Failed to save interrupted state", exc_info=True)

        # Mark tools as rejected AFTER saving state
        for tool_msg in list(adapter._current_tool_messages.values()):
            tool_msg.set_rejected()
        adapter._current_tool_messages.clear()

        # Report tokens even on interrupt — failed work still counts as spend
        _commit_token_tracking(adapter, latest_main_context_input, latest_main_context_output, spend_input_delta, spend_output_delta, spend_cache_read_delta, spend_cache_creation_delta)
        sidecar_emitter.emit_turn_end()
        if conv_log:
            conv_log.close()
        return

    except KeyboardInterrupt:
        # Clear active message immediately so it won't block pruning
        # If we don't do this, the store still thinks it's actice and protects
        # from pruning, which breaks get_messages_to_prune(), potentially
        # blocking all future pruning
        if adapter._set_active_message:
            adapter._set_active_message(None)

        # Hide spinner (may still show "Summarizing" if interrupted mid-summary)
        if adapter._set_spinner:
            await adapter._set_spinner(None)

        await adapter._mount_message(AppMessage("Interrupted by user"))

        # Save accumulated state before marking tools as rejected (best-effort)
        # State update failures shouldn't prevent cleanup
        try:
            interrupted_msg = _build_interrupted_ai_message(
                pending_text_by_namespace,
                adapter._current_tool_messages,
            )
            if interrupted_msg:
                await agent.aupdate_state(config, {"messages": [interrupted_msg]})

            cancellation_msg = HumanMessage(
                content="[SYSTEM] Task interrupted by user. "
                "Previous operation was cancelled."
            )
            await agent.aupdate_state(config, {"messages": [cancellation_msg]})
        except Exception:
            logger.debug("Failed to save interrupted state", exc_info=True)

        # Mark tools as rejected AFTER saving state
        for tool_msg in list(adapter._current_tool_messages.values()):
            tool_msg.set_rejected()
        adapter._current_tool_messages.clear()

        # Report tokens even on interrupt — failed work still counts as spend
        _commit_token_tracking(adapter, latest_main_context_input, latest_main_context_output, spend_input_delta, spend_output_delta, spend_cache_read_delta, spend_cache_creation_delta)
        sidecar_emitter.emit_turn_end()
        if conv_log:
            conv_log.close()
        return

    # Normal completion: commit token tracking
    _commit_token_tracking(adapter, latest_main_context_input, latest_main_context_output, spend_input_delta, spend_output_delta, spend_cache_read_delta, spend_cache_creation_delta)
    sidecar_emitter.emit_turn_end()
    if conv_log:
        conv_log.close()


def _commit_token_tracking(
    adapter: "TextualUIAdapter",
    latest_main_context_input: int,
    latest_main_context_output: int,
    spend_input_delta: int,
    spend_output_delta: int,
    spend_cache_read_delta: int = 0,
    spend_cache_creation_delta: int = 0,
) -> None:
    """Commit captured token data to both tracking paths.

    Called exactly once per terminal path (normal completion, CancelledError,
    KeyboardInterrupt).  Never called per-chunk.

    Path A (context window): overwrite with latest main-agent context.
    Path B (cumulative spend): accumulate and emit to daemon.
    """
    has_context = latest_main_context_input or latest_main_context_output
    has_spend = spend_input_delta or spend_output_delta

    # Path A: update context window display (main non-summarization turns only)
    if adapter._token_tracker:
        if has_context:
            adapter._token_tracker.add(
                latest_main_context_input + latest_main_context_output
            )
        else:
            adapter._token_tracker.show()  # Restore previous value

    # Path B: accumulate spend and emit to daemon (all invocations)
    if has_spend:
        if adapter._spend_tracker:
            adapter._spend_tracker.record_turn(
                spend_input_delta,
                spend_output_delta,
                spend_cache_read_delta,
                spend_cache_creation_delta,
            )
        _emit_oat_tokens(adapter, spend_input_delta, spend_output_delta)


def _emit_oat_model() -> None:
    """Emit an [OAT_MODEL] banner to stdout so agent logs record the model."""
    import os
    import sys

    from oat_cli.config import settings

    model_id = settings.model_name or "unknown"
    provider = settings.model_provider or ""
    if provider and not model_id.startswith(f"{provider}:"):
        model_id = f"{provider}:{model_id}"
    line = f"[OAT_MODEL] {model_id}"
    out = getattr(sys, "__stdout__", None) or sys.stdout
    print(line, file=out, flush=True)

    tool_log = os.environ.get("OAT_TOOL_LOG")
    if tool_log:
        try:
            with open(tool_log, "a") as f:
                f.write(line + "\n")
                f.flush()
        except OSError:
            pass


def _emit_oat_tokens(
    adapter: "TextualUIAdapter",
    delta_input: int,
    delta_output: int,
) -> None:
    """Emit a structured [OAT_TOKENS] line to stdout for daemon parsing.

    The daemon's output watcher detects this line and updates per-agent
    token counts in the state file, which are then exposed in list_agents
    and displayed in the TUI token bar.

    Field semantics (honest names — no aliases):
      - ``delta_input`` / ``delta_output``: tokens spent this request
      - ``cumulative_input`` / ``cumulative_output``: monotonic lifetime totals

    Mixed-version operation is unsupported for v1.  Python runtime and Go
    daemon/TUI ship atomically in the same release.
    """
    import os
    import sys

    cumulative_input = 0
    cumulative_output = 0
    cumulative_cache_read = 0
    cumulative_cache_creation = 0
    if adapter._spend_tracker:
        cumulative_input = adapter._spend_tracker.total_input
        cumulative_output = adapter._spend_tracker.total_output
        cumulative_cache_read = getattr(
            adapter._spend_tracker, "total_cache_read", 0
        )
        cumulative_cache_creation = getattr(
            adapter._spend_tracker, "total_cache_creation", 0
        )

    payload = {
        "delta_input": delta_input,
        "delta_output": delta_output,
        "cumulative_input": cumulative_input,
        "cumulative_output": cumulative_output,
    }
    # Include cache metrics only when non-zero so non-caching providers
    # keep the payload compact and don't overwrite existing cache totals
    # with zeros on the daemon side.
    if cumulative_cache_read > 0 or cumulative_cache_creation > 0:
        payload["cache_read"] = cumulative_cache_read
        payload["cache_creation"] = cumulative_cache_creation
    line = f"[OAT_TOKENS] {json.dumps(payload)}"

    # Use __stdout__ to bypass Textual's stdout redirection.
    # Inside a Textual app, sys.stdout is captured by the framework;
    # __stdout__ is the original fd 1 that reaches the PTY/log file.
    out = getattr(sys, "__stdout__", None) or sys.stdout
    print(line, file=out, flush=True)

    # Also append directly to the log file the daemon tails. When
    # OAT_TOOL_LOG is set, direct_backend.go skips capturing PTY output
    # to this file (ConversationLogger writes it instead), so the stdout
    # print above never reaches the watcher. Writing the same line
    # directly keeps the daemon's OutputWatcher pipeline intact.
    log_path = os.environ.get("OAT_TOOL_LOG")
    if log_path:
        try:
            with Path(log_path).open("a", encoding="utf-8") as f:
                f.write(line + "\n")
                f.flush()
        except OSError:
            pass

    # Mirror the same payload onto the sidecar when OAT_SIDECAR_SOCKET is
    # set. No-op when unset — safe for every existing deployment. The
    # daemon's monotonicity guard dedupes: whichever path arrives first
    # wins, the second emission at the same cumulative is a no-op. This
    # gives token accounting a lossless delivery path (the stdout path
    # can drop under the rawBroadcaster's 512-slot channel flood).
    sidecar_emitter.emit_token_usage(
        delta_input=delta_input,
        delta_output=delta_output,
        cumulative_input=cumulative_input,
        cumulative_output=cumulative_output,
        cache_read=cumulative_cache_read,
        cache_creation=cumulative_cache_creation,
    )


async def _flush_assistant_text_ns(
    adapter: TextualUIAdapter,
    text: str,
    ns_key: tuple,
    assistant_message_by_namespace: dict[tuple, Any],
) -> None:
    """Flush accumulated assistant text for a specific namespace.

    Finalizes the streaming by stopping the MarkdownStream.
    If no message exists yet, creates one with the full content.
    """
    if not text.strip():
        return

    current_msg = assistant_message_by_namespace.get(ns_key)
    if current_msg is None:
        # No message was created during streaming - create one with full content
        msg_id = f"asst-{uuid.uuid4().hex[:8]}"
        current_msg = AssistantMessage(text, id=msg_id)
        await adapter._mount_message(current_msg)
        await current_msg.write_initial_content()
        assistant_message_by_namespace[ns_key] = current_msg
    else:
        # Stop the stream to finalize the content
        await current_msg.stop_stream()

    # When the AssistantMessage was first mounted and recorded in the
    # MessageStore, it had empty content (streaming hadn't started yet).
    # Now that streaming is done, the widget holds the full text in
    # `_content`, but the store's MessageData still has `content=""`.
    # If the message is later pruned and re-hydrated, `to_widget()` would
    # recreate it from that stale empty string. This call copies the
    # widget's final content back into the store so re-hydration works.
    if adapter._sync_message_content and current_msg.id:
        adapter._sync_message_content(current_msg.id, current_msg._content)

    # Clear active message since streaming is done
    if adapter._set_active_message:
        adapter._set_active_message(None)
