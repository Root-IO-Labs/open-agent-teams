#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    echo "Usage: benchmarks/judge-cursor-gate.sh --model <name>"
    echo ""
    echo "  --model <name>   Short model name (e.g., gemini31pro, haiku45, gpt53codex)"
    echo "  --help            Show this help"
    echo ""
    echo "Run from the open-agent-teams repo root after a Cursor gate test."
    exit 0
}

MODEL_NAME=""
while [[ $# -gt 0 ]]; do
    case $1 in
        --model) MODEL_NAME="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; usage ;;
    esac
done

if [[ -z "$MODEL_NAME" ]]; then
    echo "Error: --model is required"
    usage
fi

BENCH_REPO="$HOME/cursor-bench-repo"
GENERATED="$BENCH_REPO/scripts/blackbox-test.sh"

if [[ ! -f "$GENERATED" ]]; then
    echo "Error: $GENERATED not found"
    echo "Has the Cursor model finished writing the test?"
    exit 1
fi

TIMESTAMP=$(date -u +"%Y%m%d-%H%M%S")
RESULTS_DIR="${SCRIPT_DIR}/results/${TIMESTAMP}-${MODEL_NAME}-cursor-gate"
mkdir -p "$RESULTS_DIR"

echo "==> Copying generated test to $RESULTS_DIR/"
cp "$GENERATED" "$RESULTS_DIR/gate-generated-test.sh"
chmod +x "$RESULTS_DIR/gate-generated-test.sh"
echo "    $(wc -l < "$RESULTS_DIR/gate-generated-test.sh" | tr -d ' ') lines"

echo ""
echo "==> Running judge..."
"${SCRIPT_DIR}/judge-blackbox.sh" \
    --generated "$RESULTS_DIR/gate-generated-test.sh" \
    --reference "${SCRIPT_DIR}/acceptance-test.sh" \
    --output "$RESULTS_DIR/gate.json"

echo ""
echo "==> Results:"
jq '{score, verdict, score_breakdown}' "$RESULTS_DIR/gate.json"
echo ""
echo "Full results: $RESULTS_DIR/gate.json"
