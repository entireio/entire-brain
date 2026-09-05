# Changelog

All notable changes to `entire-brain` are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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

### Added

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
