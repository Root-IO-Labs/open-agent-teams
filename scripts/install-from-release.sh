#!/usr/bin/env bash
# install-from-release.sh — Install OAT from an extracted release tarball.
#
# Run this script from inside the extracted release archive. It will:
#   1. Verify Python 3.11+ and uv are installed (offers to install uv).
#   2. Move the tarball into a stable location at ~/.oat/install/.
#   3. Symlink `oat` and `oat-agent` into ~/.local/bin/ (or another
#      bin dir on your PATH).
#   4. Run `uv sync` inside the bundled agent-runtime to set up the
#      Python virtualenv.
#
# This script is shipped inside every release tarball as `install.sh`.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

err()  { echo -e "${RED}Error:${NC} $*" >&2; }
warn() { echo -e "${YELLOW}Warning:${NC} $*"; }
ok()   { echo -e "${GREEN}✓${NC} $*"; }
info() { echo -e "${BLUE}→${NC} $*"; }

# ---------- detect the extracted archive root ----------

# This script lives at <archive-root>/install.sh after extraction. Resolve
# the archive root from the script's own location so the user can run it
# from any cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARCHIVE_ROOT="$SCRIPT_DIR"

if [[ ! -x "$ARCHIVE_ROOT/oat" ]] || [[ ! -x "$ARCHIVE_ROOT/oat-agent" ]]; then
    err "This script must be run from inside an extracted OAT release archive."
    err "Expected to find oat and oat-agent next to install.sh in: $ARCHIVE_ROOT"
    exit 1
fi
if [[ ! -d "$ARCHIVE_ROOT/agent-runtime" ]]; then
    err "agent-runtime/ not found in $ARCHIVE_ROOT"
    err "The release archive may be corrupted — try downloading again."
    exit 1
fi

# ---------- prerequisites ----------

check_python() {
    local py=""
    for candidate in python3.13 python3.12 python3.11 python3 python; do
        if command -v "$candidate" >/dev/null 2>&1; then
            py="$candidate"
            break
        fi
    done
    if [[ -z "$py" ]]; then
        err "Python is not installed or not in PATH."
        echo "    Install Python 3.11+ from https://www.python.org/downloads/" >&2
        exit 1
    fi
    local ver major minor
    ver="$($py -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
    major="$(echo "$ver" | cut -d. -f1)"
    minor="$(echo "$ver" | cut -d. -f2)"
    if [[ "$major" -lt 3 ]] || { [[ "$major" -eq 3 ]] && [[ "$minor" -lt 11 ]]; }; then
        err "Python 3.11+ is required but found Python $ver."
        echo "    Install a newer version from https://www.python.org/downloads/" >&2
        exit 1
    fi
    ok "Python $ver found ($py)"
}

install_uv() {
    info "Installing uv (https://docs.astral.sh/uv/) ..."
    if ! curl -LsSf https://astral.sh/uv/install.sh | sh; then
        err "uv install failed."
        echo "    Install manually:  curl -LsSf https://astral.sh/uv/install.sh | sh" >&2
        exit 1
    fi
    # uv installer adds itself to ~/.local/bin or ~/.cargo/bin — make sure
    # we can find it for the rest of this run.
    for candidate in "$HOME/.local/bin" "$HOME/.cargo/bin"; do
        if [[ -x "$candidate/uv" ]]; then
            export PATH="$candidate:$PATH"
            break
        fi
    done
}

check_uv() {
    if command -v uv >/dev/null 2>&1; then
        ok "uv found ($(uv --version 2>/dev/null || echo 'unknown'))"
        return 0
    fi
    warn "uv (Python package manager) is not installed."
    if [[ -t 0 ]]; then
        read -r -p "    Install uv automatically? [Y/n] " reply
        reply="${reply:-Y}"
    else
        reply="Y"
    fi
    case "$reply" in
        [Yy]*) install_uv ;;
        *)
            err "uv is required. Install it manually:"
            echo "    curl -LsSf https://astral.sh/uv/install.sh | sh" >&2
            exit 1
            ;;
    esac
    if ! command -v uv >/dev/null 2>&1; then
        err "uv was installed but isn't on PATH yet."
        echo "    Restart your shell (or 'source ~/.zshrc' / '~/.bashrc') and re-run this installer." >&2
        exit 1
    fi
    ok "uv $(uv --version 2>/dev/null) installed"
}

# ---------- install steps ----------

INSTALL_DIR="${OAT_INSTALL_DIR:-$HOME/.oat/install}"
BIN_DIR="${OAT_BIN_DIR:-$HOME/.local/bin}"

stage_files() {
    info "Staging files into $INSTALL_DIR ..."
    mkdir -p "$INSTALL_DIR"
    # rsync if available (preserves perms cleanly), else cp -R.
    if command -v rsync >/dev/null 2>&1; then
        rsync -a --delete \
            --exclude='__pycache__' \
            --exclude='*.pyc' \
            --exclude='.venv' \
            "$ARCHIVE_ROOT/" "$INSTALL_DIR/"
    else
        rm -rf "$INSTALL_DIR"
        mkdir -p "$INSTALL_DIR"
        cp -R "$ARCHIVE_ROOT"/. "$INSTALL_DIR"/
    fi
    chmod +x "$INSTALL_DIR/oat" "$INSTALL_DIR/oat-agent"
    ok "Files staged at $INSTALL_DIR"
}

symlink_binaries() {
    mkdir -p "$BIN_DIR"
    ln -sfn "$INSTALL_DIR/oat" "$BIN_DIR/oat"
    ln -sfn "$INSTALL_DIR/oat-agent" "$BIN_DIR/oat-agent"
    ok "Symlinked oat, oat-agent into $BIN_DIR"
}

setup_python_venv() {
    local cli_dir="$INSTALL_DIR/agent-runtime/libs/cli"
    if [[ ! -d "$cli_dir" ]]; then
        err "agent-runtime/libs/cli/ not found in $INSTALL_DIR"
        exit 1
    fi
    info "Creating Python virtualenv in $cli_dir/.venv (uv sync) ..."
    (cd "$cli_dir" && uv sync --frozen) || {
        # --frozen requires uv.lock. If the lockfile is missing for any
        # reason, fall back to a non-frozen sync.
        warn "uv sync --frozen failed; retrying without --frozen ..."
        (cd "$cli_dir" && uv sync)
    }
    ok "Python venv ready at $cli_dir/.venv"
}

# ---------- summary ----------

print_summary() {
    local on_path="no"
    if echo "$PATH" | tr ':' '\n' | grep -Fxq "$BIN_DIR"; then
        on_path="yes"
    fi

    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo -e "  ${BOLD}${GREEN}OAT installed successfully!${NC}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Binaries:    $BIN_DIR/oat, $BIN_DIR/oat-agent"
    echo "  Install:     $INSTALL_DIR"
    echo "  Python venv: $INSTALL_DIR/agent-runtime/libs/cli/.venv"
    echo ""
    if [[ "$on_path" != "yes" ]]; then
        # Render the PATH hint with $HOME collapsed if BIN_DIR is under it,
        # so users see the more familiar `$HOME/.local/bin` form by default
        # but custom OAT_BIN_DIR overrides still get a literal path that
        # actually works.
        local path_hint="$BIN_DIR"
        if [[ "$BIN_DIR" == "$HOME/"* ]]; then
            path_hint="\$HOME/${BIN_DIR#$HOME/}"
        fi
        echo -e "  ${YELLOW}${BIN_DIR} is not on your PATH.${NC}"
        echo "  Add this to your shell profile (~/.zshrc or ~/.bashrc):"
        echo ""
        echo "      export PATH=\"${path_hint}:\$PATH\""
        echo ""
        echo "  Then reload:  source ~/.zshrc   (or restart your terminal)"
        echo ""
    fi
    echo "  Next steps:"
    echo "    1. Set your API key:   echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.oat/.env"
    echo "    2. Start the daemon:   oat start"
    echo "    3. Initialize a repo:  oat init https://github.com/yourorg/yourrepo"
    echo "    4. Watch progress:     oat ui"
    echo ""
    echo "  Full guide:    https://github.com/Root-IO-Labs/open-agent-teams#getting-started"
    echo "  Verify install: oat version"
    echo ""
}

# ---------- main ----------

echo "==> Checking prerequisites ..."
check_python
check_uv

echo ""
echo "==> Installing OAT ..."
stage_files
symlink_binaries
setup_python_venv

print_summary
