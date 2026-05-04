#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/judge-blackbox.sh --generated <path> --reference <path> --output <path> [options]

Use an LLM judge to structurally compare a model-generated blackbox test
against the human-written reference acceptance-test.sh.

Required:
  --generated <path>        Path to model-generated blackbox-test.sh
  --reference <path>        Path to reference acceptance-test.sh
  --output <path>           Path to write gate.json results

Options:
  --judge-model <provider:model>
                            Judge model. Resolution order:
                              1. --judge-model flag (this option)
                              2. OAT_BENCH_LLM_MODEL env var
                              3. anthropic:claude-sonnet-4-6 (hard fallback)
                            run.sh passes the orchestrator --model here so
                            the gate uses the same provider you're testing
                            (no surprise charges from a different provider).
                            Accepts any provider:model string OAT supports.
                            Note: LLM judges differ in strictness — pin
                            --judge-model when comparing scores across
                            orchestrators.
  --threshold <score>       Pass/fail threshold 0-100 (default: 70)
  --help                    Show this help message

Environment:
  OAT_BENCH_LLM_MODEL       Optional fallback model used when --judge-model
                            is absent.
  <PROVIDER>_API_KEY        API key for whichever provider the resolved
                            judge model uses (ANTHROPIC_API_KEY /
                            OPENAI_API_KEY / GOOGLE_API_KEY /
                            OPENROUTER_API_KEY / etc.). Local providers
                            (ollama:) need no key.
EOF
    exit 0
}

GENERATED=""
REFERENCE=""
OUTPUT=""
JUDGE_MODEL=""
THRESHOLD=70

while [[ $# -gt 0 ]]; do
    case $1 in
        --generated) GENERATED="$2"; shift 2 ;;
        --reference) REFERENCE="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --judge-model) JUDGE_MODEL="$2"; shift 2 ;;
        --threshold) THRESHOLD="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$GENERATED" || -z "$REFERENCE" || -z "$OUTPUT" ]]; then
    echo "Error: --generated, --reference, and --output are all required"
    exit 1
fi

# Resolve the judge model. Provider key checking happens inside llm_call.py
# so we don't need an upfront ANTHROPIC_API_KEY hard-fail here — the helper
# emits a clear, provider-aware "missing FOO_API_KEY" error.
RESOLVED_JUDGE_MODEL=""
if [[ -n "$JUDGE_MODEL" ]]; then
    RESOLVED_JUDGE_MODEL="$JUDGE_MODEL"
elif [[ -n "${OAT_BENCH_LLM_MODEL:-}" ]]; then
    RESOLVED_JUDGE_MODEL="$OAT_BENCH_LLM_MODEL"
else
    RESOLVED_JUDGE_MODEL="anthropic:claude-sonnet-4-6"
fi

if [[ ! -f "$GENERATED" ]]; then
    echo "Error: Generated test not found: ${GENERATED}"
    exit 1
fi

if [[ ! -f "$REFERENCE" ]]; then
    echo "Error: Reference test not found: ${REFERENCE}"
    exit 1
fi

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

log "Judging model-generated test against reference"
log "Generated: ${GENERATED} ($(wc -l < "$GENERATED" | tr -d ' ') lines)"
log "Reference: ${REFERENCE} ($(wc -l < "$REFERENCE" | tr -d ' ') lines)"
log "Judge model: ${RESOLVED_JUDGE_MODEL}"
log "Threshold: ${THRESHOLD}/100"

GENERATED_CONTENT=$(cat "$GENERATED")
REFERENCE_CONTENT=$(cat "$REFERENCE")

JUDGE_START=$(date +%s)

RUBRIC='You are an expert test quality judge. Compare a model-generated blackbox acceptance test against a human-written reference test for a CLI application called "robotic-barista."

Score the generated test across four dimensions (each 0-25, total 0-100):

## Feature Coverage (0-25)
Does the generated test cover all CLI command groups?
- inventory commands (list, add, accumulate, unit mismatch)
- recipes commands (list, add, show, duplicate detection, validation)
- order commands (place with --size, validate, brew)
- orders commands (list, status filter)

## Error Handling (0-25)
Does the generated test verify error cases?
- Unit mismatch on inventory add
- Duplicate recipe name
- Recipe without ingredients
- Nonexistent recipe in order place
- Invalid state transitions (validate on completed, brew on placed)
- Nonexistent order IDs

## Workflow Coverage (0-25)
Does the generated test verify end-to-end workflows?
- Full order lifecycle: place -> validate -> brew
- Inventory consumption after brewing
- Insufficient inventory rejection
- Size scaling (S, M, L)
- Data isolation via BARISTA_DATA_DIR

## Test Rigor (0-25)
Is the test well-structured and reliable?
- Proper exit code checking
- Output validation (not just exit codes)
- Data isolation between test sections
- Scoring/summary system
- Clear PASS/FAIL output per test case
- Self-contained (no external dependencies)

Respond with ONLY a JSON object (no markdown, no explanation outside the JSON):
{
  "score": <total 0-100>,
  "feature_coverage": <0-25>,
  "error_handling": <0-25>,
  "workflow_coverage": <0-25>,
  "test_rigor": <0-25>,
  "analysis": "<2-3 paragraph analysis explaining the scores, what the generated test does well, and what it misses compared to the reference>"
}'

USER_PROMPT="## Reference Test (human-written acceptance-test.sh)

\`\`\`bash
${REFERENCE_CONTENT}
\`\`\`

## Model-Generated Test (blackbox-test.sh)

\`\`\`bash
${GENERATED_CONTENT}
\`\`\`

Score the model-generated test according to the rubric. Respond with ONLY the JSON object."

# Build the helper payload. llm_call.py wraps any langchain provider so the
# judge works with the orchestrator's model regardless of vendor.
PAYLOAD_FILE=$(mktemp)
trap "rm -f '$PAYLOAD_FILE'" EXIT

jq -n \
    --arg system "$RUBRIC" \
    --arg user "$USER_PROMPT" \
    '{
        system: $system,
        messages: [{role: "user", content: $user}],
        max_tokens: 4096
    }' > "$PAYLOAD_FILE"

log "Calling LLM judge via llm_call.py..."

HELPER_RESPONSE=""
HELPER_EXIT=0
HELPER_RESPONSE=$(python3 "${SCRIPT_DIR}/llm_call.py" \
    --model "$RESOLVED_JUDGE_MODEL" \
    --payload "$PAYLOAD_FILE") || HELPER_EXIT=$?

if [[ $HELPER_EXIT -ne 0 ]]; then
    case $HELPER_EXIT in
        2) log "Error: LLM helper missing API key for the judge provider. See stderr above." ;;
        3) log "Error: LLM helper provider call failed after retries. See stderr above." ;;
        4) log "Error: LLM helper failed to resolve judge model. See stderr above." ;;
        *) log "Error: LLM helper exited with code ${HELPER_EXIT}. See stderr above." ;;
    esac
    exit 1
fi

if [[ -z "$HELPER_RESPONSE" ]]; then
    log "Error: LLM helper returned no JSON output"
    exit 1
fi

# Extract the judge's response text and the resolved provider:model string
# (used below in gate.json so a bare 'claude-sonnet-4-6' is recorded as
# 'anthropic:claude-sonnet-4-6').
JUDGE_TEXT=$(echo "$HELPER_RESPONSE" | jq -r '.text // empty')
RESOLVED_FROM_HELPER=$(echo "$HELPER_RESPONSE" | jq -r '.model // empty')
if [[ -n "$RESOLVED_FROM_HELPER" ]]; then
    RESOLVED_JUDGE_MODEL="$RESOLVED_FROM_HELPER"
fi
INPUT_TOKENS=$(echo "$HELPER_RESPONSE" | jq -r '.input_tokens // 0')
OUTPUT_TOKENS=$(echo "$HELPER_RESPONSE" | jq -r '.output_tokens // 0')

if [[ -z "$JUDGE_TEXT" ]]; then
    log "Error: Empty text from LLM helper"
    exit 1
fi

# Parse the JSON from the judge's response (strip any markdown fences)
JUDGE_JSON=$(echo "$JUDGE_TEXT" | sed 's/^```json//; s/^```//; s/```$//' | jq '.' 2>/dev/null || true)

if [[ -z "$JUDGE_JSON" ]]; then
    log "Warning: Could not parse judge response as JSON, attempting extraction..."
    JUDGE_JSON=$(python3 -c "
import json, sys
text = sys.stdin.read()
start = text.find('{')
end = text.rfind('}')
if start >= 0 and end > start:
    print(json.dumps(json.loads(text[start:end+1])))
" <<< "$JUDGE_TEXT" 2>/dev/null || true)
fi

if [[ -z "$JUDGE_JSON" ]]; then
    log "Warning: JSON parse failed, attempting regex extraction from truncated response..."
    _score=$(echo "$JUDGE_TEXT" | grep -oE '"score"\s*:\s*[0-9]+' | head -1 | grep -oE '[0-9]+$' || true)
    _fc=$(echo "$JUDGE_TEXT" | grep -oE '"feature_coverage"\s*:\s*[0-9]+' | head -1 | grep -oE '[0-9]+$' || true)
    _eh=$(echo "$JUDGE_TEXT" | grep -oE '"error_handling"\s*:\s*[0-9]+' | head -1 | grep -oE '[0-9]+$' || true)
    _wc=$(echo "$JUDGE_TEXT" | grep -oE '"workflow_coverage"\s*:\s*[0-9]+' | head -1 | grep -oE '[0-9]+$' || true)
    _tr=$(echo "$JUDGE_TEXT" | grep -oE '"test_rigor"\s*:\s*[0-9]+' | head -1 | grep -oE '[0-9]+$' || true)

    if [[ -n "$_score" && -n "$_fc" && -n "$_eh" && -n "$_wc" && -n "$_tr" ]]; then
        _analysis=$(python3 -c "
import sys, re
text = sys.stdin.read()
m = re.search(r'\"analysis\"\s*:\s*\"((?:[^\"\\\\]|\\\\.)*)\"?', text)
print(m.group(1)[:500] if m else 'Analysis truncated in API response')
" <<< "$JUDGE_TEXT" 2>/dev/null || echo "Analysis truncated in API response")
        JUDGE_JSON=$(jq -n \
            --argjson score "$_score" \
            --argjson feature_coverage "$_fc" \
            --argjson error_handling "$_eh" \
            --argjson workflow_coverage "$_wc" \
            --argjson test_rigor "$_tr" \
            --arg analysis "$_analysis" \
            '{score: $score, feature_coverage: $feature_coverage, error_handling: $error_handling, workflow_coverage: $workflow_coverage, test_rigor: $test_rigor, analysis: $analysis}')
        log "Regex extraction successful: score=${_score}, fc=${_fc}, eh=${_eh}, wc=${_wc}, tr=${_tr}"
    fi
fi

if [[ -z "$JUDGE_JSON" ]]; then
    log "Error: Failed to parse judge response"
    log "Raw response: ${JUDGE_TEXT:0:500}"
    exit 1
fi

JUDGE_END=$(date +%s)
DURATION=$((JUDGE_END - JUDGE_START))

SCORE=$(echo "$JUDGE_JSON" | jq -r '.score // 0')
FEATURE_COVERAGE=$(echo "$JUDGE_JSON" | jq -r '.feature_coverage // 0')
ERROR_HANDLING=$(echo "$JUDGE_JSON" | jq -r '.error_handling // 0')
WORKFLOW_COVERAGE=$(echo "$JUDGE_JSON" | jq -r '.workflow_coverage // 0')
TEST_RIGOR=$(echo "$JUDGE_JSON" | jq -r '.test_rigor // 0')
ANALYSIS=$(echo "$JUDGE_JSON" | jq -r '.analysis // "No analysis provided"')

if [[ "$SCORE" -ge "$THRESHOLD" ]]; then
    VERDICT="pass"
    PROCEEDED=true
else
    VERDICT="fail"
    PROCEEDED=false
fi

# Build gate.json — record the resolved provider:model string so future
# readers can tell exactly which model produced the verdict (a bare
# 'claude-sonnet-4-6' becomes 'anthropic:claude-sonnet-4-6').
jq -n \
    --arg model "$RESOLVED_JUDGE_MODEL" \
    --arg verdict "$VERDICT" \
    --argjson score "$SCORE" \
    --argjson threshold "$THRESHOLD" \
    --argjson feature_coverage "$FEATURE_COVERAGE" \
    --argjson error_handling "$ERROR_HANDLING" \
    --argjson workflow_coverage "$WORKFLOW_COVERAGE" \
    --argjson test_rigor "$TEST_RIGOR" \
    --arg analysis "$ANALYSIS" \
    --argjson duration_seconds "$DURATION" \
    --argjson proceeded_to_benchmark "$PROCEEDED" \
    '{
        model: $model,
        verdict: $verdict,
        score: $score,
        threshold: $threshold,
        score_breakdown: {
            feature_coverage: $feature_coverage,
            error_handling: $error_handling,
            workflow_coverage: $workflow_coverage,
            test_rigor: $test_rigor
        },
        analysis: $analysis,
        duration_seconds: $duration_seconds,
        proceeded_to_benchmark: $proceeded_to_benchmark
    }' > "$OUTPUT"

echo ""
echo "=========================================="
echo "  Blackbox Gate Judgment"
echo "=========================================="
echo "  Score:     ${SCORE} / 100 (threshold: ${THRESHOLD})"
VERDICT_UPPER=$(echo "$VERDICT" | tr '[:lower:]' '[:upper:]')
echo "  Verdict:   ${VERDICT_UPPER}"
echo "  Breakdown:"
echo "    Feature Coverage:  ${FEATURE_COVERAGE}/25"
echo "    Error Handling:    ${ERROR_HANDLING}/25"
echo "    Workflow Coverage: ${WORKFLOW_COVERAGE}/25"
echo "    Test Rigor:        ${TEST_RIGOR}/25"
echo "  Duration:  ${DURATION}s"
echo "=========================================="
echo ""
log "Results saved to: ${OUTPUT}"

log "API tokens: ${INPUT_TOKENS} input, ${OUTPUT_TOKENS} output"

exit 0
