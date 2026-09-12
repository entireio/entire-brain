"""The B-session read-isolation profile must actually be BUILT, not swallowed.

run_b.py's module docstring promises that, on macOS, every B session runs under
run.py's deny-first sandbox profile "re-allowing only this cell's worktree, so
sibling results, task JSONs, and session-A artifacts are not reachable from B",
and it justifies the arm-neutral worktree layout by saying the profile is what
makes the in-cell adjacency safe.

That promise is only worth anything if the profile is non-None. run.py's
`temporal_agent_read_isolation` requires `tools["bin"]`; a caller that passes an
empty `tools` dict raises KeyError inside profile construction, and a blanket
`except Exception` then turns the failure into `(None, {...})` -- isolation off,
on every cell, on every platform, recorded only as a `reason` string nothing
asserts on. These tests pin the live path so that regression is loud.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, run_b  # noqa: E402

SANDBOX_PRESENT = _harness.sandbox_executable().is_file()


def _fake_graphmark_root(base: pathlib.Path) -> pathlib.Path:
    """Just enough of a graphmark checkout for netjail resolution."""
    root = base / "graphmark"
    (root / "tools" / "netjail").mkdir(parents=True, exist_ok=True)
    return root


@unittest.skipUnless(SANDBOX_PRESENT,
                     "no /usr/bin/sandbox-exec: read isolation is inert by design here")
class ReadIsolationIsLiveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.graphmark = _fake_graphmark_root(self.tmp)
        self.worktree = self.tmp / "wt" / "abc123"
        self.worktree.mkdir(parents=True)
        self.config = {"graphmark_root": str(self.graphmark)}

    def test_profile_is_built_and_not_swallowed(self):
        profile, provenance = run_b.read_isolation(
            self.config, self.worktree, {"HOME": str(self.tmp), "PATH": "/usr/bin"}
        )
        self.assertIsNotNone(
            profile,
            f"read isolation produced NO profile, so every B session runs "
            f"unsandboxed: {provenance}",
        )
        self.assertEqual(provenance.get("backend"), "macos-sandbox-exec", provenance)

    def test_profile_denies_the_harness_tree_and_re_allows_the_worktree(self):
        profile, _ = run_b.read_isolation(
            self.config, self.worktree, {"HOME": str(self.tmp), "PATH": "/usr/bin"}
        )
        self.assertIsNotNone(profile)
        denied = [line for line in profile.splitlines() if line.startswith("(deny")]
        allowed = [line for line in profile.splitlines() if line.startswith("(allow")]
        self.assertTrue(
            any(str(_harness.AGENT_BENCH_DIR) in line for line in denied),
            "the harness tree (session-A artifacts, task JSONs) is not denied",
        )
        self.assertTrue(
            any(str(self.worktree.resolve()) in line for line in allowed),
            "this cell's own worktree is not re-allowed",
        )

    def test_a_results_root_outside_the_harness_tree_is_denied(self):
        """`--out /somewhere/else` must not put sibling arms' packets back in reach."""
        results = self.tmp / "elsewhere" / "results"
        results.mkdir(parents=True)
        profile, provenance = run_b.read_isolation(
            self.config, self.worktree, {"HOME": str(self.tmp), "PATH": "/usr/bin"},
            results_root=results,
        )
        self.assertIsNotNone(profile)
        self.assertTrue(
            any(line.startswith("(deny") and str(results.resolve()) in line
                for line in profile.splitlines()),
            "a results tree outside the harness checkout stays readable, so a B "
            "session can read every sibling arm's packet.txt",
        )
        self.assertIn(str(results.resolve()), provenance.get("extra_denied_roots") or [])

    def test_profile_construction_failure_is_loud_not_silent(self):
        """A broken profile must abort the cell, never downgrade to no sandbox."""
        original = _harness.temporal_agent_read_isolation

        def boom(*_args, **_kwargs):
            raise RuntimeError("synthetic profile failure")

        _harness.temporal_agent_read_isolation = boom  # type: ignore[assignment]
        try:
            with self.assertRaises(RuntimeError):
                run_b.read_isolation(
                    self.config, self.worktree, {"HOME": str(self.tmp), "PATH": "/usr/bin"}
                )
        finally:
            _harness.temporal_agent_read_isolation = original  # type: ignore[assignment]

    def test_isolation_can_be_disabled_only_by_an_explicit_recorded_opt_out(self):
        import os

        os.environ[run_b.NO_READ_ISOLATION_ENV] = "1"
        try:
            profile, provenance = run_b.read_isolation(
                self.config, self.worktree, {"HOME": str(self.tmp), "PATH": "/usr/bin"}
            )
        finally:
            os.environ.pop(run_b.NO_READ_ISOLATION_ENV, None)
        self.assertIsNone(profile)
        self.assertEqual(provenance.get("reason"), "disabled by BM_NO_READ_ISOLATION")


if __name__ == "__main__":
    unittest.main()
