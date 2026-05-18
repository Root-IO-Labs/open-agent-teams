"""Help screens and argparse utilities for the CLI.

This module is imported at CLI startup to wire `-h` actions into the
argparse tree.  It must stay lightweight — no SDK or langchain imports.
"""

import argparse
from collections.abc import Callable

from oat_cli._version import __version__
from oat_cli.config import (
    COLORS,
    DOCS_URL,
    _is_editable_install,
    console,
)


def build_help_parent(
    help_fn: Callable[[], None],
    make_help_action: Callable[[Callable[[], None]], type[argparse.Action]],
) -> list[argparse.ArgumentParser]:
    """Build a parent parser whose `-h` invokes *help_fn*.

    This eliminates boilerplate: without the helper every `add_parser`
    call would need its own three-line parent-parser setup.  Used by both
    `main.parse_args` and `skills.commands.setup_skills_parser`.

    Args:
        help_fn: Zero-argument callable that renders a Rich help screen.
        make_help_action: Factory that turns *help_fn* into an argparse
            Action class (see `main._make_help_action`).

    Returns:
        Single-element list suitable for the `parents` kwarg of
        `add_parser`.
    """
    parent = argparse.ArgumentParser(add_help=False)
    parent.add_argument("-h", "--help", action=make_help_action(help_fn))
    return [parent]


def show_help() -> None:
    """Show top-level help information for the oat-agent CLI."""
    install_type = " (local)" if _is_editable_install() else ""
    banner_color = (
        COLORS["primary_dev"] if _is_editable_install() else COLORS["primary"]
    )
    console.print()
    console.print(
        f"[bold {banner_color}]oat-cli[/bold {banner_color}]"
        f" v{__version__}{install_type}"
    )
    console.print()
    console.print(
        f"Docs: [link={DOCS_URL}]{DOCS_URL}[/link]",
        style=COLORS["dim"],
    )
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print(
        "  oat-agent [OPTIONS]                           Start interactive thread"
    )
    console.print(
        "  oat-agent list                                List all available agents"
    )
    console.print(
        "  oat-agent reset --agent AGENT [--target SRC]  Reset an agent's prompt"
    )
    console.print(
        "  oat-agent skills <list|create|info|delete>    Manage agent skills"
    )
    console.print(
        "  oat-agent threads <list|delete>               Manage conversation threads"
    )
    console.print()

    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print(
        "  -r, --resume [ID]          Resume thread: -r for most recent, -r ID for specific"  # noqa: E501
    )
    console.print("  -a, --agent NAME           Agent to use (e.g., coder, researcher)")
    console.print("  -M, --model MODEL          Model to use (e.g., gpt-4o)")
    console.print(
        "  --model-params JSON        Extra model kwargs (e.g., '{\"temperature\": 0.7}')"  # noqa: E501
    )
    console.print("  --profile-override JSON    Override model profile fields as JSON")
    console.print("  -m, --message TEXT         Initial prompt to auto-submit on start")
    console.print(
        "  --auto-approve             Auto-approve all tool calls (toggle: Shift+Tab)"
    )
    console.print("  --sandbox TYPE             Remote sandbox for execution")
    console.print(
        "  --sandbox-id ID            Reuse existing sandbox (skips creation/cleanup)"
    )
    console.print(
        "  --sandbox-setup PATH       Setup script to run in sandbox after creation"
    )
    console.print("  -n, --non-interactive MSG  Run a single task and exit")
    console.print("  -q, --quiet                Clean output for piping (needs -n)")
    console.print("  --thread-id ID             Use custom thread ID for new session")
    console.print(
        "  --no-stream                Buffer full response instead of streaming"
    )
    console.print(
        "  --shell-allow-list CMDS    Comma-separated local shell commands to allow"
    )
    console.print(
        "  --deny-tool NAME           Hide a tool from the agent (repeatable; e.g. task)"
    )
    console.print("  --default-model [MODEL]    Set, show, or manage the default model")
    console.print("  --clear-default-model      Clear the default model")
    console.print("  -v, --version              Show oat-agent CLI and SDK versions")
    console.print("  -h, --help                 Show this help message and exit")
    console.print()

    console.print("[bold]Non-Interactive Mode:[/bold]", style=COLORS["primary"])
    console.print(
        "  oat-agent -n 'Summarize README.md'     # Run task (no local shell access)",
        style=COLORS["dim"],
    )
    console.print(
        "  oat-agent -n 'List files' --shell-allow-list recommended  # Use safe commands",  # noqa: E501
        style=COLORS["dim"],
    )
    console.print(
        "  oat-agent -n 'Search logs' --shell-allow-list ls,cat,grep # Specify list",
        style=COLORS["dim"],
    )
    console.print()


def show_list_help() -> None:
    """Show help information for the `list` subcommand.

    Invoked via the `-h` argparse action or directly from `cli_main`.
    """
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent list")
    console.print()
    console.print(
        "List all agents found in ~/.oat/agents/. Each agent has its own",
    )
    console.print(
        "AGENTS.md system prompt and separate thread history.",
    )
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  -h, --help        Show this help message")
    console.print()


def show_reset_help() -> None:
    """Show help information for the `reset` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent reset --agent NAME [--target SRC]")
    console.print()
    console.print(
        "Restore an agent's AGENTS.md to the built-in default, or copy",
    )
    console.print(
        "another agent's AGENTS.md. This deletes the agent's directory",
    )
    console.print(
        "and recreates it with the new prompt.",
    )
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Agent to reset (required)")
    console.print("  --target SRC      Copy AGENTS.md from another agent instead")
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent reset --agent coder")
    console.print("  oat-agent reset --agent coder --target researcher")
    console.print()


def show_skills_help() -> None:
    """Show help information for the `skills` subcommand.

    Invoked via the `-h` argparse action or directly from
    `execute_skills_command` when no subcommand is given.
    """
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills <command> [options]")
    console.print()
    console.print("[bold]Commands:[/bold]", style=COLORS["primary"])
    console.print("  list|ls           List all available skills")
    console.print("  create <name>     Create a new skill")
    console.print("  info <name>       Show detailed information about a skill")
    console.print("  delete <name>     Delete a skill")
    console.print()
    console.print("[bold]Common options:[/bold]", style=COLORS["primary"])
    console.print("  --agent <name>    Specify agent identifier (default: agent)")
    console.print("  --project         Use project-level skills instead of user-level")
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills list")
    console.print("  oat-agent skills list --project")
    console.print("  oat-agent skills create my-skill")
    console.print("  oat-agent skills create my-skill --agent myagent")
    console.print("  oat-agent skills info my-skill")
    console.print("  oat-agent skills delete my-skill")
    console.print("  oat-agent skills delete my-skill --force --project")
    console.print("  oat-agent skills delete -h")
    console.print()
    console.print(
        "[bold]Skill directories (highest precedence first):[/bold]",
        style=COLORS["primary"],
    )
    console.print(
        "[dim]  1. .agents/skills/                 project skills\n"
        "  2. .oat/skills/                   project skills (alias)\n"
        "  3. ~/.agents/skills/               user skills\n"
        "  4. ~/.oat/agents/<agent>/skills/   user skills (alias)\n"
        "  5. <package>/built_in_skills/      built-in skills[/dim]",
        style=COLORS["dim"],
    )
    console.print()


def show_skills_list_help() -> None:
    """Show help information for the `skills list` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills list [options]")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Agent identifier (default: agent)")
    console.print("  --project         Show only project-level skills")
    console.print("  -h, --help        Show this help message")
    console.print()


def show_skills_create_help() -> None:
    """Show help information for the `skills create` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills create <name> [options]")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Agent identifier (default: agent)")
    console.print(
        "  --project         Create in project directory instead of user directory"
    )
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills create web-research")
    console.print("  oat-agent skills create my-skill --project")
    console.print()


def show_skills_info_help() -> None:
    """Show help information for the `skills info` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills info <name> [options]")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Agent identifier (default: agent)")
    console.print("  --project         Search only in project skills")
    console.print("  -h, --help        Show this help message")
    console.print()


def show_skills_delete_help() -> None:
    """Show help information for the `skills delete` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills delete <name> [options]")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Agent identifier (default: agent)")
    console.print("  --project         Search only in project skills")
    console.print("  -f, --force       Skip confirmation prompt")
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent skills delete old-skill")
    console.print("  oat-agent skills delete old-skill --force")
    console.print("  oat-agent skills delete old-skill --project")
    console.print()


def show_threads_help() -> None:
    """Show help information for the `threads` subcommand.

    Invoked via the `-h` argparse action or directly from `cli_main`
    when no threads subcommand is given.
    """
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads <command> [options]")
    console.print()
    console.print("[bold]Commands:[/bold]", style=COLORS["primary"])
    console.print("  list|ls           List all threads")
    console.print("  delete <ID>       Delete a thread")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads list")
    console.print("  oat-agent threads delete abc123")
    console.print()


def show_threads_delete_help() -> None:
    """Show help information for the `threads delete` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads delete <ID>")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads delete abc123")
    console.print()


def show_threads_list_help() -> None:
    """Show help information for the `threads list` subcommand."""
    console.print()
    console.print("[bold]Usage:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads list [options]")
    console.print()
    console.print("[bold]Options:[/bold]", style=COLORS["primary"])
    console.print("  --agent NAME      Filter by agent name")
    console.print("  --limit N         Maximum threads to display (default: 20)")
    console.print("  -h, --help        Show this help message")
    console.print()
    console.print("[bold]Examples:[/bold]", style=COLORS["primary"])
    console.print("  oat-agent threads list")
    console.print("  oat-agent threads list --agent mybot")
    console.print("  oat-agent threads list --limit 50")
    console.print()
