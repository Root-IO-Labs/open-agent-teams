#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=./lib.sh
source "${SCRIPT_DIR}/lib.sh"

GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/collect.sh --repo <name> [options]

Collect benchmark results from a completed OAT run into a JSON report.

Required:
  --repo <name>             Benchmark repo name (under Root-IO-Labs)

Options:
  --output <path>           Output file path (default: benchmarks/results/<repo>.json)
  --help                    Show this help message

Examples:
  ./benchmarks/collect.sh --repo oat-robotic-barista-sonnet46
  ./benchmarks/collect.sh --repo oat-robotic-barista-sonnet46 --output results/baseline.json
EOF
    exit 0
}

REPO_NAME=""
OUTPUT=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --repo) REPO_NAME="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$REPO_NAME" ]]; then
    echo "Error: --repo is required"
    echo "Run with --help for usage."
    exit 1
fi

if [[ -z "$OUTPUT" ]]; then
    OUTPUT="${SCRIPT_DIR}/results/${REPO_NAME}.json"
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

# --- Preflight checks ---

GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
if [[ -z "$GH_TOKEN" ]]; then
    echo "Error: GH_TOKEN or GH_TOKEN_CLASSIC must be set."
    echo "A GitHub token with 'repo' scope is required for benchmark operations."
    exit 1
fi

if ! command -v gh &>/dev/null; then
    echo "Error: 'gh' (GitHub CLI) is required but not found."
    exit 1
fi

if ! command -v jq &>/dev/null; then
    echo "Error: 'jq' is required but not found."
    exit 1
fi

export GH_TOKEN

# Validate a JSON variable; returns 0 if valid, 1 if not
validate_json() { echo "$1" | jq empty 2>/dev/null; }

# Ensure a variable holds valid JSON for --argjson; fall back to default
ensure_json() {
    local val="$1"
    local fallback="${2:-}"
    [[ -z "$fallback" ]] && fallback='{}'
    if [[ -z "$val" ]] || ! validate_json "$val"; then
        echo "$fallback"
    else
        echo "$val"
    fi
}

# Ensure a variable holds a valid number for --argjson; fall back to 0
ensure_number() {
    local val="$1"
    if [[ -z "$val" ]] || ! [[ "$val" =~ ^-?[0-9]+\.?[0-9]*$ ]]; then
        echo "0"
    else
        echo "$val"
    fi
}

echo "==> Collecting results for ${REPO_FULL}"

# Verify repo exists
if ! gh repo view "${REPO_FULL}" --json name &>/dev/null; then
    echo "Error: Repository ${REPO_FULL} not found or not accessible."
    exit 1
fi

# --- Detect model from repo description ---

MODEL=$(gh repo view "${REPO_FULL}" --json description --jq '.description' 2>/dev/null | sed 's/^OAT benchmark: //')
echo "    Model: ${MODEL}"

# --- Collect issue data ---

echo "==> Collecting issue data..."

ISSUES_JSON=$(gh issue list --repo "${REPO_FULL}" --state all --json number,title,state,labels,closedAt --limit 100 2>/dev/null)
[[ -z "$ISSUES_JSON" ]] && ISSUES_JSON='[]'

# Defensive: if the live list is short relative to issues.json (the static
# source of truth shipped with the benchmark), poll briefly for indexing to
# catch up, then per-issue-fetch any still-missing issues and merge them in.
# This protects summarize.sh's per-wave completion %, per-worker attribution,
# and token-cost-per-issue tables against degraded-index conditions at
# collect time. No-op when issues.json is missing (custom benchmark).
COLLECT_MISSING_NUMBERS=""
COLLECT_EXPECTED_TOTAL=0
for _wlbl in wave:0 wave:1 wave:2 wave:3 wave:4; do
    _wn=$(expected_wave_count "$_wlbl")
    [[ "$_wn" -eq 0 ]] && continue
    COLLECT_EXPECTED_TOTAL=$((COLLECT_EXPECTED_TOTAL + _wn))
    while IFS= read -r _enum; do
        [[ -z "$_enum" ]] && continue
        if ! echo "$ISSUES_JSON" | jq --argjson n "$_enum" -e 'any(.[]; .number == $n)' >/dev/null 2>&1; then
            COLLECT_MISSING_NUMBERS="${COLLECT_MISSING_NUMBERS}${_enum} "
        fi
    done <<< "$(expected_wave_issues "$_wlbl")"
done

if [[ -n "$COLLECT_MISSING_NUMBERS" ]]; then
    # Try a brief polling loop before falling back to per-issue probes; the
    # index frequently catches up within one or two ticks.
    _elapsed=0
    while [[ "$_elapsed" -lt "$OAT_INDEX_POLL_TIMEOUT" && -n "$COLLECT_MISSING_NUMBERS" ]]; do
        sleep "$OAT_INDEX_POLL_INTERVAL"
        _elapsed=$((_elapsed + OAT_INDEX_POLL_INTERVAL))
        ISSUES_JSON=$(gh issue list --repo "${REPO_FULL}" --state all --json number,title,state,labels,closedAt --limit 100 2>/dev/null)
        [[ -z "$ISSUES_JSON" ]] && ISSUES_JSON='[]'
        _still=""
        for _n in $COLLECT_MISSING_NUMBERS; do
            if ! echo "$ISSUES_JSON" | jq --argjson n "$_n" -e 'any(.[]; .number == $n)' >/dev/null 2>&1; then
                _still="${_still}${_n} "
            fi
        done
        COLLECT_MISSING_NUMBERS="$_still"
    done

    # Anything still missing: per-issue fetch + merge into ISSUES_JSON.
    _lagging_disp=""
    for _n in $COLLECT_MISSING_NUMBERS; do
        _obj=$(_lib_gh_issue_view_json "$REPO_FULL" "$_n")
        if [[ -n "$_obj" ]]; then
            ISSUES_JSON=$(echo "$ISSUES_JSON" | jq --argjson obj "$_obj" '. + [$obj]')
            _lagging_disp="${_lagging_disp}#${_n} "
        else
            echo "ERROR:   gh issue #${_n} missing from repo at collect time (per-issue probe failed) -- excluded from analysis" >&2
        fi
    done
    if [[ -n "$_lagging_disp" ]]; then
        _lagging_disp="${_lagging_disp% }"
        echo "WARNING: gh issue list lagging at collect time -- merged ${_lagging_disp} via per-issue probes (likely GitHub indexing degradation, see https://www.githubstatus.com/)" >&2
    fi
fi

ISSUES_TOTAL=$(echo "$ISSUES_JSON" | jq 'length')
ISSUES_CLOSED=$(echo "$ISSUES_JSON" | jq '[.[] | select(.state == "CLOSED")] | length')
ISSUES_OPEN=$(echo "$ISSUES_JSON" | jq '[.[] | select(.state == "OPEN")] | length')

echo "    Issues: ${ISSUES_TOTAL} total, ${ISSUES_CLOSED} closed, ${ISSUES_OPEN} open"

# Per-wave breakdown
wave_stats() {
    local wave_label="$1"
    local total closed
    total=$(echo "$ISSUES_JSON" | jq --arg w "$wave_label" '[.[] | select(.labels[]?.name == $w)] | length')
    closed=$(echo "$ISSUES_JSON" | jq --arg w "$wave_label" '[.[] | select(.labels[]?.name == $w and .state == "CLOSED")] | length')
    echo "{\"issues\": ${total}, \"closed\": ${closed}}"
}

WAVE0=$(wave_stats "wave:0")
WAVE1=$(wave_stats "wave:1")
WAVE2=$(wave_stats "wave:2")
WAVE3=$(wave_stats "wave:3")
WAVE4=$(wave_stats "wave:4")

# --- Collect PR data ---

echo "==> Collecting PR data..."

PRS_JSON=$(gh pr list --repo "${REPO_FULL}" --state all --json number,title,state,createdAt,mergedAt,headRefName,statusCheckRollup --limit 100 2>/dev/null)
PRS_TOTAL=$(echo "$PRS_JSON" | jq 'length')
PRS_MERGED=$(echo "$PRS_JSON" | jq '[.[] | select(.state == "MERGED")] | length')
PRS_OPEN=$(echo "$PRS_JSON" | jq '[.[] | select(.state == "OPEN")] | length')
PRS_CLOSED=$(echo "$PRS_JSON" | jq '[.[] | select(.state == "CLOSED" and .mergedAt == "")] | length')

# CI status per PR
PRS_CI_PASSED=$(echo "$PRS_JSON" | jq '[.[] | select(.statusCheckRollup != null) | select([.statusCheckRollup[]? | select(.conclusion == "SUCCESS" or .conclusion == "success")] | length > 0)] | length')
PRS_CI_FAILED=$(echo "$PRS_JSON" | jq '[.[] | select(.statusCheckRollup != null) | select([.statusCheckRollup[]? | select(.conclusion == "FAILURE" or .conclusion == "failure")] | length > 0)] | length')

echo "    PRs: ${PRS_TOTAL} total, ${PRS_MERGED} merged, ${PRS_OPEN} open, ${PRS_CLOSED} closed (unmerged)"
echo "    CI: ${PRS_CI_PASSED} passed, ${PRS_CI_FAILED} failed"

# Build per-PR detail array
PRS_DETAIL=$(echo "$PRS_JSON" | jq '[.[] | {
    number: .number,
    title: .title,
    state: .state,
    branch: .headRefName,
    ci_status: (if .statusCheckRollup == null then "unknown"
                elif ([.statusCheckRollup[]? | select(.conclusion == "FAILURE" or .conclusion == "failure")] | length > 0) then "failure"
                elif ([.statusCheckRollup[]? | select(.conclusion == "SUCCESS" or .conclusion == "success")] | length > 0) then "success"
                else "pending" end),
    merged: (.state == "MERGED")
}]')

# Add per-wave PR merge counts (checks title, body, and GitHub's closingIssuesReferences)
wave_prs_merged() {
    local wave_label="$1"
    local wave_issues merged_count
    wave_issues=$(echo "$ISSUES_JSON" | jq -r --arg w "$wave_label" '[.[] | select(.labels[]?.name == $w)] | .[].number')
    merged_count=0
    for issue_num in $wave_issues; do
        local title_match body_match
        # Check PR title for #N reference
        title_match=$(echo "$PRS_JSON" | jq --arg n "$issue_num" '[.[] | select(.state == "MERGED") | select(.title | test("#" + $n + "\\b"))] | length')
        if [[ "$title_match" -gt 0 ]]; then
            merged_count=$((merged_count + title_match))
            continue
        fi
        # Fallback: check if any merged PR closes this issue via GitHub API
        body_match=$(gh pr list --repo "${REPO_FULL}" --state merged --search "closes #${issue_num} OR fixes #${issue_num}" --json number --limit 5 2>/dev/null | jq 'length')
        if [[ "$body_match" -gt 0 ]]; then
            merged_count=$((merged_count + 1))
        fi
    done
    echo "$merged_count"
}

WAVE0_MERGED=$(wave_prs_merged "wave:0")
WAVE1_MERGED=$(wave_prs_merged "wave:1")
WAVE2_MERGED=$(wave_prs_merged "wave:2")
WAVE3_MERGED=$(wave_prs_merged "wave:3")
WAVE4_MERGED=$(wave_prs_merged "wave:4")

# Per-wave timing: earliest PR created → latest PR merged for issues in that wave
wave_timing() {
    local wave_label="$1"
    local wave_issues issue_num
    local earliest_created="" latest_merged=""

    wave_issues=$(echo "$ISSUES_JSON" | jq -r --arg w "$wave_label" '[.[] | select(.labels[]?.name == $w)] | .[].number')

    for issue_num in $wave_issues; do
        local pr_created pr_merged
        pr_created=$(echo "$PRS_JSON" | jq -r --arg n "$issue_num" \
            '[.[] | select(.title | test("#" + $n + "\\b"))] | sort_by(.createdAt) | first | .createdAt // empty')
        pr_merged=$(echo "$PRS_JSON" | jq -r --arg n "$issue_num" \
            '[.[] | select(.state == "MERGED") | select(.title | test("#" + $n + "\\b"))] | sort_by(.mergedAt) | last | .mergedAt // empty')

        # Fallback: if title-based search found nothing, search PRs that close this issue via body
        if [[ -z "$pr_created" || "$pr_created" == "null" ]]; then
            local fallback_json
            fallback_json=$(gh pr list --repo "${REPO_FULL}" --state merged --search "closes #${issue_num} OR fixes #${issue_num}" --json number,createdAt,mergedAt --limit 5 2>/dev/null || echo "[]")
            if [[ "$(echo "$fallback_json" | jq 'length')" -gt 0 ]]; then
                pr_created=$(echo "$fallback_json" | jq -r 'sort_by(.createdAt) | first | .createdAt // empty')
                pr_merged=$(echo "$fallback_json" | jq -r 'sort_by(.mergedAt) | last | .mergedAt // empty')
            fi
        fi

        if [[ -n "$pr_created" && "$pr_created" != "null" ]]; then
            if [[ -z "$earliest_created" ]] || [[ "$pr_created" < "$earliest_created" ]]; then
                earliest_created="$pr_created"
            fi
        fi
        if [[ -n "$pr_merged" && "$pr_merged" != "null" ]]; then
            if [[ -z "$latest_merged" ]] || [[ "$pr_merged" > "$latest_merged" ]]; then
                latest_merged="$pr_merged"
            fi
        fi
    done

    # Fallback: if no PR data, use issue closedAt timestamps
    if [[ -z "$earliest_created" || -z "$latest_merged" ]]; then
        for issue_num in $wave_issues; do
            local closed_at
            closed_at=$(echo "$ISSUES_JSON" | jq -r --argjson n "$issue_num" \
                '[.[] | select(.number == $n)] | first | .closedAt // empty')
            if [[ -n "$closed_at" && "$closed_at" != "null" ]]; then
                if [[ -z "$earliest_created" ]] || [[ "$closed_at" < "$earliest_created" ]]; then
                    earliest_created="$closed_at"
                fi
                if [[ -z "$latest_merged" ]] || [[ "$closed_at" > "$latest_merged" ]]; then
                    latest_merged="$closed_at"
                fi
            fi
        done
    fi

    local duration_seconds="null"
    if [[ -n "$earliest_created" && -n "$latest_merged" ]]; then
        local start_epoch end_epoch
        if date -j &>/dev/null 2>&1; then
            # macOS
            start_epoch=$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$earliest_created" +%s 2>/dev/null || echo "")
            end_epoch=$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$latest_merged" +%s 2>/dev/null || echo "")
        else
            # Linux
            start_epoch=$(date -d "$earliest_created" +%s 2>/dev/null || echo "")
            end_epoch=$(date -d "$latest_merged" +%s 2>/dev/null || echo "")
        fi
        if [[ -n "$start_epoch" && -n "$end_epoch" ]]; then
            duration_seconds=$((end_epoch - start_epoch))
        fi
    fi

    jq -n \
        --arg started "$earliest_created" \
        --arg completed "$latest_merged" \
        --argjson duration "$duration_seconds" \
        '{
            started_at: (if $started == "" then null else $started end),
            completed_at: (if $completed == "" then null else $completed end),
            duration_seconds: $duration
        }'
}

echo "==> Computing per-wave timing..."

# Prefer wave-timing.json from run.sh (actual wall-clock timestamps) over PR-derived timing
WAVE_TIMING_FILE="$(dirname "$OUTPUT")/wave-timing.json"

wave_timing_from_file() {
    local wave_num="$1"
    local start_epoch end_epoch duration_seconds
    start_epoch=$(jq -r --arg w "$wave_num" '.[$w].started_epoch // 0' "$WAVE_TIMING_FILE")
    end_epoch=$(jq -r --arg w "$wave_num" '.[$w].completed_epoch // 0' "$WAVE_TIMING_FILE")
    if [[ "$start_epoch" -gt 0 && "$end_epoch" -gt 0 ]]; then
        duration_seconds=$((end_epoch - start_epoch))
        local started_at completed_at
        if date -j &>/dev/null 2>&1; then
            started_at=$(date -u -j -f "%s" "$start_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
            completed_at=$(date -u -j -f "%s" "$end_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
        else
            started_at=$(date -u -d "@$start_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
            completed_at=$(date -u -d "@$end_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
        fi
        jq -n --arg s "$started_at" --arg c "$completed_at" --argjson d "$duration_seconds" \
            '{started_at: $s, completed_at: $c, duration_seconds: $d}'
    else
        wave_timing "wave:${wave_num}"
    fi
}

# Wave 0 (gate phase) always uses PR-derived timing — run.sh doesn't track wave 0 epochs
WAVE0_TIMING=$(wave_timing "wave:0")

if [[ -f "$WAVE_TIMING_FILE" ]]; then
    echo "    Using wave-timing.json from run script"
    WAVE1_TIMING=$(wave_timing_from_file "1")
    WAVE2_TIMING=$(wave_timing_from_file "2")
    WAVE3_TIMING=$(wave_timing_from_file "3")
    WAVE4_TIMING=$(wave_timing_from_file "4")
else
    WAVE1_TIMING=$(wave_timing "wave:1")
    WAVE2_TIMING=$(wave_timing "wave:2")
    WAVE3_TIMING=$(wave_timing "wave:3")
    WAVE4_TIMING=$(wave_timing "wave:4")
fi

WAVE0_MERGED=$(ensure_number "$WAVE0_MERGED")
WAVE1_MERGED=$(ensure_number "$WAVE1_MERGED")
WAVE2_MERGED=$(ensure_number "$WAVE2_MERGED")
WAVE3_MERGED=$(ensure_number "$WAVE3_MERGED")
WAVE4_MERGED=$(ensure_number "$WAVE4_MERGED")
WAVE0_TIMING=$(ensure_json "$WAVE0_TIMING" '{"duration_seconds":0}')
WAVE1_TIMING=$(ensure_json "$WAVE1_TIMING" '{"duration_seconds":0}')
WAVE2_TIMING=$(ensure_json "$WAVE2_TIMING" '{"duration_seconds":0}')
WAVE3_TIMING=$(ensure_json "$WAVE3_TIMING" '{"duration_seconds":0}')
WAVE4_TIMING=$(ensure_json "$WAVE4_TIMING" '{"duration_seconds":0}')

WAVE0=$(echo "$WAVE0" | jq --argjson m "$WAVE0_MERGED" --argjson t "$WAVE0_TIMING" '. + {prs_merged: $m, timing: $t}')
WAVE1=$(echo "$WAVE1" | jq --argjson m "$WAVE1_MERGED" --argjson t "$WAVE1_TIMING" '. + {prs_merged: $m, timing: $t}')
WAVE2=$(echo "$WAVE2" | jq --argjson m "$WAVE2_MERGED" --argjson t "$WAVE2_TIMING" '. + {prs_merged: $m, timing: $t}')
WAVE3=$(echo "$WAVE3" | jq --argjson m "$WAVE3_MERGED" --argjson t "$WAVE3_TIMING" '. + {prs_merged: $m, timing: $t}')
WAVE4=$(echo "$WAVE4" | jq --argjson m "$WAVE4_MERGED" --argjson t "$WAVE4_TIMING" '. + {prs_merged: $m, timing: $t}')

WAVE0=$(ensure_json "$WAVE0")
WAVE1=$(ensure_json "$WAVE1")
WAVE2=$(ensure_json "$WAVE2")
WAVE3=$(ensure_json "$WAVE3")
WAVE4=$(ensure_json "$WAVE4")

# Total agent-active time (sum of per-wave durations, excluding human gaps between waves)
# Re-guard timing vars: earlier jq commands can fail and leave vars empty/malformed.
WAVE0_TIMING=$(ensure_json "$WAVE0_TIMING" '{"duration_seconds":0}')
WAVE1_TIMING=$(ensure_json "$WAVE1_TIMING" '{"duration_seconds":0}')
WAVE2_TIMING=$(ensure_json "$WAVE2_TIMING" '{"duration_seconds":0}')
WAVE3_TIMING=$(ensure_json "$WAVE3_TIMING" '{"duration_seconds":0}')
WAVE4_TIMING=$(ensure_json "$WAVE4_TIMING" '{"duration_seconds":0}')
TOTAL_ACTIVE_SECONDS=$(jq -n \
    --argjson w0 "$WAVE0_TIMING" \
    --argjson w1 "$WAVE1_TIMING" \
    --argjson w2 "$WAVE2_TIMING" \
    --argjson w3 "$WAVE3_TIMING" \
    --argjson w4 "$WAVE4_TIMING" \
    '[$w0.duration_seconds, $w1.duration_seconds, $w2.duration_seconds, $w3.duration_seconds, $w4.duration_seconds] | map(select(. != null and . > 0)) | add // 0')
TOTAL_ACTIVE_SECONDS=$(ensure_number "$TOTAL_ACTIVE_SECONDS")

# --- Worker autonomy metrics ---

echo "==> Analyzing worker autonomy..."

OAT_STATE_FILE="${HOME}/.oat/state.json"
OAT_DAEMON_LOG="${HOME}/.oat/daemon.log"

WA_TOTAL=0
WA_SELF_COMPLETED=0
WA_NON_SELF=0
WA_DAEMON_AUTO_COMPLETED=0
WA_DAEMON_FORCE_REMOVED=0
WA_FAILED=0
WA_RECOVERED=0

if [[ -f "$OAT_STATE_FILE" ]]; then
    TASK_HISTORY=$(jq -r --arg repo "$REPO_NAME" '.repos[$repo].task_history // []' "$OAT_STATE_FILE" 2>/dev/null || echo "[]")
    WA_TOTAL=$(echo "$TASK_HISTORY" | jq 'length')

    WA_DAEMON_AUTO_COMPLETED=$(echo "$TASK_HISTORY" | jq '[.[] | select(.summary != null) | select(.summary | test("Auto-completed by daemon"; "i"))] | length')
    WA_DAEMON_FORCE_REMOVED=$(echo "$TASK_HISTORY" | jq '[.[] | select(.failure_reason != null) | select(.failure_reason | test("Force-removed by daemon"; "i"))] | length')
    WA_FAILED=$(echo "$TASK_HISTORY" | jq '[.[] | select(.status == "failed")] | length')
    WA_RECOVERED=$(echo "$TASK_HISTORY" | jq '[.[] | select(.status == "recovered")] | length')

    # Self-completed = total minus daemon-intervened minus failed (avoid double counting force-removed which are also failed)
    WA_NON_SELF=$((WA_DAEMON_AUTO_COMPLETED + WA_DAEMON_FORCE_REMOVED))
    WA_SELF_COMPLETED=$((WA_TOTAL - WA_NON_SELF))
    if [[ $WA_SELF_COMPLETED -lt 0 ]]; then
        WA_SELF_COMPLETED=0
    fi
else
    echo "    Warning: state.json not found at ${OAT_STATE_FILE}"
    echo "    Worker autonomy metrics will be zeroed"
fi

# Cross-check: use PR branch count if it exceeds task_history count
# (task_history can be incomplete if cleanup hadn't finished when collect ran)
PR_WORKER_COUNT=$(echo "$PRS_JSON" | jq '[.[] | select(.headRefName | startswith("work/"))] | length')
if [[ $PR_WORKER_COUNT -gt $WA_TOTAL ]]; then
    echo "    (task_history has ${WA_TOTAL}, but ${PR_WORKER_COUNT} work/* PR branches found — using higher count)"
    WA_TOTAL=$PR_WORKER_COUNT
    WA_SELF_COMPLETED=$((WA_TOTAL - WA_NON_SELF))
    if [[ $WA_SELF_COMPLETED -lt 0 ]]; then
        WA_SELF_COMPLETED=0
    fi
fi

if [[ $WA_TOTAL -gt 0 ]]; then
    WA_SELF_RATE=$(echo "scale=2; $WA_SELF_COMPLETED / $WA_TOTAL" | bc)
else
    WA_SELF_RATE="0"
fi

echo "    Workers: ${WA_TOTAL} total"
echo "    Self-completed:        ${WA_SELF_COMPLETED}"
echo "    Daemon auto-completed: ${WA_DAEMON_AUTO_COMPLETED}"
echo "    Daemon force-removed:  ${WA_DAEMON_FORCE_REMOVED}"
echo "    Failed:                ${WA_FAILED}"
echo "    Recovered:             ${WA_RECOVERED}"
echo "    Self-completion rate:  ${WA_SELF_RATE}"

WA_TOTAL=$(ensure_number "$WA_TOTAL")
WA_SELF_COMPLETED=$(ensure_number "$WA_SELF_COMPLETED")
WA_DAEMON_AUTO_COMPLETED=$(ensure_number "$WA_DAEMON_AUTO_COMPLETED")
WA_DAEMON_FORCE_REMOVED=$(ensure_number "$WA_DAEMON_FORCE_REMOVED")
WA_FAILED=$(ensure_number "$WA_FAILED")
WA_RECOVERED=$(ensure_number "$WA_RECOVERED")
[[ -z "$WA_SELF_RATE" ]] && WA_SELF_RATE="0"

WORKER_AUTONOMY=$(jq -n \
    --argjson total "$WA_TOTAL" \
    --argjson self_completed "$WA_SELF_COMPLETED" \
    --argjson daemon_auto_completed "$WA_DAEMON_AUTO_COMPLETED" \
    --argjson daemon_force_removed "$WA_DAEMON_FORCE_REMOVED" \
    --argjson failed "$WA_FAILED" \
    --argjson recovered "$WA_RECOVERED" \
    --arg self_completion_rate "$WA_SELF_RATE" \
    '{
        total_workers: $total,
        self_completed: $self_completed,
        daemon_auto_completed: $daemon_auto_completed,
        daemon_force_removed: $daemon_force_removed,
        failed: $failed,
        recovered: $recovered,
        self_completion_rate: ($self_completion_rate | tonumber)
    }')
WORKER_AUTONOMY=$(ensure_json "$WORKER_AUTONOMY")

# --- Routing metrics ---

echo "==> Extracting routing metrics..."

_TMP_ROUTING=$(mktemp)

# Extract per-model worker assignments from state.json task history
ROUTING_JSON=$(python3 -c "
import json, sys, os

state_path = os.path.expanduser('~/.oat/state.json')
daemon_log = os.path.expanduser('~/.oat/daemon.log')
repo = '${REPO_NAME}'

result = {'mode': 'unknown', 'per_model': {}, 'auto_selected': 0, 'explicit': 0, 'total_routed': 0}

# Parse state.json for per-model worker stats
try:
    with open(state_path) as f:
        st = json.load(f)
    repo_data = st.get('repos', {}).get(repo, {})

    # Active agents
    for name, agent in repo_data.get('agents', {}).items():
        model = agent.get('model', '')
        if model and agent.get('type') == 'worker':
            result['per_model'].setdefault(model, {'workers_assigned': 0, 'from': 'active'})
            result['per_model'][model]['workers_assigned'] += 1

    # Task history
    for entry in repo_data.get('task_history', []):
        model = entry.get('model', '')
        if model:
            result['per_model'].setdefault(model, {'workers_assigned': 0, 'from': 'history'})
            result['per_model'][model]['workers_assigned'] += 1
except Exception:
    pass

# Parse daemon.log for routing decisions
try:
    with open(daemon_log) as f:
        for line in f:
            if 'Model routing:' not in line:
                continue
            if 'auto-selected' in line:
                result['auto_selected'] += 1
            elif 'validated' in line:
                result['explicit'] += 1
except Exception:
    pass

result['total_routed'] = result['auto_selected'] + result['explicit']
if result['total_routed'] > 0 and len(result['per_model']) > 1:
    result['mode'] = 'multi-model'
elif result['total_routed'] > 0:
    result['mode'] = 'single-model'

worker_models = {}
try:
    for entry in repo_data.get('task_history', []):
        name = entry.get('name', '')
        model = entry.get('model', '')
        if name and model:
            worker_models[name] = model
    for name, agent in repo_data.get('agents', {}).items():
        model = agent.get('model', '')
        if model and agent.get('type') == 'worker':
            worker_models[name] = model
except Exception:
    pass
result['worker_models'] = worker_models

print(json.dumps(result))
" 2>/dev/null || echo '{}')

echo "$ROUTING_JSON" > "$_TMP_ROUTING"

# --- Verification metrics ---

echo "==> Extracting verification metrics..."

VLOG="${HOME}/.oat/verification.log"
V_TOTAL=0; V_PASSED=0; V_FAILED=0; V_SCORE_SUM=0; V_DURATION_SUM=0; V_FIX_PASS=0

if [[ -f "$VLOG" ]]; then
    # Extract metrics for this repo only
    V_METRICS=$(python3 -c "
import json, sys

repo = '${REPO_NAME}'
entries = []
with open('${VLOG}') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            e = json.loads(line)
            if e.get('repo_name') == repo:
                entries.append(e)
        except:
            pass

total = len(entries)
passed = sum(1 for e in entries if e.get('overall_passed'))
failed = total - passed
scores = [e.get('overall_score', 0) for e in entries]
avg_score = sum(scores) / len(scores) if scores else 0
durations = [e.get('duration_seconds', 0) for e in entries]
avg_duration = sum(durations) / len(durations) if durations else 0

# Fix-then-pass: same agent has a fail then later a pass
agents_failed = set()
fix_pass = 0
for e in entries:
    agent = e.get('agent_name', '')
    if not e.get('overall_passed'):
        agents_failed.add(agent)
    elif agent in agents_failed:
        fix_pass += 1

# Per-check averages
checks = {}
for check_name in ('task_alignment', 'input_validation', 'file_integrity', 'syntax_validation', 'test_execution'):
    check_scores = [e.get('checks', {}).get(check_name, {}).get('score', 0) for e in entries if e.get('checks', {}).get(check_name)]
    checks[check_name] = sum(check_scores) / len(check_scores) if check_scores else 0

result = {
    'total_runs': total,
    'passed': passed,
    'failed': failed,
    'avg_score': round(avg_score, 1),
    'avg_duration_seconds': round(avg_duration, 1),
    'fix_then_pass': fix_pass,
    'per_check_avg': {k: round(v, 1) for k, v in checks.items()}
}
print(json.dumps(result))
" 2>/dev/null || echo '{}')

    V_TOTAL=$(echo "$V_METRICS" | jq -r '.total_runs // 0')
    V_PASSED=$(echo "$V_METRICS" | jq -r '.passed // 0')
    V_FAILED=$(echo "$V_METRICS" | jq -r '.failed // 0')
    V_AVG_SCORE=$(echo "$V_METRICS" | jq -r '.avg_score // 0')
    V_FIX_PASS=$(echo "$V_METRICS" | jq -r '.fix_then_pass // 0')

    echo "    Verification runs:   ${V_TOTAL}"
    echo "    Passed:              ${V_PASSED}"
    echo "    Failed:              ${V_FAILED}"
    echo "    Avg score:           ${V_AVG_SCORE}"
    echo "    Fix-then-pass:       ${V_FIX_PASS}"
else
    echo "    No verification log found at ${VLOG}"
    V_METRICS='{}'
fi

VERIFICATION_METRICS="${V_METRICS:-{}}"
VERIFICATION_METRICS=$(ensure_json "$VERIFICATION_METRICS")

# --- Token usage extraction ---
#
# Two data sources, in order of reliability:
#   1. state.json — daemon tracks per-agent cumulative tokens via [OAT_TOKENS]
#      events. This is authoritative when the daemon was running.
#   2. Agent log files — each contains [OAT_TOKENS] JSON lines. We parse the
#      LAST emission per agent (highest cumulative values).
#
# We use state.json as primary, log parsing as fallback for agents not in state.

echo "==> Extracting token usage..."

OAT_STATE_FILE="${HOME}/.oat/state.json"
OAT_OUTPUT_DIR="${HOME}/.oat/output/${REPO_NAME}"

# --- Snapshot state.json to results directory ---
# This preserves per-agent token data for post-hoc analysis.
if [[ -f "$OAT_STATE_FILE" ]]; then
    STATE_SNAPSHOT_DIR="$(dirname "$OUTPUT")"
    mkdir -p "$STATE_SNAPSHOT_DIR"
    cp "$OAT_STATE_FILE" "${STATE_SNAPSHOT_DIR}/state-snapshot.json"
    echo "    State snapshot saved to ${STATE_SNAPSHOT_DIR}/state-snapshot.json"
fi

# extract_tokens_from_state <agent_name>
# Reads input_tokens + output_tokens from state.json for a given agent.
# Outputs total tokens as an integer, or empty string if not found.
extract_tokens_from_state() {
    local agent_name="$1"
    if [[ ! -f "$OAT_STATE_FILE" ]]; then
        echo ""
        return
    fi
    local input output
    input=$(jq -r --arg repo "$REPO_NAME" --arg agent "$agent_name" \
        '.repos[$repo].agents[$agent].input_tokens // 0' "$OAT_STATE_FILE" 2>/dev/null)
    output=$(jq -r --arg repo "$REPO_NAME" --arg agent "$agent_name" \
        '.repos[$repo].agents[$agent].output_tokens // 0' "$OAT_STATE_FILE" 2>/dev/null)
    if [[ "$input" == "0" && "$output" == "0" ]] || [[ "$input" == "null" || "$output" == "null" ]]; then
        echo ""
        return
    fi
    echo $(( input + output ))
}

# extract_tokens_from_log <log_file>
# Parses the last [OAT_TOKENS] JSON line from an agent log.
# Outputs cumulative_input + cumulative_output as integer, or empty string.
extract_tokens_from_log() {
    local log_file="$1"
    if [[ ! -f "$log_file" ]]; then
        echo ""
        return
    fi
    local last_line
    last_line=$(grep '\[OAT_TOKENS\]' "$log_file" 2>/dev/null | tail -1 || true)
    if [[ -z "$last_line" ]]; then
        echo ""
        return
    fi
    # Extract JSON after the [OAT_TOKENS] prefix
    local json_part
    json_part=$(echo "$last_line" | sed 's/.*\[OAT_TOKENS\] //')
    local cum_in cum_out
    cum_in=$(echo "$json_part" | jq -r '.cumulative_input // 0' 2>/dev/null)
    cum_out=$(echo "$json_part" | jq -r '.cumulative_output // 0' 2>/dev/null)
    if [[ "$cum_in" == "0" && "$cum_out" == "0" ]] || [[ "$cum_in" == "null" ]]; then
        echo ""
        return
    fi
    echo $(( cum_in + cum_out ))
}

# extract_tokens_detailed_from_state <agent_name>
# Returns JSON object with input/output/cache breakdown, or empty string.
extract_tokens_detailed_from_state() {
    local agent_name="$1"
    if [[ ! -f "$OAT_STATE_FILE" ]]; then
        echo ""
        return
    fi
    local result
    result=$(jq -r --arg repo "$REPO_NAME" --arg agent "$agent_name" \
        '.repos[$repo].agents[$agent] // empty |
         if .input_tokens > 0 or .output_tokens > 0 then
           {input_tokens: .input_tokens, output_tokens: .output_tokens, total_tokens: (.input_tokens + .output_tokens), model: (.model // "default")}
           + (if .cache_read_tokens > 0 then {cache_read_tokens: .cache_read_tokens} else {} end)
           + (if .cache_creation_tokens > 0 then {cache_creation_tokens: .cache_creation_tokens} else {} end)
         else empty end' "$OAT_STATE_FILE" 2>/dev/null)
    echo "${result:-}"
}

# extract_agent <agent_name> <log_file>
# Primary: state.json. Fallback: log file. Returns total tokens as integer.
extract_agent_tokens() {
    local agent_name="$1"
    local log_file="$2"
    local tokens
    tokens=$(extract_tokens_from_state "$agent_name")
    if [[ -n "$tokens" && "$tokens" != "0" ]]; then
        echo "$tokens"
        return
    fi
    extract_tokens_from_log "$log_file"
}

# --- Extract system agent tokens ---
TU_WORKSPACE_RAW=$(extract_agent_tokens "default" "${OAT_OUTPUT_DIR}/default.log")
TU_SUPERVISOR_RAW=$(extract_agent_tokens "supervisor" "${OAT_OUTPUT_DIR}/supervisor.log")
TU_MERGE_QUEUE_RAW=$(extract_agent_tokens "merge-queue" "${OAT_OUTPUT_DIR}/merge-queue.log")

# --- Extract per-worker tokens ---
TU_WORKER_TOTAL=0
TU_WORKER_COUNT=0
TU_WORKER_NA=0
TU_WORKER_MIN=""
TU_WORKER_MAX=""
TU_PER_WORKER="{}"

# Collect worker names from state.json (primary) and log files (fallback).
# Bash 3.2 compat (macOS default shell): no associative arrays. Use
# jq + sort -u to produce a deduplicated newline-separated list, then
# iterate with `while IFS= read`. Same pattern used in summarize.sh.
WORKERS_DIR="${OAT_OUTPUT_DIR}/workers"
WORKER_NAMES_LIST=$(
    {
        if [[ -f "$OAT_STATE_FILE" ]]; then
            jq -r --arg repo "$REPO_NAME" \
                '.repos[$repo].agents // {} | to_entries[] | select(.value.type == "worker") | .key' \
                "$OAT_STATE_FILE" 2>/dev/null
        fi
        if [[ -d "$WORKERS_DIR" ]]; then
            for wlog in "$WORKERS_DIR"/*.log; do
                [[ -f "$wlog" ]] || continue
                basename "$wlog" .log
            done
        fi
    } | sort -u
)

while IFS= read -r wname; do
    [[ -z "$wname" ]] && continue
    wtokens=$(extract_agent_tokens "$wname" "${WORKERS_DIR}/${wname}.log")
    if [[ -n "$wtokens" && "$wtokens" != "0" ]]; then
        TU_WORKER_COUNT=$((TU_WORKER_COUNT + 1))
        TU_WORKER_TOTAL=$((TU_WORKER_TOTAL + wtokens))

        # Per-worker detail with model info
        detail=$(extract_tokens_detailed_from_state "$wname")
        if [[ -n "$detail" ]]; then
            TU_PER_WORKER=$(echo "$TU_PER_WORKER" | jq --arg name "$wname" --argjson detail "$detail" '. + {($name): $detail}')
        else
            TU_PER_WORKER=$(echo "$TU_PER_WORKER" | jq --arg name "$wname" --argjson tokens "$wtokens" '. + {($name): {total_tokens: $tokens}}')
        fi

        if [[ -z "$TU_WORKER_MIN" ]] || (( wtokens < TU_WORKER_MIN )); then
            TU_WORKER_MIN="$wtokens"
        fi
        if [[ -z "$TU_WORKER_MAX" ]] || (( wtokens > TU_WORKER_MAX )); then
            TU_WORKER_MAX="$wtokens"
        fi
    else
        TU_WORKER_NA=$((TU_WORKER_NA + 1))
    fi
done <<< "$WORKER_NAMES_LIST"

# --- Compute totals ---
TU_TOTAL=0
for val in "$TU_WORKSPACE_RAW" "$TU_SUPERVISOR_RAW" "$TU_MERGE_QUEUE_RAW" "$TU_WORKER_TOTAL"; do
    if [[ -n "$val" && "$val" != "0" ]]; then
        TU_TOTAL=$((TU_TOTAL + val))
    fi
done

# --- Human-readable formatting ---
_fmt_tokens() {
    local raw="$1"
    if [[ -z "$raw" || "$raw" == "0" ]]; then
        echo "N/A"
        return
    fi
    if (( raw >= 1000000 )); then
        printf "%.1fM" "$(echo "scale=1; $raw / 1000000" | bc)"
    elif (( raw >= 1000 )); then
        printf "%.1fK" "$(echo "scale=1; $raw / 1000" | bc)"
    else
        echo "$raw"
    fi
}

echo "    Workspace:    $(_fmt_tokens "$TU_WORKSPACE_RAW")"
echo "    Supervisor:   $(_fmt_tokens "$TU_SUPERVISOR_RAW")"
echo "    Merge-queue:  $(_fmt_tokens "$TU_MERGE_QUEUE_RAW")"
echo "    Workers:      $(_fmt_tokens "$TU_WORKER_TOTAL") (${TU_WORKER_COUNT} with data, ${TU_WORKER_NA} N/A)"
echo "    Total:        $(_fmt_tokens "$TU_TOTAL")"

# Defensive defaults
TU_WORKER_COUNT="${TU_WORKER_COUNT:-0}"
TU_WORKER_NA="${TU_WORKER_NA:-0}"
[[ "$TU_PER_WORKER" == "{}" || -z "$TU_PER_WORKER" ]] && TU_PER_WORKER='{}'
echo "$TU_PER_WORKER" | jq empty 2>/dev/null || TU_PER_WORKER='{}'

TOKEN_USAGE=$(jq -n \
    --argjson workspace "${TU_WORKSPACE_RAW:-null}" \
    --argjson supervisor "${TU_SUPERVISOR_RAW:-null}" \
    --argjson merge_queue "${TU_MERGE_QUEUE_RAW:-null}" \
    --argjson worker_total "$TU_WORKER_TOTAL" \
    --argjson total "$TU_TOTAL" \
    --argjson workers_with_data "$TU_WORKER_COUNT" \
    --argjson workers_no_data "$TU_WORKER_NA" \
    --argjson worker_min "${TU_WORKER_MIN:-null}" \
    --argjson worker_max "${TU_WORKER_MAX:-null}" \
    --argjson per_worker "$TU_PER_WORKER" \
    '{
        workspace_tokens: $workspace,
        supervisor_tokens: $supervisor,
        merge_queue_tokens: $merge_queue,
        worker_tokens_total: $worker_total,
        total_tokens: $total,
        workers_with_data: $workers_with_data,
        workers_no_data: $workers_no_data,
        worker_token_min: $worker_min,
        worker_token_max: $worker_max,
        per_worker: $per_worker
    }')

# --- Validate ALL --argjson variables before final report assembly ---
# Any variable passed to jq --argjson MUST be valid JSON. Empty strings,
# "N/A", or malformed output from earlier steps will crash the assembly.

TOKEN_USAGE=$(ensure_json "$TOKEN_USAGE")
PRS_DETAIL=$(ensure_json "$PRS_DETAIL" '[]')
WORKER_AUTONOMY=$(ensure_json "$WORKER_AUTONOMY")
VERIFICATION_METRICS=$(ensure_json "$VERIFICATION_METRICS")
WAVE0=$(ensure_json "$WAVE0")
WAVE1=$(ensure_json "$WAVE1")
WAVE2=$(ensure_json "$WAVE2")
WAVE3=$(ensure_json "$WAVE3")
WAVE4=$(ensure_json "$WAVE4")
ISSUES_TOTAL=$(ensure_number "$ISSUES_TOTAL")
ISSUES_CLOSED=$(ensure_number "$ISSUES_CLOSED")
PRS_TOTAL=$(ensure_number "$PRS_TOTAL")
PRS_MERGED=$(ensure_number "$PRS_MERGED")
PRS_CI_PASSED=$(ensure_number "$PRS_CI_PASSED")
PRS_CI_FAILED=$(ensure_number "$PRS_CI_FAILED")
TOTAL_ACTIVE_SECONDS=$(ensure_number "$TOTAL_ACTIVE_SECONDS")

# --- Build final report ---

COLLECTED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Write large JSON variables to temp files to avoid bash expansion corruption.
# --slurpfile reads from files safely regardless of string size or special chars.
_TMP_PRS=$(mktemp)
_TMP_TOKEN=$(mktemp)
_TMP_AUTONOMY=$(mktemp)
_TMP_VERIFICATION=$(mktemp)
_TMP_WAVE0=$(mktemp)
_TMP_WAVE1=$(mktemp)
_TMP_WAVE2=$(mktemp)
_TMP_WAVE3=$(mktemp)
_TMP_WAVE4=$(mktemp)
printf '%s\n' "$PRS_DETAIL" > "$_TMP_PRS"
printf '%s\n' "$TOKEN_USAGE" > "$_TMP_TOKEN"
printf '%s\n' "$WORKER_AUTONOMY" > "$_TMP_AUTONOMY"
printf '%s\n' "$VERIFICATION_METRICS" > "$_TMP_VERIFICATION"
printf '%s\n' "$WAVE0" > "$_TMP_WAVE0"
printf '%s\n' "$WAVE1" > "$_TMP_WAVE1"
printf '%s\n' "$WAVE2" > "$_TMP_WAVE2"
printf '%s\n' "$WAVE3" > "$_TMP_WAVE3"
printf '%s\n' "$WAVE4" > "$_TMP_WAVE4"
_cleanup_slurp() { rm -f "$_TMP_PRS" "$_TMP_TOKEN" "$_TMP_AUTONOMY" "$_TMP_VERIFICATION" "$_TMP_ROUTING" "$_TMP_WAVE0" "$_TMP_WAVE1" "$_TMP_WAVE2" "$_TMP_WAVE3" "$_TMP_WAVE4"; }
trap '_cleanup_slurp' EXIT

# Validate temp files and repair any invalid JSON before assembly
for _var_name in PRS_DETAIL TOKEN_USAGE WORKER_AUTONOMY VERIFICATION_METRICS WAVE0 WAVE1 WAVE2 WAVE3 WAVE4; do
    _var_file=""
    case "$_var_name" in
        PRS_DETAIL) _var_file="$_TMP_PRS" ;;
        TOKEN_USAGE) _var_file="$_TMP_TOKEN" ;;
        WORKER_AUTONOMY) _var_file="$_TMP_AUTONOMY" ;;
        VERIFICATION_METRICS) _var_file="$_TMP_VERIFICATION" ;;
        WAVE0) _var_file="$_TMP_WAVE0" ;;
        WAVE1) _var_file="$_TMP_WAVE1" ;;
        WAVE2) _var_file="$_TMP_WAVE2" ;;
        WAVE3) _var_file="$_TMP_WAVE3" ;;
        WAVE4) _var_file="$_TMP_WAVE4" ;;
    esac
    if ! jq empty "$_var_file" 2>/dev/null; then
        echo "Warning: ${_var_name} contains invalid JSON -- repairing with fallback"
        echo "  (first 120 chars: $(head -c 120 "$_var_file"))"
        if [[ "$_var_name" == "PRS_DETAIL" ]]; then
            echo '[]' > "$_var_file"
        else
            echo '{}' > "$_var_file"
        fi
    fi
done

ROUTING_CONFIG_FILE="$(dirname "$OUTPUT")/routing-config.json"
ORCHESTRATOR_MODEL=""
if [[ -f "$ROUTING_CONFIG_FILE" ]]; then
    ORCHESTRATOR_MODEL=$(jq -r '.orchestrator_model // empty' "$ROUTING_CONFIG_FILE" 2>/dev/null || true)
fi

if ! REPORT=$(jq -n \
    --arg model "$MODEL" \
    --arg orchestrator_model "$ORCHESTRATOR_MODEL" \
    --arg repo "$REPO_FULL" \
    --arg collected_at "$COLLECTED_AT" \
    --argjson issues_total "$ISSUES_TOTAL" \
    --argjson issues_closed "$ISSUES_CLOSED" \
    --argjson prs_created "$PRS_TOTAL" \
    --argjson prs_merged "$PRS_MERGED" \
    --argjson prs_ci_passed "$PRS_CI_PASSED" \
    --argjson prs_ci_failed "$PRS_CI_FAILED" \
    --argjson total_active_seconds "$TOTAL_ACTIVE_SECONDS" \
    --slurpfile worker_autonomy "$_TMP_AUTONOMY" \
    --slurpfile routing "$_TMP_ROUTING" \
    --slurpfile verification "$_TMP_VERIFICATION" \
    --slurpfile wave0 "$_TMP_WAVE0" \
    --slurpfile wave1 "$_TMP_WAVE1" \
    --slurpfile wave2 "$_TMP_WAVE2" \
    --slurpfile wave3 "$_TMP_WAVE3" \
    --slurpfile wave4 "$_TMP_WAVE4" \
    --slurpfile prs "$_TMP_PRS" \
    --slurpfile token_usage "$_TMP_TOKEN" \
    '{
        model: $model,
        repo: $repo,
        collected_at: $collected_at,
        summary: {
            issues_total: $issues_total,
            issues_closed: $issues_closed,
            prs_created: $prs_created,
            prs_merged: $prs_merged,
            prs_ci_passed: $prs_ci_passed,
            prs_ci_failed: $prs_ci_failed,
            total_agent_active_seconds: $total_active_seconds
        },
        worker_autonomy: $worker_autonomy[0],
        routing: $routing[0],
        verification: $verification[0],
        token_usage: $token_usage[0],
        waves: {
            "0": $wave0[0],
            "1": $wave1[0],
            "2": $wave2[0],
            "3": $wave3[0],
            "4": $wave4[0]
        },
        prs: $prs[0]
    } + (if $orchestrator_model != "" then {orchestrator_model: $orchestrator_model} else {} end)'); then
    echo "Warning: Full report assembly failed, writing partial report..."
    REPORT=$(jq -n \
        --arg model "$MODEL" \
        --arg repo "$REPO_FULL" \
        --arg collected_at "$COLLECTED_AT" \
        --argjson issues_total "$ISSUES_TOTAL" \
        --argjson issues_closed "$ISSUES_CLOSED" \
        --argjson prs_created "$PRS_TOTAL" \
        --argjson prs_merged "$PRS_MERGED" \
        '{
            model: $model,
            repo: $repo,
            collected_at: $collected_at,
            partial: true,
            error: "Full report assembly failed -- some --argjson variables contained invalid JSON",
            summary: {
                issues_total: $issues_total,
                issues_closed: $issues_closed,
                prs_created: $prs_created,
                prs_merged: $prs_merged
            }
        }')
fi

# --- Write output ---

mkdir -p "$(dirname "$OUTPUT")"
echo "$REPORT" > "$OUTPUT"

echo ""
echo "==> Results saved to ${OUTPUT}"
echo ""
format_duration() {
    local seconds=$1
    if [[ "$seconds" == "0" || "$seconds" == "null" ]]; then
        echo "n/a"
        return
    fi
    local hours=$((seconds / 3600))
    local minutes=$(( (seconds % 3600) / 60 ))
    if [[ $hours -gt 0 ]]; then
        echo "${hours}h ${minutes}m"
    else
        echo "${minutes}m"
    fi
}

TOTAL_ACTIVE_FMT=$(format_duration "$TOTAL_ACTIVE_SECONDS")

echo "Summary:"
echo "    Model:              ${MODEL}"
echo "    Issues:             ${ISSUES_CLOSED}/${ISSUES_TOTAL} closed"
echo "    PRs merged:         ${PRS_MERGED}/${PRS_TOTAL}"
echo "    CI passed:          ${PRS_CI_PASSED}"
echo "    CI failed:          ${PRS_CI_FAILED}"
echo "    Agent-active time:  ${TOTAL_ACTIVE_FMT}"
echo "    Self-complete rate: ${WA_SELF_COMPLETED}/${WA_TOTAL} (${WA_SELF_RATE})"
echo ""

for wave_num in 1 2 3 4; do
    local_wave_var="WAVE${wave_num}"
    local_wave_data="${!local_wave_var}"
    local_duration=$(echo "$local_wave_data" | jq -r '.timing.duration_seconds // 0')
    local_duration_fmt=$(format_duration "$local_duration")
    echo "    Wave ${wave_num}: $(echo "$local_wave_data" | jq -r '"\(.closed)/\(.issues) closed, \(.prs_merged) PRs merged"') (${local_duration_fmt})"
done
