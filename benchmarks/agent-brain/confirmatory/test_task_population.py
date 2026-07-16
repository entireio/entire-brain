from __future__ import annotations

import copy
import hashlib
import pathlib
import tempfile
import unittest

import task_population as population


def digest(label: str) -> str:
    return hashlib.sha256(label.encode("utf-8")).hexdigest()


def attributes(label: str) -> dict:
    return {
        "repository": "entireio/example",
        "task_family": f"family-{label}",
        "structural_difficulty": {
            "formula": "bounded_components_v1",
            "touched_file_count": 1,
            "language_count": 1,
            "validation_target_count": 1,
            "critical_invariant_count": 1,
            "cross_package": False,
            "requires_migration": False,
            "score": 4,
            "band": "low",
        },
        "memory_status": "memory_answerable",
        "validation_kind": "unit",
        "critical_failure_class": "correctness",
        "cache_state_design": "counterbalanced_cold_warm",
    }


def member(label: str, membership: str) -> dict:
    holdout = membership == "confirmatory_holdout"
    task_attributes = None if holdout else attributes(label)
    return {
        "member_ref": digest(f"member:{label}"),
        "membership": membership,
        "task_id": None if holdout else f"task-{label}",
        "task_id_sha256": None if holdout else digest(f"task:{label}"),
        "task_overlap_commitment_sha256": digest(f"task-overlap:{label}"),
        "repository_commitment_sha256": digest("repository:entireio/example"),
        "family_commitment_sha256": digest(f"related-family:{label}"),
        "attributes": task_attributes,
        "attributes_sha256": digest(f"sealed-attributes:{label}"),
        "artifacts": {
            "prompt_commitment_sha256": digest(f"prompt:{label}"),
            "patch_commitment_sha256": digest(f"patch:{label}"),
            "validation_commitment_sha256": digest(f"validation:{label}"),
            "source_history_commitment_sha256": digest(f"source-history:{label}"),
            "fix_commitment_sha256": digest(f"fix:{label}"),
        },
        "source_session_commitments": [digest(f"session:{label}")],
        "contamination_state": "commitment_only" if holdout else "reviewed_clear",
        "review_commitment_sha256": "0" * 64,
    }


def review(item: dict) -> dict:
    return {
        "member_ref": item["member_ref"],
        "task_id": item["task_id"],
        "prompt_commitment_sha256": item["artifacts"]["prompt_commitment_sha256"],
        "reviewer": "reviewer-1",
        "reviewed_at": "2026-07-16T00:00:00Z",
        "disposition": "approved_symptom_only",
        "checks": {
            "symptom_only": True,
            "no_answer_bearing_file_hint": True,
            "no_answer_bearing_function_hint": True,
            "no_answer_bearing_workflow_hint": True,
            "hidden_validation_isolated": True,
            "retrieval_query_neutral": True,
        },
        "rationale_code": "all_checks_pass",
        "rationale_sha256": digest(f"rationale:{item['member_ref']}"),
        "review_sha256": "0" * 64,
    }


def fixture(*, calibration_count: int = 1, status: str = "development_build") -> tuple[dict, dict]:
    members = [member("optimization-1", "development_optimization")]
    members.extend(
        member(f"calibration-{index}", "development_calibration")
        for index in range(1, calibration_count + 1)
    )
    members.append(member("holdout-commitment-1", "confirmatory_holdout"))
    ledger = {
        "schema_version": 2,
        "profile": "agent_brain_task_review_ledger_v2",
        "hash_algorithm": "sha256_canonical_json_null_self_v1",
        "ledger_sha256": "0" * 64,
        "reviews": [review(item) for item in members],
    }
    ledger = population.seal_review_ledger(ledger)
    contract = {
        "schema_version": 2,
        "profile": "agent_brain_task_population_v2",
        "status": status,
        "hash_algorithm": "sha256_canonical_json_null_self_v1",
        "population_sha256": "0" * 64,
        "selection_policy": {
            "eligibility_rule": "post_cutoff_source_plus_test_reverse_patch_v1",
            "eligibility_ledger_schema_version": 1,
            "eligibility_ledger_path": "benchmarks/agent-brain/task-eligibility-ledger-v1.json",
            "eligibility_ledger_sha256": digest("synthetic-eligibility-ledger"),
            "eligible_task_count": len(members),
            "selection_mode": "all_eligible",
            "selection_seed_commitment_sha256": None,
            "selection_timing": "locked_before_retrieval_or_treatment_observation",
            "selection_verification_status": "verified" if status != "development_build" else "pending",
            "selection_receipt_sha256": digest("synthetic-selection-receipt") if status != "development_build" else None,
        },
        "split_policy": {
            "development_optimization_usage": "product_iteration",
            "development_calibration_usage": "variance_only_after_candidate_lock",
            "confirmatory_holdout_usage": "commitment_only_until_protocol_freeze",
            "cross_split_overlap_policy": "reject_prompt_patch_fix_session_family_overlap",
            "assignment_scheme": "hmac_sha256_owner_key_v1",
            "assignment_key_sha256": digest("synthetic-owner-assignment-key"),
            "assignment_verification_status": "verified" if status != "development_build" else "pending",
            "assignment_receipt_sha256": digest("synthetic-assignment-receipt") if status != "development_build" else None,
            "overlap_commitment_scheme": "hmac_sha256_owner_key_v1",
            "overlap_commitment_key_sha256": digest("synthetic-owner-overlap-key"),
            "overlap_commitment_verification_status": "verified" if status != "development_build" else "pending",
            "overlap_commitment_receipt_sha256": digest("synthetic-overlap-receipt") if status != "development_build" else None,
            "calibration_min_independent_tasks": 12,
            "holdout_plaintext": "forbidden",
        },
        "review_ledger": {
            "schema_version": 2,
            "path": "benchmarks/agent-brain/task-review-ledger-v2.json",
            "sha256": "0" * 64,
        },
        "summary": {
            "total_members": 0,
            "active_members": 0,
            "excluded_members": 0,
            "membership_counts": {
                "development_optimization": 0,
                "development_calibration": 0,
                "confirmatory_holdout": 0,
            },
            "unique_prompt_commitments": 0,
            "unique_patch_commitments": 0,
            "related_edge_count": 0,
            "holdout_plaintext_fields": 0,
        },
        "members": members,
        "related_task_edges": [],
    }
    return population.seal_population(contract, ledger), ledger


def reseal(contract: dict, ledger: dict) -> tuple[dict, dict]:
    new_ledger = population.seal_review_ledger(ledger)
    return population.seal_population(contract, new_ledger), new_ledger


class TaskPopulationV2Test(unittest.TestCase):
    def assert_invalid(self, contract: dict, ledger: dict, phrase: str) -> None:
        with self.assertRaisesRegex(population.TaskPopulationError, phrase):
            population.validate_population(contract, ledger)

    def test_valid_development_contract_and_commitment_only_holdout(self) -> None:
        contract, ledger = fixture()
        summary = population.validate_population(contract, ledger)
        self.assertEqual(summary["total_members"], 3)
        self.assertEqual(summary["holdout_plaintext_fields"], 0)
        holdout = next(item for item in contract["members"] if item["membership"] == "confirmatory_holdout")
        self.assertIsNone(holdout["task_id"])
        self.assertIsNone(holdout["attributes"])

    def test_candidate_requires_twelve_independent_calibration_tasks(self) -> None:
        contract, ledger = fixture(status="candidate_unopened")
        self.assert_invalid(contract, ledger, "fewer than 12 independent calibration tasks")
        contract, ledger = fixture(calibration_count=12, status="candidate_unopened")
        summary = population.validate_population(contract, ledger)
        self.assertEqual(summary["membership_counts"]["development_calibration"], 12)

        contract, ledger = fixture(calibration_count=12, status="candidate_unopened")
        contract = copy.deepcopy(contract)
        contract["split_policy"]["overlap_commitment_verification_status"] = "pending"
        contract["split_policy"]["overlap_commitment_receipt_sha256"] = None
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "requires verified blind overlap commitments")

    def test_selection_universe_is_bound_before_retrieval_or_treatment(self) -> None:
        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["selection_policy"]["eligible_task_count"] += 1
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "account for every eligible task")

        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["selection_policy"]["selection_mode"] = "preregistered_seeded_sample"
        contract["selection_policy"]["eligible_task_count"] += 2
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "requires a non-placeholder seed commitment")

        contract, ledger = fixture(status="candidate_unopened", calibration_count=12)
        contract = copy.deepcopy(contract)
        contract["selection_policy"]["selection_verification_status"] = "pending"
        contract["selection_policy"]["selection_receipt_sha256"] = None
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "verified treatment-blind task selection")

    def test_candidate_requires_verified_split_assignment_and_no_placeholders(self) -> None:
        contract, ledger = fixture(status="candidate_unopened", calibration_count=12)
        contract = copy.deepcopy(contract)
        contract["split_policy"]["assignment_verification_status"] = "pending"
        contract["split_policy"]["assignment_receipt_sha256"] = None
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "verified deterministic split assignment")

        contract, ledger = fixture(status="candidate_unopened", calibration_count=12)
        contract = copy.deepcopy(contract)
        contract["members"][0]["repository_commitment_sha256"] = "0" * 64
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "placeholder SHA-256 fields")

    def test_prompt_and_patch_commitments_are_globally_unique(self) -> None:
        for field, phrase in (
            ("prompt_commitment_sha256", "prompt commitments"),
            ("patch_commitment_sha256", "patch commitments"),
        ):
            contract, ledger = fixture()
            contract = copy.deepcopy(contract)
            ledger = copy.deepcopy(ledger)
            contract["members"][1]["artifacts"][field] = contract["members"][0]["artifacts"][field]
            if field == "prompt_commitment_sha256":
                ledger["reviews"][1]["prompt_commitment_sha256"] = ledger["reviews"][0]["prompt_commitment_sha256"]
            contract, ledger = reseal(contract, ledger)
            self.assert_invalid(contract, ledger, phrase)

    def test_fix_session_and_related_family_cannot_cross_active_splits(self) -> None:
        cases = (
            ("fix", "active splits share a fix-commit"),
            ("session", "active splits share a source-session"),
            ("family", "active splits share a materially related task-family"),
        )
        for kind, phrase in cases:
            contract, ledger = fixture()
            contract = copy.deepcopy(contract)
            if kind == "fix":
                contract["members"][1]["artifacts"]["fix_commitment_sha256"] = contract["members"][0]["artifacts"]["fix_commitment_sha256"]
            elif kind == "session":
                contract["members"][1]["source_session_commitments"] = contract["members"][0]["source_session_commitments"]
            else:
                contract["members"][1]["family_commitment_sha256"] = contract["members"][0]["family_commitment_sha256"]
            contract, ledger = reseal(contract, ledger)
            self.assert_invalid(contract, ledger, phrase)

    def test_task_overlap_commitments_are_unique_and_blinded(self) -> None:
        key = b"k" * 32
        first = population.overlap_commitment(key, "task", b"task-a")
        self.assertEqual(first, population.overlap_commitment(key, "task", b"task-a"))
        self.assertNotEqual(first, population.overlap_commitment(key, "prompt", b"task-a"))
        self.assertNotEqual(first, hashlib.sha256(b"task-a").hexdigest())
        with self.assertRaisesRegex(population.TaskPopulationError, "at least 32 bytes"):
            population.overlap_commitment(b"short", "task", b"task-a")

        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["members"][1]["task_overlap_commitment_sha256"] = contract["members"][0][
            "task_overlap_commitment_sha256"
        ]
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "duplicate task-identity commitment")

        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["split_policy"]["overlap_commitment_key_sha256"] = "0" * 64
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "key receipt cannot be a placeholder")

    def test_material_related_edge_cannot_cross_splits(self) -> None:
        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["related_task_edges"].append(
            {
                "left_member_ref": contract["members"][0]["member_ref"],
                "right_member_ref": contract["members"][1]["member_ref"],
                "relation_kind": "task_family",
                "material": True,
                "evidence_sha256": digest("edge-evidence"),
                "disposition": "retain_same_split",
            }
        )
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "materially crosses population splits")

    def test_holdout_plaintext_shape_is_rejected(self) -> None:
        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        holdout = contract["members"][-1]
        holdout["task_id"] = "revealed-holdout-task"
        holdout["attributes"] = attributes("revealed-holdout")
        contract["population_sha256"] = population._self_hash(contract, "population_sha256")
        self.assert_invalid(contract, ledger, "confirmatory_holdout")

    def test_difficulty_score_and_band_are_derived(self) -> None:
        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        difficulty = contract["members"][0]["attributes"]["structural_difficulty"]
        difficulty["score"] = 5
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "structural difficulty score is not derived")

        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        difficulty = contract["members"][0]["attributes"]["structural_difficulty"]
        difficulty["band"] = "medium"
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "structural difficulty band is not derived")

    def test_review_coverage_and_all_explicit_checks_are_required(self) -> None:
        contract, ledger = fixture()
        ledger = copy.deepcopy(ledger)
        ledger["reviews"][0]["checks"]["retrieval_query_neutral"] = False
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "incomplete task-validity checklist")

        contract, ledger = fixture()
        ledger = copy.deepcopy(ledger)
        ledger["reviews"].pop()
        contract, ledger = reseal(contract, ledger)
        self.assert_invalid(contract, ledger, "review ledger membership does not exactly match")

    def test_self_hashes_and_summary_fail_closed(self) -> None:
        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["members"][0]["task_overlap_commitment_sha256"] = digest("tampered")
        self.assert_invalid(contract, ledger, "population self hash")

        contract, ledger = fixture()
        ledger = copy.deepcopy(ledger)
        ledger["reviews"][0]["reviewer"] = "reviewer-2"
        self.assert_invalid(contract, ledger, "review ledger self hash")

        contract, ledger = fixture()
        contract = copy.deepcopy(contract)
        contract["summary"]["total_members"] = 999
        contract["population_sha256"] = population._self_hash(contract, "population_sha256")
        self.assert_invalid(contract, ledger, "summary does not match")

    def test_duplicate_json_keys_and_floats_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "duplicate.json"
            path.write_text('{"schema_version":2,"schema_version":2}', encoding="utf-8")
            with self.assertRaisesRegex(population.TaskPopulationError, "duplicate object key"):
                population.load_json(path)
        with self.assertRaisesRegex(population.TaskPopulationError, "forbids floating-point"):
            population.canonical_json_bytes({"bad": 1.0})


if __name__ == "__main__":
    unittest.main()
