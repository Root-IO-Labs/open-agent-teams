# Agent instructions for `budget-cli`

This repo is a routing-test fixture. Agents receive single, focused tasks that match one of the pre-seeded issues below. The task description is self-contained — you don't need to read this file to complete a task. It's documented here so humans can audit which tasks exist.

## Ground rules

- Do not add external dependencies beyond `click` and `pytest` (already in `pyproject.toml`).
- Keep changes minimal and scoped to the task. Don't refactor unrelated code.
- Tests must pass. `pytest` with no args is the gate.
- `ruff check .` should be clean after your changes.

## Pre-seeded issues (these are the tasks the harness will spawn workers for)

### Trivial

- **typo-01:** `src/budget_cli/cli.py` prints "Added expance" (should be "expense").

### Simple

- **validate-01:** `budget add <category> <amount>` accepts negative amounts silently. Reject with a non-zero exit code and a clear error message.
- **env-home-01:** The storage path is hardcoded to `~/.budget/state.json`. It should honor `BUDGET_HOME` env var if set, falling back to `~/.budget`.
- **filter-01:** `budget list` has no `--category` filter. Add one that restricts output to a specific category.

### Standard

- **csv-export-01:** Add `budget export --format csv` that writes all entries to stdout as CSV (columns: `date,category,amount,note`). Keep existing `--format json` working.
- **month-total-02:** `budget total` currently sums everything. Add `--month YYYY-MM` flag that filters to a specific month. Fail gracefully if the format is wrong.

### Complex

- **storage-split-01:** The current `storage.py` does both serialization AND file I/O. Split it: a `Serializer` interface (with JSON + CSV implementations) and a `Repo` class that owns the file handle. All existing tests must still pass. Update `cli.py` to the new structure. This touches 3+ files.
- **typed-errors-01:** Replace the `ValueError` / `click.BadParameter` mix with a small hierarchy (`BudgetError` base, `StorageError`, `ValidationError`, `ExportError`). Wire through cli.py's error-handling, return non-zero exit codes. Tests should still pass; add one new test per error type.
