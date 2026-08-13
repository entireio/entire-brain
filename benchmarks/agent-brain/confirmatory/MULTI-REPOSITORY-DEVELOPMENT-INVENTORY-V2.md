# Multi-repository development inventory v2

This slice expands the development-only task inventory across three pinned repositories. It does
not run a candidate test, reverse a patch, call a model/provider, inspect private material, assign a
population split, create an owner key, or authorize calibration or holdout use. Every identity in
these artifacts has been inspected during development and is permanently ineligible for calibration
and confirmatory holdout.

## Pinned repository windows

Each scan uses exactly 31 first-parent integration units ending at the contract's pinned head. The
scanner requires the exact remote URL, requires the pinned head to be equal to or an ancestor of the
current authoritative remote tip, and recomputes the `pinned-head~31` base and unit count. A later
legitimate fetch therefore does not invalidate the checked snapshot.

| Repository ID | Remote ref | Base | Head | Candidates | Low / medium / high |
| --- | --- | --- | --- | ---: | ---: |
| `github.com/entireio/entire-brain` | `entireio/main` | `fc65f656615f4623a2370ec7943f8d575714cfe8` | `bc0203b761cbc500bfcb0acff723dad57ae37770` | 18 | 5 / 7 / 6 |
| `github.com/entirehq/entiredb` | `origin/main` | `4bde2fe4bc5bc913c35540e46cbb1d4f6fa131ab` | `860769996e8741ff8a93cc955dadb4e7a81f4317` | 25 | 9 / 4 / 12 |
| `github.com/entireio/entire-graph` | `origin/main` | `d3271d235a2669da67d9f6924bb31cf6fd82349c` | `fafe92878b5e920fe161d8078ee91762fe33e9b4` | 19 | 6 / 3 / 10 |

The resulting 62 candidates comprise 37 single-parent integration units and 25 two-parent feature
merges. Test evidence comprises 55 changed-Go-test-only candidates, five candidates with changed Go
tests plus fixture/helper evidence, and two fixture/helper-only candidates. All 62 have resolved
module and package-target bindings and are marked `structurally_ready_not_executed`; this is not a
negative-control result.

## Structural contract

`task_eligibility_v2.py` defines test evidence as changed `_test.go` files, any path below a
`testdata` component, and paths below a top-level `test` or `tests` directory. Production Go is
strictly `.go AND NOT test_evidence`, so the two sets cannot overlap. Repository ledgers retain exact
content commitments and counts rather than plaintext changed paths.

Changed `_test.go` evidence binds its package directory. Fixture/helper evidence binds the nearest
ancestor package that contains `_test.go` files in the candidate tree, without reversing the
fixture/helper. Package directories and command targets are stored as exact SHA-256 commitments;
module roots remain explicit. A missing module or owner binding makes a candidate structurally
blocked rather than silently dropping it. A changed-test package remains an exact compile target if
the change deletes its final `_test.go`; the recorded candidate-tree test count is then zero. A
fixture/helper owner, by contrast, must retain at least one candidate-tree `_test.go`.

Every candidate binds:

- exact commit, first parent, tree, first-parent position, and one- or two-parent unit kind;
- a single-parent lineage containing the integration commit itself, or the exact feature-branch
  lineage for a two-parent merge;
- disjoint production/test path-set commitments and evidence-kind counts;
- raw SHA-256 and `git patch-id --stable` identities for source, test, and full patches;
- candidate-tree `go.mod` and `go.work` blob IDs, raw hashes, directives, module roots, and test
  package target commitments;
- the exact Git binary version/hash that produces identities, the repository-specific Go binary
  version/hash, `GOOS`, `GOARCH`, `CGO_ENABLED`, `CC`, and `CXX`,
  plus the native C and C++ compiler driver version/target/hashes; and
- explicit unresolved source-session status and `not_executed` negative-control status.

All scanner Git subprocesses set `GIT_NO_REPLACE_OBJECTS=1`, `GIT_NO_LAZY_FETCH=1`, and
`GIT_OPTIONAL_LOCKS=0` internally. A repository with any `refs/replace/*` entry, nonempty
`info/grafts`, or shallow boundary is rejected as an additional fail-closed defense. System/global
Git config, config injection, external diff, and diff environment overrides are neutralized. Every
Git read invokes the verified absolute executable. Changed-path and patch commands pin literal
pathspecs, order, prefixes, full object IDs, context, algorithm,
heuristics, color, text conversion, renames, and submodule rendering. Attribute lookup is pinned to
each candidate commit; a nonempty repository `info/attributes` file or any effective `diff`
attribute on a diffed path fails closed. Adversarial tests require whole-ledger byte equality under
hostile local/global diff configuration and worktree attribute drift.

## Global exact-overlap registry

`development-task-overlap-registry-v1.json` combines the 62 v2 identities with the existing 23 CLI
v1 development identities. Its 85 candidates contain 27 low, 17 medium, and 41 high static-scope
entries. There are zero duplicate groups across commit ID, tree ID, source/test/full diff SHA-256,
or source/test/full stable patch ID.

All 85 diff and stable-patch comparison fields use the single
`canonical_git_object_diff_full_index_v1` profile. The 23 CLI v1 input's legacy raw source/test diff
hashes remain separately labeled and are revalidated, but are not compared against canonical v2
identities. The registry binds the exact registry builder, v2 scanner, CLI v1 scanner, CLI scanner's
canonical-JSON helper, schema, and Git binary/version; every v2 ledger must name the same current v2
scanner hash.

Repeated changed/source/test path-set commitments are reported separately. They are not treated as
task identity, family identity, or semantic overlap. Related-family, semantic, and source-session
overlap all remain unresolved because no owner-held commitments or authoritative review receipts
exist. No owner HMAC is fabricated.

## Reproduction without candidate execution

The repository and toolchain inputs are frozen in `development-task-repositories-v2.json`. Generate
one ledger with the matching local Git, Go, and native compiler binaries:

```bash
python3 benchmarks/agent-brain/confirmatory/task_eligibility_v2.py scan \
  --repo /path/to/entire-brain \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --repository-key entire-brain \
  --schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json \
  --git-binary /path/to/pinned/git \
  --go-binary /path/to/pinned/go \
  --native-binary /path/to/pinned/clang \
  --native-cxx-binary /path/to/pinned/clang++ \
  --output /tmp/development-task-eligibility-entire-brain-v2.json
```

Validate the checked dependency bytes and rebuild every structural binding from Git objects:

```bash
python3 benchmarks/agent-brain/confirmatory/task_eligibility_v2.py check \
  benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --repo /path/to/entire-brain \
  --git-binary /path/to/pinned/git \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json
```

Build the registry from the existing CLI ledger and all three v2 ledgers:

```bash
python3 benchmarks/agent-brain/confirmatory/task_overlap_registry.py build \
  --git-binary /path/to/pinned/git \
  --cli-repo /path/to/entire-cli \
  --cli-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json \
  --output /tmp/development-task-overlap-registry-v1.json
```

Validate every registry input byte, implementation/schema dependency, and the CLI-derived stable
patch IDs by rebuilding the complete registry:

```bash
python3 benchmarks/agent-brain/confirmatory/task_overlap_registry.py check \
  benchmarks/agent-brain/confirmatory/development-task-overlap-registry-v1.json \
  --git-binary /path/to/pinned/git \
  --cli-repo /path/to/entire-cli \
  --cli-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --v2-ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json
```

Run the structural tests from the confirmatory directory:

```bash
python3 -m unittest test_task_eligibility_v2.py test_task_overlap_registry.py
```

The separate v2 negative-control plan/check slice now freezes exact inputs and resource/isolation
requirements, but the executor, classifier, approval trust mechanism, cache seed, and private-log
writer remain absent. “Structurally ready” means only that a later isolated runner can reconstruct
an exact module/package target and reversal set; it does not mean a baseline or reversal has run or
passed. See `MULTI-REPOSITORY-NEGATIVE-CONTROL-RUN-PLAN-V2.md`.
