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
  --judge-model <model>     Claude model for judging (default: claude-sonnet-4-6)
  --threshold <score>       Pass/fail threshold 0-100 (default: 70)
  --help                    Show this help message

Environment:
  ANTHROPIC_API_KEY         Required for the LLM judge
EOF
    exit 0
}

GENERATED=""
REFERENCE=""
OUTPUT=""
JUDGE_MODEL="claude-sonnet-4-6"
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

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    echo "Error: ANTHROPIC_API_KEY is not set"
    exit 1
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
log "Judge model: ${JUDGE_MODEL}"
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

# Build the API payload
PAYLOAD=$(jq -n \
    --arg model "$JUDGE_MODEL" \
    --arg system "$RUBRIC" \
    --arg user "$USER_PROMPT" \
    '{
        model: $model,
        max_tokens: 4096,
        system: $system,
        messages: [{role: "user", content: $user}]
    }')

log "Calling Claude API..."

RESPONSE=""
CURL_EXIT=0
MAX_ATTEMPTS=3
ATTEMPT=1
BACKOFF=5

while [[ $ATTEMPT -le $MAX_ATTEMPTS ]]; do
    RESPONSE=$(curl -sS --max-time 120 \
        https://api.anthropic.com/v1/messages \
        -H "x-api-key: ${ANTHROPIC_API_KEY}" \
        -H "anthropic-version: 2023-06-01" \
        -H "content-type: application/json" \
        -d "$PAYLOAD" 2>&1) && CURL_EXIT=0 || CURL_EXIT=$?

    if [[ $CURL_EXIT -eq 0 && -n "$RESPONSE" ]]; then
        break
    fi
    if [[ $ATTEMPT -lt $MAX_ATTEMPTS ]]; then
        log "Attempt ${ATTEMPT}/${MAX_ATTEMPTS} failed. Retrying in ${BACKOFF}s..."
        sleep "$BACKOFF"
        BACKOFF=$((BACKOFF * 2))
    fi
    ATTEMPT=$((ATTEMPT + 1))
done

if [[ $CURL_EXIT -ne 0 ]]; then
    log "Error: Claude API call failed after ${MAX_ATTEMPTS} attempts"
    exit 1
fi

# Check for API errors
ERROR_TYPE=$(echo "$RESPONSE" | jq -r '.error.type // empty' 2>/dev/null || true)
if [[ -n "$ERROR_TYPE" ]]; then
    ERROR_MSG=$(echo "$RESPONSE" | jq -r '.error.message // "unknown"' 2>/dev/null || true)
    log "Error: Claude API error: ${ERROR_TYPE}: ${ERROR_MSG}"
    exit 1
fi

# Extract the judge's response
JUDGE_TEXT=$(echo "$RESPONSE" | jq -r '.content[0].text // empty' 2>/dev/null || true)

if [[ -z "$JUDGE_TEXT" ]]; then
    log "Error: Empty response from Claude API"
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

# Build gate.json
jq -n \
    --arg model "$JUDGE_MODEL" \
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

INPUT_TOKENS=$(echo "$RESPONSE" | jq -r '.usage.input_tokens // "?"' 2>/dev/null || true)
OUTPUT_TOKENS=$(echo "$RESPONSE" | jq -r '.usage.output_tokens // "?"' 2>/dev/null || true)
log "API tokens: ${INPUT_TOKENS} input, ${OUTPUT_TOKENS} output"

exit 0
