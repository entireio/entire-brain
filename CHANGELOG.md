# Changelog

All notable changes to `entire-brain` are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

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
