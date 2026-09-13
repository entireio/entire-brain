# Changelog

All notable changes to `entire-brain` are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- Every printed repair command resolves in the dispatch mode that printed it.
  The memory health and error surfaces named `entire memory repair` and
  `entire memory migrate` — commands NEITHER mode can run, because `memory` is a
  subcommand of this binary and not a verb of the host CLI. They now carry the
  spelling `setupCommandPrefix` resolves, the same as every other printer.
- `doctor` and `config` are visible in `--help` again. Both were hidden in June
  2026 as plugin plumbing; since then `doctor` became the documented
  verification step with an exit-code contract (`--fail-on`) that CI is meant to
  gate on, `scripts/install.sh` runs both and tells the reader to re-run
  `doctor`, and a user who did that and then grepped `--help` found nothing.

### Fixed

- **The v0.3.0 fact-integrity cross-check compared counts, so a corruption that
  preserved the count was invisible.** Overwriting one line of `facts.ndjson`
  with a copy of another leaves the store parseable, the line count unchanged
  and the manifest's number correct — and the fact whose line was taken is gone.
  `get fact:<id>` answered "not found" while `doctor` said `facts: ok`, `status`
  raised no warning, and `search` went back to volunteering that "the answer may
  genuinely not be in the brain" — the exact false statement #242 was written to
  eliminate, reached by a route the count could not see. The check now also
  compares IDENTITY: within a branch an id names exactly one fact (every writer
  keys by id before it writes), so a branch that yields the same id twice was
  not written by this program and the repeat is standing where another fact used
  to be. The four existing modes keep their own words; this is a fifth,
  `duplicated`, because a substitution is not a shortfall — nothing is short,
  something was replaced. The comparison is scoped to a branch and never to the
  brain: a fact id is derived from text and paths only, so `facts promote`
  carrying a fact between branches stores one id twice on purpose, and a
  brain-wide id set would have reported every promote as data loss. A duplicate
  that was appended rather than written over used to land in `stale`, whose
  advice — "the manifest is behind the store — run `entire brain refresh`" —
  would have re-declared the collided store and silenced the brain about it; it
  now lands in `duplicated`, which names the re-distill.
- **A missing `git` was reported as a missing repository.** `git rev-parse
  --show-toplevel` fails for both reasons and the four repo-ness gates (`setup`,
  `path`, `refresh`, `workspace add`) read every failure as the second. On a
  machine with no git, a perfectly good repository was reported as "not a git
  repository" and the remedy offered was `git init` — which needs the program
  that is missing. The two are now told apart by a `git --version` probe, which
  answers on git's presence alone and so cannot be confounded by the state of
  the directory, and each gets its own message and its own followable remedy.
- `docs/getting-started.md` advertised two install methods that do not exist: a
  "prebuilt release archive" (the tagged releases carry no binaries and no
  source archive) and `go install` (which the same file later admits is
  unavailable). It now describes only the source install that works, and says
  plainly that the Go toolchain is therefore not optional.
- `scripts/install.sh`'s Entire CLI prerequisite said "install the Entire CLI",
  which is the restatement of the problem, not a fix. It now gives the same
  grade of answer the Go check already gave: the Homebrew cask, the install
  script, and the repository.
- `CHANGELOG.md` had exactly one heading, `## [Unreleased]`, covering work that
  shipped in `v0.3.0` — no version section had ever been cut at any tag. The
  `0.3.0` and `0.2.0` sections below are that history, recorded from the
  published release notes.

## [0.3.0] - 2026-09-13

219 commits since `v0.2.0`, 62 merged PRs. Cut after an adversarial campaign of
roughly 400 hostile invocations across nine attack families: zero panics, zero
stack traces, zero escapes outside the store, no lost updates under 12
concurrent writers, and `bundle import` rejecting traversal, absolute paths,
symlinks, hardlinks, device nodes and a 600 MB decompression bomb. What the
campaign found instead was commands that reported success while something was
wrong, which is most of what follows.

### Changed

- The documented journey now ends where a user actually ends up: a brain
  onboarded in *their* repository. It used to stop at the install. `README.md`
  said "That is the whole install" and the next thing a reader was given was
  `entire brain refresh --agent none`; `entire brain setup` appeared only as an
  aside two sections later, and `docs/getting-started.md` — a guide whose whole
  job is the journey — never mentioned `setup` at all in 762 lines. The install
  is machine-scoped and gives no repository a brain, so a reader finished it
  with working plugins and nothing to show for them.
  - `README.md` opens `## Install` with the two commands the whole journey is
    (install once per machine, `entire brain setup` once per repository), renames
    step 1 to "Install the plugins on this machine", and promotes `setup` to
    **step 3**, the next thing a reader does. The old step 3 (`refresh`) is now
    step 3b, described as the manual path rather than the default one.
  - A new **step 3a** documents what `setup` genuinely requires — a local path
    that is a git repository, checked before any side effect — versus what it
    degrades around: `entire enable`, captured sessions, the `entire graph`
    provider, a service manager, an agent CLI. It states the real order
    (`entire enable` then `setup`) and that the order is a quality decision, not
    a correctness one, and it names what a repository new to Entire actually
    produces: an all-green run, with `Brain ready (degraded)` explained as
    exit-0 information and each remaining cause — an uncommitted worktree, a
    missing `entire graph` provider, an unreadable checkpoint inventory — given
    with its fix.
  - Step 3 says plainly that the watcher is installed as a **persistent
    launchd/systemd service, by default, with no prompt**, names the
    `RunAtLoad`/`KeepAlive` and `Restart=always` that make it survive reboot,
    and gives `--no-daemon` and `entire brain setup --uninstall-daemon`.
  - `docs/getting-started.md` gains "Onboard a repository: `entire brain setup`"
    in place of "First run", with the phase table, the spend labels, the
    prerequisite/ordering section, and `refresh` kept as the by-hand path.
  - `docs/operations.md` gains an "After the install: onboard a repository"
    section, because the installer it documents is not the last command a user
    runs.
  - `scripts/install.sh`'s closing banner said `Next: build a brain: entire
    brain refresh --agent none` — the same truncation in the terminal. It now
    says the machine is set up, no repository has a brain yet, and gives
    `cd /path/to/your/repo && entire brain setup` with its token-spend caveat.
- Every command `setup` and `status` print back is now spelled the way the
  reader reached this binary. The brain ships as a kubectl-style external
  command of the Entire CLI, so a reader who types `entire brain setup` never
  types the binary's own name — and on a managed install they cannot: the
  binary lives in `<xdg_data>/entire/plugins/bin`, a directory the Entire CLI
  prepends to `PATH` inside its own process and nowhere else. Everything was
  printed as `entire-brain …` regardless, so the closing **Next** block handed a
  plugin reader three commands their shell cannot resolve. The signal is the
  host's: its plugin dispatcher sets `ENTIRE_CLI_VERSION` unconditionally on
  every plugin launch (`os.Args[0]` is no signal — the dispatcher execs the
  resolved path). Absent it the standalone name stays, which is the honest
  answer for a reader who may not have the host CLI at all. The prefix is
  resolved once per run and threaded to every printer, so the summary's Next
  block, every skip and failure line, all three hints, the `status` verdict and
  `setup --help` move together.
- `setup` says what it will register with the machine **before** it does it. A
  run that will install the watcher now opens, ahead of any work, with one line
  naming the unit and its path, saying plainly that it is a persistent service
  that starts again at every login, and giving both `--no-daemon` and
  `entire brain setup --uninstall-daemon` on the same screen. Previously the
  only mention arrived mid-summary, after a persistent launchd agent had been
  registered in the user's real `~/Library/LaunchAgents` and started, and
  `--uninstall-daemon` appeared only in `setup --help`. The default is
  deliberately unchanged — whether the watcher installs unasked is a product
  decision — but the consequence is now stated first. A re-run that finds the
  service already there says it *keeps* it instead of claiming a second
  install; `--no-daemon` announces nothing it will not do; a platform with no
  service manager has nothing to announce.
- Installing from git is one command that works with nothing arranged in
  advance. `scripts/install.sh` no longer requires `entire-graph` to be a
  sibling checkout with that exact name — the documented constraint that made
  the install path fail for anyone who cloned one repository, cloned under a
  different directory name, or ran from a worktree. It now resolves the provider
  by three routes and prints the one it took: `$ENTIRE_GRAPH_DIR` when set (an
  override that is not an `entire-graph` checkout is a hard error, not a silent
  fallback); any checkout already on the machine — siblings of this checkout and
  of the main worktree when this is a linked one, plus the usual source
  directories under `$HOME`, accepted only when `cmd/entire-graph` is really
  there rather than only the directory name; otherwise a shallow clone of the
  public `entireio/entire-graph` into
  `${XDG_CACHE_HOME:-~/.cache}/entire-brain/entire-graph`, reused and refreshed
  by later runs. When no route can work it fails naming all three and how to
  satisfy each. `ENTIRE_INSTALL_OFFLINE=1` disables the clone for an air-gapped
  machine, and `ENTIRE_GRAPH_REPO`/`ENTIRE_GRAPH_CACHE` redirect it.
- `scripts/install.sh` checks the prerequisites before it builds anything and
  reports all the missing ones at once with the fix for each — `git`, the
  `entire` CLI, a Go toolchain at least as new as the `go` directive in `go.mod`
  (tolerating an older `go` when `GOTOOLCHAIN` may fetch the newer one), and a C
  compiler for `entire-graph`'s tree-sitter cgo bindings. Previously the first
  two were separate late failures and a missing C compiler surfaced minutes into
  a build.

### Fixed

- First-run onboarding. Every item below made a brand-new user's very first
  `entire-brain setup` report a failure it did not have to:
  - `session export failed (... parse checkpoint list json: invalid character
    'U' after top-level value)`. The `U` is the host `entire` CLI's
    `Update available!` banner, which released versions print on the SAME stream
    as `--json` output. The whole-buffer `json.Unmarshal` is now a decode of the
    first JSON value that tolerates trailing output. Because the host caches its
    version check for 24h, this failed on the first-ever run and silently healed
    on the second.
  - A missing `entire-graph` plugin reported only
    `provider_doctor_failed: semantic provider no-egress status is not
    verified`. The provider doctor's actual error is now surfaced, shortened to
    one line, and names the install step when the plugin is simply absent.
  - `entire-brain doctor` — the command `setup` tells you to run when a
    component fails — printed nothing about the repository unless
    `ENTIRE_REPO_ROOT` was set, which only the host sets. It now infers the
    repository from the working directory, and says so when it cannot.
  - A pristine clone graded itself `freshness: degraded` because the host CLI's
    own `.entire/logs/entire.log` made the live-state check see a dirty tree.
    That check now applies the same ignore policy every other reader already
    applied.
  - `setup` in a directory that is not a git repository failed four components
    with a raw `fatal: not a git repository`, registered a workspace, installed
    the watcher, and exited 0. It now refuses up front, before any side effect.
  - `--uninstall-daemon` appeared only in `setup --help`. `setup` and `status`
    now name it, and `setup` says plainly that a persistent service was
    installed and where its unit lives.
  - The watcher logged `budget=0` — the value a setup-installed watcher always
    has — while `--max-sessions`, the cap that actually binds it, was not logged
    at all.
  - `status` hard-coded five component names, so a component `setup` reported as
    FAILED (`entities`, say) was missing from `status` entirely.
  - `x semantic index: repo key mismatch` on **every** repository created with
    `git init` — and, less visibly, on every non-GitHub origin. `entire-graph`
    recognises `github.com` remotes and otherwise falls back to
    `local/<basename>`; the brain recognises many hosts by slug and, with no
    usable remote, derives `local/<basename>-<sha256(abs path)[:12]>`. So the
    two tools spelled the same repository differently, and the hint blamed the
    reader's repository ("this repo has no git remote; add one … or upgrade the
    entire CLI") for a disagreement between two of ours — advice that fired on
    repositories which already had the remote it asked for, and that no upgrade
    of either tool would have satisfied. The brain now recognises the provider's
    own spelling of the repository it just asked the provider to index and
    stores the snapshot under its own key. **A fresh `git init` repository
    reaches an all-green `setup` with no user action.** This is not a weaker
    crossover guard: the snapshot's commit and tree are still validated against
    this repository's HEAD, and a snapshot naming a genuinely different
    repository is still refused — with a hint that now says to rebuild the index
    (`refresh index --force`) rather than to change the repository. Bundle
    import and stored-snapshot read-back still compare keys exactly.
  - A dirty worktree had no remedy. `entire enable` writes `.entire/settings.json`
    and the agent's `.claude/settings.json` and does not commit them, so the very
    next `setup` — the documented next step — found a dirty worktree and refused
    to seed or index it, naming `--worktree`, a flag `setup` does not accept.
    Both refusals now share one error code and `setup` attaches the remedy:
    commit or stash, and why the tree is dirty when the reader edited nothing.
  - A repository with no sessions yet read as a fault: "Entire returned no
    readable checkpoint IDs from an incomplete persistent-store inventory".
    True, and useless. The detail survives for anyone debugging a real fault; the
    hint now says what the state means and points at `entire checkpoint list`.
    A routed discovery that genuinely failed keeps its own detail.
  - `setup` did not build the durable history projection and then told the user
    to run `brief`, so the first `brief` after a first-ever `setup` dropped its
    transcript half and printed "history index missing; run `entire brain
    refresh`" — setup's own recommended next command reporting a gap in setup's
    own output. `setup` and a watch tick now say which behaviour each wants, and
    `refresh` skips the projection when its target directory does not exist
    (the state a run where every deterministic source failed used to leave,
    surfacing as a raw `lstat <brainDir>: no such file or directory`).

### Added

- `scripts/bootstrap.sh`: the one command for a machine with nothing on it. It
  uses the checkout it was run from when there is one, otherwise clones
  `entire-brain` (falling back to `gh repo clone` for the private-repo
  credentials), then execs `scripts/install.sh`. There is deliberately no
  `curl | sh` form: `entireio/entire-brain` is a private repository, so
  `raw.githubusercontent.com` answers 404 without a token; the install is a
  source build, so the clone has to happen either way and a pipe removes no
  step; and cloning first puts the script that builds and registers the plugins
  on disk, at a reviewable commit, before any of it runs.
- `ENTIRE_BRAIN_DAEMON_NO_REGISTER`: writes the launchd/systemd unit file but
  never calls `launchctl`/`systemctl`. `ENTIRE_BRAIN_DAEMON_DIR` redirects the
  unit FILE only — `launchctl load -w` acts on the caller's live session
  whatever directory the file came from — so a CI job or smoke test that set
  only the directory still registered a real KeepAlive agent.
- `scripts/trial-setup.sh`: one command that runs the whole first-run experience
  against a throwaway repository in a fully redirected sandbox, exercises the
  failure modes, and proves on teardown that nothing leaked into the machine's
  launchd/systemd session.
- `entire brain setup`: one command that builds the deterministic core (free,
  blocking, seconds), starts a detached fact backfill over past sessions
  (newest first, cheap model), and installs a single machine-wide watcher
  service (launchd agent on macOS, systemd user unit on Linux) so the brain
  stays fresh without anyone remembering to run anything. Re-running it is
  idempotent: one daemon, one workspace membership, one backfill.
- `entire brain status` gained an **Onboarding** section: fact-backfill
  progress (`facts: N/M sessions distilled`), whether a backfill process is
  actually alive, the watcher's installed/current/running state, the last
  watcher tick, and which instant-phase components exist.
- `entire brain distill` gained `--newest-first` and `--max-sessions`, so a
  bounded pass can prioritise the sessions whose facts are most likely to
  matter.
- The entity index refresh is one of the components the instant phase reports,
  so `setup` names it alongside sessions, seed, docs and the semantic index and
  `doctor` can print why it failed — instead of the reason going to a stderr
  line that a `--json` run discards.
- `setup` renders progress: one in-place line per phase carrying a spinner, a
  colour-ramped bar where a real count exists (checkpoint export, history
  index, semantic files) and a per-component tick row, phase-coded colours
  (instant, backfill, daemon, done, skipped, failed), and a closing summary
  block with per-phase timings. All of it is gated on the destination stream —
  a pipe, a CI log, a hook and `--json` get plain, byte-stable, ASCII lines
  with no escape sequences, `NO_COLOR` is honoured, and a terminal whose locale
  is not UTF-8 gets an ASCII glyph set.
- `entire brain status --verbose` prints the full report (coverage histograms,
  freshness axes, blind spots, live state). The default report is now short.
- `entire brain doctor` lists the semantic blind spots in full, grouped by
  reason, which is where the short `status` now points for the detail.

### Fixed

- `setup` printed every phase line twice — once bare from the progress update
  and once with a " done" suffix from the same task finishing. A task now emits
  exactly one line per distinct phase, and the final label is printed once.
- `setup` and the refresh it calls painted competing in-place lines on the same
  terminal row from two different streams, so neither erased the other and a
  long line wrapped into a visible duplicate. In-place rendering is now owned
  by one process-wide line: only the innermost task paints, and every live line
  is truncated to the terminal width so it can never wrap.
- The semantic phase's progress flooded and flickered: its label changes on
  every few hundred records and neither the repaint nor the plain-mode line was
  rate limited. Repaints are capped at 10/s and plain-mode labels that differ
  only in their counters are coalesced.
- Stray replacement glyphs ("□") on terminals whose locale is not UTF-8: the
  spinner, marks, bars, em dashes and separators are gated on the locale, with
  an ASCII fallback.
- `entire brain status` printed the same skipped files twice — once as semantic
  partial failures and again as blind spots — one line each, which on a repo
  that vendors JSON corpora meant 88 lines of the same fact. Repeated warnings
  are collapsed to one line per reason, obvious data files are grouped as "N
  data files skipped", and no record is listed twice across sections.

### Changed — token spend

**Read this before upgrading: two code paths that spent nothing now spend.**

`watch --distill` and the session-end hook's distill both constructed their
options in Go, and both left `concurrency` at zero. `runDistill` rejects that
before making a single agent call, so every gated distill either of them ever
attempted failed instantly and silently. Setting the field fixed the bug — and
in doing so turned two dead paths into live, recurring token spend. Nothing
about the gating changed; what changed is that the gates are now reached.

Concretely, after this release:

- an installed watcher calls an agent at most once per `--distill-every`
  (default 24h) **per repo**, on `--model`/`--effort`;
- a wired `SessionEnd` hook distills the session that just ended;
- `setup` additionally runs one detached backfill pass at install time.

What bounds it: `--backfill-budget` sessions per background pass;
`--distill-every` plus each repo's persisted watch cursor (which survives
restarts); the distill cache, so no session is ever distilled twice by any entry
point; and a cross-process distill lock, so two passes over one brain can never
both spend. `entire brain setup --no-backfill --no-daemon` spends nothing at
all, `entire brain setup --uninstall-daemon` removes the watcher, and
`entire brain status` shows what has been done so far.

### Fixed

- **The daemon distilled once, ever.** `setup` installed the watcher with
  `--budget 1`, but `--budget` counts gated agent runs for the life of the
  process and never resets, and the installed unit is a KeepAlive service that
  never exits. One machine therefore distilled exactly once, forever, across
  every repo — while reporting itself healthy. The daemon now relies on
  `--distill-every` plus the per-repo watch cursor, the durable guard that
  bounds spend per repo per window without latching off.
- **`setup` could double-distill its own sessions.** It spawned the detached
  backfill and installed a watcher whose first tick distilled immediately; the
  brain write lock only wraps the flush, so both reached the agent first. A
  try-only distill lock, held for the whole pass, now lets exactly one spend and
  makes the second skip cleanly.
- **Re-running `setup` mid-backfill started a second backfill** over the same
  pending sessions. It now detects the live pass, reports its progress, and
  skips.
- **A bare `setup` distilled the entire corpus.** `--backfill-budget` now
  defaults to 25 sessions per pass (newest first) and reports what the cap did
  and how to lift it; `--backfill-budget 0` is the explicit "whole corpus".
- **Setting up a second repo restarted the daemon.** The machine-wide unit
  embedded the calling repo's log path, so every new repo rendered different
  bytes, which `status` read as drift and "repaired" by unloading and reloading
  the running watcher. The daemon log now lives at a machine-level path and the
  unit is byte-identical regardless of which repo ran setup.
- **Concurrent `setup` runs lost workspace registrations.** The workspace
  manifest was read-modify-written without a lock, so two repos registering at
  once left only one member. All manifest mutations now go through one locked
  path.
- **`status` claimed the session-end hook was wired when it belonged to another
  tool.** Any non-empty `SessionEnd` command satisfied the check. It now
  verifies the command invokes the Entire CLI's hook, reports which command
  matched, and separately reports whether `entire` is on `PATH` — a hook naming
  a binary this machine does not have is worse than no hook.
- **A re-run silently reverted daemon customisations.** "Did the user pass this
  flag?" was answered by comparing values against defaults, which cannot
  distinguish an explicit default from silence. It now reads
  `Flags().Changed`, and `--interval`, `--distill-every`, `--model` and
  `--effort` are remembered per repo alongside the workspace and daemon name.
- **Renaming the daemon left the old one running** under a label no later
  `status` or `--uninstall-daemon` could name. The previous unit is now
  unloaded before the new one is written.
- **Two daemon names could collide on one launchd label.** The label dropped an
  `entire-` prefix, mapping `entire-watch` and `watch` onto the same job and
  plist. Labels now carry a short digest of the full name.
- Systemd units escape `%` in `WorkingDirectory=`, `StandardOutput=`,
  `StandardError=` and `Environment=` (an unescaped `%` is a specifier, so
  `%h` silently expanded to the home directory), and any value containing a
  newline is rejected rather than rendered.

### Security

- The daemon's unit path now honours `ENTIRE_BRAIN_DAEMON_DIR`, so a sandboxed
  or test run cannot install a live launchd agent or systemd unit into a real
  user's directories even when it can see the real `$HOME`.

### Security — 0.3.0 release notes

- **A repository's git hooks were a fifth execution vector, undefended.**
  `git_harden.go` states the model in its own header — "A repository is data,
  not a trust boundary" — and defended four vectors. Git hooks need no config
  keys at all, just an executable file, so they are strictly easier to arm than
  the four that were covered; arbitrary code ran as the user while the tool
  printed `+ healthy`. The security suite could never have caught it because its
  own fixture disarmed hooks before every test. Suppression is
  `core.hooksPath=/dev/null/entire-brain-hooks-disabled`: absolute, because git
  resolves a relative hooksPath against the repository's own worktree, and
  `/dev/null/…` rather than a merely-nonexistent directory, because `/dev/null`
  is a character device so the path is ENOTDIR by construction. Executions per
  invocation went 6→0 (`refresh --agent none`), 3→0 (`refresh --worktree
  --agent none`) and 1→0 (`status`, `status --verbose`, `overview`). (#241)
- **The MCP surface advertised a limit it could not answer.** Every size
  argument declares `"maximum": 10000`; against a real index (48,275 symbols,
  155,298 relations) eight tools hard-failed at their own stated ceiling and
  five more returned unflagged multi-megabyte documents. `brain_impact` built
  33.8 MB and died; `brain_brief` spent 14.3s to return nothing. Responses are
  now bounded to a 128 KiB budget with an explicit truncation marker, using one
  shared row cap so an impact answer cannot come back containing no impact.
  Every default-limit call is byte-identical. (#240)
- Earlier in the cycle: a git-config RCE path (#210), an HTTPS floor (#211), and
  MCP framing and recoverability fixes (#214, #215, #234).

### Fixed — durable memory

- **Facts could vanish while three health surfaces reported them present.**
  Replacing one of five lines in `facts.ndjson` with valid non-fact JSON had
  `status` say 5, `doctor` say 5, and `verify` — whose entire job is fact
  integrity — see 4 and report `0 orphaned`, exit 0. Nothing cross-checked the
  two numbers. With all five replaced, the retrieval note volunteered a false
  explanation: "the answer may genuinely not be in the brain." The cross-check
  now covers `status`, `doctor`, `verify`, `facts map` and the retrieval note,
  keeping four failure modes in their own words — missing, unreadable, lossy,
  stale — and naming which branch's store will not parse. In `unreadable` mode
  it reports "store unreadable", never "0 readable": zero was a measurement
  nobody took. (#242, #239)
- **`privacy purge` deleted facts without rebuilding `sources.facts`**, so a
  sanctioned purge left the manifest overstating — on disk indistinguishable
  from the corruption above. Found while fixing it; without this, the new check
  would have reported every legitimate purge as data loss.

### Fixed — diagnostics

- **`doctor` printed `error` and exited 0**, so it was unusable in CI or an
  agent loop. It now takes `--fail-on {error|warn|none}`, defaulting to `error`,
  with findings scoped so a missing host CLI is reported but does not fail a
  gate about the brain's own store. It also stopped crying wolf: six warnings on
  a pristine brain became two. Three of those could never pass by construction,
  including `memory_reconciliation`, which printed with no reason at all because
  a named string type was read with a bare `.(string)` assertion that failed for
  every value but one. (#239)
- Other surfaces that reported success while wrong: `status` marked a shredded
  semantic index as built; `workspace add /etc/passwd` succeeded; `get` and
  `multi-get` exited 0 on a miss while sibling `show` exited 1; `{}` and
  `schema_version: 0` were accepted as "manifest current"; unparseable MCP lines
  were silently dropped depending on their first character; and degenerate repos
  leaked raw git text including a hardening flag the user never typed.
  (#243, #236, #237, #238)

### Changed — release engineering

- All eight `release-evidence` checks pass against this tag, with the Radar
  tool-contract lane re-measured, not re-pinned (#245). The gate runs at release
  — a `v*` tag or `workflow_dispatch` — rather than on every push to main
  (#202), and the README's examples execute in CI (#203).

### Known — 0.3.0

- Both repositories are private and there is no published binary, so the
  documented `git clone` install cannot work for anyone outside the org. That is
  a visibility decision, not a code fix.
- A repository reached through a symlinked path (macOS `/tmp` → `/private/tmp`)
  can produce a `repo_identity_conflict` between the canonical and pre-upgrade
  alias keys. It fails closed and names both keys, both paths and the remedy;
  normal paths are unaffected.
- The git-hooks fix is reasoned rather than executed on Windows — the ENOTDIR
  guarantee is POSIX-only and degrades there to ordinary non-existence — and its
  tests skip rather than fail on a coarse-timestamp filesystem. The MCP
  truncation bounds the response, not the work: `brain_impact` still builds its
  33 MB before discarding it.

## [0.2.0] - 2026-09-12

First release since `v0.1.0` (2026-07-02), 514 commits later. Cut on the first
`main` that satisfies the release gates end to end: CI green (28/28 on this
tag), every `release-evidence` lane passing, and the four known path/identity
escapes closed. Each defect below was verified by reproducing it on unpatched
`main` first, then proving the fix closes it.

### Security

- **Path guards accepted a bare `..`** — `filepath.Clean("..")` is `".."`,
  neither `"."` nor `"../"`-prefixed, so it fell through both shared guards.
  `brain_delete_project` passed the repo key straight to `os.RemoveAll`: the
  whole plugin data root, every project's brain included. (#204)
- **Skill writes escaped the skills directory**, two ways. `path.Join`
  *collapses* `".."` rather than rejecting it, and the skill name is parsed from
  agent frontmatter — text an untrusted repo reaches. Separately the overwrite
  guard used `os.Stat`, which reports a dangling symlink as absent, so the write
  then created the file the link pointed at. A `SKILL.md` is an
  agent-instruction file. (#96)
- **Repo-key guards were missing** at several store-path sites, and
  `repoKeyFromRemote` could mint a reserved key (`workspaces/...`). (#153)
- **The repo id went unescaped into the hosted brain MCP request path.** That
  request carries a bearer token, so a `/`, `?` or `#` in the id sent the token
  to an endpoint nobody asked for. `publish.go` had escaped its own id since it
  was written; this was the one site that did not. (#205)

### Changed

- `release-evidence` runs at release — a `v*` tag or `workflow_dispatch` —
  rather than on every push to `main`. It pins six source files by content hash,
  so any edit marked the evidence stale; the run that had main red since
  2026-09-07 was tripped by an import-grouping commit, and the lanes had been
  re-recorded eight times since June for the same reason. Nothing was weakened
  (the audits still run with `--fail-on-flags`), and a hole closed: the workflow
  previously had no tag trigger at all, so a release could be published without
  its proof ever being checked. This tag is the first where it ran. (#202)
- Both distill scheduler lanes and the Radar tool-contract lane were re-measured,
  not re-pinned, against this tree, and came back identical to the retained
  workloads: current-repo 206 sessions / 246 chunks, 1.25x gate, 2.9851x; and
  large-repo 2,375 sessions / 2,435 chunks, 1.50x gate, 1.7931x. (#206, #207)
- The README's own examples now execute in CI. Eleven documented commands had no
  test behind them; all eleven are covered, so nothing is announced that is not
  tested. (#203)

### Known — 0.2.0

- The README correction landing in #208 — `refresh --worktree` does not update
  the semantic index, and the "no released product version" line — merged to
  `main` after this tag was cut, so it ships in the next release rather than
  this one.
