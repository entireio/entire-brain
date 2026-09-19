#!/usr/bin/env bash
# One-line installer for Entire Brain.
#
#   curl -fsSL https://raw.githubusercontent.com/entireio/entire-brain/main/scripts/get-brain.sh | bash
#
# Downloads a released binary for this platform, verifies it against the
# release checksums, and registers it with the Entire CLI. No Go toolchain and
# no C compiler: Brain's default build is pure Go, so the published binary runs
# as-is. Use scripts/install.sh instead when you want to build from source.
set -euo pipefail

GITHUB_REPO="entireio/entire-brain"
DEFAULT_INSTALL_DIR="${ENTIRE_BRAIN_INSTALL_DIR:-$HOME/.local/bin}"
CHANNEL="stable"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'; GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[0;33m'; NC=$'\033[0m'
else
  BOLD=""; GREEN=""; RED=""; YELLOW=""; NC=""
fi
info()  { printf '%s==>%s %s\n' "$GREEN" "$NC" "$*"; }
warn()  { printf '%s!%s   %s\n' "$YELLOW" "$NC" "$*" >&2; }
error() { printf '%serror:%s %s\n' "$RED" "$NC" "$*" >&2; exit 1; }

usage() {
  # Kept inline rather than read back out of $0: the documented invocation is
  # curl | bash, where $0 is "bash" and there is no script file to read.
  cat <<'USAGE'
Install Entire Brain.

  curl -fsSL https://raw.githubusercontent.com/entireio/entire-brain/main/scripts/get-brain.sh | bash

Options:
  --nightly          Install the newest nightly prerelease instead of stable
  --version vX.Y.Z   Install a specific release
  --dir <path>       Install somewhere other than ~/.local/bin
  -h, --help         Show this message

The download is verified against the release checksums. Brain's default build
is pure Go, so no Go toolchain and no C compiler are needed.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --nightly) CHANNEL="nightly"; shift ;;
    --version) WANT_VERSION="${2:?--version needs a value}"; shift 2 ;;
    --dir)     DEFAULT_INSTALL_DIR="${2:?--dir needs a path}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) error "unknown option: $1" ;;
  esac
done

for c in curl tar uname; do
  command -v "$c" >/dev/null || error "missing required command: $c"
done

# ---- platform ----------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux|darwin) : ;;
  *) error "unsupported OS: $os. Windows users: download the .zip from
  https://github.com/${GITHUB_REPO}/releases/latest" ;;
esac
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) error "unsupported architecture: $arch" ;;
esac

# ---- version -----------------------------------------------------------
resolve_version() {
  local api="https://api.github.com/repos/${GITHUB_REPO}/releases"
  local out=""
  if [ "$CHANNEL" = "nightly" ]; then
    out="$(curl -fsSL "${api}?per_page=20" 2>/dev/null \
      | grep -o '"tag_name": *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' \
      | grep -- '-nightly\.' | head -1 || true)"
  else
    out="$(curl -fsSL "${api}/latest" 2>/dev/null \
      | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/' || true)"
  fi
  printf '%s' "$out"
}

version="${WANT_VERSION:-}"
[ -n "$version" ] || version="$(resolve_version || true)"
[ -n "$version" ] || error "could not resolve a ${CHANNEL} release. Check your network, or
  pass --version vX.Y.Z. Releases: https://github.com/${GITHUB_REPO}/releases"
bare="${version#v}"

info "Installing Entire Brain ${BOLD}${version}${NC} (${os}/${arch})"

# ---- download and verify ----------------------------------------------
asset="entire-brain_${bare}_${os}_${arch}.tar.gz"
base="https://github.com/${GITHUB_REPO}/releases/download/${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}" \
  || error "no build for ${os}/${arch} in ${version}
  Available assets: https://github.com/${GITHUB_REPO}/releases/tag/${version}"

# The checksum is the only integrity signal this script has, and the README and
# SECURITY.md both promise the download is verified against it. So every way of
# not verifying is fatal: a checksums.txt we cannot fetch, a checksums.txt with
# no entry for this asset, no tool to hash with, and of course a mismatch.
# Failing open here would quietly downgrade a curl | bash install to nothing.
curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" 2>/dev/null \
  || error "could not fetch checksums.txt for ${version}
  Refusing to install an unverified binary. Every release publishes this file,
  so a missing one means the download path is not trustworthy right now.
  Assets: https://github.com/${GITHUB_REPO}/releases/tag/${version}"

if command -v sha256sum >/dev/null; then
  have="$(cd "$tmp" && sha256sum "$asset" | awk '{print $1}')"
elif command -v shasum >/dev/null; then
  have="$(cd "$tmp" && shasum -a 256 "$asset" | awk '{print $1}')"
else
  error "no sha256sum or shasum on PATH, so the download cannot be verified.
  Install coreutils (Linux) or use the system shasum (macOS), then re-run."
fi

want="$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}' | head -1)"
[ -n "$want" ] || error "checksums.txt has no entry for ${asset}"
[ "$have" = "$want" ] || error "checksum mismatch for ${asset}
  expected ${want}
  got      ${have}"
info "checksum verified"

tar -xzf "${tmp}/${asset}" -C "$tmp"
[ -f "${tmp}/entire-brain" ] || error "archive did not contain entire-brain"
chmod +x "${tmp}/entire-brain"

mkdir -p "$DEFAULT_INSTALL_DIR"
mv "${tmp}/entire-brain" "${DEFAULT_INSTALL_DIR}/entire-brain"
info "installed to ${BOLD}${DEFAULT_INSTALL_DIR}/entire-brain${NC}"

# ---- register with the Entire CLI --------------------------------------
if command -v entire >/dev/null; then
  if entire plugin install "${DEFAULT_INSTALL_DIR}/entire-brain" --force >/dev/null 2>&1; then
    info "registered with the Entire CLI"
    echo
    entire brain version 2>/dev/null || true
    echo
    if ! entire graph version >/dev/null 2>&1; then
      warn "Entire Graph is not installed. Brain uses it for code analysis, and
  setup will report the semantic index as unavailable without it:
  entire plugin install graph"
    fi
    printf '%sNext:%s  entire brain setup     (in a Git repository)\n' "$BOLD" "$NC"
  else
    warn "could not register the plugin automatically. Run:
  entire plugin install ${DEFAULT_INSTALL_DIR}/entire-brain --force"
  fi
else
  warn "the Entire CLI was not found on PATH. Install it first:
  curl -fsSL https://entire.io/install.sh | bash
then run:
  entire plugin install ${DEFAULT_INSTALL_DIR}/entire-brain --force"
fi

case ":${PATH}:" in
  *":${DEFAULT_INSTALL_DIR}:"*) : ;;
  *) warn "${DEFAULT_INSTALL_DIR} is not on your PATH. Add:
  export PATH=\"${DEFAULT_INSTALL_DIR}:\$PATH\"" ;;
esac
