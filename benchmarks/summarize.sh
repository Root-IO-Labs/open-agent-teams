#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/summarize.sh --dir <results-dir> [options]

Extract log signals from an OAT benchmark run and generate an LLM-powered
summary using the model that was used to orchestrate the run (or any model
OAT supports if overridden).

Required:
  --dir <path>              Path to timestamped results directory
                            (must contain collect.json and/or acceptance.json)

Options:
  --repo <name>             OAT repo name for log lookup
                            (auto-detected from collect.json if omitted)
  --model <provider:model>  Model to use for the summary. Resolution order:
                              1. --model flag (this option)
                              2. OAT_BENCH_LLM_MODEL env var
                              3. orchestrator_model from collect.json
                              4. anthropic:claude-sonnet-4-6 (hard fallback)
                            Accepts any provider:model string OAT supports
                            (anthropic, openai, google_genai, openrouter,
                            deepseek, ollama, ...).
  --output <path>           Output file (default: <dir>/summary.md)
  --signals-only            Only extract log signals, skip the LLM call
  --help                    Show this help message

Environment:
  OAT_BENCH_LLM_MODEL       Optional fallback model used when --model is
                            absent and collect.json has no orchestrator_model.
  <PROVIDER>_API_KEY        API key for whichever provider the resolved model
                            uses (ANTHROPIC_API_KEY / OPENAI_API_KEY /
                            GOOGLE_API_KEY / OPENROUTER_API_KEY / etc.).
                            Local providers (ollama:) need no key. If the
                            required key is missing the summary step is
                            skipped (signals are still extracted).

Examples:
  # Summarize a completed benchmark run using whichever model orchestrated it
  ./benchmarks/summarize.sh --dir benchmarks/results/20260304-011245-run

  # Extract signals only (no API key needed)
  ./benchmarks/summarize.sh --dir benchmarks/results/20260304-011245-run --signals-only

  # Use a different summarizer than the orchestrator
  ./benchmarks/summarize.sh --dir benchmarks/results/my-run \
      --model openai:gpt-5.2

  # Use a specific repo name for log lookup
  ./benchmarks/summarize.sh --dir benchmarks/results/my-run --repo oat-robotic-barista-sonnet46
EOF
    exit 0
}

RESULTS_DIR=""
REPO_NAME=""
# LLM_MODEL is resolved after we read collect.json so the orchestrator
# model can win if the user didn't pass --model. Empty here means "not
# explicitly set on the CLI".
LLM_MODEL=""
OUTPUT=""
SIGNALS_ONLY=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --dir) RESULTS_DIR="$2"; shift 2 ;;
        --repo) REPO_NAME="$2"; shift 2 ;;
        --model) LLM_MODEL="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --signals-only) SIGNALS_ONLY=true; shift ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$RESULTS_DIR" ]]; then
    echo "Error: --dir is required"
    echo "Run with --help for usage."
    exit 1
fi

if [[ ! -d "$RESULTS_DIR" ]]; then
    echo "Error: Directory not found: ${RESULTS_DIR}"
    exit 1
fi

if [[ -z "$OUTPUT" ]]; then
    OUTPUT="${RESULTS_DIR}/summary.md"
fi

SIGNALS_FILE="${RESULTS_DIR}/log-signals.txt"
COLLECT_JSON="${RESULTS_DIR}/collect.json"
ACCEPTANCE_JSON="${RESULTS_DIR}/acceptance.json"
GATE_JSON="${RESULTS_DIR}/gate.json"
BLACKBOX_ACCEPTANCE_JSON="${RESULTS_DIR}/blackbox-acceptance.json"
CONVERGENCE_JSON="${RESULTS_DIR}/convergence.json"

# --- Auto-detect repo name from collect.json ---

if [[ -z "$REPO_NAME" && -f "$COLLECT_JSON" ]]; then
    REPO_FULL=$(jq -r '.repo // empty' "$COLLECT_JSON" 2>/dev/null || true)
    if [[ -n "$REPO_FULL" ]]; then
        REPO_NAME=$(basename "$REPO_FULL")
    fi
fi

if [[ -z "$REPO_NAME" ]]; then
    echo "Warning: Could not detect repo name. Use --repo to specify."
    echo "         Log signal extraction will be skipped."
fi

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

# =============================================================================
# Step 1: Extract log signals
# =============================================================================

extract_signals() {
    local repo="$1"
    local output_file="$2"

    local oat_output_dir="${HOME}/.oat/output/${repo}"
    local oat_state="${HOME}/.oat/state.json"
    local oat_daemon_log="${HOME}/.oat/daemon.log"

    echo "# Log Signals for ${repo}" > "$output_file"
    echo "# Extracted at $(date -u +"%Y-%m-%dT%H:%M:%SZ")" >> "$output_file"
    echo "" >> "$output_file"

    # Determine the run's time window from collect.json
    local run_start="" run_end=""
    if [[ -f "$COLLECT_JSON" ]]; then
        run_start=$(jq -r '.waves["0"].timing.started_at // empty' "$COLLECT_JSON" 2>/dev/null || true)
        run_end=$(jq -r '.collected_at // empty' "$COLLECT_JSON" 2>/dev/null || true)
    fi

    # --- Identify relevant worker logs by modification time ---

    local workers_dir="${oat_output_dir}/workers"
    if [[ ! -d "$workers_dir" ]]; then
        echo "## NO WORKER LOGS FOUND" >> "$output_file"
        echo "Directory not found: ${workers_dir}" >> "$output_file"
        return
    fi

    local worker_logs=()
    local run_start_epoch="" run_end_epoch=""

    # Parse ISO timestamps to epoch (macOS needs TZ=UTC to handle the trailing Z correctly)
    if [[ -n "$run_start" && "$run_start" != "null" ]]; then
        if date -j &>/dev/null 2>&1; then
            run_start_epoch=$(TZ=UTC date -j -f "%Y-%m-%dT%H:%M:%SZ" "$run_start" +%s 2>/dev/null || true)
        else
            run_start_epoch=$(date -d "$run_start" +%s 2>/dev/null || true)
        fi
    fi
    if [[ -n "$run_end" && "$run_end" != "null" ]]; then
        if date -j &>/dev/null 2>&1; then
            run_end_epoch=$(TZ=UTC date -j -f "%Y-%m-%dT%H:%M:%SZ" "$run_end" +%s 2>/dev/null || true)
        else
            run_end_epoch=$(date -d "$run_end" +%s 2>/dev/null || true)
        fi
    fi

    if [[ -n "$run_start_epoch" && -n "$run_end_epoch" ]]; then
        # Allow 5 min buffer before start and 10 min after end
        local window_start=$((run_start_epoch - 300))
        local window_end=$((run_end_epoch + 600))

        while IFS= read -r logfile; do
            local mod_epoch
            if stat -f %m "$logfile" &>/dev/null 2>&1; then
                mod_epoch=$(stat -f %m "$logfile")
            else
                mod_epoch=$(stat -c %Y "$logfile" 2>/dev/null || echo "0")
            fi
            if [[ "$mod_epoch" -ge "$window_start" && "$mod_epoch" -le "$window_end" ]]; then
                worker_logs+=("$logfile")
            fi
        done < <(find "$workers_dir" -name "*.log" -type f 2>/dev/null)
    else
        # Fallback: use the 30 most recently modified logs (exclude rotated archives)
        while IFS= read -r logfile; do
            worker_logs+=("$logfile")
        done < <(ls -t "$workers_dir"/*.log 2>/dev/null | head -30)
    fi

    local worker_count=${#worker_logs[@]}
    echo "## WORKER LOGS (${worker_count} files from run window)" >> "$output_file"
    echo "" >> "$output_file"

    if [[ $worker_count -eq 0 ]]; then
        echo "(no worker logs found in run window)" >> "$output_file"
        echo "" >> "$output_file"
        # Skip to system agent logs
    fi

    for logfile in ${worker_logs[@]+"${worker_logs[@]}"}; do
        local worker_name
        worker_name=$(basename "$logfile" | sed 's/\.log.*//')
        local file_size
        file_size=$(wc -c < "$logfile" | tr -d ' ')
        local file_size_mb
        file_size_mb=$(awk "BEGIN { printf \"%.1f\", $file_size / 1048576 }")

        echo "### Worker: ${worker_name} (${file_size_mb} MB)" >> "$output_file"

        # Task info from state.json
        if [[ -f "$oat_state" ]]; then
            local task_info
            task_info=$(jq -r --arg name "$worker_name" --arg repo "$repo" \
                '.repos[$repo].task_history[]? | select(.name == $name) | "Task: \(.task)\nBranch: \(.branch)\nStatus: \(.status)\nModel: \(.model // "unknown")"' \
                "$oat_state" 2>/dev/null || true)
            if [[ -n "$task_info" ]]; then
                echo "$task_info" >> "$output_file"
            fi
        fi

        echo "" >> "$output_file"

        # First 5 lines (startup context) -- truncated to 200 chars/line
        echo "--- First lines ---" >> "$output_file"
        head -5 "$logfile" 2>/dev/null | LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' | cut -c1-200 >> "$output_file" || true
        echo "" >> "$output_file"

        # Last 5 lines (completion context) -- truncated to 200 chars/line
        echo "--- Last lines ---" >> "$output_file"
        tail -5 "$logfile" 2>/dev/null | LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' | cut -c1-200 >> "$output_file" || true
        echo "" >> "$output_file"

        # Key signals (grep with ANSI stripping, deduped via uniq to collapse TUI re-renders)
        echo "--- Key signals ---" >> "$output_file"
        LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' "$logfile" 2>/dev/null | \
            grep -iE 'error[: ]|merge conflict|CONFLICTING|CI fail|checks fail|gh pr create|oat agent complete|oat agent waiting|\[daemon\]|stuck|timeout' 2>/dev/null | \
            grep -ivE 'no error|error.?free|without error|0 errors|error.?handler|error.?handling|error.?message|error.?code|error.?class|test.*error|ErrorResponse' 2>/dev/null | \
            cut -c1-200 | uniq | tail -20 >> "$output_file" || true
        echo "" >> "$output_file"

        # Detect repeated patterns that indicate loops.
        # Nudge and completion counts use daemon.log (accurate) rather than
        # worker output logs (inflated by Textual TUI re-renders).
        echo "--- Loop detection ---" >> "$output_file"
        local complete_count rebase_count nudge_count synced_count
        if [[ -f "$oat_daemon_log" ]]; then
            nudge_count=$(grep -c "Nudged worker ${worker_name}.*${repo}" "$oat_daemon_log" 2>/dev/null) || nudge_count=0
            complete_count=$(grep -c "${repo}/${worker_name} marked as ready for cleanup" "$oat_daemon_log" 2>/dev/null) || complete_count=0
            rebase_count=$(grep "Woke worker.*${repo}/${worker_name}" "$oat_daemon_log" 2>/dev/null | grep -ci "conflict\|rebase" 2>/dev/null) || rebase_count=0
            synced_count=$(grep -c "Notified active worker.*${repo}/${worker_name}" "$oat_daemon_log" 2>/dev/null) || synced_count=0
        else
            nudge_count=0
            complete_count=0
            rebase_count=0
            synced_count=0
        fi
        if [[ "$complete_count" -gt 2 ]]; then
            echo "WARNING: worker marked for cleanup ${complete_count} times (possible loop)" >> "$output_file"
        fi
        if [[ "$rebase_count" -gt 3 ]]; then
            echo "WARNING: woken for conflicts ${rebase_count} times (possible rebase loop)" >> "$output_file"
        fi
        if [[ "$nudge_count" -gt 3 ]]; then
            echo "WARNING: ${nudge_count} daemon nudges (possible stuck worker)" >> "$output_file"
        fi
        if [[ "$synced_count" -gt 1 ]]; then
            echo "WARNING: ${synced_count} worktree sync notifications (possible message flood)" >> "$output_file"
        fi
        if [[ "$complete_count" -le 2 && "$rebase_count" -le 3 && "$nudge_count" -le 3 && "$synced_count" -le 1 ]]; then
            echo "(no loops detected)" >> "$output_file"
        fi
        echo "" >> "$output_file"
    done

    # --- System agent logs (supervisor, merge-queue, workspace) ---

    echo "## SYSTEM AGENT LOGS" >> "$output_file"
    echo "" >> "$output_file"

    for agent_type in supervisor merge-queue default; do
        local agent_label="$agent_type"
        if [[ "$agent_type" == "default" ]]; then
            agent_label="workspace"
        fi

        # Find the most recent log for this agent type
        local agent_log=""
        agent_log=$(ls -t "${oat_output_dir}/${agent_type}.log"* 2>/dev/null | head -1 || true)

        if [[ -z "$agent_log" || ! -f "$agent_log" ]]; then
            echo "### ${agent_label}: no log found" >> "$output_file"
            echo "" >> "$output_file"
            continue
        fi

        local file_size
        file_size=$(wc -c < "$agent_log" | tr -d ' ')
        local file_size_mb
        file_size_mb=$(awk "BEGIN { printf \"%.1f\", $file_size / 1048576 }")

        echo "### ${agent_label} (${file_size_mb} MB)" >> "$output_file"
        echo "" >> "$output_file"

        # Extract key signals
        echo "--- Key signals ---" >> "$output_file"
        LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' "$agent_log" 2>/dev/null | \
            grep -iE 'worker.*creat|worker.*remov|worker.*spawn|oat worker|merge.*pr|pr.*merg|identity|multiclaude|"I don.t have a formal role"|wave|error[: ]|\[daemon\]|stuck|idle.?mode' 2>/dev/null | \
            grep -ivE 'error.?handler|error.?handling|error.?message|error.?code|error.?class|test.*error|ErrorResponse' 2>/dev/null | \
            cut -c1-200 | tail -40 >> "$output_file" || true
        echo "" >> "$output_file"
    done

    # --- Daemon log ---

    if [[ -f "$oat_daemon_log" ]]; then
        echo "## DAEMON LOG SIGNALS" >> "$output_file"
        echo "" >> "$output_file"

        local daemon_signals=""
        if [[ -n "$run_start_epoch" ]]; then
            # Extract signals from the daemon log during the run window
            daemon_signals=$(LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' "$oat_daemon_log" 2>/dev/null | \
                grep -iE 'nudge|force.?remov|auto.?complet|pr.?monitor|wake|idle.?mode|fetch.*fail|health.*check.*dead|restart.*agent|ready.*cleanup|marked.*cleanup' 2>/dev/null | \
                tail -60 || true)
        else
            daemon_signals=$(LC_ALL=C sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' "$oat_daemon_log" 2>/dev/null | \
                grep -iE 'nudge|force.?remov|auto.?complet|pr.?monitor|wake|idle.?mode|fetch.*fail|health.*check.*dead|restart.*agent|ready.*cleanup|marked.*cleanup' 2>/dev/null | \
                tail -60 || true)
        fi

        if [[ -n "$daemon_signals" ]]; then
            echo "$daemon_signals" >> "$output_file"
        else
            echo "(no relevant daemon signals found)" >> "$output_file"
        fi
        echo "" >> "$output_file"
    fi

    local signals_size
    signals_size=$(wc -c < "$output_file" | tr -d ' ')
    local signals_kb
    signals_kb=$(awk "BEGIN { printf \"%.1f\", $signals_size / 1024 }")
    log "Log signals extracted: ${signals_kb} KB -> ${output_file}"
}

# =============================================================================
# Step 2: Resolve model + call LLM
# =============================================================================

# Resolve the summarizer model using the documented priority chain.
# Sets the global RESOLVED_MODEL. Echoes a one-line log explaining the source.
resolve_summary_model() {
    if [[ -n "$LLM_MODEL" ]]; then
        RESOLVED_MODEL="$LLM_MODEL"
        log "Summary model: ${RESOLVED_MODEL} (from --model)"
        return 0
    fi
    if [[ -n "${OAT_BENCH_LLM_MODEL:-}" ]]; then
        RESOLVED_MODEL="$OAT_BENCH_LLM_MODEL"
        log "Summary model: ${RESOLVED_MODEL} (from OAT_BENCH_LLM_MODEL)"
        return 0
    fi
    if [[ -f "$COLLECT_JSON" ]]; then
        local from_collect
        from_collect=$(jq -r '.orchestrator_model // .model // empty' "$COLLECT_JSON" 2>/dev/null || true)
        if [[ -n "$from_collect" && "$from_collect" != "null" ]]; then
            RESOLVED_MODEL="$from_collect"
            log "Summary model: ${RESOLVED_MODEL} (from collect.json orchestrator_model)"
            return 0
        fi
    fi
    RESOLVED_MODEL="anthropic:claude-sonnet-4-6"
    log "Summary model: ${RESOLVED_MODEL} (hard fallback — no --model, env var, or collect.json present)"
}

call_llm_api() {
    local signals_file="$1"
    local output_file="$2"

    local collect_content=""
    local acceptance_content=""
    local signals_content=""
    local gate_content=""
    local blackbox_acceptance_content=""
    local convergence_content=""

    if [[ -f "$COLLECT_JSON" ]]; then
        collect_content=$(cat "$COLLECT_JSON")
    fi
    if [[ -f "$ACCEPTANCE_JSON" ]]; then
        acceptance_content=$(cat "$ACCEPTANCE_JSON")
    fi
    if [[ -f "$signals_file" ]]; then
        signals_content=$(cat "$signals_file")
    fi
    if [[ -f "$GATE_JSON" ]]; then
        gate_content=$(cat "$GATE_JSON")
    fi
    if [[ -f "$BLACKBOX_ACCEPTANCE_JSON" ]]; then
        blackbox_acceptance_content=$(cat "$BLACKBOX_ACCEPTANCE_JSON")
    fi
    if [[ -f "$CONVERGENCE_JSON" ]]; then
        convergence_content=$(cat "$CONVERGENCE_JSON")
    fi

    if [[ -z "$collect_content" && -z "$acceptance_content" ]]; then
        log "No result files found to summarize"
        return 1
    fi

    local user_prompt
    user_prompt=$(cat <<'PROMPT_END'
You are analyzing results from an automated multi-agent coding benchmark. The benchmark uses OAT (Open Agent Teams) to orchestrate multiple AI coding agents working on a shared GitHub repository. The project is "robotic-barista" -- a Python CLI app for recipe management, inventory tracking, and order processing.

Agents are organized into waves (wave:0-4) where each wave depends on the previous one completing. Workers are spawned per issue and work in parallel within a wave. A supervisor monitors progress, a merge-queue handles PR merges, and a workspace agent orchestrates wave progression.

IMPORTANT CONTEXT -- Blackbox Gate:
The benchmark has a "blackbox gate" phase (wave:0) that runs BEFORE the main wave progression. Issues #1-#4 are the gate tasks (wave:0 label) where parallel workers generate modular blackbox acceptance test modules from the spec. If gate.json is present in the data, this run included a gate phase. If all waves show "skipped" or zero timing, the run was either gate-only (--gate-only flag) or the gate failed. Do NOT confuse the gate workers' activity with Wave 1 work. Wave 1 issues are #5-9 (interface contracts, domain entities, etc.), NOT the gate issues #1-4.

IMPORTANT CONTEXT -- Convergence Loop:
After waves 1-4 complete, the benchmark enters a convergence loop. The model-generated blackbox test is run against the built application. If it fails (binary pass/fail based on exit code), the failure report is sent to the workspace agent, which creates fix issues (labeled wave:fix-0, wave:fix-1, etc.) and spawns workers. The test is re-run after fixes, repeating up to 4 iterations or until a convergence timeout. convergence.json contains the results of each iteration. The convergence verdict can be: "pass" (test passed), "timeout" (convergence or grand timeout), "max_iterations" (all 4 iterations exhausted), "no_progress" (identical failure lines for 2+ consecutive iterations -- fix workers had zero effect), or "skipped" (convergence was not run).

Given the data below, produce a structured markdown report:

## 1. Executive Summary
2-3 sentence overview of how the run went overall.

## 2. Scorecard
Key numbers in a table: acceptance score, issues closed, PRs merged, total duration, worker autonomy rate.

## 3. Model Assignment (multi-model runs only)
If routing.mode is "multi-model" in the collect.json data, include this section. Otherwise skip it entirely.
- **Orchestrator model**: Which model was used for system agents (workspace, supervisor, merge-queue, etc.) -- use orchestrator_model or the top-level model field.
- **Per-model worker stats**: A table grouped by model with columns: model name, workers assigned, PRs merged by those workers, average tokens per worker, and total daemon nudge count for workers on that model. Cross-reference routing.worker_models with the per-PR and token_usage.per_worker data to build this table.
- **Verifier model**: Which model was used for verification agents (verify-* workers), how many were spawned, and their aggregate stats (runs, pass rate, avg score). Present verifiers separately from the worker table since they serve a different role (code quality gating vs feature implementation).

## 4. What Went Well
Specific things that worked smoothly. Reference worker names and issue numbers.

## 5. What Went Wrong
Failures, anomalies, stuck agents, wasted effort. Be specific.

## 6. Token Efficiency Concerns
Use the `token_usage` section from collect.json for actual per-agent token counts (if available).
Report the total tokens for the run and highlight workers with unusually high usage.
Flag signs of waste: long thinking periods, unnecessary restarts, redundant work, workers that didn't produce PRs, workers with N/A token data (API hung).
If token_usage is not available, estimate from log file sizes (>15 MB suggests excessive work).

## 7. Per-Wave Breakdown
Brief summary of each wave's performance including timing and any issues.

## 8. Acceptance Test Analysis
Which categories scored well/poorly and why. Reference specific test names.

## 9. Blackbox Gate Analysis
If gate.json is available, analyze the blackbox gate results:
- What score did the model-generated test receive? Break down by dimension (feature coverage, error handling, workflow coverage, test rigor).
- Did the gate pass or fail? What does this tell us about the model's understanding of the specification?
- If the gate failed, what specific areas were weakest? What does this imply about the model's ability to converge?
If gate.json is not available (gate was skipped or not run), note this and skip.

## 10. Convergence Loop Analysis
If convergence.json is available, analyze the convergence loop results:
- Did the model-generated blackbox test pass on the first attempt after waves, or did it require fix iterations?
- How many iterations were needed? What was the verdict (pass/timeout/max_iterations)?
- What specific failures did the blackbox test catch that the waves missed?
- Did the fix iterations make progress (improving pass counts between iterations)?
If convergence data is not available (convergence was skipped), note this and skip.

## 11. Dual-Run Comparison
If both acceptance.json AND blackbox-acceptance.json are available, compare them:
- How does the reference acceptance test score compare to the model-generated test score?
- If the model's test gives a higher score than the reference test, it may be too lenient -- identify specific areas where the model's test is less rigorous.
- If both score high, the model truly understood the spec end-to-end.
If dual-run data is not available, note this and skip.

## 12. Recommendations
Actionable suggestions for improving the next run (spec changes, prompt changes, OAT config changes).

Be specific. Reference worker names, issue numbers, and exact error messages. Flag anything that looks undesirable even if the final result was correct.

IMPORTANT: Worker output logs may contain duplicate lines from terminal UI re-renders.
The "Loop detection" counts for nudges, completions, conflicts, and sync notifications use daemon.log (accurate).
Key signal lines are deduplicated via uniq but may still show some repeated patterns.
Rely on the loop detection counts, not raw key signal repetition, to assess whether loops occurred.

IMPORTANT: OAT rotates worker log files when they exceed 10MB (copy-then-truncate). This may produce
a truncated active log for some workers. This does NOT mean the worker was spawned twice or that there
are duplicate workers. Each worker name appears exactly once in the daemon "Added agent" log.

Pay special attention to "Loop detection" sections in the log signals. These flag:
- **Daemon nudge loops:** Workers nudged many times without completing, indicating a stuck worker or failure to process wake messages.
- **Conflict wakes:** Times the daemon woke a dormant worker due to merge conflicts (from daemon.log, accurate count).
- **Message floods:** Multiple worktree sync notifications sent to active workers.
Report any detected loops prominently under "What Went Wrong" with the affected worker names.
PROMPT_END
)

    # Append the data
    user_prompt+=$'\n\n--- OPERATIONAL METRICS (collect.json) ---\n'
    if [[ -n "$collect_content" ]]; then
        user_prompt+="$collect_content"
    else
        user_prompt+="(not available)"
    fi

    user_prompt+=$'\n\n--- FUNCTIONAL TEST RESULTS (acceptance.json) ---\n'
    if [[ -n "$acceptance_content" ]]; then
        user_prompt+="$acceptance_content"
    else
        user_prompt+="(not available)"
    fi

    user_prompt+=$'\n\n--- BLACKBOX GATE RESULTS (gate.json) ---\n'
    if [[ -n "$gate_content" ]]; then
        user_prompt+="$gate_content"
    else
        user_prompt+="(not available -- gate was skipped or not run)"
    fi

    user_prompt+=$'\n\n--- CONVERGENCE LOOP RESULTS (convergence.json) ---\n'
    if [[ -n "$convergence_content" ]]; then
        user_prompt+="$convergence_content"
    else
        user_prompt+="(not available -- convergence loop was not run or was skipped)"
    fi

    user_prompt+=$'\n\n--- MODEL-GENERATED TEST RUN RESULTS (blackbox-acceptance.json) ---\n'
    if [[ -n "$blackbox_acceptance_content" ]]; then
        user_prompt+="$blackbox_acceptance_content"
    else
        user_prompt+="(not available -- dual-run was not performed)"
    fi

    user_prompt+=$'\n\n--- CONDENSED AGENT LOG SIGNALS ---\n'
    if [[ -n "$signals_content" ]]; then
        # Truncate if too large (50K chars keeps payload manageable for API timeouts)
        if [[ ${#signals_content} -gt 50000 ]]; then
            user_prompt+="${signals_content:0:50000}"
            user_prompt+=$'\n\n(log signals truncated at 50KB)'
        else
            user_prompt+="$signals_content"
        fi
    else
        user_prompt+="(not available)"
    fi

    log "Calling LLM via llm_call.py (model: ${RESOLVED_MODEL})..."

    # Build the payload as JSON via jq so embedded quotes/newlines in the
    # prompt are escaped safely.
    local payload_file
    payload_file=$(mktemp)
    # RETURN trap fires on function return (success or failure) so the
    # tempfile is always cleaned up.
    trap "rm -f '$payload_file'" RETURN

    jq -n \
        --arg content "$user_prompt" \
        '{
            messages: [{role: "user", content: $content}],
            max_tokens: 8192
        }' > "$payload_file"

    local response_json=""
    local helper_exit=0
    response_json=$(python3 "${SCRIPT_DIR}/llm_call.py" \
        --model "$RESOLVED_MODEL" \
        --payload "$payload_file") || helper_exit=$?

    if [[ $helper_exit -ne 0 ]]; then
        case $helper_exit in
            2) log "LLM helper: missing API key for the resolved provider. See stderr above." ;;
            3) log "LLM helper: provider call failed after retries. See stderr above." ;;
            4) log "LLM helper: model resolution failed. See stderr above." ;;
            *) log "LLM helper exited with code ${helper_exit}. See stderr above." ;;
        esac
        return 1
    fi

    if [[ -z "$response_json" ]]; then
        log "LLM helper returned no JSON output (unexpected — see stderr)"
        return 1
    fi

    local summary input_tokens output_tokens resolved_provider_model
    summary=$(echo "$response_json" | jq -r '.text // empty')
    input_tokens=$(echo "$response_json" | jq -r '.input_tokens // 0')
    output_tokens=$(echo "$response_json" | jq -r '.output_tokens // 0')
    resolved_provider_model=$(echo "$response_json" | jq -r '.model // empty')

    if [[ -z "$summary" ]]; then
        log "LLM returned empty text content"
        return 1
    fi

    # Provenance header: HTML comment so it doesn't render in GitHub's
    # markdown view but is grep-able. Use the resolved provider:model
    # string so a bare "claude-sonnet-4-6" gets recorded as
    # "anthropic:claude-sonnet-4-6".
    local provenance_model="${resolved_provider_model:-$RESOLVED_MODEL}"
    local generated_at
    generated_at=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    {
        echo "<!-- Generated by ${provenance_model} at ${generated_at} -->"
        echo "$summary"
    } > "$output_file"

    log "Summary generated: ${output_file}"
    log "Token usage: ${input_tokens} input, ${output_tokens} output"
}

# =============================================================================
# Main
# =============================================================================

echo ""
log "========== Benchmark Summary =========="
echo ""

# Step 1: Extract log signals
if [[ -n "$REPO_NAME" ]]; then
    log "Extracting log signals for ${REPO_NAME}..."
    extract_signals "$REPO_NAME" "$SIGNALS_FILE"
else
    log "Skipping log signal extraction (no repo name available)"
fi

# --- Print model assignment summary ---
if [[ -f "$COLLECT_JSON" ]] && jq -e '.routing.per_model | length > 0' "$COLLECT_JSON" &>/dev/null; then
    log "Model Assignment:"

    # Orchestrators: detect types from token_usage presence
    _orch_types=$(jq -r '[
        (if .token_usage.workspace_tokens then "workspace" else empty end),
        (if .token_usage.supervisor_tokens then "supervisor" else empty end),
        (if .token_usage.merge_queue_tokens then "merge-queue" else empty end)
    ] | if length == 0 then ["workspace", "supervisor", "merge-queue"] else . end
    | join(", ")' "$COLLECT_JSON")
    _orch_model=$(jq -r '.orchestrator_model // .model // "unknown"' "$COLLECT_JSON")
    echo "  Orchestrators (${_orch_types}):"
    echo "    ${_orch_model}"

    # Verifiers: verify-* entries from worker_models
    _verify_count=$(jq '[.routing.worker_models // {} | keys[] | select(startswith("verify-"))] | length' "$COLLECT_JSON")
    if [[ "$_verify_count" -gt 0 ]]; then
        _verify_model=$(jq -r '
            [.routing.worker_models // {} | to_entries[] | select(.key | startswith("verify-"))]
            | first | .value // "unknown"' "$COLLECT_JSON")
        echo "  Verifiers (${_verify_count} spawned):"
        echo "    ${_verify_model}"
    fi

    # Workers: non-verify entries grouped by model, sorted by count descending
    echo "  Workers:"
    jq -r '
        (.routing.worker_models // {}) | to_entries
        | map(select(.key | startswith("verify-") | not))
        | group_by(.value) | map({model: .[0].value, count: length})
        | sort_by(-.count) | .[]
        | "\(.model)\t\(.count)"
    ' "$COLLECT_JSON" | while IFS=$'\t' read -r _model _count; do
        printf "    %-35s %d workers\n" "$_model" "$_count"
    done

    echo ""
fi

# Step 2: Generate LLM summary
if [[ "$SIGNALS_ONLY" == true ]]; then
    log "Signals-only mode, skipping LLM summary"
else
    resolve_summary_model
    # llm_call.py handles the missing-API-key case with a clear error and
    # exit code 2. The summary is optional, so we don't want a missing key
    # to fail the whole benchmark — log the helper's complaint and move on.
    if call_llm_api "$SIGNALS_FILE" "$OUTPUT"; then
        echo ""
        log "--- Summary Preview ---"
        head -20 "$OUTPUT"
        echo "..."
        log "(full summary at ${OUTPUT})"
    else
        log "LLM summary generation skipped or failed (see helper messages above)"
    fi
fi

echo ""
log "Done."
