#!/usr/bin/env python3
"""Tests that private-artifact detection judges paths inside the agent worktree.

Benchmark worktrees live under benchmarks/agent-brain/results/, so the absolute
path of an ordinary source file in the worktree contains the same substring that
marks a private benchmark artifact. Matching the raw absolute path therefore
flagged agents for editing the very file the task asked them to edit, which fails
the adherence axis and disqualifies the comparison that depends on those records.
"""

from __future__ import annotations

import importlib.util
import pathlib
import sys
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("run", HERE / "run.py")
assert SPEC is not None and SPEC.loader is not None
run = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = run
SPEC.loader.exec_module(run)


WORKTREE = "/repo/benchmarks/agent-brain/results/panel-x-20260814T000000Z/.worktrees/repo-abc123/repo"


class ForbiddenArtifactScopeTest(unittest.TestCase):
    def _flags_read(self, path: str) -> bool:
        return run.tool_arguments_access_forbidden_memory_artifact(
            "Read", {"file_path": path}, WORKTREE
        )

    def test_task_target_file_in_worktree_is_allowed(self) -> None:
        self.assertFalse(self._flags_read(f"{WORKTREE}/internal/cli/facts_merge.go"))

    def test_benchmark_scaffolding_inside_worktree_is_flagged(self) -> None:
        self.assertTrue(self._flags_read(f"{WORKTREE}/benchmarks/agent-brain/tasks/task.json"))

    def test_brain_state_inside_worktree_is_flagged(self) -> None:
        self.assertTrue(self._flags_read(f"{WORKTREE}/.entire/facts.json"))
        self.assertTrue(self._flags_read(f"{WORKTREE}/.benchmark/plugin/data/repos/local/repo-a/x"))

    def test_private_artifact_outside_worktree_is_flagged(self) -> None:
        self.assertTrue(self._flags_read("/elsewhere/benchmarks/agent-brain/tasks/task.json"))

    def test_relative_paths_keep_their_meaning(self) -> None:
        self.assertFalse(self._flags_read("internal/cli/facts_merge.go"))
        self.assertTrue(self._flags_read(".entire/facts.json"))

    def test_commands_are_scoped_the_same_way(self) -> None:
        self.assertFalse(
            run.command_accesses_forbidden_memory_artifact(
                f"go test {WORKTREE}/internal/cli/...", WORKTREE
            )
        )
        self.assertTrue(
            run.command_accesses_forbidden_memory_artifact(
                f"cat {WORKTREE}/benchmarks/agent-brain/tasks/task.json", WORKTREE
            )
        )

    def test_detection_without_a_worktree_is_unchanged(self) -> None:
        self.assertTrue(run.command_accesses_forbidden_memory_artifact("cat .entire/facts.json"))
        self.assertFalse(run.command_accesses_forbidden_memory_artifact("cat internal/cli/x.go"))


if __name__ == "__main__":
    unittest.main()
