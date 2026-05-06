#!/usr/bin/env bash
# bench.sh — Streamlined OAT benchmark runner.
#
# Usage:
#   bash benchmarks/bench.sh --model anthropic:claude-sonnet-4-6
#   bash benchmarks/bench.sh --model anthropic:claude-sonnet-4-6 --name my-fix-1
#   bash benchmarks/bench.sh --model anthropic:claude-sonnet-4-6 --compare-to results/BASELINE_*.json
#
# What it does:
#   1. Kills stale oat ui zombies that block oat repo init
#   2. Ensures daemon is running (starts/restarts if needed)
#   3. Prunes sessions.db if > 400 MB
#   4. Runs coffee-cli-mini benchmark
#   5. Compares to baseline (or --compare-to file) and prints token delta
#   6. Exits 0 only if acceptance = 100%

set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COFFEE_DIR="${BENCH_DIR}/coffee-cli-mini"
SCRIPTS_DIR="${COFFEE_DIR}/scripts"
RESULTS_DIR="${COFFEE_DIR}/results"
AGENT_RUNTIME_CLI="${BENCH_DIR}/../agent-runtime/libs/cli"

# Use the agent-runtime venv if available so prune can import oat_cli
if [[ -x "${AGENT_RUNTIME_CLI}/.venv/bin/python3" ]]; then
    PYTHON="${AGENT_RUNTIME_CLI}/.venv/bin/python3"
else
    PYTHON="python3"
fi

# ─── Args ─────────────────────────────────────────────────────────────────────
MODEL=""
NAME_SUFFIX="$(date +%s)"
COMPARE_TO=""
SKIP_PRUNE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --model)       MODEL="$2";       shift 2 ;;
        --name)        NAME_SUFFIX="$2"; shift 2 ;;
        --compare-to)  COMPARE_TO="$2";  shift 2 ;;
        --skip-prune)  SKIP_PRUNE=true;  shift ;;
        --help)
            sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# //'
            exit 0 ;;
        *) echo "Unknown flag: $1  (use --help)"; exit 1 ;;
    esac
done

[[ -n "$MODEL" ]] || { echo "Error: --model required.  bench.sh --model anthropic:claude-sonnet-4-6"; exit 1; }

log() { echo "[$(date '+%H:%M:%S')] $*"; }
hr()  { echo "────────────────────────────────────────────────────────────"; }

# ─── 1. Kill zombie oat ui processes ─────────────────────────────────────────
kill_stale_ui() {
    local pids
    pids=$(pgrep -f "oat ui" 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
        log "Killing stale oat ui processes: $(echo "$pids" | tr '\n' ' ')"
        echo "$pids" | xargs kill -9 2>/dev/null || true
        sleep 1
    fi
}

# ─── 2. Ensure daemon is alive (auto-nuke if wedged) ─────────────────────────
# Runs `oat daemon status` with a 5s wall-clock cap. If it hangs that long,
# the daemon is wedged (socket unresponsive, or kernel UE state) — we nuke
# it and start fresh. This is the single most important reliability lever:
# without it, the benchmark inherits whatever broken state the last run left.
status_with_timeout() {
    local out
    local tmp
    tmp=$(mktemp)
    ( oat daemon status >"$tmp" 2>&1 ) &
    local pid=$!
    local waited=0
    while (( waited < 5 )); do
        sleep 1
        if ! kill -0 $pid 2>/dev/null; then
            out=$(cat "$tmp")
            rm -f "$tmp"
            echo "$out"
            return 0
        fi
        (( waited++ ))
    done
    # Timeout — kill the hung status call and signal wedge
    kill -9 $pid 2>/dev/null || true
    rm -f "$tmp"
    return 2
}

ensure_daemon() {
    log "Checking daemon health (5s timeout)..."
    local out
    if out=$(status_with_timeout); then
        if echo "$out" | grep -qi "running"; then
            log "  Daemon OK"
            return 0
        fi
        log "  Daemon not running — starting"
    else
        log "  Daemon status hung >5s — wedged. Nuking."
        oat daemon nuke 2>&1 | sed 's/^/    /' || true
    fi

    oat daemon start 2>/dev/null || true

    local i=0
    while (( i < 10 )); do
        sleep 2
        if out=$(status_with_timeout) && echo "$out" | grep -qi "running"; then
            log "  Daemon started OK"
            return 0
        fi
        (( i++ ))
    done

    log "ERROR: daemon failed to start after 20s"
    log "       tail -30 ~/.oat/daemon.log for details:"
    tail -30 ~/.oat/daemon.log 2>/dev/null || true
    return 1
}

# ─── 3. Prune sessions.db if bloated ─────────────────────────────────────────
prune_sessions() {
    local db="${HOME}/.oat/sessions.db"
    [[ -f "$db" ]] || return 0
    $SKIP_PRUNE && return 0

    local size_mb
    size_mb=$(du -m "$db" | cut -f1)
    if (( size_mb < 400 )); then
        log "  sessions.db is ${size_mb}MB — no pruning needed"
        return 0
    fi

    log "  sessions.db is ${size_mb}MB — pruning old threads (>30 days)"
    "$PYTHON" -c "
import sys, asyncio
sys.path.insert(0, '${AGENT_RUNTIME_CLI}')
from oat_cli.sessions import prune_old_threads
asyncio.run(prune_old_threads())
" 2>&1 | sed 's/^/    /' || log "  prune failed — continuing anyway"
}

# ─── Main ─────────────────────────────────────────────────────────────────────
hr
log "OAT Benchmark  model=${MODEL}  name=${NAME_SUFFIX}"
hr

kill_stale_ui
ensure_daemon
prune_sessions

log "==> Running coffee-cli-mini benchmark"
bash "${SCRIPTS_DIR}/run.sh" \
    --model "$MODEL" \
    --name "$NAME_SUFFIX" \
    --total-timeout 45

# ─── Find result files ────────────────────────────────────────────────────────
REPO_NAME="oat-coffee-mini-${NAME_SUFFIX}"

COLLECT=$(ls -t "${RESULTS_DIR}/${REPO_NAME}"*collect*.json 2>/dev/null | head -1 || true)
if [[ -z "$COLLECT" ]]; then
    COLLECT=$(ls -t "${RESULTS_DIR}/"*collect*.json 2>/dev/null | head -1 || true)
fi

ACCEPT=$(ls -t "${RESULTS_DIR}/${REPO_NAME}"*acceptance*.json 2>/dev/null | head -1 || true)
if [[ -z "$ACCEPT" ]]; then
    ACCEPT=$(ls -t "${RESULTS_DIR}/"*acceptance*.json 2>/dev/null | head -1 || true)
fi

# ─── Token comparison ─────────────────────────────────────────────────────────
hr
if [[ -n "$COLLECT" ]]; then
    if [[ -z "$COMPARE_TO" ]]; then
        COMPARE_TO=$(ls "${RESULTS_DIR}/BASELINE_"*.json 2>/dev/null | head -1 || true)
    fi

    if [[ -n "$COMPARE_TO" ]]; then
        log "Token delta vs $(basename "${COMPARE_TO}")"
        python3 "${SCRIPTS_DIR}/compare.py" \
            "$COMPARE_TO" "$COLLECT" \
            --label-a "baseline" --label-b "${NAME_SUFFIX}" 2>/dev/null || \
            log "  (compare.py failed — check files manually)"
    else
        log "No baseline to compare against — run will be recorded but no delta shown"
        log "  Hint: copy ${COLLECT} to ${RESULTS_DIR}/BASELINE_<name>.json to set a baseline"
    fi
else
    log "WARN: no collect JSON found"
fi

# ─── Acceptance verdict ───────────────────────────────────────────────────────
hr
if [[ -n "$ACCEPT" ]]; then
    SCORE=$("$PYTHON" -c "import json; d=json.load(open('${ACCEPT}')); print(d.get('score_pct', 0))" 2>/dev/null || echo 0)
    if (( SCORE == 100 )); then
        log "PASS  acceptance ${SCORE}%  —  run ${NAME_SUFFIX} is valid"
        hr
        exit 0
    else
        log "FAIL  acceptance ${SCORE}%  (need 100%)"
        "$PYTHON" -c "
import json
d = json.load(open('${ACCEPT}'))
for f in d.get('failures', []):
    print('  FAIL:', f)
" 2>/dev/null || true
        hr
        exit 1
    fi
else
    log "WARN: no acceptance JSON found — benchmark may not have completed"
    hr
    exit 1
fi
