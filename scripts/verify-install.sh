#!/usr/bin/env bash
# verify-install.sh — Verify OAT installation on a fresh machine.
#
# Checks that all prerequisites are present, OAT builds and installs
# correctly, the benchmark scripts are well-formed, and basic CLI
# commands work. Does NOT run an actual benchmark (no LLM calls, no
# GitHub repo creation).
#
# Usage:
#   ./scripts/verify-install.sh              # normal run
#   ./scripts/verify-install.sh --verbose    # show all checks
#   ./scripts/verify-install.sh --fix        # attempt to fix missing deps (macOS)

set -euo pipefail

VERBOSE=false
FIX=false

for arg in "$@"; do
    case "$arg" in
        --verbose) VERBOSE=true ;;
        --fix) FIX=true ;;
        --help) cat <<'EOF'
Usage: ./scripts/verify-install.sh [--verbose] [--fix]

Verifies OAT installation prerequisites and build.

Options:
  --verbose    Show all checks (not just failures)
  --fix        Attempt to install missing dependencies via brew (macOS only)
  --help       Show this help message

What it checks:
  1. System prerequisites (Go, git, gh, jq, python3, uv/pip)
  2. Go version compatibility
  3. OAT builds successfully
  4. OAT installs to $GOPATH/bin
  5. CLI commands work (oat --help, oat version)
  6. Python agent runtime dependencies
  7. Benchmark scripts are executable and have valid syntax
  8. Required benchmark files exist (issues.json)

What it does NOT do:
  - Create GitHub repos or issues
  - Make LLM API calls
  - Start the OAT daemon
  - Require any API keys
EOF
            exit 0 ;;
        *) echo "Unknown flag: $arg"; exit 1 ;;
    esac
done

# --- Framework ---

PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0

pass() {
    PASS_COUNT=$((PASS_COUNT + 1))
    if [[ "$VERBOSE" == true ]]; then
        echo "  PASS: $1"
    fi
}

fail() {
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "  FAIL: $1"
    if [[ -n "${2:-}" ]]; then
        echo "        $2"
    fi
}

warn() {
    WARN_COUNT=$((WARN_COUNT + 1))
    echo "  WARN: $1"
    if [[ -n "${2:-}" ]]; then
        echo "        $2"
    fi
}

# --- Locate repo root ---

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ ! -f "$REPO_ROOT/go.mod" ]]; then
    echo "Error: Cannot find go.mod. Run this script from the repo root or scripts/ directory."
    exit 1
fi

cd "$REPO_ROOT"

echo ""
echo "=========================================="
echo "  OAT Installation Verification"
echo "=========================================="
echo ""
echo "  Repo: $REPO_ROOT"
echo ""

# ==========================================================================
# 1. System Prerequisites
# ==========================================================================

echo "--- System Prerequisites ---"

check_cmd() {
    local cmd="$1"
    local install_hint="${2:-}"
    if command -v "$cmd" &>/dev/null; then
        local version
        version=$("$cmd" --version 2>&1 | head -1 || echo "unknown")
        pass "$cmd found ($version)"
        return 0
    else
        if [[ "$FIX" == true && -n "$install_hint" ]] && command -v brew &>/dev/null; then
            echo "  FIX:  Installing $cmd via: $install_hint"
            eval "$install_hint" || true
            if command -v "$cmd" &>/dev/null; then
                pass "$cmd installed successfully"
                return 0
            fi
        fi
        fail "$cmd not found" "${install_hint:+Install with: $install_hint}"
        return 1
    fi
}

check_cmd "go" "brew install go"
check_cmd "git" "brew install git"
check_cmd "gh" "brew install gh"
check_cmd "jq" "brew install jq"
check_cmd "python3" "brew install python3"

# uv is preferred but pip works too
if command -v uv &>/dev/null; then
    pass "uv found (fast Python package manager)"
elif command -v pip3 &>/dev/null || command -v pip &>/dev/null; then
    warn "uv not found, pip available (uv is faster for benchmarks)" \
        "Install with: brew install uv"
else
    fail "Neither uv nor pip found" "Install with: brew install uv"
fi

echo ""

# ==========================================================================
# 2. Go Version
# ==========================================================================

echo "--- Go Version ---"

if command -v go &>/dev/null; then
    GO_VERSION=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | sed 's/go//')
    REQUIRED_VERSION=$(grep '^go ' go.mod | awk '{print $2}')
    GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
    GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
    REQ_MAJOR=$(echo "$REQUIRED_VERSION" | cut -d. -f1)
    REQ_MINOR=$(echo "$REQUIRED_VERSION" | cut -d. -f2)

    if [[ "$GO_MAJOR" -gt "$REQ_MAJOR" ]] || \
       [[ "$GO_MAJOR" -eq "$REQ_MAJOR" && "$GO_MINOR" -ge "$REQ_MINOR" ]]; then
        pass "Go $GO_VERSION >= required $REQUIRED_VERSION"
    else
        fail "Go $GO_VERSION < required $REQUIRED_VERSION" \
            "Update Go: brew upgrade go  (or download from https://go.dev/dl/)"
    fi
fi

echo ""

# ==========================================================================
# 3. Build
# ==========================================================================

echo "--- Build ---"

if go build ./... 2>&1; then
    pass "go build ./... succeeded"
else
    fail "go build ./... failed"
fi

echo ""

# ==========================================================================
# 4. Install
# ==========================================================================

echo "--- Install ---"

GOBIN="$(go env GOPATH)/bin"

if go install ./cmd/oat 2>&1; then
    pass "go install ./cmd/oat succeeded"
    if [[ -f "$GOBIN/oat" ]]; then
        pass "oat binary exists at $GOBIN/oat"
    else
        fail "oat binary not found at $GOBIN/oat"
    fi
else
    fail "go install ./cmd/oat failed"
fi

if go install ./cmd/oat-agent 2>&1; then
    pass "go install ./cmd/oat-agent succeeded"
else
    fail "go install ./cmd/oat-agent failed"
fi

# Check PATH
if command -v oat &>/dev/null; then
    pass "oat is on PATH"
else
    warn "oat not on PATH" \
        "Add to your shell profile: export PATH=\"\$PATH:$GOBIN\""
fi

echo ""

# ==========================================================================
# 5. CLI Smoke Test
# ==========================================================================

echo "--- CLI Smoke Test ---"

if command -v oat &>/dev/null; then
    if oat --help &>/dev/null; then
        pass "oat --help works"
    else
        fail "oat --help failed"
    fi

    OAT_VERSION=$(oat version 2>&1 || echo "")
    if [[ -n "$OAT_VERSION" ]]; then
        pass "oat version: $OAT_VERSION"
    else
        warn "oat version returned empty (may not have a version subcommand yet)"
    fi
else
    fail "oat not on PATH, skipping CLI smoke test"
fi

echo ""

# ==========================================================================
# 6. Python Agent Runtime
# ==========================================================================

echo "--- Agent Runtime ---"

if [[ -d "agent-runtime" ]]; then
    pass "agent-runtime/ directory exists"

    if [[ -f "agent-runtime/pyproject.toml" ]]; then
        pass "agent-runtime/pyproject.toml exists"
    else
        warn "agent-runtime/pyproject.toml not found"
    fi

    # Check if agent-runtime symlink exists at GOBIN
    if [[ -L "$GOBIN/agent-runtime" || -d "$GOBIN/agent-runtime" ]]; then
        pass "agent-runtime symlink at $GOBIN/agent-runtime"
    else
        warn "No agent-runtime symlink at $GOBIN/agent-runtime" \
            "Run: ./scripts/install.sh  (creates the symlink)"
    fi
else
    fail "agent-runtime/ directory not found"
fi

echo ""

# ==========================================================================
# 7. Benchmark Scripts
# ==========================================================================

echo "--- Benchmark Scripts ---"

BENCH_SCRIPTS=(
    "benchmarks/run.sh"
    "benchmarks/setup.sh"
    "benchmarks/collect.sh"
    "benchmarks/acceptance-test.sh"
    "benchmarks/summarize.sh"
    "benchmarks/cleanup.sh"
)

for script in "${BENCH_SCRIPTS[@]}"; do
    if [[ ! -f "$script" ]]; then
        fail "$script not found"
        continue
    fi

    if [[ ! -x "$script" ]]; then
        if [[ "$FIX" == true ]]; then
            chmod +x "$script"
            pass "$script made executable"
        else
            fail "$script exists but not executable" "Fix with: chmod +x $script"
        fi
    else
        pass "$script is executable"
    fi

    # Syntax check
    if bash -n "$script" 2>&1; then
        pass "$script syntax valid"
    else
        fail "$script has syntax errors"
    fi
done

# Check issues.json
if [[ -f "benchmarks/issues.json" ]]; then
    ISSUE_COUNT=$(jq 'length' benchmarks/issues.json 2>/dev/null || echo "0")
    if [[ "$ISSUE_COUNT" -eq 20 ]]; then
        pass "benchmarks/issues.json has $ISSUE_COUNT issues (expected 20)"
    else
        warn "benchmarks/issues.json has $ISSUE_COUNT issues (expected 20)"
    fi
else
    fail "benchmarks/issues.json not found"
fi

echo ""

# ==========================================================================
# 8. Unit Tests
# ==========================================================================

echo "--- Unit Tests (quick) ---"

if go test ./internal/state/... -count=1 -timeout 30s 2>&1 | tail -1 | grep -q "^ok"; then
    pass "internal/state tests pass"
else
    fail "internal/state tests failed"
fi

if go test ./pkg/config/... -count=1 -timeout 30s 2>&1 | tail -1 | grep -q "^ok"; then
    pass "pkg/config tests pass"
else
    fail "pkg/config tests failed"
fi

if go test ./internal/names/... -count=1 -timeout 30s 2>&1 | tail -1 | grep -q "^ok"; then
    pass "internal/names tests pass"
else
    fail "internal/names tests failed"
fi

echo ""

# ==========================================================================
# 9. Environment Variables (informational)
# ==========================================================================

echo "--- API Keys (informational, not required) ---"

check_env() {
    local var="$1"
    local desc="$2"
    if [[ -n "${!var:-}" ]]; then
        pass "$var is set ($desc)"
    else
        warn "$var not set ($desc)"
    fi
}

check_env "GH_TOKEN_CLASSIC" "required for benchmarks"
check_env "ANTHROPIC_API_KEY" "for Claude models"
check_env "OPENAI_API_KEY" "for GPT models"
check_env "GOOGLE_API_KEY" "for Gemini models"

echo ""

# ==========================================================================
# Summary
# ==========================================================================

TOTAL=$((PASS_COUNT + FAIL_COUNT + WARN_COUNT))

echo "=========================================="
echo "  Results"
echo "=========================================="
echo ""
printf "  Passed:   %d\n" "$PASS_COUNT"
printf "  Warnings: %d\n" "$WARN_COUNT"
printf "  Failed:   %d\n" "$FAIL_COUNT"
printf "  Total:    %d\n" "$TOTAL"
echo ""

if [[ $FAIL_COUNT -eq 0 ]]; then
    echo "  OAT is ready to use!"
    if [[ $WARN_COUNT -gt 0 ]]; then
        echo "  (Some warnings above — review them for optional improvements)"
    fi
    echo ""
    echo "  Next steps:"
    echo "    1. Start the daemon:  oat daemon start"
    echo "    2. Initialize a repo: oat repo init <github-url>"
    echo "    3. Run a benchmark:   ./benchmarks/run.sh --model claude-sonnet-4-6 --name test"
    echo ""
    exit 0
else
    echo "  $FAIL_COUNT check(s) failed. Fix the issues above before proceeding."
    echo ""
    if [[ "$FIX" == false ]]; then
        echo "  Tip: Run with --fix to attempt automatic fixes (macOS with brew)"
    fi
    echo ""
    exit 1
fi
