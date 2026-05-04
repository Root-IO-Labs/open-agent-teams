#!/usr/bin/env bash
# helpers.sh -- Shared test framework for blackbox acceptance tests.
#
# This file is auto-sourced by scripts/blackbox-test.sh before any
# test-*.sh modules.  It provides:
#   - CLI detection (barista / python -m fallbacks)
#   - run_cmd / setup_cmd helpers
#   - pass / fail / pass_partial result reporting
#   - Assertion helpers (assert_success, assert_error, assert_empty, ...)
#   - Data-isolation via new_data_dir()
#   - Per-category scoring and JSON output
#
# Test modules should define functions (e.g. test_inventory_commands)
# that call the helpers below.  Do NOT redefine these functions.

set -uo pipefail
# NOTE: Do NOT use set -e.  Tests intentionally run commands that fail.

# ── Globals ──────────────────────────────────────────────────────────

PASSED=0
PARTIAL_COUNT=0
FAILED=0
TOTAL=0
RESULTS=()

CATEGORIES=""
TMPDIR_BASE=$(mktemp -d)
trap 'rm -rf "$TMPDIR_BASE"' EXIT

# ── Category Management ──────────────────────────────────────────────

register_category() {
    if [[ $# -ne 3 ]]; then
        echo "USAGE ERROR: register_category requires exactly 3 args: register_category <cat> <budget> <label> (got $#)" >&2
        return 1
    fi
    local cat="$1" budget="$2" label="$3"
    CATEGORIES="${CATEGORIES:+$CATEGORIES }${cat}"
    eval "CAT_BUDGET_${cat}=${budget}"
    eval "CAT_LABEL_${cat}=\"${label}\""
    eval "CAT_PASSED_${cat}=0"
    eval "CAT_TOTAL_${cat}=0"
}

_cat_get() { eval "echo \$CAT_${1}_${2}"; }

_cat_add_passed() {
    local cat_name="${1:-}"
    if [[ -z "$cat_name" ]] || ! eval "test -n \"\${CAT_PASSED_${cat_name}+x}\"" 2>/dev/null; then
        echo "USAGE ERROR: _cat_add_passed called with unregistered category '${cat_name}'" >&2
        return 1
    fi
    local current
    current=$(eval "echo \$CAT_PASSED_${cat_name}")
    eval "CAT_PASSED_${cat_name}=\$(awk \"BEGIN { printf \\\"%.2f\\\", $current + $2 }\")"
}

_cat_inc_total() {
    local cat_name="${1:-}"
    if [[ -z "$cat_name" ]] || ! eval "test -n \"\${CAT_TOTAL_${cat_name}+x}\"" 2>/dev/null; then
        echo "USAGE ERROR: _cat_inc_total called with unregistered category '${cat_name}'" >&2
        return 1
    fi
    eval "CAT_TOTAL_${cat_name}=\$(( CAT_TOTAL_${cat_name} + 1 ))"
}

# ── Result Reporting ─────────────────────────────────────────────────

pass() {
    local name="$1"
    local cat="${2:-}"
    PASSED=$((PASSED + 1))
    TOTAL=$((TOTAL + 1))
    RESULTS+=("{\"name\": \"$name\", \"status\": \"pass\"}")
    echo "  PASS: $name"
    if [[ -n "$cat" ]]; then
        _cat_add_passed "$cat" 1
        _cat_inc_total "$cat"
    fi
}

pass_partial() {
    local name="$1"
    local cat="${2:-}"
    local reason="${3:-}"
    PARTIAL_COUNT=$((PARTIAL_COUNT + 1))
    TOTAL=$((TOTAL + 1))
    local reason_escaped
    reason_escaped=$(echo "$reason" | sed 's/"/\\"/g' | tr '\n' ' ')
    RESULTS+=("{\"name\": \"$name\", \"status\": \"partial\", \"reason\": \"$reason_escaped\"}")
    echo "  PARTIAL: $name"
    if [[ -n "$reason" ]]; then
        echo "           $reason"
    fi
    if [[ -n "$cat" ]]; then
        _cat_add_passed "$cat" 0.75
        _cat_inc_total "$cat"
    fi
}

fail() {
    local name="$1"
    local reason="${2:-}"
    local cat="${3:-}"
    FAILED=$((FAILED + 1))
    TOTAL=$((TOTAL + 1))
    local reason_escaped
    reason_escaped=$(echo "$reason" | sed 's/"/\\"/g' | tr '\n' ' ')
    RESULTS+=("{\"name\": \"$name\", \"status\": \"fail\", \"reason\": \"$reason_escaped\"}")
    echo "  FAIL: $name"
    if [[ -n "$reason" ]]; then
        echo "        $reason"
    fi
    if [[ -n "$cat" ]]; then
        _cat_inc_total "$cat"
    fi
}

# ── CLI Detection ────────────────────────────────────────────────────

CLI_CMD=()

detect_cli() {
    if command -v barista &>/dev/null; then
        CLI_CMD=(barista)
    elif python3 -m robotic_barista.cli --help &>/dev/null 2>&1; then
        CLI_CMD=(python3 -m robotic_barista.cli)
    elif python3 -m robotic_barista --help &>/dev/null 2>&1; then
        CLI_CMD=(python3 -m robotic_barista)
    elif [[ -f scripts/run.sh ]]; then
        CLI_CMD=(bash scripts/run.sh)
    else
        echo "FATAL: No CLI entry point found"
        echo "Tried: barista, python3 -m robotic_barista.cli, python3 -m robotic_barista, scripts/run.sh"
        exit 1
    fi
    echo "  CLI detected: ${CLI_CMD[*]}"
}

# ── Command Execution ────────────────────────────────────────────────

EXIT_CODE=0
OUTPUT=""

run_cmd() {
    EXIT_CODE=0
    OUTPUT=$("${CLI_CMD[@]}" "$@" 2>&1) || EXIT_CODE=$?
}

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

# ── Assertion Helpers ────────────────────────────────────────────────

assert_success() {
    if [[ $# -lt 1 ]]; then
        echo "USAGE ERROR: assert_success requires at least 1 arg: assert_success <name> [cat] (got $#)" >&2
        fail "unknown" "assert_success called with no arguments"
        return
    fi
    local name="$1" cat="${2:-}"
    if [[ $EXIT_CODE -eq 0 ]]; then
        pass "$name" "$cat"
    else
        fail "$name" "exit=$EXIT_CODE, output: $(echo "$OUTPUT" | head -c 200)" "$cat"
    fi
}

assert_success_with_output() {
    if [[ $# -lt 2 ]]; then
        echo "USAGE ERROR: assert_success_with_output requires at least 2 args: assert_success_with_output <name> <pattern> [cat] (got $#)" >&2
        fail "${1:-unknown}" "assert_success_with_output called with wrong number of arguments ($# given, 2+ required)"
        return
    fi
    local name="$1" pattern="$2" cat="${3:-}"
    # Pre-flight: reject invalid ERE patterns (exit code 2 = bad regex)
    local _regex_rc=0
    echo "" | grep -E "$pattern" >/dev/null 2>&1 || _regex_rc=$?
    if [[ $_regex_rc -eq 2 ]]; then
        echo "USAGE ERROR: invalid ERE pattern '$pattern' -- if you meant a glob, convert *foo* to foo or .*foo.*" >&2
        fail "$name" "invalid regex pattern: '$pattern'" "$cat"
        return
    fi
    if [[ $EXIT_CODE -eq 0 ]] && echo "$OUTPUT" | grep -Eqi "$pattern"; then
        pass "$name" "$cat"
    elif [[ $EXIT_CODE -eq 0 ]]; then
        pass_partial "$name" "$cat" "Succeeded but output missing pattern '$pattern'"
    else
        fail "$name" "exit=$EXIT_CODE, pattern='$pattern'" "$cat"
    fi
}

assert_error() {
    if [[ $# -lt 2 ]]; then
        echo "USAGE ERROR: assert_error requires at least 2 args: assert_error <name> <pattern> [cat] (got $#)" >&2
        fail "${1:-unknown}" "assert_error called with wrong number of arguments ($# given, 2+ required)"
        return
    fi
    local name="$1" pattern="$2" cat="${3:-}"
    # Pre-flight: reject invalid ERE patterns (exit code 2 = bad regex)
    local _regex_rc=0
    echo "" | grep -E "$pattern" >/dev/null 2>&1 || _regex_rc=$?
    if [[ $_regex_rc -eq 2 ]]; then
        echo "USAGE ERROR: invalid ERE pattern '$pattern' -- if you meant a glob, convert *foo* to foo or .*foo.*" >&2
        fail "$name" "invalid regex pattern: '$pattern'" "$cat"
        return
    fi
    if [[ $EXIT_CODE -ne 0 ]] && echo "$OUTPUT" | grep -Eqi "$pattern"; then
        pass "$name" "$cat"
    elif [[ $EXIT_CODE -ne 0 ]]; then
        pass_partial "$name" "$cat" "Failed as expected but missing pattern '$pattern'"
    else
        fail "$name" "expected error, got exit=0" "$cat"
    fi
}

assert_empty() {
    if [[ $# -lt 1 ]]; then
        echo "USAGE ERROR: assert_empty requires at least 1 arg: assert_empty <name> [cat] (got $#)" >&2
        fail "unknown" "assert_empty called with no arguments"
        return
    fi
    local name="$1" cat="${2:-}"
    if [[ $EXIT_CODE -eq 0 ]] && [[ -z "$OUTPUT" || "$(echo "$OUTPUT" | tr -d '[:space:]')" == "" ]]; then
        pass "$name" "$cat"
    elif [[ $EXIT_CODE -eq 0 ]]; then
        pass_partial "$name" "$cat" "exit=0 but output was not empty: $(echo "$OUTPUT" | head -c 100)"
    else
        fail "$name" "expected empty output, got exit=$EXIT_CODE" "$cat"
    fi
}

# ── Data Isolation ───────────────────────────────────────────────────
#
# WARNING: Do NOT call new_data_dir via command substitution:
#
#   BAD:  data_dir=$(new_data_dir)    # export runs in subshell — BARISTA_DATA_DIR unchanged!
#   GOOD: new_data_dir               # sets BARISTA_DATA_DIR in the current shell
#         local data_dir="$BARISTA_DATA_DIR"  # capture the path if you need it
#
# The function exports BARISTA_DATA_DIR as a side effect.  $() creates a
# subshell, so the export is discarded when the subshell exits and the
# parent shell's BARISTA_DATA_DIR remains unchanged — silently breaking
# data isolation between test groups.

new_data_dir() {
    local dir
    dir=$(mktemp -d "$TMPDIR_BASE/test-XXXXXX")
    export BARISTA_DATA_DIR="$dir"
    echo "$dir"
}

# ── ID Extraction ────────────────────────────────────────────────────

extract_id() {
    local text="$1"
    local id=""
    # Prefixed IDs: alphabetic prefix + hyphen + hex suffix
    id=$(echo "$text" | grep -oE "[a-zA-Z]+-[a-f0-9]{6,}" | head -1 || true)
    if [[ -z "$id" ]]; then
        # Pure hex / UUID-like patterns
        id=$(echo "$text" | grep -oE "[a-f0-9-]{8,}" | head -1 || true)
    fi
    if [[ -z "$id" ]]; then
        id=$(echo "$text" | grep -oE "'[^']+'" | head -1 | tr -d "'" || true)
    fi
    if [[ -z "$id" ]]; then
        id=$(echo "$text" | head -1 | tr -d '[:space:]' || true)
    fi
    echo "$id"
}

# ── Scoring & JSON Output ───────────────────────────────────────────

compute_score() {
    local total_score=0
    SCORE_BREAKDOWN_JSON="{"
    local first=true
    for cat in $CATEGORIES; do
        local p t b earned
        p=$(_cat_get PASSED "$cat")
        t=$(_cat_get TOTAL "$cat")
        b=$(_cat_get BUDGET "$cat")
        earned=0
        if [[ $t -gt 0 ]]; then
            earned=$(awk "BEGIN { printf \"%.1f\", ($p / $t) * $b }")
        fi
        total_score=$(awk "BEGIN { printf \"%.1f\", $total_score + $earned }")
        if [[ "$first" == true ]]; then first=false; else SCORE_BREAKDOWN_JSON+=","; fi
        SCORE_BREAKDOWN_JSON+="\"$cat\":{\"earned\":$earned,\"possible\":$b,\"passed\":$p,\"total\":$t}"
    done
    SCORE_BREAKDOWN_JSON+="}"
    SCORE="$total_score"
}

save_results() {
    local output_file="${1:-results.json}"
    compute_score
    local json="["
    local first=true
    for r in "${RESULTS[@]}"; do
        if $first; then first=false; else json+=","; fi
        json+="$r"
    done
    json+="]"

    local summary
    summary=$(cat <<ENDJSON
{
  "summary": {"total": $TOTAL, "passed": $PASSED, "partial": $PARTIAL_COUNT, "failed": $FAILED},
  "score": $SCORE,
  "score_breakdown": $SCORE_BREAKDOWN_JSON,
  "tests": $json
}
ENDJSON
)
    echo "$summary" > "$output_file"
    echo ""
    echo "  Results saved to: ${output_file}"
}

print_results() {
    compute_score
    echo ""
    echo "============================================"
    echo "  Blackbox Test Results"
    echo "============================================"
    printf "  Score:  %s\n" "$SCORE"
    printf "  Tests:  %d passed, %d partial, %d failed (of %d)\n" "$PASSED" "$PARTIAL_COUNT" "$FAILED" "$TOTAL"
    echo ""
    for cat in $CATEGORIES; do
        local p t b label earned
        p=$(_cat_get PASSED "$cat")
        t=$(_cat_get TOTAL "$cat")
        b=$(_cat_get BUDGET "$cat")
        label=$(_cat_get LABEL "$cat")
        earned=0
        if [[ $t -gt 0 ]]; then
            earned=$(awk "BEGIN { printf \"%.1f\", ($p / $t) * $b }")
        fi
        printf "  %-20s %5s / %d\n" "${label}:" "$earned" "$b"
    done
    echo "============================================"
}
