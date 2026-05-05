# Schema v2 — manual verification checklist

This document is the hand-driven counterpart to the automated E2E tests.
Run through this before marking PR #104 ready for review. Every step has a
"what good looks like" so you can tell when the build is broken.

The automated coverage lives in:
- `internal/daemon/schema_v2_e2e_test.go` — 5 scenarios that simulate the
  real workflow without spinning a backend
- `internal/routing/*_test.go` — unit + integration tests on every helper

The steps below let you verify the pipeline end-to-end on your actual machine,
not just in CI.

---

## Setup (once)

```bash
# Build the binary in this worktree
cd /Users/rajdjagirdar/Downloads/mono-repo/open-agent-teams/.claude/worktrees/routing-outcome-schema-v2
go build -o /tmp/oat-v2 ./cmd/oat

# Backup your current corpus before doing anything
cp ~/.oat/routing-history.jsonl ~/.oat/routing-history.pre-v2-test.jsonl
```

---

## 1. Migration runs cleanly on your existing 207-record corpus

```bash
# Run the explicit migration
/tmp/oat-v2 routing migrate-v1
```

**Expected output:**
```
Migration complete: 207 v1→v2, 0 already v2, 0 unparseable preserved.
Original backed up to: /Users/rajdjagirdar/.oat/routing-history.jsonl.v1.bak.jsonl
```

**Verify:**
```bash
# Backup exists and matches your pre-test save
diff ~/.oat/routing-history.v1.bak.jsonl ~/.oat/routing-history.pre-v2-test.jsonl
# (should print nothing)

# Every record is now schema v2
jq -s '[.[] | .schema_version] | unique' ~/.oat/routing-history.jsonl
# expected: [2]

# Every record has a record_id, oat_version, provider
jq -s 'map(.record_id != "" and .oat_version != "" and .provider != "") | all' ~/.oat/routing-history.jsonl
# expected: true

# v1-migrated sentinel is on every record (since these all came from the v1 corpus)
jq -s '[.[] | .oat_version] | unique' ~/.oat/routing-history.jsonl
# expected: ["v1-migrated"]

# Task features extracted post-hoc
jq -s 'map(.task_features != null) | all' ~/.oat/routing-history.jsonl
# expected: true

# record_ids are stable across runs (idempotency)
jq -r '.record_id' ~/.oat/routing-history.jsonl | head -3
/tmp/oat-v2 routing migrate-v1  # run again
jq -r '.record_id' ~/.oat/routing-history.jsonl | head -3
# both prints should match exactly
```

---

## 2. `oat routing report` produces sensible numbers

```bash
/tmp/oat-v2 routing report
```

**Expected:** a table with one row per model. For your corpus:
- `anthropic:claude-haiku-4-5` should have n_scored=148
- `openai:gpt-5.4-mini` should have n_scored=36
- `anthropic:claude-sonnet-4-6` should have n_scored=13
- Sonnet's `succ%` should be ≥ 80%; haiku's somewhere in the middle; mini's lowest
- `wilson95` < `succ%` always (Wilson is a lower bound)
- `wall_p50` should be in the tens of seconds; `wall_p95` should be 4-10× higher

**Sanity check:** the per-model breakdown should match what you remember from
your benchmark runs. If sonnet shows 13 records but you recall running it
many more times, something dropped data.

---

## 3. Privacy modes honored on disk

Strict mode redacts free text:
```bash
# Set up an isolated test home
export OAT_HOME_TEST=/tmp/oat-strict-test
mkdir -p $OAT_HOME_TEST

# Write a fake task that contains a "secret"
cat > $OAT_HOME_TEST/seed.go <<'EOF'
package main
import (
    "github.com/Root-IO-Labs/open-agent-teams/internal/routing"
)
func main() {
    l := routing.NewOutcomeLogger("/tmp/oat-strict-test/history.jsonl", nil)
    l.Log(routing.OutcomeRecord{
        Repo: "x", Worker: "y", Model: "z",
        TaskText: "TOTALLY_SECRET_BUSINESS_LOGIC",
        Summary:  "did the secret thing",
    })
}
EOF

# Run with strict mode
OAT_LOG_PRIVACY=strict go run $OAT_HOME_TEST/seed.go

# Verify the secret is not on disk
grep TOTALLY_SECRET /tmp/oat-strict-test/history.jsonl && echo "FAIL: leaked" || echo "OK: redacted"
```

**Expected:** `OK: redacted`

Local mode keeps text:
```bash
OAT_LOG_PRIVACY=local go run $OAT_HOME_TEST/seed.go
# Read the second line we just wrote
tail -1 /tmp/oat-strict-test/history.jsonl | jq -r '.task_text'
# expected: TOTALLY_SECRET_BUSINESS_LOGIC
```

---

## 4. Kill switch silences logging entirely

```bash
# Spin up the daemon with the kill switch
OAT_OUTCOME_LOG=off /tmp/oat-v2 daemon start --foreground &
DAEMON_PID=$!
sleep 2

# Do something that would normally log (run any benchmark)
# ...

# Stop the daemon
kill $DAEMON_PID

# Confirm no new records were written
wc -l ~/.oat/routing-history.jsonl  # before
# (run benchmark)
wc -l ~/.oat/routing-history.jsonl  # should be the same
```

**Expected:** line count unchanged. Daemon ran normally; logging path was
short-circuited at `NewOutcomeLogger` returning nil.

---

## 5. `oat routing share --dry-run` sanitizes per privacy mode

Without opt-in:
```bash
unset OAT_LOG_PRIVACY
/tmp/oat-v2 routing share --dry-run
```
**Expected:** friendly opt-in prompt, no JSON output.

Features-only:
```bash
OAT_LOG_PRIVACY=share-features /tmp/oat-v2 routing share --dry-run > /tmp/share-features.json 2>/dev/null
jq '.records[0] | {task_text, summary, repo}' /tmp/share-features.json
# expected: all three are absent or "" (stripped)

jq '.records[0] | {model, success_score, success_score_basis}' /tmp/share-features.json
# expected: all three present
```

Share-all:
```bash
OAT_LOG_PRIVACY=share-all /tmp/oat-v2 routing share --dry-run > /tmp/share-all.json 2>/dev/null
jq '.records[0] | {task_text, summary, repo}' /tmp/share-all.json
# expected: at least task_text and repo populated (where the source record had them)
```

---

## 6. PR-state backfill ticker actually runs

The backfiller runs every 5 minutes inside the daemon goroutine. To verify
it's wired up:

```bash
# Start the daemon
/tmp/oat-v2 daemon start --foreground &
DAEMON_PID=$!
sleep 1

# Check the daemon log for the backfiller startup line
grep -i backfill ~/.oat/daemon.log | head -5
# expected: an INFO line about starting the backfill goroutine

# Wait through one tick (5 min) — or trigger immediately by sending SIGUSR1
# Alternatively, inspect the sidecar file location
ls -la ~/.oat/routing-history.backfill.jsonl 2>/dev/null
# expected: file appears once any record with pr_number gets observed,
# OR no file if no eligible record exists yet (fresh corpus)

kill $DAEMON_PID
```

**Note:** to test end-to-end with real PRs, you'd run a benchmark that
creates a worker with `pr_number != 0`, then wait 1-7 days for the lag
buckets to fire. For day-zero verification, the unit tests in
`backfill_test.go` (which mock `gh`) cover the actual logic.

---

## 7. Cardinal rule: routing decisions independent of logger

This is automated but worth a one-shot check:

```bash
cd /Users/rajdjagirdar/Downloads/mono-repo/open-agent-teams/.claude/worktrees/routing-outcome-schema-v2
go test -count=1 -v -run "TestRouteForTask_Determ|TestRouteForTask_NoLogger|TestRouteForTask_NoSymbol" ./internal/routing/
```

**Expected:** all three pass with no skips. If `TestRouteForTask_NoSymbolReferences`
fails after a future change, someone introduced a logger dependency in the
router hot path — block the PR until reverted.

---

## 8. Restore your real corpus

When you're done testing:

```bash
# If you ran migrations on your real corpus and want to roll back:
cp ~/.oat/routing-history.pre-v2-test.jsonl ~/.oat/routing-history.jsonl

# Or keep the v2 version (recommended) and remove the test backup:
rm ~/.oat/routing-history.pre-v2-test.jsonl

# Clean up test artifacts
rm -rf /tmp/oat-strict-test /tmp/share-features.json /tmp/share-all.json /tmp/oat-v2
```

---

## What this checklist does NOT cover

- **Real worker spawn → PR creation → backfill cycle.** Requires a live
  benchmark run + GitHub PR + multi-day wait for lag buckets. Tested via
  the unit-test mocks in `backfill_test.go`; functional verification only
  comes from running a real benchmark.
- **Concurrent workers writing simultaneously.** Covered by
  `TestOutcomeLogger_ConcurrentStress` which throws 5000 records at the
  logger from 100 goroutines and asserts no torn lines.
- **Sidecar protocol completion metadata.** Not yet implemented; see
  PR #104 description for the deferred-list rationale.
