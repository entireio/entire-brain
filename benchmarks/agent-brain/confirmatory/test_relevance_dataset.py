import importlib.util
import json
import pathlib
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("relevance_dataset.py")
SPEC = importlib.util.spec_from_file_location("relevance_dataset", MODULE_PATH)
relevance = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(relevance)


HERE = pathlib.Path(__file__).parent
REPO = HERE.parents[2]


class RelevanceDatasetTests(unittest.TestCase):
    def _fixture(self):
        temporary = tempfile.TemporaryDirectory()
        repo = pathlib.Path(temporary.name)
        (repo / "tasks").mkdir()
        config = {
            "id": "task-one",
            "prompt": "Fix the cumulative cache accounting.",
            "rolling_cutoff_rfc3339": "2026-02-01T00:00:00Z",
            "exclude_session_ids": [],
        }
        config_path = repo / "tasks" / "task-one.json"
        config_path.write_text(json.dumps(config))
        inventory = {
            "tasks": [{
                "task_id": "task-one",
                "assigned_split": "development",
                "state": "agent_run",
                "artifacts": {
                    "config_path": "tasks/task-one.json",
                    "config_sha256": relevance.sha256_bytes(config_path.read_bytes()),
                    "prompt_sha256": relevance.sha256_text(config["prompt"]),
                },
            }],
        }
        inventory_path = repo / "inventory.json"
        inventory_path.write_text(json.dumps(inventory))
        facts = [
            {"id": "fact:positive", "kind": "decision", "status": "active", "text": "Accumulate cache usage.",
             "provenance": [{"session_id": "old"}]},
            {"id": "fact:distractor", "kind": "gotcha", "status": "active", "text": "Cache is inclusive elsewhere.",
             "provenance": [{"session_id": "old"}]},
            {"id": "fact:irrelevant", "kind": "decision", "status": "active", "text": "Use another API.",
             "provenance": [{"session_id": "old"}]},
        ]
        snapshot = {
            "schema_version": 1,
            "source_facts_sha256": "a" * 64,
            "source_session_dates_sha256": "b" * 64,
            "fact_count": len(facts),
            "facts": facts,
            "session_dates": {"old": "2026-01-01T00:00:00Z"},
        }
        snapshot_path = repo / "snapshot.json"
        snapshot_path.write_text(json.dumps(snapshot))
        evidence = {
            "type": "manual_frozen_corpus_review",
            "source_id": "fixture",
            "rationale": "Reviewed the fixture.",
        }
        labels = {
            "schema_version": 1,
            "dataset_id": "fixture",
            "created_at": "2026-02-02T00:00:00Z",
            "source_corpus": {"facts_sha256": "a" * 64, "session_dates_sha256": "b" * 64},
            "allowed_inventory_states": list(relevance.SAFE_STATES),
            "items": [
                {
                    "query_id": "product",
                    "task_id": "task-one",
                    "query_source": "user_prompt_derived",
                    "null_query": False,
                    "label_evidence": evidence,
                    "judgments": [
                        {"fact_id": "fact:positive", "grade": "solving", "cluster_id": "positive", "rationale": "Direct."},
                        {"fact_id": "fact:distractor", "grade": "hard_topical_distractor", "cluster_id": "distractor", "rationale": "Wrong format."},
                        {"fact_id": "fact:irrelevant", "grade": "irrelevant", "cluster_id": "irrelevant", "rationale": "Unrelated."},
                    ],
                },
                {
                    "query_id": "oracle",
                    "task_id": "task-one",
                    "query_source": "oracle_upper_bound",
                    "query_text": "Which cache usage should accumulate?",
                    "null_query": False,
                    "label_evidence": evidence,
                    "judgments": [
                        {"fact_id": "fact:positive", "grade": "relevant_alternative", "cluster_id": "positive", "rationale": "Useful."},
                        {"fact_id": "fact:distractor", "grade": "hard_topical_distractor", "cluster_id": "distractor", "rationale": "Wrong format."},
                    ],
                },
                {
                    "query_id": "null",
                    "task_id": "task-one",
                    "query_source": "oracle_upper_bound",
                    "query_text": "No answer exists for this synthetic query.",
                    "null_query": True,
                    "label_evidence": evidence,
                    "judgments": [
                        {"fact_id": "fact:distractor", "grade": "hard_topical_distractor", "cluster_id": "distractor", "rationale": "Topical only."},
                    ],
                },
            ],
        }
        labels_path = repo / "labels.json"
        labels_path.write_text(json.dumps(labels))
        return temporary, repo, labels_path, inventory_path, snapshot_path

    def test_canonical_fact_hash_ignores_object_key_order(self):
        left = {"id": "fact:x", "text": "value"}
        right = {"text": "value", "id": "fact:x"}
        self.assertEqual(relevance.fact_sha256(left), relevance.fact_sha256(right))

    def test_materialize_derives_hashes_and_temporal_eligibility(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        result = relevance.materialize(repo, labels, inventory, snapshot)
        self.assertEqual(result["development"]["item_count"], 3)
        self.assertEqual(result["development"]["unique_task_count"], 1)
        product = result["development"]["items"][0]
        self.assertEqual(product["query_sha256"], relevance.sha256_text(product["query_text"]))
        self.assertTrue(product["judgments"][0]["eligible"])
        self.assertRegex(product["judgments"][0]["fact_sha256"], r"^[0-9a-f]{64}$")

    def test_materialize_rejects_unexposed_or_nondevelopment_task(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(inventory.read_text())
        payload["tasks"][0]["state"] = "unseen"
        inventory.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "not exposed development"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_materialize_rejects_stale_inventory_config_hash(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        (repo / "tasks" / "task-one.json").write_text("{}")
        with self.assertRaisesRegex(relevance.DatasetError, "config hash differs"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_materialize_does_not_read_unselected_unseen_config(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(inventory.read_text())
        payload["tasks"].append({
            "task_id": "sealed-task",
            "assigned_split": "confirmatory",
            "state": "unseen",
            "artifacts": {
                "config_path": "tasks/must-not-be-read.json",
                "config_sha256": "c" * 64,
                "prompt_sha256": "d" * 64,
            },
        })
        inventory.write_text(json.dumps(payload))
        result = relevance.materialize(repo, labels, inventory, snapshot)
        self.assertEqual(result["development"]["unique_task_count"], 1)

    def test_materialize_rejects_positive_fact_at_or_after_cutoff(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["session_dates"]["old"] = "2026-02-01T00:00:00Z"
        snapshot.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "answerable query has no eligible positive"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_materialize_rejects_superseded_fact(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["facts"][0]["status"] = "superseded"
        snapshot.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "only active facts may be judged"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_materialize_rejects_product_query_override(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        payload["items"][0]["query_text"] = "hand tuned"
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "derive exactly from task prompt"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_null_query_cannot_have_eligible_positive(self):
        temporary, repo, labels, inventory, snapshot = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        payload["items"][2]["judgments"][0]["grade"] = "solving"
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "null query has eligible positive"):
            relevance.materialize(repo, labels, inventory, snapshot)

    def test_checked_in_dataset_rebuilds_and_keeps_holdout_unopened(self):
        labels = HERE / "offline-relevance-development-labels.json"
        inventory = HERE / "task-inventory.json"
        snapshot = HERE / "offline-relevance-fact-snapshot.json"
        dataset = HERE / "offline-relevance-dataset.json"
        expected = relevance.materialize(REPO, labels, inventory, snapshot)
        actual = relevance.load_json(dataset)
        relevance.validate_dataset(actual, expected)
        self.assertEqual(actual["development"]["item_count"], 13)
        self.assertEqual(actual["development"]["unique_task_count"], 12)
        self.assertEqual(actual["sealed_holdout"]["items"], [])


if __name__ == "__main__":
    unittest.main()
