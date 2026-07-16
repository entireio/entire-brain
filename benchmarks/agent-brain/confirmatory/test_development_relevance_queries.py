import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest import mock


MODULE_PATH = pathlib.Path(__file__).with_name("development_relevance_queries.py")
SPEC = importlib.util.spec_from_file_location("development_relevance_queries", MODULE_PATH)
queries = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(queries)

RELEVANCE_PATH = pathlib.Path(__file__).with_name("relevance_dataset.py")
RELEVANCE_SPEC = importlib.util.spec_from_file_location("fixture_relevance_dataset", RELEVANCE_PATH)
relevance = importlib.util.module_from_spec(RELEVANCE_SPEC)
assert RELEVANCE_SPEC.loader is not None
RELEVANCE_SPEC.loader.exec_module(relevance)


class DevelopmentRelevanceQueryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.repo = pathlib.Path(self.temporary.name)
        (self.repo / "metadata").mkdir()
        self.fixture, self.source_paths = self._build_fixture()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    @staticmethod
    def _dump(path: pathlib.Path, value: object) -> None:
        path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")), encoding="utf-8")

    def _write_source(self, role: str, value: object) -> None:
        path = self.source_paths[role]
        self._dump(path, value)
        self.fixture["source_bindings"][role]["sha256"] = queries.sha256_bytes(path.read_bytes())
        self._refresh_fixture_hash()

    def _refresh_fixture_hash(self) -> None:
        self.fixture["fixture_sha256"] = queries.fixture_sha256(self.fixture)

    def _build_fixture(self):
        cutoff = "2026-01-02T00:00:00Z"
        exclusions = ["synthetic-excluded-session"]
        policy_hash = queries.temporal_policy_sha256(cutoff, exclusions)

        product_items = []
        label_items = []
        review_items = []
        inventory_tasks = []
        for index in range(13):
            task_id = f"synthetic-task-{index:02d}"
            query_id = f"synthetic-product-{index:02d}"
            query_text = f"Synthetic product query number {index}."
            prompt_hash = queries.sha256_bytes(query_text.encode("utf-8"))
            config_hash = queries.sha256_bytes(f"synthetic-config-{index}".encode("utf-8"))
            null_query = index == 12
            item = {
                "query_id": query_id,
                "task_id": task_id,
                "query_source": "user_prompt_derived",
                "null_query": null_query,
                "query_text": query_text,
                "query_sha256": prompt_hash,
                "task_prompt_sha256": prompt_hash,
                "source_config_sha256": config_hash,
                "temporal_cutoff": cutoff,
                "exclude_session_ids": exclusions,
                "temporal_policy_sha256": policy_hash,
            }
            product_items.append(item)
            label_items.append({
                "query_id": query_id,
                "task_id": task_id,
                "query_source": "user_prompt_derived",
                "null_query": null_query,
            })
            review_items.append({
                "query_id": query_id,
                "task_id": task_id,
                "query_source": "user_prompt_derived",
            })
            inventory_tasks.append({
                "task_id": task_id,
                "assigned_split": "development",
                "artifacts": {
                    "config_sha256": config_hash,
                    "prompt_sha256": prompt_hash,
                    "config_path": f"synthetic/config-{index}.json",
                },
            })

        oracle_text = "Synthetic oracle query with reviewed wording."
        oracle = {
            "query_id": "synthetic-oracle-00",
            "task_id": "synthetic-task-00",
            "query_source": "oracle_upper_bound",
            "null_query": False,
            "query_text": oracle_text,
            "query_sha256": queries.sha256_bytes(oracle_text.encode("utf-8")),
            "task_prompt_sha256": product_items[0]["task_prompt_sha256"],
            "source_config_sha256": product_items[0]["source_config_sha256"],
            "temporal_cutoff": cutoff,
            "exclude_session_ids": exclusions,
            "temporal_policy_sha256": policy_hash,
        }
        label_items.insert(1, {
            "query_id": oracle["query_id"],
            "task_id": oracle["task_id"],
            "query_source": oracle["query_source"],
            "null_query": False,
            "query_text": oracle_text,
        })
        review_items.insert(1, {
            "query_id": oracle["query_id"],
            "task_id": oracle["task_id"],
            "query_source": oracle["query_source"],
        })
        fixture_items = [product_items[0], oracle, *product_items[1:]]

        facts = [{"id": "synthetic-fact", "status": "active", "text": "Synthetic fact."}]
        dates = {"synthetic-session": "2026-01-01T00:00:00Z"}
        roots = {
            "source_facts_sha256": "a" * 64,
            "source_session_dates_sha256": "b" * 64,
            "source_active_fact_count": 1,
            "source_active_fact_catalog_sha256": "c" * 64,
            "fact_count": len(facts),
            "facts_sha256": queries.sha256_bytes(queries.canonical_json(facts)),
            "session_date_count": len(dates),
            "session_dates_sha256": queries.sha256_bytes(queries.canonical_json(dates)),
        }
        labels = {
            "schema_version": 2,
            "label_set_id": "synthetic-label-set-v1",
            "source_corpus": {
                "facts_sha256": roots["source_facts_sha256"],
                "session_dates_sha256": roots["source_session_dates_sha256"],
                "active_fact_count": roots["source_active_fact_count"],
                "active_fact_catalog_sha256": roots["source_active_fact_catalog_sha256"],
            },
            "snapshot_commitment": {
                "fact_count": roots["fact_count"],
                "facts_sha256": roots["facts_sha256"],
                "session_date_count": roots["session_date_count"],
                "session_dates_sha256": roots["session_dates_sha256"],
            },
            "items": label_items,
        }
        sources = {
            "labels": labels,
            "review_ledger": {"schema_version": 1, "items": review_items},
            "null_review_ledger": {
                "schema_version": 1,
                "items": [{
                    "query_id": product_items[-1]["query_id"],
                    "task_id": product_items[-1]["task_id"],
                    "query_sha256": product_items[-1]["query_sha256"],
                    "temporal_policy_sha256": product_items[-1]["temporal_policy_sha256"],
                }],
            },
            "fact_snapshot": {
                "schema_version": 2,
                **roots,
                "facts": facts,
                "session_dates": dates,
            },
            "engine_pins": {"schema_version": 4, "pin_set_id": "synthetic-pins"},
            "task_inventory": {"schema_version": 2, "tasks": inventory_tasks},
        }
        source_paths = {}
        source_bindings = {}
        for role, value in sources.items():
            path = self.repo / "metadata" / f"{role}.json"
            self._dump(path, value)
            source_paths[role] = path
            source_bindings[role] = {
                "path": path.relative_to(self.repo).as_posix(),
                "sha256": queries.sha256_bytes(path.read_bytes()),
            }
        source_bindings["label_set_id"] = labels["label_set_id"]
        source_bindings["snapshot_roots"] = roots
        fixture = {
            "schema_version": 1,
            "fixture_id": "synthetic-development-relevance-v1",
            "purpose": queries.PURPOSE,
            "created_at": "2026-01-03T00:00:00Z",
            "source_bindings": source_bindings,
            "coverage": dict(queries.EXPECTED_COVERAGE),
            "items": fixture_items,
            "fixture_sha256": "0" * 64,
        }
        fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
        return fixture, source_paths

    def test_valid_synthetic_fixture_loads(self) -> None:
        fixture_path = self.repo / "development-relevance-queries-v1.json"
        self._dump(fixture_path, self.fixture)
        verified = queries.load_verified_fixture(fixture_path, self.repo)
        self.assertEqual(verified["fixture_sha256"], self.fixture["fixture_sha256"])
        summary = queries.safe_summary(verified)
        self.assertNotIn("items", summary)
        self.assertEqual(summary["coverage"], queries.EXPECTED_COVERAGE)

    def test_schema_is_exact_and_does_not_embed_query_plaintext(self) -> None:
        schema = json.loads(
            pathlib.Path(__file__).with_name("schemas").joinpath(
                "development-relevance-queries-v1.schema.json"
            ).read_text(encoding="utf-8")
        )
        self.assertFalse(schema["additionalProperties"])
        self.assertFalse(schema["properties"]["source_bindings"]["additionalProperties"])
        self.assertFalse(schema["$defs"]["queryItem"]["additionalProperties"])
        self.assertEqual(set(schema["required"]), queries.TOP_LEVEL_FIELDS)
        self.assertEqual(set(schema["$defs"]["queryItem"]["required"]), queries.ITEM_FIELDS)
        self.assertNotIn("Synthetic product query", json.dumps(schema))
        self.assertEqual(relevance.validate_schema_instance(self.fixture, schema, "fixture"), [])

    def test_fixture_self_hash_excludes_only_self_hash_field(self) -> None:
        original = self.fixture["fixture_sha256"]
        copy_fixture = copy.deepcopy(self.fixture)
        copy_fixture["fixture_sha256"] = "f" * 64
        self.assertEqual(queries.fixture_sha256(copy_fixture), original)
        copy_fixture["fixture_id"] = "changed"
        self.assertNotEqual(queries.fixture_sha256(copy_fixture), original)

    def test_absolute_traversal_and_excluded_source_paths_fail_before_source_io(self) -> None:
        bad_paths = (
            "/metadata/labels.json",
            "metadata/../labels.json",
            "metadata/HOLDOUT/labels.json",
            "metadata\\labels.json",
        )
        for bad_path in bad_paths:
            with self.subTest(path=bad_path):
                fixture = copy.deepcopy(self.fixture)
                fixture["source_bindings"]["labels"]["path"] = bad_path
                fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
                with mock.patch.object(pathlib.Path, "read_bytes", side_effect=AssertionError("source I/O")):
                    with self.assertRaises(queries.FixtureError):
                        queries.verify_fixture(fixture, self.repo)

    def test_all_source_paths_are_lexically_checked_before_any_source_io(self) -> None:
        fixture = copy.deepcopy(self.fixture)
        fixture["source_bindings"]["task_inventory"]["path"] = "metadata/holdout/inventory.json"
        fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
        with mock.patch.object(pathlib.Path, "read_bytes", side_effect=AssertionError("source I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "excluded path component"):
                queries.verify_fixture(fixture, self.repo)

    def test_extra_fields_fail_closed(self) -> None:
        self.fixture["unexpected"] = True
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "fields differ"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_source_hash_mismatch_fails_closed(self) -> None:
        self.fixture["source_bindings"]["labels"]["sha256"] = "f" * 64
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "labels SHA-256 mismatch"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_self_hash_mismatch_fails_closed(self) -> None:
        self.fixture["fixture_sha256"] = "f" * 64
        with self.assertRaisesRegex(queries.FixtureError, "self-hash mismatch"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_product_text_must_match_inventory_prompt_hash(self) -> None:
        self.fixture["items"][0]["query_text"] = "Different synthetic product query."
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "query text/hash mismatch"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_oracle_text_must_match_reviewed_label(self) -> None:
        oracle = self.fixture["items"][1]
        oracle["query_text"] = "Different synthetic oracle query."
        oracle["query_sha256"] = queries.sha256_bytes(oracle["query_text"].encode("utf-8"))
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "differs from the reviewed label"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_null_item_must_match_null_review_commitment(self) -> None:
        ledger = json.loads(self.source_paths["null_review_ledger"].read_text(encoding="utf-8"))
        ledger["items"][0]["temporal_policy_sha256"] = "f" * 64
        self._write_source("null_review_ledger", ledger)
        with self.assertRaisesRegex(queries.FixtureError, "null-review commitment"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_query_ids_must_exactly_cover_labels_without_duplicates(self) -> None:
        self.fixture["items"][-1]["query_id"] = self.fixture["items"][0]["query_id"]
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "duplicate query_id"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_inventory_prompt_hash_binding_fails_closed(self) -> None:
        inventory = json.loads(self.source_paths["task_inventory"].read_text(encoding="utf-8"))
        inventory["tasks"][0]["artifacts"]["prompt_sha256"] = "f" * 64
        self._write_source("task_inventory", inventory)
        with self.assertRaisesRegex(queries.FixtureError, "task prompt hash mismatch"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_snapshot_canonical_hash_is_recomputed(self) -> None:
        snapshot = json.loads(self.source_paths["fact_snapshot"].read_text(encoding="utf-8"))
        snapshot["facts"][0]["text"] = "Mutated synthetic fact."
        self._write_source("fact_snapshot", snapshot)
        with self.assertRaisesRegex(queries.FixtureError, "facts_sha256 is not canonical"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_temporal_policy_hash_is_recomputed(self) -> None:
        self.fixture["items"][0]["temporal_policy_sha256"] = "f" * 64
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "temporal policy hash mismatch"):
            queries.verify_fixture(self.fixture, self.repo)

    def test_duplicate_json_keys_are_rejected(self) -> None:
        with self.assertRaisesRegex(queries.FixtureError, "duplicate JSON object key"):
            queries.decode_json(b'{"schema_version":1,"schema_version":1}', "fixture")


if __name__ == "__main__":
    unittest.main()
