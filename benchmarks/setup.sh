#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GITHUB_OWNER="${GITHUB_OWNER:-$(gh api /user --jq '.login' 2>/dev/null || echo 'Root-IO-Labs')}"

usage() {
    cat <<'EOF'
Usage: ./benchmarks/setup.sh --model <model> [options]

Create a private benchmark repo from robotic-barista and optionally start OAT.

Required:
  --model <model>           Default LLM model for all OAT agents

Options:
  --worker-model <model>    Override model for workers (default: same as --model)
  --name <suffix>           Repo name suffix (default: timestamp)
  --setup-only              Only create repo and issues; skip OAT initialization
  --help                    Show this help message

Examples:
  ./benchmarks/setup.sh --model claude-sonnet-4-6
  ./benchmarks/setup.sh --model claude-sonnet-4-6 --worker-model gemini-2.5-pro --name gemini-test
  ./benchmarks/setup.sh --model claude-sonnet-4-6 --setup-only
EOF
    exit 0
}

MODEL=""
WORKER_MODEL=""
NAME_SUFFIX=""
SETUP_ONLY=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --model) MODEL="$2"; shift 2 ;;
        --worker-model) WORKER_MODEL="$2"; shift 2 ;;
        --name) NAME_SUFFIX="$2"; shift 2 ;;
        --setup-only) SETUP_ONLY=true; shift ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; echo "Run with --help for usage."; exit 1 ;;
    esac
done

if [[ -z "$MODEL" ]]; then
    echo "Error: --model is required"
    echo "Run with --help for usage."
    exit 1
fi

if [[ -z "$NAME_SUFFIX" ]]; then
    NAME_SUFFIX="$(date +%s)"
fi

REPO_NAME="oat-robotic-barista-${NAME_SUFFIX}"
REPO_FULL="${GITHUB_OWNER}/${REPO_NAME}"
REPO_URL="https://github.com/${REPO_FULL}"

# --- Preflight checks ---

GH_TOKEN="${GH_TOKEN_CLASSIC:-${GH_TOKEN:-}}"
if [[ -z "$GH_TOKEN" ]]; then
    echo "Error: GH_TOKEN or GH_TOKEN_CLASSIC must be set."
    echo "A GitHub token with 'repo' scope is required to create benchmark repos."
    exit 1
fi

if ! command -v gh &>/dev/null; then
    echo "Error: 'gh' (GitHub CLI) is required but not found."
    exit 1
fi

if ! command -v jq &>/dev/null; then
    echo "Error: 'jq' is required but not found."
    exit 1
fi

if [[ "$SETUP_ONLY" == false ]] && ! command -v oat &>/dev/null; then
    echo "Error: 'oat' is required but not found. Use --setup-only to skip OAT init."
    exit 1
fi

export GH_TOKEN

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

echo "==> Benchmark Setup"
echo "    Source:       benchmarks/robotic-barista/ (bundled)"
echo "    Target:       ${REPO_FULL}"
echo "    Model:        ${MODEL}"
if [[ -n "$WORKER_MODEL" ]]; then
    echo "    Worker Model: ${WORKER_MODEL}"
fi
echo ""

# --- Step 1: Create private repo ---

echo "==> Creating private repo ${REPO_FULL}..."
gh repo create "${REPO_FULL}" --private --description "OAT benchmark: ${MODEL}" 2>&1
echo "    Created: ${REPO_URL}"

# --- Step 2: Copy bundled source and push ---

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "==> Preparing benchmark repo from bundled source..."
cp -r "${SCRIPT_DIR}/robotic-barista" "${TMPDIR}/source"
# Overlay OAT-level docs that may have been updated independently of the bundle
cp "${SCRIPT_DIR}/blackbox-testing.md" "${TMPDIR}/source/docs/blackbox-testing.md"
cd "${TMPDIR}/source"
git init -b main --quiet
git add -A
git commit -m "Initial commit: robotic-barista benchmark scaffold" --quiet 2>&1

echo "==> Pushing initial state to ${REPO_FULL}..."
git remote add origin "https://x-access-token:${GH_TOKEN}@github.com/${REPO_FULL}.git"
sleep 2 # GitHub's Git hosting lags behind the API after repo creation
for attempt in 1 2 3; do
    if git push --force origin main --quiet 2>&1; then
        break
    fi
    if [[ $attempt -eq 3 ]]; then
        echo "Error: git push failed after $attempt attempts"
        exit 1
    fi
    echo "    Push attempt $attempt failed (repo still propagating), retrying in 3s..."
    sleep 3
done
git remote set-url origin "${REPO_URL}.git"
echo "    Pushed initial commit to ${REPO_URL}"

cd "${SCRIPT_DIR}"

# --- Step 3: Create labels ---

echo "==> Creating labels..."

LABELS=(
    "wave:0" "wave:1" "wave:2" "wave:3" "wave:4"
    "wave:fix-0" "wave:fix-1" "wave:fix-2" "wave:fix-3"
    "blocker"
    "area:testing" "area:domain" "area:storage" "area:services" "area:cli" "area:documentation"
    "risk:low" "risk:medium" "risk:high"
    "type:test" "type:implementation" "type:documentation"
    "layer:interface" "layer:integration" "layer:system"
    "tdd:required"
    "parallel"
    "oat"
)

for label in "${LABELS[@]}"; do
    gh label create "$label" --repo "${REPO_FULL}" --color "ededed" --force 2>/dev/null || true
done
echo "    Created ${#LABELS[@]} labels"

# --- Step 4: Create issues ---

echo "==> Creating issues..."

ISSUES_FILE="${SCRIPT_DIR}/issues.json"
if [[ ! -f "$ISSUES_FILE" ]]; then
    echo "Error: issues.json not found at ${ISSUES_FILE}"
    exit 1
fi

ISSUE_COUNT=$(jq 'length' "$ISSUES_FILE")
for i in $(seq 0 $((ISSUE_COUNT - 1))); do
    TITLE=$(jq -r ".[$i].title" "$ISSUES_FILE")
    BODY=$(jq -r ".[$i].body" "$ISSUES_FILE")
    EXPECTED_NUM=$(jq -r ".[$i].number" "$ISSUES_FILE")

    LABEL_ARGS=""
    LABEL_LIST=$(jq -r ".[$i].labels[]" "$ISSUES_FILE")
    for label in $LABEL_LIST; do
        LABEL_ARGS="${LABEL_ARGS} --label ${label}"
    done

    CREATED_URL=$(gh issue create \
        --repo "${REPO_FULL}" \
        --title "${TITLE}" \
        --body "${BODY}" \
        ${LABEL_ARGS} 2>&1)

    CREATED_NUM=$(echo "$CREATED_URL" | grep -o '[0-9]*$')
    if [[ "$CREATED_NUM" != "$EXPECTED_NUM" ]]; then
        echo "    WARNING: Issue '${TITLE}' created as #${CREATED_NUM}, expected #${EXPECTED_NUM}"
        echo "             Cross-references (Depends on: #N) may be incorrect"
    fi

    echo "    #${CREATED_NUM}: ${TITLE}"
done

echo "    Created ${ISSUE_COUNT} issues"

# --- Step 5: Start OAT (unless --setup-only) ---

if [[ "$SETUP_ONLY" == true ]]; then
    echo ""
    echo "==> Setup complete (--setup-only mode)"
    echo "    Repo: ${REPO_URL}"
    echo "    Issues: ${REPO_URL}/issues"
    echo ""
    echo "    To start OAT manually:"
    echo "      oat repo init ${REPO_URL} --model ${MODEL}"
    exit 0
fi

echo ""
log "==> Initializing OAT..."

OAT_CMD="oat repo init ${REPO_URL} --model ${MODEL}"
echo "    Running: ${OAT_CMD}"
eval "$OAT_CMD"

# Benchmarks are unattended — enable workspace stuck detection so the daemon
# can restart a stuck workspace instead of waiting for a human that isn't there.
oat config "${REPO_NAME}" --workspace-stuck-detection=true
echo "    Workspace stuck detection: enabled"

if [[ -n "$WORKER_MODEL" ]]; then
    echo ""
    echo "    Note: Worker model override (${WORKER_MODEL}) should be passed when"
    echo "    creating workers: oat worker create <task> --model ${WORKER_MODEL}"
fi

echo ""
log "==> Benchmark started!"
echo "    Repo:   ${REPO_URL}"
echo "    Model:  ${MODEL}"
