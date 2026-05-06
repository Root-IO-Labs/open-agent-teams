#!/usr/bin/env bash
set -euo pipefail

# Run an A/B comparison: single-model baseline vs multi-model routing.
#
# Usage:
#   ./benchmarks/run-routing-comparison.sh \
#     --model anthropic:claude-sonnet-4-6 \
#     --available-worker-models "anthropic:claude-sonnet-4-6,google_genai:gemini-2.5-flash,openai:o4-mini"
#
# This runs two full benchmark legs sequentially:
#   Leg A (baseline): single model for all workers
#   Leg B (routing):  supervisor picks from available models
#
# Results are written to benchmarks/results/routing-comparison-<timestamp>/

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/run-routing-comparison.sh --model <model> --available-worker-models <csv> [options]

Required:
  --model <model>             Orchestrator model (used for both legs)
  --available-worker-models <csv>  Comma-separated models for workers in routing leg

Options:
  --wave-timeout <min>        Max minutes per wave (default: 30)
  --timeout <min>             Max total minutes per leg (default: 240)
  --skip-gate                 Skip blackbox gate for both legs
  --help                      Show this help message

The comparison runs two sequential benchmark legs and generates a comparison report.
EOF
    exit 0
}

MODEL=""
AVAILABLE_MODELS=""
EXTRA_ARGS=()

while [[ $# -gt 0 ]]; do
    case $1 in
        --model) MODEL="$2"; shift 2 ;;
        --available-worker-models|--available-models) AVAILABLE_MODELS="$2"; shift 2 ;;
        --wave-timeout|--timeout|--gate-threshold|--gate-timeout)
            EXTRA_ARGS+=("$1" "$2"); shift 2 ;;
        --skip-gate|--skip-convergence)
            EXTRA_ARGS+=("$1"); shift ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; exit 1 ;;
    esac
done

if [[ -z "$MODEL" || -z "$AVAILABLE_MODELS" ]]; then
    echo "Error: --model and --available-worker-models are both required"
    echo "Run with --help for usage."
    exit 1
fi

TS=$(date +%Y%m%d-%H%M%S)
COMP_DIR="${SCRIPT_DIR}/results/routing-comparison-${TS}"
mkdir -p "$COMP_DIR"

log() { echo "[$(date +%H:%M:%S)] $*"; }

log "=== Routing A/B Comparison ==="
log "Supervisor model: ${MODEL}"
log "Available models: ${AVAILABLE_MODELS}"
log "Results dir:      ${COMP_DIR}"
echo ""

# --- Leg A: Baseline (single model) ---

log "===== LEG A: BASELINE (single model: ${MODEL}) ====="

"${SCRIPT_DIR}/run.sh" \
    --model "$MODEL" \
    --name "routing-baseline-${TS}" \
    "${EXTRA_ARGS[@]}" \
    2>&1 | tee "${COMP_DIR}/baseline-output.log"

# Find the baseline results dir
BASELINE_DIR=$(ls -td "${SCRIPT_DIR}/results/"*"routing-baseline-${TS}"* 2>/dev/null | head -1)
if [[ -n "$BASELINE_DIR" ]]; then
    ln -sf "$BASELINE_DIR" "${COMP_DIR}/baseline"
    log "Baseline results: ${BASELINE_DIR}"
else
    log "WARNING: Could not find baseline results directory"
fi

echo ""
log "===== LEG B: ROUTING (multi-model) ====="

"${SCRIPT_DIR}/run.sh" \
    --model "$MODEL" \
    --routing-mode \
    --available-worker-models "$AVAILABLE_MODELS" \
    --name "routing-multi-${TS}" \
    "${EXTRA_ARGS[@]}" \
    2>&1 | tee "${COMP_DIR}/routing-output.log"

# Find the routing results dir
ROUTING_DIR=$(ls -td "${SCRIPT_DIR}/results/"*"routing-multi-${TS}"* 2>/dev/null | head -1)
if [[ -n "$ROUTING_DIR" ]]; then
    ln -sf "$ROUTING_DIR" "${COMP_DIR}/routing"
    log "Routing results: ${ROUTING_DIR}"
else
    log "WARNING: Could not find routing results directory"
fi

# --- Generate comparison report ---

BASELINE_COLLECT="${BASELINE_DIR}/collect.json"
ROUTING_COLLECT="${ROUTING_DIR}/collect.json"

if [[ -f "$BASELINE_COLLECT" && -f "$ROUTING_COLLECT" ]]; then
    log "Generating comparison report..."
    "${SCRIPT_DIR}/compare-routing.sh" \
        --baseline "$BASELINE_COLLECT" \
        --routing "$ROUTING_COLLECT" \
        --output "${COMP_DIR}/comparison.md"
    log "Report: ${COMP_DIR}/comparison.md"
else
    log "WARNING: Missing collect.json files — skipping comparison report"
    [[ ! -f "$BASELINE_COLLECT" ]] && log "  Missing: ${BASELINE_COLLECT}"
    [[ ! -f "$ROUTING_COLLECT" ]] && log "  Missing: ${ROUTING_COLLECT}"
fi

log ""
log "=== Comparison complete ==="
log "Results: ${COMP_DIR}"
