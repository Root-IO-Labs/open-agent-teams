"""Test fixtures that redirect state to a tempdir so tests don't stomp on ~/.budget."""
from __future__ import annotations

import os
from pathlib import Path

import pytest


@pytest.fixture(autouse=True)
def isolated_budget_home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Point both BUDGET_HOME and HOME at a tempdir so each test is clean.

    Tests that are verifying the env-var-honoring behavior will look for
    BUDGET_HOME specifically. Other tests just benefit from a clean HOME.
    """
    budget_home = tmp_path / ".budget"
    monkeypatch.setenv("BUDGET_HOME", str(budget_home))
    monkeypatch.setenv("HOME", str(tmp_path))
    return budget_home
