# budget-cli operational spec

A minimal personal-expense tracker, shipped as a fixture for OAT's routing-quality research. This spec defines the contract tasks are verified against.

## Commands

### `budget add <category> <amount> [--note TEXT]`

Record a new expense.

- `category`: free-form string (no whitespace).
- `amount`: positive float. Negative values MUST be rejected with a non-zero exit code.
- `--note`: optional free-text note.

Side effect: append an `Entry` to persisted state, keyed by today's ISO date.

### `budget list [--category NAME]`

Print all entries, one per line: `DATE  CATEGORY  AMOUNT [— NOTE]`.

When `--category NAME` is passed, only show entries with that exact category.

Empty state → print `(no entries)` and exit 0.

### `budget total [--month YYYY-MM]`

Print the sum of all amounts as `Total: $X.YY`.

When `--month YYYY-MM` is passed, only sum entries whose date starts with that month prefix. Invalid format → exit 1 with a clear error.

### `budget export [--format json|csv]`

Dump state to stdout.

- `--format json` (default): pretty-printed JSON of `{entries: [...]}`.
- `--format csv`: header `date,category,amount,note` followed by one row per entry.

## Storage

State is a list of entries persisted to a JSON file.

**Path resolution:**
1. If `BUDGET_HOME` env var is set → `$BUDGET_HOME/state.json`.
2. Otherwise → `$HOME/.budget/state.json`.

## Error handling

The CLI should expose a typed error hierarchy rooted at `BudgetError`:

- `ValidationError` (exit code 1) — user-input problems
- `StorageError` (exit code 2) — can't read/write state
- `ExportError` (exit code 3) — unsupported export format, bad destination

The CLI's top-level handler catches `BudgetError` and prints the message to stderr before exiting with the appropriate code. Any other exception propagates (so developers see tracebacks during development).

## Non-goals

- No multi-user support.
- No currency conversion.
- No locking/concurrent writes (single-user assumption).
- No external databases.
