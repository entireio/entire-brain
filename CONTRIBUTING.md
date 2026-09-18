# Contributing

1. **Branch from current `main`.** Tags mark releases on `main`; they are not branches to work from.
2. **Run the full suite before you push:**
   ```bash
   go test ./... -count=1
   gofmt -l -s .
   go vet ./...
   ```
3. **Every fix needs a test that fails without it.** Prove it: revert the fix's behaviour while keeping identifiers compiling, run the test, and put the failure output in the pull request.
4. **Reproduce before you fix.** Paste the real output of the defect on unpatched `main`, then the output after.
5. **Re-test combinations after merging.** Two individually-green pull requests can break `main` together.
6. **Never commit `.env` or `.session` files.** Guard before every commit:
   ```bash
   git add -A -n | grep -iE "\.env|\.session" && echo "ABORT"
   ```
7. **Re-record release evidence before tagging.** The evidence manifests pin source files by content hash; edits to pinned files mark a lane stale.

Be explicit about what you did *not* verify. A pull request that names its gaps is worth more than one that implies there are none.


## Tech Stack & Dependencies

| Layer | Choice |
|---|---|
| Language | **Go 1.27** |
| CLI framework | `spf13/cobra` |
| Git access | `go-git/go-git/v6` (plus hardened `git` subprocess calls) |
| Storage | SQLite — `modernc.org/sqlite` (pure Go, default) or `mattn/go-sqlite3` (cgo) |
| Vector search | `asg017/sqlite-vec-go-bindings` |
| TUI | `charmbracelet/bubbletea`, `bubbles`, `lipgloss` |
| Agent interface | Model Context Protocol over stdio (JSON-RPC 2.0, `Content-Length` framing) |

### Build tags

The default build is pure Go. Optional build tags enable additional indexing and embedding capabilities.

| Build | Command | What it adds |
|---|---|---|
| Default | `go build ./cmd/entire-brain` | Everything except the items below |
| `brain_cgo` | `go build -tags brain_cgo ./cmd/entire-brain` | Gemma-class embedder; conversation and history join vector search |
| `sqlite_fts5` | `go build -tags "brain_cgo sqlite_fts5" ./cmd/entire-brain` | SQLite FTS5 full-text index |

> Building without these tags degrades *explicitly* — affected commands say what is unavailable and why. They never silently return empty results.


See the [development reference](docs/reference.md#development) for mise tasks
and [installation options](docs/operations.md#local-install-brain-only) for local builds.
