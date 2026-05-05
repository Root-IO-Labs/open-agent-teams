# budget-cli

A minimal personal-expense tracker CLI. Built as a fixture for OAT's routing evaluation suite.

## Install

```bash
pip install -e ".[dev]"
```

## Usage

```bash
budget add food 12.50 --note "lunch"
budget add transport 3.00
budget list
budget total --month 2026-04
```

State lives at `~/.budget/state.json` (override with `BUDGET_HOME`).

## Development

```bash
pytest               # run tests
ruff check .         # lint
ruff format .        # format
```

## Current state (intentional)

This repo ships incomplete by design — see `AGENTS.md` for the TODO list.
Expect at least the following to need fixing:

- A misspelling in user-facing help text
- Missing input validation on `add` (accepts negative amounts)
- No CSV export
- No per-category filter on `list`
- Hardcoded state path (env var not honored)
