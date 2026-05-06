"""Tests for budget-cli.

Most tests pass against the shipped seed — they verify the baseline features
that already exist. A few are xfail-marked: those correspond to the seeded
"missing feature" tasks. A worker that completes a task should flip its
xfail to pass, and the verify script for that task looks for the pass.
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest
from click.testing import CliRunner

from budget_cli.cli import cli
from budget_cli.models import BudgetState, Entry


# ── Baseline tests: these pass out of the box ────────────────────────────────

def test_add_then_list():
    runner = CliRunner()
    r = runner.invoke(cli, ["add", "food", "12.50"])
    assert r.exit_code == 0

    r = runner.invoke(cli, ["list"])
    assert r.exit_code == 0
    assert "food" in r.output
    assert "12.50" in r.output


def test_total_sums_entries():
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "10"])
    runner.invoke(cli, ["add", "food", "20"])
    r = runner.invoke(cli, ["total"])
    assert r.exit_code == 0
    assert "30.00" in r.output


def test_export_json_is_valid():
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "1.50"])
    r = runner.invoke(cli, ["export", "--format", "json"])
    assert r.exit_code == 0
    parsed = json.loads(r.output)
    assert "entries" in parsed
    assert len(parsed["entries"]) == 1


def test_entry_roundtrip():
    e = Entry(category="food", amount=1.23, date="2026-04-23", note="lunch")
    d = e.to_dict()
    restored = Entry.from_dict(d)
    assert restored == e


# ── xfail tests: each maps to a seeded task. Completing the task flips to pass.

@pytest.mark.xfail(reason="seed task typo-01: 'expance' → 'expense'")
def test_typo_expense_not_expance():
    runner = CliRunner()
    r = runner.invoke(cli, ["add", "food", "1"])
    assert "expance" not in r.output.lower(), "typo 'expance' still present"
    assert "expense" in r.output.lower() or "added" in r.output.lower()


@pytest.mark.xfail(reason="seed task validate-01: reject negative amounts")
def test_reject_negative_amount():
    # Use `--` to tell Click "positional args follow" so -5 isn't parsed
    # as an option. Today Click accepts it, storage saves it, and we exit 0.
    # After validate-01, the handler should check amount < 0 and fail cleanly.
    runner = CliRunner()
    r = runner.invoke(cli, ["add", "--", "food", "-5"])
    assert r.exit_code != 0, "should reject negative amounts"
    # Belt + suspenders: also check the word "negative" or "invalid" in output
    assert "negative" in r.output.lower() or "invalid" in r.output.lower() or "error" in r.output.lower()


@pytest.mark.xfail(reason="seed task env-home-01: honor BUDGET_HOME")
def test_env_home_respected(tmp_path, monkeypatch):
    custom = tmp_path / "custom-budget"
    monkeypatch.setenv("BUDGET_HOME", str(custom))
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "1"])
    assert (custom / "state.json").exists(), "BUDGET_HOME env var not honored"


@pytest.mark.xfail(reason="seed task filter-01: --category filter on list")
def test_list_category_filter():
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "5"])
    runner.invoke(cli, ["add", "transport", "3"])
    r = runner.invoke(cli, ["list", "--category", "food"])
    assert r.exit_code == 0
    assert "food" in r.output
    assert "transport" not in r.output, "--category filter not applied"


@pytest.mark.xfail(reason="seed task csv-export-01: --format csv")
def test_export_csv():
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "1.50", "--note", "lunch"])
    r = runner.invoke(cli, ["export", "--format", "csv"])
    assert r.exit_code == 0
    assert "date,category,amount,note" in r.output
    assert "food,1.5" in r.output or "food,1.50" in r.output


@pytest.mark.xfail(reason="seed task month-total-02: --month flag on total")
def test_total_month_filter():
    runner = CliRunner()
    runner.invoke(cli, ["add", "food", "10"])
    r = runner.invoke(cli, ["total", "--month", "1999-01"])
    assert r.exit_code == 0
    assert "0.00" in r.output, "--month filter didn't exclude current entries"


# ── Complex-task verification hooks ──────────────────────────────────────────
# These tests check that specific structural changes happened. They're also
# xfail in the seed and pass after the task is completed.

@pytest.mark.xfail(reason="seed task storage-split-01: Repo + Serializer split")
def test_storage_split_has_serializer():
    # After the split, we expect either budget_cli.serializer or serialization logic
    # extracted from storage.py. The verify script asserts on imports rather than a
    # specific API shape — workers have some freedom in naming.
    import importlib
    # Accept either of these shapes:
    for modname in ("budget_cli.serializer", "budget_cli.serialization"):
        try:
            importlib.import_module(modname)
            return
        except ImportError:
            continue
    pytest.fail("no serializer module found — expected budget_cli.serializer or .serialization")


@pytest.mark.xfail(reason="seed task typed-errors-01: BudgetError hierarchy")
def test_typed_errors_hierarchy():
    from budget_cli import errors  # noqa: F401 — must exist

    assert hasattr(errors, "BudgetError"), "BudgetError base class missing"
    for sub in ("StorageError", "ValidationError", "ExportError"):
        assert hasattr(errors, sub), f"{sub} missing from budget_cli.errors"
        cls = getattr(errors, sub)
        assert issubclass(cls, errors.BudgetError), f"{sub} must subclass BudgetError"
