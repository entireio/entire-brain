#!/usr/bin/env python3
"""Tests that a task's decided value can be removed from agent-visible history.

A task whose premise is "this decision lives only in retained session history"
only holds if the value is absent from the ordinary git history the agent may
read. setup_replacements rewrite the working tree, so the decided value stayed
committed at the pinned base commit, and the baseline-history audit deliberately
permits ordinary log/blame/pickaxe use. An agent running `git log -S` therefore
recovered the answer without any protocol violation.
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
BROKEN = "defaultFactConfidenceThreshold = 0.7"


def _git(*args: str, cwd: pathlib.Path) -> str:
    return subprocess.run(
        ["git", *args], cwd=cwd, check=True, capture_output=True, text=True
    ).stdout


def _seed_repo(root: pathlib.Path) -> pathlib.Path:
    repo = root / "source"
    repo.mkdir()
    _git("init", "--initial-branch=main", cwd=repo)
    _git("config", "user.email", "t@example.invalid", cwd=repo)
    _git("config", "user.name", "T", cwd=repo)
    target = repo / "facts_merge.go"
    target.write_text(f"const (\n\t{ANSWER}\n)\n")
    _git("add", "-A", cwd=repo)
    _git("commit", "-m", "add the decided gate", cwd=repo)
    # A later commit that does not touch the value, so history has depth.
    (repo / "README.md").write_text("docs\n")
    _git("add", "-A", cwd=repo)
    _git("commit", "-m", "docs", cwd=repo)
    return repo


class HistoryAnswerScrubTest(unittest.TestCase):
    def test_task_must_opt_in(self) -> None:
        task = {"setup_replacements": [{"old": ANSWER, "new": BROKEN}]}
        self.assertEqual(run.history_scrub_replacements(task), [])
        task["scrub_answer_from_history"] = True
        self.assertEqual(run.history_scrub_replacements(task), [(ANSWER.encode(), BROKEN.encode())])

    def test_extra_replacements_are_collected(self) -> None:
        task = {
            "scrub_answer_from_history": True,
            "setup_replacements": [{"old": ANSWER, "new": BROKEN}],
            "history_scrub_replacements": [
                {"old": "falls back to the default (0.75)", "new": "falls back to the default"}
            ],
        }
        pairs = run.history_scrub_replacements(task)
        self.assertEqual(len(pairs), 2)
        self.assertIn((b"falls back to the default (0.75)", b"falls back to the default"), pairs)

    def test_answer_is_absent_from_every_commit_after_scrub(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            repo = _seed_repo(root)
            head = _git("rev-parse", "HEAD", cwd=repo).strip()

            # Without the scrub the pickaxe finds the decided value, which is the
            # leak that let a no_brain baseline solve a history-only task.
            unscrubbed = run.filtered_agent_history_repo(repo, head, [], scrub_replacements=[])
            hit = subprocess.run(
                ["git", "log", "--all", "-S", ANSWER, "--oneline"],
                cwd=unscrubbed, capture_output=True, text=True,
            )
            self.assertTrue(hit.stdout.strip(), "expected the unscrubbed history to leak the answer")

            scrubbed = run.filtered_agent_history_repo(
                repo, head, [], scrub_replacements=[(ANSWER.encode(), BROKEN.encode())]
            )
            miss = subprocess.run(
                ["git", "log", "--all", "-S", ANSWER, "--oneline"],
                cwd=scrubbed, capture_output=True, text=True,
            )
            self.assertEqual(miss.stdout.strip(), "", "scrubbed history still leaks the answer")

            revisions = _git("rev-list", "--all", cwd=scrubbed).split()
            grep = subprocess.run(
                ["git", "grep", "-l", "-F", "-e", ANSWER, *revisions],
                cwd=scrubbed, capture_output=True, text=True,
            )
            self.assertEqual(grep.stdout.strip(), "")

            # The history stays usable: the file still exists, with the broken value.
            blob = _git("show", "HEAD:facts_merge.go", cwd=scrubbed)
            self.assertIn(BROKEN, blob)
            self.assertEqual(len(revisions), 2, "scrubbing must preserve the commit graph")

    def test_scrubbed_and_unscrubbed_use_separate_cache_entries(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            repo = _seed_repo(root)
            head = _git("rev-parse", "HEAD", cwd=repo).strip()
            plain = run.filtered_agent_history_repo(repo, head, [], scrub_replacements=[])
            scrubbed = run.filtered_agent_history_repo(
                repo, head, [], scrub_replacements=[(ANSWER.encode(), BROKEN.encode())]
            )
            self.assertNotEqual(plain, scrubbed)


if __name__ == "__main__":
    unittest.main()
