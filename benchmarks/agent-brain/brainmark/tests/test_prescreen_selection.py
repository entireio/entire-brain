"""seal_prescreen.select_slate / propose_dev_split -- determinism and the caps.

The slate decides which pairs two humans spend an afternoon judging, and it is
the one step of the seal that no later hash can catch: a biased slate produces a
correctly-sealed, correctly-hashed, quietly unrepresentative task set. So the
properties are pinned here rather than eyeballed once in a summary table:

  * byte-stable output under input reordering and PYTHONHASHSEED,
  * the per-repo cap holds against the ACHIEVED slate, not the requested one,
    and is never relaxed to reach the target,
  * near-duplicate A/B instances are capped,
  * every candidate is accounted for, with a rationale code from the fixed
    vocabulary -- selected and excluded alike.

Offline, no network, no paid calls, no filesystem dependency (the selector is a
pure function; the fixtures are synthesized in-process).
"""

from __future__ import annotations

import collections
import pathlib
import random
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import seal_prescreen  # noqa: E402


def entry(pair_id: str, repo: str, score: float, *, a_id: str | None = None,
          b_id: str | None = None, pool: str = "swe_bench", language: str = "Python",
          overlap: float = 0.1, flagged: bool = False) -> dict:
    return {
        "pair_id": pair_id,
        "repo": repo,
        "score": score,
        "file_jaccard": score / 2,
        "symbol_overlap": score / 2,
        "patch_body_overlap": overlap,
        "needs_human_review": flagged,
        "shared_files": ["src/m.py"],
        "shared_symbols": [],
        "a_id": a_id or f"{pair_id}-A",
        "b_id": b_id or f"{pair_id}-B",
        "pool": pool,
        "language": language,
        "language_source": "instance",
        "a_language": language,
        "a_language_source": "instance",
        "stratum": f"{pool}/{language}",
        "instances_resolved": True,
        "_candidate": {},
    }


def synth_pool(n_repos: int = 12, per_repo: int = 40, langs: tuple[str, ...] = ("Python",),
               pools: tuple[str, ...] = ("swe_bench", "local")) -> list[dict]:
    """A candidate pool with a deliberately uneven shape.

    Repo 0 gets 4x the mass of the rest, which is what the real pool looks like
    (django/django was 701 of 1806) and what the repo cap exists to contain.
    """
    out: list[dict] = []
    for r in range(n_repos):
        count = per_repo * 4 if r == 0 else per_repo
        lang = langs[r % len(langs)]
        pool = pools[r % len(pools)]
        for i in range(count):
            # Index-major scoring: the i-th pair of every repo scores about the
            # same, so rank order interleaves repositories the way the real
            # candidate pool does. Repo-major scores would make "rank order"
            # and "repo order" the same thing and hide every diversity bug.
            out.append(entry(
                f"r{r:02d}-p{i:03d}", f"owner/repo{r:02d}",
                round(2.0 - (i * 0.01) - (r * 0.0001), 6),
                pool=pool, language=lang,
            ))
    return out


DEFAULTS = dict(slate_size=100, repo_cap_frac=0.12, dup_cap=2,
                stratum_floor=2, stratum_cap_frac=0.40)


class DeterminismTest(unittest.TestCase):
    def test_input_order_cannot_move_the_slate(self):
        pool = synth_pool()
        shuffled = list(pool)
        random.Random(20260815).shuffle(shuffled)
        first = seal_prescreen.select_slate(pool, **DEFAULTS)
        second = seal_prescreen.select_slate(shuffled, **DEFAULTS)
        self.assertEqual(
            [(e["pair_id"], e["rationale"], e["rank"]) for e in first["slate"]],
            [(e["pair_id"], e["rationale"], e["rank"]) for e in second["slate"]],
        )
        self.assertEqual([e["pair_id"] for e in first["excluded"]],
                         [e["pair_id"] for e in second["excluded"]])

    def test_repeated_calls_are_identical(self):
        pool = synth_pool()
        a = seal_prescreen.select_slate(pool, **DEFAULTS)
        b = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertEqual([e["pair_id"] for e in a["slate"]],
                         [e["pair_id"] for e in b["slate"]])
        self.assertEqual(a["relaxations"], b["relaxations"])

    def test_slate_is_returned_in_rank_order_with_dense_ranks(self):
        result = seal_prescreen.select_slate(synth_pool(), **DEFAULTS)
        slate = result["slate"]
        self.assertEqual([e["rank"] for e in slate], list(range(1, len(slate) + 1)))
        keys = [seal_prescreen.rank_key(e) for e in slate]
        self.assertEqual(keys, sorted(keys))


class CapTest(unittest.TestCase):
    def test_no_repo_exceeds_the_share_cap_of_the_achieved_slate(self):
        result = seal_prescreen.select_slate(synth_pool(), **DEFAULTS)
        slate = result["slate"]
        counts = collections.Counter(e["repo"] for e in slate)
        cap = max(1, int(len(slate) * DEFAULTS["repo_cap_frac"]))
        self.assertLessEqual(max(counts.values()), cap)
        self.assertLessEqual(max(counts.values()) / len(slate),
                             DEFAULTS["repo_cap_frac"] + 1e-9)

    def test_repo_cap_is_not_relaxed_to_reach_the_target(self):
        # Everything in ONE repo: no non-empty slate can be <= 12% one repo, so
        # the cap is INFEASIBLE. The slate must come out short (the cap held
        # during selection) and the residual breach must be REPORTED, not fixed
        # by collapsing the slate to a single pair.
        pool = [entry(f"only-p{i:03d}", "owner/only", 1.0 - i * 0.001) for i in range(400)]
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertTrue(result["slate_short"])
        self.assertEqual(len(result["slate"]), result["repo_cap"])  # 12, not 100
        self.assertFalse(result["repo_cap_respected"])
        self.assertEqual(result["repo_cap_violations"], ["owner/only"])

    def test_short_slate_retightens_the_repo_cap_when_that_is_feasible(self):
        # 10 repos: repo `a` can supply 20 pairs, the other nine 5 each. The
        # requested-size cap (12 of 100) yields 12 + 45 = 57, where 12/57 = 21%
        # breaches the cap. Retightening to the largest feasible size is possible
        # here, so it must happen -- and must NOT collapse the slate.
        pool = [entry(f"a-p{i:02d}", "owner/a", 1.0 - i * 0.001) for i in range(20)]
        for r in range(1, 10):
            pool += [entry(f"r{r}-p{i}", f"owner/r{r}", 0.9 - r * 0.01 - i * 0.001)
                     for i in range(5)]
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        slate = result["slate"]
        counts = collections.Counter(e["repo"] for e in slate)
        self.assertTrue(result["repo_cap_respected"], result["repo_cap_violations"])
        self.assertLessEqual(max(counts.values()) / len(slate),
                             DEFAULTS["repo_cap_frac"] + 1e-9)
        self.assertGreaterEqual(len(slate), 45)  # the trim did not collapse it
        codes = {c for e in result["excluded"] for c in e["rationale"]}
        self.assertIn("DROP_REPO_CAP_FINAL", codes)
        self.assertTrue(any(r.startswith("REPO_CAP_RETIGHTENED")
                            for r in result["relaxations"]))

    def test_max_compliant_size_is_zero_when_the_cap_cannot_be_met(self):
        self.assertEqual(seal_prescreen._max_compliant_size([30], 0.12, 30), 0)
        self.assertEqual(seal_prescreen._max_compliant_size([30, 30], 0.12, 60), 0)
        # Ten repos of five can host a 50-pair slate at 12% (cap 6 each).
        self.assertEqual(seal_prescreen._max_compliant_size([5] * 10, 0.12, 50), 50)

    def test_duplicate_a_and_b_instances_are_capped(self):
        pool = []
        for i in range(40):
            pool.append(entry(f"dup-{i:02d}", "owner/x", 1.0 - i * 0.001,
                              a_id="shared-A", b_id=f"b-{i:02d}"))
        for i in range(40):
            pool.append(entry(f"dupb-{i:02d}", "owner/y", 0.9 - i * 0.001,
                              a_id=f"a-{i:02d}", b_id="shared-B"))
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        a_counts = collections.Counter(e["a_id"] for e in result["slate"])
        b_counts = collections.Counter(e["b_id"] for e in result["slate"])
        self.assertLessEqual(a_counts["shared-A"], DEFAULTS["dup_cap"])
        self.assertLessEqual(b_counts["shared-B"], DEFAULTS["dup_cap"])
        codes = collections.Counter(
            c for e in result["excluded"] for c in e["rationale"])
        self.assertGreater(codes["DROP_DUP_A"], 0)
        self.assertGreater(codes["DROP_DUP_B"], 0)

    def test_dup_cap_of_one_admits_each_instance_once(self):
        pool = [entry(f"p{i:02d}", "owner/x", 1.0 - i * 0.001,
                      a_id="A", b_id=f"b{i}") for i in range(10)]
        result = seal_prescreen.select_slate(
            pool, **{**DEFAULTS, "dup_cap": 1, "slate_size": 10})
        self.assertEqual(sum(1 for e in result["slate"] if e["a_id"] == "A"), 1)


class AccountingTest(unittest.TestCase):
    def test_every_candidate_is_accounted_for_exactly_once(self):
        pool = synth_pool()
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        seen = [e["pair_id"] for e in result["slate"]] + \
               [e["pair_id"] for e in result["excluded"]]
        self.assertEqual(sorted(seen), sorted(e["pair_id"] for e in pool))
        self.assertEqual(len(seen), len(set(seen)))

    def test_accounting_holds_through_the_repo_cap_retighten_path(self):
        # Regression: the trim used to pop trimmed pairs out of the `chosen` map,
        # so step 6 re-classified them and each appeared TWICE in `excluded` --
        # once as DROP_REPO_CAP_FINAL and once as DROP_RANK.
        pool = [entry(f"a-p{i:02d}", "owner/a", 1.0 - i * 0.001) for i in range(20)]
        for r in range(1, 10):
            pool += [entry(f"r{r}-p{i}", f"owner/r{r}", 0.9 - r * 0.01 - i * 0.001)
                     for i in range(5)]
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertTrue(any(c == "DROP_REPO_CAP_FINAL"
                            for e in result["excluded"] for c in e["rationale"]))
        seen = [e["pair_id"] for e in result["slate"]] + \
               [e["pair_id"] for e in result["excluded"]]
        self.assertEqual(len(seen), len(set(seen)))
        self.assertEqual(sorted(seen), sorted(e["pair_id"] for e in pool))

    def test_every_rationale_code_is_in_the_fixed_vocabulary(self):
        result = seal_prescreen.select_slate(synth_pool(), **DEFAULTS)
        for group in ("slate", "excluded"):
            for e in result[group]:
                self.assertTrue(e["rationale"], f"{e['pair_id']} has no rationale")
                for code in e["rationale"]:
                    self.assertIn(code, seal_prescreen.RATIONALE_CODES)

    def test_top_ranked_candidate_is_always_slated(self):
        pool = synth_pool()
        best = min(pool, key=seal_prescreen.rank_key)
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertIn(best["pair_id"], {e["pair_id"] for e in result["slate"]})


class StratumTest(unittest.TestCase):
    def test_small_stratum_gets_its_floor(self):
        # 400 Python pairs outrank 3 Ruby pairs on every score; without the floor
        # pass the Ruby stratum would never be reviewed at all.
        pool = [entry(f"py-{i:03d}", f"owner/py{i % 20:02d}", 2.0 - i * 0.001)
                for i in range(400)]
        pool += [entry(f"rb-{i}", "owner/rb", 0.01 + i * 0.001,
                       pool="local", language="Ruby") for i in range(3)]
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        ruby = [e for e in result["slate"] if e["language"] == "Ruby"]
        self.assertGreaterEqual(len(ruby), DEFAULTS["stratum_floor"])
        by_floor = [e for e in ruby if e["rationale"] == ["SEL_STRATUM_FLOOR"]]
        self.assertGreaterEqual(len(by_floor), DEFAULTS["stratum_floor"])
        # Without the floor pass, rank order alone would never reach them.
        self.assertTrue(all(e["rank"] > DEFAULTS["stratum_floor"] for e in by_floor))

    def test_stratum_ceiling_relaxes_only_when_short_and_says_so(self):
        # One stratum, ceiling 40 of a 100 slate: the slate cannot fill without
        # relaxing, and the relaxation must be recorded, not silent.
        pool = [entry(f"p-{i:03d}", f"owner/r{i % 20:02d}", 2.0 - i * 0.001)
                for i in range(300)]
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertEqual(len(result["slate"]), DEFAULTS["slate_size"])
        self.assertTrue(result["relaxations"])
        self.assertIn("STRATUM_CAP_RELAXED", result["relaxations"][0])
        codes = {c for e in result["slate"] for c in e["rationale"]}
        self.assertIn("SEL_RELAX_STRATUM", codes)

    def test_no_relaxation_when_the_slate_fills_within_both_caps(self):
        pool = synth_pool(n_repos=20, per_repo=20,
                          langs=("Python", "Go", "Rust", "Java"))
        result = seal_prescreen.select_slate(pool, **DEFAULTS)
        self.assertEqual(len(result["slate"]), DEFAULTS["slate_size"])
        self.assertEqual(result["relaxations"], [])


class DevSplitTest(unittest.TestCase):
    def _slate(self):
        pool = synth_pool(n_repos=24, per_repo=20,
                          langs=("Python", "Go", "Rust", "Java", "Ruby", "PHP"))
        return seal_prescreen.select_slate(pool, **{**DEFAULTS, "slate_size": 200})["slate"]

    def test_dev_split_is_a_subset_of_the_slate_of_the_requested_size(self):
        slate = self._slate()
        dev = seal_prescreen.propose_dev_split(slate, 20)
        self.assertEqual(len(dev), 20)
        self.assertEqual(len(set(dev)), 20)
        self.assertTrue(set(dev) <= {e["pair_id"] for e in slate})

    def test_dev_split_spans_repos_and_languages(self):
        slate = self._slate()
        dev = set(seal_prescreen.propose_dev_split(slate, 20))
        picked = [e for e in slate if e["pair_id"] in dev]
        self.assertEqual(len({e["repo"] for e in picked}), 20)
        self.assertGreaterEqual(len({e["language"] for e in picked}), 6)

    def test_dev_split_is_deterministic(self):
        slate = self._slate()
        self.assertEqual(seal_prescreen.propose_dev_split(slate, 20),
                         seal_prescreen.propose_dev_split(slate, 20))

    def test_dev_split_relaxes_repo_uniqueness_rather_than_coming_up_short(self):
        # Three repos, 20 requested: one-per-repo is impossible, so the proposal
        # must fall back to 2-per-repo and then to none, still returning 20.
        pool = [entry(f"p-{i:03d}", f"owner/r{i % 3}", 2.0 - i * 0.001) for i in range(60)]
        slate = seal_prescreen.select_slate(
            pool, **{**DEFAULTS, "slate_size": 60, "repo_cap_frac": 1.0})["slate"]
        dev = seal_prescreen.propose_dev_split(slate, 20)
        self.assertEqual(len(dev), 20)

    def test_dev_split_of_zero_or_empty_slate_is_empty(self):
        self.assertEqual(seal_prescreen.propose_dev_split([], 20), [])
        self.assertEqual(seal_prescreen.propose_dev_split(self._slate(), 0), [])


class LanguageInferenceTest(unittest.TestCase):
    def test_declared_language_wins(self):
        self.assertEqual(seal_prescreen.language_of({"language": "Go", "pool": "swe_bench"}),
                         ("Go", "instance"))

    def test_python_only_pool_is_inferred_and_labelled(self):
        self.assertEqual(seal_prescreen.language_of({"pool": "swe_bench_verified"}),
                         ("Python", "pool_default"))

    def test_membership_of_a_python_only_pool_is_inferred_and_labelled(self):
        # The miner attributes an instance present in both a graphmark task JSON
        # and SWE-bench to `local`, which declares no language; membership of the
        # Python-only dump is the evidence, and it is labelled as such.
        self.assertEqual(seal_prescreen.language_of({"pool": "local"}, ["swe_bench"]),
                         ("Python", "pool_membership"))

    def test_no_evidence_is_reported_as_unspecified_never_guessed(self):
        self.assertEqual(
            seal_prescreen.language_of({"pool": "local"}, ["swe_bench_multilingual"]),
            ("unspecified", "absent"))


class GuardTest(unittest.TestCase):
    def test_rejects_a_dup_cap_below_one(self):
        with self.assertRaises(ValueError):
            seal_prescreen.select_slate([], **{**DEFAULTS, "dup_cap": 0})

    def test_rejects_a_negative_slate_size(self):
        with self.assertRaises(ValueError):
            seal_prescreen.select_slate([], **{**DEFAULTS, "slate_size": -1})

    def test_empty_pool_yields_an_empty_slate(self):
        result = seal_prescreen.select_slate([], **DEFAULTS)
        self.assertEqual(result["slate"], [])
        self.assertEqual(result["excluded"], [])


if __name__ == "__main__":
    unittest.main()
