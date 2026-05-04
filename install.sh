#!/usr/bin/env bash
# install.sh — One-line installer for OAT.
#
# Downloads the latest pre-built release for your platform, extracts it,
# installs the binaries to ~/.local/bin/, and sets up the Python venv.
#
# Usage (from anywhere):
#
#     curl -fsSL https://raw.githubusercontent.com/Root-IO-Labs/open-agent-teams/main/install.sh | bash
#
# Optional environment variables:
#     OAT_VERSION=v0.1.0   # pin a specific release tag (default: latest)
#     OAT_INSTALL_DIR      # override staging dir (default: ~/.oat/install)
#     OAT_BIN_DIR          # override symlink dir (default: ~/.local/bin)

set -euo pipefail

REPO_OWNER="Root-IO-Labs"
REPO_NAME="open-agent-teams"
REPO_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}"
API_URL="https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

err()  { echo -e "${RED}Error:${NC} $*" >&2; }
warn() { echo -e "${YELLOW}Warning:${NC} $*"; }
ok()   { echo -e "${GREEN}✓${NC} $*"; }
info() { echo -e "${BLUE}→${NC} $*"; }

# ---------- platform detection ----------

detect_os() {
    local os
    os="$(uname -s)"
    case "$os" in
        Darwin) echo "darwin" ;;
        Linux)  echo "linux" ;;
        *)
            err "Unsupported OS: $os"
            err "OAT currently ships pre-built binaries for macOS and Linux only."
            err "Build from source: ${REPO_URL}#building-from-source"
            exit 1
            ;;
    esac
}

detect_arch() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)  echo "x86_64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)
            err "Unsupported architecture: $arch"
            err "OAT supports x86_64 and arm64."
            exit 1
            ;;
    esac
}

# ---------- release lookup ----------

resolve_version() {
    if [[ -n "${OAT_VERSION:-}" ]]; then
        echo "$OAT_VERSION"
        return
    fi
    # Use the GitHub redirect on /releases/latest to find the tag without
    # needing jq or auth. Falls back to the API if curl can't follow.
    local resolved
    resolved="$(curl -sIL -o /dev/null -w '%{url_effective}' \
        "${REPO_URL}/releases/latest" 2>/dev/null | \
        sed -n 's|.*/tag/\(v[^/]*\)$|\1|p')"
    if [[ -z "$resolved" ]]; then
        # Last-resort: hit the API and grep for "tag_name".
        resolved="$(curl -fsSL "${API_URL}/releases/latest" 2>/dev/null | \
            grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
    fi
    if [[ -z "$resolved" ]]; then
        err "Could not resolve the latest release tag."
        err "Set OAT_VERSION explicitly (e.g. OAT_VERSION=v0.1.0)."
        exit 1
    fi
    echo "$resolved"
}

# ---------- download + extract + delegate ----------

download_and_extract() {
    local version="$1"
    local os="$2"
    local arch="$3"

    # Goreleaser archive name template:
    # oat_<version-without-v>_<os>_<arch>.tar.gz
    local version_no_v="${version#v}"
    local archive="oat_${version_no_v}_${os}_${arch}.tar.gz"
    local url="${REPO_URL}/releases/download/${version}/${archive}"

    local tmpdir
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    info "Downloading OAT ${version} for ${os}/${arch} ..."
    if ! curl -fsSL --retry 3 -o "${tmpdir}/${archive}" "$url"; then
        err "Failed to download $url"
        err "Check that the release exists at ${REPO_URL}/releases/tag/${version}"
        exit 1
    fi
    ok "Downloaded ${archive}"

    info "Extracting ..."
    tar -xzf "${tmpdir}/${archive}" -C "$tmpdir"
    ok "Extracted"

    # Find the install.sh inside the extracted tree (some archive layouts
    # nest the contents one level deep, others put them at the top).
    local installer
    if [[ -x "${tmpdir}/install.sh" ]]; then
        installer="${tmpdir}/install.sh"
    else
        installer="$(find "$tmpdir" -maxdepth 2 -name install.sh -perm -u+x | head -n1)"
    fi
    if [[ -z "${installer:-}" || ! -x "$installer" ]]; then
        err "install.sh not found in the extracted archive."
        err "The release tarball may be malformed."
        exit 1
    fi

    info "Running bundled installer ..."
    echo ""
    OAT_INSTALL_DIR="${OAT_INSTALL_DIR:-$HOME/.oat/install}" \
    OAT_BIN_DIR="${OAT_BIN_DIR:-$HOME/.local/bin}" \
        bash "$installer"
}

# ---------- main ----------

main() {
    local os arch version
    os="$(detect_os)"
    arch="$(detect_arch)"
    version="$(resolve_version)"

    echo ""
    echo "  Installing OAT ${version}  (${os}/${arch})"
    echo "  Repo: ${REPO_URL}"
    echo ""

    download_and_extract "$version" "$os" "$arch"
}

main "$@"
