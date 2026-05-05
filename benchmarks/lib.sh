#!/usr/bin/env bash
# benchmarks/lib.sh -- shared helpers for benchmark scripts.
#
# Sourced by benchmarks/run.sh and benchmarks/collect.sh. Provides defensive
# primitives against degraded GitHub Issues/search index conditions (e.g. the
# Apr 27 2026 ElasticSearch incident, where `gh issue list --label X` silently
# returned an incomplete set for hours while the per-issue endpoints stayed
# correct). Treats benchmarks/issues.json as the static source of truth for
# expected wave composition (waves 0-4); the live `gh issue list` is treated
# as a sanity check with retry-and-fallback when it disagrees.
#
# Tunables (env-overridable):
#   OAT_INDEX_POLL_TIMEOUT   how long to poll before giving up (default 120s)
#   OAT_INDEX_POLL_INTERVAL  per-poll sleep (default 10s)
#   ISSUES_JSON_PATH         override the source-of-truth JSON path
#   BENCHMARKS_DIR           override the benchmarks directory (auto-resolved)
#
# Bash 3.2 + `set -u` safe: no associative arrays, no process substitution,
# no `arr=( $(cmd) )` on potentially-empty output.

# -- Globals -----------------------------------------------------------------

# Resolve our own directory so the file is location-independent regardless of
# where the caller sources it from.
_LIB_SH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCHMARKS_DIR="${BENCHMARKS_DIR:-${_LIB_SH_DIR}}"
ISSUES_JSON_PATH="${ISSUES_JSON_PATH:-${BENCHMARKS_DIR}/issues.json}"
OAT_INDEX_POLL_TIMEOUT="${OAT_INDEX_POLL_TIMEOUT:-120}"
OAT_INDEX_POLL_INTERVAL="${OAT_INDEX_POLL_INTERVAL:-10}"

# -- Internal helpers --------------------------------------------------------

_lib_log_warn() { echo "WARNING: $*" >&2; }
_lib_log_error() { echo "ERROR:   $*" >&2; }
_lib_log_info() { echo "[lib] $*" >&2; }

# List issue numbers for a wave label from GitHub.
# Args: <repo_full> <wave_label> <state: all|open>
# Echoes newline-separated, sorted numbers; empty on failure.
_lib_gh_wave_numbers() {
    local repo_full="$1"
    local wave_label="$2"
    local state="$3"
    gh issue list --repo "$repo_full" --label "$wave_label" --state "$state" \
        --json number --jq '.[].number' 2>/dev/null | sort -n
}

# Probe a single issue's state via the per-issue endpoint, which is unaffected
# by search-index degradation.
# Echoes "open", "closed", or empty (not found / network error).
_lib_gh_issue_state() {
    local repo_full="$1"
    local num="$2"
    gh api "repos/${repo_full}/issues/${num}" --jq '.state' 2>/dev/null
}

# Fetch a single issue as a JSON object using the same schema as
# `gh issue list --json <fields>`. Used by collect.sh to merge indexing-lagged
# issues back into ISSUES_JSON. Echoes a JSON object on success; empty on
# 404 / network error.
_lib_gh_issue_view_json() {
    local repo_full="$1"
    local num="$2"
    local fields="${3:-number,title,state,labels,closedAt}"
    gh issue view "$num" --repo "$repo_full" --json "$fields" 2>/dev/null
}

# Count non-empty numeric lines in a string. Bash-3.2 + `set -u` safe.
# Always echoes a single integer.
_lib_count_lines() {
    local input="$1"
    if [[ -z "$input" ]]; then
        echo 0
        return 0
    fi
    # `grep -c` always prints the count and exits 1 on no match; swallow the
    # exit code with `|| true` so we get a clean "0" instead of "0\n0" under
    # `set -u` callers (see benchmarks/run.sh PRE_COUNT comment).
    local n
    n=$(echo "$input" | grep -c '^[0-9][0-9]*$' || true)
    [[ -z "$n" ]] && n=0
    echo "$n"
}

# -- Public helpers ----------------------------------------------------------

# expected_wave_issues "<wave-label>"
#
# Echoes sorted, newline-separated issue numbers from issues.json for the
# given label. Empty echo when the JSON is missing/unreadable, jq fails, or
# no issues carry the label.
expected_wave_issues() {
    local wave_label="$1"
    [[ -r "$ISSUES_JSON_PATH" ]] || return 0
    jq -r --arg label "$wave_label" \
        '[.[] | select((.labels // []) | index($label)) | .number] | sort | .[]' \
        "$ISSUES_JSON_PATH" 2>/dev/null
}

# expected_wave_count "<wave-label>"
#
# Echoes the integer count of issues with the given label in issues.json.
# Echoes 0 when the JSON is missing/unreadable or no issues match.
expected_wave_count() {
    local wave_label="$1"
    local nums
    nums=$(expected_wave_issues "$wave_label")
    _lib_count_lines "$nums"
}

# discover_wave_issues_with_retry <repo_full> <wave_label>
#
# Returns (on stdout) the trustworthy list of OPEN issue numbers for the wave
# label, sorted, one per line. Polls `gh issue list --label X --state all`
# until the live count meets or exceeds the expected count from issues.json,
# or OAT_INDEX_POLL_TIMEOUT elapses.
#
# On the happy path (live count meets expectation immediately or within the
# timeout), this is a single `gh issue list` call -- no measurable overhead.
#
# On timeout, runs per-issue probes against the per-issue endpoint (which is
# unaffected by ES degradation) for each missing-from-live number to classify:
#   - open   -> indexing-lagged but real and open; merged into the result
#   - closed -> indexing-lagged but already closed; excluded (no worker needed)
#   - 404    -> setup silently failed; excluded with an ERROR log
# A single per-wave WARNING summarizes the indexing-lagged misses.
#
# When expected_count == 0 (unknown label, missing JSON, future use against
# wave:fix-N), this is a graceful no-op: returns the live `--state open` list
# with no warning, no fallback, no probes.
#
# All log lines go to stderr; stdout is just numbers so callers can pipe it
# safely into `while read`.
discover_wave_issues_with_retry() {
    local repo_full="$1"
    local wave_label="$2"

    local expected_count
    expected_count=$(expected_wave_count "$wave_label")

    # No-op fast path: caller has no expectation, return live as-is.
    if [[ "$expected_count" -eq 0 ]]; then
        _lib_gh_wave_numbers "$repo_full" "$wave_label" open
        return 0
    fi

    # First (immediate) poll.
    local live_all_numbers live_all_count
    live_all_numbers=$(_lib_gh_wave_numbers "$repo_full" "$wave_label" all)
    live_all_count=$(_lib_count_lines "$live_all_numbers")

    if [[ "$live_all_count" -ge "$expected_count" ]]; then
        _lib_gh_wave_numbers "$repo_full" "$wave_label" open
        return 0
    fi

    # Degraded path: poll up to OAT_INDEX_POLL_TIMEOUT.
    local elapsed=0
    while [[ "$elapsed" -lt "$OAT_INDEX_POLL_TIMEOUT" ]]; do
        sleep "$OAT_INDEX_POLL_INTERVAL"
        elapsed=$((elapsed + OAT_INDEX_POLL_INTERVAL))
        live_all_numbers=$(_lib_gh_wave_numbers "$repo_full" "$wave_label" all)
        live_all_count=$(_lib_count_lines "$live_all_numbers")
        if [[ "$live_all_count" -ge "$expected_count" ]]; then
            _lib_gh_wave_numbers "$repo_full" "$wave_label" open
            return 0
        fi
    done

    # Timeout. Compute the set of expected-but-not-live numbers, then probe.
    local expected_numbers
    expected_numbers=$(expected_wave_issues "$wave_label")

    local missing_numbers=""
    while IFS= read -r enum; do
        [[ -z "$enum" ]] && continue
        if ! echo "$live_all_numbers" | grep -qx "$enum"; then
            missing_numbers="${missing_numbers}${enum}"$'\n'
        fi
    done <<< "$expected_numbers"

    local missing_open_numbers=""
    local missing_open_disp=""
    local missing_closed_disp=""
    local probe_state mnum

    while IFS= read -r mnum; do
        [[ -z "$mnum" ]] && continue
        probe_state=$(_lib_gh_issue_state "$repo_full" "$mnum")
        case "$probe_state" in
            open)
                missing_open_numbers="${missing_open_numbers}${mnum}"$'\n'
                missing_open_disp="${missing_open_disp}#${mnum} "
                ;;
            closed)
                missing_closed_disp="${missing_closed_disp}#${mnum} "
                ;;
            *)
                _lib_log_error "gh issue #${mnum} missing from repo (per-issue probe returned 404 -- setup may have silently failed) -- excluded from fallback list"
                ;;
        esac
    done <<< "$missing_numbers"

    if [[ -n "$missing_open_disp" || -n "$missing_closed_disp" ]]; then
        local summary="${missing_open_disp}${missing_closed_disp}"
        summary="${summary% }"
        _lib_log_warn "gh issue list lagging for ${wave_label} -- live=${live_all_count} expected=${expected_count} missing=${summary} -- using JSON fallback (likely GitHub indexing degradation, see https://www.githubstatus.com/)"
    fi

    # Result: live --state open  union  indexing-lagged-but-open numbers.
    {
        _lib_gh_wave_numbers "$repo_full" "$wave_label" open
        printf '%s' "$missing_open_numbers"
    } | grep -E '^[0-9]+$' | sort -n -u
}
