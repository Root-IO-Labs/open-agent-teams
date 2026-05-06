# Writing Robust CLI Blackbox Tests

This guide covers general best practices for writing blackbox acceptance tests against a CLI application. The goal is to produce a test script that reliably validates whether the application meets its specification — without importing internals or reading implementation files.

## Core Principles

1. **Test only the public interface.** Your test script should invoke the CLI binary and inspect its stdout, stderr, and exit codes. Never import application modules or read internal data files directly.

2. **Derive tests from the specification.** Every test case should trace back to a documented behavior. Read the spec thoroughly — error cases, edge cases, and workflows are all testable.

3. **Be a skeptical user.** Think about what a real user would try, including mistakes: wrong arguments, missing required flags, invalid values, operations in the wrong order.

4. **Never use `set -e`.** Test scripts intentionally run commands that fail (to verify error handling). Using `set -e` causes the script to exit on the first expected failure. Use `set -uo pipefail` instead.

5. **Target bash 4+ but avoid bash-version pitfalls.** macOS ships with bash 3.2 (due to licensing), which does **not** support associative arrays (`declare -A`), `${var,,}` lowercase expansion, or `readarray`/`mapfile`. If you need associative arrays, use indexed arrays with a naming convention instead, or use simple variables. If the test must run on macOS (common in CI and developer machines), stick to bash 3.2-compatible syntax. Using `#!/usr/bin/env bash` with `declare -A` will silently fail on stock macOS, causing `set -u` to trigger "unbound variable" errors that crash the script before any tests run.

## Flexible Output Parsing

CLI implementations vary in how they format output. Your test should handle this gracefully:

- **ID formats differ.** Some implementations return sequential IDs like `order-1`, others return UUIDs like `order-a1b2c3d4`. Write extraction helpers that handle multiple formats rather than hardcoding a single regex pattern.

```bash
# Fragile — breaks if IDs aren't sequential integers
id=$(echo "$output" | grep -o 'order-[0-9]*')

# Better — handles prefixed IDs with various character sets
extract_id() {
    local output="$1" prefix="$2"
    echo "$output" | grep -oE "${prefix}-[a-zA-Z0-9-]+" | head -1
}

# Most robust — multi-strategy: try UUID, then prefix, then quoted, then first line
extract_id_flexible() {
    local text="$1"
    local id=""
    id=$(echo "$text" | grep -oE "[a-f0-9-]{8,}" | head -1 || true)
    if [[ -z "$id" ]]; then
        id=$(echo "$text" | grep -oE "'[^']+'" | head -1 | tr -d "'" || true)
    fi
    if [[ -z "$id" ]]; then
        id=$(echo "$text" | head -1 | tr -d '[:space:]' || true)
    fi
    echo "$id"
}
```

- **Case-insensitive matching.** Error messages and status labels may differ in capitalization. Use `grep -i` for pattern matching.

- **Match key phrases, not exact strings.** Instead of expecting `"Error: Item 'widget' already exists"`, match on `"already exists"` or `"duplicate"`. Implementations may word things differently while communicating the same error.

- **Be specific enough to distinguish errors.** A pattern like `"error"` alone matches *any* error message and tells you nothing about whether the *right* error was triggered. Use phrases specific to the error condition (e.g., `"not found"` for missing resources, `"already exists"` for duplicates, `"mismatch"` for type conflicts).

- **Match structural elements, not embedded values.** When verifying detailed output, check for section headers or labels rather than specific embedded values. The structure proves the implementation is correct; exact value formatting varies.

- **Verify mutating command output.** For commands that create or modify state (add, create, update, delete), verify the confirmation message format matches the spec, not just the exit code. A successful exit code with wrong or missing confirmation output indicates the operation may have partially succeeded or the output contract is broken. This catches "works but doesn't report correctly" bugs that exit-code-only checks miss.

## Scoring and Reporting

A good test script doesn't just report pass/fail — it provides structured results:

- **Group tests by functional area** (e.g., CRUD commands, workflow operations, error handling). This makes it easy to see which areas of the application are working and which aren't.

- **Assign point budgets per category.** Not all features are equally important. A complete end-to-end workflow matters more than a single error message check. Weight your scoring accordingly.

- **Report per-category subtotals and a total score.** This gives a nuanced view rather than a single pass/fail number.

```bash
# Example scoring structure
for cat in $CATEGORIES; do
    echo "  ${cat}: ${passed}/${total} (${points}/${budget} points)"
done
echo "Total: ${total_points} / ${max_points}"
```

- **Always print clear PASS/FAIL per test case** with the test name, so failures are easy to locate and debug.

- **Use integer arithmetic for scoring.** Avoid external math tools like `bc` — they may not be installed on all systems. Use bash integer arithmetic (`$(( ))`) instead. If you need fractional points, multiply by 100 and work in "centpoints" (e.g., 75 centpoints = 0.75 points), or track numerator and denominator separately.

- **Support partial credit.** Some tests may partially pass — the command succeeds but the output wording doesn't match exactly. Rather than a binary pass/fail, award fractional credit (e.g., 0.75) to capture "functionally correct but differently worded" results.

```bash
pass_partial() {
    local name="$1" reason="$2"
    PARTIAL=$((PARTIAL + 1))
    echo "  PARTIAL: $name"
    echo "           $reason"
}
```

## Machine-Readable Results

In addition to console output, save results to a JSON file so external tools can parse them. Use string concatenation or `printf` to build the JSON — don't require `jq` as a dependency.

```bash
RESULTS=()

pass() {
    RESULTS+=("{\"name\": \"$1\", \"status\": \"pass\"}")
    echo "  PASS: $1"
}

fail() {
    local reason_escaped=$(echo "$2" | sed 's/"/\\"/g')
    RESULTS+=("{\"name\": \"$1\", \"status\": \"fail\", \"reason\": \"$reason_escaped\"}")
    echo "  FAIL: $1"
}

save_results() {
    local json="["
    local first=true
    for r in "${RESULTS[@]}"; do
        if $first; then first=false; else json+=","; fi
        json+="$r"
    done
    json+="]"
    echo "$json" > results.json
}
```

Include per-test status, per-category scores, and a total score in the JSON output.

## CLI Detection and Fallbacks

Don't assume the CLI is available under a single name. Applications may be installed as:
- A console script entry point (e.g., `myapp`)
- A Python module invocation (e.g., `python -m myapp.cli`)
- A wrapper script in `scripts/` or `bin/`

Check the project's spec or `pyproject.toml` for the exact entry point names, then build a detection function that tries multiple paths. Use a **bash array** for the command so multi-word invocations like `python -m myapp.cli` work correctly with `run_cmd`:

```bash
detect_cli() {
    if command -v myapp &>/dev/null; then
        CLI_CMD=(myapp)
    elif python -m myapp.cli --help &>/dev/null 2>&1; then
        CLI_CMD=(python -m myapp.cli)
    elif [[ -f scripts/run.sh ]]; then
        CLI_CMD=(bash scripts/run.sh)
    else
        echo "FATAL: No CLI entry point found"
        echo "Tried: myapp, python -m myapp.cli, scripts/run.sh"
        exit 1
    fi
}

detect_cli

run_cmd() {
    EXIT_CODE=0
    OUTPUT=$("${CLI_CMD[@]}" "$@" 2>&1) || EXIT_CODE=$?
}
```

Using an array (`CLI_CMD`) avoids word-splitting issues. A plain string variable like `CLI="python -m myapp.cli"` breaks when passed to `$CLI "$@"` because the shell treats the entire string as one word.

## Data Isolation

Tests must not interfere with each other. Use temporary directories for each test section:

- Create a fresh data directory before each logical group of tests.
- Use environment variables to point the application at the temporary directory (check the spec for the relevant env var).
- Clean up with a `trap` on exit.

```bash
TMPDIR_BASE=$(mktemp -d)
trap 'rm -rf "$TMPDIR_BASE"' EXIT

new_data_dir() {
    mktemp -d "$TMPDIR_BASE/test-XXXXXX"
}
```

This prevents test pollution — a failure in one section won't cascade into false failures in another.

A clean pattern is to wrap each test group in its own function, each creating a fresh data directory:

```bash
test_create_commands() {
    new_data_dir                          # sets BARISTA_DATA_DIR as a side-effect
    local data_dir="$BARISTA_DATA_DIR"    # capture the path if you need it
    # ... creation tests ...
}

test_error_cases() {
    new_data_dir
    # ... error handling tests ...
}

test_create_commands
test_error_cases
```

This makes tests self-contained and easy to run or debug individually.

> **WARNING — subshell trap:** Do **not** call `new_data_dir` inside `$()` command substitution (e.g., `data_dir=$(new_data_dir)`). The scaffold's `helpers.sh` version of `new_data_dir` exports the data-directory environment variable as a side-effect. `$()` runs the function in a subshell, so the export is silently discarded and the parent shell keeps the old value — breaking data isolation between test groups. Always call it bare and read `$BARISTA_DATA_DIR` afterward.

- **State mutation within a test group.** Even within a single test group that shares a data directory, commands that transition state permanently change a resource. If test A validates an order and test B later checks that the same order is in "placed" state, test B will fail -- the order is now "validated." Create dedicated resources for each test that needs a specific state, or group state-transition tests in their own function with a fresh data directory. Never reuse a mutated resource for assertions about its original state.

- **Never use silent helpers for test preconditions.** A common mistake is creating a `run_cmd_silent` wrapper that redirects all output to `/dev/null` for setup steps (adding inventory, creating recipes). If any setup command fails silently, every test that depends on that data produces cascading false failures that are impossible to diagnose. Instead, create a `setup_cmd` helper that uses `run_cmd` internally and fatally aborts with diagnostic output on failure:

```bash
setup_cmd() {
    local desc="$1"; shift
    run_cmd "$@"
    if [[ $EXIT_CODE -ne 0 ]]; then
        echo "  FATAL: setup step failed: $desc"
        echo "         command: ${CLI_CMD[*]} $*"
        echo "         exit=$EXIT_CODE output: $(echo "$OUTPUT" | head -c 500)"
        echo "         last error: $(echo "$OUTPUT" | tail -1)"
        return 1
    fi
}

test_workflows() {
    export APP_DATA_DIR=$(new_data_dir)
    setup_cmd "seed data" create-item --name "test-item" || return
    setup_cmd "seed config" set-config --key "timeout" --value "30" || return
    # ... actual tests follow ...
}
```

This way, if a setup step fails, the test function exits immediately with a clear error message instead of producing dozens of misleading failures.

> **Diagnostic output matters.** FATAL and FAIL messages should include enough error output to identify the root cause without re-running the command. The `head -c 500` captures the first 500 bytes of output (enough for most tracebacks), and `tail -1` captures the last line — which for Python tracebacks is the actual exception (e.g., `KeyError: 'ingredient_name'`). Together they give both context and the actionable diagnostic. If your FATAL message truncates the output too aggressively (e.g., `head -c 200`), critical information like exception types and missing keys will be lost, making failures impossible to debug from the test output alone.

> **Common pitfall: helper function names are not CLI arguments.**
> `run_cmd` and `setup_cmd` are shell functions defined in your test script -- they are **not** subcommands of the application being tested. Never pass one helper's name as an argument to another helper.
>
> ```bash
> # WRONG -- "run_cmd" is passed as a CLI argument, not recognized by the app
> setup_cmd "place order" run_cmd order place latte --size M
>
> # RIGHT -- pass only the CLI arguments after the description
> setup_cmd "place order" order place latte --size M
> ```
>
> `setup_cmd` already calls `run_cmd` internally. Nesting the names causes the application to receive `"run_cmd"` as a literal argument, which either silently fails or triggers confusing errors.

## Exit Code and Output Validation

Always check **both** the exit code and the output content:

- A command that exits 0 but produces wrong output is a bug.
- A command that exits non-zero but with the right error message is a correctly handled error.

```bash
run_cmd() {
    EXIT_CODE=0
    OUTPUT=$("${CLI_CMD[@]}" "$@" 2>&1) || EXIT_CODE=$?
}

assert_success_with_output() {
    local name="$1" pattern="$2"
    if [[ $EXIT_CODE -eq 0 ]] && echo "$OUTPUT" | grep -Eqi "$pattern"; then
        pass "$name"
    elif [[ $EXIT_CODE -eq 0 ]]; then
        pass_partial "$name" "Succeeded but output missing pattern '$pattern'"
    else
        fail "$name" "exit=$EXIT_CODE, pattern='$pattern'"
    fi
}

assert_error() {
    local name="$1" pattern="$2"
    if [[ $EXIT_CODE -ne 0 ]] && echo "$OUTPUT" | grep -Eqi "$pattern"; then
        pass "$name"
    elif [[ $EXIT_CODE -ne 0 ]]; then
        pass_partial "$name" "Failed as expected but missing pattern '$pattern'"
    else
        fail "$name" "expected error, got exit=0"
    fi
}

assert_empty() {
    local name="$1"
    if [[ $EXIT_CODE -eq 0 ]] && [[ -z "$OUTPUT" || "$(echo "$OUTPUT" | tr -d '[:space:]')" == "" ]]; then
        pass "$name"
    else
        fail "$name" "expected empty output, got: $(echo "$OUTPUT" | head -c 50)"
    fi
}
```

Build a family of assertion helpers (`assert_success_with_output`, `assert_error`, `assert_empty`, etc.) to reduce code duplication and make tests easier to read.

- **Verify assertion helpers actually propagate failures.** If your test defines custom assertion helpers (like `assert_success_with_output` or `assert_error`), verify they actually propagate failures. A broken helper that silently swallows errors will make every test that uses it appear to pass. Test at least one helper with a known-failing case early in the script to confirm failures are detected.

- **Use graduated assertions.** The helpers above use three-tier logic: full credit if both exit code and pattern match, partial credit if the exit code is correct but the output wording differs, and fail only if the exit code is wrong. This captures "functionally correct but differently worded" implementations that deserve some credit.

- **Use multi-alternative patterns.** Different implementations may word the same error differently. Use `grep -Eqi` with `|`-separated alternatives: `"not found|no such|does not exist"`. This is more resilient than matching a single exact phrase.

- **Checking for empty results:** When testing that a list starts empty, use `[[ -z "$OUTPUT" ]]` rather than grep patterns like `'^$'`, which behave unexpectedly on multiline output.

**Common pitfall:** Do NOT use `OUTPUT=$("$@" 2>&1) || true` followed by `EXIT_CODE=$?`. The `|| true` swallows the command's exit code — `$?` will always be 0, making every failure assertion pass silently. Use `|| EXIT_CODE=$?` instead so the real exit code is captured.

Avoid using `eval` to run commands — it introduces quoting issues and makes debugging harder. Pass commands directly.

## Error Case Completeness

The specification defines error conditions. Test **every one**:

- Missing required arguments
- Invalid argument values (wrong type, out of range)
- Operations on nonexistent resources
- Invalid state transitions (e.g., completing an already-completed item)
- Constraint violations (duplicates, mismatched types)

For each error case, verify:
1. The exit code is non-zero
2. The error message contains a relevant keyword (case-insensitive)

## Variant Coverage

When the specification defines a set of valid values (e.g., enum options, size tiers, status filters, command aliases), test **every** value -- not just one. Missing variant coverage is a common gap:

- If the spec defines three valid options for a parameter, test all three
- If the spec defines filterable statuses, test each filter value
- If the spec defines multiple output formats, verify all of them

Read the spec carefully to identify all enumerated values, then ensure your test exercises each one.

## End-to-End Workflows

Individual command tests aren't enough. Test multi-step workflows that chain commands as a real user would:

- **Happy path:** Create resources, perform operations, verify final state.
- **Use real command-generated IDs.** When a command creates a resource and returns an ID, capture that exact ID and pass it to subsequent commands. Never hardcode or fabricate IDs. This catches format-sensitive bugs (e.g., an ID containing characters that interact with shell or CLI argument parsing) that hardcoded test values would miss.
- **Failure path:** Set up conditions that cause an operation to fail, verify the failure is handled correctly and the system state is consistent afterward.
- **Resource lifecycle:** Create, use, verify consumption/state changes, verify persistence across separate CLI invocations.
- **Verify round-trip immediately after creation.** When a `create` command returns an ID, the very next command should use that ID to read, update, or operate on the resource. If a just-created resource comes back as "not found," that is a critical storage bug -- fail the test hard, do not award partial credit. This pattern catches a common class of integration bugs where the service layer and storage layer use mismatched method names or code paths (e.g., `create` writes successfully but `get` looks up via a different method that doesn't exist or reads from a different location).
- **Data persistence:** Write data with one invocation, read it back with another (same data directory) to verify persistence works.
- **Verify state changes quantitatively.** After a command that changes quantities (adding inventory, consuming resources, placing orders), query the current state and verify the numbers changed by the expected amount. Check before and after values -- not just that the list is non-empty. This catches bugs where the command appears to succeed but doesn't actually modify the underlying data, or modifies it by the wrong amount.

## Script Structure

A well-organized test script follows this structure:

1. **Preamble:** `set -uo pipefail`, helper functions, CLI detection. **Do NOT use `set -e`** (see Core Principle 4).
2. **Test infrastructure:** `pass()`, `fail()`, `run()`, assertion helpers, scoring setup
3. **Test sections:** Grouped by feature area, each with its own data directory
4. **Results:** Per-category summary, total score, final exit code (0 if all pass, 1 if any fail)

Keep the script self-contained — no external dependencies beyond the CLI under test and standard Unix tools (`grep`, `mktemp`, `wc`, etc.).

## helpers.sh API Reference

The scaffold's `scripts/blackbox-tests/helpers.sh` provides these functions. All are auto-sourced by `scripts/blackbox-test.sh` -- do NOT run `test-*.sh` files directly.

| Function | Arguments | Description |
|----------|-----------|-------------|
| `register_category` | `cat budget label` (3 required) | Register a scoring category |
| `run_cmd` | `args...` (subcommands only, no `barista` prefix) | Run CLI command, sets `EXIT_CODE` and `OUTPUT` |
| `setup_cmd` | `desc args...` (1+ required) | Like `run_cmd` but aborts on failure |
| `assert_success` | `name [cat]` (1 required, 1 optional) | Pass if `EXIT_CODE == 0` |
| `assert_error` | `name pattern [cat]` (2 required, 1 optional) | Pass if `EXIT_CODE != 0` and output matches pattern |
| `assert_success_with_output` | `name pattern [cat]` (2 required, 1 optional) | Pass if `EXIT_CODE == 0` and output matches pattern |
| `assert_empty` | `name [cat]` (1 required, 1 optional) | Pass if `EXIT_CODE == 0` and output is empty |
| `pass` | `name [cat]` | Record a pass |
| `fail` | `name [reason] [cat]` | Record a failure |
| `pass_partial` | `name [cat] [reason]` | Record partial credit (0.75) |
| `new_data_dir` | (none) | Create isolated data dir, exports `BARISTA_DATA_DIR` |
| `extract_id` | `text` (1 required) | Extract an ID from command output |

The optional `cat` argument must match a category key previously passed to `register_category`. Using an unregistered category will produce an error.
