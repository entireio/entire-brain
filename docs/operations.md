# Operations

`entire-brain` is a local Entire CLI plugin. It owns the persistent local brain,
semantic SQLite stores, bundle import/export, graph query surfaces, MCP stdio
tools, and benchmark/evidence commands.

## Local Install

```sh
scripts/install-local.sh
```

The script builds `./entire-brain`, installs it with `entire plugin install
./entire-brain --force`, and prints `entire brain version`. It fails before
writing anything if the parent `entire` CLI is not on `PATH`.

The default build is the pure-Go stack. To install the optional cgo vector stack:

```sh
BUILD_TAGS="brain_cgo sqlite_fts5" scripts/install-local.sh
```

## Release Archives

```sh
scripts/release.sh
```

The release script writes `dist/release-<version>/` with one `.tar.gz` archive per
target and a `SHA256SUMS` manifest. `VERSION=<value>` overrides the version;
otherwise the script uses `git describe --tags --always --dirty`.

Default targets are:

- `darwin/amd64`
- `darwin/arm64`
- `linux/amd64`
- `linux/arm64`
- `windows/amd64`

Set `ENTIRE_RELEASE_TARGETS` to override that list:

```sh
ENTIRE_RELEASE_TARGETS="linux/amd64" scripts/release.sh
```

The default release build uses the pure-Go fallback stack with `CGO_ENABLED=0`,
which is the portable distribution path. To build cgo artifacts, set
`BUILD_TAGS="brain_cgo sqlite_fts5"` and provide the matching platform compiler.
The script records checksums for artifacts it successfully builds; it does not
sign artifacts or publish them.

## Shared Baselines

Semantic bundle import/export is the policy-controlled shared baseline path for
this repository. Use:

```sh
entire brain bundle export --output /tmp/repo-brain.tar
entire brain bundle import /tmp/repo-brain.tar --sha256 <sha256>
```

The implementation validates checksums, schema compatibility, repo policy, path
traversal, symlink safety, size caps, and redaction before import. Shared
baselines remain explicit local files; there is no remote publishing or registry
discovery.
