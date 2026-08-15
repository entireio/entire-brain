#!/usr/bin/env python3
"""Tests for the isolation repairs: checkpoint object purge, scrubbed-task
boundary policy, and generic host-path record hygiene.

The checkpoint purge closes an observed leak: deleting the checkpoint ref and
running `git prune` left the fetched PACK untouched, so an agent recovered the
"deleted" transcripts (and the task's answer) via `git fsck --unreachable` plus
`git cat-file`.
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


ANSWER = "defaultFactConfidenceThreshold = 0.75"


def _git(*args: str, cwd: pathlib.Path) -> str:
    return subprocess.run(
        ["git", *args], cwd=cwd, check=True, capture_output=True, text=True
    ).stdout


def _seed_checkpoint_source(root: pathlib.Path) -> pathlib.Path:
    """A source repo whose checkpoint history is big enough to arrive as a pack."""
    src = root / "src"
    src.mkdir()
    _git("init", "-q", cwd=src)
    _git("config", "user.email", "t@example.invalid", cwd=src)
    _git("config", "user.name", "T", cwd=src)
    for i in range(60):
        (src / f"transcript-{i}.jsonl").write_text(f"session {i} {ANSWER}\n")
    _git("add", "-A", cwd=src)
    _git("commit", "-qm", "checkpoints", cwd=src)
    for i in range(60):
        (src / f"transcript-{i}.jsonl").write_text(f"session {i} updated\n")
    _git("add", "-A", cwd=src)
    _git("commit", "-qm", "more checkpoints", cwd=src)
    return src


class CheckpointObjectPurgeTest(unittest.TestCase):
    def test_removal_leaves_no_recoverable_transcript_objects(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            src = _seed_checkpoint_source(root)
            worktree = root / "worktree"
            worktree.mkdir()
            _git("init", "-q", cwd=worktree)
            _git("config", "user.email", "t@example.invalid", cwd=worktree)
            _git("config", "user.name", "T", cwd=worktree)
            _git("commit", "-qm", "base", "--allow-empty", cwd=worktree)
            _git("fetch", "-q", str(src), f"+HEAD:{run.CHECKPOINT_REF}", cwd=worktree)

            removed = run.remove_agent_visible_entire_history(worktree)
            self.assertTrue(removed)

            unreachable = subprocess.run(
                ["git", "fsck", "--unreachable", "--no-reflogs"],
                cwd=worktree, capture_output=True, text=True,
            ).stdout.strip()
            self.assertEqual(unreachable, "", "fetched checkpoint objects survived removal")

            # The answer must be unrecoverable from the whole object store.
            grep = subprocess.run(
                ["git", "grep", "-F", ANSWER, "--all-match", "--cached"],
                cwd=worktree, capture_output=True, text=True,
            )
            self.assertNotEqual(grep.returncode, 0)


class ScrubbedTaskBoundaryPolicyTest(unittest.TestCase):
    AGENT_INFO = {
        "activity": {
            "commands": [
                "git cat-file -p 2d38362 | head -5",
                "git reflog --all",
            ]
        }
    }
    ATTESTATIONS = [{"source_history_parent": "2d383620e9d1dc5609f1657d1d40a4b247433b03"}]

    def test_unscrubbed_tasks_keep_the_strict_exclusion(self) -> None:
        audit = run.baseline_history_audit(self.AGENT_INFO, self.ATTESTATIONS, {})
        self.assertFalse(audit["ok"])
        self.assertTrue(audit["findings"])

    def test_scrubbed_tasks_record_probing_as_advisory(self) -> None:
        audit = run.baseline_history_audit(
            self.AGENT_INFO, self.ATTESTATIONS, {"scrub_answer_from_history": True}
        )
        self.assertTrue(audit["ok"], "probing an answer-free boundary must not void the row")
        self.assertEqual(audit["findings"], [])
        self.assertTrue(audit["advisory_findings"], "the probing must stay visible in the record")

    def test_clean_rows_are_unchanged_either_way(self) -> None:
        clean = {"activity": {"commands": ["go test ./..."]}}
        for task in ({}, {"scrub_answer_from_history": True}):
            audit = run.baseline_history_audit(clean, self.ATTESTATIONS, task)
            self.assertTrue(audit["ok"])
            self.assertNotIn("advisory_findings", audit)


class RecordHostPathHygieneTest(unittest.TestCase):
    def test_unregistered_host_paths_are_scrubbed_generically(self) -> None:
        record = {
            "agent_info": {
                "activity": {
                    "commands": [
                        "cat /Users/someone/private/notes.txt",
                        "ls /tmp/agent-scratch",
                        "go test ./internal/cli/",
                    ]
                }
            },
            "worktree": "/wt/repo/internal",
        }
        out = run.redact_record_host_paths(record, {pathlib.Path("/wt/repo"): "<agent-worktree>"})
        commands = out["agent_info"]["activity"]["commands"]
        self.assertNotIn("/Users/", " ".join(commands))
        self.assertNotIn("/tmp/", " ".join(commands))
        self.assertIn("go test ./internal/cli/", commands)
        self.assertEqual(out["worktree"], "<agent-worktree>/internal")


if __name__ == "__main__":
    unittest.main()
