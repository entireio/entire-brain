#!/usr/bin/env bash
# Build actual immutable releases; never exercise a mock of the old serializer.
set -euo pipefail
repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"
compat_tmp=$(mktemp -d)
trap 'rm -rf "$compat_tmp"' EXIT
go build -o "$compat_tmp/current-brain" ./cmd/entire-brain
# Test supported stable releases v0.2.0 through v0.5.0; pin commits, not moving tags.
for release in \
  v0.2.0:321aa8f9206db2ba69dbe90c1a7d43d56e06e3d9 \
  v0.3.0:d9382246dca7b21e9f9a01db51f51cb2700ffb69 \
  v0.4.0:f2792b8d69d86fd2a689e8a24839de837ecd0369 \
  v0.5.0:fb99b029b4b03ad85c66022cb2114058485301f2; do
  legacy_version=${release%%:*}
  legacy_commit=${release#*:}
  printf 'Testing stable Brain %s\n' "$legacy_version"
  legacy_dir="$compat_tmp/$legacy_commit"
  mkdir -p "$legacy_dir"
  git archive "$legacy_commit" | tar -x -C "$legacy_dir"
  (cd "$legacy_dir" && go build -o "$legacy_dir/legacy-brain" ./cmd/entire-brain)
  compat_pattern='^(TestRepoFactStoreReleased|TestAutomaticMemoryCleanupReleased)'
  if [ "$legacy_version" = v0.5.0 ]; then
    # v0.5.0 already uses the flat store; the old-client legacy-writer suite
    # intentionally targets only the three pre-flat releases.
    compat_pattern='^TestAutomaticMemoryCleanupReleased$'
  fi
  ENTIRE_BRAIN_TEST_PRE_FLAT_BINARY="$compat_tmp/f2792b8d69d86fd2a689e8a24839de837ecd0369/legacy-brain" \
  ENTIRE_BRAIN_TEST_LEGACY_VERSION="$legacy_version" \
  ENTIRE_BRAIN_TEST_LEGACY_BINARY="$legacy_dir/legacy-brain" \
  ENTIRE_BRAIN_TEST_BINARY="$compat_tmp/current-brain" \
    go test -race -count=1 -timeout 10m ./internal/cli/internal/factstore ./internal/cli -run "$compat_pattern"
done
