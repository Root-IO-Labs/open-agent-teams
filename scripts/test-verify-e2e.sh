#!/usr/bin/env bash
# =============================================================================
# End-to-end verification system test
# Runs against a real oat repo (information-bot) with zero manual input.
# =============================================================================
set -uo pipefail

REPO_NAME="information-bot"
WORKER_NAME=""
WORKTREE=""
PASS=0
FAIL=0
TOTAL=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${YELLOW}--- Test $((TOTAL+1)) ---${NC} $*"; }
pass() { TOTAL=$((TOTAL+1)); PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${NC}: $1"; }
fail() { TOTAL=$((TOTAL+1)); FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${NC}: $1 — $2"; }

# run_oat: capture output and exit code WITHOUT swallowing it
# Usage: run_oat <args...>  → sets OAT_OUT and OAT_EXIT
run_oat() {
    OAT_EXIT=0
    OAT_OUT=$("$OAT" "$@" 2>&1) || OAT_EXIT=$?
}

# =============================================================================
# Setup
# =============================================================================
OAT=""
if command -v oat &>/dev/null; then
    OAT="oat"
elif [[ -f ./oat ]]; then
    OAT="./oat"
else
    echo "Error: oat binary not found. Run 'make build' or 'go install ./cmd/oat' first."
    exit 1
fi
echo "Using: $OAT ($(command -v $OAT 2>/dev/null || echo 'local'))"

if ! $OAT status &>/dev/null; then
    echo "Starting daemon..."
    $OAT start
    sleep 2
fi

if [[ ! -d "$HOME/.oat/wts/$REPO_NAME/default" ]]; then
    echo "Error: $REPO_NAME not initialized."
    exit 1
fi

echo ""
echo "=========================================="
echo "  Verify System E2E — $REPO_NAME"
echo "=========================================="
echo ""

# =============================================================================
# Create test worker
# =============================================================================
log "Creating test worker..."
run_oat worker create "e2e verify test" --repo "$REPO_NAME"

# Extract worker name from state (most reliable)
WORKER_NAME=$(python3 -c "
import json
with open('$HOME/.oat/state.json') as f:
    d = json.load(f)
agents = d.get('repos',{}).get('$REPO_NAME',{}).get('agents',{})
for name, a in sorted(agents.items(), key=lambda x: x[1].get('created_at',''), reverse=True):
    if a.get('type') == 'worker':
        print(name)
        break
" 2>/dev/null)

if [[ -z "$WORKER_NAME" ]]; then
    echo "Error: Could not find test worker"
    exit 1
fi

WORKTREE="$HOME/.oat/wts/$REPO_NAME/$WORKER_NAME"
echo "Worker: $WORKER_NAME"
echo "Worktree: $WORKTREE"
echo ""

cd "$WORKTREE"
git fetch origin main --quiet 2>/dev/null || true

# =============================================================================
# Test 1: Verify runs and produces human-readable output
# =============================================================================
log "Verify produces output"
run_oat worker verify --verbose --skip-tests
if echo "$OAT_OUT" | grep -q "VERIFICATION RESULTS"; then
    pass "verify produces results output"
else
    fail "verify produces results output" "no VERIFICATION RESULTS found"
fi

# =============================================================================
# Test 2: --json produces valid, parseable JSON (no progress lines mixed in)
# =============================================================================
log "--json output"
run_oat worker verify --json --skip-tests
if echo "$OAT_OUT" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    pass "--json produces valid JSON"
else
    fail "--json produces valid JSON" "parse error; first 200 chars: $(echo "$OAT_OUT" | head -c 200)"
fi

# Check required fields
HAS_FIELDS=$(echo "$OAT_OUT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    for k in ('overall_score','overall_passed','task_alignment','file_integrity','syntax_validation','test_execution'):
        assert k in d, f'missing {k}'
    print('ok')
except Exception as e:
    print(f'fail: {e}')
" 2>/dev/null)
if [[ "$HAS_FIELDS" == "ok" ]]; then
    pass "--json has all required fields"
else
    fail "--json has all required fields" "$HAS_FIELDS"
fi

# =============================================================================
# Test 3: Python syntax error detected
# =============================================================================
log "Syntax validation catches Python errors"
cat > "$WORKTREE/_verify_test_bad.py" << 'EOF'
def broken(
    print("missing paren and colon"
EOF
git add _verify_test_bad.py && git commit -m "add broken python" --quiet

run_oat worker verify --json --skip-tests
SYNTAX_CAUGHT=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
si = d.get('syntax_validation',{}).get('issues',[]) or []
fi = d.get('file_integrity',{}).get('issues',[]) or []
all_issues = si + fi
print('yes' if any('_verify_test_bad' in str(i) for i in all_issues) else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$SYNTAX_CAUGHT" == "yes" ]]; then
    pass "syntax validation detects Python error"
else
    fail "syntax validation detects Python error" "got: $SYNTAX_CAUGHT"
fi

# =============================================================================
# Test 4: Duplicate code detected
# =============================================================================
log "File integrity catches duplicates"
cat > "$WORKTREE/_verify_test_dup.py" << 'EOF'
def handler(request):
    process(request)
    return response()

def handler(request):
    process(request)
    return response()

def other():
    pass
EOF
git add _verify_test_dup.py && git commit -m "add duplicated code" --quiet

run_oat worker verify --json --skip-tests
DUP_CAUGHT=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
issues = d.get('file_integrity',{}).get('issues',[]) or []
print('yes' if any('duplicate' in str(i).lower() and '_verify_test_dup' in str(i) for i in issues) else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$DUP_CAUGHT" == "yes" ]]; then
    pass "file integrity detects duplicate blocks"
else
    fail "file integrity detects duplicate blocks" "got: $DUP_CAUGHT"
fi

# =============================================================================
# Test 5: --fix reports activity
# =============================================================================
log "--fix mode"
run_oat worker verify --fix --verbose --skip-tests
if echo "$OAT_OUT" | grep -qiE "fix|removed|repair|auto"; then
    pass "--fix mode reports fixes"
else
    fail "--fix mode reports fixes" "no fix output"
fi

# =============================================================================
# Test 6: Truncation markers detected
# =============================================================================
log "Truncation marker detection"
cat > "$WORKTREE/_verify_test_trunc.py" << 'EOF'
def main():
    setup()
    # ... rest omitted
    pass
EOF
git add _verify_test_trunc.py && git commit -m "add truncated file" --quiet

run_oat worker verify --json --skip-tests
TRUNC_CAUGHT=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
issues = d.get('file_integrity',{}).get('issues',[]) or []
print('yes' if any('truncat' in str(i).lower() or 'omitted' in str(i).lower() for i in issues) else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$TRUNC_CAUGHT" == "yes" ]]; then
    pass "truncation markers detected"
else
    fail "truncation markers detected" "got: $TRUNC_CAUGHT"
fi

# =============================================================================
# Test 7: Clean code passes
# =============================================================================
log "Clean code passes verification"
rm -f "$WORKTREE"/_verify_test_*.py
cat > "$WORKTREE/_verify_test_clean.py" << 'EOF'
def hello(name: str) -> str:
    return f"Hello, {name}!"

if __name__ == "__main__":
    print(hello("world"))
EOF
git add -A && git commit -m "clean code only" --quiet

run_oat worker verify --json --skip-tests
CLEAN_PASSED=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
fi = d.get('file_integrity',{}).get('passed', False)
sv = d.get('syntax_validation',{}).get('passed', False)
print('yes' if fi and sv else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$CLEAN_PASSED" == "yes" ]]; then
    pass "clean code passes integrity + syntax"
else
    fail "clean code passes integrity + syntax" "got: $CLEAN_PASSED"
fi

# =============================================================================
# Test 8: Exit code non-zero on failure
# =============================================================================
log "Non-zero exit on failure"
# Create MULTIPLE bad files to push score well below 70.
# Each broken file hits syntax (-25 * 0.20 = -5) and integrity (-10 * 0.35 = -3.5).
for i in 1 2 3 4 5; do
cat > "$WORKTREE/_verify_test_bad${i}.py" << 'EOF'
def broken(
    x = {
    # terrible syntax
    // not python at all
    <!-- truncated -->
EOF
done
git add _verify_test_bad*.py && git commit -m "many broken files" --quiet

run_oat worker verify --skip-tests
if [[ $OAT_EXIT -ne 0 ]]; then
    pass "exit code non-zero on verification failure (exit=$OAT_EXIT)"
else
    SCORE=$(echo "$OAT_OUT" | grep -oE "Score: [0-9.]+" | head -1 || echo "unknown")
    fail "exit code non-zero on failure" "exit=0, $SCORE"
fi
rm -f "$WORKTREE"/_verify_test_bad*.py

# =============================================================================
# Test 9: Exit code 0 on success
# =============================================================================
log "Zero exit on success"
rm -f "$WORKTREE"/_verify_test_*.py
cat > "$WORKTREE/_verify_test_clean.py" << 'EOF'
def hello():
    return "world"
EOF
git add -A && git commit -m "back to clean" --quiet

run_oat worker verify --skip-tests
if [[ $OAT_EXIT -eq 0 ]]; then
    pass "exit code 0 on success"
else
    fail "exit code 0 on success" "exit=$OAT_EXIT"
fi

# =============================================================================
# Test 10: PR gate blocks unverified commits
# =============================================================================
log "PR gate blocks unverified"
echo "# unverified" >> "$WORKTREE/_verify_test_clean.py"
git add -A && git commit -m "unverified commit" --quiet

run_oat pr create --title "Should Block" --body "test"
if [[ $OAT_EXIT -ne 0 ]] && echo "$OAT_OUT" | grep -qi "verif"; then
    pass "PR gate blocks unverified commits"
else
    fail "PR gate blocks unverified commits" "exit=$OAT_EXIT"
fi

# =============================================================================
# Test 11: Verification log records results
# =============================================================================
log "Verification log records commit SHA"
run_oat worker verify --skip-tests
COMMIT_SHA=$(git rev-parse HEAD 2>/dev/null)

# Find the verification log — it lives at ~/.oat/verification.log
VLOG="$HOME/.oat/verification.log"
LOG_CHECK=$(python3 -c "
import json, glob, os
# Try known path first, then search
paths = ['$VLOG']
paths += glob.glob(os.path.expanduser('~/.oat/**/verification.log'), recursive=True)
for p in paths:
    if not os.path.exists(p):
        continue
    with open(p) as f:
        for line in reversed(f.readlines()):
            line = line.strip()
            if not line: continue
            entry = json.loads(line)
            if entry.get('commit_sha') == '$COMMIT_SHA':
                print('passed' if entry.get('overall_passed') else 'failed')
                exit(0)
print('not_found')
" 2>/dev/null || echo "error")
if [[ "$LOG_CHECK" == "passed" ]]; then
    pass "verification log records passing result for commit"
else
    fail "verification log records passing result" "got: $LOG_CHECK"
fi

# =============================================================================
# Test 12: --skip-tests flag
# =============================================================================
log "--skip-tests flag"
run_oat worker verify --skip-tests --json
SKIP_OK=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
issues = d.get('test_execution',{}).get('issues',[]) or []
print('yes' if any('skip' in str(i).lower() for i in issues) else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$SKIP_OK" == "yes" ]]; then
    pass "--skip-tests skips test execution"
else
    fail "--skip-tests skips test execution" "got: $SKIP_OK"
fi

# =============================================================================
# Test 13: .oat/ directory excluded from checks
# =============================================================================
log ".oat/ excluded from file checks"
run_oat worker verify --json --skip-tests
OAT_DIR_FOUND=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
all_issues = []
for check in ('file_integrity','syntax_validation'):
    all_issues.extend(d.get(check,{}).get('issues',[]) or [])
print('yes' if any('.oat/' in str(i) for i in all_issues) else 'no')
" 2>/dev/null || echo "parse_error")
if [[ "$OAT_DIR_FOUND" == "no" ]]; then
    pass ".oat/ directory excluded from checks"
else
    fail ".oat/ directory excluded" "found .oat/ references in issues"
fi

# =============================================================================
# =============================================================================
#  BENCHMARK READINESS TESTS
#  These simulate the exact flow that happens during a real benchmark run.
# =============================================================================
# =============================================================================

echo ""
echo "=========================================="
echo "  Benchmark Readiness Tests"
echo "=========================================="
echo ""

# =============================================================================
# Test 14: PR gate error does NOT mention --force
# (weak models read the error and immediately bypass if they see --force)
# =============================================================================
log "PR gate error hides --force from agents"
echo "# force-test" >> "$WORKTREE/_verify_test_clean.py"
git add -A && git commit -m "test force hint" --quiet

run_oat pr create --title "Force Test" --body "test"
if echo "$OAT_OUT" | grep -qi "\-\-force"; then
    fail "PR gate hides --force hint" "error output contains --force"
else
    pass "PR gate hides --force from agents"
fi

# =============================================================================
# Test 15: --force bypass still works for humans
# =============================================================================
log "--force bypass works (human escape hatch)"
run_oat pr create --title "Force Test" --body "test" --force
# We expect this to proceed past the gate (may fail at gh pr create, that's fine)
if echo "$OAT_OUT" | grep -qi "Skipping verification gate"; then
    pass "--force bypasses verification gate"
else
    fail "--force bypasses verification gate" "no skip message: $(echo "$OAT_OUT" | head -c 200)"
fi

# =============================================================================
# Test 16: Full benchmark worker cycle (verify → fail → fix → verify → pass → gate)
# This is exactly what happens during a wave in run.sh
# =============================================================================
log "Full benchmark worker cycle: bad code → verify fail → fix → verify pass → gate pass"

# Step 1: Worker writes bad code (simulates weak model output)
rm -f "$WORKTREE"/_verify_test_*.py
cat > "$WORKTREE/_verify_test_feature.py" << 'PYEOF'
def broken(
    x = {
    # terrible syntax
    <!-- truncated -->
PYEOF
cat > "$WORKTREE/_verify_test_feature2.py" << 'PYEOF'
def also_broken(
    y = [
    // not python
    /* truncated */
PYEOF
cat > "$WORKTREE/_verify_test_feature3.py" << 'PYEOF'
def third_broken(
    z = (
    # ... rest omitted
PYEOF
git add -A && git commit -m "worker implements feature (badly)" --quiet

# Step 2: Worker runs verify — should fail
run_oat worker verify --json --skip-tests
CYCLE_FAIL=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print('failed' if not d.get('overall_passed', True) else 'passed')
" 2>/dev/null || echo "parse_error")

if [[ "$CYCLE_FAIL" == "failed" ]]; then
    pass "benchmark cycle: verify correctly fails on bad code"
else
    fail "benchmark cycle: verify correctly fails on bad code" "got: $CYCLE_FAIL"
fi

# Step 3: Worker tries oat pr create — should be blocked
run_oat pr create --title "Bad PR" --body "test"
if [[ $OAT_EXIT -ne 0 ]]; then
    pass "benchmark cycle: PR gate blocks after failed verify"
else
    fail "benchmark cycle: PR gate blocks after failed verify" "exit=0"
fi

# Step 4: Worker fixes the code
rm -f "$WORKTREE"/_verify_test_feature*.py
cat > "$WORKTREE/_verify_test_feature.py" << 'PYEOF'
def calculate_total(items: list) -> float:
    total = 0.0
    for item in items:
        total += item.get("price", 0) * item.get("quantity", 1)
    return total

if __name__ == "__main__":
    sample = [{"price": 10.0, "quantity": 2}, {"price": 5.0, "quantity": 3}]
    print(f"Total: {calculate_total(sample)}")
PYEOF
git add -A && git commit -m "worker fixes code" --quiet

# Step 5: Worker re-runs verify — should pass now
run_oat worker verify --json --skip-tests
CYCLE_PASS=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print('passed' if d.get('overall_passed', False) else 'failed')
" 2>/dev/null || echo "parse_error")

if [[ "$CYCLE_PASS" == "passed" ]]; then
    pass "benchmark cycle: verify passes after fix"
else
    fail "benchmark cycle: verify passes after fix" "got: $CYCLE_PASS"
fi

# Step 6: PR gate should now allow (may fail at gh pr create, but gate passes)
run_oat pr create --title "Good PR" --body "test"
if echo "$OAT_OUT" | grep -q "Verification passed"; then
    pass "benchmark cycle: PR gate passes after successful verify"
elif [[ $OAT_EXIT -eq 0 ]]; then
    pass "benchmark cycle: PR gate passes after successful verify"
else
    # Check if it failed for a non-gate reason (e.g., gh pr create network error)
    if echo "$OAT_OUT" | grep -qi "verif"; then
        fail "benchmark cycle: PR gate passes" "still blocked by verification"
    else
        pass "benchmark cycle: PR gate passes (gh pr create failed for non-gate reason)"
    fi
fi

# =============================================================================
# Test 17: Verification log has correct structure for collect.sh
# =============================================================================
log "Verification log structure for collect.sh"
VLOG="$HOME/.oat/verification.log"
VLOG_OK=$(python3 -c "
import json, os
vlog = '$VLOG'
if not os.path.exists(vlog):
    print('no_file')
    exit(0)
required_keys = ['commit_sha', 'repo_name', 'agent_name', 'overall_score', 'overall_passed', 'duration_seconds', 'checks']
check_keys = ['task_alignment', 'input_validation', 'file_integrity', 'syntax_validation', 'test_execution']
with open(vlog) as f:
    lines = [l.strip() for l in f.readlines() if l.strip()]
if not lines:
    print('empty')
    exit(0)
# Check last entry
entry = json.loads(lines[-1])
for k in required_keys:
    if k not in entry:
        print(f'missing_key:{k}')
        exit(0)
checks = entry.get('checks', {})
for ck in check_keys:
    if ck not in checks:
        print(f'missing_check:{ck}')
        exit(0)
    sub = checks[ck]
    if 'score' not in sub or 'passed' not in sub:
        print(f'missing_subkey_in:{ck}')
        exit(0)
print('ok')
" 2>/dev/null || echo "error")
if [[ "$VLOG_OK" == "ok" ]]; then
    pass "verification log has all fields collect.sh needs"
else
    fail "verification log structure" "got: $VLOG_OK"
fi

# =============================================================================
# Test 18: Multiple verify runs for same commit (fix-then-pass pattern)
# The log should have both entries; getLastVerificationForCommit returns the latest
# =============================================================================
log "Fix-then-pass: latest entry wins"
# We already have a passing verify from the cycle test above.
# Add a bad file, verify (fail), then remove it, verify again (pass)
cat > "$WORKTREE/_verify_test_tmp_bad.py" << 'PYEOF'
def bad(
    <!-- truncated -->
PYEOF
git add -A && git commit -m "temp bad" --quiet

run_oat worker verify --skip-tests 2>/dev/null  # will fail, that's fine
FTP_SHA_BAD=$(git rev-parse HEAD)

# Now fix
rm "$WORKTREE/_verify_test_tmp_bad.py"
git add -A && git commit -m "temp fix" --quiet

run_oat worker verify --skip-tests 2>/dev/null  # should pass
FTP_SHA_GOOD=$(git rev-parse HEAD)

FTP_CHECK=$(python3 -c "
import json
with open('$VLOG') as f:
    lines = [l.strip() for l in f if l.strip()]
# Find entries for the good commit
for line in reversed(lines):
    entry = json.loads(line)
    if entry.get('commit_sha') == '$FTP_SHA_GOOD':
        print('passed' if entry.get('overall_passed') else 'failed')
        exit(0)
print('not_found')
" 2>/dev/null || echo "error")
if [[ "$FTP_CHECK" == "passed" ]]; then
    pass "fix-then-pass: latest verify result is used"
else
    fail "fix-then-pass: latest verify result" "got: $FTP_CHECK"
fi

# =============================================================================
# Test 19: Base branch detection works (not hardcoded to origin/main)
# =============================================================================
log "Base branch detection"
# In information-bot, origin/main should exist. Verify getModifiedFiles finds files.
run_oat worker verify --json --skip-tests
FILES_FOUND=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
# If getModifiedFiles works, at least file_integrity should mention files or have no 'No modified files' issue
issues = d.get('file_integrity',{}).get('issues',[]) or []
has_no_files = any('No modified files' in str(i) for i in issues)
print('no_files' if has_no_files else 'has_files')
" 2>/dev/null || echo "parse_error")
if [[ "$FILES_FOUND" == "has_files" ]]; then
    pass "base branch detection: getModifiedFiles finds committed work"
else
    fail "base branch detection" "got: $FILES_FOUND (git diff may not be diffing against base branch)"
fi

# =============================================================================
# Test 20: Scores are in valid range and weights sum correctly
# =============================================================================
log "Score calculation sanity check"
run_oat worker verify --json --skip-tests
SCORE_OK=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
score = d.get('overall_score', -1)
passed = d.get('overall_passed', None)
# Score should be 0-100
if score < 0 or score > 100:
    print(f'score_out_of_range:{score}')
    exit(0)
# passed should match threshold
expected_passed = score >= 70
if passed != expected_passed:
    print(f'passed_mismatch:score={score},passed={passed}')
    exit(0)
# Each check should have 0-100 score
for check in ('task_alignment','file_integrity','syntax_validation','test_execution','input_validation'):
    cs = d.get(check,{}).get('score', -1)
    if cs < 0 or cs > 100:
        print(f'check_score_bad:{check}={cs}')
        exit(0)
print('ok')
" 2>/dev/null || echo "parse_error")
if [[ "$SCORE_OK" == "ok" ]]; then
    pass "scores in valid range, threshold logic correct"
else
    fail "score calculation" "got: $SCORE_OK"
fi

# =============================================================================
# Test 21: Verify timeout is sufficient (parent > child)
# =============================================================================
log "Timeout configuration"
# Check that the parent timeout (4min) > child test timeout (3min)
# by grepping the binary's source or just checking verify doesn't immediately timeout
TIMEOUT_OK=$(echo "$OAT_OUT" | python3 -c "
import sys, json
d = json.load(sys.stdin)
# If timeout was broken, we'd see 'context canceled' or 'timed out' in issues
all_issues = []
for check in ('task_alignment','file_integrity','syntax_validation','test_execution','input_validation'):
    all_issues.extend(d.get(check,{}).get('issues',[]) or [])
has_timeout = any('context canceled' in str(i).lower() or 'timed out' in str(i).lower() for i in all_issues)
print('timeout_error' if has_timeout else 'ok')
" 2>/dev/null || echo "parse_error")
if [[ "$TIMEOUT_OK" == "ok" ]]; then
    pass "no timeout/context errors during verification"
else
    fail "timeout configuration" "got: $TIMEOUT_OK"
fi

# =============================================================================
# Cleanup
# =============================================================================
echo ""
echo "Cleaning up..."
cd "$WORKTREE" 2>/dev/null || true
rm -f "$WORKTREE"/_verify_test_*.py 2>/dev/null || true
git checkout -- . 2>/dev/null || true
git clean -fd 2>/dev/null || true

cd "$(git -C "$WORKTREE" rev-parse --show-toplevel 2>/dev/null || dirname "$(dirname "$(readlink -f "$0")")")"
$OAT agent complete --repo "$REPO_NAME" --agent "$WORKER_NAME" 2>/dev/null || true

# =============================================================================
# Results
# =============================================================================
echo ""
echo "=========================================="
echo "  Results: $PASS/$TOTAL passed, $FAIL/$TOTAL failed"
echo "=========================================="

if [[ $FAIL -eq 0 ]]; then
    echo -e "  ${GREEN}ALL TESTS PASSED${NC}"
    exit 0
else
    echo -e "  ${RED}$FAIL TESTS FAILED${NC}"
    exit 1
fi
