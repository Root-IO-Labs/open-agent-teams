#!/usr/bin/env bash
set -euo pipefail

GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/cleanup.sh --repo <name>

Clean up all local state from a benchmark run. Optionally delete the GitHub repo.

Required:
  --repo <name>             Benchmark repo name (e.g. oat-robotic-barista-sonnet46-auto)

Options:
  --delete-remote           Also delete the GitHub repo (requires delete_repo scope on token)
  --help                    Show this help message

Examples:
  ./benchmarks/cleanup.sh --repo oat-robotic-barista-sonnet46-auto
  ./benchmarks/cleanup.sh --repo oat-robotic-barista-sonnet46-auto --delete-remote
EOF
    exit 0
}

REPO_NAME=""
DELETE_REMOTE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --repo) REPO_NAME="$2"; shift 2 ;;
        --delete-remote) DELETE_REMOTE=true; shift ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$REPO_NAME" ]]; then
    echo "Error: --repo is required"
    echo "Run with --help for usage."
    exit 1
fi

REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"

echo "==> Cleaning up benchmark: ${REPO_NAME}"

# Step 1: Remove OAT state, agent sessions, worktrees
# Pipe 'y' so we don't hang if oat repo rm prompts for uncommitted-change confirmation
if echo y | oat repo rm "$REPO_NAME" >/dev/null 2>&1; then
    echo "    Removed OAT state and agent sessions"
else
    echo "    OAT state not found (already removed or never initialized)"
fi

# Step 2: Remove cloned repo directory
REPO_DIR="${HOME}/.oat/repos/${REPO_NAME}"
if [[ -d "$REPO_DIR" ]]; then
    rm -rf "$REPO_DIR"
    echo "    Removed repo directory: ${REPO_DIR}"
else
    echo "    Repo directory not found (already removed)"
fi

# Step 3: Remove worktree directory (in case oat repo rm missed it)
WT_DIR="${HOME}/.oat/wts/${REPO_NAME}"
if [[ -d "$WT_DIR" ]]; then
    rm -rf "$WT_DIR"
    echo "    Removed worktree directory: ${WT_DIR}"
fi

# Step 4: Remove output logs
OUTPUT_DIR="${HOME}/.oat/output/${REPO_NAME}"
if [[ -d "$OUTPUT_DIR" ]]; then
    rm -rf "$OUTPUT_DIR"
    echo "    Removed output logs: ${OUTPUT_DIR}"
else
    echo "    Output logs not found (already removed)"
fi

# Step 5: Optionally delete GitHub repo
if [[ "$DELETE_REMOTE" == true ]]; then
    GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
    if [[ -n "$GH_TOKEN" ]]; then
        export GH_TOKEN
    fi

    echo "    Deleting GitHub repo: ${REPO_FULL}..."
    if gh repo delete "$REPO_FULL" --yes 2>/dev/null; then
        echo "    Deleted GitHub repo"
    else
        echo "    Failed to delete GitHub repo (may need delete_repo scope on token)"
        echo "    Delete manually: https://github.com/${REPO_FULL}/settings"
    fi
else
    echo ""
    echo "    GitHub repo NOT deleted. To delete it:"
    echo "      - Run again with --delete-remote"
    echo "      - Or delete manually: https://github.com/${REPO_FULL}/settings"
fi

echo ""
echo "==> Cleanup complete"
