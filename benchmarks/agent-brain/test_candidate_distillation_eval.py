from __future__ import annotations

import copy
import hashlib
import pathlib
import sys
import unittest

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import candidate_distillation_eval as EVAL


def output(provider: str, repetition: int, refs: list[str], emitted: list[str] | None = None) -> dict:
    return {"provider": provider, "repetition": repetition, "admitted_span_fact_ids": refs,
            "emitted_facts": [{"reference_id": ref, "faithful": True} for ref in (emitted if emitted is not None else refs)]}


def session(role: str, index: int) -> dict:
    refs = [f"{role}-{index}-a", f"{role}-{index}-b"]
    outputs = [output("deterministic", 1, refs)]
    source_content = f"sealed source {role} {index}"
    return {"session_id": f"{role}-session-{index}", "family_id": f"{role}-family-{index}", "stratum": "authority", "source_content": source_content,
            "source_content_sha256": hashlib.sha256(source_content.encode()).hexdigest(),
            "reference_facts": [{"id": refs[0], "authority_class": True}, {"id": refs[1]}],
            "expected_repetitions": {"deterministic": 1}, "outputs": {"candidate": copy.deepcopy(outputs), "legacy": copy.deepcopy(outputs)}}


def evidence() -> dict:
    development = [session("dev", i) for i in range(2)]
    confirmation = [session("confirm", i) for i in range(2)]
    retrieval = [{"family_id": row["family_id"], "stratum": "authority", "candidate": {"recall": 1, "precision": 1, "useful_per_1k": 2},
                  "legacy": {"recall": 1, "precision": 1, "useful_per_1k": 1}} for row in confirmation]
    value = {"schema": EVAL.SCHEMA, "contract": {"split_seed": "frozen", "source_cutoff": "2026-09-10T00:00:00Z", "strata": ["authority"],
            "corpus_sizes": {"development": 2, "confirmation": 2}, "provider_repetitions": {"deterministic": 1},
            "metric_version": EVAL.METRIC_VERSION,
            "bootstrap": {"method": "paired_stratified_cluster_bootstrap/v1", "resamples": 10000, "seed": 42},
            "power": {"development_only": True, "source_partition": "development", "one_sided_alpha": .05, "target_power": .80,
                      "recall_noninferiority_margin": .02, "required_independent_families": 2,
                      "calculation": {"method": "normal_approximation_paired_recall/v1", "observed_mean_difference": 0.0, "observed_sd": 0.0, "computed_required_independent_families": 2}}},
            "partitions": {"development": {"source_sessions": development}, "confirmation": {"source_sessions": confirmation, "retrieval_tasks": retrieval}}}
    value["preconfirmation_seal"] = {"schema": "candidate-distillation-preconfirmation-seal/v1",
        "contract_sha256": EVAL.canonical_sha256(value["contract"]),
        "development_roster_sha256": EVAL.canonical_sha256(EVAL.partition_roster(value["partitions"]["development"])),
        "confirmation_roster_sha256": EVAL.canonical_sha256(EVAL.partition_roster(value["partitions"]["confirmation"]))}
    value["scored_partition_digests"] = {role: EVAL.canonical_sha256(value["partitions"][role]) for role in ("development", "confirmation")}
    return value


def reseal(value: dict) -> None:
    value["preconfirmation_seal"]["contract_sha256"] = EVAL.canonical_sha256(value["contract"])
    for role in ("development", "confirmation"):
        value["preconfirmation_seal"][f"{role}_roster_sha256"] = EVAL.canonical_sha256(EVAL.partition_roster(value["partitions"][role]))
        value["scored_partition_digests"][role] = EVAL.canonical_sha256(value["partitions"][role])


class CandidateDistillationEvalTest(unittest.TestCase):
    def test_scores_complete_paired_evidence(self) -> None:
        report = EVAL.score(evidence())
        self.assertEqual(report["status"], "pass")
        self.assertTrue(all(report["gates"].values()))

    def test_missing_provider_coverage_fails_closed(self) -> None:
        value = evidence()
        value["partitions"]["confirmation"]["source_sessions"][0]["outputs"]["candidate"] = []
        reseal(value)
        with self.assertRaisesRegex(EVAL.EvidenceError, "coverage is missing"):
            EVAL.score(value)

    def test_rejects_family_overlap_and_unsealed_contract(self) -> None:
        value = evidence()
        value["partitions"]["confirmation"]["source_sessions"][0]["family_id"] = "dev-family-0"
        reseal(value)
        with self.assertRaisesRegex(EVAL.EvidenceError, "both development and confirmation"):
            EVAL.score(value)
        value = evidence()
        value["contract"]["bootstrap"]["resamples"] = 50
        reseal(value)
        with self.assertRaisesRegex(EVAL.EvidenceError, "10000"):
            EVAL.score(value)

    def test_rejects_reexported_source_and_tampered_partition(self) -> None:
        value = evidence()
        value["partitions"]["confirmation"]["source_sessions"][0]["source_content"] = value["partitions"]["development"]["source_sessions"][0]["source_content"]
        value["partitions"]["confirmation"]["source_sessions"][0]["source_content_sha256"] = value["partitions"]["development"]["source_sessions"][0]["source_content_sha256"]
        reseal(value)
        with self.assertRaisesRegex(EVAL.EvidenceError, "duplicate source content"):
            EVAL.score(value)
        value = evidence()
        value["partitions"]["confirmation"]["source_sessions"][0]["source_content"] = "tampered"
        with self.assertRaisesRegex(EVAL.EvidenceError, "source_content_sha256 does not match"):
            EVAL.score(value)

    def test_span_uses_complete_reference_denominator(self) -> None:
        value = evidence()
        row = value["partitions"]["confirmation"]["source_sessions"][0]
        missing = row["reference_facts"][1]["id"]
        for arm in ("candidate", "legacy"):
            row["outputs"][arm][0]["admitted_span_fact_ids"] = [row["reference_facts"][0]["id"]]
            row["outputs"][arm][0]["emitted_facts"] = [{"reference_id": row["reference_facts"][0]["id"], "faithful": True}]
        reseal(value)
        report = EVAL.score(value)
        self.assertLess(report["span_recall"]["all_facts"], .95)
        self.assertFalse(report["gates"]["all_fact_span_recall"])
        self.assertNotIn(missing, row["outputs"]["candidate"][0]["admitted_span_fact_ids"])

    def test_empty_negative_session_counts_emitted_false_positive(self) -> None:
        value = evidence()
        row = value["partitions"]["confirmation"]["source_sessions"][0]
        row["reference_facts"] = []
        for arm in ("candidate", "legacy"):
            row["outputs"][arm][0]["admitted_span_fact_ids"] = []
        row["outputs"]["candidate"][0]["emitted_facts"] = [{"reference_id": "unsupported", "faithful": False}]
        row["outputs"]["legacy"][0]["emitted_facts"] = []
        reseal(value)
        report = EVAL.score(value)
        self.assertFalse(report["gates"]["durable_precision_nonregression"])

    def test_rejects_selector_drift_and_bad_power_recomputation(self) -> None:
        value = evidence()
        row = value["partitions"]["confirmation"]["source_sessions"][0]
        row["expected_repetitions"] = {"deterministic": 2}
        for arm in ("candidate", "legacy"):
            row["outputs"][arm].append(output("deterministic", 2, [row["reference_facts"][0]["id"]]))
        with self.assertRaisesRegex(EVAL.EvidenceError, "selector drifted"):
            EVAL._output_metrics(row, "candidate", "fixture", {"deterministic": 2})
        value = evidence()
        value["contract"]["power"]["calculation"]["observed_sd"] = .1
        reseal(value)
        with self.assertRaisesRegex(EVAL.EvidenceError, "observed sd"):
            EVAL.score(value)

    def test_post_judgment_digest_detects_output_tampering(self) -> None:
        value = evidence()
        value["partitions"]["confirmation"]["source_sessions"][0]["outputs"]["candidate"][0]["emitted_facts"] = []
        with self.assertRaisesRegex(EVAL.EvidenceError, "scored confirmation partition digest mismatch"):
            EVAL.score(value)

    def test_development_negative_is_retained_in_power_inputs(self) -> None:
        value = evidence()
        row = value["partitions"]["development"]["source_sessions"][0]
        row["reference_facts"] = []
        for arm in ("candidate", "legacy"):
            row["outputs"][arm][0]["admitted_span_fact_ids"] = []
            row["outputs"][arm][0]["emitted_facts"] = []
        reseal(value)
        self.assertEqual(EVAL.score(value)["status"], "pass")


if __name__ == "__main__":
    unittest.main()
