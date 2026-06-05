#!/usr/bin/env bash
# End-to-end Langfuse telemetry test runner.
#
# This script automates the manual UI-verification steps from
# docs/specs/langfuse-telemetry.md so a maintainer can re-run the full
# stack any time: real OAT daemon, real GitHub test repo, real LLM agent,
# real Langfuse cloud project. Use this when the unit/integration tests
# pass but you want to confirm the trace tree actually appears in the UI.
#
# Prereqs: `oat`, `gh`, `jq`, `curl`. Telemetry must already be enabled
# (`oat telemetry status` shows "enabled (langfuse)"). LLM provider key
# must be in the environment (ANTHROPIC_API_KEY by default).
#
# Usage:
#   scripts/test-telemetry-e2e.sh                 # show status + usage
#   scripts/test-telemetry-e2e.sh doctor          # check prereqs
#   scripts/test-telemetry-e2e.sh setup           # create test repo + oat init
#   scripts/test-telemetry-e2e.sh run             # spawn worker, capture trace
#   scripts/test-telemetry-e2e.sh verify          # confirm trace landed via API
#   scripts/test-telemetry-e2e.sh full            # setup + run + verify
#   scripts/test-telemetry-e2e.sh cleanup         # tear down repo + worker

set -euo pipefail

STATE_FILE="$HOME/.oat/state.json"
DAEMON_LOG="$HOME/.oat/daemon.log"
SCRIPT_STATE="$HOME/.oat/.telemetry-test-state"   # tracks current test repo across invocations

# ─── helpers ─────────────────────────────────────────────────────────

green()  { printf '\033[32m%s\033[0m\n' "$*"; }
red()    { printf '\033[31m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }

die() { red "ERROR: $*"; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"
}

# Read a value out of ~/.oat/state.json. Usage: jq_state '.telemetry.host'
jq_state() {
  [ -f "$STATE_FILE" ] || die "no $STATE_FILE — has oat ever run?"
  jq -r "$1 // empty" "$STATE_FILE"
}

save_state()  { echo "$1=$2" >> "$SCRIPT_STATE"; }
load_state()  { [ -f "$SCRIPT_STATE" ] && grep "^$1=" "$SCRIPT_STATE" | tail -1 | cut -d= -f2- || echo ""; }
clear_state() { rm -f "$SCRIPT_STATE"; }

# ─── doctor ──────────────────────────────────────────────────────────

cmd_doctor() {
  bold "Prereq check:"
  local ok=1
  for tool in oat gh jq curl; do
    if command -v "$tool" >/dev/null 2>&1; then
      green "  ✓ $tool ($(command -v "$tool"))"
    else
      red "  ✗ $tool (missing)"; ok=0
    fi
  done

  # gh auth
  if gh auth status >/dev/null 2>&1; then
    local gh_user
    gh_user=$(gh api user --jq .login 2>/dev/null || echo "?")
    green "  ✓ gh authed as $gh_user"
  else
    red "  ✗ gh not authed — run: gh auth login"; ok=0
  fi

  # telemetry enabled
  if [ -f "$STATE_FILE" ] && [ "$(jq_state '.telemetry.enabled')" = "true" ]; then
    local host pub
    host=$(jq_state '.telemetry.host')
    pub=$(jq_state '.telemetry.public_key')
    green "  ✓ telemetry enabled (host=$host, public_key=${pub:0:10}...)"
  else
    red "  ✗ telemetry not enabled — run: oat telemetry setup"; ok=0
  fi

  # LLM key (Anthropic is the common case; warn if missing but don't fail)
  if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    green "  ✓ ANTHROPIC_API_KEY set"
  elif [ -n "${OPENAI_API_KEY:-}" ]; then
    green "  ✓ OPENAI_API_KEY set (Anthropic preferred but OpenAI works)"
  else
    yellow "  ⚠ no LLM key in env (ANTHROPIC_API_KEY / OPENAI_API_KEY)"
    yellow "    the worker will fail; export one before running 'run'"
  fi

  # daemon running?
  if oat daemon status >/dev/null 2>&1; then
    green "  ✓ daemon running"
  else
    yellow "  ⚠ daemon not running — 'run' will start it"
  fi

  [ "$ok" = "1" ] && green "All required prereqs satisfied." || die "fix issues above first"
}

# ─── setup ───────────────────────────────────────────────────────────

cmd_setup() {
  cmd_doctor
  echo
  bold "Creating test repo..."

  local gh_user
  gh_user=$(gh api user --jq .login)
  local repo_name="oat-telemetry-test-$(date +%s)"
  local repo_url="https://github.com/${gh_user}/${repo_name}"

  # Create empty repo on GitHub
  gh repo create "$repo_name" --public \
    --description "Ephemeral repo for OAT Langfuse telemetry test (safe to delete)" \
    --confirm 2>/dev/null \
    || gh repo create "$repo_name" --public \
       --description "Ephemeral repo for OAT Langfuse telemetry test (safe to delete)"
  green "  ✓ created $repo_url"

  # Seed the repo with one file so worker has something to edit
  local tmpdir
  tmpdir=$(mktemp -d)
  (
    cd "$tmpdir"
    git init -q -b main
    echo "# Telemetry test repo" > README.md
    echo "Created $(date)" >> README.md
    git -c user.name=oat-agent -c user.email=oat-agent@noreply.github.com add README.md
    git -c user.name=oat-agent -c user.email=oat-agent@noreply.github.com commit -q -m "init"
    git remote add origin "$repo_url"
    git push -q -u origin main
  )
  rm -rf "$tmpdir"
  green "  ✓ seeded with README.md"

  # Run oat init
  bold "Running oat init..."
  oat init "$repo_url" "$repo_name"
  green "  ✓ oat init complete"

  clear_state
  save_state REPO_NAME "$repo_name"
  save_state REPO_URL  "$repo_url"
  save_state GH_USER   "$gh_user"
  echo
  green "Setup complete. Run: $0 run"
}

# ─── run ─────────────────────────────────────────────────────────────

cmd_run() {
  local repo_name
  repo_name=$(load_state REPO_NAME)
  [ -n "$repo_name" ] || die "no test repo — run setup first"

  # Ensure daemon is up
  if ! oat daemon status >/dev/null 2>&1; then
    yellow "daemon not running — starting it"
    oat daemon start
    sleep 2
  fi

  bold "Daemon telemetry banner check:"
  if grep -q "Telemetry: langfuse enabled" "$DAEMON_LOG"; then
    green "  ✓ daemon initialized telemetry"
    grep "Telemetry: langfuse enabled" "$DAEMON_LOG" | tail -1 | sed 's/^/    /'
  else
    yellow "  ⚠ daemon log doesn't show telemetry banner — restarting daemon"
    oat daemon restart
    sleep 2
    grep -q "Telemetry: langfuse enabled" "$DAEMON_LOG" \
      && green "  ✓ banner appears after restart" \
      || die "daemon never logged telemetry banner — telemetry config not loading"
  fi

  # Capture log position so we can grep just the new entries
  local log_start
  log_start=$(wc -l < "$DAEMON_LOG")

  bold "Spawning worker..."
  local task="Add a line saying 'tested telemetry $(date +%H:%M:%S)' to README.md"
  oat worker create "$task" --repo "$repo_name"

  # Wait briefly for the daemon to allocate the trace and emit agent_start.
  # The router decision + env propagation happen synchronously inside
  # startAgentWithConfig, so a 3-second wait is plenty.
  sleep 3

  # Pull the new daemon log entries since spawn.
  local new_log
  new_log=$(tail -n +"$log_start" "$DAEMON_LOG")

  # Find the worker name (e.g., "worker-abc-123") from the most recent
  # "Started and registered agent" line.
  local worker_name
  worker_name=$(echo "$new_log" | grep -oE 'agent [^/]+/[^ ]+' | tail -1 | awk '{print $2}' | cut -d/ -f2)
  if [ -z "$worker_name" ]; then
    die "couldn't find worker name in daemon log; spawn may have failed"
  fi
  green "  ✓ worker '$worker_name' spawned"
  save_state WORKER_NAME "$worker_name"

  # The trace ID isn't directly logged today; we'll fetch it via the Langfuse
  # API by querying recent traces for the agent_id. Note this for the verify
  # step.
  save_state SPAWN_TIME "$(date -u +%s)"

  bold "Waiting 60s for the worker to make LLM calls + tool calls..."
  for i in 60 50 40 30 20 10; do
    sleep 10
    echo "  ${i}s remaining..."
  done

  green "Worker has had 60s to produce telemetry. Run: $0 verify"
}

# ─── verify ──────────────────────────────────────────────────────────

cmd_verify() {
  local worker_name spawn_time host pub sec
  worker_name=$(load_state WORKER_NAME)
  spawn_time=$(load_state SPAWN_TIME)
  [ -n "$worker_name" ] || die "no worker recorded — run 'run' first"

  host=$(jq_state '.telemetry.host')
  pub=$(jq_state '.telemetry.public_key')
  sec=$(jq_state '.telemetry.secret_key')
  [ -n "$host" ] && [ -n "$pub" ] && [ -n "$sec" ] || die "telemetry creds missing in state.json"

  bold "Querying Langfuse API for traces from worker '$worker_name'..."

  # Langfuse public API: /api/public/traces?fromTimestamp=ISO
  local from_iso
  from_iso=$(date -u -r "$spawn_time" +%Y-%m-%dT%H:%M:%S.000Z 2>/dev/null \
          || python3 -c "import datetime; print(datetime.datetime.utcfromtimestamp($spawn_time).strftime('%Y-%m-%dT%H:%M:%S.000Z'))")

  local resp
  resp=$(curl -sS -u "$pub:$sec" "$host/api/public/traces?fromTimestamp=$from_iso&limit=20") \
    || die "Langfuse API call failed"

  local trace_count
  trace_count=$(echo "$resp" | jq '.data | length')
  echo "  found $trace_count trace(s) since worker spawn"

  if [ "$trace_count" = "0" ]; then
    yellow "No traces yet. Possible causes:"
    yellow "  - worker hasn't made any LLM calls (still planning?)"
    yellow "  - background flush hasn't completed (wait 5s, retry)"
    yellow "  - env propagation broken (check $DAEMON_LOG for the worker)"
    exit 1
  fi

  # Find a trace whose name matches "agent:..." OR whose metadata.agent_id matches
  local trace_id trace_name
  trace_id=$(echo "$resp" | jq -r --arg w "$worker_name" \
    '.data[] | select((.metadata.agent_id // "") == $w or .name | startswith("agent:")) | .id' \
    | head -1)
  if [ -z "$trace_id" ]; then
    trace_id=$(echo "$resp" | jq -r '.data[0].id')
    yellow "  ⚠ couldn't match a trace to worker name — using most recent: $trace_id"
  fi
  trace_name=$(echo "$resp" | jq -r --arg id "$trace_id" '.data[] | select(.id==$id) | .name')

  # Fetch the trace with all its observations
  local detail
  detail=$(curl -sS -u "$pub:$sec" "$host/api/public/traces/$trace_id")

  local n_obs
  n_obs=$(echo "$detail" | jq '.observations | length')

  bold "Trace: $trace_name ($trace_id)"
  echo "  observations: $n_obs"
  echo
  echo "  Event breakdown:"
  echo "$detail" | jq -r '.observations | group_by(.type) | .[] | "    \(.[0].type): \(length)"'
  echo
  echo "  Event names:"
  echo "$detail" | jq -r '.observations[] | "    [\(.type)] \(.name)"' | sort -u

  # Pass/fail assertions
  local pass=0 fail=0
  check() {
    if eval "$2"; then green "  ✓ $1"; pass=$((pass+1)); else red "  ✗ $1"; fail=$((fail+1)); fi
  }

  echo
  bold "Assertions:"
  check "trace exists in Langfuse"        "[ -n '$trace_id' ]"
  check "≥1 generation (LLM call)"        "echo '$detail' | jq -e '.observations | map(select(.type==\"GENERATION\")) | length >= 1' >/dev/null"
  check "≥1 event (router/agent_*)"       "echo '$detail' | jq -e '.observations | map(select(.type==\"EVENT\")) | length >= 1' >/dev/null"
  check "router_decision event present"   "echo '$detail' | jq -e '.observations | map(select(.name==\"router_decision\")) | length >= 1' >/dev/null"
  check "agent_start event present"       "echo '$detail' | jq -e '.observations | map(select(.name==\"agent_start\")) | length >= 1' >/dev/null"

  echo
  echo "Open in Langfuse:  $host/project/$(echo "$detail" | jq -r '.projectId')/traces/$trace_id"
  echo
  if [ "$fail" = "0" ]; then
    green "Verify passed ($pass/$pass)."
  else
    red   "Verify failed ($fail failure(s))."
    exit 1
  fi
}

# ─── cleanup ─────────────────────────────────────────────────────────

cmd_cleanup() {
  local repo_name repo_url worker_name
  repo_name=$(load_state REPO_NAME)
  repo_url=$(load_state REPO_URL)
  worker_name=$(load_state WORKER_NAME)

  if [ -z "$repo_name" ]; then
    yellow "no test repo recorded — nothing to clean up"
    exit 0
  fi

  bold "Tearing down $repo_name..."

  if [ -n "$worker_name" ]; then
    oat worker rm "$worker_name" --force 2>/dev/null \
      && green "  ✓ removed worker $worker_name" \
      || yellow "  ⚠ worker rm failed (may already be gone)"
  fi

  oat repo rm "$repo_name" 2>/dev/null \
    && green "  ✓ untracked repo from oat" \
    || yellow "  ⚠ oat repo rm failed (may already be gone)"

  if [ -n "$repo_url" ]; then
    gh repo delete "$repo_url" --yes 2>/dev/null \
      && green "  ✓ deleted GitHub repo $repo_url" \
      || yellow "  ⚠ gh repo delete failed (auth scope?) — delete manually if needed"
  fi

  clear_state
  green "Cleanup done."
}

# ─── status ──────────────────────────────────────────────────────────

cmd_status() {
  bold "Current test state:"
  if [ -f "$SCRIPT_STATE" ]; then
    cat "$SCRIPT_STATE" | sed 's/^/  /'
  else
    yellow "  no active test repo"
  fi
  echo
  bold "Telemetry:"
  oat telemetry status | sed 's/^/  /'
  echo
  bold "Daemon:"
  oat daemon status 2>&1 | head -5 | sed 's/^/  /' || true
}

# ─── full pipeline ───────────────────────────────────────────────────

cmd_full() {
  cmd_setup
  echo
  cmd_run
  echo
  cmd_verify
}

# ─── dispatch ────────────────────────────────────────────────────────

case "${1:-}" in
  doctor)  cmd_doctor ;;
  setup)   cmd_setup ;;
  run)     cmd_run ;;
  verify)  cmd_verify ;;
  cleanup) cmd_cleanup ;;
  status)  cmd_status ;;
  full)    cmd_full ;;
  ""|help|-h|--help)
    cat <<EOF
$(bold "oat telemetry e2e test runner")

Subcommands:
  $(green doctor)   check that oat, gh, jq, curl, telemetry creds, and an LLM key are all present
  $(green setup)    create a fresh test repo on your GitHub + run \`oat init\` on it
  $(green run)      spawn a worker, give it 60s to produce LLM + tool spans
  $(green verify)   hit the Langfuse public API to confirm the trace tree landed
  $(green cleanup)  remove the worker, oat repo, and GitHub repo
  $(green full)     setup → run → verify in one shot

  $(green status)   show what's currently set up

Typical flow: $0 full   # one command does everything
              $0 cleanup # tear it all down when satisfied

State is tracked in ~/.oat/.telemetry-test-state — safe to delete by hand
if anything gets stuck.
EOF
    ;;
  *) die "unknown subcommand: $1 (try: $0 help)" ;;
esac
