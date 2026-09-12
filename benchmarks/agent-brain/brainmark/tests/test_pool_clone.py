"""Repo-cache sweep: idempotency, ledger stability, safety of the staging dir.

Offline. `clone_one` is monkeypatched everywhere a clone would be attempted --
these tests are about the LEDGER and the skip logic, which is where a resumable
sweep goes wrong, not about git's ability to clone.
"""

from __future__ import annotations

import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import clone_repos  # noqa: E402


def _instance(iid: str, repo: str) -> dict:
    return {
        "instance_id": iid, "repo": repo, "base_commit": iid,
        "created_at": "2020-01-01 00:00:00", "patch": "", "problem_statement": "x",
    }


def _config(tmp: pathlib.Path, instances: list[dict]) -> dict:
    (tmp / "tasks").mkdir(parents=True, exist_ok=True)
    (tmp / "tasks" / "t.json").write_text(
        json.dumps({"instances": instances}), encoding="utf-8")
    (tmp / "cache").mkdir(parents=True, exist_ok=True)
    return {
        "graphmark_root": str(tmp),
        "repo_cache": str(tmp / "cache"),
        "task_globs": ["tasks/*.json"],
        "clone": {
            "min_instances_per_repo": 2, "filter": "blob:none",
            "host": "https://github.com/", "timeout_sec": 5,
            "ledger": str(tmp / "LEDGER.json"),
        },
        "pools": {"cache_dir": str(tmp / "pools"), "registry": {}},
        "_config_sha256": "test",
    }


def _make_repo(path: pathlib.Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "init", "-q", str(path)], check=True, capture_output=True)


class RepoInventoryTest(unittest.TestCase):
    def test_single_instance_repos_are_never_cloned(self):
        """One instance can never form an (A, B) pair, so it is not worth disk."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/lonely"),
                                _instance("b", "o/paired"), _instance("c", "o/paired")])
            attempted = []
            original = clone_repos.clone_one
            clone_repos.clone_one = (
                lambda config, repo, dest: (attempted.append(repo) or (True, "cloned")))
            try:
                result = clone_repos.sweep(cfg)
            finally:
                clone_repos.clone_one = original
            self.assertEqual(result["repos"]["o/lonely"]["state"],
                             clone_repos.STATE_BELOW_MIN)
            self.assertEqual(result["repos"]["o/lonely"]["action"], "not_needed")
            self.assertEqual(attempted, ["o/paired"],
                             "a single-instance repo was cloned")

    def test_existing_clone_is_detected_and_skipped(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/repo"), _instance("b", "o/repo")])
            _make_repo(tmp / "cache" / "o_repo")
            attempts = []
            original = clone_repos.clone_one
            clone_repos.clone_one = lambda *a, **k: (attempts.append(a) or (True, "cloned"))
            try:
                result = clone_repos.sweep(cfg)
            finally:
                clone_repos.clone_one = original
            self.assertEqual(attempts, [], "cloned a repo that was already present")
            self.assertEqual(result["repos"]["o/repo"]["state"], clone_repos.STATE_OK)
            self.assertEqual(result["repos"]["o/repo"]["action"], "already_present")

    def test_directory_that_is_not_a_git_repo_is_not_counted_as_present(self):
        """A bare mkdir would make the ancestry oracle silently answer 'no'."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            (tmp / "cache" / "o_repo").mkdir(parents=True)
            self.assertFalse(clone_repos.is_git_repo(tmp / "cache" / "o_repo"))
            _make_repo(tmp / "cache" / "o_real")
            self.assertTrue(clone_repos.is_git_repo(tmp / "cache" / "o_real"))

    def test_only_repos_and_limit_are_honored(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [
                _instance("a1", "o/one"), _instance("a2", "o/one"),
                _instance("b1", "o/two"), _instance("b2", "o/two"),
                _instance("c1", "o/three"), _instance("c2", "o/three"),
            ])
            attempted = []
            original = clone_repos.clone_one
            clone_repos.clone_one = (
                lambda config, repo, dest: (attempted.append(repo) or (True, "cloned")))
            try:
                clone_repos.sweep(cfg, only_repos=["o/one", "o/two"], limit=1)
            finally:
                clone_repos.clone_one = original
            self.assertEqual(len(attempted), 1)
            self.assertIn(attempted[0], {"o/one", "o/two"})


class LedgerIdempotencyTest(unittest.TestCase):
    def test_second_sweep_writes_a_byte_identical_ledger(self):
        """MUTATION TARGET: a wallclock or duration field here breaks this."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/repo"), _instance("b", "o/repo"),
                                _instance("c", "o/solo")])

            def fake_clone(config, repo, dest):
                _make_repo(dest)
                return True, "cloned"

            original = clone_repos.clone_one
            clone_repos.clone_one = fake_clone
            try:
                clone_repos.sweep(cfg)
                ledger_path = pathlib.Path(cfg["clone"]["ledger"])
                first = ledger_path.read_bytes()
                clone_repos.sweep(cfg)
                second = ledger_path.read_bytes()
                clone_repos.sweep(cfg)
                third = ledger_path.read_bytes()
            finally:
                clone_repos.clone_one = original

            self.assertEqual(second, third, "ledger is not stable across re-runs")
            payload = json.loads(second)
            self.assertEqual(payload["repos"]["o/repo"]["state"], clone_repos.STATE_OK)
            self.assertEqual(payload["repos"]["o/repo"]["attempts"], 1,
                             "a present repo was re-attempted")
            self.assertEqual(payload["summary"]["pairable_repos_cached"], 1)
            self.assertIn(b'"action"', first)

    def test_failure_is_recorded_and_skipped_not_fatal(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/bad"), _instance("b", "o/bad"),
                                _instance("c", "o/good"), _instance("d", "o/good")])

            def fake_clone(config, repo, dest):
                if repo == "o/bad":
                    return False, "timeout after 5s"
                _make_repo(dest)
                return True, "cloned"

            original = clone_repos.clone_one
            clone_repos.clone_one = fake_clone
            try:
                result = clone_repos.sweep(cfg)
            finally:
                clone_repos.clone_one = original

            self.assertEqual(result["repos"]["o/bad"]["state"], clone_repos.STATE_FAILED)
            self.assertEqual(result["repos"]["o/bad"]["error"], "timeout after 5s")
            self.assertEqual(result["repos"]["o/good"]["state"], clone_repos.STATE_OK)
            self.assertEqual(result["summary"]["failed"], 1)
            self.assertIn("o/bad", result["_run"]["failed"])

    def test_failed_repo_is_retried_on_the_next_sweep(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/flaky"), _instance("b", "o/flaky")])
            state = {"calls": 0}

            def fake_clone(config, repo, dest):
                state["calls"] += 1
                if state["calls"] == 1:
                    return False, "transient"
                _make_repo(dest)
                return True, "cloned"

            original = clone_repos.clone_one
            clone_repos.clone_one = fake_clone
            try:
                clone_repos.sweep(cfg)
                result = clone_repos.sweep(cfg)
            finally:
                clone_repos.clone_one = original

            self.assertEqual(state["calls"], 2)
            self.assertEqual(result["repos"]["o/flaky"]["state"], clone_repos.STATE_OK)
            self.assertEqual(result["repos"]["o/flaky"]["attempts"], 2)
            self.assertIsNone(result["repos"]["o/flaky"]["error"])

    def test_corrupt_ledger_does_not_stop_the_sweep(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/repo"), _instance("b", "o/repo")])
            pathlib.Path(cfg["clone"]["ledger"]).write_text("{not json", encoding="utf-8")
            _make_repo(tmp / "cache" / "o_repo")
            result = clone_repos.sweep(cfg)
            self.assertEqual(result["repos"]["o/repo"]["state"], clone_repos.STATE_OK)

    def test_dry_run_writes_no_ledger_and_clones_nothing(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [_instance("a", "o/repo"), _instance("b", "o/repo")])
            attempted = []
            original = clone_repos.clone_one
            clone_repos.clone_one = (
                lambda config, repo, dest: (attempted.append(repo) or (True, "cloned")))
            try:
                clone_repos.sweep(cfg, dry_run=True)
            finally:
                clone_repos.clone_one = original
            self.assertEqual(attempted, [])
            self.assertFalse(pathlib.Path(cfg["clone"]["ledger"]).exists())


class CloneSafetyTest(unittest.TestCase):
    def test_clone_url_uses_the_configured_host(self):
        with tempfile.TemporaryDirectory() as raw:
            cfg = _config(pathlib.Path(raw), [])
            self.assertEqual(clone_repos.clone_url(cfg, "o/r"),
                             "https://github.com/o/r.git")

    def test_failed_clone_leaves_no_staging_dir_behind(self):
        """A half-clone at the final path would fool the ancestry oracle."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, [])
            cfg["clone"]["host"] = "file:///nonexistent-host-for-tests/"
            dest = tmp / "cache" / "o_repo"
            ok, message = clone_repos.clone_one(cfg, "o/repo", dest)
            self.assertFalse(ok)
            self.assertTrue(message)
            self.assertFalse(dest.exists(), "failed clone left the destination behind")
            self.assertFalse((dest.parent / f"{dest.name}.tmp").exists(),
                             "failed clone left a staging dir behind")


if __name__ == "__main__":
    unittest.main()
