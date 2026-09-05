# Operations

`entire-brain` is a local Entire CLI plugin. It owns the persistent local brain,
semantic SQLite stores, bundle import/export, graph query surfaces, MCP stdio
tools, and benchmark/evidence commands.

## Full Install

```sh
scripts/install.sh
```

The end-to-end installer, and the one the README documents. It has four effects
on the machine and no others: it builds and registers the `entire-graph`
semantic provider, builds and registers `entire-brain`, writes the default
plugin configuration (`entire brain config init`), and runs `entire brain
doctor`. Both binaries are built inside their checkouts and linked into Entire's
managed plugin directory; nothing is installed system-wide.

Before it builds anything it checks every prerequisite -- `git`, the `entire`
CLI, a Go toolchain at least as new as the `go` directive in `go.mod`, and a C
compiler for `entire-graph`'s tree-sitter cgo bindings -- and reports all the
missing ones in one pass with the fix for each. `entire-brain` itself is a
pure-Go build; the cgo requirement is the provider's.

Neither checkout has to have a particular name, and the two do not have to be
siblings. The provider is resolved by three routes, and the one taken is printed
under `==> entire-graph semantic provider`:

| Route | Source | Notes |
| --- | --- | --- |
| 1 | `$ENTIRE_GRAPH_DIR` | Used exactly as given. A value that is not an `entire-graph` checkout is a hard error, never a silent fallback. |
| 2 | A checkout on this machine | Siblings of the brain checkout and of the main worktree when it is a linked one, the route-3 cache, and the usual source directories under `$HOME`. Accepted only when `cmd/entire-graph` and `scripts/install-local.sh` are both present, so a directory that merely has the name is skipped. |
| 3 | A shallow clone | `$ENTIRE_GRAPH_REPO` (default the public `entireio/entire-graph`) into `$ENTIRE_GRAPH_CACHE` (default `${XDG_CACHE_HOME:-~/.cache}/entire-brain/entire-graph`). Later runs reuse it and refresh it with a shallow fetch, and keep going on the cached copy when the network is unavailable. |

`ENTIRE_INSTALL_OFFLINE=1` disables route 3, so an air-gapped machine fails with
an explanation naming all three routes rather than blocking on a clone.

`scripts/bootstrap.sh` wraps this for a machine with nothing: it clones
`entire-brain` when it is not already present -- falling back to `gh repo clone`
for the private-repo credentials -- and then execs `scripts/install.sh`. There is
deliberately no `curl | sh` form: `entireio/entire-brain` is private, so
`raw.githubusercontent.com` serves 404 for it without a token; the source has to
be cloned regardless because the install is a source build; and cloning first
puts the script that registers your plugins on disk at a reviewable commit
before it runs.

## Local Install (brain only)

```sh
scripts/install-local.sh
```

The script builds `./entire-brain`, installs it with `entire plugin install
./entire-brain --force`, and prints `entire brain version`. It fails before
writing anything if the parent `entire` CLI is not on `PATH`. It does not touch
the provider, so `entire graph` has to be installed separately for the semantic
layer.

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
The script records checksums for artifacts it successfully builds; it also signs
archives when a local signing key is explicitly configured:

- `COSIGN_KEY=<key-ref>` with `cosign` on `PATH` writes `<archive>.sig`.
- `GPG_SIGNING_KEY=<key-id>` with `gpg` on `PATH` writes `<archive>.asc`.

The script does not publish artifacts.

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
