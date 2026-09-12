"""HF pool loaders: normalization, offline read, merge order, pool-mode mining.

Every test here is OFFLINE and runs without the `datasets` package installed --
`datasets` is needed to download a pool and for nothing else. The fixtures are
canned instance JSON in the exact shape `pool_loaders.download_pool` writes.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, mine_pairs, pool_loaders  # noqa: E402


# --------------------------------------------------------------------------
# fixtures
# --------------------------------------------------------------------------


def _patch(path: str, added: list[str], hunk_symbol: str = "def handler") -> str:
    return "\n".join([
        f"diff --git a/{path} b/{path}",
        "index 111..222 100644",
        f"--- a/{path}",
        f"+++ b/{path}",
        f"@@ -1,3 +1,4 @@ {hunk_symbol}",
        *[f"+{line}" for line in added],
    ]) + "\n"


def _hf_row(iid: str, repo: str, commit: str, created: str, patch: str,
            f2p: str = '["tests/test_x.py::test_one"]') -> dict:
    """A raw HF row: FAIL_TO_PASS/PASS_TO_PASS arrive as JSON *strings*."""
    return {
        "instance_id": iid,
        "repo": repo,
        "base_commit": commit,
        "created_at": created,
        "patch": patch,
        "test_patch": _patch("tests/test_x.py", ["assert thing_works()"]),
        "problem_statement": f"fix {iid}",
        "hints_text": "",
        "version": "1.0",
        "environment_setup_commit": commit,
        "FAIL_TO_PASS": f2p,
        "PASS_TO_PASS": "[]",
    }


class FakeOracle(mine_pairs.AncestryOracle):
    """Every commit exists; listed-earlier is always the ancestor."""

    def __init__(self, verdict: bool | None = True) -> None:
        super().__init__(pathlib.Path("/nonexistent"))
        self._verdict = verdict

    def repo_available(self, repo: str) -> bool:
        return self._verdict is not None

    def has_commit(self, repo: str, commit: str) -> bool:
        return self._verdict is not None

    def is_ancestor(self, repo: str, older: str, newer: str) -> bool | None:
        return self._verdict


def _write_pool_cache(cache: pathlib.Path, name: str, dataset_id: str,
                      revision: str, rows: list[dict]) -> None:
    cache.mkdir(parents=True, exist_ok=True)
    instances = pool_loaders.normalize_rows(rows, name)
    doc = pool_loaders.pool_document(
        name, {"hf_id": dataset_id, "split": "test"}, revision, instances)
    (cache / f"{name}.json").write_text(_harness.pretty_json(doc), encoding="utf-8")
    (cache / f"{name}.meta.json").write_text(_harness.pretty_json({
        "pool": name, "dataset_id": dataset_id, "revision": revision,
        "count": len(instances),
    }), encoding="utf-8")


def _config(tmp: pathlib.Path, registry: dict, local_instances: list[dict] | None = None) -> dict:
    (tmp / "tasks").mkdir(parents=True, exist_ok=True)
    (tmp / "tasks" / "t.json").write_text(
        json.dumps({"instances": local_instances or []}), encoding="utf-8")
    return {
        "graphmark_root": str(tmp),
        "repo_cache": str(tmp / "cache"),
        "task_globs": ["tasks/*.json"],
        "pools": {"cache_dir": str(tmp / "pools"), "registry": registry},
        "mining": {
            "min_shared_files": 1, "leakage_overlap_max": 0.8,
            "leakage_borderline_min": 0.6, "require_ancestor": True,
            "one_a_per_b": True, "target_candidates": 1,
            "score_weights": {"file_jaccard": 1.0, "symbol_overlap": 1.0},
        },
        "_config_sha256": "test",
    }


# --------------------------------------------------------------------------
# normalization
# --------------------------------------------------------------------------


class NormalizationTest(unittest.TestCase):
    def test_json_string_test_lists_are_decoded(self):
        row = _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00",
                      _patch("src/m.py", ["x = 1"]),
                      f2p='["tests/a.py::t1", "tests/a.py::t2"]')
        inst = pool_loaders.normalize_row(row, "p")
        self.assertEqual(inst["FAIL_TO_PASS"], ["tests/a.py::t1", "tests/a.py::t2"])
        self.assertEqual(inst["PASS_TO_PASS"], [])

    def test_already_decoded_lists_survive(self):
        row = _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"]))
        row["FAIL_TO_PASS"] = ["tests/a.py::t1"]
        self.assertEqual(pool_loaders.normalize_row(row, "p")["FAIL_TO_PASS"],
                         ["tests/a.py::t1"])

    def test_unusable_rows_are_dropped_not_defaulted(self):
        """A row with no patch cannot be paired or graded.

        Emitting it with patch="" would silently become "no shared files" and
        make the gate statistics lie about why it was rejected.
        """
        for missing in ("instance_id", "repo", "base_commit", "patch"):
            row = _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00",
                          _patch("src/m.py", ["x = 1"]))
            row[missing] = ""
            self.assertIsNone(pool_loaders.normalize_row(row, "p"), missing)

    def test_pool_tag_is_recorded_on_every_instance(self):
        inst = pool_loaders.normalize_row(
            _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00",
                    _patch("src/m.py", ["x = 1"])), "swe_bench_verified")
        self.assertEqual(inst["pool"], "swe_bench_verified")

    def test_normalize_rows_is_row_order_independent(self):
        rows = [
            _hf_row("r__b", "o/r", "c2", "2021-01-01 00:00:00", _patch("src/m.py", ["y = 2"])),
            _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"])),
        ]
        forward = pool_loaders.normalize_rows(rows, "p")
        backward = pool_loaders.normalize_rows(list(reversed(rows)), "p")
        self.assertEqual(forward, backward)
        self.assertEqual([i["instance_id"] for i in forward], ["r__a", "r__b"])

    def test_duplicate_instance_id_first_wins(self):
        first = _hf_row("dup", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"]))
        second = _hf_row("dup", "o/r", "c9", "2029-01-01 00:00:00", _patch("src/m.py", ["z = 9"]))
        out = pool_loaders.normalize_rows([first, second], "p")
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["base_commit"], "c1")


# --------------------------------------------------------------------------
# offline read + merge order
# --------------------------------------------------------------------------


class OfflineLoadTest(unittest.TestCase):
    def test_load_pool_needs_no_datasets_package(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, {"p1": {"hf_id": "x/y", "split": "test",
                                       "revision": "rev1", "priority": 0, "enabled": True}})
            _write_pool_cache(tmp / "pools", "p1", "x/y", "rev1", [
                _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"]))])
            # Poison the import so a hidden `import datasets` would explode.
            saved = sys.modules.get("datasets")
            sys.modules["datasets"] = None  # type: ignore[assignment]
            try:
                pool, prov = pool_loaders.load_pool(cfg, "p1")
            finally:
                if saved is None:
                    sys.modules.pop("datasets", None)
                else:
                    sys.modules["datasets"] = saved
            self.assertEqual(list(pool), ["r__a"])
            self.assertEqual(prov["revision"], "rev1")
            self.assertEqual(prov["dataset_id"], "x/y")

    def test_missing_cache_raises_and_never_returns_empty(self):
        """A silently empty pool would read as 'no candidates', not 'no data'."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, {"p1": {"hf_id": "x/y", "split": "test",
                                       "revision": None, "priority": 0, "enabled": True}})
            with self.assertRaises(pool_loaders.PoolError):
                pool_loaders.load_pool(cfg, "p1")

    def test_unknown_pool_name_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            cfg = _config(pathlib.Path(raw), {})
            with self.assertRaises(pool_loaders.PoolError):
                pool_loaders.sort_pools(cfg, ["nope"])

    def test_merge_order_is_priority_not_flag_order(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, {
                "hi": {"hf_id": "x/hi", "split": "test", "revision": "r1",
                       "priority": 0, "enabled": True},
                "lo": {"hf_id": "x/lo", "split": "test", "revision": "r2",
                       "priority": 5, "enabled": True},
            })
            shared = "dup__1"
            _write_pool_cache(tmp / "pools", "hi", "x/hi", "r1", [
                _hf_row(shared, "o/r", "aaa", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"]))])
            _write_pool_cache(tmp / "pools", "lo", "x/lo", "r2", [
                _hf_row(shared, "o/r", "zzz", "2029-01-01 00:00:00", _patch("src/m.py", ["z = 9"]))])

            forward, prov_f = pool_loaders.load_pools(cfg, ["hi", "lo"])
            backward, prov_b = pool_loaders.load_pools(cfg, ["lo", "hi"])
            self.assertEqual(forward, backward)
            self.assertEqual([p["pool"] for p in prov_f], [p["pool"] for p in prov_b])
            # priority 0 wins the shared instance_id
            self.assertEqual(forward[shared]["base_commit"], "aaa")

    def test_pool_revisions_block_for_the_index(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = _config(tmp, {"p1": {"hf_id": "x/y", "split": "test",
                                       "revision": "rev1", "priority": 0, "enabled": True}})
            _write_pool_cache(tmp / "pools", "p1", "x/y", "rev1", [
                _hf_row("r__a", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", ["x = 1"]))])
            _, prov = pool_loaders.load_pools(cfg, ["p1"])
            revisions = pool_loaders.pool_revisions(prov)
            self.assertEqual(revisions["p1"]["revision"], "rev1")
            self.assertEqual(revisions["p1"]["dataset_id"], "x/y")
            self.assertEqual(len(revisions["p1"]["sha256"]), 64)

    def test_enabled_pools_excludes_the_unlicensed_pool(self):
        """SWE-bench_Multimodal must stay out until its license audit clears."""
        config = _harness.load_config()
        registry = pool_loaders.registry(config)
        self.assertIn("swe_bench_multimodal", registry)
        self.assertFalse(registry["swe_bench_multimodal"]["enabled"])
        self.assertIn("LICENSE", registry["swe_bench_multimodal"]["_disabled_reason"].upper())
        self.assertNotIn("swe_bench_multimodal", pool_loaders.enabled_pools(config))

    def test_every_enabled_pool_is_revision_pinned(self):
        """An unpinned pool means mining could drift with the Hub."""
        config = _harness.load_config()
        registry = pool_loaders.registry(config)
        for name in pool_loaders.enabled_pools(config):
            self.assertTrue(registry[name].get("revision"),
                            f"pool {name} has no revision pin")


# --------------------------------------------------------------------------
# pool-mode mining (the determinism contract, extended)
# --------------------------------------------------------------------------


class PoolModeMiningTest(unittest.TestCase):
    def _fixture_config(self, tmp: pathlib.Path) -> dict:
        cfg = _config(
            tmp,
            {
                "verified": {"hf_id": "x/verified", "split": "test", "revision": "revV",
                             "priority": 0, "enabled": True},
                "bulk": {"hf_id": "x/bulk", "split": "test", "revision": "revB",
                         "priority": 2, "enabled": True},
            },
            local_instances=[{
                "instance_id": "local__a", "repo": "o/local", "base_commit": "l1",
                "created_at": "2019-01-01 00:00:00", "problem_statement": "local a",
                "patch": _patch("src/local.py", ["alpha_one = compute_alpha(seed)"]),
                "version": "1", "language": "Python",
            }, {
                "instance_id": "local__b", "repo": "o/local", "base_commit": "l2",
                "created_at": "2019-06-01 00:00:00", "problem_statement": "local b",
                "patch": _patch("src/local.py", ["zeta_two = derive_zeta(other)"]),
                "version": "1", "language": "Python",
            }],
        )
        _write_pool_cache(tmp / "pools", "verified", "x/verified", "revV", [
            _hf_row("v__a", "o/pool", "p1", "2020-01-01 00:00:00",
                    _patch("src/pool.py", ["gamma_three = compute_gamma(seed)"])),
            _hf_row("v__b", "o/pool", "p2", "2021-01-01 00:00:00",
                    _patch("src/pool.py", ["delta_four = derive_delta(other)"])),
        ])
        _write_pool_cache(tmp / "pools", "bulk", "x/bulk", "revB", [
            _hf_row("b__a", "o/bulk", "q1", "2020-02-01 00:00:00",
                    _patch("src/bulk.py", ["epsilon_five = compute_epsilon(seed)"])),
            _hf_row("b__b", "o/bulk", "q2", "2021-02-01 00:00:00",
                    _patch("src/bulk.py", ["theta_six = derive_theta(other)"])),
        ])
        return cfg

    def test_pool_instances_reach_the_miner(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = self._fixture_config(tmp)
            local_only = mine_pairs.mine(cfg, oracle=FakeOracle(True))
            with_pools = mine_pairs.mine(cfg, oracle=FakeOracle(True),
                                         pools=["verified", "bulk"])
            self.assertEqual(local_only["candidate_count"], 1)
            self.assertEqual(with_pools["candidate_count"], 3)
            self.assertEqual(with_pools["pool_size"], 6)

    def test_index_records_every_pool_revision_sha(self):
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = self._fixture_config(tmp)
            result = mine_pairs.mine(cfg, oracle=FakeOracle(True),
                                     pools=["bulk", "verified"])
            self.assertEqual(result["pools"], ["verified", "bulk"])  # priority order
            self.assertEqual(result["pool_revisions"]["verified"]["revision"], "revV")
            self.assertEqual(result["pool_revisions"]["bulk"]["revision"], "revB")
            # and it survives the write, which is what seal.py reads
            mine_pairs.write_candidates(result, tmp / "out")
            index = json.loads((tmp / "out" / "INDEX.json").read_text(encoding="utf-8"))
            self.assertEqual(index["pool_revisions"]["verified"]["revision"], "revV")
            self.assertEqual(index["pools"], ["verified", "bulk"])

    def test_pool_mode_double_run_is_byte_identical(self):
        """MUTATION TARGET: nondeterminism in pool mode must fail this."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = self._fixture_config(tmp)
            one = mine_pairs.mine(cfg, oracle=FakeOracle(True), pools=["verified", "bulk"])
            two = mine_pairs.mine(cfg, oracle=FakeOracle(True), pools=["bulk", "verified"])
            mine_pairs.write_candidates(one, tmp / "one")
            mine_pairs.write_candidates(two, tmp / "two")
            names_one = sorted(p.name for p in (tmp / "one").glob("*.json"))
            names_two = sorted(p.name for p in (tmp / "two").glob("*.json"))
            self.assertEqual(names_one, names_two)
            self.assertGreater(len(names_one), 1)
            for name in names_one:
                self.assertEqual((tmp / "one" / name).read_bytes(),
                                 (tmp / "two" / name).read_bytes(),
                                 f"{name} differed between runs")

    def test_gates_are_unchanged_in_pool_mode(self):
        """The pool flag must add instances, never relax a gate."""
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            cfg = self._fixture_config(tmp)
            unverifiable = mine_pairs.mine(cfg, oracle=FakeOracle(None),
                                           pools=["verified", "bulk"])
            self.assertEqual(unverifiable["candidate_count"], 0)
            self.assertEqual(unverifiable["rejects"]["unverifiable_ancestry"], 3)

            leaky = self._fixture_config(tmp)
            _write_pool_cache(tmp / "pools", "verified", "x/verified", "revV", [
                _hf_row("v__a", "o/pool", "p1", "2020-01-01 00:00:00",
                        _patch("src/pool.py", ["identical_line = same_call(here)"])),
                _hf_row("v__b", "o/pool", "p2", "2021-01-01 00:00:00",
                        _patch("src/pool.py", ["identical_line = same_call(here)"])),
            ])
            result = mine_pairs.mine(leaky, oracle=FakeOracle(True), pools=["verified"])
            self.assertEqual(result["rejects"]["leakage_overlap"], 1)
            self.assertNotIn("v__a__then__v__b",
                             [c["pair_id"] for c in result["candidates"]])


class WriteCandidatesPreserveTest(unittest.TestCase):
    def test_preserve_keeps_the_named_input_file(self):
        with tempfile.TemporaryDirectory() as raw:
            out = pathlib.Path(raw) / "out"
            out.mkdir()
            (out / "SNAPSHOT.json").write_text("{}", encoding="utf-8")
            (out / "stale_pair.json").write_text("{}", encoding="utf-8")
            mine_pairs.write_candidates(
                {"candidates": [], "candidate_count": 0}, out, preserve=("SNAPSHOT.json",))
            self.assertTrue((out / "SNAPSHOT.json").is_file())
            self.assertFalse((out / "stale_pair.json").is_file())

    def test_stale_candidates_are_removed_by_default(self):
        with tempfile.TemporaryDirectory() as raw:
            out = pathlib.Path(raw) / "out"
            out.mkdir()
            (out / "stale_pair.json").write_text("{}", encoding="utf-8")
            mine_pairs.write_candidates({"candidates": [], "candidate_count": 0}, out)
            self.assertFalse((out / "stale_pair.json").is_file())


if __name__ == "__main__":
    unittest.main()
