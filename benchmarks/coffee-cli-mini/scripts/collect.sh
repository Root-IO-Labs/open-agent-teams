#!/usr/bin/env bash
# Gather metrics into results/<repo>.json:
#   - per-wave issues-closed, PRs-merged
#   - per-agent tokens (input, output, cache_read, cache_creation) from state.json
#   - wall time, cache hit %, total tokens
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./scripts/collect.sh --repo <name> [options]

Collect benchmark metrics into results/<repo>.json.

Required:
  --repo <name>            Benchmark repo name.

Options:
  --output <path>          Results JSON (default: results/<repo>-collect.json).
  --run-start-ts <ts>      Run start timestamp for provenance (ISO8601 or unix
                           seconds). Most honest source for duration; pass from
                           run.sh at benchmark kickoff.
  --run-end-ts <ts>        Run end timestamp. Defaults to max last_token_update.
  --help
EOF
    exit 0
}

REPO_NAME=""
OUTPUT=""
RUN_START_TS=""
RUN_END_TS=""
STATE_FILE_OVERRIDE=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --repo) REPO_NAME="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --run-start-ts) RUN_START_TS="$2"; shift 2 ;;
        --run-end-ts) RUN_END_TS="$2"; shift 2 ;;
        --state-file) STATE_FILE_OVERRIDE="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: unknown flag '$1'"; exit 1 ;;
    esac
done

[[ -n "$REPO_NAME" ]] || { echo "Error: --repo required"; exit 1; }
[[ -n "$OUTPUT" ]] || OUTPUT="${ROOT_DIR}/results/${REPO_NAME}-collect.json"
mkdir -p "$(dirname "$OUTPUT")"

REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"

log() { echo "[$(date '+%H:%M:%S')] $*"; }

# --- GitHub: wave + PR stats ---
log "==> Fetching issues / PRs from ${REPO_FULL}"
ISSUES_JSON=$(gh issue list --repo "${REPO_FULL}" --state all --limit 50 \
    --json number,title,state,labels)
PRS_JSON=$(gh pr list --repo "${REPO_FULL}" --state all --limit 50 \
    --json number,title,state,mergedAt,closingIssuesReferences)

wave_stats() {
    local wave="$1"
    local total closed
    total=$(echo "$ISSUES_JSON" | jq --arg w "$wave" \
        '[.[] | select(.labels[]?.name == $w)] | length')
    closed=$(echo "$ISSUES_JSON" | jq --arg w "$wave" \
        '[.[] | select(.labels[]?.name == $w and .state == "CLOSED")] | length')
    echo "$total $closed"
}

read -r W1_TOTAL W1_CLOSED < <(wave_stats "wave:1")
read -r W2_TOTAL W2_CLOSED < <(wave_stats "wave:2")
PRS_MERGED=$(echo "$PRS_JSON" | jq '[.[] | select(.state == "MERGED")] | length')
PRS_TOTAL=$(echo "$PRS_JSON" | jq 'length')

# --- Provenance inputs (cheap, gather before Python block) ---
GIT_SHA=$(git -C "$ROOT_DIR" rev-parse HEAD 2>/dev/null || echo "unknown")
# git_dirty must catch untracked files too — `git diff --quiet` only checks tracked.
if [[ -n "$(git -C "$ROOT_DIR" status --porcelain 2>/dev/null)" ]]; then
    GIT_DIRTY="true"
else
    GIT_DIRTY="false"
fi
OAT_VERSION=$(command -v oat >/dev/null && oat --version 2>/dev/null | head -1 || echo "unknown")
DAEMON_LOG="${HOME}/.oat/daemon.log"
OUTPUT_DIR="${HOME}/.oat/output/${REPO_NAME}"

# --- state.json + worker logs + daemon.log: tokens, provenance, nudges ---
# Prefer a frozen snapshot if provided (via run.sh) so persistent agents'
# post-benchmark activity doesn't inflate this run's numbers.
if [[ -n "$STATE_FILE_OVERRIDE" && -f "$STATE_FILE_OVERRIDE" ]]; then
    STATE_FILE="$STATE_FILE_OVERRIDE"
    STATE_SOURCE="snapshot"
else
    STATE_FILE="${HOME}/.oat/state.json"
    STATE_SOURCE="live"
fi
if [[ ! -f "$STATE_FILE" ]]; then
    log "WARN: state.json not found at $STATE_FILE — token metrics will be empty"
    TOKEN_JSON='{"schema_version":"2","agents":[],"workers":[],"totals":{"input":0,"output":0,"cache_read":0,"cache_creation":0,"hit_pct":0},"provenance":{},"pricing":{}}'
else
    TOKEN_JSON=$(REPO_NAME="${REPO_NAME}" STATE_FILE="${STATE_FILE}" \
        STATE_SOURCE="${STATE_SOURCE}" \
        OUTPUT_DIR="${OUTPUT_DIR}" DAEMON_LOG="${DAEMON_LOG}" \
        GIT_SHA="${GIT_SHA}" GIT_DIRTY="${GIT_DIRTY}" OAT_VERSION="${OAT_VERSION}" \
        RUN_START_TS="${RUN_START_TS}" RUN_END_TS="${RUN_END_TS}" \
        python3 <<'PYEOF'
import json, os, re, glob
from datetime import datetime, timezone

# Local timezone of this machine — daemon.log timestamps are local with no tz.
LOCAL_TZ = datetime.now().astimezone().tzinfo

repo_name = os.environ["REPO_NAME"]
state = json.load(open(os.environ["STATE_FILE"]))
repo = state.get("repos", {}).get(repo_name, {})

EPOCH_ZERO = "0001-01-01T00:00:00Z"

# Count ASSISTANT turns and TOOL calls from an agent log file. These are the
# strongest signal for "chattiness" — an agent with 45 ASSISTANT turns for 8
# incoming USER messages is running 5+ LLM rounds per message, which is where
# the supervisor spends most of its budget. Tracking this lets us verify that
# daemon-side changes (pre-computed state in nudges) actually reduce turn count.
def count_turns_and_tools(log_path):
    if not os.path.exists(log_path):
        return 0, 0, 0
    asst = tool = usr = 0
    role_re = re.compile(r"^\[\d{2}:\d{2}:\d{2}\] (ASSISTANT|TOOL|USER):")
    try:
        with open(log_path, "r", errors="replace") as f:
            for line in f:
                m = role_re.match(line)
                if not m:
                    continue
                role = m.group(1)
                if role == "ASSISTANT":
                    asst += 1
                elif role == "TOOL":
                    tool += 1
                elif role == "USER":
                    usr += 1
    except OSError:
        pass
    return asst, tool, usr

def parse_ts(s):
    if not s or s.startswith("0001-"):
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None

def last_oat_tokens(log_path):
    """Return dict from last [OAT_TOKENS] JSON line in log, or None."""
    if not os.path.exists(log_path):
        return None
    last = None
    with open(log_path, "r", errors="replace") as f:
        for line in f:
            idx = line.find("[OAT_TOKENS]")
            if idx < 0:
                continue
            try:
                last = json.loads(line[idx + len("[OAT_TOKENS]"):].strip())
            except json.JSONDecodeError:
                pass
    return last

# --- Agents (from state.json), EXCLUDING workers ---
# Workers may or may not appear in state.json depending on daemon state
# (running vs. cleaned-up). To avoid double-counting with the log-file
# derived workers[] list below, we skip type=="worker" here and treat
# log files as the single source of truth for worker tokens.
agents = []
models_seen = set()
start_candidates = []
end_candidates = []
WORKER_TYPES = {"worker", "verify-worker"}

for name, a in repo.get("agents", {}).items():
    if a.get("type") in WORKER_TYPES:
        continue  # handled below via worker log files
    inp = a.get("input_tokens") or 0
    out = a.get("output_tokens") or 0
    cr  = a.get("cache_read_tokens") or 0
    cc  = a.get("cache_creation_tokens") or 0
    hit = round(cr / inp * 100, 1) if inp > 0 else 0.0
    last_update = a.get("last_token_update", EPOCH_ZERO)
    invoked = inp > 0 or (last_update and not last_update.startswith("0001-"))
    model = a.get("model", "")
    if model:
        models_seen.add(model)
    # Parse chattiness metrics from the agent's log file.
    agent_log = os.path.join(os.environ["OUTPUT_DIR"], f"{name}.log")
    asst_turns, tool_calls, user_msgs = count_turns_and_tools(agent_log)
    agents.append({
        "name": name,
        "type": a.get("type", ""),
        "model": model,
        "invoked": bool(invoked),
        "input_tokens": inp,
        "output_tokens": out,
        "cache_read_tokens": cr,
        "cache_creation_tokens": cc,
        "cache_hit_pct": hit,
        "assistant_turns": asst_turns,
        "tool_calls":      tool_calls,
        "user_messages":   user_msgs,
    })
    # last_token_update is the *most recent* emission per agent; it overwrites
    # on every call, so min(last_update) is "when the fastest-finishing agent
    # last emitted", not run start. Only use it as end bound. Start bound is
    # derived separately from worker log file mtimes below, or from the
    # --run-start-ts flag (most honest; see BASELINE_TEMPLATE.md).
    ts = parse_ts(last_update)
    if ts:
        end_candidates.append(ts)

# --- Workers (from log files; daemon does not aggregate to state.json) ---
workers = []
worker_dir = os.path.join(os.environ["OUTPUT_DIR"], "workers")
# Track worker-log timestamps to derive run window when no explicit flag given.
# ctime (inode change) is close to file creation on macOS/Linux; mtime is last write.
worker_log_ctimes = []
worker_log_mtimes = []
for log_path in sorted(glob.glob(os.path.join(worker_dir, "*.log"))):
    try:
        st = os.stat(log_path)
        worker_log_ctimes.append(datetime.fromtimestamp(st.st_ctime, tz=LOCAL_TZ))
        worker_log_mtimes.append(datetime.fromtimestamp(st.st_mtime, tz=LOCAL_TZ))
    except OSError:
        pass
for log_path in sorted(glob.glob(os.path.join(worker_dir, "*.log"))):
    fname = os.path.basename(log_path)
    if fname.startswith("verify-"):
        wtype = "verify-worker"
        wname = fname[len("verify-"):-len(".log")]
    else:
        wtype = "worker"
        wname = fname[:-len(".log")]
    # Scan for [OAT_MODEL] marker (first occurrence wins)
    wmodel = ""
    try:
        with open(log_path, "r", errors="replace") as f:
            for line in f:
                mi = line.find("[OAT_MODEL]")
                if mi >= 0:
                    wmodel = line[mi + len("[OAT_MODEL]"):].strip()
                    break
    except OSError:
        pass
    if wmodel:
        models_seen.add(wmodel)
    last = last_oat_tokens(log_path)
    w_asst, w_tool, w_user = count_turns_and_tools(log_path)
    if last is None:
        workers.append({
            "name": wname, "type": wtype, "model": wmodel, "invoked": False,
            "input_tokens": 0, "output_tokens": 0,
            "cache_read_tokens": 0, "cache_creation_tokens": 0,
            "cache_hit_pct": 0.0, "source": "log",
            "assistant_turns": w_asst, "tool_calls": w_tool, "user_messages": w_user,
        })
        continue
    inp = last.get("cumulative_input", 0)
    out = last.get("cumulative_output", 0)
    cr  = last.get("cache_read", 0)
    cc  = last.get("cache_creation", 0)
    hit = round(cr / inp * 100, 1) if inp > 0 else 0.0
    workers.append({
        "name": wname, "type": wtype, "model": wmodel, "invoked": True,
        "input_tokens": inp, "output_tokens": out,
        "cache_read_tokens": cr, "cache_creation_tokens": cc,
        "cache_hit_pct": hit, "source": "log",
        "assistant_turns": w_asst, "tool_calls": w_tool, "user_messages": w_user,
    })

# --- Totals (agents + workers) ---
tot_in = tot_out = tot_cr = tot_cc = 0
for row in agents + workers:
    tot_in += row["input_tokens"]; tot_out += row["output_tokens"]
    tot_cr += row["cache_read_tokens"]; tot_cc += row["cache_creation_tokens"]
totals = {
    "input": tot_in, "output": tot_out,
    "cache_read": tot_cr, "cache_creation": tot_cc,
    "hit_pct": round(tot_cr / tot_in * 100, 1) if tot_in > 0 else 0.0,
    "io_ratio": round(tot_in / tot_out, 1) if tot_out > 0 else 0.0,
}

# --- Provenance: start/end, nudge count scoped to this run window ---
# start_ts precedence:
#   1. --run-start-ts flag (most honest — recorded at run kickoff)
#   2. min(worker log ctime) — workers are ephemeral, their first log write
#      approximates run start within ~seconds
#   3. None (with start_ts_source = "unavailable")
def parse_flag_ts(s):
    if not s:
        return None
    s = s.strip()
    if s.isdigit():
        return datetime.fromtimestamp(int(s), tz=timezone.utc)
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None

flag_start = parse_flag_ts(os.environ.get("RUN_START_TS", ""))
flag_end   = parse_flag_ts(os.environ.get("RUN_END_TS", ""))

if flag_start is not None:
    start_dt = flag_start
    start_ts_source = "flag"
elif worker_log_ctimes:
    start_dt = min(worker_log_ctimes)
    start_ts_source = "worker_log_ctime"
else:
    start_dt = None
    start_ts_source = "unavailable"

if flag_end is not None:
    end_dt = flag_end
    end_ts_source = "flag"
elif end_candidates:
    end_dt = max(end_candidates)
    end_ts_source = "max_last_token_update"
elif worker_log_mtimes:
    end_dt = max(worker_log_mtimes)
    end_ts_source = "max_worker_log_mtime"
else:
    end_dt = None
    end_ts_source = "unavailable"

start_ts = start_dt.isoformat() if start_dt else None
end_ts   = end_dt.isoformat()   if end_dt   else None
duration_seconds = int((end_dt - start_dt).total_seconds()) if start_dt and end_dt else None

nudge_count = 0
daemon_log = os.environ.get("DAEMON_LOG", "")
if daemon_log and os.path.exists(daemon_log) and start_dt:
    # Daemon log line prefix: "2026/04/18 16:05:49 [LEVEL] ..." — LOCAL time, no tz.
    # Must attach LOCAL_TZ (not start_dt.tzinfo, which is UTC from state.json).
    pat = re.compile(r"^(\d{4})/(\d{2})/(\d{2}) (\d{2}):(\d{2}):(\d{2}) .*nudge.*(?:in repo|to agent .* in repo) " + re.escape(repo_name))
    with open(daemon_log, "r", errors="replace") as f:
        for line in f:
            m = pat.match(line)
            if not m: continue
            try:
                line_dt = datetime(*[int(m.group(i)) for i in range(1, 7)],
                                   tzinfo=LOCAL_TZ)
            except ValueError:
                continue
            if line_dt < start_dt:
                continue
            if end_dt and line_dt > end_dt:
                continue
            nudge_count += 1

provenance = {
    "git_sha": os.environ.get("GIT_SHA", "unknown"),
    "git_dirty": os.environ.get("GIT_DIRTY") == "true",
    "oat_version": os.environ.get("OAT_VERSION", "unknown"),
    "start_ts": start_ts,
    "start_ts_source": start_ts_source,
    "end_ts": end_ts,
    "end_ts_source": end_ts_source,
    "duration_seconds": duration_seconds,
    "models_seen": sorted(models_seen),
    "nudges_sent": nudge_count,
    "nudges_sent_note": "counted from daemon.log within [start_ts, end_ts]; lower bound if log rotated",
    "state_source": os.environ.get("STATE_SOURCE", "live"),
    "state_source_note": "snapshot = frozen at run end, live = current (may include post-run activity)",
}

# --- Pricing (list per 1M tokens). Update when providers change prices. ---
# Key format matches [OAT_MODEL] values: "<provider>:<model-id>".
# cache_creation is null where the provider doesn't expose it (cost is
# effectively billed as regular input with no creation premium).
pricing = {
    "sources": {
        "anthropic": "https://docs.anthropic.com/en/docs/about-claude/pricing",
        "openai":    "https://openai.com/api/pricing/",
        "google":    "https://ai.google.dev/gemini-api/docs/pricing",
    },
    "snapshot_date": "2026-04-19",
    "models": {
        "anthropic:claude-sonnet-4-6":    {"input": 3.00,  "cache_read": 0.30,  "cache_creation": 3.75,  "output": 15.00},
        "anthropic:claude-haiku-4-5":     {"input": 0.80,  "cache_read": 0.08,  "cache_creation": 1.00,  "output":  4.00},
        "anthropic:claude-opus-4-7":      {"input": 15.00, "cache_read": 1.50,  "cache_creation": 18.75, "output": 75.00},
        "openai:gpt-4o-mini":             {"input": 0.15,  "cache_read": 0.075, "cache_creation": None,  "output":  0.60},
        "openai:o4-mini":                 {"input": 1.10,  "cache_read": 0.275, "cache_creation": None,  "output":  4.40},
        "google:gemini-2.5-flash":        {"input": 0.30,  "cache_read": 0.075, "cache_creation": None,  "output":  2.50},
        "ollama:qwen2.5:3b":              {"input": 0.00,  "cache_read": 0.00,  "cache_creation": 0.00,  "output":  0.00},
        "ollama:gemma3:1b":               {"input": 0.00,  "cache_read": 0.00,  "cache_creation": 0.00,  "output":  0.00},
    },
    "note": "Add missing models as they appear in .models_seen. cache_creation=null means provider doesn't separately bill cache writes.",
}

# --- Cost accounting: observed vs cold-start (cache-warming honesty) ---
#
# Observed cost: what Anthropic actually billed us — includes the discount
#   from cache_read tokens the workspace/supervisor/merge-queue agents
#   warmed up before workers started.
# Cold-start cost: what a first-time user who *didn't* benefit from prior
#   cache warming would pay. Treats cache_read tokens as-if they were
#   cache_creation (full-price writes). This is an upper bound; the real
#   first-time user pays somewhere between observed and cold, depending
#   on how quickly their agents fill cache.
# Report both. Headline the cold number when claiming improvement, so we
#   never ship a "win" that's really just warmer cache.
def cost_row(row, prices):
    if not prices:
        return {"observed": None, "cold_start": None}
    input_tokens = row.get("input_tokens", 0) or 0
    cache_read   = row.get("cache_read_tokens", 0) or 0
    cache_creat  = row.get("cache_creation_tokens", 0) or 0
    output       = row.get("output_tokens", 0) or 0
    # input_tokens in OAT_TOKENS schema is TOTAL input-side; subtract cache
    # buckets to get the non-cached "regular" portion.
    regular = max(0, input_tokens - cache_read - cache_creat)
    p_input = prices.get("input") or 0
    p_read  = prices.get("cache_read") or 0
    p_creat = prices.get("cache_creation") if prices.get("cache_creation") is not None else p_input
    p_out   = prices.get("output") or 0
    observed = (regular * p_input + cache_read * p_read + cache_creat * p_creat + output * p_out) / 1_000_000
    # Cold: cache_read is treated as cache_creation (first-time write, not reuse).
    cold = (regular * p_input + cache_read * p_creat + cache_creat * p_creat + output * p_out) / 1_000_000
    return {"observed": round(observed, 4), "cold_start": round(cold, 4)}

def price_for(row_model):
    # Exact match; fall back to first Anthropic model if unknown (and flag).
    m = pricing["models"]
    return m.get(row_model)

# Attach per-row cost and aggregate
totals_cost = {"observed": 0.0, "cold_start": 0.0, "unpriced_rows": []}
for row in agents + workers:
    model = row.get("model", "") or (list(models_seen)[0] if models_seen else "")
    prices = price_for(model)
    if not prices:
        row["cost"] = {"observed": None, "cold_start": None, "reason": f"no pricing for model '{model}'"}
        totals_cost["unpriced_rows"].append(row.get("name"))
        continue
    c = cost_row(row, prices)
    row["cost"] = c
    totals_cost["observed"] += c["observed"]
    totals_cost["cold_start"] += c["cold_start"]
totals_cost["observed"] = round(totals_cost["observed"], 4)
totals_cost["cold_start"] = round(totals_cost["cold_start"], 4)
totals_cost["warming_discount_pct"] = (
    round((totals_cost["cold_start"] - totals_cost["observed"]) / totals_cost["cold_start"] * 100, 1)
    if totals_cost["cold_start"] > 0 else 0.0
)
totals["cost"] = totals_cost

print(json.dumps({
    "schema_version": "2",
    "agents": agents,
    "workers": workers,
    "totals": totals,
    "provenance": provenance,
    "pricing": pricing,
}))
PYEOF
)
fi

# --- Assemble report ---
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

jq -n \
    --arg repo "$REPO_NAME" \
    --arg ts "$TIMESTAMP" \
    --argjson w1t "$W1_TOTAL" --argjson w1c "$W1_CLOSED" \
    --argjson w2t "$W2_TOTAL" --argjson w2c "$W2_CLOSED" \
    --argjson prs_merged "$PRS_MERGED" \
    --argjson prs_total "$PRS_TOTAL" \
    --argjson tokens "$TOKEN_JSON" \
    '{
        repo: $repo,
        timestamp: $ts,
        schema_version: $tokens.schema_version,
        waves: {
            "1": {total: $w1t, closed: $w1c},
            "2": {total: $w2t, closed: $w2c}
        },
        prs: {total: $prs_total, merged: $prs_merged},
        provenance: $tokens.provenance,
        pricing: $tokens.pricing,
        tokens: {
            agents: $tokens.agents,
            workers: $tokens.workers,
            totals: $tokens.totals
        }
    }' > "$OUTPUT"

echo ""
log "==> Collected to $OUTPUT"
echo ""
echo "--- Summary ---"
echo "  Wave 1:  ${W1_CLOSED}/${W1_TOTAL} issues closed"
echo "  Wave 2:  ${W2_CLOSED}/${W2_TOTAL} issues closed"
echo "  PRs:     ${PRS_MERGED}/${PRS_TOTAL} merged"
echo "$TOKEN_JSON" | jq -r '
  "  Tokens:  input=\(.totals.input) output=\(.totals.output) cache_read=\(.totals.cache_read) hit=\(.totals.hit_pct)% io_ratio=\(.totals.io_ratio):1",
  "  Cost:    observed=$\(.totals.cost.observed)  cold_start=$\(.totals.cost.cold_start)  warming_discount=\(.totals.cost.warming_discount_pct)%",
  "  Agents invoked: \([.agents[] | select(.invoked)] | length)/\(.agents | length)   Workers: \(.workers | length)",
  "  Provenance: git=\(.provenance.git_sha[:7])  dirty=\(.provenance.git_dirty)  oat=\(.provenance.oat_version)  nudges=\(.provenance.nudges_sent)  dur=\(.provenance.duration_seconds)s"
'
echo ""
