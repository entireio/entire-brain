import copy
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
    @staticmethod
    def _refresh_snapshot_commitment(labels_path, snapshot_path, *, refresh_judgment_hashes=False):
        labels = json.loads(labels_path.read_text())
        snapshot = json.loads(snapshot_path.read_text())
        snapshot["fact_count"] = len(snapshot["facts"])
        snapshot["facts_sha256"] = relevance.sha256_bytes(relevance.canonical_json(snapshot["facts"]))
        snapshot["session_date_count"] = len(snapshot["session_dates"])
        snapshot["session_dates_sha256"] = relevance.sha256_bytes(
            relevance.canonical_json(snapshot["session_dates"])
        )
        labels["snapshot_commitment"] = relevance._snapshot_commitment(snapshot)
        if refresh_judgment_hashes:
            fact_index = {fact["id"]: fact for fact in snapshot["facts"]}
            for item in labels["items"]:
                for judgment in item["judgments"]:
                    judgment["fact_sha256"] = relevance.fact_sha256(fact_index[judgment["fact_id"]])
        labels_path.write_text(json.dumps(labels))
        snapshot_path.write_text(json.dumps(snapshot))

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
        null_config = {
            "id": "task-null",
            "prompt": "No answer exists for this exposed product task.",
            "rolling_cutoff_rfc3339": "2026-02-01T00:00:00Z",
            "exclude_session_ids": [],
        }
        null_config_path = repo / "tasks" / "task-null.json"
        null_config_path.write_text(json.dumps(null_config))
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
            }, {
                "task_id": "task-null",
                "assigned_split": "development",
                "state": "prompt_inspected",
                "artifacts": {
                    "config_path": "tasks/task-null.json",
                    "config_sha256": relevance.sha256_bytes(null_config_path.read_bytes()),
                    "prompt_sha256": relevance.sha256_text(null_config["prompt"]),
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
        session_dates = {"old": "2026-01-01T00:00:00Z"}
        active_catalog_hash = relevance.fact_catalog_sha256(facts)
        snapshot = {
            "schema_version": relevance.SCHEMA_VERSION,
            "source_facts_sha256": "a" * 64,
            "source_session_dates_sha256": "b" * 64,
            "source_active_fact_count": len(facts),
            "source_active_fact_catalog_sha256": active_catalog_hash,
            "fact_count": len(facts),
            "facts_sha256": relevance.sha256_bytes(relevance.canonical_json(facts)),
            "facts": facts,
            "session_date_count": len(session_dates),
            "session_dates_sha256": relevance.sha256_bytes(relevance.canonical_json(session_dates)),
            "session_dates": session_dates,
        }
        snapshot_path = repo / "snapshot.json"
        snapshot_path.write_text(json.dumps(snapshot))
        review_ledger_path = repo / "review-ledger.json"
        review_ledger_path.write_text("fixture retained review bytes\n")
        evidence = {
            "type": "manual_frozen_corpus_review",
            "source_id": "review-ledger.json#fixture",
            "source_path": "review-ledger.json",
            "source_sha256": relevance.sha256_bytes(review_ledger_path.read_bytes()),
            "rationale": "Reviewed the fixture.",
        }
        labels = {
            "schema_version": relevance.SCHEMA_VERSION,
            "dataset_id": "fixture",
            "label_set_id": "fixture-v2",
            "created_at": "2026-02-02T00:00:00Z",
            "source_corpus": {
                "facts_sha256": "a" * 64,
                "session_dates_sha256": "b" * 64,
                "active_fact_count": len(facts),
                "active_fact_catalog_sha256": active_catalog_hash,
            },
            "snapshot_commitment": relevance._snapshot_commitment(snapshot),
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
                    "task_id": "task-null",
                    "query_source": "user_prompt_derived",
                    "null_query": True,
                    "label_evidence": evidence,
                    "judgments": [
                        {"fact_id": "fact:distractor", "grade": "hard_topical_distractor", "cluster_id": "distractor", "rationale": "Topical only."},
                    ],
                },
            ],
        }
        fact_index = {fact["id"]: fact for fact in facts}
        for item in labels["items"]:
            for judgment in item["judgments"]:
                judgment["fact_sha256"] = relevance.fact_sha256(fact_index[judgment["fact_id"]])
        null_item = labels["items"][2]
        membership = relevance.build_source_membership(facts, session_dates, "a" * 64, "b" * 64)
        null_ledger = {
            "schema_version": 1,
            "ledger_id": "fixture-null-review",
            "items": [{
                "query_id": "null",
                "task_id": "task-null",
                "query_sha256": relevance.sha256_text(null_config["prompt"]),
                "temporal_policy_sha256": relevance.temporal_policy_sha256(
                    null_config["rolling_cutoff_rfc3339"], null_config["exclude_session_ids"]
                ),
                "reviewed_at": "2026-02-02T00:00:00Z",
                "reviewer": "fixture-reviewer",
                "method": relevance.NULL_CLOSURE_METHOD,
                "source_facts_sha256": "a" * 64,
                "source_session_dates_sha256": "b" * 64,
                "eligibility_membership_root_sha256": membership["eligibility_membership_root_sha256"],
                "reviewer_assertion": "Every derived active+eligible fixture fact was reviewed.",
                "decisions": [
                    {"fact_id": "fact:positive", "fact_sha256": relevance.fact_sha256(facts[0]), "grade": "irrelevant"},
                    {"fact_id": "fact:distractor", "fact_sha256": relevance.fact_sha256(facts[1]), "grade": "hard_topical_distractor"},
                    {"fact_id": "fact:irrelevant", "fact_sha256": relevance.fact_sha256(facts[2]), "grade": "irrelevant"},
                ],
            }],
        }
        null_ledger_path = repo / "null-review-ledger.json"
        null_ledger_path.write_text(json.dumps(null_ledger))
        null_item["null_review"] = {
            "source_id": "null-review-ledger.json#null",
            "source_path": "null-review-ledger.json",
            "source_sha256": relevance.sha256_bytes(null_ledger_path.read_bytes()),
        }
        labels_path = repo / "labels.json"
        labels_path.write_text(json.dumps(labels))
        runtime = {
            "source_membership": membership,
            "null_review_ledger": null_ledger,
            "null_review_ledger_path": "null-review-ledger.json",
            "null_review_ledger_sha256": relevance.sha256_bytes(null_ledger_path.read_bytes()),
        }
        return temporary, repo, labels_path, inventory_path, snapshot_path, runtime

    def test_canonical_fact_hash_ignores_object_key_order(self):
        left = {"id": "fact:x", "text": "value"}
        right = {"text": "value", "id": "fact:x"}
        self.assertEqual(relevance.fact_sha256(left), relevance.fact_sha256(right))

    def test_materialize_derives_hashes_and_temporal_eligibility(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        result = relevance.materialize(repo, labels, inventory, snapshot, **runtime)
        self.assertEqual(result["development"]["item_count"], 3)
        self.assertEqual(result["development"]["unique_task_count"], 2)
        self.assertEqual(result["development"]["unique_answerable_product_task_count"], 1)
        self.assertEqual(result["development"]["product_null_query_count"], 1)
        product = result["development"]["items"][0]
        self.assertEqual(product["query_sha256"], relevance.sha256_text(product["query_text"]))
        self.assertTrue(product["judgments"][0]["eligible"])
        self.assertRegex(product["judgments"][0]["fact_sha256"], r"^[0-9a-f]{64}$")

    def test_materialize_rejects_unexposed_or_nondevelopment_task(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(inventory.read_text())
        payload["tasks"][0]["state"] = "unseen"
        inventory.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "not exposed development"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_rejects_stale_inventory_config_hash(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        (repo / "tasks" / "task-one.json").write_text("{}")
        with self.assertRaisesRegex(relevance.DatasetError, "config hash differs"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_does_not_read_unselected_unseen_config(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
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
        result = relevance.materialize(repo, labels, inventory, snapshot, **runtime)
        self.assertEqual(result["development"]["unique_task_count"], 2)

    def test_materialize_rejects_positive_fact_at_or_after_cutoff(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["session_dates"]["old"] = "2026-02-01T00:00:00Z"
        snapshot.write_text(json.dumps(payload))
        self._refresh_snapshot_commitment(labels, snapshot)
        with self.assertRaisesRegex(relevance.DatasetError, "answerable query has no eligible positive"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_rejects_superseded_fact(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["facts"][0]["status"] = "superseded"
        snapshot.write_text(json.dumps(payload))
        self._refresh_snapshot_commitment(labels, snapshot, refresh_judgment_hashes=True)
        with self.assertRaisesRegex(relevance.DatasetError, "only active facts may be judged"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_rejects_snapshot_fact_tampering_after_commitment_refresh(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["facts"][0]["text"] = "Tampered reviewed fact text."
        snapshot.write_text(json.dumps(payload))
        self._refresh_snapshot_commitment(labels, snapshot)
        with self.assertRaisesRegex(relevance.DatasetError, "snapshot fact differs from reviewed fact hash"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_full_source_recheck_rejects_mutated_active_catalog(self):
        temporary, _repo, labels, _inventory, snapshot, _runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        label_payload = json.loads(labels.read_text())
        snapshot_payload = json.loads(snapshot.read_text())
        snapshot_payload["facts"][0]["text"] = "Mutated full-source fact."
        with self.assertRaisesRegex(relevance.DatasetError, "active full-corpus fact catalog hash"):
            relevance.build_snapshot(
                label_payload,
                snapshot_payload["facts"],
                snapshot_payload["session_dates"],
                "a" * 64,
                "b" * 64,
            )

    def test_materialize_rejects_snapshot_date_without_reviewed_commitment(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(snapshot.read_text())
        payload["session_dates"]["old"] = "2025-12-01T00:00:00Z"
        snapshot.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "session_dates commitment is stale"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_rejects_product_query_override(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        payload["items"][0]["query_text"] = "hand tuned"
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "derive exactly from task prompt"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_requires_retained_evidence_path_and_hash(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        del payload["items"][0]["label_evidence"]["source_path"]
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "label_evidence source path is missing"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_materialize_rejects_mismatched_retained_evidence_bytes(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        (repo / "review-ledger.json").write_text("changed bytes\n")
        with self.assertRaisesRegex(relevance.DatasetError, "retained source hash differs"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_cannot_have_eligible_positive(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        payload["items"][2]["judgments"][0]["grade"] = "solving"
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "null query has eligible positive"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_requires_exhaustive_active_corpus_closure(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        payload = json.loads(labels.read_text())
        del payload["items"][2]["null_review"]
        labels.write_text(json.dumps(payload))
        with self.assertRaisesRegex(relevance.DatasetError, "requires exact retained review evidence"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_rejects_partial_corpus_review(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        runtime["null_review_ledger"]["items"][0]["decisions"].pop()
        with self.assertRaisesRegex(relevance.DatasetError, "does not exactly cover"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_rejects_duplicate_or_wrong_hash_review_rows(self):
        for attack in ("duplicate", "wrong_hash"):
            with self.subTest(attack=attack):
                temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
                self.addCleanup(temporary.cleanup)
                decisions = runtime["null_review_ledger"]["items"][0]["decisions"]
                if attack == "duplicate":
                    decisions.append(copy.deepcopy(decisions[0]))
                    expected = "missing or duplicated"
                else:
                    decisions[0]["fact_sha256"] = "f" * 64
                    expected = "differs from membership"
                with self.assertRaisesRegex(relevance.DatasetError, expected):
                    relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_rejects_positive_exhaustive_decision(self):
        temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
        self.addCleanup(temporary.cleanup)
        runtime["null_review_ledger"]["items"][0]["decisions"][0]["grade"] = "relevant_alternative"
        with self.assertRaisesRegex(relevance.DatasetError, "contains positive facts"):
            relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_null_query_rejects_temporal_membership_or_cutoff_laundering(self):
        for attack in ("provenance", "date"):
            with self.subTest(attack=attack):
                temporary, repo, labels, inventory, snapshot, runtime = self._fixture()
                self.addCleanup(temporary.cleanup)
                membership = runtime["source_membership"]
                if attack == "provenance":
                    membership["facts"][0]["provenance_session_ids"] = []
                else:
                    membership["session_dates"]["old"] = "2026-02-01T00:00:00Z"
                with self.assertRaisesRegex(relevance.DatasetError, "does not exactly cover"):
                    relevance.materialize(repo, labels, inventory, snapshot, **runtime)

    def test_d9df_url_fetch_advice_is_not_labeled_solving(self):
        labels = relevance.load_json(HERE / "offline-relevance-development-labels.json")
        item = next(row for row in labels["items"] if row["task_id"] == "entire-cli-c0701-d9df8fcca")
        grades = {judgment["fact_id"]: judgment["grade"] for judgment in item["judgments"]}
        self.assertEqual(grades["fact:8325ce8c7c9c08ba273c812f"], "hard_topical_distractor")
        self.assertEqual(grades["fact:11af406c1bcd1c4994261a6c"], "relevant_alternative")

    def test_checked_in_dataset_rebuilds_and_keeps_holdout_unopened(self):
        labels = HERE / "offline-relevance-development-labels.json"
        inventory = HERE / "task-inventory.json"
        snapshot = HERE / "offline-relevance-fact-snapshot.json"
        dataset = HERE / "offline-relevance-dataset.json"
        expected = relevance.materialize(REPO, labels, inventory, snapshot)
        actual = relevance.load_json(dataset)
        relevance.validate_dataset(actual, expected)
        self.assertEqual(actual["development"]["item_count"], 12)
        self.assertEqual(actual["development"]["unique_task_count"], 11)
        self.assertEqual(actual["development"]["null_query_count"], 0)
        self.assertEqual(actual["development"]["unique_answerable_product_task_count"], 11)
        self.assertEqual(actual["development"]["product_null_query_count"], 0)
        self.assertEqual(actual["development"]["judgment_count"], 38)
        schema_errors = relevance.validate_relevance_schemas(
            relevance.load_json(labels),
            relevance.load_json(snapshot),
            actual,
            HERE / "schemas",
        )
        self.assertEqual(schema_errors, [])
        self.assertEqual(actual["sealed_holdout"]["items"], [])

    def test_complete_membership_rejects_fabricated_fact_and_date(self):
        labels = relevance.load_json(HERE / "offline-relevance-development-labels.json")
        snapshot = relevance.load_json(HERE / "offline-relevance-fact-snapshot.json")
        contract = relevance.load_json(HERE / "relevance-source-contract.json")
        membership_path = HERE / "offline-relevance-source-membership.json"
        membership = relevance.load_json(membership_path)
        fabricated = copy.deepcopy(snapshot)
        fabricated["facts"][0]["text"] += " Fabricated."
        session_id = next(iter(fabricated["session_dates"]))
        fabricated["session_dates"][session_id] = "2020-01-01T00:00:00Z"
        with self.assertRaisesRegex(
            relevance.DatasetError,
            "snapshot fact differs from reviewed full-source membership",
        ):
            relevance.validate_source_membership(
                labels,
                fabricated,
                contract,
                membership,
                membership_sha256=relevance.sha256_bytes(membership_path.read_bytes()),
            )
        date_only = copy.deepcopy(snapshot)
        date_only["session_dates"][session_id] = "2020-01-01T00:00:00Z"
        with self.assertRaisesRegex(
            relevance.DatasetError,
            "snapshot session date differs from reviewed full-source membership",
        ):
            relevance.validate_source_membership(
                labels,
                date_only,
                contract,
                membership,
                membership_sha256=relevance.sha256_bytes(membership_path.read_bytes()),
            )

    def test_review_ledger_is_exact_per_query_and_judgment(self):
        labels = relevance.load_json(HERE / "offline-relevance-development-labels.json")
        contract = relevance.load_json(HERE / "relevance-source-contract.json")
        ledger_path = HERE / "offline-relevance-review-ledger.json"
        ledger = relevance.load_json(ledger_path)
        ledger["items"][0]["judgments"][0]["grade"] = "irrelevant"
        with self.assertRaisesRegex(relevance.DatasetError, "ledger judgments differ from labels"):
            relevance.validate_review_ledger(
                labels,
                ledger,
                contract,
                ledger_path="benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json",
                ledger_sha256=relevance.sha256_bytes(ledger_path.read_bytes()),
            )


if __name__ == "__main__":
    unittest.main()
