#!/usr/bin/env bash
set -euo pipefail

echo "==> Running repo gate"

# Lint: Catch style issues and simple bugs
echo "==> Running ruff check..."
ruff check .

# Format: Check code formatting
echo "==> Running ruff format check..."
ruff format --check .

# Typecheck: Catch type errors before runtime
echo "==> Running mypy..."
mypy src

# Tests: Fast feedback on logic correctness
echo "==> Running tests..."
python -m pytest tests/ -v

echo "==> Gate passed ✅"
