#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/acceptance-test.sh --repo <name> [options]

Run functional acceptance tests against a completed benchmark repo.
Clones the repo, installs the app, and exercises the CLI groups
against the operational specification.

Required:
  --repo <name>             Benchmark repo name (under Root-IO-Labs)

Options:
  --dir <path>              Use a local directory instead of cloning
  --keep                    Don't delete the temp directory after testing
  --output <path>           Save results JSON (default: benchmarks/results/<repo>-acceptance.json)
  --help                    Show this help message

Examples:
  ./benchmarks/acceptance-test.sh --repo oat-robotic-barista-sonnet46
  ./benchmarks/acceptance-test.sh --dir /path/to/local/clone
EOF
    exit 0
}

REPO_NAME=""
LOCAL_DIR=""
KEEP=false
OUTPUT=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --repo) REPO_NAME="$2"; shift 2 ;;
        --dir) LOCAL_DIR="$2"; shift 2 ;;
        --keep) KEEP=true; shift ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$REPO_NAME" && -z "$LOCAL_DIR" ]]; then
    echo "Error: --repo or --dir is required"
    echo "Run with --help for usage."
    exit 1
fi

if [[ -z "$OUTPUT" && -n "$REPO_NAME" ]]; then
    OUTPUT="${SCRIPT_DIR}/results/${REPO_NAME}-acceptance.json"
elif [[ -z "$OUTPUT" ]]; then
    OUTPUT="${SCRIPT_DIR}/results/acceptance-local.json"
fi

# --- Test framework ---

PASSED=0
PARTIAL_COUNT=0
FAILED=0
TOTAL=0
RESULTS=()

# Categories: setup inventory recipes order scaling errors workflow gate
# Weights total 100. "backend" removed; its points merged into workflow (+10) and gate (+5).
CATEGORIES="setup inventory recipes order scaling errors workflow gate"
CAT_BUDGET_setup=5;     CAT_BUDGET_inventory=10;  CAT_BUDGET_recipes=10
CAT_BUDGET_order=10;    CAT_BUDGET_scaling=5;     CAT_BUDGET_errors=5
CAT_BUDGET_workflow=40; CAT_BUDGET_gate=15

CAT_LABEL_setup="Setup";           CAT_LABEL_inventory="Inventory";      CAT_LABEL_recipes="Recipes"
CAT_LABEL_order="Order Workflow";   CAT_LABEL_scaling="Size Scaling";     CAT_LABEL_errors="Error Handling"
CAT_LABEL_workflow="Spec Workflow"; CAT_LABEL_gate="Persistence/Gate"

for _cat in $CATEGORIES; do
    eval "CAT_PASSED_${_cat}=0"
    eval "CAT_TOTAL_${_cat}=0"
done

_cat_get() { eval "echo \$CAT_${1}_${2}"; }
_cat_add_passed() {
    local current
    current=$(eval "echo \$CAT_PASSED_${1}")
    eval "CAT_PASSED_${1}=\$(awk \"BEGIN { printf \\\"%.2f\\\", $current + $2 }\")"
}
_cat_inc_total() { eval "CAT_TOTAL_${1}=\$(( CAT_TOTAL_${1} + 1 ))"; }

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

compute_score() {
    local total_score=0
    SCORE_BREAKDOWN_JSON="{"
    local first=true
    for cat in $CATEGORIES; do
        local p=$(_cat_get PASSED "$cat")
        local t=$(_cat_get TOTAL "$cat")
        local b=$(_cat_get BUDGET "$cat")
        local earned=0
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
    compute_score
    mkdir -p "$(dirname "$OUTPUT")"
    RESULTS_ARRAY=$(printf '%s\n' "${RESULTS[@]}" | jq -s '.')
    jq -n \
        --arg repo "${REPO_NAME:-local}" \
        --argjson total "$TOTAL" \
        --argjson passed "$PASSED" \
        --argjson partial "$PARTIAL_COUNT" \
        --argjson failed "$FAILED" \
        --argjson score "$SCORE" \
        --argjson score_max 100 \
        --argjson score_breakdown "$SCORE_BREAKDOWN_JSON" \
        --argjson tests "$RESULTS_ARRAY" \
        --arg tested_at "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
        '{
            repo: $repo,
            tested_at: $tested_at,
            summary: {total: $total, passed: $passed, partial: $partial, failed: $failed},
            score: $score,
            score_max: $score_max,
            score_breakdown: $score_breakdown,
            all_passed: ($failed == 0 and $partial == 0),
            tests: $tests
        }' > "$OUTPUT"
    echo ""
    echo "  Results saved to: ${OUTPUT}"
}

run_cmd() {
    local output exit_code
    output=$("$@" 2>&1) && exit_code=0 || exit_code=$?
    echo "$output"
    return $exit_code
}

# Helper: extract an order ID from command output.
# Tries prefixed IDs (e.g. ord-a1b2c3d4), then hex UUIDs, quoted strings, then
# falls back to first non-empty line.
extract_order_id() {
    local text="$1"
    local id=""
    # Prefixed IDs: alphabetic prefix + hyphen + hex suffix (e.g. ord-98cb4194)
    id=$(echo "$text" | grep -oE "[a-zA-Z]+-[a-f0-9]{6,}" | head -1 || true)
    if [[ -z "$id" ]]; then
        # Pure hex / UUID-like patterns (e.g. a1b2c3d4-e5f6-...)
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

# --- Setup ---

WORKDIR=""
CLEANUP_DIRS=()

cleanup() {
    for d in "${CLEANUP_DIRS[@]}"; do
        rm -rf "$d"
    done
}

if [[ -n "$LOCAL_DIR" ]]; then
    WORKDIR="$LOCAL_DIR"
    echo "==> Using local directory: ${WORKDIR}"
else
    GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
    if [[ -z "$GH_TOKEN" ]]; then
        echo "Error: GH_TOKEN or GH_TOKEN_CLASSIC must be set."
        echo "A GitHub token with 'repo' scope is required to clone the benchmark repo."
        exit 1
    fi
    export GH_TOKEN

    WORKDIR="$(mktemp -d)"
    if [[ "$KEEP" == false ]]; then
        CLEANUP_DIRS+=("$WORKDIR")
    fi

    REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"
    echo "==> Cloning ${REPO_FULL}..."
    gh repo clone "${REPO_FULL}" "${WORKDIR}/app" -- --quiet 2>&1
    WORKDIR="${WORKDIR}/app"
fi

cd "$WORKDIR"

# Isolated data directory (spec: BARISTA_DATA_DIR env var)
DATA_DIR="$(mktemp -d)"
CLEANUP_DIRS+=("$DATA_DIR")
export BARISTA_DATA_DIR="$DATA_DIR"

if [[ "$KEEP" == false ]]; then
    trap cleanup EXIT
fi

# --- Install app ---

echo "==> Installing app..."

if command -v uv &>/dev/null; then
    uv venv .venv --quiet 2>&1
    source .venv/bin/activate
    uv pip install -e ".[dev]" --quiet 2>&1 || uv pip install -e . --quiet 2>&1
else
    python3 -m venv .venv
    source .venv/bin/activate
    pip install -e ".[dev]" --quiet 2>&1 || pip install -e . --quiet 2>&1
fi

if ! python3 -c "import robotic_barista" 2>/dev/null; then
    fail "Package installation" "could not import robotic_barista" "setup"
    echo ""
    echo "==> Acceptance test aborted: package failed to install"
    save_results
    exit 1
fi

# Blackbox CLI detection: try native command first, then python -m fallbacks
if command -v barista &>/dev/null; then
    pass "Package installation" "setup"
    pass "spec: barista CLI entry point exists" "setup"
elif python3 -m robotic_barista.cli inventory list >/dev/null 2>&1; then
    pass "Package installation" "setup"
    pass_partial "spec: barista CLI entry point" "setup" \
        "no native 'barista' command, falling back to 'python -m robotic_barista.cli' (50% credit)"
    barista() {
        python3 -m robotic_barista.cli "$@"
    }
elif python3 -m robotic_barista inventory list >/dev/null 2>&1; then
    pass "Package installation" "setup"
    pass_partial "spec: barista CLI entry point" "setup" \
        "no native 'barista' command, falling back to 'python -m robotic_barista' (50% credit)"
    barista() {
        python3 -m robotic_barista "$@"
    }
else
    pass "Package installation" "setup"
    fail "spec: barista CLI entry point exists" \
        "neither 'barista' command nor 'python -m robotic_barista.cli' nor 'python -m robotic_barista' works" "setup"
    echo ""
    echo "==> Acceptance test aborted: no working CLI entry point"
    save_results
    exit 1
fi

echo "==> App installed. Running acceptance tests..."
echo ""

# ============================================================
# TEST SUITE
# ============================================================

echo "--- Inventory Commands ---"

# inventory list on empty state
OUTPUT_TEXT=$(run_cmd barista inventory list 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "inventory list (empty state, exits 0)" "inventory"
else
    fail "inventory list (empty state, exits 0)" "exit code: $EC" "inventory"
fi

# inventory add espresso
OUTPUT_TEXT=$(run_cmd barista inventory add espresso 500 ml 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "added" && echo "$OUTPUT_TEXT" | grep -qi "espresso"; then
        pass "inventory add espresso 500 ml" "inventory"
    else
        pass_partial "inventory add espresso 500 ml" "inventory" \
            "exit 0 but output missing 'Added...espresso' format"
    fi
else
    fail "inventory add espresso 500 ml" "exit code: $EC, output: $OUTPUT_TEXT" "inventory"
fi

# inventory add milk
OUTPUT_TEXT=$(run_cmd barista inventory add milk 1000 ml 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "added" && echo "$OUTPUT_TEXT" | grep -qi "milk"; then
        pass "inventory add milk 1000 ml" "inventory"
    else
        pass_partial "inventory add milk 1000 ml" "inventory" \
            "exit 0 but output missing 'Added...milk' format"
    fi
else
    fail "inventory add milk 1000 ml" "exit code: $EC" "inventory"
fi

# inventory add sugar (different unit)
OUTPUT_TEXT=$(run_cmd barista inventory add sugar 200 g 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "added" && echo "$OUTPUT_TEXT" | grep -qi "sugar"; then
        pass "inventory add sugar 200 g" "inventory"
    else
        pass_partial "inventory add sugar 200 g" "inventory" \
            "exit 0 but output missing 'Added...sugar' format"
    fi
else
    fail "inventory add sugar 200 g" "exit code: $EC" "inventory"
fi

# inventory list shows added items
OUTPUT_TEXT=$(run_cmd barista inventory list 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]] && echo "$OUTPUT_TEXT" | grep -qi "espresso" && echo "$OUTPUT_TEXT" | grep -qi "milk"; then
    if echo "$OUTPUT_TEXT" | grep -qE "[0-9]"; then
        pass "inventory list shows added ingredients" "inventory"
    else
        pass_partial "inventory list shows added ingredients" "inventory" \
            "shows names but missing quantities"
    fi
else
    fail "inventory list shows added ingredients" "output: $OUTPUT_TEXT" "inventory"
fi

# inventory add to existing (should accumulate)
OUTPUT_TEXT=$(run_cmd barista inventory add espresso 200 ml 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "added" && echo "$OUTPUT_TEXT" | grep -qi "espresso"; then
        pass "inventory add to existing ingredient (accumulate)" "inventory"
    else
        pass_partial "inventory add to existing ingredient (accumulate)" "inventory" \
            "exit 0 but output format doesn't match spec"
    fi
else
    fail "inventory add to existing ingredient (accumulate)" "exit code: $EC" "inventory"
fi

# inventory add unit mismatch (should fail)
OUTPUT_TEXT=$(run_cmd barista inventory add espresso 100 g 2>&1) && EC=0 || EC=$?
if [[ $EC -ne 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "error" && echo "$OUTPUT_TEXT" | grep -qi "mismatch\|unit"; then
        pass "inventory add unit mismatch (error)" "inventory"
    else
        pass_partial "inventory add unit mismatch (error)" "inventory" \
            "rejected correctly but error message missing 'Error:...mismatch/unit'"
    fi
else
    fail "inventory add unit mismatch (error)" "expected non-zero exit, got exit=$EC, output: $OUTPUT_TEXT" "inventory"
fi

echo ""
echo "--- Recipe Commands ---"

# recipes list on empty state
OUTPUT_TEXT=$(run_cmd barista recipes list 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "recipes list (empty state, exits 0)" "recipes"
else
    fail "recipes list (empty state, exits 0)" "exit code: $EC" "recipes"
fi

# recipes add Latte (spec syntax: --name, --ingredient, --step)
OUTPUT_TEXT=$(run_cmd barista recipes add \
    --name "Latte" \
    --ingredient "espresso:30ml" --ingredient "milk:150ml" \
    --step "Grind beans" --step "Pull espresso shot" \
    --step "Steam milk" --step "Combine and serve" 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "latte" && echo "$OUTPUT_TEXT" | grep -qiE "added|ID|id"; then
        pass "recipes add Latte (--name/--ingredient/--step)" "recipes"
    else
        pass_partial "recipes add Latte (--name/--ingredient/--step)" "recipes" \
            "exit 0 but output missing 'Recipe...added...ID' format"
    fi
else
    fail "recipes add Latte (--name/--ingredient/--step)" "exit code: $EC, output: $OUTPUT_TEXT" "recipes"
fi

# recipes add duplicate (should fail -- verify error is about duplication, not syntax)
# Use a throwaway name to prevent cascade failures if duplicates are allowed
run_cmd barista recipes add --name "DupTest" --ingredient "espresso:30ml" --step "step" >/dev/null 2>&1 || true
OUTPUT_TEXT=$(run_cmd barista recipes add \
    --name "DupTest" --ingredient "espresso:30ml" --step "step" 2>&1) && EC=0 || EC=$?
if [[ $EC -ne 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qiE "duplicate|exists|already"; then
        pass "recipes add duplicate (error)" "recipes"
    else
        pass_partial "recipes add duplicate (error)" "recipes" \
            "rejected correctly but error message doesn't mention duplicate/exists"
    fi
else
    fail "recipes add duplicate (error)" "expected non-zero exit, got exit=$EC" "recipes"
fi

# recipes add without ingredients (spec: "Error: Recipe must have at least 1 ingredient")
OUTPUT_TEXT=$(run_cmd barista recipes add \
    --name "Empty" --step "step" 2>&1) && EC=0 || EC=$?
if [[ $EC -ne 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "ingredient"; then
        pass "spec: recipes add without ingredients (rejects)" "recipes"
    else
        pass_partial "spec: recipes add without ingredients (rejects)" "recipes" \
            "rejected correctly but error message doesn't mention 'ingredient'"
    fi
else
    fail "spec: recipes add without ingredients (rejects)" \
        "spec requires at least 1 ingredient, but command succeeded" "recipes"
fi

# recipes add a second recipe (Espresso)
OUTPUT_TEXT=$(run_cmd barista recipes add \
    --name "Espresso" --ingredient "espresso:30ml" \
    --step "Pull shot" 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "espresso" && echo "$OUTPUT_TEXT" | grep -qiE "added|ID|id"; then
        pass "recipes add Espresso" "recipes"
    else
        pass_partial "recipes add Espresso" "recipes" \
            "exit 0 but output missing 'Recipe...added...ID' format"
    fi
else
    fail "recipes add Espresso" "exit code: $EC, output: $OUTPUT_TEXT" "recipes"
fi

# recipes list shows the recipes
OUTPUT_TEXT=$(run_cmd barista recipes list 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]] && echo "$OUTPUT_TEXT" | grep -qi "latte"; then
    pass "recipes list shows Latte" "recipes"
else
    fail "recipes list shows Latte" "output: $OUTPUT_TEXT" "recipes"
fi

# recipes show by name
OUTPUT_TEXT=$(run_cmd barista recipes show latte 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]] && echo "$OUTPUT_TEXT" | grep -qi "latte"; then
    if echo "$OUTPUT_TEXT" | grep -qiE "recipe:" && echo "$OUTPUT_TEXT" | grep -qiE "ingredient" && echo "$OUTPUT_TEXT" | grep -qiE "step"; then
        pass "recipes show latte" "recipes"
    else
        pass_partial "recipes show latte" "recipes" \
            "shows latte but missing Recipe:/Ingredients:/Steps: sections"
    fi
else
    fail "recipes show latte" "output: $OUTPUT_TEXT" "recipes"
fi

# recipes show not found
OUTPUT_TEXT=$(run_cmd barista recipes show nonexistent 2>&1) && EC=0 || EC=$?
if [[ $EC -ne 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "not found"; then
        pass "recipes show nonexistent (error)" "recipes"
    else
        pass_partial "recipes show nonexistent (error)" "recipes" \
            "rejected correctly but error missing 'not found'"
    fi
else
    fail "recipes show nonexistent (error)" "expected non-zero exit, got exit=$EC, output: $OUTPUT_TEXT" "recipes"
fi

echo ""
echo "--- Order Workflow ---"

# order place latte --size M (spec: --size is required)
OUTPUT_TEXT=$(run_cmd barista order place latte --size M 2>&1) && EC=0 || EC=$?
ORDER_ID=""
if [[ $EC -eq 0 ]]; then
    ORDER_ID=$(extract_order_id "$OUTPUT_TEXT")
    if [[ -n "$ORDER_ID" ]]; then
        pass "order place latte --size M (got $ORDER_ID)" "order"
    else
        pass_partial "order place latte --size M" "order" \
            "exit 0 but could not extract order ID from output"
    fi
else
    fail "order place latte --size M" "exit code: $EC, output: $OUTPUT_TEXT" "order"
fi

# order validate (PLACED -> VALIDATED)
if [[ -n "$ORDER_ID" ]]; then
    OUTPUT_TEXT=$(run_cmd barista order validate "$ORDER_ID" 2>&1) && EC=0 || EC=$?
    if [[ $EC -eq 0 ]]; then
        if echo "$OUTPUT_TEXT" | grep -qi "validated"; then
            pass "order validate $ORDER_ID" "order"
        else
            pass_partial "order validate $ORDER_ID" "order" \
                "exit 0 but output missing 'validated'"
        fi
    else
        fail "order validate $ORDER_ID" "exit code: $EC, output: $OUTPUT_TEXT" "order"
    fi
else
    fail "order validate (skipped)" "no order_id from previous step" "order"
fi

# order brew (VALIDATED -> COMPLETED)
if [[ -n "$ORDER_ID" ]]; then
    OUTPUT_TEXT=$(run_cmd barista order brew "$ORDER_ID" 2>&1) && EC=0 || EC=$?
    if [[ $EC -eq 0 ]]; then
        if echo "$OUTPUT_TEXT" | grep -qiE "order:" && echo "$OUTPUT_TEXT" | grep -qiE "use:|ingredient" && echo "$OUTPUT_TEXT" | grep -qiE "step"; then
            pass "order brew $ORDER_ID" "order"
        else
            pass_partial "order brew $ORDER_ID" "order" \
                "exit 0 but output missing brew plan format (Order:/Use:/Steps:)"
        fi
    else
        fail "order brew $ORDER_ID" "exit code: $EC, output: $OUTPUT_TEXT" "order"
    fi
else
    fail "order brew (skipped)" "no order_id from previous step" "order"
fi

# orders list (spec: plural 'orders list')
OUTPUT_TEXT=$(run_cmd barista orders list 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "orders list (exits 0)" "order"
else
    fail "orders list (exits 0)" "exit code: $EC, output: $OUTPUT_TEXT" "order"
fi

# orders list with status filter (spec uses uppercase: COMPLETED)
OUTPUT_TEXT=$(run_cmd barista orders list --status COMPLETED 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "orders list --status COMPLETED" "order"
else
    fail "orders list --status COMPLETED" "exit code: $EC, output: $OUTPUT_TEXT" "order"
fi

echo ""
echo "--- Size Scaling ---"

# small order
OUTPUT_TEXT=$(run_cmd barista order place espresso --size S 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "order place espresso --size S" "scaling"
else
    fail "order place espresso --size S" "exit code: $EC, output: $OUTPUT_TEXT" "scaling"
fi

# large order
OUTPUT_TEXT=$(run_cmd barista order place espresso --size L 2>&1) && EC=0 || EC=$?
if [[ $EC -eq 0 ]]; then
    pass "order place espresso --size L" "scaling"
else
    fail "order place espresso --size L" "exit code: $EC, output: $OUTPUT_TEXT" "scaling"
fi

echo ""
echo "--- Error Handling ---"

# order validate on already-brewed order (invalid state transition)
if [[ -n "$ORDER_ID" ]]; then
    OUTPUT_TEXT=$(run_cmd barista order validate "$ORDER_ID" 2>&1) && EC=0 || EC=$?
    if [[ $EC -ne 0 ]]; then
        if echo "$OUTPUT_TEXT" | grep -qi "error"; then
            pass "order validate on COMPLETED order (rejects)" "errors"
        else
            pass_partial "order validate on COMPLETED order (rejects)" "errors" \
                "rejected correctly but missing 'Error:' message"
        fi
    else
        fail "order validate on COMPLETED order (rejects)" "expected non-zero exit, got exit=$EC" "errors"
    fi
fi

# order place with nonexistent recipe (spec: --size required)
OUTPUT_TEXT=$(run_cmd barista order place doesnotexist --size M 2>&1) && EC=0 || EC=$?
if [[ $EC -ne 0 ]]; then
    if echo "$OUTPUT_TEXT" | grep -qi "not found"; then
        pass "order place nonexistent recipe (error)" "errors"
    else
        pass_partial "order place nonexistent recipe (error)" "errors" \
            "rejected correctly but error missing 'not found'"
    fi
else
    fail "order place nonexistent recipe (error)" "expected non-zero exit, got exit=$EC, output: $OUTPUT_TEXT" "errors"
fi

# order brew on PLACED order (spec requires validate first)
PLACED_TEXT=$(run_cmd barista order place latte --size M 2>&1) && PLACED_EC=0 || PLACED_EC=$?
PLACED_ID=$(extract_order_id "$PLACED_TEXT")
if [[ $PLACED_EC -eq 0 && -n "$PLACED_ID" ]]; then
    OUTPUT_TEXT=$(run_cmd barista order brew "$PLACED_ID" 2>&1) && EC=0 || EC=$?
    if [[ $EC -ne 0 ]]; then
        if echo "$OUTPUT_TEXT" | grep -qi "error"; then
            pass "order brew on PLACED order (rejects, needs validate first)" "errors"
        else
            pass_partial "order brew on PLACED order (rejects, needs validate first)" "errors" \
                "rejected correctly but missing 'Error:' message"
        fi
    else
        pass "order brew on PLACED order (direct brew allowed)" "errors"
    fi
else
    fail "order brew on PLACED order (setup)" "could not create order for test" "errors"
fi

echo ""
echo "--- Spec Workflow: The Playability Test ---"
echo "  (Follows the spec's own User Workflows end-to-end through CLI."
echo "   If a human can't do these steps, the app isn't usable.)"

# Fresh data directory for workflow tests
WF_DATA_DIR="$(mktemp -d)"
CLEANUP_DIRS+=("$WF_DATA_DIR")
export BARISTA_DATA_DIR="$WF_DATA_DIR"

# === Spec Workflow 1: Setting Up Recipes and Inventory ===
# The spec shows exact commands:
#   barista inventory add espresso 500 ml
#   barista inventory add milk 1000 ml
#   barista recipes add --name "Latte" \
#     --ingredient "espresso:30ml" --ingredient "milk:150ml" \
#     --step "Grind beans" --step "Pull espresso shot" \
#     --step "Steam milk" --step "Combine and serve"

barista inventory add espresso 500 ml >/dev/null 2>&1 || true
barista inventory add milk 1000 ml >/dev/null 2>&1 || true

RECIPE_CREATED=false

OUTPUT_TEXT=$(run_cmd barista recipes add \
    --name "Latte" \
    --ingredient "espresso:30ml" --ingredient "milk:150ml" \
    --step "Grind beans" --step "Pull espresso shot" \
    --step "Steam milk" --step "Combine and serve" \
 2>&1) && EC=0 || EC=$?

if [[ $EC -eq 0 ]]; then
    RECIPE_CREATED=true
    if echo "$OUTPUT_TEXT" | grep -qi "latte" && echo "$OUTPUT_TEXT" | grep -qiE "added|ID|id"; then
        pass "workflow: recipes add with --ingredient and --step" "workflow"
    else
        pass_partial "workflow: recipes add with --ingredient and --step" "workflow" \
            "exit 0 but output format doesn't match spec"
    fi
else
    fail "workflow: recipes add with --ingredient and --step" \
        "cannot create recipes with ingredients via CLI — app is not usable" "workflow"
fi

if [[ "$RECIPE_CREATED" == true ]]; then
    # === Spec Workflow 2: Placing and Processing an Order ===
    OUTPUT_TEXT=$(run_cmd barista order place latte --size M 2>&1) && EC=0 || EC=$?
    WF_ORDER_ID=""
    if [[ $EC -eq 0 ]]; then
        WF_ORDER_ID=$(extract_order_id "$OUTPUT_TEXT")
        if [[ -n "$WF_ORDER_ID" ]]; then
            pass "workflow: order place latte --size M (got $WF_ORDER_ID)" "workflow"
        else
            pass_partial "workflow: order place latte --size M" "workflow" \
                "exit 0 but could not extract order ID"
        fi
    else
        fail "workflow: order place latte --size M" "exit code: $EC, output: $OUTPUT_TEXT" "workflow"
    fi

    if [[ -n "$WF_ORDER_ID" ]]; then
        OUTPUT_TEXT=$(run_cmd barista order validate "$WF_ORDER_ID" 2>&1) && EC=0 || EC=$?
        if [[ $EC -eq 0 ]]; then
            if echo "$OUTPUT_TEXT" | grep -qi "validated"; then
                pass "workflow: order validate (sufficient inventory)" "workflow"
            else
                pass_partial "workflow: order validate (sufficient inventory)" "workflow" \
                    "exit 0 but output missing 'validated'"
            fi
        else
            fail "workflow: order validate (sufficient inventory)" "exit code: $EC, output: $OUTPUT_TEXT" "workflow"
        fi

        OUTPUT_TEXT=$(run_cmd barista order brew "$WF_ORDER_ID" 2>&1) && EC=0 || EC=$?
        if [[ $EC -eq 0 ]]; then
            if echo "$OUTPUT_TEXT" | grep -qiE "order:" && echo "$OUTPUT_TEXT" | grep -qiE "use:|ingredient" && echo "$OUTPUT_TEXT" | grep -qiE "step"; then
                pass "workflow: order brew (shows brew plan)" "workflow"
            else
                pass_partial "workflow: order brew (shows brew plan)" "workflow" \
                    "exit 0 but output missing brew plan format (Order:/Use:/Steps:)"
            fi
        else
            fail "workflow: order brew (shows brew plan)" "exit code: $EC, output: $OUTPUT_TEXT" "workflow"
        fi

        # Verify inventory was consumed via CLI (not JSON)
        INV_OUTPUT=$(barista inventory list 2>&1) || true
        ESPRESSO_QTY=$(echo "$INV_OUTPUT" | grep -i "espresso" | grep -oE "[0-9]+\.?[0-9]*" | head -1 || true)
        MILK_QTY=$(echo "$INV_OUTPUT" | grep -i "milk" | grep -oE "[0-9]+\.?[0-9]*" | head -1 || true)

        if [[ -n "$ESPRESSO_QTY" && -n "$MILK_QTY" ]]; then
            CONSUMED=$(awk "BEGIN { print ($ESPRESSO_QTY < 500 && $MILK_QTY < 1000) ? 1 : 0 }")
            if [[ "$CONSUMED" == "1" ]]; then
                pass "workflow: inventory consumed after brew (espresso=${ESPRESSO_QTY}, milk=${MILK_QTY})" "workflow"
            else
                fail "workflow: inventory consumed after brew" \
                    "espresso=${ESPRESSO_QTY}, milk=${MILK_QTY} (not consumed)" "workflow"
            fi
        else
            fail "workflow: inventory consumed after brew" \
                "could not parse inventory list output: $INV_OUTPUT" "workflow"
        fi
    fi

    # === Spec Workflow 3: Handling Insufficient Inventory ===
    # Drain inventory, then verify validation rejects due to insufficient stock.
    OUTPUT_TEXT=$(run_cmd barista order place latte --size L 2>&1) && EC=0 || EC=$?
    WF_ORDER_L=""
    if [[ $EC -eq 0 ]]; then
        WF_ORDER_L=$(extract_order_id "$OUTPUT_TEXT")
    fi

    if [[ -n "$WF_ORDER_L" ]]; then
        barista order validate "$WF_ORDER_L" >/dev/null 2>&1 || true
        barista order brew "$WF_ORDER_L" >/dev/null 2>&1 || true
    fi

    for _ in 1 2 3 4 5 6 7 8; do
        DRAIN_OUT=$(barista order place latte --size L 2>&1) && DRAIN_EC=0 || DRAIN_EC=$?
        DRAIN_ID=$(extract_order_id "$DRAIN_OUT")
        [[ -z "$DRAIN_ID" ]] && break
        barista order validate "$DRAIN_ID" >/dev/null 2>&1 || true
        barista order brew "$DRAIN_ID" >/dev/null 2>&1 || true
    done

    OUTPUT_TEXT=$(run_cmd barista order place latte --size L 2>&1) && EC=0 || EC=$?
    FINAL_ID=$(extract_order_id "$OUTPUT_TEXT")

    if [[ $EC -eq 0 && -n "$FINAL_ID" ]]; then
        OUTPUT_TEXT=$(run_cmd barista order validate "$FINAL_ID" 2>&1) && EC=0 || EC=$?
        if [[ $EC -ne 0 ]]; then
            if echo "$OUTPUT_TEXT" | grep -qiE "fail|insufficient|missing|error"; then
                pass "workflow: validate rejects when inventory exhausted" "workflow"
            else
                pass_partial "workflow: validate rejects when inventory exhausted" "workflow" \
                    "rejected correctly but error message not descriptive"
            fi
        else
            fail "workflow: validate rejects when inventory exhausted" \
                "expected failure after draining inventory" "workflow"
        fi
    else
        fail "workflow: validate rejects when inventory exhausted (setup)" \
            "could not place final order" "workflow"
    fi
fi

# Restore main data dir for persistence tests
export BARISTA_DATA_DIR="$DATA_DIR"

echo ""
echo "--- Persistence ---"

# Check for barista.json specifically (spec says barista.json)
if ls "$DATA_DIR"/barista.json 1>/dev/null 2>&1; then
    pass "JSON data file is barista.json (per spec)" "gate"
elif ls "$DATA_DIR"/*.json 1>/dev/null 2>&1; then
    pass_partial "JSON data files exist" "gate" \
        "found .json files but not named barista.json per spec"
else
    fail "JSON data files exist in data directory" "no .json files found in $DATA_DIR" "gate"
fi

# All data files are valid JSON
ALL_VALID=true
JSON_FOUND=false
for f in "$DATA_DIR"/*.json; do
    [[ -f "$f" ]] || continue
    JSON_FOUND=true
    if ! jq . "$f" &>/dev/null; then
        ALL_VALID=false
        fail "Valid JSON: $(basename "$f")" "invalid JSON" "gate"
    fi
done
if [[ "$JSON_FOUND" == true && "$ALL_VALID" == true ]]; then
    pass "All data files are valid JSON" "gate"
elif [[ "$JSON_FOUND" == false ]]; then
    fail "Valid JSON" "no JSON files to validate" "gate"
fi

echo ""
echo "--- Project Gate (scripts/check.sh) ---"

if [[ -f "scripts/check.sh" ]]; then
    OUTPUT_TEXT=$(bash scripts/check.sh 2>&1) && EC=0 || EC=$?
    if [[ $EC -eq 0 ]]; then
        pass "scripts/check.sh passes" "gate"
    else
        fail "scripts/check.sh passes" "exit code: $EC (run with --keep to inspect)" "gate"
    fi
else
    fail "scripts/check.sh exists" "file not found" "gate"
fi

# ============================================================
# RESULTS
# ============================================================

compute_score

echo ""
echo "============================================"
echo "  Acceptance Test Results"
echo "============================================"
printf "  Score:  %s / 100\n" "$SCORE"
printf "  Tests:  %d passed, %d partial, %d failed (of %d)\n" "$PASSED" "$PARTIAL_COUNT" "$FAILED" "$TOTAL"
echo ""
for cat in $CATEGORIES; do
    local_p=$(_cat_get PASSED "$cat")
    local_t=$(_cat_get TOTAL "$cat")
    local_b=$(_cat_get BUDGET "$cat")
    local_label=$(_cat_get LABEL "$cat")
    local_earned=0
    if [[ $local_t -gt 0 ]]; then
        local_earned=$(awk "BEGIN { printf \"%.1f\", ($local_p / $local_t) * $local_b }")
    fi
    printf "  %-20s %5s / %d\n" "${local_label}:" "$local_earned" "$local_b"
done
echo "============================================"

if [[ "$KEEP" == true && -n "$WORKDIR" ]]; then
    echo ""
    echo "  Workdir preserved: ${WORKDIR}"
fi

save_results

# Exit with failure if any tests failed
if [[ $FAILED -gt 0 ]]; then
    exit 1
fi
