#!/usr/bin/env bash
# Stage agent-runtime/ for release packaging.
#
# Copies agent-runtime/ to .release-stage/agent-runtime/, excluding venvs,
# Python bytecode caches, and other dev-only artifacts. GoReleaser archives
# pull from the staged copy so local snapshots don't drag in 400MB of venvs.
set -euo pipefail

SRC="${1:-agent-runtime}"
DST="${2:-.release-stage/agent-runtime}"

if [[ ! -d "$SRC" ]]; then
    echo "Error: source directory $SRC not found" >&2
    exit 1
fi

rm -rf "$DST"
mkdir -p "$(dirname "$DST")"

rsync -a \
    --exclude='.venv/' \
    --exclude='__pycache__/' \
    --exclude='*.pyc' \
    --exclude='*.pyo' \
    --exclude='.pytest_cache/' \
    --exclude='.ruff_cache/' \
    --exclude='.mypy_cache/' \
    --exclude='.tox/' \
    --exclude='node_modules/' \
    --exclude='.coverage' \
    --exclude='*.egg-info/' \
    --exclude='build/' \
    --exclude='dist/' \
    --exclude='.DS_Store' \
    --exclude='examples/' \
    --exclude='libs/partners/' \
    "$SRC/" "$DST/"

bytes=$(find "$DST" -type f | wc -l | tr -d ' ')
size=$(du -sh "$DST" | cut -f1)
echo "Staged $SRC -> $DST ($bytes files, $size)"
