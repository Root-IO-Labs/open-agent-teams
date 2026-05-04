#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/run-blackbox.sh --test <path> --repo <name> --output <path> [options]

Shim wrapper: run a model-generated blackbox test against a completed
benchmark repo and produce a structured JSON result.

Required:
  --test <path>             Path to the model-generated blackbox-test.sh
  --repo <name>             Benchmark repo name to test against

Options:
  --dir <path>              Use a local directory instead of cloning
  --output <path>           Output JSON path (default: <results-dir>/blackbox-acceptance.json)
  --smoke                   Smoke mode: use a stub CLI if detection fails (for gate-time testing before app is built)
  --keep                    Don't delete temp directory after testing
  --help                    Show this help message
EOF
    exit 0
}

TEST_SCRIPT=""
REPO_NAME=""
LOCAL_DIR=""
OUTPUT=""
SMOKE=false
KEEP=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --test) TEST_SCRIPT="$2"; shift 2 ;;
        --repo) REPO_NAME="$2"; shift 2 ;;
        --dir) LOCAL_DIR="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --smoke) SMOKE=true; shift ;;
        --keep) KEEP=true; shift ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$TEST_SCRIPT" ]]; then
    echo "Error: --test is required"
    exit 1
fi

if [[ -z "$REPO_NAME" && -z "$LOCAL_DIR" ]]; then
    echo "Error: --repo or --dir is required"
    exit 1
fi

if [[ ! -f "$TEST_SCRIPT" ]]; then
    echo "Error: Test script not found: ${TEST_SCRIPT}"
    exit 1
fi

if [[ -z "$OUTPUT" ]]; then
    OUTPUT="${SCRIPT_DIR}/results/blackbox-acceptance.json"
fi

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

CLEANUP_DIRS=()

cleanup() {
    for d in "${CLEANUP_DIRS[@]}"; do
        rm -rf "$d"
    done
}

if [[ "$KEEP" == false ]]; then
    trap cleanup EXIT
fi

# =============================================================================
# Setup: Clone or use local repo
# =============================================================================

WORKDIR=""

if [[ -n "$LOCAL_DIR" ]]; then
    WORKDIR="$LOCAL_DIR"
    log "Using local directory: ${WORKDIR}"
else
    GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
    if [[ -z "$GH_TOKEN" ]]; then
        echo "Error: GH_TOKEN or GH_TOKEN_CLASSIC must be set."
        echo "A GitHub token with 'repo' scope is required to clone the benchmark repo."
        exit 1
    fi
    export GH_TOKEN

    REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"
    WORKDIR="$(mktemp -d)"
    if [[ "$KEEP" == false ]]; then
        CLEANUP_DIRS+=("$WORKDIR")
    fi

    log "Cloning ${REPO_FULL}..."
    gh repo clone "${REPO_FULL}" "${WORKDIR}/app" -- --quiet 2>&1
    WORKDIR="${WORKDIR}/app"
fi

cd "$WORKDIR"

# Isolated data directory
DATA_DIR="$(mktemp -d)"
CLEANUP_DIRS+=("$DATA_DIR")
export BARISTA_DATA_DIR="$DATA_DIR"

# =============================================================================
# Install the app
# =============================================================================

log "Installing app..."

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
    log "Error: Package failed to install"
    jq -n \
        --arg source "model-generated" \
        --argjson exit_code 1 \
        --argjson passed 0 \
        --argjson failed 0 \
        --arg raw_output "Package installation failed" \
        --arg score_estimate "0%" \
        '{source: $source, exit_code: $exit_code, passed: $passed, failed: $failed, raw_output: $raw_output, score_estimate: $score_estimate}' \
        > "$OUTPUT"
    exit 1
fi

# Detect CLI entry point (same logic as acceptance-test.sh)
if command -v barista &>/dev/null; then
    log "Using native 'barista' command"
elif python3 -m robotic_barista.cli inventory list >/dev/null 2>&1; then
    log "Using 'python -m robotic_barista.cli' fallback"
    barista() { python3 -m robotic_barista.cli "$@"; }
elif python3 -m robotic_barista inventory list >/dev/null 2>&1; then
    log "Using 'python -m robotic_barista' fallback"
    barista() { python3 -m robotic_barista "$@"; }
elif [[ "$SMOKE" == true ]]; then
    log "Smoke mode: using stub CLI (app not built yet)"
    barista() { echo "STUB: barista $*" >&2; return 1; }
    export -f barista
else
    log "Error: No working CLI entry point"
    jq -n \
        --arg source "model-generated" \
        --argjson exit_code 1 \
        --argjson passed 0 \
        --argjson failed 0 \
        --arg raw_output "No working CLI entry point found" \
        --arg score_estimate "0%" \
        '{source: $source, exit_code: $exit_code, passed: $passed, failed: $failed, raw_output: $raw_output, score_estimate: $score_estimate}' \
        > "$OUTPUT"
    exit 1
fi

# =============================================================================
# Run the model-generated test
# =============================================================================

log "Running model-generated test: ${TEST_SCRIPT}"
log "Working directory: ${WORKDIR}"
log "Data directory: ${DATA_DIR}"

# Copy the test script to a temp location and make it executable
TEST_COPY="$(mktemp)"
cp "$TEST_SCRIPT" "$TEST_COPY"
chmod +x "$TEST_COPY"

# Find a bash 4+ to handle declare -A (associative arrays) that LLMs commonly
# generate. macOS ships bash 3.2; homebrew installs bash 5+ to /opt/homebrew/bin.
BASH_BIN="bash"
for candidate in /opt/homebrew/bin/bash /usr/local/bin/bash; do
    if [[ -x "$candidate" ]]; then
        candidate_ver=$("$candidate" -c 'echo ${BASH_VERSINFO[0]}' 2>/dev/null || echo "0")
        if [[ "$candidate_ver" -ge 4 ]]; then
            BASH_BIN="$candidate"
            break
        fi
    fi
done
log "Using bash: ${BASH_BIN} ($(${BASH_BIN} --version | head -1))"

# Capture stdout+stderr and exit code
RAW_OUTPUT=""
TEST_EXIT=0
RAW_OUTPUT=$("$BASH_BIN" "$TEST_COPY" 2>&1) && TEST_EXIT=0 || TEST_EXIT=$?

rm -f "$TEST_COPY"

log "Test exited with code: ${TEST_EXIT}"

# =============================================================================
# Parse results
# =============================================================================

# Count PASS/FAIL lines (case-insensitive, flexible patterns)
PASS_COUNT=$(echo "$RAW_OUTPUT" | grep -ciE '^\s*(\[?\s*PASS|✓|✅|ok\b)' || true)
FAIL_COUNT=$(echo "$RAW_OUTPUT" | grep -ciE '^\s*(\[?\s*FAIL|✗|✘|❌|not ok\b)' || true)

# Also try common test output patterns (match anywhere in line)
if [[ "${PASS_COUNT:-0}" -eq 0 && "${FAIL_COUNT:-0}" -eq 0 ]]; then
    PASS_COUNT=$(echo "$RAW_OUTPUT" | grep -ciE '\bPASS(ED)?\b' || true)
    FAIL_COUNT=$(echo "$RAW_OUTPUT" | grep -ciE '\bFAIL(ED)?\b' || true)
fi

PASS_COUNT=${PASS_COUNT:-0}
FAIL_COUNT=${FAIL_COUNT:-0}
TOTAL_TESTS=$((PASS_COUNT + FAIL_COUNT))
if [[ $TOTAL_TESTS -gt 0 ]]; then
    SCORE_PCT=$(awk "BEGIN { printf \"%.0f%%\", ($PASS_COUNT / $TOTAL_TESTS) * 100 }")
else
    SCORE_PCT="0%"
fi

FATAL_COUNT=$(echo "$RAW_OUTPUT" | grep -ciE '\b(FATAL|ABORT)\b' || true)
FATAL_COUNT=${FATAL_COUNT:-0}
FATAL_LINES=""
if [[ "$FATAL_COUNT" -gt 0 ]]; then
    FATAL_LINES=$(echo "$RAW_OUTPUT" | grep -iE '\b(FATAL|ABORT)\b' | head -20)
fi

if [[ "$FATAL_COUNT" -gt 0 ]]; then
    log "Parsed results: ${PASS_COUNT} passed, ${FAIL_COUNT} failed (${FATAL_COUNT} FATAL aborts) (${SCORE_PCT})"
else
    log "Parsed results: ${PASS_COUNT} passed, ${FAIL_COUNT} failed (${SCORE_PCT})"
fi

# If the test exited 0 but produced zero results, it likely crashed silently
# (e.g. bash version incompatibility, unbound variable with set -u).
# Treat this as a failure so the convergence loop doesn't declare false success.
if [[ "$TEST_EXIT" -eq 0 && "$TOTAL_TESTS" -eq 0 ]]; then
    log "WARNING: Test exited 0 but produced no PASS/FAIL results -- treating as failure"
    TEST_EXIT=1
fi

# Truncate raw output for JSON (keep under 50KB)
TRUNCATED_OUTPUT="$RAW_OUTPUT"
if [[ ${#RAW_OUTPUT} -gt 50000 ]]; then
    TRUNCATED_OUTPUT="${RAW_OUTPUT:0:25000}

... (output truncated, ${#RAW_OUTPUT} chars total) ...

${RAW_OUTPUT: -25000}"
fi

# Build result JSON
mkdir -p "$(dirname "$OUTPUT")"
jq -n \
    --arg source "model-generated" \
    --argjson exit_code "$TEST_EXIT" \
    --argjson passed "$PASS_COUNT" \
    --argjson failed "$FAIL_COUNT" \
    --argjson fatal_count "$FATAL_COUNT" \
    --arg fatal_lines "$FATAL_LINES" \
    --arg raw_output "$TRUNCATED_OUTPUT" \
    --arg score_estimate "$SCORE_PCT" \
    '{
        source: $source,
        exit_code: $exit_code,
        passed: $passed,
        failed: $failed,
        fatal_count: $fatal_count,
        fatal_lines: $fatal_lines,
        raw_output: $raw_output,
        score_estimate: $score_estimate
    }' > "$OUTPUT"

log ""
echo "  =========================================="
echo "    Blackbox Test Results"
echo "  =========================================="
echo "    Exit code:    ${TEST_EXIT}"
echo "    Passed:       ${PASS_COUNT}"
echo "    Failed:       ${FAIL_COUNT}"
if [[ "$FATAL_COUNT" -gt 0 ]]; then
echo "    FATAL aborts: ${FATAL_COUNT}"
echo "    FATAL reason: $(echo "$FATAL_LINES" | head -1)"
fi
echo "    Score est.:   ${SCORE_PCT}"
echo "  =========================================="
log ""
log "Results saved to: ${OUTPUT}"
