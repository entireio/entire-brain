#!/usr/bin/env python3
"""Tests that private-artifact detection tracks information access, not intent.

Two layers. First, paths are judged inside the agent worktree: benchmark
worktrees live under benchmarks/agent-brain/results/, so the absolute path of
an ordinary source file contains the same substring that marks a private
artifact, and matching the raw path flagged agents for editing the file the
task asked them to edit.

Second, isolation is physical: hidden directories are deleted, the checkpoint
ref and its objects are purged, and the sandbox denies the harness tree outside
the worktree. A probe of an artifact that EXISTS (the .benchmark brain store in
a treatment arm) is a hard violation because content could flow; a probe of
something removed or denied is an advisory dead end. Excluding rows for
information-free probes starved comparisons below the minimum repetitions while
proving nothing about the treatment.
"""

from __future__ import annotations

import importlib.util
import pathlib
import subprocess
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("run", HERE / "run.py")
assert SPEC is not None and SPEC.loader is not None
run = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = run
SPEC.loader.exec_module(run)


class ForbiddenArtifactHardnessTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.worktree = pathlib.Path(self._tmp.name) / "repo"
        (self.worktree / ".benchmark" / "plugin" / "data").mkdir(parents=True)
        (self.worktree / ".benchmark" / "plugin" / "data" / "index.json").write_text("{}")
        (self.worktree / "internal" / "cli").mkdir(parents=True)
        (self.worktree / "internal" / "cli" / "facts_merge.go").write_text("package cli\n")
        # benchmarks/agent-brain is deliberately ABSENT: the harness deletes it.
        self.addCleanup(self._tmp.cleanup)

    def _read_hardness(self, path: str) -> str | None:
        return run.tool_arguments_forbidden_memory_artifact_hardness(
            "Read", {"file_path": path}, str(self.worktree)
        )

    def test_task_target_file_is_not_a_reference_at_all(self) -> None:
        self.assertIsNone(self._read_hardness(f"{self.worktree}/internal/cli/facts_merge.go"))

    def test_probe_of_existing_brain_store_is_hard(self) -> None:
        self.assertEqual(
            self._read_hardness(f"{self.worktree}/.benchmark/plugin/data/index.json"), "hard"
        )
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(
                "cat .benchmark/plugin/data/index.json", str(self.worktree)
            ),
            "hard",
        )

    def test_harness_go_runtime_state_does_not_make_a_probe_hard(self) -> None:
        # The harness creates these in EVERY arm (plugin_env), so a no_brain
        # worktree contains .benchmark even though no memory artifact exists.
        # Observed live: a name-only `find` existence probe in a no_brain cell
        # classified hard and starved the baseline below minimum repetitions.
        bare = pathlib.Path(self._tmp.name) / "no-brain-repo"
        (bare / ".benchmark" / "go-build-cache").mkdir(parents=True)
        (bare / ".benchmark" / "go-tmp").mkdir(parents=True)
        (bare / ".benchmark" / "go-env").write_text("GOPROXY=off\n")
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(
                'find . -maxdepth 1 -name ".entire*" -o -maxdepth 1 -name ".benchmark*"',
                str(bare),
            ),
            "advisory",
        )
        # The moment private content appears in the container, the same probe
        # is hard again.
        (bare / ".benchmark" / "plugin" / "data").mkdir(parents=True)
        (bare / ".benchmark" / "plugin" / "data" / "index.json").write_text("{}")
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(
                'find . -maxdepth 1 -name ".benchmark*"', str(bare)
            ),
            "hard",
        )

    def test_probe_of_removed_directory_is_advisory(self) -> None:
        self.assertEqual(
            self._read_hardness(f"{self.worktree}/benchmarks/agent-brain/tasks/task.json"),
            "advisory",
        )
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(
                "ls benchmarks/agent-brain 2>/dev/null", str(self.worktree)
            ),
            "advisory",
        )

    def test_absolute_path_outside_worktree_is_advisory(self) -> None:
        # The sandbox denies the harness tree; the probe cannot read content.
        self.assertEqual(
            self._read_hardness("/elsewhere/benchmarks/agent-brain/results/.worktrees"),
            "advisory",
        )

    def test_exclusion_patterns_are_not_findings(self) -> None:
        for command in (
            'find . -iname "*.md" -not -path "./.benchmark/*"',
            'find . -path ./.benchmark -prune -o -print',
            'grep -rn conf . --exclude-dir=.benchmark',
            'find . | grep -v /.benchmark',
        ):
            self.assertIsNone(
                run.command_forbidden_memory_artifact_hardness(command, str(self.worktree)),
                command,
            )

    def test_checkpoint_ref_probe_tracks_ref_existence(self) -> None:
        subprocess.run(["git", "init", "-q", str(self.worktree)], check=True)
        env_cmd = ["git", "-C", str(self.worktree)]
        subprocess.run([*env_cmd, "config", "user.email", "t@example.invalid"], check=True)
        subprocess.run([*env_cmd, "config", "user.name", "T"], check=True)
        subprocess.run([*env_cmd, "commit", "-qm", "base", "--allow-empty"], check=True)
        probe = "git log refs/heads/entire/checkpoints/v1"
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(probe, str(self.worktree)),
            "advisory",
            "purged ref: nothing to read",
        )
        subprocess.run(
            [*env_cmd, "update-ref", "refs/heads/entire/checkpoints/v1", "HEAD"], check=True
        )
        self.assertEqual(
            run.command_forbidden_memory_artifact_hardness(probe, str(self.worktree)),
            "hard",
            "a present ref is raw session history",
        )

    def test_detection_without_a_worktree_stays_strict(self) -> None:
        self.assertTrue(run.command_accesses_forbidden_memory_artifact("cat .entire/facts.json"))
        self.assertFalse(run.command_accesses_forbidden_memory_artifact("cat internal/cli/x.go"))


if __name__ == "__main__":
    unittest.main()
