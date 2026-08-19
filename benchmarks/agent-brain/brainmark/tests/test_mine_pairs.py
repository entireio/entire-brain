"""Miner: determinism, leakage screen, ancestry gate, patch parsing."""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, mine_pairs  # noqa: E402


def _patch(path: str, added: list[str], removed: list[str] | None = None,
           hunk_symbol: str = "def handler") -> str:
    lines = [f"diff --git a/{path} b/{path}", "index 111..222 100644",
             f"--- a/{path}", f"+++ b/{path}", f"@@ -1,3 +1,4 @@ {hunk_symbol}"]
    lines += [f"-{line}" for line in (removed or [])]
    lines += [f"+{line}" for line in added]
    return "\n".join(lines) + "\n"


def _instance(iid: str, repo: str, commit: str, created: str, patch: str) -> dict:
    return {
        "instance_id": iid, "repo": repo, "base_commit": commit,
        "created_at": created, "patch": patch, "problem_statement": f"fix {iid}",
        "version": "1", "language": "Python",
    }


class FakeOracle(mine_pairs.AncestryOracle):
    """Every commit exists and older-listed-first is always an ancestor."""

    def __init__(self, verdict: bool | None = True) -> None:
        super().__init__(pathlib.Path("/nonexistent"))
        self._verdict = verdict

    def repo_available(self, repo: str) -> bool:
        return self._verdict is not None

    def has_commit(self, repo: str, commit: str) -> bool:
        return self._verdict is not None

    def is_ancestor(self, repo: str, older: str, newer: str) -> bool | None:
        return self._verdict


class PatchParsingTest(unittest.TestCase):
    def test_files_and_symbols(self):
        patch = _patch("src/a.py", ["x = 1"], hunk_symbol="def widget_handler")
        self.assertEqual(mine_pairs.patch_files(patch), {"src/a.py"})
        self.assertIn("widget_handler", mine_pairs.patch_symbols(patch))

    def test_body_tokens_exclude_metadata(self):
        patch = _patch("src/verylongpathname.py", ["alpha_token = 1"])
        tokens = mine_pairs.patch_body_tokens(patch)
        self.assertIn("alpha_token", tokens)
        # +++/--- header paths must NOT leak into body tokens, or every same-file
        # pair would look like a leak.
        self.assertNotIn("verylongpathname", tokens)

    def test_overlap_coefficient_is_containment(self):
        small = {"a", "b"}
        big = {"a", "b", "c", "d", "e", "f", "g", "h"}
        self.assertEqual(mine_pairs.overlap_coefficient(small, big), 1.0)
        self.assertLess(mine_pairs.jaccard(small, big), 0.3)


class LeakageScreenTest(unittest.TestCase):
    """MUTATION TARGET: dropping the 0.8 cap must fail this test."""

    def _config(self, tmp: pathlib.Path, overlap_max: float = 0.8) -> dict:
        return {
            "graphmark_root": str(tmp), "repo_cache": str(tmp / "cache"),
            "task_globs": ["tasks/*.json"],
            "mining": {
                "min_shared_files": 1, "leakage_overlap_max": overlap_max,
                "leakage_borderline_min": 0.6, "require_ancestor": True,
                "one_a_per_b": True, "target_candidates": 1,
                "score_weights": {"file_jaccard": 1.0, "symbol_overlap": 1.0},
            },
            "_config_sha256": "test",
        }

    def _write_pool(self, tmp: pathlib.Path, instances: list[dict]) -> None:
        import json

        (tmp / "tasks").mkdir(parents=True, exist_ok=True)
        (tmp / "tasks" / "t.json").write_text(
            json.dumps({"instances": instances}), encoding="utf-8"
        )

    def test_identical_patches_are_rejected_as_leakage(self):
        import tempfile

        body = ["def fix_the_bug(value):", "    return value + 1", "    # careful here"]
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            self._write_pool(tmp, [
                _instance("r__a", "o/r", "c1", "2020-01-01 00:00:00", _patch("src/m.py", body)),
                _instance("r__b", "o/r", "c2", "2021-01-01 00:00:00", _patch("src/m.py", body)),
            ])
            result = mine_pairs.mine(self._config(tmp), oracle=FakeOracle(True))

        # A IS B's fix -> not a second task, a lookup. Must be rejected.
        self.assertEqual(result["candidate_count"], 0, "leaked pair was emitted")
        self.assertEqual(result["rejects"]["leakage_overlap"], 1)

    def test_distinct_patches_on_a_shared_file_survive(self):
        import tempfile

        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            self._write_pool(tmp, [
                _instance("r__a", "o/r", "c1", "2020-01-01 00:00:00",
                          _patch("src/m.py", ["alpha_one = compute_alpha(seed)"])),
                _instance("r__b", "o/r", "c2", "2021-01-01 00:00:00",
                          _patch("src/m.py", ["zeta_two = derive_zeta(other)"])),
            ])
            result = mine_pairs.mine(self._config(tmp), oracle=FakeOracle(True))

        self.assertEqual(result["candidate_count"], 1)
        self.assertLess(result["candidates"][0]["leakage"]["patch_body_overlap"], 0.8)

    def test_unverifiable_ancestry_is_never_emitted(self):
        import tempfile

        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            self._write_pool(tmp, [
                _instance("r__a", "o/r", "c1", "2020-01-01 00:00:00",
                          _patch("src/m.py", ["alpha_one = compute_alpha(seed)"])),
                _instance("r__b", "o/r", "c2", "2021-01-01 00:00:00",
                          _patch("src/m.py", ["zeta_two = derive_zeta(other)"])),
            ])
            result = mine_pairs.mine(self._config(tmp), oracle=FakeOracle(None))

        self.assertEqual(result["candidate_count"], 0)
        self.assertEqual(result["rejects"]["unverifiable_ancestry"], 1)


class DeterminismTest(unittest.TestCase):
    """MUTATION TARGET: any nondeterminism in the miner must fail this."""

    def test_double_run_is_byte_identical(self):
        import tempfile

        config = _harness.load_config()
        result_a = mine_pairs.mine(config)
        result_b = mine_pairs.mine(config)
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            mine_pairs.write_candidates(result_a, tmp / "one")
            mine_pairs.write_candidates(result_b, tmp / "two")
            names_a = sorted(p.name for p in (tmp / "one").glob("*.json"))
            names_b = sorted(p.name for p in (tmp / "two").glob("*.json"))
            self.assertEqual(names_a, names_b)
            for name in names_a:
                self.assertEqual(
                    (tmp / "one" / name).read_bytes(),
                    (tmp / "two" / name).read_bytes(),
                    f"{name} differed between runs",
                )

    def test_real_pool_yields_candidates_with_full_provenance(self):
        config = _harness.load_config()
        result = mine_pairs.mine(config)
        self.assertGreater(result["candidate_count"], 0, "miner found nothing at all")
        for cand in result["candidates"]:
            for key in ("pair_id", "repo", "a", "b", "shared_files", "score", "leakage"):
                self.assertIn(key, cand)
            self.assertTrue(cand["ancestry_verified"])
            self.assertLess(cand["leakage"]["patch_body_overlap"],
                            config["mining"]["leakage_overlap_max"])
            self.assertGreaterEqual(len(cand["shared_files"]), 1)
            self.assertNotEqual(cand["a"]["instance_id"], cand["b"]["instance_id"])
            self.assertLessEqual(cand["a"]["created_at"], cand["b"]["created_at"])


if __name__ == "__main__":
    unittest.main()
