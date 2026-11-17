# Contributing

Start here when changing entire-brain. The [contributor concepts guide](docs/concepts.md)
explains the vocabulary, source layers, and data flow; it is a map for contributors
and agents, not a replacement for the operational reference.

1. **Branch from current `main`.** Tags mark releases on `main`; they are not branches to work from.
2. **Run the full suite before you push:**
   ```bash
   mise run test:prepush
   ```
   That is `go test ./... -count=1`, `gofmt -l -s .` and `go vet ./...`, in one
   place so this file and the task cannot drift apart.

   It is uncached on purpose: `mise run test` is cached, which is what you want
   in the inner loop and not what you want as the last thing before a push --
   a cached pass tells you some earlier tree was green, not this one. It is
   also not `-race`: CI owns that, and `internal/cli` alone takes 17-20 minutes
   under `-race` on a developer machine while saturating it.

   **Two failures that mean your environment, not your change:**
   - `FORCE_COLOR` set in your shell makes colour-asserting tests fail on
     correct code. Unset it, or run `env -u FORCE_COLOR ...`.
   - A red `test (macos-latest)` on CI is more often the job's time cap than
     your diff. Re-read the log before you start bisecting.

   Before concluding a failure is yours, reproduce it on unpatched `main`.
3. **Every fix needs a test that fails without it.** Prove it: revert the fix's behaviour while keeping identifiers compiling, run the test, and put the failure output in the pull request.
4. **Reproduce before you fix.** Paste the real output of the defect on unpatched `main`, then the output after.
5. **Re-test combinations after merging.** Two individually-green pull requests can break `main` together.
6. **Never commit `.env` or `.session` files.** Guard before every commit:
   ```bash
   git add -A -n | grep -iE "\.env|\.session" && echo "ABORT"
   ```
7. **Run `mise run release:readiness` before tagging.** Retained benchmark results describe historical revisions, not the current release.

Be explicit about what you did *not* verify. A pull request that names its gaps is worth more than one that implies there are none.

## Source map

| Path | Start here for |
|---|---|
| `cmd/entire-brain/` | Plugin executable entry point |
| `internal/cli/` | Commands, source projections, retrieval, MCP, and hooks |
| `internal/config/`, `internal/repoid/` | Configuration and repository identity |
| `internal/agentsetup/`, `templates/` | Managed agent instructions and intake templates |
| `internal/tui/` | Terminal dashboard |
| `*_test.go` alongside source | Unit and contract tests |
| `docs/` | User, contributor, design, and operational documentation |
| `scripts/`, `mise-tasks/`, `mise.toml` | Installation, release scripts, and local verification tasks |
| `benchmarks/` | Archived benchmark results; no executable experiment tooling |

## Tech Stack & Dependencies

| Layer | Choice |
|---|---|
| Language | **Go 1.27** |
| CLI framework | `spf13/cobra` |
| Git access | `go-git/go-git/v6` (plus hardened `git` subprocess calls) |
| Storage | SQLite — `modernc.org/sqlite` (pure Go) |
| Retrieval | BM25 lexical ranking |
| TUI | `charmbracelet/bubbletea`, `bubbles`, `lipgloss` |
| Agent interface | Model Context Protocol over stdio (JSON-RPC 2.0, `Content-Length` framing) |

`go.mod` targets Go 1.27.3, the version the `github.com/entireio/cli`
dependency requires; `mise.toml` pins Go 1.27.3 and ShellCheck for local
development. See the
[development reference](docs/reference.md#development) for broader checks,
including the aggregate `check` task and build variants. The full release gate
is separate from the normal lint/test loop.

### Development tools

Project tools and test fixtures use Go. Python packages and virtual environments
are not required. Run `go run ./tools/coverage -race -output /tmp/brain-coverage`
for test coverage and instrumented CLI checks. The output directory must be empty.

CI runs the complete Go suite on Linux, macOS, and Windows. Go tooling divides
the Windows CLI suite into six groups and verifies complete test results.
Required checks also include real Brain–Graph integration and repository publication checks.

See the [development reference](docs/reference.md#development) for mise tasks
and [installation options](docs/operations.md#local-install-brain-only) for local builds.

For how a release is actually cut — the tag-triggered build, the nightly
channel, and what the readiness gate permits you to claim — see
[docs/releasing.md](docs/releasing.md).
