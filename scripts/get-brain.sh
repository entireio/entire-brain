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
# Scratch space for the API lookup and the download. Created here, before the
# version lookup, so the lookup can keep the response headers and body.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The API lookup keeps the HTTP status, headers and body instead of letting
# curl -f collapse every failure into an empty string. Unauthenticated GitHub
# API calls are capped per IP address, and an exhausted cap answers 403 (or
# 429). Reporting that as "check your network" sends the user after the wrong
# problem, so say what actually happened and how to get past it.
api_status=""
api_get() {
  api_status="$(curl -sSL -D "${tmp}/api.headers" -o "${tmp}/api.json" \
    -w '%{http_code}' "$1" 2>/dev/null)" || api_status="000"
}

api_rate_limited() {
  case "$api_status" in 403|429) : ;; *) return 1 ;; esac
  grep -qiE '^x-ratelimit-remaining: *0[[:space:]]*$' "${tmp}/api.headers" 2>/dev/null \
    || grep -qi 'rate limit' "${tmp}/api.json" 2>/dev/null
}

rate_limit_error() {
  # Two different clocks. An exhausted primary quota (remaining: 0) resets at
  # x-ratelimit-reset, an epoch. A secondary limit can leave the primary quota
  # untouched and asks for a pause in retry-after, in seconds; its
  # x-ratelimit-reset belongs to the unrelated primary quota, so ignore it.
  local secs="" reset="" after="" wait=""
  if grep -qiE '^x-ratelimit-remaining: *0[[:space:]]*$' "${tmp}/api.headers" 2>/dev/null; then
    reset="$({ grep -iE '^x-ratelimit-reset:' "${tmp}/api.headers" || true; } \
      | tail -1 | tr -dc '0-9')"
    [ -z "$reset" ] || secs=$(( reset - $(date +%s) ))
  else
    after="$({ grep -iE '^retry-after:' "${tmp}/api.headers" 2>/dev/null || true; } \
      | tail -1 | tr -dc '0-9')"
    [ -z "$after" ] || secs="$after"
  fi
  if [ -n "$secs" ]; then
    [ "$secs" -ge 1 ] || secs=1
    wait=" It resets in about $(( (secs + 59) / 60 )) minute(s)."
  fi
  error "GitHub's API rate limit is exhausted (HTTP ${api_status} from api.github.com),
  so the newest ${CHANNEL} release could not be looked up.${wait}
  Wait and re-run, or skip the lookup by naming a release:
    curl -fsSL https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/get-brain.sh | bash -s -- --version vX.Y.Z
  Releases: https://github.com/${GITHUB_REPO}/releases"
}

resolve_version() {
  local api="https://api.github.com/repos/${GITHUB_REPO}/releases"
  if [ "$CHANNEL" = "nightly" ]; then
    api_get "${api}?per_page=20"
  else
    api_get "${api}/latest"
  fi
  api_rate_limited && rate_limit_error
  case "$api_status" in
    000) error "could not reach api.github.com to resolve a ${CHANNEL} release. Check your network, or
  pass --version vX.Y.Z. Releases: https://github.com/${GITHUB_REPO}/releases" ;;
    2??) : ;;
    *) error "could not resolve a ${CHANNEL} release: api.github.com answered HTTP ${api_status}.
  Pass --version vX.Y.Z to skip the lookup. Releases: https://github.com/${GITHUB_REPO}/releases" ;;
  esac
  if [ "$CHANNEL" = "nightly" ]; then
    version="$({ grep -o '"tag_name": *"[^"]*"' "${tmp}/api.json" || true; } \
      | sed 's/.*"\([^"]*\)"$/\1/' | { grep -- '-nightly\.' || true; } | head -1)"
  else
    version="$({ grep -o '"tag_name": *"[^"]*"' "${tmp}/api.json" || true; } \
      | head -1 | sed 's/.*"\([^"]*\)"$/\1/')"
  fi
}

version="${WANT_VERSION:-}"
[ -n "$version" ] || resolve_version
[ -n "$version" ] || error "could not resolve a ${CHANNEL} release: the GitHub API response
  named no matching tag. Pass --version vX.Y.Z. Releases: https://github.com/${GITHUB_REPO}/releases"
bare="${version#v}"

info "Installing Entire Brain ${BOLD}${version}${NC} (${os}/${arch})"

# ---- download and verify ----------------------------------------------
asset="entire-brain_${bare}_${os}_${arch}.tar.gz"
base="https://github.com/${GITHUB_REPO}/releases/download/${version}"

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

# pipefail would make a no-match grep (exit 1) kill the script at this
# assignment, so the error below would never print. Contain it here.
want="$({ grep " ${asset}\$" "${tmp}/checksums.txt" || true; } | awk '{print $1}' | head -1)"
[ -n "$want" ] || error "checksums.txt has no entry for ${asset}
  The release may have been published with different asset names.
  Assets: https://github.com/${GITHUB_REPO}/releases/tag/${version}"
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
