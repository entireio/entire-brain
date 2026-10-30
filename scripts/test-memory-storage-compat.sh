#!/usr/bin/env bash
# Build actual immutable releases; never exercise a mock of the old serializer.
set -euo pipefail
repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"
compat_tmp=$(mktemp -d)
trap 'rm -rf "$compat_tmp"' EXIT
go build -o "$compat_tmp/current-brain" ./cmd/entire-brain
# Test every stable release; pin commits, not moving tags.
for release in \
  v0.1.0:b3618702c9b5178b69a36dfc5b8165496a8ea5b1 \
  v0.2.0:321aa8f9206db2ba69dbe90c1a7d43d56e06e3d9 \
  v0.3.0:d9382246dca7b21e9f9a01db51f51cb2700ffb69 \
  v0.4.0:f2792b8d69d86fd2a689e8a24839de837ecd0369; do
  legacy_version=${release%%:*}
  legacy_commit=${release#*:}
  legacy_dir="$compat_tmp/$legacy_commit"
  mkdir -p "$legacy_dir"
  git archive "$legacy_commit" | tar -x -C "$legacy_dir"
  (cd "$legacy_dir" && go build -o "$compat_tmp/legacy-brain" ./cmd/entire-brain)
  ENTIRE_BRAIN_TEST_LEGACY_VERSION="$legacy_version" \
  ENTIRE_BRAIN_TEST_LEGACY_BINARY="$compat_tmp/legacy-brain" \
  ENTIRE_BRAIN_TEST_BINARY="$compat_tmp/current-brain" \
    go test -race -count=1 -timeout 10m ./internal/cli/internal/factstore -run '^TestRepoFactStoreReleased'
done
