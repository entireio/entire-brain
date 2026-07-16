import copy
import importlib.util
import inspect
import json
import os
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
        self.fixture_path = self.repo.joinpath(*queries.FIXTURE_RELATIVE_PATH.parts)
        self.fixture_path.parent.mkdir(parents=True)
        self.fixture, self.source_paths, self.temporal_receipt = self._build_fixture()
        self.trusted_source_bindings = copy.deepcopy(self.fixture["source_bindings"])

    def tearDown(self) -> None:
        self.temporary.cleanup()

    @staticmethod
    def _dump(path: pathlib.Path, value: object) -> None:
        path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")), encoding="utf-8")

    def _write_fixture(self, mode: int = 0o600) -> None:
        self._dump(self.fixture_path, self.fixture)
        self.fixture_path.chmod(mode)

    def _write_source(self, role: str, value: object) -> None:
        path = self.source_paths[role]
        self._dump(path, value)
        digest = queries.sha256_bytes(path.read_bytes())
        self.fixture["source_bindings"][role]["sha256"] = digest
        self.trusted_source_bindings[role]["sha256"] = digest
        self._refresh_fixture_hash()

    def _refresh_fixture_hash(self) -> None:
        self.fixture["fixture_sha256"] = queries.fixture_sha256(self.fixture)

    def _refresh_temporal_receipt_hash(self) -> None:
        self.temporal_receipt["receipt_sha256"] = queries.temporal_receipt_sha256(
            self.temporal_receipt
        )

    def _verify(self) -> dict[str, object]:
        return queries._verify_fixture_with_trusted_receipts_for_testing(
            self.fixture,
            self.repo,
            self.trusted_source_bindings,
            self.temporal_receipt,
        )

    def _load(self) -> dict[str, object]:
        return queries._load_verified_fixture_with_trusted_receipts_for_testing(
            self.repo,
            self.trusted_source_bindings,
            self.temporal_receipt,
        )

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
            "fixture_id": queries.FIXTURE_ID,
            "purpose": queries.PURPOSE,
            "created_at": "2026-01-03T00:00:00Z",
            "source_bindings": source_bindings,
            "coverage": dict(queries.EXPECTED_COVERAGE),
            "items": fixture_items,
            "fixture_sha256": "0" * 64,
        }
        fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
        temporal_receipt = {
            "schema_version": 1,
            "receipt_id": queries.TEMPORAL_RECEIPT_ID,
            "purpose": queries.TEMPORAL_RECEIPT_PURPOSE,
            "created_at": "2026-01-03T00:00:00Z",
            "label_set_id": labels["label_set_id"],
            "query_count": 14,
            "items": [{
                "query_id": item["query_id"],
                "task_id": item["task_id"],
                "temporal_policy_sha256": item["temporal_policy_sha256"],
            } for item in fixture_items],
            "receipt_sha256": "0" * 64,
        }
        temporal_receipt["receipt_sha256"] = queries.temporal_receipt_sha256(temporal_receipt)
        return fixture, source_paths, temporal_receipt

    def test_valid_synthetic_fixture_verifies_and_loads_from_fixed_private_path(self) -> None:
        verified = self._verify()
        self.assertEqual(verified["fixture_sha256"], self.fixture["fixture_sha256"])
        self._write_fixture()
        loaded = self._load()
        self.assertEqual(loaded["fixture_sha256"], self.fixture["fixture_sha256"])

    def test_default_source_trust_is_machine_pinned_not_fixture_selected(self) -> None:
        pinned = queries.default_trusted_source_bindings()
        self.assertEqual(
            pinned["labels"]["sha256"],
            "1702cd625e71a9f5ad69ef41b5f5f1ef314bfa6df160d9aa002380f7a4ee77fe",
        )
        self.assertEqual(pinned["label_set_id"], "exposed-c0701-manual-review-2026-07-15-v3")
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("source I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "machine trust root"):
                queries._verify_fixture_core(
                    self.fixture,
                    self.repo,
                    pinned,
                    self.temporal_receipt,
                )

    def test_default_temporal_receipt_pin_blocks_before_plaintext_fixture_io(self) -> None:
        self.assertIsNone(queries.DEFAULT_TEMPORAL_RECEIPT_SHA256)
        with mock.patch.object(queries, "_read_fixed_private_json", side_effect=AssertionError("I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "receipt hash is not pinned"):
                queries.load_verified_fixture(self.repo)

    def test_fixture_controlled_safe_source_binding_cannot_change_trust(self) -> None:
        self.fixture["source_bindings"]["labels"]["path"] = "metadata/other-labels.json"
        self._refresh_fixture_hash()
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("source I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "machine trust root"):
                self._verify()

    def test_fixture_controlled_label_set_and_roots_cannot_change_trust(self) -> None:
        for field in ("label_set_id", "snapshot_roots"):
            with self.subTest(field=field):
                fixture = copy.deepcopy(self.fixture)
                if field == "label_set_id":
                    fixture["source_bindings"][field] = "fixture-selected-label-set"
                else:
                    fixture["source_bindings"][field]["facts_sha256"] = "f" * 64
                fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
                with mock.patch.object(
                    queries, "_read_prepared_file", side_effect=AssertionError("source I/O")
                ):
                    with self.assertRaisesRegex(queries.FixtureError, "machine trust root"):
                        queries._verify_fixture_with_trusted_receipts_for_testing(
                            fixture,
                            self.repo,
                            self.trusted_source_bindings,
                            self.temporal_receipt,
                        )

    def test_temporal_policy_requires_external_trusted_commitment(self) -> None:
        item = self.fixture["items"][0]
        item["temporal_cutoff"] = "2026-01-04T00:00:00Z"
        item["temporal_policy_sha256"] = queries.temporal_policy_sha256(
            item["temporal_cutoff"], item["exclude_session_ids"]
        )
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "trusted receipt"):
            self._verify()

    def test_temporal_receipt_task_binding_is_enforced(self) -> None:
        self.temporal_receipt["items"][0]["task_id"] = "different-synthetic-task"
        self._refresh_temporal_receipt_hash()
        with self.assertRaisesRegex(queries.FixtureError, "trusted receipt"):
            self._verify()

    def test_schemas_are_exact_and_accept_only_synthetic_contract_shapes(self) -> None:
        schema_dir = pathlib.Path(__file__).with_name("schemas")
        fixture_schema = json.loads(
            (schema_dir / "development-relevance-queries-v1.schema.json").read_text(encoding="utf-8")
        )
        temporal_schema = json.loads(
            (schema_dir / "development-relevance-temporal-policy-receipt-v1.schema.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertFalse(fixture_schema["additionalProperties"])
        self.assertFalse(fixture_schema["properties"]["source_bindings"]["additionalProperties"])
        self.assertFalse(fixture_schema["$defs"]["queryItem"]["additionalProperties"])
        self.assertEqual(set(fixture_schema["required"]), queries.TOP_LEVEL_FIELDS)
        self.assertEqual(set(fixture_schema["$defs"]["queryItem"]["required"]), queries.ITEM_FIELDS)
        self.assertEqual(relevance.validate_schema_instance(self.fixture, fixture_schema, "fixture"), [])
        self.assertEqual(
            relevance.validate_schema_instance(self.temporal_receipt, temporal_schema, "receipt"), []
        )
        self.assertNotIn("Synthetic product query", json.dumps((fixture_schema, temporal_schema)))

    def test_fixture_self_hash_excludes_only_self_hash_field(self) -> None:
        original = self.fixture["fixture_sha256"]
        copy_fixture = copy.deepcopy(self.fixture)
        copy_fixture["fixture_sha256"] = "f" * 64
        self.assertEqual(queries.fixture_sha256(copy_fixture), original)
        copy_fixture["fixture_id"] = "changed"
        self.assertNotEqual(queries.fixture_sha256(copy_fixture), original)

    def test_noncanonical_and_excluded_source_paths_fail_before_source_io(self) -> None:
        bad_paths = (
            "/metadata/labels.json",
            "metadata/../labels.json",
            "metadata/./labels.json",
            "metadata//labels.json",
            "metadata/labels.json/",
            "metadata/HOLDOUT/labels.json",
            "metadata\\labels.json",
        )
        for bad_path in bad_paths:
            with self.subTest(path=bad_path):
                fixture = copy.deepcopy(self.fixture)
                fixture["source_bindings"]["labels"]["path"] = bad_path
                fixture["fixture_sha256"] = queries.fixture_sha256(fixture)
                with mock.patch.object(
                    queries, "_read_prepared_file", side_effect=AssertionError("source I/O")
                ):
                    with self.assertRaises(queries.FixtureError):
                        queries._verify_fixture_with_trusted_receipts_for_testing(
                            fixture,
                            self.repo,
                            self.trusted_source_bindings,
                            self.temporal_receipt,
                        )

    def test_schema_and_verifier_agree_on_noncanonical_paths(self) -> None:
        schema = json.loads(
            pathlib.Path(__file__).with_name("schemas").joinpath(
                "development-relevance-queries-v1.schema.json"
            ).read_text(encoding="utf-8")
        )
        for bad_path in ("metadata//labels.json", "metadata/./labels.json"):
            with self.subTest(path=bad_path):
                fixture = copy.deepcopy(self.fixture)
                fixture["source_bindings"]["labels"]["path"] = bad_path
                self.assertTrue(relevance.validate_schema_instance(fixture, schema, "fixture"))
                with self.assertRaises(queries.FixtureError):
                    queries._validate_shape(fixture)

    def test_symlink_source_is_rejected_before_follow_or_read(self) -> None:
        alias = self.repo / "metadata" / "alias.json"
        os.symlink("holdout/synthetic-source.json", alias)
        relative = alias.relative_to(self.repo).as_posix()
        self.fixture["source_bindings"]["labels"]["path"] = relative
        self.trusted_source_bindings["labels"]["path"] = relative
        self._refresh_fixture_hash()
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("source I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "must not contain symlinks"):
                self._verify()

    def test_intermediate_source_symlink_is_rejected_before_follow_or_read(self) -> None:
        alias_directory = self.repo / "alias-directory"
        os.symlink("holdout", alias_directory)
        relative = "alias-directory/synthetic-source.json"
        self.fixture["source_bindings"]["labels"]["path"] = relative
        self.trusted_source_bindings["labels"]["path"] = relative
        self._refresh_fixture_hash()
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("source I/O")):
            with self.assertRaisesRegex(queries.FixtureError, "must not contain symlinks"):
                self._verify()

    def test_loader_has_no_arbitrary_fixture_path_surface(self) -> None:
        self.assertEqual(list(inspect.signature(queries.load_verified_fixture).parameters), ["repo_root"])
        elsewhere = self.repo / "elsewhere.json"
        self._dump(elsewhere, self.fixture)
        elsewhere.chmod(0o600)
        with self.assertRaisesRegex(queries.FixtureError, "fixture metadata check failed"):
            self._load()

    def test_plaintext_fixture_requires_mode_0600_before_read(self) -> None:
        self._write_fixture(0o644)
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("read")):
            with self.assertRaisesRegex(queries.FixtureError, "mode must be 0600"):
                self._load()

    def test_plaintext_fixture_requires_current_owner_before_read(self) -> None:
        self._write_fixture()
        with mock.patch.object(queries.os, "getuid", return_value=os.getuid() + 1):
            with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("read")):
                with self.assertRaisesRegex(queries.FixtureError, "owned by the current user"):
                    self._load()

    def test_plaintext_fixture_symlink_is_rejected_before_read(self) -> None:
        target = self.repo / "synthetic-private-target.json"
        self._dump(target, self.fixture)
        target.chmod(0o600)
        os.symlink(target, self.fixture_path)
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("read")):
            with self.assertRaisesRegex(queries.FixtureError, "must not contain symlinks"):
                self._load()

    def test_plaintext_fixture_must_be_a_regular_file_before_read(self) -> None:
        self.fixture_path.mkdir()
        self.fixture_path.chmod(0o600)
        with mock.patch.object(queries, "_read_prepared_file", side_effect=AssertionError("read")):
            with self.assertRaisesRegex(queries.FixtureError, "must be a regular file"):
                self._load()

    def test_fixture_identifier_is_pinned_and_not_reflected_in_error(self) -> None:
        secret_identifier = "fixture-controlled-secret-identifier"
        self.fixture["fixture_id"] = secret_identifier
        self._refresh_fixture_hash()
        with self.assertRaises(queries.FixtureError) as raised:
            self._verify()
        self.assertNotIn(secret_identifier, str(raised.exception))

    def test_success_and_error_surfaces_do_not_emit_fixture_ids_or_text(self) -> None:
        summary_input = copy.deepcopy(self.fixture)
        summary_input["coverage"] = {"fixture_controlled": self.fixture["items"][0]["query_text"]}
        summary = queries.safe_summary(summary_input)
        encoded = json.dumps(summary)
        self.assertNotIn("fixture_id", encoded)
        self.assertNotIn(self.fixture["items"][0]["query_id"], encoded)
        self.assertNotIn(self.fixture["items"][0]["query_text"], encoded)

        secret_id = "private-query-identifier"
        secret_text = "private query plaintext must not be reflected"
        self.fixture["items"][0]["query_id"] = secret_id
        self.fixture["items"][0]["query_text"] = secret_text
        self._refresh_fixture_hash()
        with self.assertRaises(queries.FixtureError) as raised:
            self._verify()
        self.assertNotIn(secret_id, str(raised.exception))
        self.assertNotIn(secret_text, str(raised.exception))

    def test_extra_fields_fail_closed_without_echoing_field_name(self) -> None:
        secret_field = "fixture_controlled_secret_field"
        self.fixture[secret_field] = True
        self._refresh_fixture_hash()
        with self.assertRaises(queries.FixtureError) as raised:
            self._verify()
        self.assertNotIn(secret_field, str(raised.exception))

    def test_bound_source_raw_hash_mismatch_fails_closed(self) -> None:
        self.source_paths["labels"].write_text("{}", encoding="utf-8")
        with self.assertRaisesRegex(queries.FixtureError, "labels SHA-256 mismatch"):
            self._verify()

    def test_self_hash_mismatch_fails_closed(self) -> None:
        self.fixture["fixture_sha256"] = "f" * 64
        with self.assertRaisesRegex(queries.FixtureError, "self-hash mismatch"):
            self._verify()

    def test_product_text_must_match_inventory_prompt_hash(self) -> None:
        self.fixture["items"][0]["query_text"] = "Different synthetic product query."
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "query text/hash mismatch"):
            self._verify()

    def test_oracle_text_must_match_reviewed_label(self) -> None:
        oracle = self.fixture["items"][1]
        oracle["query_text"] = "Different synthetic oracle query."
        oracle["query_sha256"] = queries.sha256_bytes(oracle["query_text"].encode("utf-8"))
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "differs from the reviewed label"):
            self._verify()

    def test_null_item_must_match_null_review_commitment(self) -> None:
        ledger = json.loads(self.source_paths["null_review_ledger"].read_text(encoding="utf-8"))
        ledger["items"][0]["temporal_policy_sha256"] = "f" * 64
        self._write_source("null_review_ledger", ledger)
        with self.assertRaisesRegex(queries.FixtureError, "null-review commitment"):
            self._verify()

    def test_query_ids_must_exactly_cover_labels_without_duplicates(self) -> None:
        self.fixture["items"][-1]["query_id"] = self.fixture["items"][0]["query_id"]
        self._refresh_fixture_hash()
        with self.assertRaisesRegex(queries.FixtureError, "duplicate query_id"):
            self._verify()

    def test_inventory_prompt_hash_binding_fails_closed(self) -> None:
        inventory = json.loads(self.source_paths["task_inventory"].read_text(encoding="utf-8"))
        inventory["tasks"][0]["artifacts"]["prompt_sha256"] = "f" * 64
        self._write_source("task_inventory", inventory)
        with self.assertRaisesRegex(queries.FixtureError, "task prompt hash mismatch"):
            self._verify()

    def test_snapshot_canonical_hash_is_recomputed(self) -> None:
        snapshot = json.loads(self.source_paths["fact_snapshot"].read_text(encoding="utf-8"))
        snapshot["facts"][0]["text"] = "Mutated synthetic fact."
        self._write_source("fact_snapshot", snapshot)
        with self.assertRaisesRegex(queries.FixtureError, "facts_sha256 is not canonical"):
            self._verify()

    def test_temporal_policy_hash_is_recomputed_after_external_receipt_match(self) -> None:
        self.fixture["items"][0]["temporal_policy_sha256"] = "f" * 64
        self.temporal_receipt["items"][0]["temporal_policy_sha256"] = "f" * 64
        self._refresh_fixture_hash()
        self._refresh_temporal_receipt_hash()
        with self.assertRaisesRegex(queries.FixtureError, "temporal policy hash mismatch"):
            self._verify()

    def test_duplicate_json_keys_are_rejected_without_echoing_key(self) -> None:
        secret_key = "private_query_text_as_a_key"
        raw = (f'{{"{secret_key}":1,"{secret_key}":2}}').encode("utf-8")
        with self.assertRaises(queries.FixtureError) as raised:
            queries.decode_json(raw, "fixture")
        self.assertNotIn(secret_key, str(raised.exception))


if __name__ == "__main__":
    unittest.main()
