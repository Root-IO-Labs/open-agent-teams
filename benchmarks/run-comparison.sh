#!/usr/bin/env bash
set -uo pipefail
# NOTE: intentionally NOT set -e — we handle errors ourselves so one
# failure doesn't kill the entire overnight run.

# run-comparison.sh — Run robotic-barista benchmarks on Sonnet and Haiku in
# parallel, capturing every log and interaction for post-mortem analysis.
#
# Usage:
#   GH_TOKEN_CLASSIC=$GH_TOKEN_CLASSIC ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY \
#     ./benchmarks/run-comparison.sh [--wave-timeout 45] [--timeout 360]
#
# (ANTHROPIC_API_KEY is only required because both legs run Anthropic models
# by default. If you point this script at non-Anthropic models, set the
# corresponding provider key instead — run.sh / summarize.sh /
# judge-blackbox.sh resolve provider keys per-run via llm_call.py.)
#
# Results land in: benchmarks/results/comparison-<timestamp>/
#   sonnet/        — full benchmark results + all logs
#   haiku/         — full benchmark results + all logs
#   comparison.md  — side-by-side summary (generated at the end)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
COMPARISON_DIR="$SCRIPT_DIR/results/comparison-$TIMESTAMP"

# Configurable timeouts (generous defaults for overnight)
WAVE_TIMEOUT=45
GRAND_TIMEOUT=360
CONVERGENCE_TIMEOUT=90

# When unset, the inner run.sh defaults each to the orchestrator model
# being compared (so an OpenAI vs Anthropic comparison won't accidentally
# bill the judge/summary calls to a third provider).
JUDGE_MODEL=""
SUMMARY_MODEL=""

# Parse optional overrides
while [[ $# -gt 0 ]]; do
    case $1 in
        --wave-timeout) WAVE_TIMEOUT="$2"; shift 2 ;;
        --timeout) GRAND_TIMEOUT="$2"; shift 2 ;;
        --convergence-timeout) CONVERGENCE_TIMEOUT="$2"; shift 2 ;;
        --judge-model) JUDGE_MODEL="$2"; shift 2 ;;
        --summary-model) SUMMARY_MODEL="$2"; shift 2 ;;
        --help)
            cat <<'EOF'
Usage: ./benchmarks/run-comparison.sh [--wave-timeout MIN] [--timeout MIN]
                                       [--convergence-timeout MIN]
                                       [--judge-model PROVIDER:MODEL]
                                       [--summary-model PROVIDER:MODEL]

Runs robotic-barista benchmarks on Sonnet 4.6 and Haiku 4.5 in parallel.
Results saved to benchmarks/results/comparison-<timestamp>/

Required env vars:
  GH_TOKEN_CLASSIC    Classic PAT with repo scope

Optional env vars:
  <PROVIDER>_API_KEY  API key for whichever provider each leg's model uses
                      (ANTHROPIC_API_KEY for the default Sonnet+Haiku legs;
                      OPENAI_API_KEY / GOOGLE_API_KEY / etc. if you swap
                      models below). run.sh, summarize.sh, and
                      judge-blackbox.sh surface a clear per-run error if
                      the relevant key is missing.
  OAT_BENCH_LLM_MODEL Optional override for the judge / summary models
                      used by the per-run scripts (lower precedence than
                      --judge-model / --summary-model below).

Optional flags:
  --judge-model       Override the LLM gate judge for both legs. Forwarded
                      to each inner run.sh invocation. When unset, each leg
                      defaults to its own orchestrator model (Sonnet for the
                      Sonnet leg, Haiku for the Haiku leg) so cross-provider
                      comparisons don't incur surprise charges to a third
                      provider.
  --summary-model     Override the LLM summarizer for both legs. Same
                      semantics as --judge-model.

Defaults: --wave-timeout 45  --timeout 360  --convergence-timeout 90
EOF
            exit 0
            ;;
        *) echo "Unknown flag: $1"; exit 1 ;;
    esac
done

# ── Preflight checks ────────────────────────────────────────────────
echo "=== Preflight checks ==="
echo ""

fail=false
for cmd in gh jq oat uv; do
    if command -v "$cmd" &>/dev/null; then
        echo "  OK  $cmd ($(command -v "$cmd"))"
    else
        echo "  FAIL $cmd not found"
        fail=true
    fi
done

echo ""

if [[ -z "${GH_TOKEN_CLASSIC:-}" ]]; then
    echo "  FAIL GH_TOKEN_CLASSIC must be set (classic PAT with repo scope)"
    fail=true
else
    echo "  OK  GH_TOKEN_CLASSIC is set"
fi

# Provider API keys are checked per-run by llm_call.py (it knows which
# provider each leg's model resolves to and emits an actionable
# "missing FOO_API_KEY" error). We deliberately don't pin Anthropic at
# startup so cross-provider comparison runs (e.g. Sonnet vs gpt-5.2) work
# without requiring keys for both providers up front.

# Verify gh auth actually works
echo ""
if gh auth status &>/dev/null; then
    GITHUB_OWNER="$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')"
    echo "  OK  gh authenticated as: $GITHUB_OWNER"
else
    echo "  FAIL gh auth status failed — run 'gh auth login' first"
    fail=true
fi

echo ""

if [[ "$fail" == true ]]; then
    echo "Aborting due to preflight failures."
    exit 1
fi

# Export for child processes — run.sh expects GH_TOKEN_CLASSIC
export GH_TOKEN_CLASSIC
export ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}"
export GITHUB_OWNER

# ── Daemon health ───────────────────────────────────────────────────
echo "=== Daemon check ==="

if oat daemon status &>/dev/null; then
    echo "  Daemon is running"
else
    echo "  Starting OAT daemon..."
    oat daemon start
    sleep 3
    if ! oat daemon status &>/dev/null; then
        echo "  FAIL: daemon did not start"
        exit 1
    fi
    echo "  Daemon started"
fi

# Snapshot daemon log before we start (so we can diff later)
DAEMON_LOG_START_LINE=0
if [[ -f "$HOME/.oat/daemon.log" ]]; then
    DAEMON_LOG_START_LINE=$(wc -l < "$HOME/.oat/daemon.log" | tr -d ' ')
fi

echo ""

# ── Setup ────────────────────────────────────────────────────────────
SONNET_NAME="sonnet46-$TIMESTAMP"
HAIKU_NAME="haiku45-$TIMESTAMP"

SONNET_DIR="$COMPARISON_DIR/sonnet"
HAIKU_DIR="$COMPARISON_DIR/haiku"
mkdir -p "$SONNET_DIR" "$HAIKU_DIR"

SONNET_REPO="oat-bench-$SONNET_NAME"
HAIKU_REPO="oat-bench-$HAIKU_NAME"

# Save run metadata
cat > "$COMPARISON_DIR/run-metadata.json" <<METAEOF
{
  "timestamp": "$TIMESTAMP",
  "github_owner": "$GITHUB_OWNER",
  "sonnet": {
    "model": "anthropic:claude-sonnet-4-6",
    "repo": "$SONNET_REPO",
    "name": "$SONNET_NAME"
  },
  "haiku": {
    "model": "anthropic:claude-haiku-4-5",
    "repo": "$HAIKU_REPO",
    "name": "$HAIKU_NAME"
  },
  "config": {
    "wave_timeout_min": $WAVE_TIMEOUT,
    "grand_timeout_min": $GRAND_TIMEOUT,
    "convergence_timeout_min": $CONVERGENCE_TIMEOUT,
    "daemon_log_start_line": $DAEMON_LOG_START_LINE
  }
}
METAEOF

echo "=== Comparison run: $TIMESTAMP ==="
echo "  Sonnet repo: $SONNET_REPO"
echo "  Haiku  repo: $HAIKU_REPO"
echo "  Results dir: $COMPARISON_DIR"
echo "  Wave timeout: ${WAVE_TIMEOUT}m | Grand timeout: ${GRAND_TIMEOUT}m | Convergence: ${CONVERGENCE_TIMEOUT}m"
echo ""

# ── Daemon watchdog ──────────────────────────────────────────────────
# Runs in background, checks daemon every 5 min. If dead, restarts it.
# This prevents both benchmarks from silently stalling overnight.

daemon_watchdog() {
    local watchdog_log="$COMPARISON_DIR/watchdog.log"
    while true; do
        sleep 300  # 5 minutes
        if ! oat daemon status &>/dev/null 2>&1; then
            echo "[$(date '+%H:%M:%S')] WATCHDOG: daemon down — restarting" >> "$watchdog_log"
            oat daemon start >> "$watchdog_log" 2>&1 || true
            sleep 5
            if oat daemon status &>/dev/null 2>&1; then
                echo "[$(date '+%H:%M:%S')] WATCHDOG: daemon recovered" >> "$watchdog_log"
            else
                echo "[$(date '+%H:%M:%S')] WATCHDOG: daemon restart FAILED" >> "$watchdog_log"
            fi
        fi
    done
}

daemon_watchdog &
WATCHDOG_PID=$!

# Clean up watchdog on exit
cleanup() {
    kill "$WATCHDOG_PID" 2>/dev/null || true
    wait "$WATCHDOG_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "  Daemon watchdog started (PID $WATCHDOG_PID, checks every 5m)"
echo ""

# ── Run benchmarks in parallel ───────────────────────────────────────

run_benchmark() {
    local model="$1"
    local name="$2"
    local result_dir="$3"
    local repo_name="$4"
    local log_file="$result_dir/full-run.log"
    local short_name="${model##*:}"  # e.g. "claude-sonnet-4-6"

    echo "[$(date '+%H:%M:%S')] Starting $short_name benchmark → $repo_name" | tee -a "$log_file"

    # Build optional model-override args. ${arr[@]+"${arr[@]}"} is the
    # bash-3.2-safe expansion under `set -u`: expands to nothing when the
    # array is empty rather than tripping "unbound variable".
    local extra_args=()
    if [[ -n "$JUDGE_MODEL" ]]; then
        extra_args+=(--judge-model "$JUDGE_MODEL")
    fi
    if [[ -n "$SUMMARY_MODEL" ]]; then
        extra_args+=(--summary-model "$SUMMARY_MODEL")
    fi

    # Run the benchmark, capturing all stdout/stderr.
    # || true ensures we continue even if run.sh exits non-zero.
    "$SCRIPT_DIR/run.sh" \
        --model "$model" \
        --name "$name" \
        --wave-timeout "$WAVE_TIMEOUT" \
        --timeout "$GRAND_TIMEOUT" \
        --convergence-timeout "$CONVERGENCE_TIMEOUT" \
        --nudge-timeout 12 \
        --poll-interval 120 \
        ${extra_args[@]+"${extra_args[@]}"} \
        >> "$log_file" 2>&1 || {
        echo "[$(date '+%H:%M:%S')] $short_name run.sh exited with code $?" >> "$log_file"
    }

    echo "[$(date '+%H:%M:%S')] $short_name benchmark finished" | tee -a "$log_file"

    # ── Collect all logs and artifacts ─────────────────────────────
    echo "[$(date '+%H:%M:%S')] $short_name: collecting artifacts..." >> "$log_file"

    # 1. Copy the timestamped results folder that run.sh created
    local latest_result
    latest_result=$(ls -dt "$SCRIPT_DIR/results/"*"-$name"* 2>/dev/null | head -1)
    if [[ -n "$latest_result" && -d "$latest_result" ]]; then
        cp -r "$latest_result"/* "$result_dir/" 2>/dev/null || true
        echo "  Copied results from $latest_result" >> "$log_file"
    fi

    # 2. Agent output logs (every agent's full output stream)
    local oat_output="$HOME/.oat/output/$repo_name"
    if [[ -d "$oat_output" ]]; then
        mkdir -p "$result_dir/agent-logs"
        # Use find + cp to handle nested dirs cleanly
        find "$oat_output" -name "*.log" -exec cp {} "$result_dir/agent-logs/" \; 2>/dev/null || true
        # Workers subdir
        if [[ -d "$oat_output/workers" ]]; then
            mkdir -p "$result_dir/agent-logs/workers"
            find "$oat_output/workers" -name "*.log" -exec cp {} "$result_dir/agent-logs/workers/" \; 2>/dev/null || true
        fi
        echo "  Copied agent logs" >> "$log_file"
    fi

    # 3. Daemon log — only lines from after we started, filtered to this repo
    if [[ -f "$HOME/.oat/daemon.log" ]]; then
        tail -n +"$((DAEMON_LOG_START_LINE + 1))" "$HOME/.oat/daemon.log" \
            | grep -i "$repo_name" > "$result_dir/daemon-filtered.log" 2>/dev/null || true
        # Full daemon log from this session
        tail -n +"$((DAEMON_LOG_START_LINE + 1))" "$HOME/.oat/daemon.log" \
            > "$result_dir/daemon-session.log" 2>/dev/null || true
        echo "  Captured daemon logs" >> "$log_file"
    fi

    # 4. State snapshot at end of run
    if [[ -f "$HOME/.oat/state.json" ]]; then
        jq --arg repo "$repo_name" '.repos[$repo] // empty' "$HOME/.oat/state.json" \
            > "$result_dir/state-snapshot.json" 2>/dev/null || true
    fi

    # 5. Message history for this repo
    local msg_dir="$HOME/.oat/messages/$repo_name"
    if [[ -d "$msg_dir" ]]; then
        mkdir -p "$result_dir/messages"
        cp -r "$msg_dir"/* "$result_dir/messages/" 2>/dev/null || true
    fi

    # 6. History entries for this repo
    if [[ -f "$HOME/.oat/history.jsonl" ]]; then
        grep "$repo_name" "$HOME/.oat/history.jsonl" > "$result_dir/history.jsonl" 2>/dev/null || true
    fi

    # 7. Git log from the benchmark repo
    local repo_clone=""
    if [[ -d "$HOME/.oat/wts/$repo_name/default" ]]; then
        repo_clone="$HOME/.oat/wts/$repo_name/default"
    elif [[ -d "$HOME/.oat/wts/$repo_name" ]]; then
        repo_clone="$HOME/.oat/wts/$repo_name"
    fi
    if [[ -n "$repo_clone" ]]; then
        git -C "$repo_clone" log --all --oneline --graph > "$result_dir/git-log.txt" 2>/dev/null || true
        git -C "$repo_clone" log --all --stat > "$result_dir/git-log-stat.txt" 2>/dev/null || true
        git -C "$repo_clone" diff --stat "$(git -C "$repo_clone" rev-list --max-parents=0 HEAD 2>/dev/null | head -1)"..HEAD \
            > "$result_dir/git-diff-summary.txt" 2>/dev/null || true
        echo "  Captured git history" >> "$log_file"
    fi

    # 8. PR list with full details
    gh pr list --repo "$GITHUB_OWNER/$repo_name" --state all \
        --json number,title,state,author,createdAt,mergedAt,closedAt,additions,deletions,labels,headRefName \
        > "$result_dir/prs.json" 2>/dev/null || true

    # 9. Issue list with full details
    gh issue list --repo "$GITHUB_OWNER/$repo_name" --state all --limit 100 \
        --json number,title,state,labels,createdAt,closedAt \
        > "$result_dir/issues.json" 2>/dev/null || true

    # 10. PR review comments (captures agent interactions on PRs)
    local pr_numbers
    pr_numbers=$(jq -r '.[].number' "$result_dir/prs.json" 2>/dev/null || true)
    if [[ -n "$pr_numbers" ]]; then
        mkdir -p "$result_dir/pr-comments"
        for pr_num in $pr_numbers; do
            gh api "repos/$GITHUB_OWNER/$repo_name/pulls/$pr_num/comments" \
                > "$result_dir/pr-comments/$pr_num.json" 2>/dev/null || true
        done
        echo "  Captured PR comments" >> "$log_file"
    fi

    echo "[$(date '+%H:%M:%S')] $short_name — all artifacts collected" | tee -a "$log_file"
}

# Launch both in parallel
echo "Launching benchmarks..."
echo ""

run_benchmark "anthropic:claude-sonnet-4-6" "$SONNET_NAME" "$SONNET_DIR" "$SONNET_REPO" &
SONNET_PID=$!

# Stagger start by 30s to avoid gh API race conditions during repo creation
sleep 30

run_benchmark "anthropic:claude-haiku-4-5" "$HAIKU_NAME" "$HAIKU_DIR" "$HAIKU_REPO" &
HAIKU_PID=$!

echo "Both benchmarks running in parallel."
echo "  Sonnet PID: $SONNET_PID"
echo "  Haiku  PID: $HAIKU_PID"
echo ""
echo "Monitor with:"
echo "  oat ui --repo $SONNET_REPO"
echo "  oat ui --repo $HAIKU_REPO"
echo "  tail -f $SONNET_DIR/full-run.log"
echo "  tail -f $HAIKU_DIR/full-run.log"
echo ""
echo "Waiting for both to complete..."
echo ""

# Wait for both — don't fail if one does
SONNET_EXIT=0
HAIKU_EXIT=0
wait $SONNET_PID || SONNET_EXIT=$?
wait $HAIKU_PID  || HAIKU_EXIT=$?

echo ""
echo "=== Both benchmarks complete ==="
echo "  Sonnet exit code: $SONNET_EXIT"
echo "  Haiku  exit code: $HAIKU_EXIT"
echo ""

# ── Capture watchdog log ────────────────────────────────────────────
if [[ -f "$COMPARISON_DIR/watchdog.log" ]]; then
    WATCHDOG_EVENTS=$(wc -l < "$COMPARISON_DIR/watchdog.log" | tr -d ' ')
    echo "  Watchdog events: $WATCHDOG_EVENTS"
else
    echo "  Watchdog: no events (daemon stayed healthy)"
fi
echo ""

# ── Generate comparison summary ──────────────────────────────────────

generate_comparison() {
    local out="$COMPARISON_DIR/comparison.md"

    cat > "$out" <<HEADER
# Benchmark Comparison: Sonnet 4.6 vs Haiku 4.5

**Date:** $(date '+%Y-%m-%d %H:%M')
**Config:** wave_timeout=${WAVE_TIMEOUT}m, grand_timeout=${GRAND_TIMEOUT}m, convergence=${CONVERGENCE_TIMEOUT}m
**Sonnet exit:** $SONNET_EXIT | **Haiku exit:** $HAIKU_EXIT

## Quick Scores

HEADER

    for model_dir in "$SONNET_DIR" "$HAIKU_DIR"; do
        local label
        if [[ "$model_dir" == "$SONNET_DIR" ]]; then label="Sonnet 4.6"; else label="Haiku 4.5"; fi

        echo "### $label" >> "$out"
        echo "" >> "$out"

        # Gate score
        if [[ -f "$model_dir/gate.json" ]]; then
            local gate_score gate_verdict
            gate_score=$(jq -r '.score // .total_score // "n/a"' "$model_dir/gate.json" 2>/dev/null || echo "n/a")
            gate_verdict=$(jq -r '.verdict // "n/a"' "$model_dir/gate.json" 2>/dev/null || echo "n/a")
            echo "- **Gate:** $gate_score/100 ($gate_verdict)" >> "$out"
        else
            echo "- **Gate:** no data" >> "$out"
        fi

        # Acceptance score
        if [[ -f "$model_dir/acceptance.json" ]]; then
            local acc_score acc_passed acc_total
            acc_score=$(jq -r '.score // "n/a"' "$model_dir/acceptance.json" 2>/dev/null || echo "n/a")
            acc_passed=$(jq -r '.summary.passed // "?"' "$model_dir/acceptance.json" 2>/dev/null || echo "?")
            acc_total=$(jq -r '.summary.total // "?"' "$model_dir/acceptance.json" 2>/dev/null || echo "?")
            echo "- **Acceptance:** $acc_score/100 ($acc_passed/$acc_total tests)" >> "$out"
        else
            echo "- **Acceptance:** no data" >> "$out"
        fi

        # Collect metrics
        if [[ -f "$model_dir/collect.json" ]]; then
            local issues_closed prs_merged active_time self_rate
            issues_closed=$(jq -r '.summary.issues_closed // "?"' "$model_dir/collect.json" 2>/dev/null || echo "?")
            prs_merged=$(jq -r '.summary.prs_merged // "?"' "$model_dir/collect.json" 2>/dev/null || echo "?")
            active_time=$(jq -r '.summary.total_agent_active_seconds // 0' "$model_dir/collect.json" 2>/dev/null || echo "0")
            self_rate=$(jq -r '.worker_autonomy.self_completion_rate // "?"' "$model_dir/collect.json" 2>/dev/null || echo "?")
            local active_min=$((active_time / 60))
            echo "- **Issues closed:** $issues_closed" >> "$out"
            echo "- **PRs merged:** $prs_merged" >> "$out"
            echo "- **Active time:** ${active_min}m" >> "$out"
            echo "- **Self-completion rate:** $self_rate" >> "$out"
        fi

        # Convergence
        if [[ -f "$model_dir/convergence.json" ]]; then
            local conv_verdict conv_iters
            conv_verdict=$(jq -r '.verdict // "n/a"' "$model_dir/convergence.json" 2>/dev/null || echo "n/a")
            conv_iters=$(jq -r '.iterations // "?"' "$model_dir/convergence.json" 2>/dev/null || echo "?")
            echo "- **Convergence:** $conv_verdict ($conv_iters iterations)" >> "$out"
        fi

        echo "" >> "$out"
    done

    cat >> "$out" <<'FOOTER'
## Captured Artifacts (per model)

| File | Description |
|------|-------------|
| `full-run.log` | Complete stdout/stderr from run.sh |
| `terminal-output.txt` | run.sh's own terminal capture |
| `agent-logs/*.log` | Every agent's full output stream |
| `agent-logs/workers/*.log` | Worker agent output streams |
| `daemon-filtered.log` | Daemon log entries for this repo only |
| `daemon-session.log` | Full daemon log from this session |
| `state-snapshot.json` | OAT state at end of run |
| `messages/` | Inter-agent message files |
| `history.jsonl` | Task history entries |
| `git-log.txt` | Full git commit graph |
| `git-log-stat.txt` | Commits with file change stats |
| `git-diff-summary.txt` | Total diff from initial commit to HEAD |
| `prs.json` | All PRs with full metadata |
| `pr-comments/*.json` | Review comments on each PR |
| `issues.json` | All issues with metadata |
| `gate.json` | Blackbox gate judgment |
| `collect.json` | Operational metrics |
| `acceptance.json` | Ground-truth test results |
| `convergence.json` | Convergence loop results |
| `summary.md` | LLM-generated analysis |

## How to review

```bash
cd <results-dir>

# Compare acceptance scores
jq '.score' sonnet/acceptance.json haiku/acceptance.json

# Diff the gate judgments
diff <(jq . sonnet/gate.json) <(jq . haiku/gate.json)

# Look at what each model committed
diff sonnet/git-log.txt haiku/git-log.txt

# Read agent interactions
less sonnet/agent-logs/default.log    # workspace agent
less sonnet/agent-logs/supervisor.log

# Check how many PRs each model created vs merged
jq 'length' sonnet/prs.json haiku/prs.json
jq '[.[] | select(.state == "MERGED")] | length' sonnet/prs.json haiku/prs.json

# Check daemon decisions (nudges, force-removes, PR monitor)
less sonnet/daemon-filtered.log

# Full LLM summaries
cat sonnet/summary.md
cat haiku/summary.md

# Check if watchdog had to intervene
cat watchdog.log 2>/dev/null || echo "No watchdog events"
```
FOOTER

    echo ""
    echo "Comparison written to: $out"
}

generate_comparison

echo ""
echo "=========================================="
echo "  Done"
echo "=========================================="
echo ""
echo "Results: $COMPARISON_DIR"
echo ""
echo "Quick review:"
echo "  cat $COMPARISON_DIR/comparison.md"
echo "  ls $COMPARISON_DIR/sonnet/"
echo "  ls $COMPARISON_DIR/haiku/"
echo ""
echo "Cleanup (when ready):"
echo "  ./benchmarks/cleanup.sh --repo $SONNET_REPO --delete-remote"
echo "  ./benchmarks/cleanup.sh --repo $HAIKU_REPO --delete-remote"
