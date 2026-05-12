#!/usr/bin/env bash
# Test the planner agent end-to-end with Claude-only models.
#
# Usage:
#   bash scripts/test-planner.sh                        # uses default tiny public repo
#   bash scripts/test-planner.sh <github-url>           # uses your repo
#   bash scripts/test-planner.sh <github-url> <name>    # explicit repo name
#
# Prereqs:
#   - ANTHROPIC_API_KEY in env (or ~/.oat/.env)
#   - git working
#
# What it does:
#   1. Builds the oat binary from this checkout
#   2. Stops the old daemon (if any) and starts a fresh one with the new binary
#   3. Clears state for the test repo
#   4. Runs `oat init <url> --model anthropic:claude-sonnet-4-6`
#   5. Waits for the planner agent to actually spawn
#   6. Opens the TUI

set -euo pipefail

OAT_ROOT="/Users/rajdjagirdar/Downloads/oat-public-prep/open-agent-teams"
OAT_BIN="$OAT_ROOT/dist/oat"
MODEL="anthropic:claude-sonnet-4-6"

GITHUB_URL="${1:-https://github.com/octocat/Hello-World}"
REPO_NAME="${2:-$(basename "$GITHUB_URL" .git)}"

cd "$OAT_ROOT"

echo "=== 1. Branch + recent commits ==="
git --no-pager log --oneline -6
echo

echo "=== 2. vet + build + tui/state/prompts tests ==="
go vet ./... && echo "vet: ok"
go build -o "$OAT_BIN" ./cmd/oat && echo "build: ok"
# Also install to ~/go/bin so any 'oat' command launched off PATH (e.g.
# from another shell) uses the same binary. Earlier we hit a nasty bug
# where the daemon was being spawned via the stale ~/go/bin/oat from a
# prior `go install`, which didn't know about AgentTypePlanner and kept
# cleaning up the planner agent as transient on every restart.
go install ./cmd/oat && echo "install: ok"
go test ./internal/tui/... ./internal/state/... ./internal/prompts/... 2>&1 | tail -5
echo

echo "=== 3. ANTHROPIC_API_KEY check ==="
if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  if grep -q "ANTHROPIC_API_KEY" "$HOME/.oat/.env" 2>/dev/null; then
    echo "ok — found in ~/.oat/.env"
  else
    echo "ERROR: ANTHROPIC_API_KEY is missing"
    echo "  export ANTHROPIC_API_KEY=sk-ant-..."
    echo "  or: echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.oat/.env"
    exit 1
  fi
else
  echo "ok — set in env"
fi
echo

echo "=== 4. Stop OLD daemon (likely running an old binary) ==="
"$OAT_BIN" daemon stop 2>&1 | tail -3 || true
# If still wedged, nuke it.
sleep 1
if "$OAT_BIN" daemon status 2>&1 | grep -q "Running: true"; then
  echo "  daemon still up after stop — using nuke"
  "$OAT_BIN" daemon nuke 2>&1 | tail -3 || true
  sleep 1
fi
echo "  daemon stopped"
echo

echo "=== 5. Clear state for $REPO_NAME (force-fresh) ==="
rm -rf "$HOME/.oat/repos/$REPO_NAME"
rm -rf "$HOME/.oat/wts/$REPO_NAME"
echo "  cleared ~/.oat/repos/$REPO_NAME and ~/.oat/wts/$REPO_NAME"
echo

echo "=== 6. Start NEW daemon with the freshly-built binary ==="
"$OAT_BIN" daemon start
# Wait up to 10s for the daemon socket to be ready.
for i in {1..20}; do
  if "$OAT_BIN" daemon status 2>&1 | grep -q "Running: true"; then
    echo "  daemon up (PID $($OAT_BIN daemon status | grep PID | awk '{print $2}'))"
    break
  fi
  sleep 0.5
done
if ! "$OAT_BIN" daemon status 2>&1 | grep -q "Running: true"; then
  echo "ERROR: daemon failed to start within 10s"
  exit 1
fi
echo

echo "=== 7. oat init $GITHUB_URL $REPO_NAME --model $MODEL ==="
# The daemon keeps repo registrations in-memory across restarts via its own
# state file. A prior failed init can leave $REPO_NAME registered even though
# the on-disk repo/wts dirs are gone. Force-remove from daemon state first.
"$OAT_BIN" repo rm "$REPO_NAME" 2>/dev/null || true
"$OAT_BIN" init "$GITHUB_URL" "$REPO_NAME" --model "$MODEL"
echo

echo "=== 8. Wait for planner to spawn (up to 20s) ==="
spawned=0
for i in {1..40}; do
  if "$OAT_BIN" status --repo "$REPO_NAME" 2>&1 | grep -qi "planner"; then
    spawned=1
    echo "  planner registered after ${i}*0.5s"
    break
  fi
  sleep 0.5
done
if [[ $spawned -eq 0 ]]; then
  echo "WARNING: planner did not appear in 'oat status' within 20s"
  echo "         showing full status anyway:"
  "$OAT_BIN" status --repo "$REPO_NAME" 2>&1 | head -20
fi
echo

echo "=== 9. Final status for $REPO_NAME ==="
"$OAT_BIN" status --repo "$REPO_NAME" 2>&1 | head -30
echo

echo "=== 10. About to launch the TUI ==="
cat <<EOF

VERIFY THIS CHECKLIST INSIDE THE TUI:
  [ ] Sidebar: 'planner' is the FIRST row (above workspace/supervisor)
  [ ] Tab → focus list; cursor lands on planner row
  [ ] Enter on planner row → opens the planner view
  [ ] Content renders (NOT "Initializing planner..." — proves 47785a0 fix)
  [ ] Esc → back to workspace; Tab → cursor still on planner row
  [ ] Ctrl+L from anywhere → same planner view opens
  [ ] Type a requirement + Enter → mocked 3-task response renders

To quit: Ctrl+C
To stop the daemon after: $OAT_BIN daemon stop
EOF
echo
read -r -p "Press enter to launch the TUI..." _
exec "$OAT_BIN" ui --repo "$REPO_NAME"
