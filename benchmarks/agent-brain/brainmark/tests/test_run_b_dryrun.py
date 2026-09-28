"""run_b end-to-end against a STUBBED `claude` on PATH. No paid calls, no network.

This exercises the real driver -- packet build, symmetry gate, worktree, patch
collection, cc_out extraction, mechmetrics, results layout, resume guard -- with
the only substitution being the agent binary itself. A stub that writes a canned
stream and touches a file is enough to prove the plumbing, and it is the only
part of the paid path that can be checked for free.
"""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, run_b  # noqa: E402

ARMS = ["no_brain", "mem0"]  # mem0 is stubbed below; full_brain has its own test


CLAUDE_STUB = textwrap.dedent("""\
    #!/usr/bin/env bash
    # Stubbed `claude`: emits a canned stream-JSON session and makes one edit,
    # so collect_patch.sh has something real to collect.
    set -u
    printf '%s\\n' '{"type":"system","subtype":"init","model":"stub"}'
    printf '%s\\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"x"}}]}}'
    printf '%s\\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"STUB.md"}}]}}'
    printf '%s\\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"STUB.md"}}]}}'
    # Report our own cwd the way a real agent's shell output does. The fairness
    # test reads this back: whatever lands here is readable BY THE AGENT.
    printf '{"type":"assistant","message":{"content":[{"type":"text","text":"cwd=%s"}]}}\\n' "$PWD"
    echo "brainmark stub edit" >> STUB_EDIT.txt
    printf '%s\\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":1234,"total_cost_usd":0.01,"modelUsage":{"stub":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}'
    exit 0
""")


def make_git_repo(path: pathlib.Path) -> str:
    """A tiny real git repo so worktree add / collect_patch behave normally."""
    path.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"}
    run = lambda *a: subprocess.run(["git", "-C", str(path), *a], check=True,  # noqa: E731
                                    capture_output=True, env=env)
    run("init", "-q", "-b", "main")
    (path / "STUB.md").write_text("hello\n", encoding="utf-8")
    run("add", "-A")
    run("commit", "-qm", "init")
    out = subprocess.run(["git", "-C", str(path), "rev-parse", "HEAD"],
                         capture_output=True, text=True, check=True)
    return out.stdout.strip()


class RunBDryRunTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        tmp = pathlib.Path(self.tmp.name)

        # Fake graphmark root: netjail + collect_patch.sh copied from the real one.
        self.graphmark = tmp / "graphmark"
        (self.graphmark / "tools").mkdir(parents=True)
        from graphmark_fixture import install
        install(self.graphmark)

        # Repo cache with one real repo.
        self.repo_cache = tmp / "repo-cache"
        self.repo_cache.mkdir()
        self.commit = make_git_repo(self.repo_cache / "o_r")

        # `claude` stub first on PATH.
        self.stubdir = tmp / "stubbin"
        self.stubdir.mkdir()
        stub = self.stubdir / "claude"
        stub.write_text(CLAUDE_STUB, encoding="utf-8")
        stub.chmod(0o755)
        self._old_path = os.environ["PATH"]
        os.environ["PATH"] = f"{self.stubdir}{os.pathsep}{self._old_path}"

        # Keep the opaque worktrees inside this test's tmpdir so the suite
        # cleans up after itself. The arm-neutrality being asserted is a
        # property of the PATH SHAPE, not of which parent dir is used.
        self.wt_root = tmp / "wt"
        self._old_wt_root = os.environ.get(run_b.WORKTREE_ROOT_ENV)
        os.environ[run_b.WORKTREE_ROOT_ENV] = str(self.wt_root)
        self._old_in_cell = os.environ.pop(run_b.WORKTREE_IN_CELL_ENV, None)

        self.config = json.loads((_harness.BRAINMARK_DIR / "config.json").read_text())
        self.config.update({
            "graphmark_root": str(self.graphmark),
            "repo_cache": str(self.repo_cache),
            "arms": ARMS,
            "_config_sha256": "test",
        })
        self.config["agent"]["timeout_sec"] = 120
        # These dry-run tests stub the `claude` binary on PATH; pin the backend so the
        # suite is independent of the machine-level default in config.json.
        self.config["agent"]["backend"] = "claude"

        self.pair = {
            "pair_id": "a1__then__b1", "repo": "o/r",
            "a": {"instance_id": "a1", "base_commit": self.commit,
                  "problem_statement": "earlier issue"},
            "b": {"instance_id": "b1", "base_commit": self.commit,
                  "problem_statement": "later related issue"},
        }

        # A pinned session-A transcript.
        self.a_dir = tmp / "A"
        self.a_dir.mkdir()
        transcript = self.a_dir / "session_transcript.jsonl"
        transcript.write_text("".join(json.dumps(e) + "\n" for e in [
            {"type": "assistant", "message": {"content": [
                {"type": "text", "text": "The widget renderer lives in STUB.md."}]}},
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "name": "Read", "input": {"file_path": "STUB.md"}}]}},
        ]), encoding="utf-8")
        (self.a_dir / "meta.json").write_text(_harness.pretty_json({
            "pair_id": "a1__then__b1",
            "transcript_path": str(transcript),
            "transcript_sha256": _harness.sha256_file(transcript),
            "created_at": "2026-01-01T00:00:00Z",
        }), encoding="utf-8")

        self.out_root = tmp / "results"

        # Stub the mem0 arm's packet so no competitor backend is needed.
        from brainmark import memsources
        from brainmark.memsources import base as msbase

        self._real_mem0 = memsources.ARMS["mem0"]
        memsources.ARMS["mem0"] = lambda query, max_bytes, top_k, **kw: msbase.build_packet(
            arm="mem0", query=query,
            results=msbase.normalize_results(
                [{"memory": "prior session read STUB.md", "score": 1.0, "id": "m1"}], top_k),
            max_bytes=max_bytes, prep={"seconds": 0.01},
        )

    def tearDown(self) -> None:
        from brainmark import memsources

        memsources.ARMS["mem0"] = self._real_mem0
        os.environ["PATH"] = self._old_path
        for key, value in ((run_b.WORKTREE_ROOT_ENV, self._old_wt_root),
                           (run_b.WORKTREE_IN_CELL_ENV, self._old_in_cell)):
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self.tmp.cleanup()

    def test_full_brain_prepares_isolated_repo_at_pinned_base(self):
        source = self.repo_cache / "o_r"
        for number in range(2):
            (source / "history.txt").write_text(str(number))
            subprocess.run(["git", "-C", str(source), "add", "history.txt"], check=True)
            subprocess.run(["git", "-C", str(source), "-c", "user.name=t", "-c",
                            "user.email=t@e", "commit", "-qm", f"history {number}"], check=True)
        self.pair["b"]["base_commit"] = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
        (source / "future.txt").write_text("future solution")
        subprocess.run(["git", "-C", str(source), "add", "future.txt"], check=True)
        subprocess.run(["git", "-C", str(source), "-c", "user.name=t", "-c",
                        "user.email=t@e", "commit", "-qm", "future"], check=True)
        future = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
        binary = self.stubdir / "entire-brain"
        binary.write_text("#!" + sys.executable + "\n" + textwrap.dedent("""
            import json, os, pathlib, subprocess, sys
            command = sys.argv[1]
            if command in ("status", "search"):
                assert pathlib.Path("STUB.md").read_text() == "hello\\n"
                assert not pathlib.Path("future.txt").exists()
                assert pathlib.Path(".git").is_dir()
            if command == "status":
                print(json.dumps({"brain": {"path": os.environ["ENTIRE_PLUGIN_DATA_DIR"] + "/brain"}}))
            elif command == "search":
                print(json.dumps({"results": [{"id": "f1", "text": "prior STUB.md", "score": 1.0}]}))
            else:
                print("{}")
        """))
        binary.chmod(0o755)
        self.config["brain"]["binary"] = str(binary)
        self.config["brain"]["distill"] = {"agent": "claude", "model": "stub", "effort": "low"}
        run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                       tier="pilot", arms=["full_brain"])
        root = self.out_root / self.pair["pair_id"]
        repo = root / "brain-repo"
        self.assertIn("prior STUB.md", (root / "full_brain" / "packet.txt").read_text())
        expected_history = subprocess.check_output(
            ["git", "-C", str(source), "log", "--first-parent", "--format=%H",
             self.pair["b"]["base_commit"]], text=True)
        self.assertEqual(subprocess.check_output(
            ["git", "-C", str(repo), "log", "--first-parent", "--format=%H"], text=True),
            expected_history)

        self.assertEqual(subprocess.check_output(["git", "-C", str(repo), "remote"], text=True), "")
        self.assertNotEqual(subprocess.run(["git", "-C", str(repo), "cat-file", "-e", future],
                                          capture_output=True).returncode, 0)
        self.assertEqual(subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip(), future)
        self.assertEqual(subprocess.check_output(["git", "-C", str(source), "status", "--porcelain"], text=True), "")

    def test_full_results_layout_is_produced(self):
        summary = run_b.run_pair(
            self.config, self.pair, self.a_dir, self.out_root, tier="pilot", arms=ARMS,
        )
        pair_root = self.out_root / self.pair["pair_id"]
        self.assertTrue((pair_root / "pair_summary.json").is_file())

        for arm in ARMS:
            cell = pair_root / arm
            with self.subTest(arm=arm):
                for name in ("cc_out.json", "stream.jsonl", "patch.diff", "packet.txt",
                             "packet.sha256", "prompt_sym.sha256", "prompt.txt", "meta.json"):
                    self.assertTrue((cell / name).is_file(), f"{arm}: missing {name}")

                self.assertEqual(
                    (cell / "packet.sha256").read_text(encoding="utf-8").strip(),
                    _harness.sha256_file(cell / "packet.txt"),
                )
                cc_out = json.loads((cell / "cc_out.json").read_text(encoding="utf-8"))
                self.assertEqual(cc_out["subtype"], "success")
                self.assertAlmostEqual(cc_out["total_cost_usd"], 0.01)

                meta = json.loads((cell / "meta.json").read_text(encoding="utf-8"))
                self.assertEqual(meta["mechmetrics"]["locate_calls_pre_edit"], 2)
                self.assertFalse(meta["mechmetrics"]["no_edit"])
                self.assertGreater(meta["patch_bytes"], 0, "collect_patch produced nothing")

    def test_all_arms_share_one_symmetry_sha(self):
        summary = run_b.run_pair(
            self.config, self.pair, self.a_dir, self.out_root, tier="pilot", arms=ARMS)
        shas = {
            arm: (self.out_root / self.pair["pair_id"] / arm / "prompt_sym.sha256")
            .read_text(encoding="utf-8").strip()
            for arm in ARMS
        }
        self.assertEqual(len(set(shas.values())), 1, shas)
        self.assertEqual(set(shas.values()), {summary["prompt_sym_sha256"]})

    def test_packets_differ_between_arms(self):
        summary = run_b.run_pair(
            self.config, self.pair, self.a_dir, self.out_root, tier="pilot", arms=ARMS)
        self.assertNotEqual(summary["packet_sha256"]["no_brain"],
                            summary["packet_sha256"]["mem0"])

    def test_resume_skips_a_complete_cell(self):
        run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                       tier="pilot", arms=ARMS)
        again = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                               tier="pilot", arms=ARMS, resume=True)
        for arm in ARMS:
            self.assertEqual(again["cells"][arm].get("skipped"), "already complete")

    def test_a_transcript_pin_mismatch_aborts(self):
        transcript = pathlib.Path(
            json.loads((self.a_dir / "meta.json").read_text())["transcript_path"])
        transcript.write_text("tampered\n", encoding="utf-8")
        with self.assertRaises(RuntimeError) as ctx:
            run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                           tier="pilot", arms=ARMS)
        self.assertIn("pin mismatch", str(ctx.exception))

    def test_worktree_path_leaks_neither_the_arm_name_nor_the_results_root(self):
        """FAIRNESS. The agent's own `pwd` must not tell it which arm it is in.

        A worktree at <results>/<pair_id>/<arm>/worktree puts the arm name in
        the cwd, and the cwd appears in the agent's shell output -- an
        arm-asymmetric cue delivered outside the prompt. It also leaves the
        SIBLING arms' packet.txt/prompt.txt two levels above the agent.

        Asserted on BOTH the recorded path and the cwd the stub actually
        observed, because only the latter proves what the agent could read.
        """
        run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                       tier="pilot", arms=ARMS)
        results_root = str(self.out_root.resolve())

        for arm in ARMS:
            cell = self.out_root / self.pair["pair_id"] / arm
            with self.subTest(arm=arm):
                meta = json.loads((cell / "meta.json").read_text(encoding="utf-8"))
                self.assertTrue(meta["worktree_arm_neutral"])

                observed = [
                    json.loads(line)["message"]["content"][0]["text"]
                    for line in (cell / "stream.jsonl").read_text(
                        encoding="utf-8").splitlines()
                    if '"cwd=' in line
                ]
                self.assertEqual(len(observed), 1, "stub did not report its cwd")
                agent_cwd = observed[0][len("cwd="):]

                for where, path in (("meta", meta["worktree"]), ("agent cwd", agent_cwd)):
                    resolved = str(pathlib.Path(path).resolve())
                    self.assertNotIn(arm, resolved,
                                     f"{where} leaks the ARM NAME: {resolved}")
                    self.assertNotIn(self.pair["pair_id"], resolved,
                                     f"{where} leaks the pair id: {resolved}")
                    self.assertFalse(
                        resolved.startswith(results_root),
                        f"{where} is inside the results tree, so sibling arms' "
                        f"packets are reachable from it: {resolved}")

        # ...and the two arms must not collide on one directory.
        paths = {
            arm: json.loads(
                (self.out_root / self.pair["pair_id"] / arm / "meta.json")
                .read_text(encoding="utf-8"))["worktree"]
            for arm in ARMS
        }
        self.assertEqual(len(set(paths.values())), len(ARMS), paths)

    def test_null_test_mode_gives_every_arm_the_sentinel(self):
        summary = run_b.run_pair(
            self.config, self.pair, self.a_dir, self.out_root, tier="pilot", arms=ARMS,
            force_empty_packet=True)
        shas = set(summary["packet_sha256"].values())
        self.assertEqual(len(shas), 1, "null test must deliver identical packets")


class WorktreePathTest(unittest.TestCase):
    """`worktree_path` in isolation -- no agent, no git, no results tree."""

    CELL = pathlib.Path("/results/a1__then__b1/full_brain")

    def setUp(self) -> None:
        self._saved = {k: os.environ.pop(k, None)
                       for k in (run_b.WORKTREE_ROOT_ENV, run_b.WORKTREE_IN_CELL_ENV)}

    def tearDown(self) -> None:
        for key, value in self._saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def test_default_is_arm_neutral_and_outside_the_cell(self):
        path, neutral = run_b.worktree_path(self.CELL, "a1__then__b1", "full_brain", 0)
        self.assertTrue(neutral)
        text = str(path)
        self.assertNotIn("full_brain", text)
        self.assertNotIn("a1__then__b1", text)
        self.assertNotIn("results", text)
        self.assertFalse(text.startswith(str(self.CELL)))

    def test_arms_get_distinct_paths_and_the_key_is_stable(self):
        args = ("a1__then__b1", None)
        paths = {arm: run_b.worktree_path(self.CELL, args[0], arm, args[1])[0]
                 for arm in ("no_brain", "full_brain", "mem0")}
        self.assertEqual(len(set(paths.values())), 3, paths)
        # Stable across calls -- a resumed run must reuse the same directory.
        self.assertEqual(run_b.worktree_key("p", "no_brain", 1),
                         run_b.worktree_key("p", "no_brain", 1))
        # ...and reps do not share one.
        self.assertNotEqual(run_b.worktree_key("p", "no_brain", 1),
                            run_b.worktree_key("p", "no_brain", 2))

    def test_key_is_opaque_hex(self):
        key = run_b.worktree_key("a1__then__b1", "full_brain", 0)
        self.assertEqual(len(key), 16)
        self.assertTrue(all(c in "0123456789abcdef" for c in key), key)

    def test_root_env_is_honoured(self):
        os.environ[run_b.WORKTREE_ROOT_ENV] = "/scratch/wt"
        path, neutral = run_b.worktree_path(self.CELL, "p", "mem0", 0)
        self.assertTrue(neutral)
        self.assertTrue(str(path).startswith("/scratch/wt"), path)

    def test_debug_opt_in_restores_the_in_cell_layout_and_is_flagged(self):
        os.environ[run_b.WORKTREE_IN_CELL_ENV] = "1"
        path, neutral = run_b.worktree_path(self.CELL, "p", "mem0", 0)
        self.assertEqual(path, self.CELL / "worktree")
        self.assertFalse(neutral, "an in-cell worktree must NEVER report as arm-neutral")


if __name__ == "__main__":
    unittest.main()
