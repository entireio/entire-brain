import importlib.util
import json
import pathlib
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("ranking_diagnostics.py")
SPEC = importlib.util.spec_from_file_location("ranking_diagnostics", MODULE_PATH)
ranking = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(ranking)


class RankingDiagnosticsTests(unittest.TestCase):
    def test_closed_negative_boost_reports_rank_and_top_k_effect(self):
        result = ranking.closed_negative_ablation(
            [{
                "query_id": "q",
                "facts": [
                    {"id": "newer", "kind": "convention", "lexical_score_before_kind_boost": 240,
                     "updated_at": "2026-06-10T13:00:00Z"},
                    {"id": "negative", "kind": "closed-negative", "lexical_score_before_kind_boost": 240,
                     "updated_at": "2026-06-10T12:00:00Z"},
                ],
            }],
            boost=30,
            top_k=1,
        )
        effect = result["queries"][0]["closed_negative_effects"][0]
        self.assertEqual(effect["rank_without_boost_1based"], 2)
        self.assertEqual(effect["rank_with_boost_1based"], 1)
        self.assertEqual(effect["rank_improvement"], 1)
        self.assertTrue(effect["top_k_membership_changed"])

    def test_packet_aggregation_separates_best_rank_from_hits_and_clusters(self):
        result = ranking.aggregate_packet(
            [
                {"query_id": "a", "facts": [
                    {"id": "aaa", "text": "Turn end events without ids must be deduped by usage"},
                    {"id": "bbb", "text": "Cursor transcript UUIDs prevent checkpoint collapse"},
                ]},
                {"query_id": "b", "facts": [
                    {"id": "bbb", "text": "Cursor transcript UUIDs prevent checkpoint collapse"},
                    {"id": "ccc", "text": "Turn end events without ids must be deduped by usage value"},
                ]},
            ],
            cap=3,
            cluster_threshold=0.8,
        )
        self.assertEqual([row["fact_id"] for row in result["delivered"]], ["bbb", "aaa", "ccc"])
        self.assertEqual(result["delivered"][0]["best_rank_1based"], 1)
        self.assertEqual(result["delivered"][0]["hit_count"], 2)
        self.assertEqual(result["near_duplicate_clusters"]["occupied_slots"], 2)
        self.assertAlmostEqual(result["near_duplicate_clusters"]["occupancy_fraction"], 2 / 3)

    def test_checked_in_fixture_is_valid_and_non_mutating(self):
        fixture = pathlib.Path(__file__).with_name("fixtures") / "ranking-baseline-input-v1.json"
        result = ranking.analyze(json.loads(fixture.read_text()))
        self.assertEqual(result["schema_version"], ranking.OUTPUT_SCHEMA)
        self.assertFalse(result["production_behavior_changed"])


if __name__ == "__main__":
    unittest.main()
