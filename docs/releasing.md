# Releasing Brain

Brain releases the same way Graph does: a tag triggers a build of every target,
the binaries and their checksums are attached to a GitHub release, and a nightly
prerelease is cut from `main` each morning.

## Cutting a release

1. Move the work into a `## [X.Y.Z]` section in `CHANGELOG.md`, dated.
2. Tag and push:

   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

`.github/workflows/release.yml` does the rest: it builds `linux`, `darwin`, and
`windows` for both `amd64` and `arm64`, packages each as a `.tar.gz` (`.zip` on
Windows), writes `checksums.txt`, and creates the release with generated notes.
Re-running it against an existing tag updates that release rather than failing.

You can also run it from the Actions tab with a version, which is the way to
re-cut a release whose upload failed.

### Why one runner builds everything

Brain's default build is pure Go — both the SQLite driver and the vector path
have cgo-free implementations (`internal/cli/sqlite_driver_purego.go`,
`internal/cli/embed_vec_purego.go`), and SQLite itself comes from
`modernc.org/sqlite` rather than a C binding — so `CGO_ENABLED=0`
cross-compiles every target from one Linux runner.
Graph needs a runner per OS because its tree-sitter bindings need cgo. The
release workflow asserts this rather than assuming it:

```yaml
- name: Verify the default build needs no cgo
  run: CGO_ENABLED=0 go build -o /dev/null ./cmd/entire-brain
```

If someone adds a cgo dependency to the default build, that step fails and the
release stops, instead of silently producing binaries that only run on the
build host.

The `brain_cgo` build tag still exists for the native embedder. It is not part
of a release build.

## Nightlies

`.github/workflows/nightly.yml` runs at 06:00 UTC, matching Graph, and tags
`vX.Y.Z-nightly.<timestamp>.<sha>` where `X.Y.Z` is the newest version in the
changelog.

It skips days `main` has not moved. A dated prerelease that is byte-identical to
yesterday's makes "which nightly has the fix" harder to answer, not easier, so
the run checks whether the newest prerelease already points at `HEAD` and exits
if it does.

## Pull requests build the release path

`release.yml` also runs on pull requests that change it. That run builds every
target and skips the publish job, so a change to the release process is proven
on the PR rather than on the tag that depends on it.

## What may be claimed in release material

`mise run release:readiness` reports which claims the retained evidence
supports. It classifies every track as one of `proven`, `local-gated`,
`pending-retained-proof`, `pending-target-evidence`, or `no-claim`, and the gate
fails when release material asserts something the evidence does not carry.

Run it before writing an announcement, and treat anything short of `proven` as
not sayable.

## Install channels

Releases reach users three ways:

| channel | resolves through |
|---|---|
| `entire plugin install brain` | the [plugin index](https://github.com/entireio/plugin-index) |
| `scripts/get-brain.sh` | the GitHub releases API, verifying `checksums.txt` |
| a downloaded archive | the release page |

The installer verifies the downloaded archive against `checksums.txt` and exits
non-zero on a mismatch. A missing checksum file is a warning; a wrong checksum
is fatal.
