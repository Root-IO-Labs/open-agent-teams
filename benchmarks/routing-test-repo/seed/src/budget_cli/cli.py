"""Budget CLI entry point.

Intentional issues (see AGENTS.md):
- Typo "expance" in add-command success message (task typo-01).
- `add` accepts negative amounts silently (task validate-01).
- `list` has no --category filter (task filter-01).
- No CSV export (task csv-export-01).
- `total` has no --month filter (task month-total-02).
"""
from __future__ import annotations

import json
import sys
from datetime import date

import click

from .models import BudgetState, Entry
from .storage import load_state, save_state


@click.group()
def cli() -> None:
    """budget — track personal expenses."""


@cli.command("add")
@click.argument("category")
@click.argument("amount", type=float)
@click.option("--note", default="", help="Optional note.")
def add(category: str, amount: float, note: str) -> None:
    """Add a new expense entry."""
    # NOTE: no validation of `amount` — negative values are silently accepted.
    state = load_state()
    entry = Entry(
        category=category,
        amount=amount,
        date=date.today().isoformat(),
        note=note,
    )
    state.add(entry)
    save_state(state)
    # NOTE: typo "expance" — see AGENTS.md typo-01.
    click.echo(f"Added expance: {category} ${amount:.2f}")


@cli.command("list")
def list_cmd() -> None:
    """List all entries."""
    # NOTE: no --category filter. See AGENTS.md filter-01.
    state = load_state()
    if not state.entries:
        click.echo("(no entries)")
        return
    for e in state.entries:
        note_part = f" — {e.note}" if e.note else ""
        click.echo(f"{e.date}  {e.category:<12} ${e.amount:>8.2f}{note_part}")


@cli.command("total")
def total() -> None:
    """Sum of all entries."""
    # NOTE: no --month filter. See AGENTS.md month-total-02.
    state = load_state()
    total_amount = sum(e.amount for e in state.entries)
    click.echo(f"Total: ${total_amount:.2f}")


@cli.command("export")
@click.option("--format", "fmt", type=click.Choice(["json"]), default="json",
              help="Output format.")
def export(fmt: str) -> None:
    """Export state. Currently JSON only (see AGENTS.md csv-export-01)."""
    state = load_state()
    if fmt == "json":
        json.dump(state.to_dict(), sys.stdout, indent=2)
        click.echo()
    else:
        raise click.BadParameter(f"unsupported format: {fmt}")


if __name__ == "__main__":
    cli()
