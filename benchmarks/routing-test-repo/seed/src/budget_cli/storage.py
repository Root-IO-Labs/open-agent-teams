"""JSON persistence for budget state.

Intentional issues (see AGENTS.md):
- `BUDGET_HOME` env var is not honored (task env-home-01).
- Serialization + file I/O are mixed (task storage-split-01).
"""
from __future__ import annotations

import json
import os
from pathlib import Path

from .models import BudgetState


def _default_state_path() -> Path:
    # NOTE: hardcoded — does not honor BUDGET_HOME. See AGENTS.md env-home-01.
    return Path.home() / ".budget" / "state.json"


def load_state(path: Path | None = None) -> BudgetState:
    """Load state from disk, returning an empty state if no file exists."""
    p = path or _default_state_path()
    if not p.exists():
        return BudgetState()
    with p.open() as f:
        data = json.load(f)
    return BudgetState.from_dict(data)


def save_state(state: BudgetState, path: Path | None = None) -> None:
    """Persist state to disk, creating parent dirs as needed."""
    p = path or _default_state_path()
    p.parent.mkdir(parents=True, exist_ok=True)
    with p.open("w") as f:
        json.dump(state.to_dict(), f, indent=2)
