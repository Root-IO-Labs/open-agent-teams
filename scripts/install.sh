#!/usr/bin/env bash
# Install OAT from source: Go binaries + Python virtual environment.
# Usage:
#   ./scripts/install.sh                    # install from current repo (run after clone)
#   ./scripts/install.sh https://github.com/yourorg/oat   # clone and install

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

err()  { echo -e "${RED}Error:${NC} $*" >&2; }
warn() { echo -e "${YELLOW}Warning:${NC} $*"; }
ok()   { echo -e "${GREEN}✓${NC} $*"; }

# ---------- prerequisite checks ----------

check_go() {
	if ! command -v go >/dev/null 2>&1; then
		err "go is not installed or not in PATH."
		echo "    Install Go 1.24.2+ from https://go.dev/dl/" >&2
		exit 1
	fi
	local ver
	ver="$(go version | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | sed 's/^go//')"
	ok "Go $ver found"
}

check_uv() {
	if ! command -v uv >/dev/null 2>&1; then
		err "uv (Python package manager) is not installed."
		echo "    Install it with:  curl -LsSf https://astral.sh/uv/install.sh | sh" >&2
		echo "    More info: https://docs.astral.sh/uv/getting-started/installation/" >&2
		exit 1
	fi
	ok "uv found ($(uv --version 2>/dev/null || echo 'unknown version'))"
}

check_python() {
	local py=""
	for candidate in python3 python; do
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

	local ver
	ver="$($py -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
	local major minor
	major="$(echo "$ver" | cut -d. -f1)"
	minor="$(echo "$ver" | cut -d. -f2)"

	if [[ "$major" -lt 3 ]] || { [[ "$major" -eq 3 ]] && [[ "$minor" -lt 11 ]]; }; then
		err "Python 3.11+ is required but found Python $ver."
		echo "    Install a newer version from https://www.python.org/downloads/" >&2
		exit 1
	fi
	ok "Python $ver found"
}

# ---------- install steps ----------

install_go_binaries() {
	local dir="$1"
	echo ""
	echo "==> Installing Go binaries from $dir ..."
	(cd "$dir" && go install ./cmd/oat ./cmd/oat-agent)

	local gobin
	gobin="$(go env GOPATH)/bin"

	local abs_runtime
	abs_runtime="$(cd "$dir" && pwd)/agent-runtime"
	if [ -d "$abs_runtime" ]; then
		ln -sfn "$abs_runtime" "$gobin/agent-runtime"
		ok "Symlinked $gobin/agent-runtime -> $abs_runtime"
	fi

	ok "Go binaries installed to $gobin"

	if ! echo "$PATH" | tr ':' '\n' | grep -q "$(go env GOPATH)/bin"; then
		warn "$gobin is not in your PATH."
		echo "    Add this to your shell profile (~/.zshrc or ~/.bashrc):"
		echo "      export PATH=\"\$PATH:\$(go env GOPATH)/bin\""
	fi
}

install_python_venv() {
	local dir="$1"
	local cli_dir="$dir/agent-runtime/libs/cli"

	if [[ ! -d "$cli_dir" ]]; then
		err "agent-runtime/libs/cli/ directory not found at $cli_dir"
		exit 1
	fi

	echo ""
	echo "==> Setting up Python virtual environment ..."
	(cd "$cli_dir" && uv sync)
	ok "Python venv created at $cli_dir/.venv"
}

# ---------- summary ----------

print_summary() {
	local gobin
	gobin="$(go env GOPATH)/bin"
	echo ""
	echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	echo -e "${GREEN}  OAT installed successfully!${NC}"
	echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	echo ""
	echo "  Binaries:  $gobin/oat, $gobin/oat-agent"
	echo "  Runtime:   $gobin/agent-runtime (symlink)"
	echo "  Python:    agent-runtime/libs/cli/.venv"
	echo ""
	echo "  Next steps:"
	echo "    1. Set up your API key:  echo 'ANTHROPIC_API_KEY=sk-...' >> ~/.oat/.env"
	echo "    2. Start the daemon:     oat start"
	echo "    3. Initialize a repo:    oat init https://github.com/yourorg/yourrepo"
	echo "    4. Watch progress:       oat ui"
	echo ""
	echo "  Full guide: docs/QUICKSTART.md"
	echo ""
}

# ---------- main ----------

echo "==> Checking prerequisites ..."
check_go
check_uv
check_python

if [[ -n "${1:-}" && "$1" =~ ^https?:// ]]; then
	repo_url="$1"
	tmpdir=""
	tmpdir=$(mktemp -d)
	trap 'rm -rf "$tmpdir"' EXIT
	echo ""
	echo "==> Cloning $repo_url ..."
	git clone --depth 1 "$repo_url" "$tmpdir"
	install_go_binaries "$tmpdir"
	install_python_venv "$tmpdir"
	print_summary
else
	root=""
	[[ -f "cmd/oat/main.go" ]] && root="."
	[[ -z "$root" && -f "../cmd/oat/main.go" ]] && root=".."
	if [[ -n "$root" ]]; then
		install_go_binaries "$root"
		install_python_venv "$root"
		print_summary
	else
		echo "Usage:"
		echo "  $0                          # run from repo root (after git clone)"
		echo "  $0 <repo-clone-url>         # clone from URL and install"
		echo ""
		echo "Example:"
		echo "  $0 https://github.com/Root-IO-Labs/open-agent-teams"
		exit 1
	fi
fi
