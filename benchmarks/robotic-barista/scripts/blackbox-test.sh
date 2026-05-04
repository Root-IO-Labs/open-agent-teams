#!/usr/bin/env bash
# blackbox-test.sh -- Entry point for modular blackbox acceptance tests.
#
# Sources the shared helpers, auto-discovers test-*.sh modules in
# blackbox-tests/, runs all test_* functions, and reports results.
#
# Test modules are created by wave:0 workers (one file per issue).
# This file and helpers.sh are bundled in the scaffold -- workers
# should NOT modify either.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load shared test framework
source "${SCRIPT_DIR}/blackbox-tests/helpers.sh"

# Detect the CLI entry point
detect_cli

echo ""
echo "============================================"
echo "  Blackbox Acceptance Tests"
echo "============================================"
echo ""

# Auto-discover and source test modules
MODULE_COUNT=0
for module in "${SCRIPT_DIR}"/blackbox-tests/test-*.sh; do
    [[ -f "$module" ]] || continue
    echo "Loading module: $(basename "$module")"
    source "$module"
    MODULE_COUNT=$((MODULE_COUNT + 1))
done

if [[ $MODULE_COUNT -eq 0 ]]; then
    echo ""
    echo "WARNING: No test modules found in ${SCRIPT_DIR}/blackbox-tests/"
    echo "         Expected files matching test-*.sh"
    echo ""
    exit 1
fi

echo ""
echo "Loaded $MODULE_COUNT test module(s). Running tests..."
echo ""

# Run all test_* functions defined by the modules.
# Functions must be named test_<something> to be auto-discovered.
FUNC_COUNT=0
for func in $(declare -F | awk '{print $3}' | grep '^test_' | sort); do
    echo "--- ${func} ---"
    "$func"
    echo ""
    FUNC_COUNT=$((FUNC_COUNT + 1))
done

if [[ $FUNC_COUNT -eq 0 ]]; then
    echo "WARNING: No test_* functions found. Modules may not define runnable tests."
    exit 1
fi

# Print results and save JSON
print_results
save_results "${SCRIPT_DIR}/../blackbox-results.json"

# Exit with failure if any tests failed
if [[ $FAILED -gt 0 ]]; then
    exit 1
fi
