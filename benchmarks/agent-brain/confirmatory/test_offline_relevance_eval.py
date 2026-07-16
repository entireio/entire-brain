#!/usr/bin/env python3
"""Synthetic, offline-only contract tests for offline_relevance_eval.py."""

from __future__ import annotations

import copy
import hashlib
import json
import pathlib
import unittest
from typing import Any, Callable

import offline_relevance_eval as relevance


def _hash(label: str) -> str:
    return hashlib.sha256(label.encode("utf-8")).hexdigest()


def _raw(value: object) -> bytes:
    return relevance.canonical_json(value)


def _result(fact_id: str, kind: str, cluster_id: str, tokens: int) -> dict[str, object]:
    return {
        "fact_id": fact_id,
        "fact_sha256": _hash(fact_id),
        "kind": kind,
        "cluster_id": cluster_id,
        "eligible": True,
        "estimated_tokens": tokens,
    }


def _synthetic_case() -> dict[str, Any]:
    arms = [
        {
            "id": "lexical",
            "semantic": False,
            "effective_engine_required": "lexical",
            "namespace": "lexical-v1",
        },
        {
            "id": "model2vec",
            "semantic": True,
            "effective_engine_required": "model2vec",
            "namespace": "model2vec-v1",
        },
        {
            "id": "embeddinggemma",
            "semantic": True,
            "effective_engine_required": "embeddinggemma",
            "namespace": "embeddinggemma-v1",
        },
    ]
    engine_matrix = {"matrix_id": "synthetic-matrix-v1", "arms": arms}
    engine_pins = {
        "pin_set_id": "synthetic-pins-v1",
        "binary": {"binary_sha256": _hash("binary"), "binary_size_bytes": 4096},
        "engines": {
            "lexical": {"dimension": None},
            "model2vec": {"dimension": 512},
            "embeddinggemma": {"dimension": 768},
        },
    }
    facts = [
        {
            "fact_id": "positive-1",
            "fact_sha256": _hash("positive-1"),
            "status": "active",
            "provenance_session_ids": ["session-old"],
        },
        {
            "fact_id": "positive-2",
            "fact_sha256": _hash("positive-2"),
            "status": "active",
            "provenance_session_ids": ["session-old"],
        },
        {
            "fact_id": "distractor-1",
            "fact_sha256": _hash("distractor-1"),
            "status": "active",
            "provenance_session_ids": ["session-old"],
        },
        {
            "fact_id": "irrelevant-1",
            "fact_sha256": _hash("irrelevant-1"),
            "status": "active",
            "provenance_session_ids": ["session-old"],
        },
        {
            "fact_id": "future-1",
            "fact_sha256": _hash("future-1"),
            "status": "active",
            "provenance_session_ids": ["session-future"],
        },
    ]
    membership = {
        "fact_count": len(facts),
        "active_fact_count": len(facts),
        "facts": facts,
        "session_dates": {
            "session-old": "2026-01-01T00:00:00Z",
            "session-future": "2026-03-01T00:00:00Z",
        },
        "eligibility_membership_root_sha256": relevance._membership_root(facts),
    }
    fixture = {
        "schema_version": 1,
        "fixture_id": "synthetic-development-fixture-v1",
        "items": [
            {
                "query_id": "query-product",
                "task_id": "task-product",
                "query_source": "user_prompt_derived",
                "null_query": False,
                "query_text": "synthetic product query sentinel",
                "temporal_cutoff": "2026-02-01T00:00:00Z",
                "exclude_session_ids": [],
            },
            {
                "query_id": "query-null",
                "task_id": "task-null",
                "query_source": "user_prompt_derived",
                "null_query": True,
                "query_text": "synthetic null query sentinel",
                "temporal_cutoff": "2026-02-01T00:00:00Z",
                "exclude_session_ids": [],
            },
            {
                "query_id": "query-oracle",
                "task_id": "task-oracle",
                "query_source": "oracle_upper_bound",
                "null_query": False,
                "query_text": "synthetic oracle query sentinel",
                "temporal_cutoff": "2026-02-01T00:00:00Z",
                "exclude_session_ids": [],
            },
        ],
        "fixture_sha256": "",
    }
    fixture["fixture_sha256"] = relevance._self_hash(fixture, "fixture_sha256")
    product_judgments = [
        {
            "fact_id": "positive-1",
            "fact_sha256": _hash("positive-1"),
            "grade": "solving",
            "kind": "decision",
            "cluster_id": "cluster-positive-1",
        },
        {
            "fact_id": "positive-2",
            "fact_sha256": _hash("positive-2"),
            "grade": "relevant_alternative",
            "kind": "procedure",
            "cluster_id": "cluster-positive-2",
        },
        {
            "fact_id": "distractor-1",
            "fact_sha256": _hash("distractor-1"),
            "grade": "hard_topical_distractor",
            "kind": "constraint",
            "cluster_id": "cluster-distractor-1",
        },
        {
            "fact_id": "irrelevant-1",
            "fact_sha256": _hash("irrelevant-1"),
            "grade": "irrelevant",
            "kind": "observation",
            "cluster_id": "cluster-irrelevant-1",
        },
    ]
    dataset = {
        "schema_version": 2,
        "dataset_id": "synthetic-offline-relevance-v2",
        "development": {
            "items": [
                {
                    "query_id": "query-product",
                    "task_id": "task-product",
                    "query_source": "user_prompt_derived",
                    "null_query": False,
                    "judgments": product_judgments,
                },
                {
                    "query_id": "query-null",
                    "task_id": "task-null",
                    "query_source": "user_prompt_derived",
                    "null_query": True,
                    "judgments": copy.deepcopy(product_judgments[2:]),
                },
                {
                    "query_id": "query-oracle",
                    "task_id": "task-oracle",
                    "query_source": "oracle_upper_bound",
                    "null_query": False,
                    "judgments": [copy.deepcopy(product_judgments[0])],
                },
            ]
        },
    }
    producer_identity = relevance.finalize_producer_identity(
        {
            "schema_version": 1,
            "schema": relevance.PRODUCER_IDENTITY_SCHEMA,
            "producer_id": "synthetic-ranked-run-producer-v1",
            "runner_sha256": _hash("synthetic-runner-artifact"),
            "producer_code_sha256": _hash("synthetic-runner-code"),
            "ranking_result_metadata_policy_id": "synthetic-result-metadata-v1",
            "ranking_result_metadata_policy_sha256": _hash(
                "synthetic-result-metadata-policy"
            ),
            "token_estimation_policy_id": "synthetic-token-estimation-v1",
            "token_estimation_policy_sha256": _hash(
                "synthetic-token-estimation-policy"
            ),
            "serialization_policy_id": "synthetic-serialization-v1",
            "serialization_policy_sha256": _hash("synthetic-serialization-policy"),
            "producer_identity_sha256": "",
        }
    )
    engine_matrix_raw = _raw(engine_matrix)
    engine_pins_raw = _raw(engine_pins)
    producer_identity_raw = _raw(producer_identity)
    dataset_raw = _raw(dataset)
    membership_raw = _raw(membership)
    bindings = relevance._build_input_bindings(
        engine_matrix_raw,
        engine_matrix,
        engine_pins_raw,
        engine_pins,
        producer_identity_raw,
        producer_identity,
        fixture,
        dataset_raw,
        dataset,
        membership_raw,
        membership,
    )
    result_rows = [
        _result("positive-1", "decision", "cluster-positive-1", 10),
        _result("positive-2", "procedure", "cluster-positive-2", 20),
        _result("distractor-1", "constraint", "cluster-distractor-1", 30),
        _result("irrelevant-1", "observation", "cluster-irrelevant-1", 40),
    ]
    query_rows = [
        {
            "query_id": "query-product",
            "task_id": "task-product",
            "query_source": "user_prompt_derived",
            "null_query": False,
            "telemetry": {
                "active_fact_count": 5,
                "eligible_fact_count": 4,
                "temporal_exclusion_count": 1,
                "delivered_count": 4,
                "ranking_latency_ms": 2.5,
            },
            "ordered_results": result_rows,
        },
        {
            "query_id": "query-null",
            "task_id": "task-null",
            "query_source": "user_prompt_derived",
            "null_query": True,
            "telemetry": {
                "active_fact_count": 5,
                "eligible_fact_count": 4,
                "temporal_exclusion_count": 1,
                "delivered_count": 0,
                "ranking_latency_ms": 1.0,
            },
            "ordered_results": [],
        },
        {
            "query_id": "query-oracle",
            "task_id": "task-oracle",
            "query_source": "oracle_upper_bound",
            "null_query": False,
            "telemetry": {
                "active_fact_count": 5,
                "eligible_fact_count": 4,
                "temporal_exclusion_count": 1,
                "delivered_count": 1,
                "ranking_latency_ms": 0.5,
            },
            "ordered_results": [copy.deepcopy(result_rows[0])],
        },
    ]
    runs: list[bytes] = []
    dimensions = {"lexical": None, "model2vec": 512, "embeddinggemma": 768}
    for arm in arms:
        arm_id = arm["id"]
        for aggregation in relevance.AGGREGATION_CANDIDATES:
            semantic = bool(arm["semantic"])
            identity = {
                "arm_id": arm_id,
                "effective_engine": arm["effective_engine_required"],
                "semantic": semantic,
                "binary_sha256": engine_pins["binary"]["binary_sha256"],
                "binary_size_bytes": engine_pins["binary"]["binary_size_bytes"],
                "vector_namespace": arm["namespace"],
                "vector_artifact_sha256": _hash(f"artifact-{arm_id}") if semantic else None,
                "vector_artifact_size_bytes": 8192 if semantic else None,
                "embedding_dimension": dimensions[arm_id],
                "fallback_used": False,
            }
            run = {
                "schema_version": 1,
                "schema": relevance.RANKED_RUN_SCHEMA,
                "run_id": f"run-{arm_id}-{aggregation}",
                "created_at": "2026-04-01T00:00:00Z",
                "input_bindings": copy.deepcopy(bindings),
                "producer_identity": copy.deepcopy(producer_identity),
                "engine_identity": identity,
                "aggregation_rule": aggregation,
                "k_candidates": list(relevance.K_CANDIDATES),
                "queries": copy.deepcopy(query_rows),
                "run_sha256": "",
            }
            runs.append(_raw(relevance.finalize_ranked_run(run)))
    return {
        "engine_matrix": engine_matrix,
        "engine_matrix_raw": engine_matrix_raw,
        "engine_pins": engine_pins,
        "engine_pins_raw": engine_pins_raw,
        "producer_identity": producer_identity,
        "producer_identity_raw": producer_identity_raw,
        "fixture": fixture,
        "dataset": dataset,
        "dataset_raw": dataset_raw,
        "membership": membership,
        "membership_raw": membership_raw,
        "runs": runs,
    }


class OfflineRelevanceEvalTests(unittest.TestCase):
    def setUp(self) -> None:
        self.case: dict[str, Any] = _synthetic_case()

    def evaluate(self, **overrides: Any) -> dict[str, Any]:
        values: dict[str, Any] = {
            "engine_matrix_raw": self.case["engine_matrix_raw"],
            "engine_pins_raw": self.case["engine_pins_raw"],
            "producer_identity_raw": self.case["producer_identity_raw"],
            "fixture": self.case["fixture"],
            "dataset_raw": self.case["dataset_raw"],
            "source_membership_raw": self.case["membership_raw"],
            "ranked_run_raws": self.case["runs"],
        }
        values.update(overrides)
        return relevance.evaluate(**values)

    def mutate_run(
        self, index: int, mutate: Callable[[dict[str, Any]], None]
    ) -> list[bytes]:
        runs = list(self.case["runs"])
        run = json.loads(runs[index])
        mutate(run)
        runs[index] = _raw(relevance.finalize_ranked_run(run))
        return runs

    def test_metrics_and_per_engine_selection_are_deterministic(self) -> None:
        report = self.evaluate()
        self.assertEqual(report, self.evaluate())
        self.assertEqual(report["report_sha256"], relevance._self_hash(report, "report_sha256"))
        self.assertEqual(report["producer_identity"], self.case["producer_identity"])
        self.assertEqual(
            report["input_bindings"]["producer_identity_file_sha256"],
            relevance.sha256_bytes(self.case["producer_identity_raw"]),
        )
        self.assertEqual(
            report["producer_identity"]["producer_identity_sha256"],
            relevance._self_hash(report["producer_identity"], "producer_identity_sha256"),
        )
        self.assertEqual(len(report["evaluated_runs"]), 6)
        self.assertEqual(len(report["selections"]), 3)
        first = report["evaluated_runs"][0]["candidates"][0]
        product = first["product"]
        oracle = first["oracle"]
        self.assertEqual(product["mean_recall_at_1"], 0.5)
        self.assertEqual(product["mean_recall_at_5"], 1.0)
        self.assertEqual(product["mean_recall_at_10"], 1.0)
        self.assertEqual(product["mean_mrr"], 1.0)
        self.assertEqual(product["mean_ndcg_at_10"], 1.0)
        self.assertEqual(product["mean_precision_at_k"], 0.2)
        self.assertEqual(product["null_false_positive_numerator"], 0)
        self.assertEqual(product["null_false_positive_denominator"], 1)
        self.assertEqual(product["null_query_false_positive_rate"], 0.0)
        self.assertEqual(product["maximum_cluster_occupancy"], 1)
        self.assertEqual(oracle["query_count"], 1)
        self.assertEqual(oracle["null_query_false_positive_rate"], None)
        for selection in report["selections"]:
            self.assertEqual(selection["status"], "selected")
            self.assertEqual(selection["k"], 5)
            self.assertEqual(
                selection["aggregation_rule"], "best_rank_then_hit_count_v1"
            )
        for evaluated in report["evaluated_runs"]:
            self.assertEqual(
                evaluated["producer_identity_sha256"],
                report["producer_identity"]["producer_identity_sha256"],
            )

    def test_artifacts_never_copy_query_or_fact_text(self) -> None:
        report = self.evaluate()
        forbidden = {"query_text", "fact_text", "text", "content", "prompt"}

        def walk(value: object) -> None:
            if isinstance(value, dict):
                self.assertTrue(forbidden.isdisjoint(value))
                for child in value.values():
                    walk(child)
            elif isinstance(value, list):
                for child in value:
                    walk(child)

        walk(report)
        encoded = _raw(report)
        self.assertNotIn(b"synthetic product query sentinel", encoded)
        self.assertNotIn(b"synthetic null query sentinel", encoded)
        for raw in self.case["runs"]:
            self.assertNotIn(b"query_text", raw)
            self.assertNotIn(b"fact_text", raw)

    def test_metric_contract_freezes_weights_denominators_and_strata(self) -> None:
        contract = self.evaluate()["metric_contract"]
        self.assertEqual(
            contract["grade_weights"],
            {
                "solving": 3,
                "relevant_alternative": 2,
                "hard_topical_distractor": 0,
                "irrelevant": 0,
            },
        )
        self.assertEqual(contract["mrr_cutoff"], 12)
        self.assertEqual(contract["ndcg_cutoff"], 10)
        self.assertEqual(
            contract["null_false_positive_denominator"], "product_null_query_count"
        )
        self.assertEqual(
            contract["strata"], ["user_prompt_derived", "oracle_upper_bound"]
        )
        self.assertEqual(
            contract["ranking_result_metadata_policy_scope"],
            "fact_hash_kind_cluster_eligibility_and_query_telemetry_fields",
        )
        self.assertEqual(
            contract["token_estimation_policy_scope"],
            "positive_estimated_tokens_per_delivered_fact",
        )

    def test_duplicate_ranked_fact_fails_closed(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            query = run["queries"][0]
            query["ordered_results"].append(copy.deepcopy(query["ordered_results"][0]))
            query["telemetry"]["delivered_count"] += 1

        with self.assertRaisesRegex(relevance.EvaluationError, "duplicate result fact IDs"):
            self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_duplicate_ranked_query_fails_closed(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            run["queries"][1] = copy.deepcopy(run["queries"][0])

        with self.assertRaisesRegex(relevance.EvaluationError, "duplicate query IDs"):
            self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_duplicate_dataset_judgment_fails_closed(self) -> None:
        dataset = copy.deepcopy(self.case["dataset"])
        judgments = dataset["development"]["items"][0]["judgments"]
        judgments.append(copy.deepcopy(judgments[0]))
        with self.assertRaisesRegex(relevance.EvaluationError, "duplicate judgment fact IDs"):
            self.evaluate(dataset_raw=_raw(dataset))

    def test_duplicate_membership_fact_fails_closed(self) -> None:
        membership = copy.deepcopy(self.case["membership"])
        membership["facts"].append(copy.deepcopy(membership["facts"][0]))
        membership["fact_count"] += 1
        membership["active_fact_count"] += 1
        with self.assertRaisesRegex(relevance.EvaluationError, "duplicate fact IDs"):
            self.evaluate(source_membership_raw=_raw(membership))

    def test_unknown_relevance_grade_fails_closed(self) -> None:
        dataset = copy.deepcopy(self.case["dataset"])
        dataset["development"]["items"][0]["judgments"][0]["grade"] = "maybe"
        with self.assertRaisesRegex(relevance.EvaluationError, "unknown relevance grade"):
            self.evaluate(dataset_raw=_raw(dataset))

    def test_malformed_null_query_fails_closed(self) -> None:
        dataset = copy.deepcopy(self.case["dataset"])
        dataset["development"]["items"][1]["null_query"] = None
        with self.assertRaisesRegex(relevance.EvaluationError, "metadata is malformed"):
            self.evaluate(dataset_raw=_raw(dataset))

    def test_null_query_with_positive_judgment_fails_closed(self) -> None:
        dataset = copy.deepcopy(self.case["dataset"])
        dataset["development"]["items"][1]["judgments"].append(
            copy.deepcopy(dataset["development"]["items"][0]["judgments"][0])
        )
        with self.assertRaisesRegex(relevance.EvaluationError, "null query contains a positive"):
            self.evaluate(dataset_raw=_raw(dataset))

    def test_temporal_leakage_fails_closed(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            query = run["queries"][0]
            query["ordered_results"].append(
                {
                    "fact_id": "future-1",
                    "fact_sha256": _hash("future-1"),
                    "kind": "observation",
                    "cluster_id": "cluster-future-1",
                    "eligible": False,
                    "estimated_tokens": 5,
                }
            )
            query["telemetry"]["delivered_count"] += 1

        with self.assertRaisesRegex(relevance.EvaluationError, "temporal leakage"):
            self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_authenticated_input_hash_drift_fails_closed(self) -> None:
        membership = copy.deepcopy(self.case["membership"])
        membership["synthetic_drift_marker"] = 1
        with self.assertRaisesRegex(relevance.EvaluationError, "input bindings differ"):
            self.evaluate(source_membership_raw=_raw(membership))

    def test_engine_identity_mismatches_fail_closed(self) -> None:
        cases = (
            (
                0,
                lambda run: run["engine_identity"].__setitem__("binary_sha256", _hash("wrong")),
                "binary identity differs",
            ),
            (
                0,
                lambda run: run["engine_identity"].__setitem__("vector_namespace", "wrong"),
                "vector namespace differs",
            ),
            (
                2,
                lambda run: run["engine_identity"].__setitem__("embedding_dimension", 123),
                "embedding dimension differs",
            ),
        )
        for index, mutate, message in cases:
            with self.subTest(message=message):
                with self.assertRaisesRegex(relevance.EvaluationError, message):
                    self.evaluate(ranked_run_raws=self.mutate_run(index, mutate))

    def test_engine_identity_is_stable_across_aggregation_runs(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            run["engine_identity"]["vector_artifact_sha256"] = _hash("different-artifact")

        with self.assertRaisesRegex(relevance.EvaluationError, "differs across aggregation"):
            self.evaluate(ranked_run_raws=self.mutate_run(3, mutate))

    def test_producer_identity_is_self_hashed_externally_bound_and_matrix_stable(self) -> None:
        for field in (
            "producer_code_sha256",
            "ranking_result_metadata_policy_sha256",
            "token_estimation_policy_sha256",
            "serialization_policy_sha256",
        ):
            with self.subTest(field=field):
                def mutate(run: dict[str, Any], changed_field: str = field) -> None:
                    producer = run["producer_identity"]
                    producer[changed_field] = _hash(f"different-{changed_field}")
                    run["producer_identity"] = relevance.finalize_producer_identity(producer)

                with self.assertRaisesRegex(
                    relevance.EvaluationError, "bound producer contract"
                ):
                    self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))
        with self.assertRaisesRegex(relevance.EvaluationError, "input bindings differ"):
            self.evaluate(
                producer_identity_raw=b"\n" + self.case["producer_identity_raw"]
            )

    def test_external_producer_self_hash_and_placeholder_hashes_fail_closed(self) -> None:
        producer = copy.deepcopy(self.case["producer_identity"])
        producer["producer_identity_sha256"] = _hash("tampered-producer-self-hash")
        with self.assertRaisesRegex(relevance.EvaluationError, "producer identity self-hash"):
            self.evaluate(producer_identity_raw=_raw(producer))

        producer = copy.deepcopy(self.case["producer_identity"])
        producer["producer_code_sha256"] = relevance.ZERO_SHA256
        producer = relevance.finalize_producer_identity(producer)
        with self.assertRaisesRegex(relevance.EvaluationError, "all-zero placeholder"):
            self.evaluate(producer_identity_raw=_raw(producer))

    def test_ranked_result_fact_hash_drift_fails_closed(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            run["queries"][0]["ordered_results"][0]["fact_sha256"] = _hash(
                "wrong-fact-bytes"
            )

        with self.assertRaisesRegex(
            relevance.EvaluationError, "fact SHA-256 differs from source membership"
        ):
            self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_delivered_result_token_estimate_must_be_positive(self) -> None:
        def mutate(run: dict[str, Any]) -> None:
            run["queries"][0]["ordered_results"][0]["estimated_tokens"] = 0

        with self.assertRaisesRegex(relevance.EvaluationError, "estimated_tokens must be positive"):
            self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_external_identity_and_binding_hash_placeholders_fail_closed(self) -> None:
        def zero_binding(run: dict[str, Any]) -> None:
            run["input_bindings"]["dataset_sha256"] = relevance.ZERO_SHA256

        def zero_producer(run: dict[str, Any]) -> None:
            producer = run["producer_identity"]
            producer["runner_sha256"] = relevance.ZERO_SHA256
            run["producer_identity"] = relevance.finalize_producer_identity(producer)

        def zero_engine(run: dict[str, Any]) -> None:
            run["engine_identity"]["binary_sha256"] = relevance.ZERO_SHA256

        def zero_fact(run: dict[str, Any]) -> None:
            run["queries"][0]["ordered_results"][0]["fact_sha256"] = relevance.ZERO_SHA256

        for label, mutate in (
            ("input binding", zero_binding),
            ("producer identity", zero_producer),
            ("engine identity", zero_engine),
            ("fact identity", zero_fact),
        ):
            with self.subTest(label=label):
                with self.assertRaisesRegex(relevance.EvaluationError, "all-zero placeholder"):
                    self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))

    def test_dataset_judgments_must_be_membership_bound_and_temporally_eligible(self) -> None:
        cases = (
            (
                "absent from source membership",
                {
                    "fact_id": "missing-1",
                    "fact_sha256": _hash("missing-1"),
                    "grade": "irrelevant",
                    "kind": "observation",
                    "cluster_id": "cluster-missing-1",
                },
            ),
            (
                "outside authenticated temporal eligibility",
                {
                    "fact_id": "future-1",
                    "fact_sha256": _hash("future-1"),
                    "grade": "irrelevant",
                    "kind": "observation",
                    "cluster_id": "cluster-future-1",
                },
            ),
        )
        for message, judgment in cases:
            with self.subTest(message=message):
                dataset = copy.deepcopy(self.case["dataset"])
                dataset["development"]["items"][0]["judgments"].append(judgment)
                with self.assertRaisesRegex(relevance.EvaluationError, message):
                    self.evaluate(dataset_raw=_raw(dataset))

        dataset = copy.deepcopy(self.case["dataset"])
        dataset["development"]["items"][0]["judgments"][0]["fact_sha256"] = _hash(
            "wrong-reviewed-fact-bytes"
        )
        with self.assertRaisesRegex(
            relevance.EvaluationError,
            "dataset judgment fact SHA-256 differs from source membership",
        ):
            self.evaluate(dataset_raw=_raw(dataset))

    def test_full_engine_aggregation_matrix_is_required(self) -> None:
        with self.assertRaisesRegex(relevance.EvaluationError, "full engine/aggregation matrix"):
            self.evaluate(ranked_run_raws=self.case["runs"][:-1])

    def test_ranked_run_self_hash_is_required(self) -> None:
        runs = list(self.case["runs"])
        run = json.loads(runs[0])
        run["run_sha256"] = "0" * 64
        runs[0] = _raw(run)
        with self.assertRaisesRegex(relevance.EvaluationError, "self-hash mismatch"):
            self.evaluate(ranked_run_raws=runs)

    def test_json_decoder_rejects_duplicate_keys_and_nonfinite_values(self) -> None:
        with self.assertRaisesRegex(relevance.EvaluationError, "duplicate JSON object key"):
            relevance.decode_json(b'{"schema_version":1,"schema_version":1}', "synthetic")
        with self.assertRaisesRegex(relevance.EvaluationError, "non-finite"):
            relevance.decode_json(b'{"value":NaN}', "synthetic")

    def test_rfc3339_nanoseconds_and_offsets_preserve_chronology(self) -> None:
        first = relevance._timestamp("2026-01-01T00:00:00.000000001Z", "first")[1]
        second = relevance._timestamp("2026-01-01T00:00:00.000000002Z", "second")[1]
        self.assertLess(first, second)

        def mutate(run: dict[str, Any]) -> None:
            run["created_at"] = "2026-04-01T01:00:00+05:00"

        report = self.evaluate(ranked_run_raws=self.mutate_run(0, mutate))
        self.assertEqual(report["generated_at"], "2026-04-01T00:00:00Z")
        with self.assertRaisesRegex(relevance.EvaluationError, "valid timestamp"):
            relevance._timestamp("2026-01-01T00:00:00+00:99", "invalid")

    def test_versioned_schemas_are_strict_and_text_free(self) -> None:
        schema_dir = pathlib.Path(__file__).with_name("schemas")
        schemas = {
            "offline-relevance-ranked-run-v1.schema.json": relevance.RANKED_RUN_SCHEMA,
            "offline-relevance-report-v1.schema.json": relevance.REPORT_SCHEMA,
            "offline-relevance-producer-identity-v1.schema.json": (
                relevance.PRODUCER_IDENTITY_SCHEMA
            ),
        }
        for name, identity in schemas.items():
            with self.subTest(schema=name):
                raw = (schema_dir / name).read_bytes()
                schema = json.loads(raw)
                self.assertEqual(schema["$schema"], "https://json-schema.org/draft/2020-12/schema")
                self.assertFalse(schema["additionalProperties"])
                self.assertEqual(schema["properties"]["schema"]["const"], identity)
                self.assertNotIn(b'"query_text"', raw)
                self.assertNotIn(b'"fact_text"', raw)

        ranked = json.loads(
            (schema_dir / "offline-relevance-ranked-run-v1.schema.json").read_bytes()
        )
        result_schema = ranked["$defs"]["result"]
        self.assertIn("fact_sha256", result_schema["required"])
        self.assertEqual(result_schema["properties"]["estimated_tokens"]["minimum"], 1)
        self.assertEqual(
            ranked["$defs"]["externalSha256"]["pattern"],
            "^(?!0{64}$)[0-9a-f]{64}$",
        )
        producer = json.loads(
            (schema_dir / "offline-relevance-producer-identity-v1.schema.json").read_bytes()
        )
        self.assertIn("producer_code_sha256", producer["required"])
        self.assertIn("producer_identity_sha256", producer["required"])


if __name__ == "__main__":
    unittest.main()
