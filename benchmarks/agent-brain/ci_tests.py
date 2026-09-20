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
    "confirmatory/test_restricted_replay_attestation.py": (
        "requires a temporary directory that is not group- or world-writable; "
        "the default tempdir is /tmp (1777) on Linux and a private "
        "/var/folders/... on macOS"
    ),
}

ROOT = pathlib.Path(__file__).resolve().parent

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
    for path in modules():
        rel = path.relative_to(ROOT).as_posix()
        if rel in DARWIN_ONLY and sys.platform != "darwin":
            print(f"skipped (host)  {rel}", flush=True)
            continue
        ok, output = run(path)
        print(f"{'ok              ' if ok else 'FAIL            '}{rel}", flush=True)
        if not ok:
            sys.stdout.write(output)
            failed.append(rel)

    for rel in failed:
        print(f"failing test module: {rel}", file=sys.stderr)
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
