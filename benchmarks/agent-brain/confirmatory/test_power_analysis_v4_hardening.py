from __future__ import annotations

import contextlib
import copy
import hashlib
import io
import json
import math
import pathlib
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import draft202012 as DRAFT
import power_analysis_v4 as V4
import test_power_analysis_v4 as FIX


PRODUCT = V4.PRODUCT
TASK = V4.TASK_POPULATION


def digest(label: str) -> str:
    return PRODUCT.value_sha256({"hardening-fixture": label})


def raw_json(value: object) -> bytes:
    return (
        json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def raw_sha(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


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


def population_member(
    label: str,
    membership: str,
    *,
    task_id: str | None,
    contamination_state: str,
) -> dict:
    holdout = membership == "confirmatory_holdout"
    return {
        "member_ref": digest(f"member:{label}"),
        "membership": membership,
        "task_id": None if holdout else task_id,
        "task_id_sha256": None if holdout else digest(f"task-id:{label}"),
        "task_overlap_commitment_sha256": digest(f"overlap:{label}"),
        "repository_commitment_sha256": digest("repository:entireio/example"),
        "family_commitment_sha256": digest(f"family:{label}"),
        "attributes": None if holdout else attributes(label),
        "attributes_sha256": digest(f"attributes:{label}"),
        "artifacts": {
            "prompt_commitment_sha256": digest(f"prompt:{label}"),
            "patch_commitment_sha256": digest(f"patch:{label}"),
            "validation_commitment_sha256": digest(f"validation:{label}"),
            "source_history_commitment_sha256": digest(f"history:{label}"),
            "fix_commitment_sha256": digest(f"fix:{label}"),
        },
        "source_session_commitments": [digest(f"session:{label}")],
        "contamination_state": contamination_state,
        "review_commitment_sha256": digest(f"review-placeholder:{label}"),
    }


def review(member: dict) -> dict:
    excluded = member["contamination_state"] == "excluded"
    return {
        "member_ref": member["member_ref"],
        "task_id": member["task_id"],
        "prompt_commitment_sha256": member["artifacts"][
            "prompt_commitment_sha256"
        ],
        "reviewer": "fixture-reviewer",
        "reviewed_at": "2026-07-16T00:00:00Z",
        "disposition": (
            "exclude_oracle_assisted" if excluded else "approved_symptom_only"
        ),
        "checks": {
            "symptom_only": not excluded,
            "no_answer_bearing_file_hint": not excluded,
            "no_answer_bearing_function_hint": not excluded,
            "no_answer_bearing_workflow_hint": not excluded,
            "hidden_validation_isolated": not excluded,
            "retrieval_query_neutral": not excluded,
        },
        "rationale_code": "overlap_or_contamination" if excluded else "all_checks_pass",
        "rationale_sha256": digest(f"review-rationale:{member['member_ref']}"),
        "review_sha256": digest(f"review-unsealed:{member['member_ref']}"),
    }


def sealed(value: dict, field: str = "identity_sha256") -> dict:
    value[field] = PRODUCT.self_sha256(value, field)
    return value


class CalibrationCase:
    def __init__(
        self,
        *,
        state: str = "candidate",
        evidence_class: str = "synthetic_fixture",
        homogeneous: bool = True,
        structural_zero: bool = False,
        duplicate_task_hash: bool = False,
        related_calibration_pair: bool = False,
        shared_fix_pair: bool = False,
        shared_session_pair: bool = False,
        excluded_bridge: bool = False,
    ) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.paths: dict[str, pathlib.Path] = {}
        self.state = state
        self.evidence_class = evidence_class
        self.owner_path: pathlib.Path | None = None
        try:
            self._build(
                homogeneous=homogeneous,
                structural_zero=structural_zero,
                duplicate_task_hash=duplicate_task_hash,
                related_calibration_pair=related_calibration_pair,
                shared_fix_pair=shared_fix_pair,
                shared_session_pair=shared_session_pair,
                excluded_bridge=excluded_bridge,
            )
        except BaseException:
            self.temporary.cleanup()
            raise

    def __enter__(self) -> "CalibrationCase":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def close(self) -> None:
        self.temporary.cleanup()

    def write(self, name: str, value: dict) -> pathlib.Path:
        path = self.root / f"{name}.json"
        path.write_bytes(raw_json(value))
        self.paths[name] = path
        return path

    def artifact_args(self) -> dict:
        return {
            "product_cycle": self.paths["product"],
            "task_population": self.paths["population"],
            "review_ledger": self.paths["ledger"],
            "selection_receipt": self.paths["selection"],
            "assignment_receipt": self.paths["assignment"],
            "overlap_receipt": self.paths["overlap"],
            "candidate_lock_receipt": self.paths["candidate"],
            "owner_approval_receipt": self.owner_path,
        }

    def reload_bundle(self) -> V4.ArtifactBundle:
        return V4.load_artifact_bundle(**self.artifact_args())

    def _verification_receipt(self, kind: str, population: dict) -> dict:
        return sealed(
            {
                "schema": "agent-brain-task-population-verification-receipt/v1",
                "kind": kind,
                "status": "verified",
                "verification_subject_schema": V4.POPULATION_RECEIPT_SUBJECT_SCHEMA,
                "verification_subject_sha256": V4._population_receipt_subject_sha256(
                    population, kind
                ),
                "verifier": "fixture-verifier",
                "verified_at": "2026-07-16T00:00:00Z",
                "authentication_status": V4.NO_TRUST_ANCHOR,
                "identity_sha256": digest(f"{kind}-unsealed"),
            }
        )

    def _population(
        self,
        *,
        related_calibration_pair: bool,
        shared_fix_pair: bool,
        shared_session_pair: bool,
        excluded_bridge: bool,
    ) -> tuple[dict, dict]:
        members = [
            population_member(
                "optimization",
                "development_optimization",
                task_id="optimization-task",
                contamination_state="reviewed_clear",
            )
        ]
        calibration_members = [
            population_member(
                f"calibration-{index:02d}",
                "development_calibration",
                task_id=f"task-{index:02d}",
                contamination_state="reviewed_clear",
            )
            for index in range(12)
        ]
        if related_calibration_pair:
            calibration_members[1]["family_commitment_sha256"] = calibration_members[
                0
            ]["family_commitment_sha256"]
        if shared_fix_pair:
            calibration_members[1]["artifacts"][
                "fix_commitment_sha256"
            ] = calibration_members[0]["artifacts"]["fix_commitment_sha256"]
        if shared_session_pair:
            calibration_members[1]["source_session_commitments"] = list(
                calibration_members[0]["source_session_commitments"]
            )
        members.extend(calibration_members)
        bridge: dict | None = None
        if excluded_bridge:
            bridge = population_member(
                "excluded-bridge",
                "development_optimization",
                task_id="excluded-bridge-task",
                contamination_state="excluded",
            )
            members.append(bridge)
        members.append(
            population_member(
                "holdout",
                "confirmatory_holdout",
                task_id=None,
                contamination_state="commitment_only",
            )
        )
        ledger = TASK.seal_review_ledger(
            {
                "schema_version": 2,
                "profile": "agent_brain_task_review_ledger_v2",
                "hash_algorithm": "sha256_canonical_json_null_self_v1",
                "ledger_sha256": digest("ledger-unsealed"),
                "reviews": [review(member) for member in members],
            }
        )
        selection_policy = {
            "eligibility_rule": "post_cutoff_source_plus_test_reverse_patch_v1",
            "eligibility_ledger_schema_version": 1,
            "eligibility_ledger_path": "benchmarks/agent-brain/task-eligibility-ledger-v1.json",
            "eligibility_ledger_sha256": digest("eligibility-ledger"),
            "eligible_task_count": len(members),
            "selection_mode": "all_eligible",
            "selection_seed_commitment_sha256": None,
            "selection_timing": "locked_before_retrieval_or_treatment_observation",
            "selection_verification_status": "verified",
            "selection_receipt_sha256": digest("selection-placeholder"),
        }
        split_policy = {
            "development_optimization_usage": "product_iteration",
            "development_calibration_usage": "variance_only_after_candidate_lock",
            "confirmatory_holdout_usage": "commitment_only_until_protocol_freeze",
            "cross_split_overlap_policy": "reject_prompt_patch_fix_session_family_overlap",
            "assignment_scheme": "hmac_sha256_owner_key_v1",
            "assignment_key_sha256": digest("assignment-key"),
            "assignment_verification_status": "verified",
            "assignment_receipt_sha256": digest("assignment-placeholder"),
            "overlap_commitment_scheme": "hmac_sha256_owner_key_v1",
            "overlap_commitment_key_sha256": digest("overlap-key"),
            "overlap_commitment_verification_status": "verified",
            "overlap_commitment_receipt_sha256": digest("overlap-placeholder"),
            "calibration_min_independent_tasks": 12,
            "holdout_plaintext": "forbidden",
        }
        related_edges: list[dict] = []
        if bridge is not None:
            for index in (0, 1):
                related_edges.append(
                    {
                        "left_member_ref": calibration_members[index]["member_ref"],
                        "right_member_ref": bridge["member_ref"],
                        "relation_kind": "fix_lineage",
                        "material": True,
                        "evidence_sha256": digest(f"bridge-edge:{index}"),
                        "disposition": "exclude_one",
                    }
                )
        population = {
            "schema_version": 2,
            "profile": V4.TASK_POPULATION_PROFILE,
            "status": "frozen_unopened" if self.state == "frozen" else "candidate_unopened",
            "hash_algorithm": "sha256_canonical_json_null_self_v1",
            "population_sha256": digest("population-unsealed"),
            "selection_policy": selection_policy,
            "split_policy": split_policy,
            "review_ledger": {
                "schema_version": 2,
                "path": "benchmarks/agent-brain/task-review-ledger-v2.json",
                "sha256": digest("ledger-link-unsealed"),
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
            "related_task_edges": related_edges,
        }
        provisional = TASK.seal_population(population, ledger)
        selection_path = self.write(
            "selection", self._verification_receipt("selection", provisional)
        )
        assignment_path = self.write(
            "assignment", self._verification_receipt("assignment", provisional)
        )
        overlap_path = self.write(
            "overlap",
            self._verification_receipt("overlap_commitment", provisional),
        )
        population["selection_policy"]["selection_receipt_sha256"] = raw_sha(
            selection_path.read_bytes()
        )
        population["split_policy"]["assignment_receipt_sha256"] = raw_sha(
            assignment_path.read_bytes()
        )
        population["split_policy"][
            "overlap_commitment_receipt_sha256"
        ] = raw_sha(overlap_path.read_bytes())
        return TASK.seal_population(population, ledger), ledger

    def _execution_contract(self, product: dict) -> dict:
        shared = product["shared_execution"]
        return sealed(
            {
                "schema": "agent-brain-power-v4-execution-contract/v1",
                "status": "locked_before_calibration_opening",
                "provider_id": "fixture-provider",
                "agent_cli_id": "codex",
                "agent_cli_version": "5.6-fixture",
                "requested_model_id": "gpt-5.6-sol-fixture",
                "resolved_model_id": shared["model_id"],
                "effort": shared["effort"],
                "runner_sha256": shared["runner_sha256"],
                "timeout_policy_sha256": digest("timeout-policy"),
                "agent_timeout_limit_seconds": 900,
                "timeout_component_limit_seconds": None,
                "identity_sha256": digest("execution-contract-unsealed"),
            }
        )

    def _execution_attestations(
        self, product: dict, contract: dict
    ) -> list[dict]:
        common = {
            field: contract[field]
            for field in (
                "provider_id",
                "agent_cli_id",
                "agent_cli_version",
                "requested_model_id",
                "resolved_model_id",
                "effort",
                "runner_sha256",
                "timeout_policy_sha256",
                "agent_timeout_limit_seconds",
                "timeout_component_limit_seconds",
            )
        }
        values = []
        for cell in sorted(product["cells"], key=lambda item: item["run_id"]):
            values.append(
                sealed(
                    {
                        "run_id": cell["run_id"],
                        "cell_identity_sha256": cell["identity_sha256"],
                        **common,
                        "identity_sha256": digest(
                            f"attestation-unsealed:{cell['run_id']}"
                        ),
                    }
                )
            )
        return values

    def _planning(
        self,
        *,
        product: dict,
        population_binding: dict,
        bundle: V4.ArtifactBundle,
        execution_contract: dict,
        implementation_lock: dict,
    ) -> dict:
        development = self.evidence_class == "development_measurement"
        return sealed(
            {
                "schema": "agent-brain-final-calibration-plan/v1",
                "profile": (
                    "production_development_measurement_v1"
                    if development
                    else "synthetic_fixture_test_v1"
                ),
                "method": V4.RESAMPLING_METHOD,
                "target_power": V4.TARGET_POWER,
                "confidence": V4.CONFIDENCE,
                "resamples": V4.PRODUCTION_RESAMPLES if development else 100,
                "seed": V4.PRODUCTION_SEED if development else 2_026_071_600,
                "candidate_task_clusters": (
                    list(V4.PRODUCTION_CANDIDATE_GRID) if development else [12]
                ),
                "arms": list(PRODUCT.ARMS),
                "repetitions_per_arm": product["design"]["repetitions_per_arm"],
                "agent_retry_limit": 0,
                "replacement_cell_limit": 0,
                "reserve_cell_limit": 0,
                "planning_alternatives": FIX.alternatives(),
                "precalibration_product_plan_sha256": V4._precalibration_plan_sha256(
                    product
                ),
                "task_population_contract_sha256": population_binding[
                    "population_sha256"
                ],
                "task_population_file_sha256": bundle.task_population.sha256,
                "review_ledger_file_sha256": bundle.review_ledger.sha256,
                "selection_receipt_file_sha256": bundle.selection_receipt.sha256,
                "assignment_receipt_file_sha256": bundle.assignment_receipt.sha256,
                "overlap_receipt_file_sha256": bundle.overlap_receipt.sha256,
                "execution_contract_sha256": execution_contract["identity_sha256"],
                "v4_implementation_lock_sha256": implementation_lock[
                    "identity_sha256"
                ],
                "identity_sha256": digest("plan-unsealed"),
            }
        )

    def _build(
        self,
        *,
        homogeneous: bool,
        structural_zero: bool,
        duplicate_task_hash: bool,
        related_calibration_pair: bool,
        shared_fix_pair: bool,
        shared_session_pair: bool,
        excluded_bridge: bool,
    ) -> None:
        product = FIX.product_cycle_fixture(
            evidence_class=self.evidence_class,
            homogeneous=homogeneous,
            structural_zero=structural_zero,
            duplicate_task_hash=duplicate_task_hash,
        )
        population, ledger = self._population(
            related_calibration_pair=related_calibration_pair,
            shared_fix_pair=shared_fix_pair,
            shared_session_pair=shared_session_pair,
            excluded_bridge=excluded_bridge,
        )
        self.write("product", product)
        self.write("population", population)
        self.write("ledger", ledger)
        placeholder_candidate = sealed(
            {
                "schema": "agent-brain-candidate-lock-receipt/v1",
                "status": "locked_before_calibration_opening",
                "complete_plan_sha256": digest("placeholder-plan"),
                "candidate_product_identity_sha256": digest("placeholder-candidate"),
                "task_population_file_sha256": digest("placeholder-population"),
                "issuer": "fixture-owner",
                "locked_at": "2026-07-16T00:00:00Z",
                "authentication_status": V4.NO_TRUST_ANCHOR,
                "identity_sha256": digest("candidate-receipt-unsealed"),
            }
        )
        self.write("candidate", placeholder_candidate)
        provisional = self.reload_bundle()
        population_binding, _, _ = V4._derive_population_binding(
            bundle=provisional, product_root=product
        )
        implementation_lock = V4._expected_implementation_lock(provisional)
        execution_contract = self._execution_contract(product)
        execution_attestations = self._execution_attestations(
            product, execution_contract
        )
        planning = self._planning(
            product=product,
            population_binding=population_binding,
            bundle=provisional,
            execution_contract=execution_contract,
            implementation_lock=implementation_lock,
        )
        candidate_receipt = sealed(
            {
                "schema": "agent-brain-candidate-lock-receipt/v1",
                "status": "locked_before_calibration_opening",
                "complete_plan_sha256": planning["identity_sha256"],
                "candidate_product_identity_sha256": product[
                    "product_identities"
                ]["candidate"]["identity_sha256"],
                "task_population_file_sha256": provisional.task_population.sha256,
                "issuer": "fixture-owner",
                "locked_at": "2026-07-16T00:00:00Z",
                "authentication_status": V4.NO_TRUST_ANCHOR,
                "identity_sha256": digest("candidate-receipt-unsealed"),
            }
        )
        self.write("candidate", candidate_receipt)
        bundle = self.reload_bundle()
        if self.state == "frozen":
            owner_receipt = sealed(
                {
                    "schema": "agent-brain-owner-approval-receipt/v1",
                    "status": "structurally_approved_not_authenticated",
                    "complete_plan_sha256": planning["identity_sha256"],
                    "candidate_lock_receipt_file_sha256": bundle.candidate_lock_receipt.sha256,
                    "approver": "fixture-owner",
                    "approved_at": "2026-07-16T00:00:00Z",
                    "authentication_status": V4.NO_TRUST_ANCHOR,
                    "identity_sha256": digest("owner-receipt-unsealed"),
                }
            )
            self.owner_path = self.write("owner", owner_receipt)
            bundle = self.reload_bundle()
        candidate_lock = sealed(
            {
                "status": "structurally_locked_before_development_calibration",
                "candidate_product_identity_sha256": product[
                    "product_identities"
                ]["candidate"]["identity_sha256"],
                "complete_plan_sha256": planning["identity_sha256"],
                "task_population_contract_sha256": population_binding[
                    "population_sha256"
                ],
                "task_population_file_sha256": bundle.task_population.sha256,
                "lock_receipt_file_sha256": bundle.candidate_lock_receipt.sha256,
                "authentication_status": V4.NO_TRUST_ANCHOR,
                "identity_sha256": digest("candidate-lock-unsealed"),
            }
        )
        source_bindings = sealed(
            {
                "product_cycle_file_sha256": bundle.product_cycle.sha256,
                "product_cycle_schema_file_sha256": bundle.schemas[
                    "product_cycle"
                ].sha256,
                "product_cycle_contract_sha256": product["manifest_sha256"],
                "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
                    product
                ),
                "task_population_schema_file_sha256": bundle.schemas[
                    "task_population"
                ].sha256,
                "task_population_contract_file_sha256": bundle.task_population.sha256,
                "task_population_contract_sha256": population_binding[
                    "population_sha256"
                ],
                "task_population_binding_canonical_bytes_sha256": PRODUCT.value_sha256(
                    population_binding
                ),
                "review_ledger_schema_file_sha256": bundle.schemas[
                    "review_ledger"
                ].sha256,
                "review_ledger_file_sha256": bundle.review_ledger.sha256,
                "review_ledger_sha256": ledger["ledger_sha256"],
                "selection_receipt_file_sha256": bundle.selection_receipt.sha256,
                "assignment_receipt_file_sha256": bundle.assignment_receipt.sha256,
                "overlap_receipt_file_sha256": bundle.overlap_receipt.sha256,
                "candidate_lock_receipt_file_sha256": bundle.candidate_lock_receipt.sha256,
                "owner_approval_receipt_file_sha256": (
                    bundle.owner_approval_receipt.sha256
                    if bundle.owner_approval_receipt is not None
                    else None
                ),
                "v4_implementation_lock_sha256": implementation_lock[
                    "identity_sha256"
                ],
                "execution_attestations_sha256": PRODUCT.value_sha256(
                    execution_attestations
                ),
                "identity_sha256": digest("source-bindings-unsealed"),
            }
        )
        shared = product["shared_execution"]
        candidate = product["product_identities"]["candidate"]
        locked_identities = sealed(
            {
                "product_cycle_schema_sha256": bundle.schemas["product_cycle"].sha256,
                "product_cycle_file_sha256": bundle.product_cycle.sha256,
                "product_cycle_contract_sha256": product["manifest_sha256"],
                "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
                    product
                ),
                "task_population_schema_sha256": bundle.schemas[
                    "task_population"
                ].sha256,
                "task_population_sha256": population_binding["population_sha256"],
                "task_population_file_sha256": bundle.task_population.sha256,
                "review_ledger_file_sha256": bundle.review_ledger.sha256,
                "candidate_product_identity_sha256": candidate["identity_sha256"],
                "candidate_packet_format_sha256": candidate["packet_format"]["sha256"],
                "corpus_sha256": shared["corpus_sha256"],
                "engine_sha256": shared["engine_sha256"],
                "prompt_template_sha256": shared["prompt_template_sha256"],
                "prompt_parity_algorithm": shared["prompt_parity_algorithm"],
                "cache_policy_sha256": shared["cache_policy_sha256"],
                "runner_sha256": shared["runner_sha256"],
                "model_id": shared["model_id"],
                "provider_id": execution_contract["provider_id"],
                "agent_cli_id": execution_contract["agent_cli_id"],
                "agent_cli_version": execution_contract["agent_cli_version"],
                "requested_model_id": execution_contract["requested_model_id"],
                "resolved_model_id": execution_contract["resolved_model_id"],
                "effort": shared["effort"],
                "timeout_policy_sha256": execution_contract[
                    "timeout_policy_sha256"
                ],
                "agent_timeout_limit_seconds": execution_contract[
                    "agent_timeout_limit_seconds"
                ],
                "timeout_component_limit_seconds": execution_contract[
                    "timeout_component_limit_seconds"
                ],
                "schedule_sha256": shared["schedule_sha256"],
                "price_quote_sha256": shared["price_quote_sha256"],
                "pricing_policy_sha256": shared["pricing_policy_sha256"],
                "execution_contract_sha256": execution_contract["identity_sha256"],
                "v4_implementation_lock_sha256": implementation_lock[
                    "identity_sha256"
                ],
                "identity_sha256": digest("locked-identities-unsealed"),
            }
        )
        self.calibration = sealed(
            {
                "schema": V4.CALIBRATION_SCHEMA,
                "evidence_class": self.evidence_class,
                "state": self.state,
                "owner_approval_receipt_sha256": (
                    bundle.owner_approval_receipt.sha256
                    if bundle.owner_approval_receipt is not None
                    else None
                ),
                "candidate_lock": candidate_lock,
                "v4_implementation_lock": implementation_lock,
                "execution_contract": execution_contract,
                "execution_attestations": execution_attestations,
                "source_byte_bindings": source_bindings,
                "locked_identities": locked_identities,
                "task_population_binding": population_binding,
                "product_cycle_evidence": product,
                "planning": planning,
                "identity_sha256": digest("calibration-unsealed"),
            }
        )
        self.bundle = bundle
        self.product = product
        self.population = population
        self.ledger = ledger
        self.write("calibration", self.calibration)


def reseal_calibration(calibration: dict, nested: str | None = None) -> None:
    if nested is not None:
        calibration[nested]["identity_sha256"] = PRODUCT.self_sha256(
            calibration[nested]
        )
    calibration["identity_sha256"] = PRODUCT.self_sha256(calibration)


def reseal_report(report: dict) -> None:
    report["report_sha256"] = PRODUCT.self_sha256(report, "report_sha256")


class PowerAnalysisV4RawContractTest(unittest.TestCase):
    def test_raw_preflight_and_cli_validate_complete_artifact_graph(self) -> None:
        with CalibrationCase() as case:
            receipt = V4.preflight_calibration(case.calibration, case.bundle)
            self.assertEqual(receipt["status"], "valid")
            self.assertEqual(receipt["independent_active_task_clusters"], 12)
            self.assertEqual(receipt["active_calibration_tasks"], 12)
            self.assertEqual(receipt["calibration_validated_cells"], 96)
            self.assertTrue(receipt["raw_artifacts_and_schemas_validated"])
            self.assertFalse(receipt["owner_approval_authenticated"])

            arguments = [
                "preflight",
                str(case.paths["calibration"]),
                "--product-cycle",
                str(case.paths["product"]),
                "--task-population",
                str(case.paths["population"]),
                "--review-ledger",
                str(case.paths["ledger"]),
                "--selection-receipt",
                str(case.paths["selection"]),
                "--assignment-receipt",
                str(case.paths["assignment"]),
                "--overlap-receipt",
                str(case.paths["overlap"]),
                "--candidate-lock-receipt",
                str(case.paths["candidate"]),
            ]
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                self.assertEqual(V4.main(arguments), 0)
            self.assertEqual(json.loads(output.getvalue())["status"], "valid")

    def test_synthetic_analysis_is_deterministic_but_never_decision_eligible(self) -> None:
        with CalibrationCase() as case:
            first = V4.analyze_calibration(case.calibration, case.bundle)
            second = V4.analyze_calibration(
                copy.deepcopy(case.calibration), case.bundle
            )
            self.assertEqual(first, second)
            self.assertEqual(first["status"], "evaluated")
            for key in ("benchmark_power_decision", "product_improvement_gate"):
                self.assertFalse(first[key]["eligible_evidence"])
                self.assertFalse(first[key]["passed"])
                self.assertEqual(
                    first[key]["reason"],
                    "synthetic_fixture_not_decision_eligible",
                )
            self.assertFalse(
                first["product_improvement_gate"][
                    "alters_benchmark_primary_verdict"
                ]
            )
            checked = V4.check_power_report(case.calibration, first, case.bundle)
            self.assertEqual(checked["status"], "valid")

    def test_pending_plan_is_complete_while_results_remain_null(self) -> None:
        with CalibrationCase(state="pending") as case:
            self.assertIsInstance(
                case.calibration["planning"]["planning_alternatives"], dict
            )
            report = V4.analyze_calibration(case.calibration, case.bundle)
            self.assertEqual(report["status"], "pending")
            self.assertEqual(
                report["complete_plan"]["identity_sha256"],
                report["planning_sha256"],
            )
            for row in report["power_candidates"]:
                self.assertIsNone(row["primary"])
                self.assertIsNone(row["product_diagnostic"])

    def test_production_plan_is_exact_and_no_trust_anchor_keeps_frozen_nonpassing(self) -> None:
        with CalibrationCase(
            state="frozen", evidence_class="development_measurement"
        ) as case:
            receipt = V4.preflight_calibration(case.calibration, case.bundle)
            plan = case.calibration["planning"]
            self.assertEqual(plan["resamples"], 10_000)
            self.assertEqual(plan["seed"], V4.PRODUCTION_SEED)
            self.assertEqual(
                plan["candidate_task_clusters"], list(V4.PRODUCTION_CANDIDATE_GRID)
            )
            self.assertFalse(receipt["owner_approval_authenticated"])
            fake_rows = [
                {
                    "task_clusters": 12,
                    "primary": {"statistical_gate_met": True},
                }
            ]
            decision = V4._decision(
                rows=fake_rows,
                result_key="primary",
                comparison=V4.PRIMARY_COMPARISON,
                role="benchmark_primary_power_gate",
                state="frozen",
                evidence_class="development_measurement",
                authenticated_owner_approval=False,
            )
            self.assertTrue(decision["statistical_gate_met"])
            self.assertFalse(decision["eligible_evidence"])
            self.assertFalse(decision["passed"])
            self.assertEqual(
                decision["reason"],
                "no_authenticated_owner_approval_trust_anchor",
            )

    def test_development_plan_mutations_fail_schema_and_code(self) -> None:
        mutations = (
            ("resamples", 9_999),
            ("seed", V4.PRODUCTION_SEED + 1),
            ("candidate_task_clusters", [12, 24]),
        )
        with CalibrationCase(evidence_class="development_measurement") as case:
            for field, replacement in mutations:
                with self.subTest(field=field):
                    calibration = copy.deepcopy(case.calibration)
                    calibration["planning"][field] = replacement
                    reseal_calibration(calibration, "planning")
                    with self.assertRaises(V4.PowerV4Error):
                        V4.preflight_calibration(calibration, case.bundle)

    def test_any_post_lock_plan_change_is_rejected(self) -> None:
        changes = (
            lambda plan: plan.__setitem__("seed", plan["seed"] + 1),
            lambda plan: plan.__setitem__("resamples", 101),
            lambda plan: plan.__setitem__("candidate_task_clusters", [12, 24]),
            lambda plan: plan["planning_alternatives"]["primary"][
                "elapsed_time"
            ].__setitem__("planning_alternative_ratio", 0.61),
        )
        with CalibrationCase() as case:
            for mutate in changes:
                calibration = copy.deepcopy(case.calibration)
                mutate(calibration["planning"])
                reseal_calibration(calibration, "planning")
                with self.assertRaises(V4.PowerV4Error):
                    V4.preflight_calibration(calibration, case.bundle)

    def test_raw_whitespace_changes_are_detected_even_when_json_value_is_same(self) -> None:
        for artifact in ("product", "population", "ledger"):
            with self.subTest(artifact=artifact), CalibrationCase() as case:
                path = case.paths[artifact]
                path.write_bytes(path.read_bytes() + b"\n")
                bundle = case.reload_bundle()
                with self.assertRaises(V4.PowerV4Error):
                    V4.preflight_calibration(case.calibration, bundle)

    def test_full_task_population_validator_runs_on_loaded_raw_files(self) -> None:
        with CalibrationCase() as case:
            ledger = copy.deepcopy(case.ledger)
            ledger["reviews"][0]["checks"]["symptom_only"] = False
            case.write("ledger", ledger)
            with self.assertRaisesRegex(
                V4.PowerV4Error, "task-population validation failed"
            ):
                case.reload_bundle()

    def test_receipt_subject_and_raw_hash_are_both_enforced(self) -> None:
        with CalibrationCase() as case:
            receipt = json.loads(case.paths["selection"].read_text())
            receipt["verification_subject_sha256"] = digest("wrong-subject")
            sealed(receipt)
            case.write("selection", receipt)
            bundle = case.reload_bundle()
            with self.assertRaises(V4.PowerV4Error):
                V4.preflight_calibration(case.calibration, bundle)

    def test_population_receipts_bind_selection_assignment_and_overlap_outputs(self) -> None:
        def assert_subject_rejects(
            case: CalibrationCase, population: dict, ledger: dict
        ) -> None:
            sealed_ledger = TASK.seal_review_ledger(ledger)
            sealed_population = TASK.seal_population(population, sealed_ledger)
            case.write("ledger", sealed_ledger)
            case.write("population", sealed_population)
            bundle = case.reload_bundle()
            with self.assertRaisesRegex(V4.PowerV4Error, "receipt subject drift"):
                V4.preflight_calibration(case.calibration, bundle)

        with CalibrationCase() as case:
            population = copy.deepcopy(case.population)
            ledger = copy.deepcopy(case.ledger)
            calibration_member = next(
                member
                for member in population["members"]
                if member["membership"] == "development_calibration"
            )
            old_ref = calibration_member["member_ref"]
            new_ref = digest("replacement-selected-member-ref")
            calibration_member["member_ref"] = new_ref
            ledger_review = next(
                row for row in ledger["reviews"] if row["member_ref"] == old_ref
            )
            ledger_review["member_ref"] = new_ref
            assert_subject_rejects(case, population, ledger)

        with CalibrationCase() as case:
            population = copy.deepcopy(case.population)
            ledger = copy.deepcopy(case.ledger)
            optimization = next(
                member
                for member in population["members"]
                if member["membership"] == "development_optimization"
                and member["contamination_state"] != "excluded"
            )
            calibration_member = next(
                member
                for member in population["members"]
                if member["membership"] == "development_calibration"
            )
            optimization["membership"] = "development_calibration"
            calibration_member["membership"] = "development_optimization"
            assert_subject_rejects(case, population, ledger)

        with CalibrationCase() as case:
            population = copy.deepcopy(case.population)
            ledger = copy.deepcopy(case.ledger)
            calibration_member = next(
                member
                for member in population["members"]
                if member["membership"] == "development_calibration"
            )
            calibration_member["task_overlap_commitment_sha256"] = digest(
                "changed-overlap-output"
            )
            assert_subject_rejects(case, population, ledger)

    def test_actual_artifact_schemas_reject_extra_fields_dates_and_oneof_drift(self) -> None:
        with CalibrationCase() as case:
            calibration = copy.deepcopy(case.calibration)
            calibration["unexpected"] = True
            reseal_calibration(calibration)
            with self.assertRaisesRegex(V4.PowerV4Error, "additional property"):
                V4.preflight_calibration(calibration, case.bundle)

        with CalibrationCase() as case:
            receipt = json.loads(case.paths["selection"].read_text())
            receipt["verified_at"] = "not-a-date"
            sealed(receipt)
            case.write("selection", receipt)
            with self.assertRaisesRegex(V4.PowerV4Error, "RFC3339"):
                case.reload_bundle()

        with CalibrationCase() as case:
            population = copy.deepcopy(case.population)
            holdout = next(
                member
                for member in population["members"]
                if member["membership"] == "confirmatory_holdout"
            )
            holdout["attributes"] = attributes("forbidden-holdout-plaintext")
            case.write("population", population)
            with self.assertRaises(V4.PowerV4Error):
                case.reload_bundle()

    def test_duplicate_product_task_hashes_and_related_tasks_do_not_count_as_independent(self) -> None:
        with self.assertRaisesRegex(V4.PowerV4Error, "task hashes must be unique"):
            CalibrationCase(duplicate_task_hash=True)
        with self.assertRaisesRegex(
            V4.PowerV4Error, "at least 12 derived|one mutually independent"
        ):
            CalibrationCase(related_calibration_pair=True)
        with self.assertRaisesRegex(
            V4.PowerV4Error, "at least 12 derived|one mutually independent"
        ):
            CalibrationCase(shared_fix_pair=True)
        with self.assertRaisesRegex(
            V4.PowerV4Error, "at least 12 derived|one mutually independent"
        ):
            CalibrationCase(shared_session_pair=True)
        with self.assertRaisesRegex(
            V4.PowerV4Error, "at least 12 derived|one mutually independent"
        ):
            CalibrationCase(excluded_bridge=True)

    def test_execution_provider_cli_model_and_timeout_attestations_require_cell_parity(self) -> None:
        mutations = {
            "provider_id": "other-provider",
            "agent_cli_id": "other-cli",
            "agent_cli_version": "other-cli-version",
            "requested_model_id": "other-requested-model",
            "resolved_model_id": "other-model",
            "timeout_policy_sha256": digest("other-timeout-policy"),
            "agent_timeout_limit_seconds": 901,
            "timeout_component_limit_seconds": 60,
        }
        with CalibrationCase() as case:
            for field, replacement in mutations.items():
                with self.subTest(field=field):
                    calibration = copy.deepcopy(case.calibration)
                    calibration["execution_attestations"][0][field] = replacement
                    sealed(calibration["execution_attestations"][0])
                    reseal_calibration(calibration)
                    with self.assertRaisesRegex(V4.PowerV4Error, "drift"):
                        V4.preflight_calibration(calibration, case.bundle)

    def test_analyzer_and_every_schema_are_bound_by_dedicated_v4_lock(self) -> None:
        with CalibrationCase() as case:
            expected = V4._expected_implementation_lock(case.bundle)
            self.assertEqual(
                case.calibration["v4_implementation_lock"], expected
            )
            calibration = copy.deepcopy(case.calibration)
            calibration["v4_implementation_lock"][
                "analyzer_file_sha256"
            ] = digest("different-analyzer")
            reseal_calibration(calibration, "v4_implementation_lock")
            with self.assertRaisesRegex(
                V4.PowerV4Error, "v4 implementation lock drift"
            ):
                V4.preflight_calibration(calibration, case.bundle)

    def test_natural_cost_estimand_supports_zero_numerators_and_zero_denominator_draws(self) -> None:
        vectors = [
            {
                "independence_cluster_sha256": digest("cluster-zero"),
                "task_ids": ["zero"],
                "task_weight": 1,
                "elapsed_log_ratio": math.log(0.8),
                "cost_numerator_mean": 0.2,
                "cost_denominator_mean": 0.0,
                "quality_difference": 0.2,
            },
            {
                "independence_cluster_sha256": digest("cluster-positive"),
                "task_ids": ["positive"],
                "task_weight": 1,
                "elapsed_log_ratio": math.log(0.8),
                "cost_numerator_mean": 0.4,
                "cost_denominator_mean": 1.0,
                "quality_difference": 0.2,
            },
        ]
        draws = [(0,), (1,)]
        statistics, parameters = V4._draw_statistics(
            vectors, draws, alternative=None
        )
        self.assertIsNone(statistics[0]["normalized_cost"])
        self.assertEqual(statistics[1]["normalized_cost"], 0.4)
        self.assertAlmostEqual(
            parameters["cost_numerator_mean"]
            / parameters["cost_denominator_mean"],
            0.6,
        )
        _, zero_draws = V4._ci_offsets(vectors, draws)
        self.assertEqual(zero_draws, 1)
        power = V4._power_for_contrast(
            vectors,
            FIX.alternatives()["primary"],
            draws,
            task_clusters=1,
            resampling_sha256=digest("resampling"),
        )
        self.assertEqual(power["zero_denominator_draws"], 1)
        self.assertEqual(
            power["zero_denominator_resample_policy"],
            "automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1",
        )
        self.assertLessEqual(
            power["joint_all_endpoint_power"],
            power["marginal_power"]["normalized_cost"],
        )

    def test_frozen_equal_task_estimands_remain_unchanged(self) -> None:
        with CalibrationCase(homogeneous=False) as case:
            report = V4.analyze_calibration(case.calibration, case.bundle)
            primary = report["calibration_estimates"]["primary"]
            self.assertAlmostEqual(
                primary["elapsed_time"]["paired_task_geometric_mean_ratio"],
                math.sqrt(0.4 * 0.8),
                places=11,
            )
            self.assertAlmostEqual(
                primary["normalized_cost"]["equal_task_weighted_mean_ratio"],
                82.0 / 110.0,
                places=11,
            )
            self.assertFalse(
                primary["normalized_cost"]["geometric_mean_task_ratios_used"]
            )
            self.assertAlmostEqual(
                primary["code_quality"]["paired_task_mean_difference"],
                0.30,
                places=11,
            )

    def test_structural_zero_treatment_costs_are_supported_without_logs(self) -> None:
        with CalibrationCase(structural_zero=True) as case:
            report = V4.analyze_calibration(case.calibration, case.bundle)
            for key in ("primary", "product_diagnostic"):
                cost = report["calibration_estimates"][key]["normalized_cost"]
                self.assertEqual(cost["equal_task_weighted_mean_ratio"], 0.0)
                self.assertTrue(cost["structural_zero_treatment_cost_supported"])
                self.assertFalse(cost["geometric_mean_task_ratios_used"])

    def test_report_schema_and_recomputation_reject_auditor_mutations(self) -> None:
        with CalibrationCase() as case:
            report = V4.analyze_calibration(case.calibration, case.bundle)
            mutations = []
            extra = copy.deepcopy(report)
            extra["unexpected"] = True
            reseal_report(extra)
            mutations.append(extra)
            passed = copy.deepcopy(report)
            passed["benchmark_power_decision"]["passed"] = True
            reseal_report(passed)
            mutations.append(passed)
            lock = copy.deepcopy(report)
            lock["integrity"]["v4_implementation_lock_sha256"] = digest(
                "other-v4-lock"
            )
            reseal_report(lock)
            mutations.append(lock)
            probability = copy.deepcopy(report)
            probability["power_candidates"][0]["primary"]["marginal_power"][
                "elapsed_time"
            ] = 1.1
            reseal_report(probability)
            mutations.append(probability)
            for mutation in mutations:
                with self.assertRaises(V4.PowerV4Error):
                    V4.validate_power_report(mutation, case.bundle)

            recomputation = copy.deepcopy(report)
            recomputation["power_candidates"][0]["primary"]["marginal_power"][
                "elapsed_time"
            ] = 0.99
            reseal_report(recomputation)
            V4.validate_power_report(recomputation, case.bundle)
            with self.assertRaisesRegex(V4.PowerV4Error, "recomputation"):
                V4.check_power_report(
                    case.calibration, recomputation, case.bundle
                )

    def test_strict_loader_rejects_duplicate_keys_and_non_json_numbers(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            duplicate = pathlib.Path(temporary) / "duplicate.json"
            duplicate.write_text('{"schema":"a","schema":"b"}', encoding="utf-8")
            with self.assertRaisesRegex(V4.PowerV4Error, "duplicate object key"):
                V4.load_calibration(duplicate)
            for constant in ("NaN", "Infinity", "-Infinity"):
                path = pathlib.Path(temporary) / f"{constant}.json"
                path.write_text('{"value":' + constant + "}", encoding="utf-8")
                with self.assertRaisesRegex(V4.PowerV4Error, "non-JSON numeric"):
                    V4.load_calibration(path)

    def test_draft_2020_12_validator_audits_dormant_branches_and_numeric_uniqueness(self) -> None:
        dialect = "https://json-schema.org/draft/2020-12/schema"
        with self.assertRaisesRegex(DRAFT.SchemaError, "unsupported schema"):
            DRAFT.Validator(
                [
                    DRAFT.SchemaDocument(
                        "bad.json",
                        {
                            "$schema": dialect,
                            "type": "object",
                            "$defs": {"dormant": {"madeUpAssertion": True}},
                        },
                    )
                ]
            )
        validator = DRAFT.Validator(
            [
                DRAFT.SchemaDocument(
                    "unique.json",
                    {
                        "$schema": dialect,
                        "type": "array",
                        "uniqueItems": True,
                    },
                )
            ]
        )
        with self.assertRaisesRegex(DRAFT.SchemaError, "uniqueItems"):
            validator.validate([1, 1.0], "unique.json", label="numeric array")

        registry = DRAFT.Validator(
            [
                DRAFT.SchemaDocument(
                    "leaf.json",
                    {
                        "$schema": dialect,
                        "$id": "https://example.test/leaf.json",
                        "type": "string",
                        "pattern": "^ok$",
                    },
                ),
                DRAFT.SchemaDocument(
                    "root.json",
                    {
                        "$schema": dialect,
                        "type": "object",
                        "required": ["value"],
                        "properties": {
                            "value": {"$ref": "leaf.json"}
                        },
                        "additionalProperties": False,
                    },
                ),
            ]
        )
        registry.validate({"value": "ok"}, "root.json", label="cross ref")
        with self.assertRaises(DRAFT.SchemaError):
            registry.validate(
                {"value": "no", "extra": True}, "root.json", label="cross ref"
            )

    def test_shared_resampling_is_exact_and_joint_is_same_draw_frequency(self) -> None:
        first, first_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=41
        )
        second, second_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=41
        )
        changed, changed_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=42
        )
        self.assertEqual(first, second)
        self.assertEqual(first_sha, second_sha)
        self.assertNotEqual(first, changed)
        self.assertNotEqual(first_sha, changed_sha)
        with CalibrationCase() as case:
            report = V4.analyze_calibration(case.calibration, case.bundle)
            for row in report["power_candidates"]:
                self.assertEqual(
                    row["primary"]["shared_resampling_sha256"],
                    row["product_diagnostic"]["shared_resampling_sha256"],
                )
                for key in ("primary", "product_diagnostic"):
                    result = row[key]
                    self.assertLessEqual(
                        result["joint_all_endpoint_power"],
                        min(result["marginal_power"].values()),
                    )
                    self.assertFalse(result["independence_shortcut_used"])


if __name__ == "__main__":
    unittest.main()
