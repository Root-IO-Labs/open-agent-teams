#!/usr/bin/env bash
# scripts/demo-schema-v2.sh — visual end-to-end demo of routing schema v2.
#
# Builds the binary, runs every piece of the new pipeline against an isolated
# $HOME, and prints what's happening at each step. Your real ~/.oat/ is never
# touched — everything runs under /tmp/oat-schema-v2-demo/.
#
# Usage:
#   ./scripts/demo-schema-v2.sh
#
# Re-runs are idempotent — the demo dir is wiped at start.
set -eu

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEMO_HOME="/tmp/oat-schema-v2-demo"
BINARY="$DEMO_HOME/oat"

# ANSI colors for section headers — 1990s tech demo aesthetic
HEAD='\033[1;36m'  # cyan bold
SUB='\033[1;33m'   # yellow bold
DIM='\033[2m'
END='\033[0m'

step() { printf "\n${HEAD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n%s\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${END}\n" "$*"; }
sub()  { printf "\n${SUB}▸ %s${END}\n" "$*"; }
note() { printf "${DIM}  %s${END}\n" "$*"; }

# ─── Setup ───────────────────────────────────────────────────────────────────
step "STEP 0 · build the binary + isolate \$HOME"
sub "Build oat from this worktree (using real GOMODCACHE so the demo dir stays clean)"

# Cleanup any prior demo dir. Go's module cache is created read-only, so a
# previous run's GOMODCACHE under DEMO_HOME would fight rm -rf. We chmod
# first so cleanup always succeeds.
if [ -d "$DEMO_HOME" ]; then
  chmod -R u+w "$DEMO_HOME" 2>/dev/null || true
  rm -rf "$DEMO_HOME"
fi
mkdir -p "$DEMO_HOME/.oat"

# Build using the user's real Go module cache (NOT under DEMO_HOME) so the
# demo dir doesn't accumulate read-only build artifacts that complicate
# re-runs.
( cd "$REPO_ROOT" && go build -o "$BINARY" ./cmd/oat )
note "binary at $BINARY"
note "isolated HOME at $DEMO_HOME"

# NOW switch HOME so every subsequent oat invocation reads/writes
# inside the demo dir. Build is done; we never invoke `go` again under
# this HOME.
export HOME="$DEMO_HOME"

# ─── Step 1 · v1 corpus + migration ──────────────────────────────────────────
step "STEP 1 · seed a synthetic v1 corpus → run migration → inspect enrichment"

sub "Seed 3 v1 records (one happy worker, one failed worker, one verifier)"
cat > "$HOME/.oat/routing-history.jsonl" <<'EOF'
{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"swift-otter","agent_type":"worker","task_text":"Fix the typo 'recieve' in cli.py","model":"anthropic:claude-3-5-sonnet-20241022","routing_source":"router-auto","wall_ms":300000,"tokens_in":12000,"tokens_out":340,"outcome":"completed","summary":"Fixed typo on line 42","pr_number":101}
{"schema_version":1,"ts":"2026-04-16T11:30:00Z","repo":"alpha","worker":"prime-falcon","agent_type":"worker","task_text":"Refactor the auth subsystem across models/, controllers/, middleware/","model":"openai:gpt-5.4-mini","routing_source":"router-auto","wall_ms":1800000,"tokens_in":85000,"tokens_out":2100,"outcome":"removed","failure_reason":"Worker stuck after 50 iterations"}
{"schema_version":1,"ts":"2026-04-17T14:00:00Z","repo":"alpha","worker":"calm-bear","agent_type":"verification","task_text":"Verify worker swift-otter commit abc123","model":"anthropic:claude-haiku-4-5","outcome":"completed","summary":"Verified: approved"}
EOF

sub "BEFORE migration — schema_version + record_id presence"
jq -s 'map({schema_version, has_record_id: (has("record_id") and .record_id != "")})' "$HOME/.oat/routing-history.jsonl"

sub "Run migration"
"$BINARY" routing migrate-v1

sub "AFTER migration — every v2 field landed correctly"
echo "  schema_version + record_id + oat_version sentinel:"
jq -s 'map({schema_version, record_id, oat_version})' "$HOME/.oat/routing-history.jsonl"
echo ""
echo "  provider + model_canonical (point-release flattened):"
jq -s 'map({model, provider, model_canonical})' "$HOME/.oat/routing-history.jsonl"
echo ""
echo "  task_features extracted post-hoc:"
jq -s 'map(.task_features | {char_count, line_count, imperative_verb, file_path_mentions})' "$HOME/.oat/routing-history.jsonl"
echo ""
echo "  Backup created (idempotent — only on first run):"
ls -la "$HOME/.oat/"

sub "Idempotency — re-run migration is a no-op"
"$BINARY" routing migrate-v1

# ─── Step 2 · report on the v2 corpus ────────────────────────────────────────
step "STEP 2 · oat routing report on the migrated corpus"
"$BINARY" routing report

# ─── Step 3 · privacy command ────────────────────────────────────────────────
step "STEP 3 · oat routing privacy"
"$BINARY" routing privacy

# ─── Step 4 · share --dry-run sanitization ───────────────────────────────────
step "STEP 4 · oat routing share --dry-run · sanitization at each privacy mode"

sub "Default (local) — sharing requires opt-in"
"$BINARY" routing share --dry-run
echo ""

sub "OAT_LOG_PRIVACY=share-features — task_text + repo stripped, hashes kept"
OAT_LOG_PRIVACY=share-features "$BINARY" routing share --dry-run 2>/dev/null | \
  jq '.records[0] | {record_id, model, provider, model_canonical,
                     task_text_present: (.task_text != null),
                     repo_present: (.repo != null),
                     prompt_hash_present: (.prompt != null),
                     success_score, success_score_basis}'

sub "OAT_LOG_PRIVACY=share-all — full text included"
OAT_LOG_PRIVACY=share-all "$BINARY" routing share --dry-run 2>/dev/null | \
  jq '.records[0] | {task_text, summary, repo, success_score}'

# ─── Step 5 · kill switch ────────────────────────────────────────────────────
step "STEP 5 · OAT_OUTCOME_LOG=off · kill switch surfaces in the privacy output"
OAT_OUTCOME_LOG=off "$BINARY" routing privacy

# ─── Step 6 · the corpus_join loop closure (recovered worker) ────────────────
step "STEP 6 · sidecar fold flips score from 0% → 100% (the recovered-worker case)"

sub "BEFORE sidecar — gpt-5.4-mini scored 0% (the failed worker)"
"$BINARY" routing report 2>&1 | grep -E "^model|^─|gpt-5.4-mini"

sub "Drop a sidecar entry showing prime-falcon's PR eventually merged"
cat > "$HOME/.oat/routing-history.backfill.jsonl" <<'EOF'
{"schema_version":1,"snapshot_ts":"2026-04-17T12:00:00Z","record_key":{"ts":"2026-04-16T11:30:00Z","worker":"prime-falcon","repo":"alpha"},"snapshot":{"ts":"2026-04-17T12:00:00Z","state":"merged","merged_at":"2026-04-17T11:55:00Z","lag_bucket":"24h","ci_status":"passed"}}
EOF
note "sidecar (separate from main file — append-only contract preserved):"
cat "$HOME/.oat/routing-history.backfill.jsonl"

sub "AFTER sidecar — score flips because LoadCorpusJoined folds the merge in"
"$BINARY" routing report 2>&1 | grep -E "^model|^─|gpt-5.4-mini"

sub "Confirm via share payload — basis is now pr_merged, score=1.0"
OAT_LOG_PRIVACY=share-features "$BINARY" routing share --dry-run 2>/dev/null | \
  jq '.records[] | select(.model == "openai:gpt-5.4-mini") |
      {model_canonical, outcome, success_score, success_score_basis}'

# ─── Step 7 · BASE CASE PROOF · routing decision changes from corpus signal ──
step "STEP 7 · BASE CASE · prove the loop closes by watching V2's pick CHANGE"

note "We use 'oat routing route' to preview what V2 would pick for a task."
note "Same task, same profiles, same pricing — only the corpus changes."
note "If the loop is closed, the pick should respond to corpus changes."

# Need profiles loaded for the route preview. Onboarding requires real
# probing — too slow for a demo. Instead, drop minimal profiles that the
# router can rank.
mkdir -p "$HOME/.oat/model-profiles"
cat > "$HOME/.oat/model-profiles/anthropic__claude-haiku-4-5.yaml" <<'EOF'
model_id: "anthropic:claude-haiku-4-5"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.88
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 1.0
  large_output: 0.9
  effective_context_class: medium
  max_input_tokens: 200000
  reasoning_controls: "thinking_budget_5000"
routing:
  autonomy_tier: full
  overall_score: 96
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
EOF

cat > "$HOME/.oat/model-profiles/google_genai__gemini-2.5-flash.yaml" <<'EOF'
model_id: "google_genai:gemini-2.5-flash"
status: known
provider:
  name: google_genai
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 1.0
  large_output: 0.9
  effective_context_class: large
  max_input_tokens: 1048576
  reasoning_controls: "none"
routing:
  autonomy_tier: standard
  overall_score: 99
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: false
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
EOF

TASK="Implement the new payment flow described in specs/payment.md so the endpoint returns 200 on charge"

sub "BASE A · empty corpus → V2 picks the highest static score (gemini-flash, score=99)"
# Wipe the corpus we built earlier so this section starts clean.
echo -n "" > "$HOME/.oat/routing-history.jsonl"
rm -f "$HOME/.oat/routing-history.backfill.jsonl"
"$BINARY" routing route --task "$TASK" --v2 2>&1 | grep -E "chosen:|reason:|hist_n"

sub "BASE B · inject 6 successful records for haiku at THIS complexity → V2's pick"
# Six records to clear the MinHistoricalSamples=5 threshold. Each must
# bucket as the same complexity as the test task. We use an identical task
# text for the records so ExtractFeatures puts them in the same bucket.
for i in 1 2 3 4 5 6; do
  REC=$(jq -nc --arg ts "2026-04-15T10:0$i:00Z" --arg task "$TASK" \
    '{schema_version: 2, ts: $ts, repo: "demo", worker: ("w-" + ($ENV.i // "x")),
      agent_type: "worker", task_text: $task,
      model: "anthropic:claude-haiku-4-5", model_canonical: "claude-haiku-4-5",
      provider: "anthropic", routing_source: "router-v1-auto",
      outcome: "completed", verify_passed: true,
      pr_state_history: [{ts: $ts, state: "merged", lag_bucket: "24h"}],
      task_features: {char_count: 96}}' \
    | jq -c .)
  echo "$REC" >> "$HOME/.oat/routing-history.jsonl"
done
note "Added 6 PR-merged records for haiku (clears N≥5 threshold)"

"$BINARY" routing route --task "$TASK" --v2 2>&1 | grep -E "chosen:|reason:|hist_n"

sub "BASE C · also inject 6 records for gemini-flash, but ALL FAILED"
for i in 1 2 3 4 5 6; do
  REC=$(jq -nc --arg ts "2026-04-16T10:0$i:00Z" --arg task "$TASK" \
    '{schema_version: 2, ts: $ts, repo: "demo", worker: ("g-" + ($ENV.i // "x")),
      agent_type: "worker", task_text: $task,
      model: "google_genai:gemini-2.5-flash", model_canonical: "gemini-2.5-flash",
      provider: "google_genai", routing_source: "router-v1-auto",
      outcome: "removed", removal_reason: "failed",
      task_features: {char_count: 96}}' \
    | jq -c .)
  echo "$REC" >> "$HOME/.oat/routing-history.jsonl"
done
note "Added 6 failed records for gemini-flash (its hist_factor should drop to 0.5)"

"$BINARY" routing route --task "$TASK" --v2 2>&1 | grep -E "chosen:|reason:|hist_n"

sub "VERIFICATION · the corpus actually drove these picks"
note "If the routing report shows the success rates we expect, the pipeline"
note "is genuinely reading the same data the router consults."
"$BINARY" routing report

sub "BASE-CASE INTERPRETATION"
cat <<'EOF'
  A · Empty corpus → V2 picks gemini-flash (static score 99 > haiku's 96).
      reason shows: hist_factor=1.00, hist_n=0, "no corpus".

  B · Six SUCCESSFUL haiku records added; gemini-flash has zero records.
      V2 STILL picks gemini-flash. Why?
        gemini effective = 99 × 1.00 = 99    (no signal, factor=1)
        haiku  effective = 96 × wilson(6/6) = 96 × 0.69 ≈ 66
      Positive evidence on a challenger isn't enough to switch when the
      incumbent has no negative evidence. This is V2 being CONSERVATIVE —
      it won't churn routing on the first hint of a better option.

  C · Six FAILED gemini-flash records added. NOW the pick flips:
        gemini effective = 99 × max(0.5, wilson(0/6)) = 99 × 0.5 = 49.5
        haiku  effective = 96 × 0.69                              ≈ 66
      The pick goes to haiku — the reason output shows hist_factor=0.69,
      hist_n=6 confirming the corpus signal IS being consumed.

  This is the loop closing visibly. Same task, same profiles, same
  pricing — different routing decision, driven entirely by what the
  corpus has observed. V2 only switches when there's evidence in BOTH
  directions: positive on the challenger AND negative on the incumbent.
EOF

# ─── Done ────────────────────────────────────────────────────────────────────
step "DONE"
note "Demo dir: $HOME — wipe with: rm -rf $DEMO_HOME"
note "Your real ~/.oat/ was never touched."
echo ""
