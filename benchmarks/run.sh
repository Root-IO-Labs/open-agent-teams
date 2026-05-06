#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=./lib.sh
source "${SCRIPT_DIR}/lib.sh"

# Cache the expected wave composition from issues.json once at startup so the
# per-tick count_total_wave_issues() helper (called ~240 times per benchmark
# from wait_for_wave) is a pure indirect global lookup, no jq re-parse.
# expected_wave_count() returns "0" for unknown labels / missing JSON, so this
# is safe for custom benchmarks that don't ship an issues.json.
EXPECTED_WAVE_0=$(expected_wave_count "wave:0")
EXPECTED_WAVE_1=$(expected_wave_count "wave:1")
EXPECTED_WAVE_2=$(expected_wave_count "wave:2")
EXPECTED_WAVE_3=$(expected_wave_count "wave:3")
EXPECTED_WAVE_4=$(expected_wave_count "wave:4")

GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/run.sh --model <model> [options]

Run a fully automated benchmark: setup repo, drive wave progression, collect results.

Required:
  --model <model>           LLM model to benchmark

Options:
  --name <suffix>           Repo name suffix (default: timestamp)
  --worker-model <model>    Different model for workers (default: same as --model)
  --wave-timeout <min>      Max minutes per wave (default: 30)
  --nudge-timeout <min>     Minutes before sending soft nudge (default: 12)
  --timeout <min>           Max total minutes (default: 240)
  --poll-interval <sec>     Seconds between completion polls (default: 120)
  --skip-setup              Skip setup.sh, use existing repo (requires --repo)
  --repo <name>             Existing repo name (with --skip-setup)
  --skip-collect            Skip collect.sh at the end
  --skip-acceptance         Skip acceptance-test.sh at the end
  --skip-summary            Skip summarize.sh at the end
  --skip-gate               Skip the blackbox test gate
  --gate-threshold <score>  Pass/fail threshold for gate (default: 70)
  --gate-only               Run only the gate, skip waves even if gate passes
  --gate-timeout <min>      Max minutes for gate worker (default: 30)
  --judge-model <model>     Model for the LLM gate judge (default: tracks --model
                            so the gate uses the same provider you're testing).
                            Override to pin a fixed judge across runs when
                            comparing scores between orchestrators.
  --summary-model <model>   Model for the post-run LLM summary (default:
                            tracks --model so the summary uses the same
                            provider you're testing).
  --skip-convergence        Skip the post-wave convergence loop
  --convergence-timeout <min>  Max total minutes for convergence loop (default: 60)
  --convergence-iter-timeout <min>  Max minutes per convergence iteration (default: 30)
  --routing-mode             Enable supervisor-driven model routing (multi-model)
  --available-worker-models <csv>  Comma-separated models for workers (requires --routing-mode)
  --available-models <csv>   Alias for --available-worker-models (backward compat)
  --help                    Show this help message

Examples:
  ./benchmarks/run.sh --model claude-sonnet-4-6
  ./benchmarks/run.sh --model claude-sonnet-4-6 --name baseline --wave-timeout 60
  ./benchmarks/run.sh --model gemini-2.5-pro --worker-model gemini-2.5-pro --name gemini
  ./benchmarks/run.sh --skip-setup --repo oat-robotic-barista-mytest
EOF
    exit 0
}

MODEL=""
WORKER_MODEL=""
NAME_SUFFIX=""
WAVE_TIMEOUT=30
NUDGE_TIMEOUT=12
GRAND_TIMEOUT=240
POLL_INTERVAL=120
SKIP_SETUP=false
SKIP_COLLECT=false
SKIP_ACCEPTANCE=false
SKIP_GATE=false
GATE_THRESHOLD=70
GATE_ONLY=false
GATE_TIMEOUT=30
# Empty until after arg parsing so we can default to $MODEL (the orchestrator
# model) rather than hard-coding an Anthropic SKU. Avoids surprise charges
# when the user runs --model openai:... but also has ANTHROPIC_API_KEY set.
JUDGE_MODEL=""
SUMMARY_MODEL=""
SKIP_CONVERGENCE=false
CONVERGENCE_TIMEOUT=60
CONVERGENCE_ITER_TIMEOUT=30
REPO_NAME=""
ROUTING_MODE=false
AVAILABLE_MODELS=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --model) MODEL="$2"; shift 2 ;;
        --worker-model) WORKER_MODEL="$2"; shift 2 ;;
        --name) NAME_SUFFIX="$2"; shift 2 ;;
        --wave-timeout) WAVE_TIMEOUT="$2"; shift 2 ;;
        --nudge-timeout) NUDGE_TIMEOUT="$2"; shift 2 ;;
        --timeout) GRAND_TIMEOUT="$2"; shift 2 ;;
        --poll-interval) POLL_INTERVAL="$2"; shift 2 ;;
        --skip-setup) SKIP_SETUP=true; shift ;;
        --skip-collect) SKIP_COLLECT=true; shift ;;
        --skip-acceptance) SKIP_ACCEPTANCE=true; shift ;;
        --skip-summary) SKIP_SUMMARY=true; shift ;;
        --skip-gate) SKIP_GATE=true; shift ;;
        --gate-threshold) GATE_THRESHOLD="$2"; shift 2 ;;
        --gate-only) GATE_ONLY=true; shift ;;
        --gate-timeout) GATE_TIMEOUT="$2"; shift 2 ;;
        --judge-model) JUDGE_MODEL="$2"; shift 2 ;;
        --summary-model) SUMMARY_MODEL="$2"; shift 2 ;;
        --skip-convergence) SKIP_CONVERGENCE=true; shift ;;
        --convergence-timeout) CONVERGENCE_TIMEOUT="$2"; shift 2 ;;
        --convergence-iter-timeout) CONVERGENCE_ITER_TIMEOUT="$2"; shift 2 ;;
        --repo) REPO_NAME="$2"; shift 2 ;;
        --routing-mode) ROUTING_MODE=true; shift ;;
        --available-worker-models|--available-models) AVAILABLE_MODELS="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ "$SKIP_SETUP" == false && -z "$MODEL" ]]; then
    echo "Error: --model is required (unless using --skip-setup)"
    echo "Run with --help for usage."
    exit 1
fi

if [[ "$SKIP_SETUP" == true && -z "$REPO_NAME" ]]; then
    echo "Error: --repo is required when using --skip-setup"
    echo "Run with --help for usage."
    exit 1
fi

if [[ "$ROUTING_MODE" == true && -z "$AVAILABLE_MODELS" ]]; then
    echo "Error: --available-worker-models is required when using --routing-mode"
    echo "Run with --help for usage."
    exit 1
fi

# Default judge / summary models to the orchestrator model so the gate and
# the post-run summary use the same provider the user is testing. This
# prevents surprise Anthropic (or any other) charges when running --model
# against a different provider with multiple keys present in the env.
if [[ -z "$JUDGE_MODEL" ]]; then
    if [[ -n "$MODEL" ]]; then
        JUDGE_MODEL="$MODEL"
    else
        # --skip-setup path with no --model; preserve historical behavior
        # by falling back to Sonnet so existing automation doesn't break.
        JUDGE_MODEL="anthropic:claude-sonnet-4-6"
    fi
fi
if [[ -z "$SUMMARY_MODEL" ]]; then
    if [[ -n "$MODEL" ]]; then
        SUMMARY_MODEL="$MODEL"
    else
        SUMMARY_MODEL="anthropic:claude-sonnet-4-6"
    fi
fi

# Set NAME_SUFFIX default so we can create RUN_DIR and capture terminal output from the start
if [[ -z "$NAME_SUFFIX" ]]; then
    if [[ "$SKIP_SETUP" == true ]]; then
        NAME_SUFFIX="${REPO_NAME#oat-robotic-barista-}"
    else
        NAME_SUFFIX="$(date +%s)"
    fi
fi

SKIP_SUMMARY="${SKIP_SUMMARY:-false}"

WAVE_TIMEOUT_SECS=$((WAVE_TIMEOUT * 60))
NUDGE_TIMEOUT_SECS=$((NUDGE_TIMEOUT * 60))
GRAND_TIMEOUT_SECS=$((GRAND_TIMEOUT * 60))
CONVERGENCE_TIMEOUT_SECS=$((CONVERGENCE_TIMEOUT * 60))
CONVERGENCE_ITER_TIMEOUT_SECS=$((CONVERGENCE_ITER_TIMEOUT * 60))
MAX_CONVERGENCE_ITERS=4
RUN_START=$(date +%s)
RUN_TIMESTAMP=$(date -u +"%Y%m%d-%H%M%S")

# Create results directory and tee all output so the full run is captured
RUN_DIR="${SCRIPT_DIR}/results/${RUN_TIMESTAMP}-${NAME_SUFFIX}"
mkdir -p "$RUN_DIR"
exec > >(tee "$RUN_DIR/terminal-output.txt") 2>&1

# --- Cleanup on exit: stop agents to prevent token burn on crash/failure ---

cleanup() {
    if [[ -n "${REPO_NAME:-}" ]]; then
        echo ""
        echo "[cleanup] Stopping all agents for ${REPO_NAME}..."
        oat repo hibernate --repo "$REPO_NAME" --all --yes 2>/dev/null || true
        echo "[cleanup] Agents stopped (logs preserved at ~/.oat/output/${REPO_NAME}/)"
    fi
}
trap cleanup EXIT

# --- Preflight checks ---

GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
if [[ -z "$GH_TOKEN" ]]; then
    echo "Error: GH_TOKEN or GH_TOKEN_CLASSIC must be set."
    echo "A GitHub token with 'repo' scope is required for benchmark operations."
    exit 1
fi

REQUIRED_CMDS=(gh jq oat)
for cmd in "${REQUIRED_CMDS[@]}"; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: '$cmd' is required but not found."
        exit 1
    fi
done

export GH_TOKEN

# =============================================================================
# Helper functions
# =============================================================================

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

format_duration() {
    local seconds=$1
    local hours=$((seconds / 3600))
    local minutes=$(( (seconds % 3600) / 60 ))
    if [[ $hours -gt 0 ]]; then
        echo "${hours}h ${minutes}m"
    else
        echo "${minutes}m"
    fi
}

grand_timeout_reached() {
    local now elapsed
    now=$(date +%s)
    elapsed=$((now - RUN_START))
    [[ $elapsed -ge $GRAND_TIMEOUT_SECS ]]
}

send_to_agent() {
    local repo_name="$1"
    local agent_name="$2"
    local message="$3"
    local max_retries=3
    local attempt=0

    while [[ $attempt -lt $max_retries ]]; do
        attempt=$((attempt + 1))
        if oat agent tell "$agent_name" "$message" --repo "$repo_name" >/dev/null 2>&1; then
            return 0
        fi
        sleep 3
    done
    return 1
}

send_to_workspace() {
    local repo_name="$1"
    local message="$2"

    log "Sending message to workspace agent..."
    if send_to_agent "$repo_name" "default" "$message"; then
        log "  Message delivered"
        return 0
    fi
    log "  Error: failed to deliver message after retries"
    return 1
}

send_wave_coordination() {
    local repo_name="$1"
    local wave_msg="$2"
    local supervisor_msg="$3"

    log "Sending wave to workspace and supervisor..."

    # Fire both in parallel; retry only the failed leg
    send_to_agent "$repo_name" "default" "$wave_msg" &
    local ws_pid=$!
    send_to_agent "$repo_name" "supervisor" "$supervisor_msg" &
    local sv_pid=$!

    local ws_ok=true sv_ok=true
    wait $ws_pid || ws_ok=false
    wait $sv_pid || sv_ok=false

    if $ws_ok; then
        log "  Workspace message delivered"
    else
        log "  Warning: workspace message failed after retries"
    fi
    if $sv_ok; then
        log "  Supervisor coordination delivered"
    else
        log "  Warning: supervisor coordination failed after retries"
    fi

    # Workspace message is critical; supervisor is best-effort
    $ws_ok
}

# Assemble modular blackbox test files into a single script for judging/snapshots.
# Downloads scripts/blackbox-tests/*.sh + scripts/blackbox-test.sh from the repo,
# concatenates in deterministic order (helpers first, test modules alphabetically,
# entry point last), and saves to $output_file.
# Falls back to extracting just scripts/blackbox-test.sh if modular directory missing.
assemble_gate_test() {
    local repo_full="$1"
    local output_file="$2"
    local tmpdir
    tmpdir=$(mktemp -d)

    log "Assembling gate test from repo modules..."

    # Try to list modular test directory
    local dir_listing
    dir_listing=$(gh api "repos/${repo_full}/contents/scripts/blackbox-tests" --jq '.[].name' 2>/dev/null || echo "")

    if [[ -n "$dir_listing" ]]; then
        # Download each .sh file from the modular directory
        local file_count=0
        local helpers_file=""
        local module_files=()

        while IFS= read -r filename; do
            [[ "$filename" == *.sh ]] || continue
            local content
            content=$(gh api "repos/${repo_full}/contents/scripts/blackbox-tests/${filename}" --jq '.content' 2>/dev/null | base64 -d 2>/dev/null || true)
            if [[ -n "$content" ]]; then
                echo "$content" > "${tmpdir}/${filename}"
                file_count=$((file_count + 1))
                if [[ "$filename" == "helpers.sh" ]]; then
                    helpers_file="${tmpdir}/${filename}"
                elif [[ "$filename" == test-*.sh ]]; then
                    module_files+=("${tmpdir}/${filename}")
                fi
            fi
        done <<< "$dir_listing"

        # Download the entry point
        local entry_content
        entry_content=$(gh api "repos/${repo_full}/contents/scripts/blackbox-test.sh" --jq '.content' 2>/dev/null | base64 -d 2>/dev/null || true)

        if [[ $file_count -gt 0 ]]; then
            # Assemble: shebang + comment + helpers + modules (sorted) + entry point.
            # Bash 3.2 + set -u: empty-array expansions like "${module_files[@]}"
            # raise "unbound variable", so guard with ${#arr[@]} before any [@]
            # expansion. This path can hit zero test-*.sh modules when the repo
            # has only helpers.sh (e.g., the pre-release-sanity-check fixture).
            {
                echo "#!/usr/bin/env bash"
                echo "# Assembled from modular blackbox test files ($(date -u +"%Y-%m-%dT%H:%M:%SZ"))"
                if [[ ${#module_files[@]} -gt 0 ]]; then
                    echo "# Files: helpers.sh $(printf '%s ' "${module_files[@]##*/}")blackbox-test.sh"
                else
                    echo "# Files: helpers.sh blackbox-test.sh"
                fi
                echo ""

                if [[ -n "$helpers_file" && -f "$helpers_file" ]]; then
                    echo "# === helpers.sh ==="
                    # Strip any shebang from helpers since we already have one
                    sed '1{/^#!/d;}' "$helpers_file"
                    echo ""
                fi

                # Sort module files for deterministic ordering. Build the
                # sorted_modules array via a portable read-loop (avoids both
                # `read -d ''`/mapfile bashisms and the empty-array unbound
                # expansion that the previous one-liner triggered).
                local sorted_modules=()
                if [[ ${#module_files[@]} -gt 0 ]]; then
                    while IFS= read -r mod; do
                        [[ -n "$mod" ]] && sorted_modules+=("$mod")
                    done < <(printf '%s\n' "${module_files[@]}" | sort)
                fi
                if [[ ${#sorted_modules[@]} -gt 0 ]]; then
                    for mod in "${sorted_modules[@]}"; do
                        [[ -f "$mod" ]] || continue
                        echo "# === $(basename "$mod") ==="
                        sed '1{/^#!/d;}' "$mod"
                        echo ""
                    done
                fi

                # Instead of appending the raw entry point (which has source
                # commands for external files that don't exist in assembled mode),
                # write an inline runner. All helpers and test_* functions are
                # already defined by the inlined content above.
                echo "# === runner (assembled mode -- replaces blackbox-test.sh entry point) ==="
                echo 'SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"'
                echo ""
                echo "detect_cli"
                echo ""
                echo 'echo ""'
                echo 'echo "============================================"'
                echo 'echo "  Blackbox Acceptance Tests"'
                echo 'echo "============================================"'
                echo 'echo ""'
                echo ""
                echo "FUNC_COUNT=0"
                echo 'for func in $(declare -F | awk '"'"'{print $3}'"'"' | grep '"'"'^test_'"'"' | sort); do'
                echo '    echo "--- ${func} ---"'
                echo '    "$func"'
                echo '    echo ""'
                echo '    FUNC_COUNT=$((FUNC_COUNT + 1))'
                echo "done"
                echo ""
                echo 'if [[ $FUNC_COUNT -eq 0 ]]; then'
                echo '    echo "WARNING: No test_* functions found. Modules may not define runnable tests."'
                echo '    exit 1'
                echo "fi"
                echo ""
                echo "print_results"
                echo 'save_results "${SCRIPT_DIR}/../blackbox-results.json"'
                echo ""
                echo 'if [[ $FAILED -gt 0 ]]; then'
                echo "    exit 1"
                echo "fi"
            } > "$output_file"

            chmod +x "$output_file"
            log "Assembled ${file_count} module(s) + entry point into gate test ($(wc -l < "$output_file" | tr -d ' ') lines)"
            rm -rf "$tmpdir"
            return 0
        fi
    fi

    # Fallback: extract just scripts/blackbox-test.sh as a single file
    log "No modular directory found, falling back to single-file extraction..."
    gh api "repos/${repo_full}/contents/scripts/blackbox-test.sh" --jq '.content' 2>/dev/null | base64 -d > "$output_file" 2>/dev/null || {
        log "API download failed, trying clone..."
        local clone_dir
        clone_dir="$(mktemp -d)"
        gh repo clone "${repo_full}" "${clone_dir}/repo" -- --quiet --depth 1 2>&1
        if [[ -f "${clone_dir}/repo/scripts/blackbox-test.sh" ]]; then
            cp "${clone_dir}/repo/scripts/blackbox-test.sh" "$output_file"
        else
            rm -rf "$clone_dir" "$tmpdir"
            return 1
        fi
        rm -rf "$clone_dir"
    }

    if [[ -s "$output_file" ]]; then
        chmod +x "$output_file"
        log "Extracted single-file gate test ($(wc -l < "$output_file" | tr -d ' ') lines)"
        rm -rf "$tmpdir"
        return 0
    fi

    rm -rf "$tmpdir"
    return 1
}

count_open_issues() {
    local repo_full="$1"
    local wave_num="$2"
    local result
    result=$(timeout 30 gh issue list --repo "$repo_full" --label "wave:${wave_num}" --state open --json number --jq 'length' 2>/dev/null) || true
    if [[ -z "$result" || ! "$result" =~ ^[0-9]+$ ]]; then
        echo "-1"
    else
        echo "$result"
    fi
}

count_total_wave_issues() {
    local repo_full="$1"
    local wave_num="$2"
    # Live count via gh issue list. May be silently undercounted during a
    # GitHub indexing/search degradation (see Apr 27 2026 incident).
    local live
    live=$(timeout 30 gh issue list --repo "$repo_full" --label "wave:${wave_num}" --state all --json number --jq 'length' 2>/dev/null || echo "0")
    if [[ -z "$live" || ! "$live" =~ ^[0-9]+$ ]]; then
        live=0
    fi
    # Floor to the JSON-derived expected count so wait_for_wave's
    # closed=total-open arithmetic can't false-positive when the live list is
    # missing issues. Indirect expansion with a default keeps this safe under
    # set -u when the benchmark has no issues.json (custom run, expected=0).
    local expected_var="EXPECTED_WAVE_${wave_num}"
    local expected="${!expected_var:-0}"
    if [[ "$expected" -gt "$live" ]]; then
        echo "$expected"
    else
        echo "$live"
    fi
}

has_active_workers() {
    local repo_name="$1"
    local worker_output
    worker_output=$(oat worker list --repo "$repo_name" 2>/dev/null || echo "")
    if echo "$worker_output" | grep -qi "no workers\|No workers"; then
        return 1
    fi
    if [[ -z "$worker_output" ]]; then
        return 1
    fi
    return 0
}

# Wait for a wave to complete. Returns 0 if wave completed, 1 if timed out.
wait_for_wave() {
    local repo_name="$1"
    local repo_full="$2"
    local wave_num="$3"
    local timeout_secs="$4"
    local nudge_secs="$5"

    local wave_start elapsed nudge_sent=false
    wave_start=$(date +%s)

    local total_issues
    total_issues=$(count_total_wave_issues "$repo_full" "$wave_num")
    log "    Wave ${wave_num}: ${total_issues} issues to complete (timeout: $(format_duration "$timeout_secs"))"

    while true; do
        if grand_timeout_reached; then
            log "    Grand timeout reached, stopping wave ${wave_num} poll"
            return 1
        fi

        elapsed=$(( $(date +%s) - wave_start ))

        local open_count
        open_count=$(count_open_issues "$repo_full" "$wave_num")

        if [[ "$open_count" == "-1" ]]; then
            log "    Wave ${wave_num}: gh query failed, retrying... ($(format_duration "$elapsed") elapsed)"
        else
            total_issues=$(count_total_wave_issues "$repo_full" "$wave_num")
            local closed_count=$((total_issues - open_count))
            local workers_active=true
            if ! has_active_workers "$repo_name"; then
                workers_active=false
            fi

            log "    Wave ${wave_num}: ${closed_count}/${total_issues} issues closed, workers active: ${workers_active} ($(format_duration "$elapsed") elapsed)"

            # Wave complete: all issues closed (don't gate on workers --
            # the workspace may have already started next-wave workers)
            if [[ "$open_count" -eq 0 ]]; then
                # Brief grace period for any final PR merges
                log "    All wave ${wave_num} issues closed, waiting 30s for stragglers..."
                sleep 30
                log "    Wave ${wave_num} complete!"
                return 0
            fi
        fi

        # Refresh elapsed after API calls (which can hang for minutes)
        elapsed=$(( $(date +%s) - wave_start ))

        # Soft nudge after nudge timeout (once per wave)
        if [[ $elapsed -ge $nudge_secs && "$nudge_sent" == false ]]; then
            nudge_sent=true
            log "    Soft timeout reached for wave ${wave_num}, sending nudge..."
            send_to_workspace "$repo_name" "[automated benchmark script] Some wave ${wave_num} issues are still open. Please check if any workers are stuck. If a worker needs help, send it a message via oat message send. Do NOT make code changes or fix issues yourself -- only delegate to workers."
        fi

        # Hard timeout (checked with fresh elapsed to catch API call hangs)
        if [[ $elapsed -ge $timeout_secs ]]; then
            log "    Wave ${wave_num} timed out (${open_count} issues still open)"
            return 1
        fi

        sleep "$POLL_INTERVAL"
    done
}

# =============================================================================
# Main flow
# =============================================================================

echo ""
echo "=========================================="
echo "  OAT Automated Benchmark Runner"
echo "=========================================="
echo ""

# --- Step 1: Setup ---

if [[ "$SKIP_SETUP" == false ]]; then
    if [[ -z "$NAME_SUFFIX" ]]; then
        NAME_SUFFIX="$(date +%s)"
    fi
    REPO_NAME="oat-robotic-barista-${NAME_SUFFIX}"

    log "Running setup.sh..."

    SETUP_ARGS=(--model "$MODEL" --name "$NAME_SUFFIX")
    if [[ -n "$WORKER_MODEL" ]]; then
        SETUP_ARGS+=(--worker-model "$WORKER_MODEL")
    fi

    "${SCRIPT_DIR}/setup.sh" "${SETUP_ARGS[@]}"

    log "Setup complete. Repo: ${REPO_NAME}"
else
    log "Skipping setup (using existing repo: ${REPO_NAME})"
    if [[ -z "$MODEL" ]]; then
        MODEL=$(gh repo view "${GITHUB_OWNER}/${REPO_NAME}" --json description --jq '.description' 2>/dev/null | sed 's/^OAT benchmark: //')
    fi
fi

_detect_owner() {
    local repo="$1"
    local state_file="${HOME}/.oat/state.json"
    if [[ -f "$state_file" ]]; then
        local url
        url=$(jq -r --arg r "$repo" '.repos[$r].github_url // empty' "$state_file" 2>/dev/null)
        if [[ -n "$url" ]]; then
            echo "$url" | sed -E 's|.*/([^/]+)/[^/]+$|\1|'
            return 0
        fi
    fi
    return 1
}
GITHUB_OWNER=$(_detect_owner "$REPO_NAME" 2>/dev/null || echo "$GITHUB_OWNER")

REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"

# Clean stale output logs from previous runs with the same repo name
if [[ -d "${HOME}/.oat/output/${REPO_NAME}/workers" ]]; then
    log "Cleaning stale output logs from previous run..."
    rm -rf "${HOME}/.oat/output/${REPO_NAME}/workers/"
fi

# Onboard models for routing mode
if [[ "$ROUTING_MODE" == true ]]; then
    log "Routing mode: onboarding worker models..."
    IFS=',' read -ra ROUTING_MODELS <<< "$AVAILABLE_MODELS"
    ONBOARD_FAILURES=0
    for m in "${ROUTING_MODELS[@]}"; do
        m=$(echo "$m" | xargs)  # trim whitespace
        log "  Onboarding: $m"
        if ! oat model onboard "$m" --probe-set minimum 2>&1 | tail -5; then
            log "ERROR: Failed to onboard $m"
            ONBOARD_FAILURES=$((ONBOARD_FAILURES + 1))
        fi
    done
    # Same `grep -c || echo 0` trap as PRE_COUNT in the gate phase: grep -c
    # always emits the count to stdout AND exits 1 when the count is zero, so
    # the fallback echoes a *second* "0", producing PROFILE_COUNT="0\n0".
    # Use `|| true` to swallow the exit code only.
    PROFILE_COUNT=$(oat model list 2>/dev/null | tail -n +3 | grep -c "true" || true)
    [[ -z "$PROFILE_COUNT" ]] && PROFILE_COUNT=0
    log "Model onboarding complete. ${PROFILE_COUNT} eligible profiles available."
    if [[ "$PROFILE_COUNT" -eq 0 ]]; then
        log "FATAL: No eligible models after onboarding. Check API keys and Python dependencies (pip install langchain-anthropic langchain-openai langchain-google-genai)."
        exit 1
    fi
    if [[ "$ONBOARD_FAILURES" -gt 0 ]]; then
        log "WARNING: $ONBOARD_FAILURES model(s) failed to onboard. Continuing with remaining worker models."
        log "  Eligible models: $(oat model list 2>/dev/null | tail -n +3 | grep 'true' | awk '{print $1}' | tr '\n' ', ')"
    fi

    # Warm up local (Ollama) models to prevent cold-start timeouts.
    # Ollama loads models lazily; simultaneous first requests from multiple
    # workers can time out before the model finishes loading into GPU/RAM.
    OLLAMA_URL="${OLLAMA_HOST:-http://localhost:11434}"
    OLLAMA_MODEL_COUNT=0
    for m in "${ROUTING_MODELS[@]}"; do
        m=$(echo "$m" | xargs)
        if [[ "$m" == ollama:* ]]; then
            local_model="${m#ollama:}"
            OLLAMA_MODEL_COUNT=$((OLLAMA_MODEL_COUNT + 1))
            log "  Warming up Ollama model: $local_model"
            if timeout 120 curl -sf "$OLLAMA_URL/api/generate" \
                -d "{\"model\":\"$local_model\",\"prompt\":\"\",\"stream\":false,\"keep_alive\":\"4h\"}" \
                > /dev/null 2>&1; then
                log "    Model loaded into memory (keep_alive=4h)"
            else
                log "    WARNING: Failed to warm up $local_model -- Ollama may not be running or model not pulled"
                log "    Workers using this model may fail on cold start"
            fi
        fi
    done
    if [[ "$MODEL" == ollama:* ]]; then
        local_model="${MODEL#ollama:}"
        if ! printf '%s\n' "${ROUTING_MODELS[@]}" | grep -qx "$MODEL"; then
            OLLAMA_MODEL_COUNT=$((OLLAMA_MODEL_COUNT + 1))
            log "  Warming up orchestrator model: $local_model"
            timeout 120 curl -sf "$OLLAMA_URL/api/generate" \
                -d "{\"model\":\"$local_model\",\"prompt\":\"\",\"stream\":false,\"keep_alive\":\"4h\"}" \
                > /dev/null 2>&1 || log "    WARNING: Failed to warm up orchestrator model"
        fi
    fi
    if [[ "$OLLAMA_MODEL_COUNT" -gt 1 ]]; then
        log "NOTE: $OLLAMA_MODEL_COUNT Ollama models detected. By default Ollama keeps only 1 model loaded."
        log "Set OLLAMA_MAX_LOADED_MODELS=$OLLAMA_MODEL_COUNT before starting Ollama to keep all loaded:"
        echo ""
        echo "  macOS:"
        echo "    launchctl setenv OLLAMA_MAX_LOADED_MODELS $OLLAMA_MODEL_COUNT"
        echo "    # Then restart the Ollama app (quit from menu bar, reopen)"
        echo ""
        echo "  Linux:"
        echo "    sudo systemctl edit ollama.service"
        echo "    # Add: Environment=\"OLLAMA_MAX_LOADED_MODELS=$OLLAMA_MODEL_COUNT\""
        echo "    sudo systemctl restart ollama"
        echo ""
        echo "  Manual (any platform):"
        echo "    OLLAMA_MAX_LOADED_MODELS=$OLLAMA_MODEL_COUNT ollama serve"
        echo ""
        log "Without this, switching between models will cause cold-start delays."
    fi

    # Restrict the repo to only use the specified worker models
    log "Setting allowed worker models for ${REPO_NAME}..."
    oat config "$REPO_NAME" --allowed-worker-models "$AVAILABLE_MODELS"

    # Write routing config to results dir
    mkdir -p "${RUN_DIR}"
    cat > "${RUN_DIR}/routing-config.json" <<RCEOF
{
  "routing_mode": true,
  "orchestrator_model": "${MODEL}",
  "available_models": [$(printf '"%s",' "${ROUTING_MODELS[@]}" | sed 's/,$//')],
  "onboarded_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
RCEOF
fi

echo ""
log "Benchmark configuration:"
echo "    Repo:           ${REPO_FULL}"
echo "    Model:          ${MODEL}"
if [[ "$ROUTING_MODE" == true ]]; then
    echo "    Routing mode:   enabled"
    echo "    Orchestrator:   ${MODEL}"
    echo "    Available:      ${AVAILABLE_MODELS}"
elif [[ -n "$WORKER_MODEL" ]]; then
    echo "    Worker model:   ${WORKER_MODEL}"
fi
echo "    Wave timeout:   ${WAVE_TIMEOUT} min"
echo "    Nudge timeout:  ${NUDGE_TIMEOUT} min"
echo "    Grand timeout:  ${GRAND_TIMEOUT} min"
echo "    Poll interval:  ${POLL_INTERVAL} sec"
if [[ "$SKIP_GATE" == false ]]; then
    echo "    Gate threshold: ${GATE_THRESHOLD}/100"
    echo "    Gate timeout:   ${GATE_TIMEOUT} min"
    echo "    Judge model:    ${JUDGE_MODEL}"
    if [[ "$GATE_ONLY" == true ]]; then
        echo "    Gate only:      yes (waves will be skipped)"
    fi
else
    echo "    Gate:           skipped"
fi
if [[ "$SKIP_CONVERGENCE" == false ]]; then
    echo "    Convergence:    timeout ${CONVERGENCE_TIMEOUT} min, iter timeout ${CONVERGENCE_ITER_TIMEOUT} min, max ${MAX_CONVERGENCE_ITERS} iters"
else
    echo "    Convergence:    skipped"
fi
echo ""
echo "    To monitor in another terminal:"
echo "        oat ui --repo ${REPO_NAME}"
echo ""

# --- Step 2: Wait for workspace agent to be ready ---

log "Waiting for workspace agent to be ready..."

READY_TIMEOUT=120
READY_ELAPSED=0
while true; do
    if oat status 2>/dev/null | grep -q "${REPO_NAME}"; then
        log "    Workspace agent is ready"
        break
    fi
    if [[ $READY_ELAPSED -ge $READY_TIMEOUT ]]; then
        echo "Error: Workspace agent did not become ready within ${READY_TIMEOUT}s"
        echo "Check: oat status"
        exit 1
    fi
    sleep 5
    READY_ELAPSED=$((READY_ELAPSED + 5))
done

# Give the agent a moment to finish initializing
sleep 15

# --- Step 1.5: Blackbox Gate ---

GATE_PASSED=true
GATE_SCORE=""

if [[ "$SKIP_GATE" == false ]]; then
    echo ""
    log "========== BLACKBOX GATE =========="
    echo ""

    # -- Step A: Discover wave:0 issues and send parallel gate tasks --

    GATE_START=$(date +%s)

    MODEL_FLAG=""
    if [[ -n "$WORKER_MODEL" ]]; then
        MODEL_FLAG=" --model ${WORKER_MODEL}"
    fi

    # Discover wave:0 issues with retry-and-fallback against degraded GitHub
    # indexing. Single network call on the healthy path; falls back to
    # issues.json with a loud WARNING (and ERROR for genuine 404s) when the
    # live list undercounts. See benchmarks/lib.sh.
    PRE_ISSUES=$(discover_wave_issues_with_retry "$REPO_FULL" "wave:0")
    # `grep -c` always prints the count to stdout AND exits 1 when there are
    # zero matches; `|| true` swallows just the exit code so we get a clean
    # "0" instead of "0\n0" and avoid `[[: 0 0: ...]]` errors downstream.
    PRE_COUNT=$(echo "$PRE_ISSUES" | grep -c '^[0-9]' || true)
    [[ -z "$PRE_COUNT" ]] && PRE_COUNT=0

    # Inline annotation when live disagrees with expected (the WARNING with
    # full diff is emitted once by discover_wave_issues_with_retry).
    GATE_KICKOFF_ANNOTATION=""
    if [[ "$EXPECTED_WAVE_0" -gt 0 && "$PRE_COUNT" -ne "$EXPECTED_WAVE_0" ]]; then
        GATE_KICKOFF_ANNOTATION=" (live=${PRE_COUNT} expected=${EXPECTED_WAVE_0} indexed)"
    fi

    if [[ "$PRE_COUNT" -eq 0 ]]; then
        # Reachable when issues.json is missing (custom benchmark) AND the
        # repo has no wave:0 issues, OR when the entire expected set probed
        # 404 (setup catastrophic failure -- see ERRORs above).
        log "WARNING: No wave:0 issues found in repo -- falling back to issue #1"
        PRE_ISSUES="1"
        PRE_COUNT=1
    fi

    log "Found ${PRE_COUNT} wave:0 issues${GATE_KICKOFF_ANNOTATION}: $(echo $PRE_ISSUES | tr '\n' ' ')"

    # Build worker commands for each wave:0 issue
    WORKER_CMDS=""
    for issue_num in $PRE_ISSUES; do
        issue_title=$(gh issue view "$issue_num" --repo "$REPO_FULL" --json title --jq '.title' 2>/dev/null || echo "Blackbox test module")
        WORKER_CMDS="${WORKER_CMDS}
oat worker create \"Work on issue #${issue_num}: ${issue_title}. Read docs/blackbox-testing.md for testing methodology. Read scripts/blackbox-tests/helpers.sh for the test framework API. Read your issue body for which spec sections to read and which file to create.\" --issue ${issue_num} --repo ${REPO_NAME}${MODEL_FLAG}"
    done

    GATE_MSG="Create workers for all wave:0 issues in this repo. There are ${PRE_COUNT} wave:0 issues -- each worker writes a separate test module to scripts/blackbox-tests/. They are fully parallel with no dependencies on each other.

Create one worker per issue using these exact commands:
${WORKER_CMDS}

Do not change issue numbers, repo names, or task descriptions in the commands. Do not ask for confirmation. Run them all now."

    if [[ "$ROUTING_MODE" == true ]]; then
        GATE_MSG="${GATE_MSG} IMPORTANT: You have multiple models available. For each worker command above, add --model <model> to choose the best model for that task's complexity. Refer to 'Available Models for Workers' in your system prompt for the model roster with scores and strengths. Route complex tasks (multi-file, debugging, many dependencies) to the highest-scoring model and simpler tasks (single-file, docs, config) to lower-scoring models."
    fi

    log "Sending gate tasks to workspace agent (${PRE_COUNT} parallel workers)..."
    if ! send_to_agent "$REPO_NAME" "default" "$GATE_MSG"; then
        log "Error: Failed to send gate task to workspace agent"
        GATE_PASSED=false
    fi

    # Tell supervisor not to spawn workers during gate
    GATE_SV_MSG="[benchmark] The workspace agent is creating workers for wave:0 issues (blackbox test generation). Do NOT spawn workers for any issues. The benchmark script controls all worker creation. Monitor the gate workers only."
    send_to_agent "$REPO_NAME" "supervisor" "$GATE_SV_MSG" || true

    # -- Step B: 30-second follow-up check --

    if [[ "$GATE_PASSED" == true ]]; then
        sleep 30
        VERIFY_MSG="Verify your workers are running for the wave:0 issues (${PRE_COUNT} workers expected). Check worker output logs under ~/.oat/output/${REPO_NAME}/workers/ to verify workers are active and not stuck. If any workers are missing or appear to be working on wrong issues, re-create them using the exact commands from my previous message."
        send_to_agent "$REPO_NAME" "default" "$VERIFY_MSG" || true
        log "Follow-up check sent to workspace"
    fi

    # -- Step B2: Verify workers were actually created --

    if [[ "$GATE_PASSED" == true ]]; then
        sleep 60
        WORKER_OUTPUT=$(oat worker list --repo "$REPO_NAME" 2>/dev/null || true)
        if echo "$WORKER_OUTPUT" | grep -q "No workers"; then
            log "WARNING: No workers found after 90s -- workspace agent may have failed"
            log "WARNING: Check logs: oat attach default --repo ${REPO_NAME}"
            log "WARNING: Common cause: invalid model string (model not found at provider)"
        fi
    fi

    # -- Step C: Wait for all wave:0 issues to close --

    GATE_START=$(date +%s)
    GATE_TIMEOUT_SECS=$((GATE_TIMEOUT * 60))
    WAVE0_TIMED_OUT=false
    if [[ "$GATE_PASSED" == true ]]; then
        log "Waiting for all ${PRE_COUNT} wave:0 issues to close (timeout: ${GATE_TIMEOUT}m)..."
    fi

    while [[ "$GATE_PASSED" == true ]]; do
        elapsed=$(( $(date +%s) - GATE_START ))

        open_count=$(count_open_issues "$REPO_FULL" "0")

        if [[ "$open_count" == "-1" ]]; then
            log "Wave:0 query failed, retrying... ($(format_duration "$elapsed") elapsed)"
        elif [[ "$open_count" -eq 0 ]]; then
            log "All wave:0 issues closed after $(format_duration "$elapsed")"
            break
        else
            total_w0=$(count_total_wave_issues "$REPO_FULL" "0")
            closed_w0=$((total_w0 - open_count))
            log "Wave:0: ${closed_w0}/${total_w0} issues closed ($(format_duration "$elapsed") elapsed)"
        fi

        if [[ $elapsed -ge $GATE_TIMEOUT_SECS ]]; then
            WAVE0_TIMED_OUT=true
            log "Timeout waiting for wave:0 issues (${GATE_TIMEOUT}m)"
            # Check if at least some test modules were created
            modules_exist=$(gh api "repos/${REPO_FULL}/contents/scripts/blackbox-tests" --jq '[.[] | select(.name | test("^test-.*\\.sh$"))] | length' 2>/dev/null || echo "0")
            if [[ "$modules_exist" -gt 0 ]]; then
                log "Found ${modules_exist} test module(s) despite timeout -- extracting partial result"
                break
            fi
            log "GATE FAILED: No test modules produced within timeout"
            GATE_PASSED=false
            break
        fi

        sleep "$POLL_INTERVAL"
    done

    # Merge grace period: wait for wave:0 PRs that may still be in CI/merging.
    # Only needed on the timeout path -- if all issues closed, PRs already merged.
    if [[ "$GATE_PASSED" == true && "$WAVE0_TIMED_OUT" == true ]]; then
        OPEN_W0_PRS=$(gh pr list --repo "$REPO_FULL" --state open --label "wave:0" --json number --jq 'length' 2>/dev/null || echo "0")
        if [[ "$OPEN_W0_PRS" -gt 0 ]]; then
            log "Waiting up to 60s for ${OPEN_W0_PRS} open wave:0 PR(s) to merge..."
            MERGE_GRACE_START=$(date +%s)
            while true; do
                STILL_OPEN=$(gh pr list --repo "$REPO_FULL" --state open --label "wave:0" --json number --jq 'length' 2>/dev/null || echo "0")
                GRACE_ELAPSED=$(( $(date +%s) - MERGE_GRACE_START ))
                if [[ "$STILL_OPEN" -eq 0 ]]; then
                    log "All wave:0 PRs merged ($(format_duration "$GRACE_ELAPSED"))"
                    break
                fi
                if [[ "$GRACE_ELAPSED" -ge 60 ]]; then
                    log "Merge grace period ended: ${STILL_OPEN} PR(s) still open after $(format_duration "$GRACE_ELAPSED")"
                    break
                fi
                sleep 15
            done
        fi
    fi

    # -- Step D: Assemble generated test from modular files --

    GENERATED_TEST="${RUN_DIR}/gate-generated-test.sh"

    if [[ "$GATE_PASSED" == true ]]; then
        if assemble_gate_test "$REPO_FULL" "$GENERATED_TEST"; then
            GATE_DURATION=$(($(date +%s) - GATE_START))
            log "Gate worker duration: $(format_duration "$GATE_DURATION")"
        else
            log "GATE FAILED: Could not assemble test from repo"
            GATE_PASSED=false
        fi
    fi

    # -- Step E: Judge the generated test --

    if [[ "$GATE_PASSED" == true && -f "$GENERATED_TEST" ]]; then
        log "Running judge-blackbox.sh..."
        if "${SCRIPT_DIR}/scripts/judge-blackbox.sh" \
            --generated "$GENERATED_TEST" \
            --reference "${SCRIPT_DIR}/acceptance-test.sh" \
            --output "$RUN_DIR/gate.json" \
            --judge-model "$JUDGE_MODEL" \
            --threshold "$GATE_THRESHOLD"; then

            GATE_VERDICT=$(jq -r '.verdict // "fail"' "$RUN_DIR/gate.json" 2>/dev/null || echo "fail")
            GATE_SCORE=$(jq -r '.score // 0' "$RUN_DIR/gate.json" 2>/dev/null || echo "0")

            if [[ "$GATE_VERDICT" == "pass" ]]; then
                log "GATE PASSED: score ${GATE_SCORE}/100 (threshold: ${GATE_THRESHOLD})"
                GATE_PASSED=true
            else
                log "GATE FAILED: score ${GATE_SCORE}/100 (threshold: ${GATE_THRESHOLD})"
                GATE_PASSED=false
            fi
        else
            log "Warning: judge-blackbox.sh failed"
            GATE_PASSED=false
        fi
    fi

    # Execution smoke test: verify the generated test actually runs without
    # crashing before investing time in waves. Catches bash version issues
    # (e.g. declare -A on macOS bash 3.2), syntax errors, and unbound variables
    # that the LLM judge can't detect from source alone.
    if [[ "$GATE_PASSED" == true && -f "$GENERATED_TEST" ]]; then
        log "Running execution smoke test on generated blackbox test..."
        SMOKE_OUTPUT="$RUN_DIR/gate-smoke.json"
        SMOKE_EXIT=0
        "${SCRIPT_DIR}/scripts/run-blackbox.sh" \
            --test "$GENERATED_TEST" \
            --repo "$REPO_NAME" \
            --smoke \
            --output "$SMOKE_OUTPUT" && SMOKE_EXIT=0 || SMOKE_EXIT=$?

        SMOKE_PASSED=$(jq -r '.passed // 0' "$SMOKE_OUTPUT" 2>/dev/null || echo "0")
        SMOKE_FAILED=$(jq -r '.failed // 0' "$SMOKE_OUTPUT" 2>/dev/null || echo "0")
        SMOKE_TEST_EXIT=$(jq -r '.exit_code // 1' "$SMOKE_OUTPUT" 2>/dev/null || echo "1")
        SMOKE_TOTAL=$((SMOKE_PASSED + SMOKE_FAILED))

        if [[ "$SMOKE_TOTAL" -eq 0 ]]; then
            log ""
            echo "  ========================================="
            echo "    GATE SMOKE TEST FAILED"
            echo "  ========================================="
            echo "  The generated blackbox test CRASHED without producing any PASS/FAIL results."
            echo "  This usually means a syntax error, bash compatibility issue (e.g., declare -A"
            echo "  on macOS bash 3.2), or an unbound variable that prevented the test from running."
            echo ""
            # Diagnostic source-of-truth is the captured runtime output below.
            # An earlier pre-flight design populated a SMOKE_REASONS array and
            # rendered it here; that pre-flight was replaced (8a2f71d) by the
            # current execution-based smoke runner, which surfaces the actual
            # crash text via .raw_output. The orphaned SMOKE_REASONS read was
            # left behind in a partial follow-up commit (f87f6d6) and would
            # crash this branch under set -u with "unbound variable".
            RAW_SNIPPET=$(jq -r '.raw_output // ""' "$SMOKE_OUTPUT" 2>/dev/null | head -c 500 || true)
            if [[ -n "$RAW_SNIPPET" ]]; then
                echo "  Output snippet: ${RAW_SNIPPET}"
            else
                echo "  (no captured output -- check ${SMOKE_OUTPUT} for raw runner JSON)"
            fi
            echo "  ========================================="
            log ""
            GATE_PASSED=false
        else
            log ""
            echo "  ========================================="
            echo "    GATE SMOKE TEST OK"
            echo "  ========================================="
            echo "  The blackbox test EXECUTES correctly (no crashes, syntax errors, or bash"
            echo "  compatibility issues). Syntax check passed."
            echo ""
            echo "  This is EXPECTED -- the app hasn't been built yet, so most tests fail."
            echo "  What matters is the test RUNS. Workers will now build the app."
            echo "  ========================================="
            log ""
        fi
    fi

    if [[ "$GATE_ONLY" == true ]]; then
        log "Gate-only mode: skipping wave progression"
        GATE_PASSED=false  # Prevent waves from running
    fi

    if [[ "$GATE_PASSED" == false && "$GATE_ONLY" == false ]]; then
        log "Gate failed -- skipping wave progression, proceeding to results collection"
    fi
else
    log "Blackbox gate skipped (--skip-gate)"
fi

# --- Step 3: Wave progression loop ---

WAVE_RESULTS=()
WAVE_START_1=0 WAVE_START_2=0 WAVE_START_3=0 WAVE_START_4=0
WAVE_END_1=0 WAVE_END_2=0 WAVE_END_3=0 WAVE_END_4=0

if [[ "$GATE_PASSED" == false ]]; then
    if [[ "$GATE_ONLY" == false ]]; then
        log "Skipping waves (gate did not pass)"
    fi
    for wave_num in 1 2 3 4; do
        WAVE_RESULTS+=("skipped")
    done
else

for wave_num in 1 2 3 4; do
    echo ""
    log "========== WAVE ${wave_num} =========="

    if grand_timeout_reached; then
        log "Grand timeout reached before starting wave ${wave_num}, skipping remaining waves"
        WAVE_RESULTS+=("timeout")
        continue
    fi

    WAVE_START_TS=$(date +%s)

    # Discover the trustworthy list of OPEN wave issues for the kickoff
    # message. Polls the live `gh issue list` against the JSON-derived
    # expected count and falls back with a loud WARNING (or ERROR for any
    # genuine 404s) under indexing degradation. Single network call on the
    # healthy path.
    WAVE_ISSUE_NUMBERS=$(discover_wave_issues_with_retry "$REPO_FULL" "wave:${wave_num}")
    WAVE_ISSUE_COUNT=$(echo "$WAVE_ISSUE_NUMBERS" | grep -c '^[0-9]' || true)
    [[ -z "$WAVE_ISSUE_COUNT" ]] && WAVE_ISSUE_COUNT=0

    # Inline annotation when live disagrees with expected. The detailed
    # WARNING (with the missing-issue list and status URL) is emitted once
    # per wave by discover_wave_issues_with_retry.
    EXPECTED_VAR="EXPECTED_WAVE_${wave_num}"
    EXPECTED_THIS_WAVE="${!EXPECTED_VAR:-0}"
    WAVE_KICKOFF_ANNOTATION=""
    if [[ "$EXPECTED_THIS_WAVE" -gt 0 && "$WAVE_ISSUE_COUNT" -ne "$EXPECTED_THIS_WAVE" ]]; then
        WAVE_KICKOFF_ANNOTATION=" (live=${WAVE_ISSUE_COUNT} expected=${EXPECTED_THIS_WAVE} indexed)"
    fi
    log "    Wave ${wave_num}: ${WAVE_ISSUE_COUNT} open issue(s) discovered${WAVE_KICKOFF_ANNOTATION}"

    # Floor the count for the workspace prompt: even if some expected issues
    # are currently invisible to gh issue list, the workspace agent should
    # be told the JSON-truth count so it knows to re-fetch any it can't see.
    if [[ "$EXPECTED_THIS_WAVE" -gt "$WAVE_ISSUE_COUNT" ]]; then
        WAVE_ISSUE_COUNT="$EXPECTED_THIS_WAVE"
    fi

    # Build the wave message
    if [[ $wave_num -eq 1 ]]; then
        WAVE_MSG="Look at the open issues in this repo (${REPO_FULL}). They are organized by wave labels (wave:1, wave:2, wave:3, wave:4) which represent dependency ordering -- wave N issues depend on wave N-1 being completed first. Start by creating workers for the wave:1 issues. There are exactly ${WAVE_ISSUE_COUNT} wave:1 issues. After reading issues, verify you read all ${WAVE_ISSUE_COUNT} by counting. If any output appears truncated (cut off mid-sentence or fewer results than expected), re-fetch missing issues individually with 'gh issue view <number>'. Do not ask for confirmation or clarification; use the current directory and gh CLI to inspect issues and proceed immediately."
    else
        prev_wave=$((wave_num - 1))
        WAVE_MSG="Wave ${prev_wave} is complete. Please create workers for the wave:${wave_num} issues in this repo (${REPO_FULL}). There are exactly ${WAVE_ISSUE_COUNT} wave:${wave_num} issues. Verify you read all ${WAVE_ISSUE_COUNT} before spawning workers. If output is truncated, re-fetch individually with 'gh issue view <number>'. Do not ask for confirmation or clarification; proceed immediately."
    fi

    if [[ "$ROUTING_MODE" == true ]]; then
        WAVE_MSG="${WAVE_MSG} IMPORTANT: You have multiple models available. You MUST distribute workers across them using --model <model>. Do NOT send all tasks to the same model. Match task complexity to model strength — complex tasks to the top model, standard/simple tasks to other eligible models. Check the 'Available Models for Workers' section in your system prompt for the roster."
    elif [[ -n "$WORKER_MODEL" ]]; then
        WAVE_MSG="${WAVE_MSG} Use --model ${WORKER_MODEL} when creating workers."
    fi

    WAVE_MSG="${WAVE_MSG} After spawning all workers, wait 30 seconds, then check each worker's output log (under ~/.oat/output/${REPO_NAME}/workers/) to verify they are working on the correct issues and none are stuck or confused. Fix any problems you find."

    SUPERVISOR_MSG="[benchmark] The workspace agent is now creating workers for wave:${wave_num} issues. Do NOT spawn workers for any open issues yourself -- the workspace owns all worker creation for this benchmark run. Your role: monitor existing workers, nudge stuck agents, coordinate with the merge queue, and handle escalations. Do not duplicate the workspace's worker assignments."

    send_wave_coordination "$REPO_NAME" "$WAVE_MSG" "$SUPERVISOR_MSG"

    if wait_for_wave "$REPO_NAME" "$REPO_FULL" "$wave_num" "$WAVE_TIMEOUT_SECS" "$NUDGE_TIMEOUT_SECS"; then
        WAVE_RESULTS+=("complete")
    else
        WAVE_RESULTS+=("timeout")
    fi

    WAVE_END_TS=$(date +%s)
    eval "WAVE_START_${wave_num}=\$WAVE_START_TS"
    eval "WAVE_END_${wave_num}=\$WAVE_END_TS"
    WAVE_DURATION=$((WAVE_END_TS - WAVE_START_TS))
    log "    Wave ${wave_num} duration: $(format_duration "$WAVE_DURATION")"
done

fi  # end gate_passed check

# Write wave timing for collect.sh early (defense in depth: captured even if
# the script crashes during the merge grace period or convergence loop).
jq -n \
    --argjson w1s "$WAVE_START_1" --argjson w1e "$WAVE_END_1" \
    --argjson w2s "$WAVE_START_2" --argjson w2e "$WAVE_END_2" \
    --argjson w3s "$WAVE_START_3" --argjson w3e "$WAVE_END_3" \
    --argjson w4s "$WAVE_START_4" --argjson w4e "$WAVE_END_4" \
    '{
        "1": {"started_epoch": $w1s, "completed_epoch": $w1e},
        "2": {"started_epoch": $w2s, "completed_epoch": $w2e},
        "3": {"started_epoch": $w3s, "completed_epoch": $w3e},
        "4": {"started_epoch": $w4s, "completed_epoch": $w4e}
    }' > "$RUN_DIR/wave-timing.json"
log "Wrote wave timing to ${RUN_DIR}/wave-timing.json"

# Merge grace period after waves 1-4: if the last wave timed out, wait for
# any remaining PRs to merge before the convergence loop tests main.
WAVE_RESULTS_LEN=${#WAVE_RESULTS[@]}
if [[ $WAVE_RESULTS_LEN -gt 0 && "${WAVE_RESULTS[$((WAVE_RESULTS_LEN - 1))]}" == "timeout" ]]; then
    LAST_WAVE_NUM=$WAVE_RESULTS_LEN
    OPEN_WAVE_PRS=$(gh pr list --repo "$REPO_FULL" --state open --label "wave:${LAST_WAVE_NUM}" --json number --jq 'length' 2>/dev/null || echo "0")
    if [[ "$OPEN_WAVE_PRS" -gt 0 ]]; then
        log "Waiting up to 60s for ${OPEN_WAVE_PRS} open wave:${LAST_WAVE_NUM} PR(s) to merge..."
        MERGE_GRACE_START=$(date +%s)
        while true; do
            STILL_OPEN=$(gh pr list --repo "$REPO_FULL" --state open --label "wave:${LAST_WAVE_NUM}" --json number --jq 'length' 2>/dev/null || echo "0")
            GRACE_ELAPSED=$(( $(date +%s) - MERGE_GRACE_START ))
            if [[ "$STILL_OPEN" -eq 0 ]]; then
                log "All wave:${LAST_WAVE_NUM} PRs merged ($(format_duration "$GRACE_ELAPSED"))"
                break
            fi
            if [[ "$GRACE_ELAPSED" -ge 60 ]]; then
                log "Merge grace period ended: ${STILL_OPEN} PR(s) still open after $(format_duration "$GRACE_ELAPSED")"
                break
            fi
            sleep 15
        done
    fi
fi

# --- Step 3.5: Convergence loop ---

CONVERGENCE_VERDICT="skipped"
CONVERGENCE_ITERATIONS=0
CONVERGENCE_RESULTS=()

if [[ "$GATE_PASSED" == true && "$SKIP_CONVERGENCE" == false && -f "$RUN_DIR/gate-generated-test.sh" ]]; then
    echo ""
    log "========== CONVERGENCE LOOP =========="
    echo ""

    CONV_START=$(date +%s)

    convergence_timeout_reached() {
        local now elapsed
        now=$(date +%s)
        elapsed=$((now - CONV_START))
        [[ $elapsed -ge $CONVERGENCE_TIMEOUT_SECS ]]
    }

    wait_for_fix_issues_created() {
        local repo_full="$1"
        local fix_label="$2"
        local max_wait=600  # 10 minutes
        local poll=120      # 2 minutes
        local waited=0

        log "    Waiting for ${fix_label} issues to be created (up to 10m, polling every 2m)..."

        while [[ $waited -lt $max_wait ]]; do
            sleep "$poll"
            waited=$((waited + poll))

            local count
            count=$(gh issue list --repo "$repo_full" --label "$fix_label" --state all --json number --jq 'length' 2>/dev/null || echo "0")
            if [[ "$count" -gt 0 ]]; then
                log "    Found ${count} issue(s) with label ${fix_label} after $(format_duration "$waited")"
                return 0
            fi
            log "    No ${fix_label} issues yet ($(format_duration "$waited") elapsed)"
        done

        # Final check at the 10-minute mark
        local count
        count=$(gh issue list --repo "$repo_full" --label "$fix_label" --state all --json number --jq 'length' 2>/dev/null || echo "0")
        if [[ "$count" -gt 0 ]]; then
            log "    Found ${count} issue(s) with label ${fix_label} at final check"
            return 0
        fi

        log "    No ${fix_label} issues created within 10m -- skipping this iteration"
        return 1
    }

    # Preserve the original gate snapshot for audit (anti-cheat: original is always available)
    if [[ ! -f "$RUN_DIR/gate-generated-test-original.sh" ]]; then
        cp "$RUN_DIR/gate-generated-test.sh" "$RUN_DIR/gate-generated-test-original.sh"
    fi

    PREV_FAIL_HASH=""
    STALE_COUNT=0

    for conv_iter in $(seq 0 $((MAX_CONVERGENCE_ITERS - 1))); do
        CONVERGENCE_ITERATIONS=$((conv_iter + 1))

        if grand_timeout_reached; then
            log "Grand timeout reached, stopping convergence"
            CONVERGENCE_VERDICT="grand_timeout"
            break
        fi

        if convergence_timeout_reached; then
            log "Convergence timeout reached (${CONVERGENCE_TIMEOUT}m)"
            CONVERGENCE_VERDICT="timeout"
            break
        fi

        log "--- Convergence iteration ${conv_iter} ---"

        # Re-assemble the test from the repo -- fix-wave workers may have modified modules.
        # The original gate snapshot is preserved as gate-generated-test-original.sh.
        LATEST_TEST=$(mktemp)
        if assemble_gate_test "$REPO_FULL" "$LATEST_TEST" 2>/dev/null; then
            if ! diff -q "$RUN_DIR/gate-generated-test.sh" "$LATEST_TEST" >/dev/null 2>&1; then
                cp "$LATEST_TEST" "$RUN_DIR/gate-generated-test.sh"
                chmod +x "$RUN_DIR/gate-generated-test.sh"
                log "Updated test from repo (workers fixed it since last iteration)"
            fi
        fi
        rm -f "$LATEST_TEST"

        log "Running blackbox test against built application..."

        ITER_BB_OUTPUT="$RUN_DIR/blackbox-iter-${conv_iter}.json"
        BB_EXIT=0
        "${SCRIPT_DIR}/scripts/run-blackbox.sh" \
            --test "$RUN_DIR/gate-generated-test.sh" \
            --repo "$REPO_NAME" \
            --output "$ITER_BB_OUTPUT" && BB_EXIT=0 || BB_EXIT=$?

        ITER_PASSED=$(jq -r '.passed // 0' "$ITER_BB_OUTPUT" 2>/dev/null || echo "0")
        ITER_FAILED=$(jq -r '.failed // 0' "$ITER_BB_OUTPUT" 2>/dev/null || echo "0")
        ITER_EXIT=$(jq -r '.exit_code // 1' "$ITER_BB_OUTPUT" 2>/dev/null || echo "1")
        ITER_SCORE=$(jq -r '.score_estimate // "0%"' "$ITER_BB_OUTPUT" 2>/dev/null || echo "0%")
        CONVERGENCE_RESULTS+=("iter${conv_iter}:exit=${ITER_EXIT},passed=${ITER_PASSED},failed=${ITER_FAILED},score=${ITER_SCORE}")

        log "    Result: ${ITER_PASSED} passed, ${ITER_FAILED} failed (${ITER_SCORE}), exit code: ${ITER_EXIT}"

        # Binary pass/fail: exit code 0 means convergence achieved
        if [[ "$ITER_EXIT" -eq 0 ]]; then
            log "Blackbox test PASSED -- convergence achieved!"
            CONVERGENCE_VERDICT="pass"
            # Copy the final iteration result as the canonical blackbox-acceptance.json
            cp "$ITER_BB_OUTPUT" "$RUN_DIR/blackbox-acceptance.json"
            break
        fi

        # Last iteration -- no point sending fix messages
        if [[ $conv_iter -ge $((MAX_CONVERGENCE_ITERS - 1)) ]]; then
            log "Max convergence iterations (${MAX_CONVERGENCE_ITERS}) exhausted"
            CONVERGENCE_VERDICT="max_iterations"
            cp "$ITER_BB_OUTPUT" "$RUN_DIR/blackbox-acceptance.json"
            break
        fi

        # Check timeouts before starting fix cycle
        if convergence_timeout_reached || grand_timeout_reached; then
            log "Timeout reached after test run, stopping convergence"
            CONVERGENCE_VERDICT="timeout"
            cp "$ITER_BB_OUTPUT" "$RUN_DIR/blackbox-acceptance.json"
            break
        fi

        # Extract failure lines for the message to workspace
        FAIL_LINES=""
        if [[ -f "$ITER_BB_OUTPUT" ]]; then
            FAIL_LINES=$(jq -r '.raw_output // ""' "$ITER_BB_OUTPUT" 2>/dev/null | grep -iE 'FAIL|ERROR|not ok|✗|✘|❌' | head -30 || true)
        fi
        if [[ -z "$FAIL_LINES" ]]; then
            FAIL_LINES="(could not extract specific failure lines -- exit code was ${ITER_EXIT})"
        fi

        # Track stale iterations using failure-line hashing (not just counts).
        # Comparing only pass/fail counts is unreliable -- if 1 fix lands but
        # something else breaks, the count stays the same despite real movement.
        ITER_FAIL_HASH=$(echo "$FAIL_LINES" | sort | md5sum | cut -d' ' -f1)
        if [[ "$ITER_FAIL_HASH" == "$PREV_FAIL_HASH" ]]; then
            STALE_COUNT=$((STALE_COUNT + 1))
        else
            STALE_COUNT=0
        fi
        PREV_FAIL_HASH="$ITER_FAIL_HASH"

        if [[ "$STALE_COUNT" -ge 1 ]]; then
            log "No progress: identical failure lines for ${STALE_COUNT}+ consecutive iterations. Exiting convergence loop."
            CONVERGENCE_VERDICT="no_progress"
            cp "$ITER_BB_OUTPUT" "$RUN_DIR/blackbox-acceptance.json"
            break
        fi

        # Extract Python exceptions and other diagnostic patterns from raw output
        ERROR_CONTEXT=""
        if [[ -f "$ITER_BB_OUTPUT" ]]; then
            ERROR_CONTEXT=$(jq -r '.raw_output // ""' "$ITER_BB_OUTPUT" 2>/dev/null \
                | grep -iE 'KeyError|TypeError|AttributeError|ValueError|ImportError|ModuleNotFoundError|NameError|IndexError|FileNotFoundError|RuntimeError|last error:' \
                | sort -u | head -10 || true)
        fi

        # Also pull fatal_lines from the JSON (captured by run-blackbox.sh)
        FATAL_CONTEXT=""
        if [[ -f "$ITER_BB_OUTPUT" ]]; then
            FATAL_CONTEXT=$(jq -r '.fatal_lines // ""' "$ITER_BB_OUTPUT" 2>/dev/null || true)
        fi

        FIX_LABEL="wave:fix-${conv_iter}"

        # Build the convergence message with iteration context
        CONV_WS_MSG="The blackbox test was run against the built application and FAILED (convergence iteration ${conv_iter})."

        # For iterations > 0, include previous iteration context and result diff
        if [[ $conv_iter -gt 0 ]]; then
            PREV_ITER=$((conv_iter - 1))
            PREV_LABEL="wave:fix-${PREV_ITER}"
            PREV_ISSUE_COUNT=$(gh issue list --repo "$REPO_FULL" --label "$PREV_LABEL" --state all --json number --jq 'length' 2>/dev/null || echo "?")
            PREV_MERGED_COUNT=$(gh pr list --repo "$REPO_FULL" --state merged --label "$PREV_LABEL" --json number --jq 'length' 2>/dev/null || echo "?")
            CONV_WS_MSG="${CONV_WS_MSG}

Previous iteration fix-${PREV_ITER}: ${PREV_ISSUE_COUNT} issues created, ${PREV_MERGED_COUNT} PRs merged. The test STILL fails, meaning the previous fixes did not address the root cause. You must investigate deeper this time."

            # Compare pass/fail counts to detect identical results
            if [[ -n "${PREV_PASSED:-}" && -n "${PREV_FAILED:-}" ]]; then
                if [[ "$ITER_PASSED" == "$PREV_PASSED" && "$ITER_FAILED" == "$PREV_FAILED" ]]; then
                    CONV_WS_MSG="${CONV_WS_MSG}

Results are IDENTICAL to iteration ${PREV_ITER} (still ${ITER_PASSED} passed, ${ITER_FAILED} failed). The previous fix had zero effect on test outcomes. The root cause is something else entirely -- look at the Error diagnostics below. Consider whether the TEST ITSELF has a bug (e.g., state isolation issue, contradictory assertions). Fix-wave workers are allowed to modify scripts/blackbox-test.sh if the test is at fault."
                else
                    CONV_WS_MSG="${CONV_WS_MSG}

Results changed from iteration ${PREV_ITER}: was ${PREV_PASSED} passed/${PREV_FAILED} failed, now ${ITER_PASSED} passed/${ITER_FAILED} failed."
                fi
            fi
        fi

        CONV_WS_MSG="${CONV_WS_MSG}

Here are the failures:
${FAIL_LINES}"

        # Append error diagnostics if available
        if [[ -n "$ERROR_CONTEXT" ]]; then
            CONV_WS_MSG="${CONV_WS_MSG}

Error diagnostics (exceptions/tracebacks found in output):
${ERROR_CONTEXT}"
        fi

        if [[ -n "$FATAL_CONTEXT" ]]; then
            CONV_WS_MSG="${CONV_WS_MSG}

FATAL abort lines:
${FATAL_CONTEXT}"
        fi

        CONV_WS_MSG="${CONV_WS_MSG}

If the failures appear to be a TEST-LEVEL BUG (e.g., state isolation problems, contradictory assertions that share mutable state, or incorrect expected values), fix-wave workers are allowed to modify scripts/blackbox-test.sh in addition to the implementation code. The test is model-generated and may itself be wrong.

If a worker determines a task is GENUINELY IMPOSSIBLE or contradictory (e.g., the test asserts two mutually exclusive outcomes), they should post a comment on the issue explaining why, then call 'oat agent complete' to close the issue and move on. Do not spin forever trying to reconcile irreconcilable assertions.

IMPORTANT: You MUST create new issues labeled '${FIX_LABEL}' immediately. All previous fix waves are complete and the test was re-run against the latest merged code. The failures below persist despite previous fixes. You can check 'oat worker list' (workers showing 'waiting for PR' have finished and submitted their PR) and 'gh pr list --state merged --label ${FIX_LABEL}' to verify which PRs actually merged.

Create new issues for each distinct failure using oat issue create:
  oat issue create --title \"Fix: <description>\" --body \"<details>\" --wave fix-${conv_iter} --label ${FIX_LABEL} --file <path>
Focus on one specific failure per issue. Then spawn a worker for each issue with 'oat work'. Do not ask for confirmation; proceed immediately."

        if [[ "$ROUTING_MODE" == true ]]; then
            CONV_WS_MSG="${CONV_WS_MSG} Distribute fix workers across available models using --model. Do not send all fixes to the same model."
        elif [[ -n "$WORKER_MODEL" ]]; then
            CONV_WS_MSG="${CONV_WS_MSG} Use --model ${WORKER_MODEL} when creating workers."
        fi

        # Save current results for next iteration comparison
        PREV_PASSED="$ITER_PASSED"
        PREV_FAILED="$ITER_FAILED"

        CONV_SV_MSG="[benchmark] The workspace agent is creating fix issues (${FIX_LABEL}) to address blackbox test failures from convergence iteration ${conv_iter}. Do NOT spawn workers yourself. Monitor workers, nudge stuck agents, and handle escalations."

        log "Sending failure report to workspace and supervisor..."
        send_wave_coordination "$REPO_NAME" "$CONV_WS_MSG" "$CONV_SV_MSG"

        # Wait for fix issues to be created, then wait for them to close
        if wait_for_fix_issues_created "$REPO_FULL" "$FIX_LABEL"; then
            log "Waiting for ${FIX_LABEL} issues to be resolved..."
            wait_for_wave "$REPO_NAME" "$REPO_FULL" "fix-${conv_iter}" "$CONVERGENCE_ITER_TIMEOUT_SECS" "$NUDGE_TIMEOUT_SECS" || true

            # Wait for fix-wave PRs to actually merge before re-running the test.
            # Issues close when workers call oat agent complete, but the merge-queue
            # may not have merged their PR yet. Without this, the next convergence
            # iteration tests main without the fix, producing identical results.
            EXPECTED_PRS=$(timeout 30 gh issue list --repo "$REPO_FULL" --label "$FIX_LABEL" --state all --json number --jq 'length' 2>/dev/null || echo "0")
            if [[ "$EXPECTED_PRS" -gt 0 ]]; then
                log "    Waiting for ${FIX_LABEL} PRs to merge (expecting ~${EXPECTED_PRS})..."
                MERGE_WAIT_START=$(date +%s)
                MERGE_WAIT_TIMEOUT=180
                LAST_PRINT_TIME=0
                while true; do
                    MERGED_COUNT=$(timeout 30 gh pr list --repo "$REPO_FULL" --state merged --label "$FIX_LABEL" --json number --jq 'length' 2>/dev/null || echo "0")
                    MERGE_ELAPSED=$(( $(date +%s) - MERGE_WAIT_START ))
                    NOW=$(date +%s)
                    if [[ "$MERGED_COUNT" -ge "$EXPECTED_PRS" ]]; then
                        log "    All ${MERGED_COUNT} ${FIX_LABEL} PRs merged ($(format_duration "$MERGE_ELAPSED"))"
                        break
                    fi
                    if [[ "$MERGE_ELAPSED" -ge "$MERGE_WAIT_TIMEOUT" ]]; then
                        log "    Merge wait timed out: ${MERGED_COUNT}/${EXPECTED_PRS} merged after $(format_duration "$MERGE_ELAPSED")"
                        break
                    fi
                    SINCE_PRINT=$(( NOW - LAST_PRINT_TIME ))
                    if [[ "$SINCE_PRINT" -ge 60 ]]; then
                        log "    ${MERGED_COUNT}/${EXPECTED_PRS} PRs merged ($(format_duration "$MERGE_ELAPSED") elapsed)"
                        LAST_PRINT_TIME=$NOW
                    fi
                    sleep 30
                done
            fi
        fi
    done

    # Preserve the final test snapshot for audit (complements gate-generated-test-original.sh)
    if [[ -f "$RUN_DIR/gate-generated-test.sh" ]]; then
        cp "$RUN_DIR/gate-generated-test.sh" "$RUN_DIR/gate-generated-test-final.sh"
    fi

    CONV_END=$(date +%s)
    CONV_DURATION=$((CONV_END - CONV_START))
    log "Convergence loop finished: verdict=${CONVERGENCE_VERDICT}, iterations=${CONVERGENCE_ITERATIONS}, duration=$(format_duration "$CONV_DURATION")"

    # Write convergence.json. Guard the array expansion: bash 3.2 + set -u
    # treats an empty CONVERGENCE_RESULTS=() as "unbound" when expanded with
    # "${arr[@]}". The empty case is reachable if the convergence loop hits a
    # timeout/break before its first iteration ever appends a result.
    CONV_RESULTS_JSON="[]"
    if [[ ${#CONVERGENCE_RESULTS[@]} -gt 0 ]]; then
        for cr in "${CONVERGENCE_RESULTS[@]}"; do
            iter_num=$(echo "$cr" | sed 's/iter\([0-9]*\):.*/\1/')
            iter_exit=$(echo "$cr" | sed 's/.*exit=\([^,]*\).*/\1/')
            iter_passed=$(echo "$cr" | sed 's/.*passed=\([^,]*\).*/\1/')
            iter_failed=$(echo "$cr" | sed 's/.*failed=\([^,]*\).*/\1/')
            iter_score=$(echo "$cr" | sed 's/.*score=\(.*\)/\1/')
            CONV_RESULTS_JSON=$(echo "$CONV_RESULTS_JSON" | jq \
                --argjson iter "$iter_num" \
                --argjson exit_code "$iter_exit" \
                --argjson passed "$iter_passed" \
                --argjson failed "$iter_failed" \
                --arg score "$iter_score" \
                '. + [{"iteration": $iter, "exit_code": $exit_code, "passed": $passed, "failed": $failed, "score_estimate": $score}]')
        done
    fi

    jq -n \
        --arg verdict "$CONVERGENCE_VERDICT" \
        --argjson iterations "$CONVERGENCE_ITERATIONS" \
        --argjson duration_secs "$CONV_DURATION" \
        --arg duration_human "$(format_duration "$CONV_DURATION")" \
        --argjson results "$CONV_RESULTS_JSON" \
        '{
            verdict: $verdict,
            iterations: $iterations,
            duration_secs: $duration_secs,
            duration_human: $duration_human,
            results: $results
        }' > "$RUN_DIR/convergence.json"
    log "Wrote convergence results to ${RUN_DIR}/convergence.json"

elif [[ "$GATE_PASSED" == true && "$SKIP_CONVERGENCE" == true && -f "$RUN_DIR/gate-generated-test.sh" && "$SKIP_ACCEPTANCE" == false ]]; then
    # Convergence skipped -- fall back to a single blackbox test run (legacy dual-run behavior)
    echo ""
    log "========== BLACKBOX TEST (single run, convergence skipped) =========="
    log "Running model-generated blackbox test against finished repo..."
    "${SCRIPT_DIR}/scripts/run-blackbox.sh" \
        --test "$RUN_DIR/gate-generated-test.sh" \
        --repo "$REPO_NAME" \
        --output "$RUN_DIR/blackbox-acceptance.json" || log "Warning: run-blackbox.sh failed"
    echo ""
fi

# --- Step 4: Collect results ---

echo ""
log "========== RESULTS COLLECTION =========="

RUN_END=$(date +%s)
TOTAL_DURATION=$((RUN_END - RUN_START))
log "Total run time: $(format_duration "$TOTAL_DURATION")"
echo ""

log "Results directory: ${RUN_DIR}"

log "Wave summary:"
for i in 1 2 3 4; do
    log "    Wave ${i}: ${WAVE_RESULTS[$((i - 1))]}"
done
echo ""

if [[ "$SKIP_COLLECT" == false ]]; then
    log "Running collect.sh..."
    "${SCRIPT_DIR}/collect.sh" --repo "$REPO_NAME" --output "$RUN_DIR/collect.json" || log "Warning: collect.sh failed"
    echo ""
fi

if [[ "$SKIP_ACCEPTANCE" == false && "$GATE_PASSED" == true ]]; then
    log "Running acceptance-test.sh..."
    "${SCRIPT_DIR}/acceptance-test.sh" --repo "$REPO_NAME" --output "$RUN_DIR/acceptance.json" || log "Warning: acceptance-test.sh failed"
    echo ""
fi

if [[ "$SKIP_SUMMARY" == false ]]; then
    log "Running summarize.sh..."
    "${SCRIPT_DIR}/summarize.sh" --dir "$RUN_DIR" --repo "$REPO_NAME" \
        --model "$SUMMARY_MODEL" || log "Warning: summarize.sh failed"
    echo ""
fi

# --- Final summary ---

echo ""
echo "=========================================="
echo "  Benchmark Complete"
echo "=========================================="
echo ""
log "Repo:       https://github.com/${REPO_FULL}"
echo "Model:      ${MODEL}"
echo "Duration:   $(format_duration "$TOTAL_DURATION")"
echo ""
for i in 1 2 3 4; do
    echo "Wave ${i}: ${WAVE_RESULTS[$((i - 1))]}"
done
echo ""
if [[ -f "$RUN_DIR/gate.json" ]] && command -v jq &>/dev/null; then
    GATE_S=$(jq -r '.score // "n/a"' "$RUN_DIR/gate.json")
    GATE_V=$(jq -r '.verdict // "n/a"' "$RUN_DIR/gate.json")
    GATE_V_UPPER=$(echo "$GATE_V" | tr '[:lower:]' '[:upper:]')
    echo "Gate:       ${GATE_S}/100 -- ${GATE_V_UPPER}"
fi
ACCEPTANCE_JSON="$RUN_DIR/acceptance.json"
if [[ -f "$ACCEPTANCE_JSON" ]] && command -v jq &>/dev/null; then
    ACC_SCORE=$(jq -r '.score // "n/a"' "$ACCEPTANCE_JSON")
    ACC_PASSED=$(jq -r '.summary.passed // "?"' "$ACCEPTANCE_JSON")
    ACC_TOTAL=$(jq -r '.summary.total // "?"' "$ACCEPTANCE_JSON")
    echo "Acceptance: ${ACC_SCORE}/100 (${ACC_PASSED}/${ACC_TOTAL} tests passed)"
else
    echo "Acceptance: results not available"
fi
if [[ -f "$RUN_DIR/convergence.json" ]] && command -v jq &>/dev/null; then
    CONV_V=$(jq -r '.verdict // "n/a"' "$RUN_DIR/convergence.json")
    CONV_I=$(jq -r '.iterations // "?"' "$RUN_DIR/convergence.json")
    CONV_D=$(jq -r '.duration_human // "?"' "$RUN_DIR/convergence.json")
    CONV_V_UPPER=$(echo "$CONV_V" | tr '[:lower:]' '[:upper:]')
    echo "Convergence: ${CONV_V_UPPER} (${CONV_I} iterations, ${CONV_D})"
fi
if [[ -f "$RUN_DIR/blackbox-acceptance.json" ]] && command -v jq &>/dev/null; then
    BB_PASSED=$(jq -r '.passed // "?"' "$RUN_DIR/blackbox-acceptance.json")
    BB_FAILED=$(jq -r '.failed // "?"' "$RUN_DIR/blackbox-acceptance.json")
    BB_EST=$(jq -r '.score_estimate // "?"' "$RUN_DIR/blackbox-acceptance.json")
    echo "Blackbox:   ${BB_PASSED} passed, ${BB_FAILED} failed (${BB_EST})"
fi
echo ""
log "Results:    ${RUN_DIR}/"
echo "  terminal-output.txt Full run log"
if [[ -f "$RUN_DIR/gate.json" ]]; then
    echo "  gate.json          Blackbox gate judgment"
fi
if [[ -f "$RUN_DIR/gate-generated-test.sh" ]]; then
    echo "  gate-generated-test.sh  Model's generated test script"
fi
echo "  collect.json       Operational metrics"
echo "  acceptance.json    Functional test results"
if [[ -f "$RUN_DIR/convergence.json" ]]; then
    echo "  convergence.json   Convergence loop results"
fi
if [[ -f "$RUN_DIR/blackbox-acceptance.json" ]]; then
    echo "  blackbox-acceptance.json  Model's test run results"
fi
if [[ -f "$RUN_DIR/summary.md" ]]; then
    echo "  summary.md         LLM-generated analysis"
fi
if [[ -f "$RUN_DIR/log-signals.txt" ]]; then
    echo "  log-signals.txt    Condensed agent log signals"
fi
