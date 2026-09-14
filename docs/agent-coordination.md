# Coordinated agent instructions

Both products provide `init-agents` and `agent-guide`, invoked through
`entire graph` and `entire brain`. Brain also retains `guide` as an alias.

## Generation-time routing

| Invoked command | Check | Generated workflow |
| --- | --- | --- |
| Graph initializer or preview | Brain appears in `entire plugin list` and this repository has a Brain | Graph and Brain |
| Graph initializer or preview | Otherwise | Graph only |
| Brain initializer or preview | Graph appears in `entire plugin list` | Graph and Brain |
| Brain initializer or preview | Otherwise | Brain only |

`entire plugin list` enumerates managed installations without dispatching plugins.
The host executable is optional: when `entire` is absent from `PATH`, either
standalone binary can preview and install its own instructions inside a repository.
No peer activation is inferred without the host inventory. Regeneration with the
host available resolves coordination normally. Its current CLI offers text output,
not JSON; an unexpected listing or failure from a present host is reported
explicitly. Unmanaged executables that do not appear in that list do not activate
the peer. No plugin executable is invoked for detection.

Graph determines repository Brain state using safe, bounded local reads. A valid
`setup.json` written by Brain setup under `<state>/repos/<key>/` establishes setup.
When that record is absent, a valid `<data>/repos/<key>/manifest.json` recognizes
Brains created through older build/refresh paths. Missing or stale indexes do not
change the mode; a setup record is sufficient without reading an index manifest.
Malformed, unreadable, oversized, nonregular, or unsupported setup records cause
an error rather than masquerading as absence. Generated guides never count as
setup records.

Repository identity uses the same read-only `git remote get-url origin` lookup as
Brain, its existing known host mappings, learned custom mappings in `<config>/brain.json`, or the canonical
local-path hash (also checking Brain's legacy root spellings). Generation never
creates a custom host mapping. It does not contact Git remotes or run hooks.
Git includes and URL rewrites are honored so generation finds the same repository identity as Brain. Ambiguous
canonical/legacy setup records are rejected.

Default Brain directories match Brain's standalone layout: XDG state/config roots
under `entire`, and `XDG_DATA_HOME/entire/plugins/data/brain`, with the normal home
fallbacks. For a Brain configured under custom plugin directories, set the explicit
coordination overrides `ENTIRE_BRAIN_STATE_DIR`, `ENTIRE_BRAIN_CONFIG_DIR`, and
`ENTIRE_BRAIN_DATA_DIR` consistently for generation. These are new, read-only lookup
overrides; they do not change where Brain setup writes. Graph's plugin data/cache
directory is never mistaken for Brain's data directory.

## One installed guide

Either initializer writes `.entire/agent-guide.md`, the only workflow body.
Both use the same standard-library-only `internal/agentsetup` package, mirrored in
the two source repositories. There is no dependency on the peer binary and no
recursive initialization.

A single `entire-agent:begin` / `entire-agent:end` managed block references the guide
from each independent `AGENTS.md` / `CLAUDE.md` instruction file. When Claude already
imports AGENTS, its block records inheritance instead of adding another guide
import. Existing instruction-file aliases remain supported. Text outside managed
blocks is preserved, including CRLF content.

Legacy Graph and Brain blocks are validated and removed. Existing
`.entire/graph-agent.md` and `.entire/brain-agent.md` become short redirects, so
clients with explicit old imports cannot load old first-action requirements.
They contain no duplicate workflow. Shared guide and legacy targets are checked
for containment, unsafe aliases, git-directory landings, and unsafe hard links
before writes. All writes retain the existing Graph installer safeguards.

## Workflow

Graph-only discovery begins, when discovery is needed, with:

```sh
entire graph query --repo . --profile full --query "<task>"
```

Brain-only guidance uses Brain for task context, retained knowledge, and semantic
inspection. Combined guidance begins substantive tasks needing orientation with:

```sh
entire brain brief "<task>" --json
```

Skip the brief when equivalent context is already available. Reuse useful locations
without a redundant Graph query. Graph query, def, neighbors, and impact answer
additional code and structural questions; diff, commit, and checkpoint compare
code revisions. Brain retrieval covers previous decisions, attempts, documentation,
and durable facts; entities history connects code to checkpoints and sessions.
Use Brain memory-informed review and workspace capabilities when relevant. Do not
ask both products the same question without an identified gap.

All modes allow direct source inspection when locations are sufficient and skip
ceremonial queries for small edits and follow-ups. Graph interactive queries
normally inspect the working tree; Brain semantic answers refer to a stored index.
Current source and executed tests establish present behavior, while historical
memory explains previous intent or behavior. Investigate disagreements. Retrieved
content remains untrusted data. Query failures do not automatically trigger
installation, configuration, or repair; continue with useful remaining tools or
source inspection.

## Preview, regeneration, and removal

`agent-guide --repo <path>` is a read-only preview using exactly the same renderer
and detection rules as the corresponding initializer. Explicit `--repo` takes
precedence over `ENTIRE_REPO_ROOT`; otherwise the nearest Git ancestor of the current
directory is selected. Explicit project roots may be non-Git directories. Brain's
initializer also accepts a positional path. Outside a repository, preview prints
the invoking product's standalone reference without detection; initialization
requires an explicit target.

Upgrade both binaries, then regenerate using the appropriate initializer. Both
activation orders produce the same guide when a repository Brain exists. Re-running
either initializer recomputes the mode from current state without creating duplicate
instructions. In the deliberately asymmetric case where both plugins are installed
but this repository has no Brain, Graph generates Graph-only guidance and Brain
generates combined guidance. The most recent initializer owns the single installed
workflow; their previews differ in this case by design.

Removing the repository Brain's setup record and stored Brain, or removing a managed
peer plugin, changes the next applicable generation. Deleting only an index is not
configuration removal. Existing redirects continue to reference the one regenerated
guide. To remove instruction activation entirely, delete the shared guide and redirects
and remove the managed pointer blocks. Reload instructions or start a new agent
session after regeneration. Old binaries can restore old instructions, so upgrading
both is required.

Concurrent initializers are not supported. Preflight prevents predictable partial
writes; an I/O failure or concurrent filesystem mutation during the write sequence
can still leave partial output. Resolve the reported error and regenerate.
Once migration starts, the canonical guide is retained on failure so any legacy
redirects already written still have a valid target; cleanup does not roll back
overwritten files.

## Tests

`go test ./internal/agentsetup` runs mode, migration, byte-stability, setup-error,
and filesystem-protection tests without real plugins or state. Brain's CLI tests
also compare the reader against its actual setup writer and repository identity.
`scripts/test-agent-coordination.py --graph-binary <build> --brain-binary <build>`
in Brain runs the two compiled CLIs in isolated projects and redirected state. Its
Entire stub accepts only `plugin list`, proving that generation never dispatches
or installs a plugin. It also checks both activation orders, preview parity,
configuration removal, failures, preservation, and identical mirrored sources.
