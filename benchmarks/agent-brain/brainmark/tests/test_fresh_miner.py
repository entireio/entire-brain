"""Fresh-split miner: gates on canned PR fixtures, offline, no `gh`.

The network boundary of `mine_fresh.py` is `build_snapshot`. Everything after it
is a pure function of `SNAPSHOT.json`, so every gate is testable from a canned
snapshot with no GitHub access at all -- which is also the determinism claim
these tests exist to hold.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import mine_fresh, mine_pairs  # noqa: E402


# --------------------------------------------------------------------------
# fixtures
# --------------------------------------------------------------------------


def _file(name: str, added: list[str], status: str = "modified",
          symbol: str = "def handler") -> dict:
    patch = "\n".join([f"@@ -1,3 +1,4 @@ {symbol}", *[f"+{line}" for line in added]])
    return {"filename": name, "status": status, "previous_filename": "", "patch": patch}


def _pr(number: int, repo: str = "o/repo", *, body: str = "Fixes #12",
        title: str = "improve the widget", merged_at: str = "2026-03-01T10:00:00Z",
        base_sha: str = "base1", files: list[dict] | None = None) -> dict:
    return {
        "repo": repo,
        "number": number,
        "title": title,
        "body": body,
        "merged_at": merged_at,
        "merge_commit_sha": f"merge{number}",
        "base_sha": base_sha,
        "base_ref": "main",
        "head_sha": f"head{number}",
        "files": files if files is not None else [
            _file("src/widget.py", ["alpha_one = compute_alpha(seed)"]),
            _file("tests/test_widget.py", ["assert widget_works()"]),
        ],
    }


def _snapshot(prs: list[dict], cutoff: str = "2026-01-01") -> dict:
    return {
        "schema_version": 1, "training_cutoff": cutoff, "max_prs_per_repo": 25,
        "max_files_per_pr": 60, "repos": {}, "pr_count": len(prs), "prs": prs,
    }


class FakeOracle(mine_pairs.AncestryOracle):
    def __init__(self, verdict: bool | None = True) -> None:
        super().__init__(pathlib.Path("/nonexistent"))
        self._verdict = verdict

    def repo_available(self, repo: str) -> bool:
        return self._verdict is not None

    def has_commit(self, repo: str, commit: str) -> bool:
        return self._verdict is not None

    def is_ancestor(self, repo: str, older: str, newer: str) -> bool | None:
        return self._verdict


def _config(tmp: pathlib.Path, historical: list[dict] | None = None) -> dict:
    (tmp / "tasks").mkdir(parents=True, exist_ok=True)
    (tmp / "tasks" / "t.json").write_text(
        json.dumps({"instances": historical or []}), encoding="utf-8")
    return {
        "graphmark_root": str(tmp),
        "repo_cache": str(tmp / "cache"),
        "task_globs": ["tasks/*.json"],
        "pools": {"cache_dir": str(tmp / "pools"), "registry": {}},
        "fresh": {
            "training_cutoff": "2026-01-01", "max_prs_per_repo": 25,
            "max_files_per_pr": 60, "require_linked_issue": True,
            "require_test_delta": True, "gh_timeout_sec": 5,
            "out_dir": str(tmp / "fresh"),
        },
        "mining": {
            "min_shared_files": 1, "leakage_overlap_max": 0.8,
            "leakage_borderline_min": 0.6, "require_ancestor": True,
            "one_a_per_b": True, "target_candidates": 1,
            "score_weights": {"file_jaccard": 1.0, "symbol_overlap": 1.0},
        },
        "_config_sha256": "test",
    }


# --------------------------------------------------------------------------
# instance-level gates
# --------------------------------------------------------------------------


class LinkedIssueGateTest(unittest.TestCase):
    def test_closing_keywords_are_recognized(self):
        for text in ("Fixes #12", "fixed #12", "Closes #12", "resolve #12",
                     "This closes https://github.com/o/r/issues/12"):
            self.assertEqual(mine_fresh.linked_issues("", text), [12], text)

    def test_a_bare_mention_is_not_a_link(self):
        """`See #12` does not make the issue the problem statement."""
        self.assertEqual(mine_fresh.linked_issues("", "See #12 for context"), [])
        self.assertEqual(mine_fresh.linked_issues("", "related to #12"), [])

    def test_pr_without_a_linked_issue_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            cfg = mine_fresh.fresh_config(_config(pathlib.Path(raw)))
            matchers = mine_fresh.test_matchers(_config(pathlib.Path(raw)))
            instance, reason = mine_fresh.pr_to_instance(
                _pr(1, body="just a refactor"), matchers, cfg)
            self.assertIsNone(instance)
            self.assertEqual(reason, "no_linked_issue")


class TestDeltaGateTest(unittest.TestCase):
    def test_test_paths_across_languages(self):
        with tempfile.TemporaryDirectory() as raw:
            matchers = mine_fresh.test_matchers(_config(pathlib.Path(raw)))
            for path in ("tests/test_x.py", "src/foo_test.go", "spec/bar_spec.rb",
                         "__tests__/a.test.ts", "src/WidgetTest.java",
                         "packages/x/src/y.spec.tsx"):
                self.assertTrue(mine_fresh.is_test_path(path, matchers), path)
            for path in ("src/widget.py", "docs/testing.md", "lib/contest.go"):
                self.assertFalse(mine_fresh.is_test_path(path, matchers), path)

    def test_pr_with_no_test_delta_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            base = _config(pathlib.Path(raw))
            instance, reason = mine_fresh.pr_to_instance(
                _pr(1, files=[_file("src/widget.py", ["x = 1"])]),
                mine_fresh.test_matchers(base), mine_fresh.fresh_config(base))
            self.assertIsNone(instance)
            self.assertEqual(reason, "no_test_delta")

    def test_test_only_pr_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            base = _config(pathlib.Path(raw))
            instance, reason = mine_fresh.pr_to_instance(
                _pr(1, files=[_file("tests/test_widget.py", ["assert True"])]),
                mine_fresh.test_matchers(base), mine_fresh.fresh_config(base))
            self.assertIsNone(instance)
            self.assertEqual(reason, "no_code_delta")

    def test_gold_and_test_patches_are_split(self):
        with tempfile.TemporaryDirectory() as raw:
            base = _config(pathlib.Path(raw))
            instance, reason = mine_fresh.pr_to_instance(
                _pr(7), mine_fresh.test_matchers(base), mine_fresh.fresh_config(base))
            self.assertEqual(reason, "")
            self.assertEqual(mine_pairs.patch_files(instance["patch"]), {"src/widget.py"})
            self.assertEqual(mine_pairs.patch_files(instance["test_patch"]),
                             {"tests/test_widget.py"})
            self.assertEqual(instance["instance_id"], "o__repo-pr7")
            self.assertEqual(instance["base_commit"], "base1")
            self.assertEqual(instance["fresh_provenance"]["linked_issues"], [12])
            self.assertEqual(instance["split"], "fresh")

    def test_rebuilt_diff_is_parseable_by_the_shared_patch_parser(self):
        """mine_pairs keys off `diff --git`, which the files API does not send."""
        diff = mine_fresh.rebuild_diff([
            _file("src/a.py", ["gamma_value = derive_gamma(x)"], symbol="def alpha_fn"),
            _file("src/b.py", ["delta_value = 2"], status="added"),
        ])
        self.assertEqual(mine_pairs.patch_files(diff), {"src/a.py", "src/b.py"})
        self.assertIn("alpha_fn", mine_pairs.patch_symbols(diff))
        self.assertIn("gamma_value", mine_pairs.patch_body_tokens(diff))

    def test_created_at_is_normalized_onto_the_swebench_timeline(self):
        """RFC3339 vs 'YYYY-MM-DD HH:MM:SS' would split the ordering in two."""
        with tempfile.TemporaryDirectory() as raw:
            base = _config(pathlib.Path(raw))
            instance, _ = mine_fresh.pr_to_instance(
                _pr(1), mine_fresh.test_matchers(base), mine_fresh.fresh_config(base))
            self.assertEqual(instance["created_at"], "2026-03-01 10:00:00")
            self.assertGreater(instance["created_at"], "2024-01-01 00:00:00")


# --------------------------------------------------------------------------
# pair-level gates (shared verbatim with mine_pairs)
# --------------------------------------------------------------------------


class FreshPairGateTest(unittest.TestCase):
    def _historical(self, iid: str, commit: str, created: str, added: list[str],
                    path: str = "src/widget.py") -> dict:
        return {
            "instance_id": iid, "repo": "o/repo", "base_commit": commit,
            "created_at": created, "problem_statement": f"fix {iid}", "version": "1",
            "patch": "\n".join([
                f"diff --git a/{path} b/{path}", f"--- a/{path}", f"+++ b/{path}",
                "@@ -1,3 +1,4 @@ def handler", *[f"+{line}" for line in added],
            ]) + "\n",
        }

    def test_fresh_b_pairs_with_a_historical_a(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [self._historical(
                "hist__1", "h1", "2024-01-01 00:00:00",
                ["omega_nine = compute_omega(seed)"])])
            result = mine_fresh.mine_fresh(cfg, _snapshot([_pr(1)]),
                                           oracle=FakeOracle(True),
                                           snapshot_sha256="deadbeef")
            self.assertEqual(result["candidate_count"], 1)
            cand = result["candidates"][0]
            self.assertEqual(cand["a"]["instance_id"], "hist__1")
            self.assertEqual(cand["b"]["instance_id"], "o__repo-pr1")
            self.assertEqual(cand["a_side_split"], "historical")
            self.assertEqual(cand["split"], "fresh")
            self.assertTrue(cand["never_pool_with_main"])
            self.assertEqual(cand["snapshot_sha256"], "deadbeef")
            self.assertEqual(cand["fresh_provenance"]["pr_number"], 1)

    def test_a_historical_b_is_never_emitted_by_the_fresh_miner(self):
        """The fresh split is about post-cutoff Bs; historical pairs belong to
        mine_pairs and pooling them would defeat the whole point."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [
                self._historical("hist__1", "h1", "2024-01-01 00:00:00",
                                 ["omega_nine = compute_omega(seed)"]),
                self._historical("hist__2", "h2", "2024-06-01 00:00:00",
                                 ["sigma_ten = derive_sigma(other)"]),
            ])
            result = mine_fresh.mine_fresh(cfg, _snapshot([]), oracle=FakeOracle(True))
            self.assertEqual(result["candidate_count"], 0)

    def test_unverifiable_ancestry_is_rejected_here_too(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [self._historical(
                "hist__1", "h1", "2024-01-01 00:00:00",
                ["omega_nine = compute_omega(seed)"])])
            result = mine_fresh.mine_fresh(cfg, _snapshot([_pr(1)]),
                                           oracle=FakeOracle(None))
            self.assertEqual(result["candidate_count"], 0)
            self.assertEqual(result["rejects"]["unverifiable_ancestry"], 1)

    def test_leakage_cap_applies_to_fresh_pairs(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            same = ["identical_line = same_call(here)", "another_same = call_two(x)"]
            cfg = _config(tmp, [self._historical(
                "hist__1", "h1", "2024-01-01 00:00:00", same)])
            snapshot = _snapshot([_pr(1, files=[
                _file("src/widget.py", same),
                _file("tests/test_widget.py", ["assert widget_works()"]),
            ])])
            result = mine_fresh.mine_fresh(cfg, snapshot, oracle=FakeOracle(True))
            self.assertEqual(result["candidate_count"], 0)
            self.assertEqual(result["rejects"]["leakage_overlap"], 1)

    def test_no_shared_file_means_no_pair(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [self._historical(
                "hist__1", "h1", "2024-01-01 00:00:00",
                ["omega_nine = compute_omega(seed)"], path="src/other.py")])
            result = mine_fresh.mine_fresh(cfg, _snapshot([_pr(1)]),
                                           oracle=FakeOracle(True))
            self.assertEqual(result["candidate_count"], 0)
            self.assertEqual(result["rejects"]["no_shared_files"], 1)

    def test_fresh_a_with_fresh_b_is_allowed_and_labelled(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp)
            snapshot = _snapshot([
                _pr(1, merged_at="2026-02-01T00:00:00Z", files=[
                    _file("src/widget.py", ["alpha_one = compute_alpha(seed)"]),
                    _file("tests/test_widget.py", ["assert a()"])]),
                _pr(2, merged_at="2026-05-01T00:00:00Z", files=[
                    _file("src/widget.py", ["zeta_two = derive_zeta(other)"]),
                    _file("tests/test_widget.py", ["assert b()"])]),
            ])
            result = mine_fresh.mine_fresh(cfg, snapshot, oracle=FakeOracle(True))
            self.assertEqual(result["candidate_count"], 1)
            self.assertEqual(result["candidates"][0]["a_side_split"], "fresh")


# --------------------------------------------------------------------------
# determinism boundary + gh guard
# --------------------------------------------------------------------------


class SnapshotDeterminismTest(unittest.TestCase):
    def test_same_snapshot_yields_byte_identical_candidates(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [{
                "instance_id": "hist__1", "repo": "o/repo", "base_commit": "h1",
                "created_at": "2024-01-01 00:00:00", "problem_statement": "x",
                "patch": mine_fresh.rebuild_diff([
                    _file("src/widget.py", ["omega_nine = compute_omega(seed)"])]),
            }])
            snapshot = _snapshot([_pr(1), _pr(2, merged_at="2026-04-01T00:00:00Z")])
            one = mine_fresh.mine_fresh(cfg, snapshot, oracle=FakeOracle(True),
                                        snapshot_sha256="abc")
            two = mine_fresh.mine_fresh(cfg, dict(snapshot), oracle=FakeOracle(True),
                                        snapshot_sha256="abc")
            mine_pairs.write_candidates(one, tmp / "one")
            mine_pairs.write_candidates(two, tmp / "two")
            names = sorted(p.name for p in (tmp / "one").glob("*.json"))
            self.assertEqual(names, sorted(p.name for p in (tmp / "two").glob("*.json")))
            self.assertGreater(len(names), 1)
            for name in names:
                self.assertEqual((tmp / "one" / name).read_bytes(),
                                 (tmp / "two" / name).read_bytes(), name)

    def test_pr_order_in_the_snapshot_does_not_change_the_result(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp)
            prs = [
                _pr(1, merged_at="2026-02-01T00:00:00Z", files=[
                    _file("src/widget.py", ["alpha_one = compute_alpha(seed)"]),
                    _file("tests/test_widget.py", ["assert a()"])]),
                _pr(2, merged_at="2026-05-01T00:00:00Z", files=[
                    _file("src/widget.py", ["zeta_two = derive_zeta(other)"]),
                    _file("tests/test_widget.py", ["assert b()"])]),
            ]
            forward = mine_fresh.mine_fresh(cfg, _snapshot(prs), oracle=FakeOracle(True))
            backward = mine_fresh.mine_fresh(cfg, _snapshot(list(reversed(prs))),
                                             oracle=FakeOracle(True))
            self.assertEqual(forward["candidates"], backward["candidates"])

    def test_snapshot_is_preserved_by_a_mining_run(self):
        """write_candidates clears the directory; SNAPSHOT.json is its INPUT."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp)
            snapshot = _snapshot([_pr(1)])
            sha = mine_fresh.write_snapshot(cfg, snapshot)
            self.assertEqual(len(sha), 64)
            result = mine_fresh.mine_fresh(cfg, snapshot, oracle=FakeOracle(True),
                                           snapshot_sha256=sha)
            mine_pairs.write_candidates(result, mine_fresh.out_dir(cfg),
                                        preserve=(mine_fresh.SNAPSHOT_NAME,))
            reread, sha2 = mine_fresh.read_snapshot(cfg)
            self.assertEqual(sha, sha2)
            self.assertEqual(reread["pr_count"], 1)

    def test_missing_snapshot_raises_rather_than_mining_nothing(self):
        with tempfile.TemporaryDirectory() as raw:
            cfg = _config(pathlib.Path(raw))
            with self.assertRaises(mine_fresh.FreshUnavailable):
                mine_fresh.read_snapshot(cfg)


class GhGuardTest(unittest.TestCase):
    def test_missing_gh_is_a_skip_not_a_crash(self):
        saved = mine_fresh.shutil.which
        mine_fresh.shutil.which = lambda name: None
        try:
            ok, why = mine_fresh.gh_available()
        finally:
            mine_fresh.shutil.which = saved
        self.assertFalse(ok)
        self.assertIn("PATH", why)

    def test_cli_exits_zero_without_gh(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg_path = tmp / "config.json"
            cfg = _config(tmp)
            cfg.pop("_config_sha256")
            cfg_path.write_text(json.dumps(cfg), encoding="utf-8")
            saved = mine_fresh.shutil.which
            mine_fresh.shutil.which = lambda name: None
            try:
                code = mine_fresh.main(["--config", str(cfg_path)])
            finally:
                mine_fresh.shutil.which = saved
            self.assertEqual(code, 0)

    def test_build_snapshot_refuses_without_gh(self):
        with tempfile.TemporaryDirectory() as raw:
            cfg = _config(pathlib.Path(raw))
            saved = mine_fresh.shutil.which
            mine_fresh.shutil.which = lambda name: None
            try:
                with self.assertRaises(mine_fresh.FreshUnavailable):
                    mine_fresh.build_snapshot(cfg, ["o/repo"])
            finally:
                mine_fresh.shutil.which = saved


if __name__ == "__main__":
    unittest.main()
