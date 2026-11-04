# Releasing Brain

Brain releases the same way Graph does: a tag triggers a build of every target,
the binaries and their checksums are attached to a GitHub release, and a nightly
prerelease is cut from `main` each morning.

## Cutting a release

1. Move the work into a `## [X.Y.Z]` section in `CHANGELOG.md`, dated.
2. Confirm the version's upgrade requirements and known limits.
   Use [memory storage compatibility](memory-storage-compatibility.md) for migration,
   backup, automatic cleanup, hosted sync, and Claude capture requirements.
3. Tag and push:

   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

`.github/workflows/release.yml` does the rest: it builds `linux`, `darwin`, and
`windows` for both `amd64` and `arm64`, packages each as a `.tar.gz` (`.zip` on
Windows), writes `checksums.txt`, and creates the release with generated notes.
Re-running it against an existing tag updates that release rather than failing.

Before announcing the release, add the version's compatibility and upgrade notes
from `CHANGELOG.md` to the published GitHub release description. The workflow uses
`--generate-notes`; it does not copy the changelog text into the release body.
Include the requirements that apply to that release.

You can also run it from the Actions tab with a version, which is the way to
re-cut a release whose upload failed.

### Why one runner builds everything

Brain's default build uses the pure-Go `modernc.org/sqlite` driver.
`CGO_ENABLED=0` cross-compiles the release targets from one Linux runner.
Graph needs a runner per OS because its tree-sitter bindings need cgo. The
release workflow asserts this rather than assuming it:

```yaml
- name: Verify the default build needs no cgo
  run: CGO_ENABLED=0 go build -o /dev/null ./cmd/entire-brain
```

If someone adds a cgo dependency to the default build, that step fails and the
release stops, instead of silently producing binaries that only run on the
build host.

The embedding stack and its build variants have been removed.

### Windows verification

Cross-compiling from one runner means the release path never executes a
Windows binary, and `test.yml`'s Windows suite runs with `CGO_ENABLED=1`
because the race detector needs cgo — not the pure-Go configuration that
ships. Both `release.yml` and `nightly.yml` therefore gate publishing on a
`verify windows` job on a Depot Windows runner (hosted `windows-latest` is
2-core on private repos and cannot finish `internal/cli` in one binary within
a reasonable budget — note this puts a Depot dependency on the publish path):
it runs `go test ./...` with
`CGO_ENABLED=0` and smoke-runs the CLI (`version`, `capabilities --json`, a
`remember`/`recall` roundtrip in a scratch repo) via `tools/windows-sanity.ps1`.
The release job runs the script against the actual `windows/amd64` artifact it
is about to publish; the nightly verifies its own equivalent build, because the
nightly builds and publishes in a single job. `windows/arm64` is still only
cross-compiled — no amd64 runner can execute it.

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

`mise run release:readiness` checks the current Go source, tests, and builds.
It does not validate historical performance claims.

Benchmark reports remain in `benchmarks/agent-brain/` as historical results.
Their experiment runners, fixtures, and evidence-audit tasks have been removed.
Use the recorded source revision when interpreting those reports.
Do not present them as measurements of the current release.

## Install channels

Releases reach users three ways:

| channel | resolves through |
|---|---|
| `entire plugin install brain` | the [plugin index](https://github.com/entireio/plugin-index) |
| `scripts/get-brain.sh` | the GitHub releases API, verifying `checksums.txt` |
| a downloaded archive | the release page |

The installer verifies the downloaded archive against `checksums.txt` and exits
non-zero on a mismatch. Missing or incorrect checksums are fatal.
