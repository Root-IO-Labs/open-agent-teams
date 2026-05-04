#!/usr/bin/env bash
# check.sh — Run all contract and interface checks for deepagents-cli.
#
# Usage:
#   bash scripts/check.sh            # run everything
#   bash scripts/check.sh --no-lint  # skip linting, run only tests
#
# Exit codes:
#   0  all checks passed
#   1  one or more checks failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$ROOT_DIR"

NO_LINT=false
for arg in "$@"; do
  case "$arg" in
    --no-lint) NO_LINT=true ;;
  esac
done

FAILED=0

run() {
  local label="$1"
  shift
  echo ""
  echo "──────────────────────────────────────────"
  echo "  $label"
  echo "──────────────────────────────────────────"
  if "$@"; then
    echo "  ✓ $label passed"
  else
    echo "  ✗ $label FAILED"
    FAILED=1
  fi
}

# ── 1. Import sanity check ────────────────────────────────────────────────────
run "Import check" \
  uv run --all-groups python ./scripts/check_imports.py \
    $(find deepagents_cli -name '*.py' | tr '\n' ' ')

# ── 2. Linting (skippable) ────────────────────────────────────────────────────
if [ "$NO_LINT" = false ]; then
  run "Ruff lint" \
    uv run --all-groups ruff check deepagents_cli tests

  run "Ruff format check" \
    uv run --all-groups ruff format deepagents_cli tests --diff
fi

# ── 3. Unit tests ─────────────────────────────────────────────────────────────
run "Unit tests" \
  uv run --group test pytest -n auto --disable-socket --allow-unix-socket \
    tests/unit_tests/ \
    --cov=deepagents_cli \
    --cov-report=term-missing \
    -q

# ── 4. CLI commands interface contract tests ──────────────────────────────────
run "CLI interface contract tests" \
  uv run --group test pytest --disable-socket --allow-unix-socket \
    "tests/interfaces/cli-commands-contract.test.py" \
    -v

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "══════════════════════════════════════════"
if [ "$FAILED" -eq 0 ]; then
  echo "  All checks passed."
else
  echo "  One or more checks FAILED."
fi
echo "══════════════════════════════════════════"

exit "$FAILED"
