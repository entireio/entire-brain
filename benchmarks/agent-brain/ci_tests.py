#!/usr/bin/env python3
"""Run every benchmark unit-test module in this tree, discovered rather than listed.

CI used to invoke a hand-maintained list of five scripts, two of them narrowed to a
single TestCase class. A `test_*.py` file that was not on that list ran nowhere. At
the time this was written that was 36 of the 39 test modules under this directory,
and 310 of the 313 tests in run_test.py: the whole benchmark gate executed ten tests
in ten seconds. Every fix PR in flight against the harness carried its evidence as a
NEW test file, so none of that evidence was ever executed by the gate that reported
those PRs green.

Discovery closes that class of hole: a new `test_*.py` is picked up with no CI edit,
which is the only arrangement in which "the tests passed" and "the new test passed"
are the same statement.

Each module runs in its own interpreter, deliberately. They are not importable as one
package: several load `run.py` or an analyzer by path under a fixed module name via
importlib, and two directories each contain a `test_power_analysis.py`, so a single
`unittest discover` process collides on module names and silently drops modules — the
same failure mode this script exists to remove.
"""

from __future__ import annotations

import pathlib
import subprocess
import sys

# Modules that need a facility only a macOS host has. Each was passing on a
# developer's Mac and failing on the Linux runner the first time the gate
# actually executed it, which is the clearest possible statement that they had
# never run in CI. They are skipped where the facility is absent rather than
# quarantined, so a macOS developer still gets them.
DARWIN_ONLY: dict[str, str] = {
    "test_isolation_repairs.py": "needs /usr/bin/sandbox-exec for the read-isolation profile",
    "confirmatory/test_negative_control_darwin_capacity_v1.py": "shells /usr/bin/xcrun",
    "confirmatory/test_negative_control_owner_approval_v1.py": (
        "pins the sha256 of the host ssh-keygen, and the recorded hash is the "
        "macOS system binary"
    ),
    "confirmatory/test_restricted_replay_attestation.py": (
        "requires a temporary directory that is not group- or world-writable; "
        "the default tempdir is /tmp (1777) on Linux and a private "
        "/var/folders/... on macOS"
    ),
}

ROOT = pathlib.Path(__file__).resolve().parent

# Modules that do not pass on a clean checkout today. Each entry states why, so the
# exclusion is a visible debt rather than an invisible gap. A quarantined module that
# starts passing is also an error: that is how the list stays honest instead of
# outliving its reasons.
QUARANTINE: dict[str, str] = {
    "confirmatory/test_power_analysis.py": (
        "power-calibration-exploratory-v1.json names "
        "evidence/replay-lab-clean/panel-p01-clean-proof-claude-20260618T195058Z/records.ndjson, "
        "a panel that was replaced by the 20260816T084152Z panel and deleted. "
        "power_analysis.build_report() raises on the missing file, so 38 of its 23 "
        "tests error out. Repointing the calibration source is a change to a pre-registered "
        "statistical contract and is left to the owner of that contract."
    ),
    "confirmatory/test_check_protocol.py": (
        "the committed analyzer manifest disagrees with the analyzer sources it "
        "hashes ('analyzer files[3]: content hash mismatch'); re-recording the hashes "
        "is a change to the protocol attestation, not a test fix."
    ),
    "confirmatory/test_development_task_symptom_review.py": (
        "requires confirmatory/development-relevance-queries-v1.json, which is "
        "gitignored and generated locally, so it cannot run on a clean checkout. It "
        "should skip rather than fail; until it does, it cannot gate."
    ),
}


# Both spellings are in use: `test_*.py` for the module-per-concern files and
# `*_test.py` for run_test.py, which alone holds 313 tests of which CI ran 3.
PATTERNS = ("test_*.py", "*_test.py")


def modules() -> list[pathlib.Path]:
    found = {
        p
        for pattern in PATTERNS
        for p in ROOT.rglob(pattern)
        if "__pycache__" not in p.parts
    }
    return sorted(found)


def run(path: pathlib.Path) -> tuple[bool, str]:
    proc = subprocess.run(
        [sys.executable, str(path)],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    return proc.returncode == 0, proc.stdout + proc.stderr


def main() -> int:
    failed: list[str] = []
    unexpected_pass: list[str] = []
    for path in modules():
        rel = path.relative_to(ROOT).as_posix()
        if rel in DARWIN_ONLY and sys.platform != "darwin":
            print(f"skipped (host)  {rel}", flush=True)
            continue
        ok, output = run(path)
        if rel in QUARANTINE:
            # A quarantined module's failure output is expected and noisy; only the
            # surprise is worth printing.
            print(f"{'UNEXPECTED PASS ' if ok else 'quarantined     '}{rel}", flush=True)
            if ok:
                unexpected_pass.append(rel)
            continue
        print(f"{'ok              ' if ok else 'FAIL            '}{rel}", flush=True)
        if not ok:
            sys.stdout.write(output)
            failed.append(rel)

    for rel in sorted(set(QUARANTINE) - {p.relative_to(ROOT).as_posix() for p in modules()}):
        print(f"stale quarantine entry, module is gone: {rel}", file=sys.stderr)
        failed.append(rel)

    for rel in failed:
        print(f"failing test module: {rel}", file=sys.stderr)
    for rel in unexpected_pass:
        print(
            f"quarantined module now passes, remove it from QUARANTINE: {rel}",
            file=sys.stderr,
        )
    return 1 if failed or unexpected_pass else 0


if __name__ == "__main__":
    raise SystemExit(main())
