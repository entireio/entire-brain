#!/usr/bin/env python3
"""Final development calibration and four-arm power analysis v4.

V4 is a standalone development lane.  It consumes a completed product-cycle
v1 manifest plus a hash-bound projection of the canonical task-population v2
contract.  It never changes the locked three-arm preregistration or power-v3
artifact, authorizes no spend, and makes no provider or model call.

Power is estimated with deterministic shared resampling of independent task
clusters.  Repetition means are formed inside task/arm before contrasts.  Time
uses paired task log ratios, normalized cost preserves the frozen ratio of
equal-task-weighted arithmetic arm means, and quality uses paired task mean
differences.  Joint power is the same-draw frequency of all three endpoints
clearing their one-sided bounds; marginal powers are never multiplied.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
import copy
from dataclasses import dataclass
from decimal import Decimal
import hashlib
import importlib.util
import json
import math
import os
import pathlib
import stat
import sys
from typing import Any, Mapping, Sequence


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
import draft202012 as DRAFT  # noqa: E402
import task_population as TASK_POPULATION  # noqa: E402

PRODUCT_SPEC = importlib.util.spec_from_file_location(
    "power_v4_product_cycle", HERE / "product_cycle.py"
)
assert PRODUCT_SPEC and PRODUCT_SPEC.loader
PRODUCT = importlib.util.module_from_spec(PRODUCT_SPEC)
PRODUCT_SPEC.loader.exec_module(PRODUCT)

CALIBRATION_SCHEMA = "agent-brain-final-calibration/v1"
REPORT_SCHEMA = "agent-brain-power-analysis/v4"
TASK_POPULATION_PROFILE = "agent_brain_task_population_v2"
MIN_CALIBRATION_CLUSTERS = 12
TARGET_POWER = 0.80
CONFIDENCE = 0.95
MIN_RESAMPLES = 100
MAX_RESAMPLES = 10_000
MAX_CANDIDATE_CLUSTERS = 512
PRODUCTION_RESAMPLES = 10_000
PRODUCTION_SEED = 0x4542563453454544
PRODUCTION_CANDIDATE_GRID = (
    12,
    16,
    20,
    24,
    32,
    40,
    48,
    64,
    80,
    96,
    128,
    160,
    192,
    256,
    320,
    384,
    512,
)
RESAMPLING_METHOD = "paired_cluster_residual_bootstrap_shared_draws_v1"
RESAMPLING_DOMAIN = b"entire-brain/power-analysis-v4/shared-cluster-resample\0"
PRIMARY_COMPARISON = "retrieved_memory_vs_no_memory"
DIAGNOSTIC_COMPARISON = "retrieved_memory_vs_preoptimization_memory"
ENDPOINTS = ("elapsed_time", "normalized_cost", "code_quality")
MAX_JSON_BYTES = 64 * 1024 * 1024
NO_TRUST_ANCHOR = "unauthenticated_no_trust_anchor_fail_closed"
POPULATION_RECEIPT_SUBJECT_SCHEMA = (
    "agent-brain-task-population-verification-subject/full-population-v1"
)
SCHEMA_PATHS = {
    "product_cycle": HERE / "schemas" / "product-cycle-v1.schema.json",
    "task_population": HERE / "schemas" / "task-population-v2.schema.json",
    "review_ledger": HERE.parent / "schemas" / "task-review-ledger-v2.schema.json",
    "final_calibration": HERE / "schemas" / "final-calibration-v1.schema.json",
    "power_report": HERE / "schemas" / "power-analysis-v4.schema.json",
    "population_receipt": HERE / "schemas" / "task-population-verification-receipt-v1.schema.json",
    "candidate_lock_receipt": HERE / "schemas" / "candidate-lock-receipt-v1.schema.json",
    "owner_approval_receipt": HERE / "schemas" / "owner-approval-receipt-v1.schema.json",
}


class PowerV4Error(ValueError):
    """Raised when calibration or power evidence cannot safely be used."""


@dataclass(frozen=True)
class RawJsonArtifact:
    path: pathlib.Path
    raw_bytes: bytes
    value: dict[str, Any]
    sha256: str


@dataclass(frozen=True)
class ArtifactBundle:
    product_cycle: RawJsonArtifact
    task_population: RawJsonArtifact
    review_ledger: RawJsonArtifact
    selection_receipt: RawJsonArtifact
    assignment_receipt: RawJsonArtifact
    overlap_receipt: RawJsonArtifact
    candidate_lock_receipt: RawJsonArtifact
    owner_approval_receipt: RawJsonArtifact | None
    schemas: Mapping[str, RawJsonArtifact]
    schema_validator: DRAFT.Validator


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise PowerV4Error(message)


def _duplicate_key_guard(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        _require(key not in value, "JSON contains a duplicate object key")
        value[key] = item
    return value


def _reject_non_json_constant(value: str) -> None:
    raise ValueError(f"non-JSON numeric constant {value!r}")


def _read_regular_bytes(path: pathlib.Path, label: str) -> bytes:
    flags = os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise PowerV4Error(f"{label} cannot be opened safely: {exc}") from exc
    try:
        before = os.fstat(descriptor)
        _require(stat.S_ISREG(before.st_mode), f"{label} must be a regular file")
        _require(before.st_size <= MAX_JSON_BYTES, f"{label} exceeds the size bound")
        chunks: list[bytes] = []
        remaining = before.st_size
        while remaining:
            chunk = os.read(descriptor, min(remaining, 1024 * 1024))
            _require(bool(chunk), f"{label} changed while being read")
            chunks.append(chunk)
            remaining -= len(chunk)
        _require(os.read(descriptor, 1) == b"", f"{label} grew while being read")
        after = os.fstat(descriptor)
        _require(
            (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
            == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns),
            f"{label} changed while being read",
        )
        return b"".join(chunks)
    finally:
        os.close(descriptor)


def _load_json_artifact(path: pathlib.Path, label: str) -> RawJsonArtifact:
    raw = _read_regular_bytes(path, label)
    try:
        value = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_duplicate_key_guard,
            parse_constant=_reject_non_json_constant,
        )
    except PowerV4Error:
        raise
    except (UnicodeError, ValueError) as exc:
        raise PowerV4Error(f"{label} is not strict UTF-8 JSON: {exc}") from exc
    _require(isinstance(value, dict), f"{label} root must be an object")
    return RawJsonArtifact(
        path=path,
        raw_bytes=raw,
        value=value,
        sha256=hashlib.sha256(raw).hexdigest(),
    )


def load_calibration(path: pathlib.Path) -> dict[str, Any]:
    return _load_json_artifact(path, "calibration").value


def load_power_report(path: pathlib.Path) -> dict[str, Any]:
    return _load_json_artifact(path, "power report").value


def load_artifact_bundle(
    *,
    product_cycle: pathlib.Path,
    task_population: pathlib.Path,
    review_ledger: pathlib.Path,
    selection_receipt: pathlib.Path,
    assignment_receipt: pathlib.Path,
    overlap_receipt: pathlib.Path,
    candidate_lock_receipt: pathlib.Path,
    owner_approval_receipt: pathlib.Path | None,
) -> ArtifactBundle:
    artifacts = {
        "product_cycle": _load_json_artifact(product_cycle, "product-cycle evidence"),
        "task_population": _load_json_artifact(task_population, "task-population contract"),
        "review_ledger": _load_json_artifact(review_ledger, "task-review ledger"),
        "selection_receipt": _load_json_artifact(selection_receipt, "selection receipt"),
        "assignment_receipt": _load_json_artifact(assignment_receipt, "assignment receipt"),
        "overlap_receipt": _load_json_artifact(overlap_receipt, "overlap receipt"),
        "candidate_lock_receipt": _load_json_artifact(
            candidate_lock_receipt, "candidate-lock receipt"
        ),
    }
    approval = (
        _load_json_artifact(owner_approval_receipt, "owner-approval receipt")
        if owner_approval_receipt is not None
        else None
    )
    schemas = {
        name: _load_json_artifact(path, f"{name} schema")
        for name, path in SCHEMA_PATHS.items()
    }
    try:
        validator = DRAFT.Validator(
            [
                DRAFT.SchemaDocument(artifact.path.name, artifact.value)
                for artifact in schemas.values()
            ]
        )
        validator.validate(
            artifacts["product_cycle"].value,
            SCHEMA_PATHS["product_cycle"].name,
            label="product-cycle evidence",
        )
        validator.validate(
            artifacts["task_population"].value,
            SCHEMA_PATHS["task_population"].name,
            label="task-population contract",
        )
        validator.validate(
            artifacts["review_ledger"].value,
            SCHEMA_PATHS["review_ledger"].name,
            label="task-review ledger",
        )
        for kind in ("selection", "assignment", "overlap"):
            validator.validate(
                artifacts[f"{kind}_receipt"].value,
                SCHEMA_PATHS["population_receipt"].name,
                label=f"{kind} receipt",
            )
        validator.validate(
            artifacts["candidate_lock_receipt"].value,
            SCHEMA_PATHS["candidate_lock_receipt"].name,
            label="candidate-lock receipt",
        )
        if approval is not None:
            validator.validate(
                approval.value,
                SCHEMA_PATHS["owner_approval_receipt"].name,
                label="owner-approval receipt",
            )
    except DRAFT.SchemaError as exc:
        raise PowerV4Error(str(exc)) from exc
    try:
        TASK_POPULATION.validate_population(
            artifacts["task_population"].value,
            artifacts["review_ledger"].value,
            population_schema=SCHEMA_PATHS["task_population"],
            review_schema=SCHEMA_PATHS["review_ledger"],
        )
    except TASK_POPULATION.TaskPopulationError as exc:
        raise PowerV4Error(f"task-population validation failed: {exc}") from exc
    return ArtifactBundle(
        product_cycle=artifacts["product_cycle"],
        task_population=artifacts["task_population"],
        review_ledger=artifacts["review_ledger"],
        selection_receipt=artifacts["selection_receipt"],
        assignment_receipt=artifacts["assignment_receipt"],
        overlap_receipt=artifacts["overlap_receipt"],
        candidate_lock_receipt=artifacts["candidate_lock_receipt"],
        owner_approval_receipt=approval,
        schemas=schemas,
        schema_validator=validator,
    )


def _exact_keys(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    actual = set(value)
    missing = sorted(expected - actual)
    extra = sorted(actual - expected)
    _require(not missing and not extra, f"{label} fields mismatch: missing={missing}, extra={extra}")
    return value


def _sha256(value: Any, label: str) -> str:
    try:
        return PRODUCT._sha256(value, label)
    except PRODUCT.ProductCycleError as exc:
        raise PowerV4Error(str(exc)) from exc


def _self_sha256(value: Mapping[str, Any], field: str = "identity_sha256") -> str:
    return PRODUCT.self_sha256(value, field)


def _nonempty(value: Any, label: str) -> str:
    _require(isinstance(value, str) and bool(value), f"{label} must be a non-empty string")
    return value


def _positive_int(value: Any, label: str, *, maximum: int | None = None) -> int:
    _require(type(value) is int and value > 0, f"{label} must be a positive integer")
    if maximum is not None:
        _require(value <= maximum, f"{label} exceeds {maximum}")
    return value


def _finite(value: Any, label: str) -> float:
    _require(type(value) in {int, float}, f"{label} must be a finite number")
    rendered = float(value)
    _require(math.isfinite(rendered), f"{label} must be finite")
    return rendered


def _stable_float(value: float | Decimal) -> float:
    rendered = float(value)
    _require(math.isfinite(rendered), "analysis produced a non-finite result")
    return float(format(rendered, ".12g"))


def _task_identity_sha256(task_id: str) -> str:
    return hashlib.sha256(
        b"entire-brain/task-population-v2/task-id\0" + task_id.encode("utf-8")
    ).hexdigest()


def _population_receipt_subject_sha256(
    population: Mapping[str, Any], kind: str
) -> str:
    """Bind verified population outputs without creating receipt hash cycles."""
    _require(
        kind in {"selection", "assignment", "overlap_commitment"},
        "population receipt kind is invalid",
    )
    projected = copy.deepcopy(dict(population))
    _require(
        "population_sha256" in projected
        and isinstance(projected.get("selection_policy"), dict)
        and isinstance(projected.get("split_policy"), dict),
        "population receipt subject is incomplete",
    )
    # Each receipt attests the entire final population: selected universe,
    # member_ref->membership assignments, overlap commitments, relation edges,
    # review-ledger identity, and derived summary.  Only the self hash and raw
    # receipt hashes are nulled, because including them would be circular.
    projected["population_sha256"] = None
    projected["selection_policy"]["selection_receipt_sha256"] = None
    projected["split_policy"]["assignment_receipt_sha256"] = None
    projected["split_policy"]["overlap_commitment_receipt_sha256"] = None
    return PRODUCT.value_sha256(
        {
            "schema": POPULATION_RECEIPT_SUBJECT_SCHEMA,
            "kind": kind,
            "population": projected,
        }
    )


def _validate_population_receipt(
    artifact: RawJsonArtifact,
    *,
    expected_kind: str,
    expected_subject_sha256: str,
) -> None:
    receipt = artifact.value
    _require(receipt["kind"] == expected_kind, f"{expected_kind} receipt kind drift")
    _require(receipt["status"] == "verified", f"{expected_kind} receipt is not verified")
    _require(
        receipt["authentication_status"] == NO_TRUST_ANCHOR,
        f"{expected_kind} receipt authentication status drift",
    )
    _require(
        receipt["verification_subject_schema"]
        == POPULATION_RECEIPT_SUBJECT_SCHEMA,
        f"{expected_kind} receipt subject schema drift",
    )
    _require(
        receipt["verification_subject_sha256"] == expected_subject_sha256,
        f"{expected_kind} receipt subject drift",
    )
    _require(
        receipt["identity_sha256"] == _self_sha256(receipt),
        f"{expected_kind} receipt identity hash mismatch",
    )


def _independence_clusters(
    population: Mapping[str, Any], calibration_members: Sequence[Mapping[str, Any]]
) -> tuple[dict[str, str], str]:
    # Build the graph over the full validated population.  Projecting first
    # would miss A--excluded-X--B paths and falsely call A and B independent.
    refs = {str(member["member_ref"]): member for member in population["members"]}
    active_refs = {str(member["member_ref"]) for member in calibration_members}
    parent = {ref: ref for ref in refs}

    def find(ref: str) -> str:
        while parent[ref] != ref:
            parent[ref] = parent[parent[ref]]
            ref = parent[ref]
        return ref

    def union(left: str, right: str) -> None:
        left_root = find(left)
        right_root = find(right)
        if left_root != right_root:
            first, second = sorted((left_root, right_root))
            parent[second] = first

    for field in ("family_commitment_sha256",):
        groups: dict[str, list[str]] = defaultdict(list)
        for ref, member in refs.items():
            groups[str(member[field])].append(ref)
        for group in groups.values():
            for ref in group[1:]:
                union(group[0], ref)
    for artifact_field in ("fix_commitment_sha256",):
        groups = defaultdict(list)
        for ref, member in refs.items():
            groups[str(member["artifacts"][artifact_field])].append(ref)
        for group in groups.values():
            for ref in group[1:]:
                union(group[0], ref)
    session_groups: dict[str, list[str]] = defaultdict(list)
    for ref, member in refs.items():
        for session in member["source_session_commitments"]:
            session_groups[str(session)].append(ref)
    for group in session_groups.values():
        for ref in group[1:]:
            union(group[0], ref)
    for edge in population["related_task_edges"]:
        left = str(edge["left_member_ref"])
        right = str(edge["right_member_ref"])
        if edge["material"] is True and left in refs and right in refs:
            union(left, right)

    components: dict[str, list[str]] = defaultdict(list)
    for ref in sorted(refs):
        components[find(ref)].append(ref)
    by_task: dict[str, str] = {}
    cluster_rows: list[dict[str, Any]] = []
    for member_refs in sorted(components.values()):
        active_members = [ref for ref in member_refs if ref in active_refs]
        if not active_members:
            continue
        cluster_sha = PRODUCT.value_sha256(
            {
                "algorithm": "family_fix_session_material_edge_transitive_closure_v1",
                "member_refs": member_refs,
            }
        )
        task_ids = sorted(str(refs[ref]["task_id"]) for ref in active_members)
        for task_id in task_ids:
            by_task[task_id] = cluster_sha
        cluster_rows.append(
            {
                "independence_cluster_sha256": cluster_sha,
                "member_refs": member_refs,
                "task_ids": task_ids,
            }
        )
    return by_task, PRODUCT.value_sha256(cluster_rows)


def _derive_population_binding(
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
) -> tuple[dict[str, Any], list[str], dict[str, str]]:
    population = bundle.task_population.value
    selection = population["selection_policy"]
    split = population["split_policy"]
    receipt_specs = (
        (
            bundle.selection_receipt,
            "selection",
            selection,
            "selection_receipt_sha256",
        ),
        (
            bundle.assignment_receipt,
            "assignment",
            split,
            "assignment_receipt_sha256",
        ),
        (
            bundle.overlap_receipt,
            "overlap_commitment",
            split,
            "overlap_commitment_receipt_sha256",
        ),
    )
    for artifact, kind, subject, field in receipt_specs:
        _require(subject[field] == artifact.sha256, f"{kind} receipt raw-byte hash drift")
        _validate_population_receipt(
            artifact,
            expected_kind=kind,
            expected_subject_sha256=_population_receipt_subject_sha256(
                population, kind
            ),
        )

    calibration_members = [
        member
        for member in population["members"]
        if member["membership"] == "development_calibration"
        and member["contamination_state"] != "excluded"
    ]
    _require(
        all(member["contamination_state"] == "reviewed_clear" for member in calibration_members),
        "calibration population contains a non-reviewed-clear active member",
    )
    cluster_by_task, closure_sha = _independence_clusters(
        population, calibration_members
    )
    cluster_count = len(set(cluster_by_task.values()))
    _require(
        cluster_count >= MIN_CALIBRATION_CLUSTERS,
        "final calibration requires at least 12 derived independent clusters",
    )
    _require(
        cluster_count == len(calibration_members),
        "candidate sizing requires one mutually independent calibration task per derived cluster",
    )
    product_tasks = {str(task["task_id"]): task for task in product_root["tasks"]}
    task_ids = sorted(str(member["task_id"]) for member in calibration_members)
    _require(
        task_ids == sorted(product_tasks),
        "raw product-cycle tasks do not exactly match validated calibration members",
    )
    task_hashes = [str(product_tasks[task_id]["task_sha256"]) for task_id in task_ids]
    _require(
        len(task_hashes) == len(set(task_hashes)),
        "calibration product task hashes must be unique",
    )
    projected_members: list[dict[str, Any]] = []
    for member in sorted(calibration_members, key=lambda item: str(item["task_id"])):
        task_id = str(member["task_id"])
        projected = {
            "member_ref": member["member_ref"],
            "membership": member["membership"],
            "task_id": task_id,
            "task_id_sha256": member["task_id_sha256"],
            "product_task_sha256": product_tasks[task_id]["task_sha256"],
            "task_overlap_commitment_sha256": member[
                "task_overlap_commitment_sha256"
            ],
            "family_commitment_sha256": member["family_commitment_sha256"],
            "fix_commitment_sha256": member["artifacts"]["fix_commitment_sha256"],
            "source_session_commitments": sorted(member["source_session_commitments"]),
            "contamination_state": member["contamination_state"],
            "review_commitment_sha256": member["review_commitment_sha256"],
            "independence_cluster_sha256": cluster_by_task[task_id],
        }
        projected["identity_sha256"] = _self_sha256(projected)
        projected_members.append(projected)
    binding: dict[str, Any] = {
        "schema_version": 2,
        "profile": TASK_POPULATION_PROFILE,
        "status": population["status"],
        "schema_file_sha256": bundle.schemas["task_population"].sha256,
        "population_sha256": population["population_sha256"],
        "source_file_sha256": bundle.task_population.sha256,
        "review_ledger_sha256": bundle.review_ledger.value["ledger_sha256"],
        "review_ledger_file_sha256": bundle.review_ledger.sha256,
        "selection_verification_status": selection["selection_verification_status"],
        "selection_receipt_file_sha256": bundle.selection_receipt.sha256,
        "assignment_verification_status": split["assignment_verification_status"],
        "assignment_receipt_file_sha256": bundle.assignment_receipt.sha256,
        "overlap_commitment_verification_status": split[
            "overlap_commitment_verification_status"
        ],
        "overlap_commitment_receipt_file_sha256": bundle.overlap_receipt.sha256,
        "calibration_min_independent_tasks": split[
            "calibration_min_independent_tasks"
        ],
        "holdout_plaintext": split["holdout_plaintext"],
        "independence_closure_algorithm": "family_fix_session_material_edge_transitive_closure_v1",
        "independence_cluster_count": cluster_count,
        "independence_projection_sha256": closure_sha,
        "calibration_members": projected_members,
    }
    binding["identity_sha256"] = _self_sha256(binding)
    return binding, task_ids, cluster_by_task


def _validate_population_binding(
    value: Any,
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
) -> tuple[list[str], dict[str, dict[str, Any]], dict[str, str]]:
    expected, task_ids, cluster_by_task = _derive_population_binding(
        bundle=bundle, product_root=product_root
    )
    _require(value == expected, "task-population projection differs from validated raw artifacts")
    by_task = {member["task_id"]: member for member in expected["calibration_members"]}
    return task_ids, by_task, cluster_by_task


def _precalibration_plan_sha256(product_root: Mapping[str, Any]) -> str:
    """Hash only inputs that exist before any calibration outcome is opened."""
    design = product_root["design"]
    return PRODUCT.value_sha256(
        {
            "schema": "agent-brain-product-cycle-precalibration-plan/v1",
            "purpose": product_root["purpose"],
            "evidence_class": product_root["evidence_class"],
            "claim_scope": product_root["claim_scope"],
            "product_identity_binding_sha256": product_root["product_identities"][
                "binding_sha256"
            ],
            "task_inventory_sha256": product_root["task_inventory_sha256"],
            "shared_execution_identity_sha256": product_root["shared_execution"][
                "identity_sha256"
            ],
            "arms": design["arms"],
            "arm_product_roles": design["arm_product_roles"],
            "primary_contrast": design["primary_contrast"],
            "diagnostic_contrast": design["diagnostic_contrast"],
            "task_clusters": design["task_clusters"],
            "repetitions_per_arm": design["repetitions_per_arm"],
            "requested_cells": design["requested_cells"],
            "agent_retry_limit": design["agent_retry_limit"],
            "replacement_cell_limit": design["replacement_cell_limit"],
            "reserve_cell_limit": design["reserve_cell_limit"],
            "maximum_agent_invocations": design["maximum_agent_invocations"],
            "budget": design["budget"],
            "schedule_sha256": product_root["shared_execution"]["schedule_sha256"],
        }
    )


def _file_sha256(path: pathlib.Path, label: str) -> str:
    return hashlib.sha256(_read_regular_bytes(path, label)).hexdigest()


def _expected_implementation_lock(bundle: ArtifactBundle) -> dict[str, Any]:
    """Build the exact lock for the current analyzer, validators, and schemas."""
    lock: dict[str, Any] = {
        "schema": "agent-brain-power-v4-implementation-lock/v1",
        "analyzer_path": "benchmarks/agent-brain/confirmatory/power_analysis_v4.py",
        "analyzer_file_sha256": _file_sha256(
            HERE / "power_analysis_v4.py", "power-v4 analyzer"
        ),
        "draft202012_validator_file_sha256": _file_sha256(
            HERE / "draft202012.py", "Draft 2020-12 validator"
        ),
        "product_cycle_validator_file_sha256": _file_sha256(
            HERE / "product_cycle.py", "product-cycle validator"
        ),
        "task_population_validator_file_sha256": _file_sha256(
            HERE / "task_population.py", "task-population validator"
        ),
        "product_cycle_schema_file_sha256": bundle.schemas["product_cycle"].sha256,
        "task_population_schema_file_sha256": bundle.schemas["task_population"].sha256,
        "review_ledger_schema_file_sha256": bundle.schemas["review_ledger"].sha256,
        "final_calibration_schema_file_sha256": bundle.schemas[
            "final_calibration"
        ].sha256,
        "power_report_schema_file_sha256": bundle.schemas["power_report"].sha256,
        "population_receipt_schema_file_sha256": bundle.schemas[
            "population_receipt"
        ].sha256,
        "candidate_lock_receipt_schema_file_sha256": bundle.schemas[
            "candidate_lock_receipt"
        ].sha256,
        "owner_approval_receipt_schema_file_sha256": bundle.schemas[
            "owner_approval_receipt"
        ].sha256,
    }
    lock["identity_sha256"] = _self_sha256(lock)
    return lock


def _validate_implementation_lock(
    value: Any, *, bundle: ArtifactBundle
) -> dict[str, Any]:
    lock = _exact_keys(
        value,
        {
            "schema",
            "analyzer_path",
            "analyzer_file_sha256",
            "draft202012_validator_file_sha256",
            "product_cycle_validator_file_sha256",
            "task_population_validator_file_sha256",
            "product_cycle_schema_file_sha256",
            "task_population_schema_file_sha256",
            "review_ledger_schema_file_sha256",
            "final_calibration_schema_file_sha256",
            "power_report_schema_file_sha256",
            "population_receipt_schema_file_sha256",
            "candidate_lock_receipt_schema_file_sha256",
            "owner_approval_receipt_schema_file_sha256",
            "identity_sha256",
        },
        "v4_implementation_lock",
    )
    expected = _expected_implementation_lock(bundle)
    for field, expected_value in expected.items():
        _require(lock[field] == expected_value, f"v4 implementation lock drift: {field}")
    _require(
        lock["identity_sha256"] == _self_sha256(lock),
        "v4 implementation lock identity hash mismatch",
    )
    return lock


def _validate_execution_contract(
    value: Any, *, product_root: Mapping[str, Any]
) -> dict[str, Any]:
    contract = _exact_keys(
        value,
        {
            "schema",
            "status",
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
            "identity_sha256",
        },
        "execution_contract",
    )
    _require(contract["schema"] == "agent-brain-power-v4-execution-contract/v1", "execution contract schema changed")
    _require(contract["status"] == "locked_before_calibration_opening", "execution contract was not locked before calibration")
    for field in (
        "provider_id",
        "agent_cli_id",
        "agent_cli_version",
        "requested_model_id",
        "resolved_model_id",
        "effort",
    ):
        _nonempty(contract[field], f"execution_contract.{field}")
    shared = product_root["shared_execution"]
    _require(contract["resolved_model_id"] == shared["model_id"], "resolved model identity drift")
    _require(contract["effort"] == shared["effort"], "execution effort drift")
    _require(contract["runner_sha256"] == shared["runner_sha256"], "execution runner drift")
    _sha256(contract["timeout_policy_sha256"], "execution_contract.timeout_policy_sha256")
    _positive_int(
        contract["agent_timeout_limit_seconds"],
        "execution_contract.agent_timeout_limit_seconds",
    )
    component_limit = contract["timeout_component_limit_seconds"]
    _require(
        component_limit is None or (type(component_limit) is int and component_limit > 0),
        "execution_contract.timeout_component_limit_seconds is invalid",
    )
    _require(
        contract["identity_sha256"] == _self_sha256(contract),
        "execution contract identity hash mismatch",
    )
    return contract


def _validate_execution_attestations(
    value: Any,
    *,
    product_root: Mapping[str, Any],
    execution_contract: Mapping[str, Any],
) -> str:
    _require(isinstance(value, list), "execution_attestations must be a list")
    cells = {str(cell["run_id"]): cell for cell in product_root["cells"]}
    _require(len(value) == len(cells), "execution attestation count differs from product cells")
    expected_shared = {
        "provider_id": execution_contract["provider_id"],
        "agent_cli_id": execution_contract["agent_cli_id"],
        "agent_cli_version": execution_contract["agent_cli_version"],
        "requested_model_id": execution_contract["requested_model_id"],
        "resolved_model_id": execution_contract["resolved_model_id"],
        "effort": execution_contract["effort"],
        "runner_sha256": execution_contract["runner_sha256"],
        "timeout_policy_sha256": execution_contract["timeout_policy_sha256"],
        "agent_timeout_limit_seconds": execution_contract[
            "agent_timeout_limit_seconds"
        ],
        "timeout_component_limit_seconds": execution_contract[
            "timeout_component_limit_seconds"
        ],
    }
    seen: set[str] = set()
    for index, raw in enumerate(value):
        label = f"execution_attestations[{index}]"
        attestation = _exact_keys(
            raw,
            {
                "run_id",
                "cell_identity_sha256",
                *expected_shared.keys(),
                "identity_sha256",
            },
            label,
        )
        run_id = _nonempty(attestation["run_id"], f"{label}.run_id")
        _require(run_id in cells and run_id not in seen, f"{label} run identity drift")
        seen.add(run_id)
        cell = cells[run_id]
        _require(
            attestation["cell_identity_sha256"] == cell["identity_sha256"],
            f"{label} cell identity drift",
        )
        for field, expected_value in expected_shared.items():
            _require(attestation[field] == expected_value, f"{label}.{field} parity drift")
        execution = cell["execution_identity"]
        timing = cell["outcome"]["timing"]
        _require(attestation["runner_sha256"] == execution["runner_sha256"], f"{label} runner/cell drift")
        _require(attestation["resolved_model_id"] == execution["model_id"], f"{label} resolved-model/cell drift")
        _require(attestation["effort"] == execution["effort"], f"{label} effort/cell drift")
        _require(
            attestation["agent_timeout_limit_seconds"]
            == timing["agent_timeout_limit_seconds"],
            f"{label} agent-timeout/cell drift",
        )
        _require(
            attestation["timeout_component_limit_seconds"]
            == timing["timeout_component_limit_seconds"],
            f"{label} component-timeout/cell drift",
        )
        _require(
            attestation["identity_sha256"] == _self_sha256(attestation),
            f"{label} identity hash mismatch",
        )
    _require([row["run_id"] for row in value] == sorted(seen), "execution attestations must be ordered by run_id")
    return PRODUCT.value_sha256(value)


def _validate_candidate_lock(
    value: Any,
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
    complete_plan_sha256: str,
) -> dict[str, Any]:
    lock = _exact_keys(
        value,
        {
            "status",
            "candidate_product_identity_sha256",
            "complete_plan_sha256",
            "task_population_contract_sha256",
            "task_population_file_sha256",
            "lock_receipt_file_sha256",
            "authentication_status",
            "identity_sha256",
        },
        "candidate_lock",
    )
    _require(
        lock["status"] == "structurally_locked_before_development_calibration",
        "candidate lock must precede development calibration",
    )
    candidate_sha = _sha256(
        lock["candidate_product_identity_sha256"],
        "candidate_lock.candidate_product_identity_sha256",
    )
    _require(
        candidate_sha
        == product_root["product_identities"]["candidate"]["identity_sha256"],
        "candidate lock product identity drift",
    )
    _require(
        lock["complete_plan_sha256"] == complete_plan_sha256,
        "candidate lock complete-plan drift",
    )
    population_sha = _sha256(
        lock["task_population_contract_sha256"],
        "candidate_lock.task_population_contract_sha256",
    )
    _require(
        population_sha == population_binding["population_sha256"],
        "candidate lock task-population contract drift",
    )
    _require(
        lock["task_population_file_sha256"] == bundle.task_population.sha256,
        "candidate lock task-population raw bytes drift",
    )
    _require(
        lock["lock_receipt_file_sha256"] == bundle.candidate_lock_receipt.sha256,
        "candidate-lock receipt raw bytes drift",
    )
    _require(
        lock["authentication_status"] == NO_TRUST_ANCHOR,
        "candidate-lock authentication status drift",
    )
    receipt = bundle.candidate_lock_receipt.value
    expected_receipt = {
        "complete_plan_sha256": complete_plan_sha256,
        "candidate_product_identity_sha256": candidate_sha,
        "task_population_file_sha256": bundle.task_population.sha256,
        "authentication_status": NO_TRUST_ANCHOR,
    }
    for field, expected_value in expected_receipt.items():
        _require(receipt[field] == expected_value, f"candidate-lock receipt drift: {field}")
    _require(
        receipt["identity_sha256"] == _self_sha256(receipt),
        "candidate-lock receipt identity hash mismatch",
    )
    digest = _sha256(lock["identity_sha256"], "candidate_lock.identity_sha256")
    _require(digest == _self_sha256(lock), "candidate lock identity hash mismatch")
    return lock


def _validate_source_byte_bindings(
    value: Any,
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
    implementation_lock_sha256: str,
    execution_attestations_sha256: str,
) -> dict[str, Any]:
    bindings = _exact_keys(
        value,
        {
            "product_cycle_file_sha256",
            "product_cycle_schema_file_sha256",
            "product_cycle_contract_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_schema_file_sha256",
            "task_population_contract_file_sha256",
            "task_population_contract_sha256",
            "task_population_binding_canonical_bytes_sha256",
            "review_ledger_schema_file_sha256",
            "review_ledger_file_sha256",
            "review_ledger_sha256",
            "selection_receipt_file_sha256",
            "assignment_receipt_file_sha256",
            "overlap_receipt_file_sha256",
            "candidate_lock_receipt_file_sha256",
            "owner_approval_receipt_file_sha256",
            "v4_implementation_lock_sha256",
            "execution_attestations_sha256",
            "identity_sha256",
        },
        "source_byte_bindings",
    )
    expected = {
        "product_cycle_file_sha256": bundle.product_cycle.sha256,
        "product_cycle_schema_file_sha256": bundle.schemas["product_cycle"].sha256,
        "product_cycle_contract_sha256": product_root["manifest_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product_root
        ),
        "task_population_schema_file_sha256": bundle.schemas["task_population"].sha256,
        "task_population_contract_file_sha256": bundle.task_population.sha256,
        "task_population_contract_sha256": population_binding["population_sha256"],
        "task_population_binding_canonical_bytes_sha256": PRODUCT.value_sha256(
            population_binding
        ),
        "review_ledger_schema_file_sha256": bundle.schemas["review_ledger"].sha256,
        "review_ledger_file_sha256": bundle.review_ledger.sha256,
        "review_ledger_sha256": bundle.review_ledger.value["ledger_sha256"],
        "selection_receipt_file_sha256": bundle.selection_receipt.sha256,
        "assignment_receipt_file_sha256": bundle.assignment_receipt.sha256,
        "overlap_receipt_file_sha256": bundle.overlap_receipt.sha256,
        "candidate_lock_receipt_file_sha256": bundle.candidate_lock_receipt.sha256,
        "owner_approval_receipt_file_sha256": (
            bundle.owner_approval_receipt.sha256
            if bundle.owner_approval_receipt is not None
            else None
        ),
        "v4_implementation_lock_sha256": implementation_lock_sha256,
        "execution_attestations_sha256": execution_attestations_sha256,
    }
    for field, expected_value in expected.items():
        if expected_value is None:
            _require(bindings[field] is None, f"source byte binding drift: {field}")
            continue
        actual = _sha256(bindings[field], f"source_byte_bindings.{field}")
        _require(actual == expected_value, f"source byte binding drift: {field}")
    digest = _sha256(
        bindings["identity_sha256"], "source_byte_bindings.identity_sha256"
    )
    _require(
        digest == _self_sha256(bindings),
        "source byte bindings identity hash mismatch",
    )
    return bindings


def _validate_locked_identities(
    value: Any,
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
    execution_contract: Mapping[str, Any],
    implementation_lock_sha256: str,
) -> dict[str, Any]:
    locked = _exact_keys(
        value,
        {
            "product_cycle_schema_sha256",
            "product_cycle_file_sha256",
            "product_cycle_contract_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_schema_sha256",
            "task_population_sha256",
            "task_population_file_sha256",
            "review_ledger_file_sha256",
            "candidate_product_identity_sha256",
            "candidate_packet_format_sha256",
            "corpus_sha256",
            "engine_sha256",
            "prompt_template_sha256",
            "prompt_parity_algorithm",
            "cache_policy_sha256",
            "runner_sha256",
            "model_id",
            "provider_id",
            "agent_cli_id",
            "agent_cli_version",
            "requested_model_id",
            "resolved_model_id",
            "effort",
            "timeout_policy_sha256",
            "agent_timeout_limit_seconds",
            "timeout_component_limit_seconds",
            "schedule_sha256",
            "price_quote_sha256",
            "pricing_policy_sha256",
            "execution_contract_sha256",
            "v4_implementation_lock_sha256",
            "identity_sha256",
        },
        "locked_identities",
    )
    candidate = product_root["product_identities"]["candidate"]
    shared = product_root["shared_execution"]
    expected = {
        "product_cycle_schema_sha256": bundle.schemas["product_cycle"].sha256,
        "product_cycle_file_sha256": bundle.product_cycle.sha256,
        "product_cycle_contract_sha256": product_root["manifest_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product_root
        ),
        "task_population_schema_sha256": bundle.schemas["task_population"].sha256,
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
        "timeout_policy_sha256": execution_contract["timeout_policy_sha256"],
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
        "v4_implementation_lock_sha256": implementation_lock_sha256,
    }
    for field, expected_value in expected.items():
        if expected_value is None:
            _require(locked[field] is None, f"locked identity drift: {field}")
            continue
        if field.endswith("_sha256"):
            _sha256(locked[field], f"locked_identities.{field}")
        elif field.endswith("_seconds"):
            _positive_int(locked[field], f"locked_identities.{field}")
        else:
            _nonempty(locked[field], f"locked_identities.{field}")
        _require(locked[field] == expected_value, f"locked identity drift: {field}")
    digest = _sha256(locked["identity_sha256"], "locked_identities.identity_sha256")
    _require(digest == _self_sha256(locked), "locked identities hash mismatch")
    return locked


def _validate_ratio_alternative(value: Any, label: str) -> dict[str, float]:
    endpoint = _exact_keys(
        value, {"required_ratio_max", "planning_alternative_ratio"}, label
    )
    required = _finite(endpoint["required_ratio_max"], f"{label}.required_ratio_max")
    alternative = _finite(
        endpoint["planning_alternative_ratio"], f"{label}.planning_alternative_ratio"
    )
    _require(0 < alternative < required < 1, f"{label} alternative must be strictly better than a ratio floor in (0,1)")
    return {"required_ratio_max": required, "planning_alternative_ratio": alternative}


def _validate_quality_alternative(value: Any, label: str) -> dict[str, float]:
    endpoint = _exact_keys(
        value, {"required_difference_min", "planning_alternative_difference"}, label
    )
    required = _finite(
        endpoint["required_difference_min"], f"{label}.required_difference_min"
    )
    alternative = _finite(
        endpoint["planning_alternative_difference"],
        f"{label}.planning_alternative_difference",
    )
    _require(0 < required < alternative <= 1, f"{label} alternative must be strictly better than a difference floor in (0,1]")
    return {
        "required_difference_min": required,
        "planning_alternative_difference": alternative,
    }


def _validate_contrast_alternatives(
    value: Any,
    *,
    label: str,
    comparison: str,
    role: str,
) -> dict[str, Any]:
    contrast = _exact_keys(
        value,
        {"comparison", "role", "elapsed_time", "normalized_cost", "code_quality"},
        label,
    )
    _require(contrast["comparison"] == comparison, f"{label}.comparison changed")
    _require(contrast["role"] == role, f"{label}.role changed")
    return {
        "comparison": comparison,
        "role": role,
        "elapsed_time": _validate_ratio_alternative(
            contrast["elapsed_time"], f"{label}.elapsed_time"
        ),
        "normalized_cost": _validate_ratio_alternative(
            contrast["normalized_cost"], f"{label}.normalized_cost"
        ),
        "code_quality": _validate_quality_alternative(
            contrast["code_quality"], f"{label}.code_quality"
        ),
    }


def _validate_planning(
    value: Any,
    *,
    bundle: ArtifactBundle,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
    calibration_repetitions: int,
    evidence_class: str,
    execution_contract_sha256: str,
    implementation_lock_sha256: str,
) -> tuple[dict[str, Any], dict[str, Any]]:
    planning = _exact_keys(
        value,
        {
            "schema",
            "profile",
            "method",
            "target_power",
            "confidence",
            "resamples",
            "seed",
            "candidate_task_clusters",
            "arms",
            "repetitions_per_arm",
            "agent_retry_limit",
            "replacement_cell_limit",
            "reserve_cell_limit",
            "planning_alternatives",
            "precalibration_product_plan_sha256",
            "task_population_contract_sha256",
            "task_population_file_sha256",
            "review_ledger_file_sha256",
            "selection_receipt_file_sha256",
            "assignment_receipt_file_sha256",
            "overlap_receipt_file_sha256",
            "execution_contract_sha256",
            "v4_implementation_lock_sha256",
            "identity_sha256",
        },
        "planning",
    )
    _require(planning["schema"] == "agent-brain-final-calibration-plan/v1", "final-calibration plan schema changed")
    expected_profile = (
        "production_development_measurement_v1"
        if evidence_class == "development_measurement"
        else "synthetic_fixture_test_v1"
    )
    _require(planning["profile"] == expected_profile, "planning profile/evidence mismatch")
    _require(planning["method"] == RESAMPLING_METHOD, "power-v4 resampling method changed")
    _require(_finite(planning["target_power"], "planning.target_power") == TARGET_POWER, "target power must remain 0.80")
    _require(_finite(planning["confidence"], "planning.confidence") == CONFIDENCE, "confidence must remain 0.95")
    resamples = _positive_int(
        planning["resamples"], "planning.resamples", maximum=MAX_RESAMPLES
    )
    _require(resamples >= MIN_RESAMPLES, f"planning.resamples must be at least {MIN_RESAMPLES}")
    seed = planning["seed"]
    _require(type(seed) is int and 0 <= seed < 2**63, "planning.seed must be a nonnegative signed 63-bit integer")
    candidates = planning["candidate_task_clusters"]
    _require(isinstance(candidates, list) and bool(candidates), "candidate task counts must be a non-empty list")
    _require(
        all(type(item) is int and MIN_CALIBRATION_CLUSTERS <= item <= MAX_CANDIDATE_CLUSTERS for item in candidates),
        "candidate task counts must be integers in [12,512]",
    )
    _require(candidates == sorted(set(candidates)), "candidate task counts must be unique and increasing")
    if evidence_class == "development_measurement":
        _require(resamples == PRODUCTION_RESAMPLES, "development measurement requires exactly 10,000 resamples")
        _require(seed == PRODUCTION_SEED, "development measurement seed drift")
        _require(tuple(candidates) == PRODUCTION_CANDIDATE_GRID, "development measurement candidate grid drift")
    _require(planning["arms"] == list(PRODUCT.ARMS), "power-v4 four-arm design changed")
    repetitions = _positive_int(planning["repetitions_per_arm"], "planning.repetitions_per_arm")
    _require(
        repetitions == calibration_repetitions,
        "planned repetitions must equal the calibration repetition contract",
    )
    for field in ("agent_retry_limit", "replacement_cell_limit", "reserve_cell_limit"):
        _require(
            type(planning[field]) is int and planning[field] == 0,
            f"planning.{field} must remain integer zero",
        )
    alternatives = planning["planning_alternatives"]
    _require(isinstance(alternatives, dict), "complete pre-calibration plan requires planning alternatives")
    alternatives = _exact_keys(alternatives, {"primary", "product_diagnostic"}, "planning.planning_alternatives")
    validated = {
        "primary": _validate_contrast_alternatives(
            alternatives["primary"],
            label="planning.planning_alternatives.primary",
            comparison=PRIMARY_COMPARISON,
            role="primary_development_contrast",
        ),
        "product_diagnostic": _validate_contrast_alternatives(
            alternatives["product_diagnostic"],
            label="planning.planning_alternatives.product_diagnostic",
            comparison=DIAGNOSTIC_COMPARISON,
            role="diagnostic_product_progress_only",
        ),
    }
    expected_hashes = {
        "precalibration_product_plan_sha256": _precalibration_plan_sha256(product_root),
        "task_population_contract_sha256": population_binding["population_sha256"],
        "task_population_file_sha256": bundle.task_population.sha256,
        "review_ledger_file_sha256": bundle.review_ledger.sha256,
        "selection_receipt_file_sha256": bundle.selection_receipt.sha256,
        "assignment_receipt_file_sha256": bundle.assignment_receipt.sha256,
        "overlap_receipt_file_sha256": bundle.overlap_receipt.sha256,
        "execution_contract_sha256": execution_contract_sha256,
        "v4_implementation_lock_sha256": implementation_lock_sha256,
    }
    for field, expected_value in expected_hashes.items():
        _require(planning[field] == expected_value, f"complete plan binding drift: {field}")
    _require(
        planning["identity_sha256"] == _self_sha256(planning),
        "complete plan identity hash mismatch",
    )
    return planning, validated


def _validate_owner_approval(
    value: Any,
    *,
    state: str,
    evidence_class: str,
    bundle: ArtifactBundle,
    complete_plan_sha256: str,
    candidate_lock_receipt_file_sha256: str,
) -> bool:
    if state != "frozen":
        _require(value is None, "only frozen calibration may name an owner-approval receipt")
        _require(
            bundle.owner_approval_receipt is None,
            "non-frozen calibration may not load an owner-approval receipt",
        )
        return False
    _require(evidence_class == "development_measurement", "synthetic calibration cannot be frozen")
    approval = bundle.owner_approval_receipt
    _require(approval is not None, "frozen calibration requires an owner-approval receipt file")
    assert approval is not None
    _require(value == approval.sha256, "owner-approval receipt raw-byte hash drift")
    receipt = approval.value
    expected = {
        "complete_plan_sha256": complete_plan_sha256,
        "candidate_lock_receipt_file_sha256": candidate_lock_receipt_file_sha256,
        "authentication_status": NO_TRUST_ANCHOR,
    }
    for field, expected_value in expected.items():
        _require(receipt[field] == expected_value, f"owner-approval receipt drift: {field}")
    _require(
        receipt["identity_sha256"] == _self_sha256(receipt),
        "owner-approval receipt identity hash mismatch",
    )
    # No owner trust root is configured in this repository.  Structural
    # approval is retained for review, but cannot make a decision eligible.
    return False


def _validated_calibration(
    calibration: Any, bundle: ArtifactBundle,
) -> tuple[dict[str, Any], list[dict[str, Any]], dict[str, Any]]:
    try:
        bundle.schema_validator.validate(
            calibration,
            SCHEMA_PATHS["final_calibration"].name,
            label="final calibration",
        )
    except DRAFT.SchemaError as exc:
        raise PowerV4Error(str(exc)) from exc
    root = _exact_keys(
        calibration,
        {
            "schema",
            "evidence_class",
            "state",
            "owner_approval_receipt_sha256",
            "candidate_lock",
            "v4_implementation_lock",
            "execution_contract",
            "execution_attestations",
            "source_byte_bindings",
            "locked_identities",
            "task_population_binding",
            "product_cycle_evidence",
            "planning",
            "identity_sha256",
        },
        "calibration",
    )
    _require(root["schema"] == CALIBRATION_SCHEMA, "final-calibration schema changed")
    evidence_class = root["evidence_class"]
    _require(
        evidence_class in {"synthetic_fixture", "development_measurement"},
        "calibration evidence_class is invalid",
    )
    state = root["state"]
    _require(state in {"pending", "candidate", "frozen"}, "calibration state is invalid")
    _require(
        root["product_cycle_evidence"] == bundle.product_cycle.value,
        "embedded product-cycle evidence differs from loaded raw file",
    )
    try:
        product_root, measurements, product_meta = PRODUCT._validated_manifest(
            bundle.product_cycle.value
        )
    except PRODUCT.ProductCycleError as exc:
        raise PowerV4Error(f"product-cycle evidence is invalid: {exc}") from exc
    _require(
        product_root["evidence_class"] == evidence_class,
        "calibration and product-cycle evidence classes differ",
    )
    population = root["task_population_binding"]
    task_ids, members, cluster_by_task = _validate_population_binding(
        population, bundle=bundle, product_root=product_root
    )
    _require(
        product_meta["task_ids"] == task_ids,
        "product-cycle tasks do not exactly match the calibration population projection",
    )
    product_tasks = {item["task_id"]: item for item in product_root["tasks"]}
    for task_id in task_ids:
        _require(
            members[task_id]["product_task_sha256"]
            == product_tasks[task_id]["task_sha256"],
            f"product task identity drift for {task_id}",
        )
    implementation_lock = _validate_implementation_lock(
        root["v4_implementation_lock"], bundle=bundle
    )
    execution_contract = _validate_execution_contract(
        root["execution_contract"], product_root=product_root
    )
    execution_attestations_sha256 = _validate_execution_attestations(
        root["execution_attestations"],
        product_root=product_root,
        execution_contract=execution_contract,
    )
    planning, alternatives = _validate_planning(
        root["planning"],
        bundle=bundle,
        product_root=product_root,
        population_binding=population,
        calibration_repetitions=product_meta["repetitions"],
        evidence_class=evidence_class,
        execution_contract_sha256=execution_contract["identity_sha256"],
        implementation_lock_sha256=implementation_lock["identity_sha256"],
    )
    candidate_lock = _validate_candidate_lock(
        root["candidate_lock"],
        bundle=bundle,
        product_root=product_root,
        population_binding=population,
        complete_plan_sha256=planning["identity_sha256"],
    )
    authenticated_owner_approval = _validate_owner_approval(
        root["owner_approval_receipt_sha256"],
        state=state,
        evidence_class=evidence_class,
        bundle=bundle,
        complete_plan_sha256=planning["identity_sha256"],
        candidate_lock_receipt_file_sha256=bundle.candidate_lock_receipt.sha256,
    )
    source_bindings = _validate_source_byte_bindings(
        root["source_byte_bindings"],
        bundle=bundle,
        product_root=product_root,
        population_binding=population,
        implementation_lock_sha256=implementation_lock["identity_sha256"],
        execution_attestations_sha256=execution_attestations_sha256,
    )
    locked = _validate_locked_identities(
        root["locked_identities"],
        bundle=bundle,
        product_root=product_root,
        population_binding=population,
        execution_contract=execution_contract,
        implementation_lock_sha256=implementation_lock["identity_sha256"],
    )
    if state == "frozen":
        _require(population["status"] == "frozen_unopened", "frozen calibration requires frozen_unopened population")
    digest = _sha256(root["identity_sha256"], "calibration.identity_sha256")
    _require(digest == _self_sha256(root), "final-calibration identity hash mismatch")
    metadata = {
        "state": state,
        "evidence_class": evidence_class,
        "task_ids": task_ids,
        "cluster_by_task": cluster_by_task,
        "task_clusters": len(set(cluster_by_task.values())),
        "calibration_tasks": len(task_ids),
        "calibration_repetitions": product_meta["repetitions"],
        "calibration_cells": product_meta["expected_cells"],
        "planning": planning,
        "alternatives": alternatives,
        "authenticated_owner_approval": authenticated_owner_approval,
        "complete_plan_sha256": planning["identity_sha256"],
        "product_cycle_contract_sha256": product_root["manifest_sha256"],
        "task_population_sha256": population["population_sha256"],
        "candidate_lock_sha256": candidate_lock["identity_sha256"],
        "source_byte_bindings_sha256": source_bindings["identity_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": source_bindings[
            "product_cycle_evidence_canonical_bytes_sha256"
        ],
        "task_population_contract_file_sha256": source_bindings[
            "task_population_contract_file_sha256"
        ],
        "locked_identities_sha256": locked["identity_sha256"],
        "v4_implementation_lock_sha256": implementation_lock["identity_sha256"],
        "execution_contract_sha256": execution_contract["identity_sha256"],
        "execution_attestations_sha256": execution_attestations_sha256,
        "product_cycle_schema_file_sha256": source_bindings[
            "product_cycle_schema_file_sha256"
        ],
        "task_population_schema_file_sha256": source_bindings[
            "task_population_schema_file_sha256"
        ],
    }
    return root, measurements, metadata


def preflight_calibration(calibration: Any, bundle: ArtifactBundle) -> dict[str, Any]:
    """Fail closed on membership, identity, design, and pending-state drift."""
    root, measurements, metadata = _validated_calibration(calibration, bundle)
    return {
        "schema": "agent-brain-final-calibration-preflight/v1",
        "status": "valid",
        "state": metadata["state"],
        "evidence_class": metadata["evidence_class"],
        "calibration_sha256": root["identity_sha256"],
        "independent_active_task_clusters": metadata["task_clusters"],
        "active_calibration_tasks": metadata["calibration_tasks"],
        "calibration_repetitions_per_arm": metadata["calibration_repetitions"],
        "calibration_requested_cells": metadata["calibration_cells"],
        "calibration_validated_cells": len(measurements),
        "product_cycle_contract_sha256": metadata["product_cycle_contract_sha256"],
        "task_population_sha256": metadata["task_population_sha256"],
        "candidate_lock_sha256": metadata["candidate_lock_sha256"],
        "source_byte_bindings_sha256": metadata["source_byte_bindings_sha256"],
        "complete_plan_sha256": metadata["complete_plan_sha256"],
        "planning_ready": True,
        "owner_approval_authenticated": metadata["authenticated_owner_approval"],
        "raw_artifacts_and_schemas_validated": True,
        "budget_authorized": False,
    }


def _task_arm_means(
    measurements: Sequence[Mapping[str, Any]], task_ids: Sequence[str], field: str
) -> dict[str, dict[str, float]]:
    grouped: dict[tuple[str, str], list[Decimal]] = defaultdict(list)
    for cell in measurements:
        grouped[(cell["task_id"], cell["arm"])].append(cell[field])
    return {
        task_id: {
            arm: float(
                sum(grouped[(task_id, arm)], Decimal(0))
                / Decimal(len(grouped[(task_id, arm)]))
            )
            for arm in PRODUCT.ARMS
        }
        for task_id in task_ids
    }


def _contrast_vectors(
    measurements: Sequence[Mapping[str, Any]],
    task_ids: Sequence[str],
    cluster_by_task: Mapping[str, str],
    *,
    numerator: str,
    denominator: str,
) -> list[dict[str, Any]]:
    elapsed = _task_arm_means(measurements, task_ids, "elapsed_time")
    costs = _task_arm_means(measurements, task_ids, "normalized_cost")
    quality = _task_arm_means(measurements, task_ids, "code_quality")
    task_vectors: list[dict[str, Any]] = []
    for task_id in task_ids:
        elapsed_num = elapsed[task_id][numerator]
        elapsed_den = elapsed[task_id][denominator]
        cost_num = costs[task_id][numerator]
        cost_den = costs[task_id][denominator]
        _require(elapsed_num > 0 and elapsed_den > 0, f"elapsed log ratio is undefined for {task_id}")
        _require(cost_num >= 0 and cost_den >= 0, f"normalized costs must be nonnegative for {task_id}")
        vector = {
            "task_id": task_id,
            "independence_cluster_sha256": cluster_by_task[task_id],
            "elapsed_log_ratio": math.log(elapsed_num / elapsed_den),
            "cost_numerator_mean": cost_num,
            "cost_denominator_mean": cost_den,
            "quality_difference": quality[task_id][numerator] - quality[task_id][denominator],
        }
        _require(
            all(
                math.isfinite(float(vector[field]))
                for field in (
                    "elapsed_log_ratio",
                    "cost_numerator_mean",
                    "cost_denominator_mean",
                    "quality_difference",
                )
            ),
            f"contrast vector contains non-finite values for {task_id}",
        )
        task_vectors.append(vector)
    _require(
        _mean([float(row["cost_denominator_mean"]) for row in task_vectors]) > 0,
        "aggregate normalized-cost denominator must be positive",
    )
    grouped: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for vector in task_vectors:
        grouped[vector["independence_cluster_sha256"]].append(vector)
    vectors: list[dict[str, Any]] = []
    for cluster_sha in sorted(grouped):
        rows = grouped[cluster_sha]
        vectors.append(
            {
                "independence_cluster_sha256": cluster_sha,
                "task_ids": sorted(str(row["task_id"]) for row in rows),
                "task_weight": len(rows),
                "elapsed_log_ratio": _mean(
                    [float(row["elapsed_log_ratio"]) for row in rows]
                ),
                "cost_numerator_mean": _mean(
                    [float(row["cost_numerator_mean"]) for row in rows]
                ),
                "cost_denominator_mean": _mean(
                    [float(row["cost_denominator_mean"]) for row in rows]
                ),
                "quality_difference": _mean(
                    [float(row["quality_difference"]) for row in rows]
                ),
            }
        )
    return vectors


def shared_cluster_resamples(
    *,
    source_clusters: int,
    target_clusters: int,
    resamples: int,
    seed: int,
) -> tuple[list[tuple[int, ...]], str]:
    """Return one deterministic cluster-index stream shared by every endpoint."""
    _positive_int(source_clusters, "source_clusters")
    _positive_int(target_clusters, "target_clusters", maximum=MAX_CANDIDATE_CLUSTERS)
    _positive_int(resamples, "resamples", maximum=MAX_RESAMPLES)
    _require(type(seed) is int and 0 <= seed < 2**63, "seed is invalid")
    draws: list[tuple[int, ...]] = []
    digest = hashlib.sha256()
    digest.update(RESAMPLING_DOMAIN)
    digest.update(seed.to_bytes(8, "big"))
    digest.update(source_clusters.to_bytes(4, "big"))
    digest.update(target_clusters.to_bytes(4, "big"))
    digest.update(resamples.to_bytes(4, "big"))
    for draw_index in range(resamples):
        draw: list[int] = []
        for position in range(target_clusters):
            payload = (
                RESAMPLING_DOMAIN
                + seed.to_bytes(8, "big")
                + source_clusters.to_bytes(4, "big")
                + target_clusters.to_bytes(4, "big")
                + draw_index.to_bytes(4, "big")
                + position.to_bytes(4, "big")
            )
            index = int.from_bytes(hashlib.sha256(payload).digest()[:8], "big") % source_clusters
            draw.append(index)
            digest.update(index.to_bytes(4, "big"))
        draws.append(tuple(draw))
        digest.update(b"\n")
    return draws, digest.hexdigest()


def _mean(values: Sequence[float]) -> float:
    _require(bool(values), "cannot average an empty sequence")
    value = math.fsum(values) / len(values)
    _require(math.isfinite(value), "mean is non-finite")
    return value


def _weighted_mean(values: Sequence[tuple[float, int]]) -> float:
    _require(bool(values), "cannot average an empty weighted sequence")
    total_weight = sum(weight for _, weight in values)
    _require(total_weight > 0, "weighted mean has no positive weight")
    value = math.fsum(item * weight for item, weight in values) / total_weight
    _require(math.isfinite(value), "weighted mean is non-finite")
    return value


def _quantile(sorted_values: Sequence[float], probability: float) -> float:
    _require(bool(sorted_values), "cannot take a quantile of an empty sequence")
    _require(0 <= probability <= 1, "quantile probability is invalid")
    position = (len(sorted_values) - 1) * probability
    lower = int(math.floor(position))
    upper = int(math.ceil(position))
    if lower == upper:
        return float(sorted_values[lower])
    weight = position - lower
    return float(sorted_values[lower] * (1 - weight) + sorted_values[upper] * weight)


def _vector_parameters(vectors: Sequence[Mapping[str, Any]]) -> dict[str, float]:
    return {
        "elapsed_log_mean": _weighted_mean(
            [(float(row["elapsed_log_ratio"]), int(row["task_weight"])) for row in vectors]
        ),
        "cost_numerator_mean": _weighted_mean(
            [(float(row["cost_numerator_mean"]), int(row["task_weight"])) for row in vectors]
        ),
        "cost_denominator_mean": _weighted_mean(
            [(float(row["cost_denominator_mean"]), int(row["task_weight"])) for row in vectors]
        ),
        "quality_difference_mean": _weighted_mean(
            [(float(row["quality_difference"]), int(row["task_weight"])) for row in vectors]
        ),
    }


def _draw_statistics(
    vectors: Sequence[Mapping[str, Any]],
    draws: Sequence[Sequence[int]],
    *,
    alternative: Mapping[str, Any] | None,
) -> tuple[list[dict[str, float | None]], dict[str, float]]:
    parameters = _vector_parameters(vectors)
    _require(
        parameters["cost_numerator_mean"] >= 0
        and parameters["cost_denominator_mean"] > 0,
        "observed normalized-cost ratio is undefined",
    )
    observed_cost_ratio = (
        parameters["cost_numerator_mean"] / parameters["cost_denominator_mean"]
    )
    statistics: list[dict[str, float | None]] = []
    for draw in draws:
        selected = [vectors[index] for index in draw]
        elapsed_residual = _weighted_mean(
            [
                (float(row["elapsed_log_ratio"]), int(row["task_weight"]))
                for row in selected
            ]
        ) - parameters["elapsed_log_mean"]
        quality_residual = _weighted_mean(
            [
                (float(row["quality_difference"]), int(row["task_weight"]))
                for row in selected
            ]
        ) - parameters["quality_difference_mean"]
        drawn_cost_num = _weighted_mean(
            [
                (float(row["cost_numerator_mean"]), int(row["task_weight"]))
                for row in selected
            ]
        )
        drawn_cost_den = _weighted_mean(
            [
                (float(row["cost_denominator_mean"]), int(row["task_weight"]))
                for row in selected
            ]
        )
        _require(drawn_cost_num >= 0 and drawn_cost_den >= 0, "bootstrap normalized costs became negative")
        drawn_cost_ratio = (
            drawn_cost_num / drawn_cost_den if drawn_cost_den > 0 else None
        )
        if alternative is None:
            elapsed_value = parameters["elapsed_log_mean"] + elapsed_residual
            cost_value = drawn_cost_ratio
            quality_value = parameters["quality_difference_mean"] + quality_residual
        else:
            elapsed_value = math.log(
                alternative["elapsed_time"]["planning_alternative_ratio"]
            ) + elapsed_residual
            cost_alt = alternative["normalized_cost"]["planning_alternative_ratio"]
            cost_value = None
            if drawn_cost_ratio is not None:
                cost_value = (
                    cost_alt
                    if observed_cost_ratio == 0
                    else drawn_cost_ratio * cost_alt / observed_cost_ratio
                )
            quality_value = alternative["code_quality"][
                "planning_alternative_difference"
            ] + quality_residual
        row = {
            "elapsed_time": elapsed_value,
            "normalized_cost": cost_value,
            "code_quality": quality_value,
        }
        _require(
            math.isfinite(elapsed_value)
            and math.isfinite(quality_value)
            and (cost_value is None or math.isfinite(cost_value)),
            "resampled endpoint is non-finite",
        )
        statistics.append(row)
    return statistics, parameters


def _ci_offsets(
    vectors: Sequence[Mapping[str, Any]], draws: Sequence[Sequence[int]]
) -> tuple[dict[str, float], int]:
    bootstrap, parameters = _draw_statistics(vectors, draws, alternative=None)
    observed = {
        "elapsed_time": parameters["elapsed_log_mean"],
        "normalized_cost": parameters["cost_numerator_mean"]
        / parameters["cost_denominator_mean"],
        "code_quality": parameters["quality_difference_mean"],
    }
    cost_values: list[float] = []
    for row in bootstrap:
        value = row["normalized_cost"]
        if value is not None:
            cost_values.append(float(value))
    _require(bool(cost_values), "every cost resample has a zero denominator")
    deviations = {
        "elapsed_time": sorted(
            float(value) - observed["elapsed_time"]
            for row in bootstrap
            if (value := row["elapsed_time"]) is not None
        ),
        "normalized_cost": sorted(
            value - observed["normalized_cost"] for value in cost_values
        ),
        "code_quality": sorted(
            float(value) - observed["code_quality"]
            for row in bootstrap
            if (value := row["code_quality"]) is not None
        ),
    }
    offsets = {
        "elapsed_time": _quantile(deviations["elapsed_time"], CONFIDENCE),
        "normalized_cost": _quantile(deviations["normalized_cost"], CONFIDENCE),
        "code_quality": _quantile(deviations["code_quality"], 1 - CONFIDENCE),
    }
    _require(all(math.isfinite(item) for item in offsets.values()), "CI offset is non-finite")
    return offsets, len(bootstrap) - len(cost_values)


def _power_gate(
    marginal_power: Mapping[str, Any], joint_power: Any, *, target: float = TARGET_POWER
) -> bool:
    if set(marginal_power) != set(ENDPOINTS):
        return False
    values: list[float] = []
    for endpoint in ENDPOINTS:
        value = marginal_power[endpoint]
        if type(value) not in {int, float} or not math.isfinite(float(value)):
            return False
        values.append(float(value))
    if type(joint_power) not in {int, float} or not math.isfinite(float(joint_power)):
        return False
    return all(item >= target for item in values) and float(joint_power) >= target


def _power_for_contrast(
    vectors: Sequence[Mapping[str, Any]],
    alternatives: Mapping[str, Any],
    draws: Sequence[Sequence[int]],
    *,
    task_clusters: int,
    resampling_sha256: str,
) -> dict[str, Any]:
    offsets, zero_denominator_draws = _ci_offsets(vectors, draws)
    simulated, _ = _draw_statistics(vectors, draws, alternative=alternatives)
    thresholds = {
        "elapsed_time": math.log(alternatives["elapsed_time"]["required_ratio_max"]),
        "normalized_cost": alternatives["normalized_cost"]["required_ratio_max"],
        "code_quality": alternatives["code_quality"]["required_difference_min"],
    }
    pass_counts = {endpoint: 0 for endpoint in ENDPOINTS}
    joint_count = 0
    for row in simulated:
        cost_value = row["normalized_cost"]
        elapsed_value = row["elapsed_time"]
        quality_value = row["code_quality"]
        _require(
            elapsed_value is not None and quality_value is not None,
            "time and quality resamples must always be defined",
        )
        assert elapsed_value is not None and quality_value is not None
        passed = {
            "elapsed_time": float(elapsed_value) + offsets["elapsed_time"]
            <= thresholds["elapsed_time"],
            "normalized_cost": cost_value is not None
            and float(cost_value) + offsets["normalized_cost"]
            <= thresholds["normalized_cost"],
            "code_quality": float(quality_value) + offsets["code_quality"]
            >= thresholds["code_quality"],
        }
        for endpoint in ENDPOINTS:
            pass_counts[endpoint] += int(passed[endpoint])
        joint_count += int(all(passed.values()))
    total = len(simulated)
    marginal = {
        endpoint: _stable_float(pass_counts[endpoint] / total) for endpoint in ENDPOINTS
    }
    joint = _stable_float(joint_count / total)
    gate = _power_gate(marginal, joint)
    return {
        "comparison": alternatives["comparison"],
        "role": alternatives["role"],
        "task_clusters": task_clusters,
        "resamples": total,
        "target_power": TARGET_POWER,
        "marginal_power": marginal,
        "joint_all_endpoint_power": joint,
        "all_marginal_at_least_target": all(
            marginal[endpoint] >= TARGET_POWER for endpoint in ENDPOINTS
        ),
        "joint_at_least_target": joint >= TARGET_POWER,
        "finite_estimates_and_ci_inputs": True,
        "zero_denominator_resample_policy": "automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1",
        "zero_denominator_draws": zero_denominator_draws,
        "shared_resampling_sha256": resampling_sha256,
        "independence_shortcut_used": False,
        "statistical_gate_met": gate,
    }


def _calibration_estimates(
    vectors: Sequence[Mapping[str, Any]], draws: Sequence[Sequence[int]], *, comparison: str
) -> dict[str, Any]:
    parameters = _vector_parameters(vectors)
    offsets, zero_denominator_draws = _ci_offsets(vectors, draws)
    elapsed_log = parameters["elapsed_log_mean"]
    cost_ratio = (
        parameters["cost_numerator_mean"] / parameters["cost_denominator_mean"]
    )
    quality = parameters["quality_difference_mean"]
    vectors_sha = PRODUCT.value_sha256(
        [
            {
                "independence_cluster_sha256": row[
                    "independence_cluster_sha256"
                ],
                "task_ids": row["task_ids"],
                "task_weight": row["task_weight"],
                "elapsed_log_ratio": _stable_float(row["elapsed_log_ratio"]),
                "cost_numerator_mean": _stable_float(row["cost_numerator_mean"]),
                "cost_denominator_mean": _stable_float(row["cost_denominator_mean"]),
                "quality_difference": _stable_float(row["quality_difference"]),
            }
            for row in vectors
        ]
    )
    return {
        "comparison": comparison,
        "n_task_clusters": len(vectors),
        "n_tasks": sum(int(row["task_weight"]) for row in vectors),
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "paired_task_vectors_sha256": vectors_sha,
        "elapsed_time": {
            "estimand": "paired_task_geometric_mean_ratio",
            "paired_task_geometric_mean_ratio": _stable_float(math.exp(elapsed_log)),
            "one_sided_upper_bound": _stable_float(
                math.exp(elapsed_log + offsets["elapsed_time"])
            ),
        },
        "normalized_cost": {
            "estimand": "ratio_of_equal_task_weighted_task_arm_mean_costs",
            "equal_task_weighted_mean_ratio": _stable_float(cost_ratio),
            "one_sided_upper_bound": _stable_float(
                cost_ratio + offsets["normalized_cost"]
            ),
            "geometric_mean_task_ratios_used": False,
            "structural_zero_treatment_cost_supported": True,
            "zero_denominator_resample_policy": "automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1",
            "zero_denominator_draws": zero_denominator_draws,
        },
        "code_quality": {
            "estimand": "paired_task_mean_difference",
            "paired_task_mean_difference": _stable_float(quality),
            "one_sided_lower_bound": _stable_float(
                quality + offsets["code_quality"]
            ),
        },
        "finite_estimates_and_ci_inputs": True,
    }


def _decision(
    *,
    rows: Sequence[Mapping[str, Any]],
    result_key: str,
    comparison: str,
    role: str,
    state: str,
    evidence_class: str,
    authenticated_owner_approval: bool,
    alters_benchmark_primary_verdict: bool | None = None,
) -> dict[str, Any]:
    selected = next(
        (
            row["task_clusters"]
            for row in rows
            if isinstance(row[result_key], dict)
            and row[result_key]["statistical_gate_met"] is True
        ),
        None,
    )
    statistical_gate = selected is not None
    eligible = (
        state == "frozen"
        and evidence_class == "development_measurement"
        and authenticated_owner_approval
    )
    passed = bool(statistical_gate and eligible)
    result = {
        "comparison": comparison,
        "role": role,
        "selected_task_clusters": selected,
        "statistical_gate_met": statistical_gate,
        "every_marginal_and_joint_power_at_least_0_80": statistical_gate,
        "eligible_evidence": eligible,
        "passed": passed,
        "reason": (
            "passed"
            if passed
            else "synthetic_fixture_not_decision_eligible"
            if evidence_class == "synthetic_fixture"
            else "pending_calibration"
            if state == "pending"
            else "candidate_not_owner_approved"
            if state == "candidate"
            else "no_authenticated_owner_approval_trust_anchor"
            if not authenticated_owner_approval
            else "no_candidate_task_count_clears_every_power_gate"
        ),
    }
    if alters_benchmark_primary_verdict is not None:
        result["alters_benchmark_primary_verdict"] = (
            alters_benchmark_primary_verdict
        )
    return result


def analyze_calibration(calibration: Any, bundle: ArtifactBundle) -> dict[str, Any]:
    """Compute deterministic marginal and direct joint power for both contrasts."""
    root, measurements, metadata = _validated_calibration(calibration, bundle)
    task_ids = metadata["task_ids"]
    primary_vectors = _contrast_vectors(
        measurements,
        task_ids,
        metadata["cluster_by_task"],
        numerator="retrieved_memory",
        denominator="no_memory",
    )
    diagnostic_vectors = _contrast_vectors(
        measurements,
        task_ids,
        metadata["cluster_by_task"],
        numerator="retrieved_memory",
        denominator="preoptimization_memory",
    )
    planning = metadata["planning"]
    calibration_draws, calibration_draws_sha = shared_cluster_resamples(
        source_clusters=metadata["task_clusters"],
        target_clusters=metadata["task_clusters"],
        resamples=planning["resamples"],
        seed=planning["seed"],
    )
    estimates = {
        "primary": _calibration_estimates(
            primary_vectors, calibration_draws, comparison=PRIMARY_COMPARISON
        ),
        "product_diagnostic": _calibration_estimates(
            diagnostic_vectors,
            calibration_draws,
            comparison=DIAGNOSTIC_COMPARISON,
        ),
    }

    power_rows: list[dict[str, Any]] = []
    for task_count in planning["candidate_task_clusters"]:
        requested_cells = task_count * planning["repetitions_per_arm"] * len(PRODUCT.ARMS)
        draws, draws_sha = shared_cluster_resamples(
            source_clusters=metadata["task_clusters"],
            target_clusters=task_count,
            resamples=planning["resamples"],
            seed=planning["seed"],
        )
        if metadata["state"] == "pending":
            primary_power = None
            diagnostic_power = None
        else:
            primary_power = _power_for_contrast(
                primary_vectors,
                metadata["alternatives"]["primary"],
                draws,
                task_clusters=task_count,
                resampling_sha256=draws_sha,
            )
            diagnostic_power = _power_for_contrast(
                diagnostic_vectors,
                metadata["alternatives"]["product_diagnostic"],
                draws,
                task_clusters=task_count,
                resampling_sha256=draws_sha,
            )
        power_rows.append(
            {
                "task_clusters": task_count,
                "repetitions_per_arm": planning["repetitions_per_arm"],
                "arms": len(PRODUCT.ARMS),
                "requested_cells": requested_cells,
                "maximum_agent_invocations": requested_cells,
                "agent_retry_limit": 0,
                "replacement_cell_limit": 0,
                "reserve_cell_limit": 0,
                "shared_resampling_sha256": draws_sha,
                "primary": primary_power,
                "product_diagnostic": diagnostic_power,
            }
        )

    benchmark_decision = _decision(
        rows=power_rows,
        result_key="primary",
        comparison=PRIMARY_COMPARISON,
        role="benchmark_primary_power_gate",
        state=metadata["state"],
        evidence_class=metadata["evidence_class"],
        authenticated_owner_approval=metadata["authenticated_owner_approval"],
    )
    product_decision = _decision(
        rows=power_rows,
        result_key="product_diagnostic",
        comparison=DIAGNOSTIC_COMPARISON,
        role="separate_product_improvement_gate",
        state=metadata["state"],
        evidence_class=metadata["evidence_class"],
        authenticated_owner_approval=metadata["authenticated_owner_approval"],
        alters_benchmark_primary_verdict=False,
    )
    report: dict[str, Any] = {
        "schema": REPORT_SCHEMA,
        "state": metadata["state"],
        "evidence_class": metadata["evidence_class"],
        "status": "pending" if metadata["state"] == "pending" else "evaluated",
        "final_calibration_sha256": root["identity_sha256"],
        "owner_approval_receipt_sha256": root[
            "owner_approval_receipt_sha256"
        ],
        "owner_approval_authenticated": metadata[
            "authenticated_owner_approval"
        ],
        "planning_sha256": planning["identity_sha256"],
        "complete_plan": planning,
        "planning_alternatives": metadata["alternatives"],
        "method": {
            "resampling": RESAMPLING_METHOD,
            "seed": planning["seed"],
            "resamples": planning["resamples"],
            "cluster_unit": "derived_independence_cluster",
            "shared_draws_across_endpoints_and_contrasts": True,
            "cross_endpoint_dependence_preserved": True,
            "joint_probability_method": "same_draw_all_endpoint_pass_frequency",
            "parametric_independence_shortcut_used": False,
            "confidence": CONFIDENCE,
            "target_power": TARGET_POWER,
        },
        "design": {
            "arms": list(PRODUCT.ARMS),
            "calibration_task_clusters": metadata["task_clusters"],
            "calibration_tasks": metadata["calibration_tasks"],
            "calibration_repetitions_per_arm": metadata["calibration_repetitions"],
            "calibration_requested_cells": metadata["calibration_cells"],
            "calibration_maximum_agent_invocations": metadata["calibration_cells"],
            "candidate_task_clusters": planning["candidate_task_clusters"],
        },
        "calibration_estimates": estimates,
        "calibration_shared_resampling_sha256": calibration_draws_sha,
        "power_candidates": power_rows,
        "benchmark_power_decision": benchmark_decision,
        "product_improvement_gate": product_decision,
        "integrity": {
            "repetitions_averaged_inside_task_arm": True,
            "independent_task_clusters": metadata["task_clusters"],
            "independence_closure_algorithm": "family_fix_session_material_edge_transitive_closure_v1",
            "complete_plan_sha256": metadata["complete_plan_sha256"],
            "v4_implementation_lock_sha256": metadata[
                "v4_implementation_lock_sha256"
            ],
            "execution_contract_sha256": metadata["execution_contract_sha256"],
            "execution_attestations_sha256": metadata[
                "execution_attestations_sha256"
            ],
            "raw_artifacts_and_schemas_validated": True,
            "owner_approval_authenticated": metadata[
                "authenticated_owner_approval"
            ],
            "all_calibration_members_development_calibration": True,
            "optimization_members_used": 0,
            "holdout_members_used": 0,
            "exact_four_arm_cells_and_invocations": True,
            "product_cycle_contract_sha256": metadata[
                "product_cycle_contract_sha256"
            ],
            "product_cycle_schema_file_sha256": metadata[
                "product_cycle_schema_file_sha256"
            ],
            "product_cycle_evidence_canonical_bytes_sha256": metadata[
                "product_cycle_evidence_canonical_bytes_sha256"
            ],
            "task_population_sha256": metadata["task_population_sha256"],
            "task_population_schema_file_sha256": metadata[
                "task_population_schema_file_sha256"
            ],
            "task_population_contract_file_sha256": metadata[
                "task_population_contract_file_sha256"
            ],
            "candidate_lock_sha256": metadata["candidate_lock_sha256"],
            "source_byte_bindings_sha256": metadata[
                "source_byte_bindings_sha256"
            ],
            "locked_identities_sha256": metadata["locked_identities_sha256"],
            "budget_authorized": False,
            "provider_or_model_calls_performed": False,
        },
    }
    report["report_sha256"] = _self_sha256(report, "report_sha256")
    validate_power_report(report, bundle)
    return report


def _validate_power_result(
    value: Any,
    label: str,
    *,
    expected_comparison: str,
    expected_role: str,
    expected_task_clusters: int,
    expected_resamples: int,
    expected_resampling_sha256: str,
) -> None:
    result = _exact_keys(
        value,
        {
            "comparison",
            "role",
            "task_clusters",
            "resamples",
            "target_power",
            "marginal_power",
            "joint_all_endpoint_power",
            "all_marginal_at_least_target",
            "joint_at_least_target",
            "finite_estimates_and_ci_inputs",
            "zero_denominator_resample_policy",
            "zero_denominator_draws",
            "shared_resampling_sha256",
            "independence_shortcut_used",
            "statistical_gate_met",
        },
        label,
    )
    _require(result["comparison"] == expected_comparison, f"{label}.comparison changed")
    _require(result["role"] == expected_role, f"{label}.role changed")
    _require(
        result["task_clusters"] == expected_task_clusters,
        f"{label}.task_clusters changed",
    )
    _require(
        result["resamples"] == expected_resamples,
        f"{label}.resamples changed",
    )
    _require(_finite(result["target_power"], f"{label}.target_power") == TARGET_POWER, f"{label}.target_power changed")
    marginal = _exact_keys(result["marginal_power"], set(ENDPOINTS), f"{label}.marginal_power")
    for endpoint in ENDPOINTS:
        value = _finite(marginal[endpoint], f"{label}.marginal_power.{endpoint}")
        _require(0 <= value <= 1, f"{label}.marginal_power.{endpoint} is outside [0,1]")
    joint = _finite(result["joint_all_endpoint_power"], f"{label}.joint_all_endpoint_power")
    _require(0 <= joint <= 1, f"{label}.joint_all_endpoint_power is outside [0,1]")
    expected_marginal = all(marginal[endpoint] >= TARGET_POWER for endpoint in ENDPOINTS)
    expected_joint = joint >= TARGET_POWER
    _require(result["all_marginal_at_least_target"] is expected_marginal, f"{label} marginal gate drift")
    _require(result["joint_at_least_target"] is expected_joint, f"{label} joint gate drift")
    _require(result["finite_estimates_and_ci_inputs"] is True, f"{label} finite-input gate is false")
    _require(
        result["zero_denominator_resample_policy"]
        == "automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1",
        f"{label} zero-denominator policy drift",
    )
    _require(
        type(result["zero_denominator_draws"]) is int
        and 0 <= result["zero_denominator_draws"] <= expected_resamples,
        f"{label} zero-denominator draw count is invalid",
    )
    _require(
        _sha256(
            result["shared_resampling_sha256"],
            f"{label}.shared_resampling_sha256",
        )
        == expected_resampling_sha256,
        f"{label} resampling identity drift",
    )
    _require(result["independence_shortcut_used"] is False, f"{label} independence shortcut is prohibited")
    _require(
        result["statistical_gate_met"] is _power_gate(marginal, joint),
        f"{label} statistical gate drift",
    )


def _validate_calibration_estimate(
    value: Any,
    label: str,
    *,
    expected_comparison: str,
    expected_clusters: int,
    expected_tasks: int,
) -> None:
    estimate = _exact_keys(
        value,
        {
            "comparison",
            "n_task_clusters",
            "n_tasks",
            "repetition_aggregation",
            "paired_task_vectors_sha256",
            "elapsed_time",
            "normalized_cost",
            "code_quality",
            "finite_estimates_and_ci_inputs",
        },
        label,
    )
    _require(
        estimate["comparison"] == expected_comparison,
        f"{label}.comparison changed",
    )
    estimate_clusters = _positive_int(
        estimate["n_task_clusters"], f"{label}.n_task_clusters"
    )
    _require(estimate_clusters == expected_clusters, f"{label}.n_task_clusters changed")
    _require(
        _positive_int(estimate["n_tasks"], f"{label}.n_tasks") == expected_tasks,
        f"{label}.n_tasks changed",
    )
    _require(
        estimate["repetition_aggregation"]
        == "arithmetic_mean_within_task_arm_before_contrast",
        f"{label}.repetition_aggregation changed",
    )
    _sha256(estimate["paired_task_vectors_sha256"], f"{label}.paired_task_vectors_sha256")

    elapsed = _exact_keys(
        estimate["elapsed_time"],
        {
            "estimand",
            "paired_task_geometric_mean_ratio",
            "one_sided_upper_bound",
        },
        f"{label}.elapsed_time",
    )
    _require(
        elapsed["estimand"] == "paired_task_geometric_mean_ratio",
        f"{label}.elapsed_time estimand changed",
    )
    for field in ("paired_task_geometric_mean_ratio", "one_sided_upper_bound"):
        _require(
            _finite(elapsed[field], f"{label}.elapsed_time.{field}") > 0,
            f"{label}.elapsed_time.{field} must be positive",
        )

    cost = _exact_keys(
        estimate["normalized_cost"],
        {
            "estimand",
            "equal_task_weighted_mean_ratio",
            "one_sided_upper_bound",
            "geometric_mean_task_ratios_used",
            "structural_zero_treatment_cost_supported",
            "zero_denominator_resample_policy",
            "zero_denominator_draws",
        },
        f"{label}.normalized_cost",
    )
    _require(
        cost["estimand"] == "ratio_of_equal_task_weighted_task_arm_mean_costs",
        f"{label}.normalized_cost estimand changed",
    )
    for field in ("equal_task_weighted_mean_ratio", "one_sided_upper_bound"):
        _require(
            _finite(cost[field], f"{label}.normalized_cost.{field}") >= 0,
            f"{label}.normalized_cost.{field} must be nonnegative",
        )
    _require(
        cost["geometric_mean_task_ratios_used"] is False,
        f"{label}.normalized_cost may not use geometric task-ratio averaging",
    )
    _require(
        cost["structural_zero_treatment_cost_supported"] is True,
        f"{label}.normalized_cost must support authenticated treatment structural zeros",
    )
    _require(
        cost["zero_denominator_resample_policy"]
        == "automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1",
        f"{label}.normalized_cost zero-denominator policy drift",
    )
    _require(
        type(cost["zero_denominator_draws"]) is int
        and cost["zero_denominator_draws"] >= 0,
        f"{label}.normalized_cost zero-denominator count is invalid",
    )

    quality = _exact_keys(
        estimate["code_quality"],
        {"estimand", "paired_task_mean_difference", "one_sided_lower_bound"},
        f"{label}.code_quality",
    )
    _require(
        quality["estimand"] == "paired_task_mean_difference",
        f"{label}.code_quality estimand changed",
    )
    _finite(
        quality["paired_task_mean_difference"],
        f"{label}.code_quality.paired_task_mean_difference",
    )
    _finite(
        quality["one_sided_lower_bound"],
        f"{label}.code_quality.one_sided_lower_bound",
    )
    _require(
        estimate["finite_estimates_and_ci_inputs"] is True,
        f"{label} finite-input gate is false",
    )


def validate_power_report(report: Any, bundle: ArtifactBundle) -> None:
    """Recheck report identities, arithmetic, null states, and both power gates."""
    try:
        bundle.schema_validator.validate(
            report, SCHEMA_PATHS["power_report"].name, label="power report"
        )
    except DRAFT.SchemaError as exc:
        raise PowerV4Error(str(exc)) from exc
    root = _exact_keys(
        report,
        {
            "schema",
            "state",
            "evidence_class",
            "status",
            "final_calibration_sha256",
            "owner_approval_receipt_sha256",
            "owner_approval_authenticated",
            "planning_sha256",
            "complete_plan",
            "planning_alternatives",
            "method",
            "design",
            "calibration_estimates",
            "calibration_shared_resampling_sha256",
            "power_candidates",
            "benchmark_power_decision",
            "product_improvement_gate",
            "integrity",
            "report_sha256",
        },
        "power_report",
    )
    _require(root["schema"] == REPORT_SCHEMA, "power report schema changed")
    state = root["state"]
    _require(state in {"pending", "candidate", "frozen"}, "power report state is invalid")
    evidence = root["evidence_class"]
    _require(evidence in {"synthetic_fixture", "development_measurement"}, "power report evidence_class is invalid")
    _require(
        state != "frozen" or evidence == "development_measurement",
        "a frozen power report requires development measurement evidence",
    )
    _sha256(root["final_calibration_sha256"], "power_report.final_calibration_sha256")
    _sha256(root["planning_sha256"], "power_report.planning_sha256")
    if state == "frozen":
        _sha256(
            root["owner_approval_receipt_sha256"],
            "power_report.owner_approval_receipt_sha256",
        )
    else:
        _require(
            root["owner_approval_receipt_sha256"] is None,
            "only a frozen power report may carry owner approval",
        )
    _require(
        root["owner_approval_authenticated"] is False,
        "no owner-approval trust anchor is configured",
    )
    _sha256(
        root["calibration_shared_resampling_sha256"],
        "power_report.calibration_shared_resampling_sha256",
    )
    method = _exact_keys(
        root["method"],
        {
            "resampling",
            "seed",
            "resamples",
            "cluster_unit",
            "shared_draws_across_endpoints_and_contrasts",
            "cross_endpoint_dependence_preserved",
            "joint_probability_method",
            "parametric_independence_shortcut_used",
            "confidence",
            "target_power",
        },
        "power_report.method",
    )
    _require(
        method["resampling"] == RESAMPLING_METHOD,
        "power_report resampling method changed",
    )
    _require(
        type(method["seed"]) is int and 0 <= method["seed"] < 2**63,
        "power_report seed is invalid",
    )
    resamples = _positive_int(
        method["resamples"], "power_report.method.resamples", maximum=MAX_RESAMPLES
    )
    _require(resamples >= MIN_RESAMPLES, "power_report resample floor changed")
    _require(
        method["cluster_unit"] == "derived_independence_cluster",
        "power_report cluster unit changed",
    )
    _require(
        method["shared_draws_across_endpoints_and_contrasts"] is True,
        "power_report must share draws across endpoints and contrasts",
    )
    _require(
        method["cross_endpoint_dependence_preserved"] is True,
        "power_report must preserve cross-endpoint dependence",
    )
    _require(
        method["joint_probability_method"]
        == "same_draw_all_endpoint_pass_frequency",
        "power_report joint probability method changed",
    )
    _require(
        method["parametric_independence_shortcut_used"] is False,
        "power_report independence shortcut is prohibited",
    )
    _require(
        _finite(method["confidence"], "power_report.method.confidence")
        == CONFIDENCE,
        "power_report confidence changed",
    )
    _require(
        _finite(method["target_power"], "power_report.method.target_power")
        == TARGET_POWER,
        "power_report target power changed",
    )

    design = _exact_keys(
        root["design"],
        {
            "arms",
            "calibration_task_clusters",
            "calibration_tasks",
            "calibration_repetitions_per_arm",
            "calibration_requested_cells",
            "calibration_maximum_agent_invocations",
            "candidate_task_clusters",
        },
        "power_report.design",
    )
    _require(design["arms"] == list(PRODUCT.ARMS), "power_report arms changed")
    calibration_clusters = _positive_int(
        design["calibration_task_clusters"],
        "power_report.design.calibration_task_clusters",
    )
    _require(
        calibration_clusters >= MIN_CALIBRATION_CLUSTERS,
        "power_report has too few calibration task clusters",
    )
    calibration_tasks = _positive_int(
        design["calibration_tasks"], "power_report.design.calibration_tasks"
    )
    _require(
        calibration_tasks >= calibration_clusters,
        "power_report has fewer tasks than independence clusters",
    )
    repetitions = _positive_int(
        design["calibration_repetitions_per_arm"],
        "power_report.design.calibration_repetitions_per_arm",
    )
    calibration_cells = calibration_tasks * repetitions * len(PRODUCT.ARMS)
    _require(
        _positive_int(
            design["calibration_requested_cells"],
            "power_report.design.calibration_requested_cells",
        )
        == calibration_cells,
        "power_report calibration requested-cell arithmetic drift",
    )
    _require(
        _positive_int(
            design["calibration_maximum_agent_invocations"],
            "power_report.design.calibration_maximum_agent_invocations",
        )
        == calibration_cells,
        "power_report calibration invocation arithmetic drift",
    )
    candidate_counts = design["candidate_task_clusters"]
    _require(
        isinstance(candidate_counts, list)
        and bool(candidate_counts)
        and all(
            type(item) is int
            and MIN_CALIBRATION_CLUSTERS <= item <= MAX_CANDIDATE_CLUSTERS
            for item in candidate_counts
        ),
        "power_report candidate task counts are invalid",
    )
    _require(
        candidate_counts == sorted(set(candidate_counts)),
        "power_report candidate task counts must be unique and increasing",
    )
    raw_alternatives = _exact_keys(
        root["planning_alternatives"],
        {"primary", "product_diagnostic"},
        "power_report.planning_alternatives",
    )
    alternatives = {
        "primary": _validate_contrast_alternatives(
            raw_alternatives["primary"],
            label="power_report.planning_alternatives.primary",
            comparison=PRIMARY_COMPARISON,
            role="primary_development_contrast",
        ),
        "product_diagnostic": _validate_contrast_alternatives(
            raw_alternatives["product_diagnostic"],
            label="power_report.planning_alternatives.product_diagnostic",
            comparison=DIAGNOSTIC_COMPARISON,
            role="diagnostic_product_progress_only",
        ),
    }
    complete_plan = root["complete_plan"]
    _require(isinstance(complete_plan, dict), "power_report.complete_plan must be an object")
    _require(
        root["planning_sha256"] == complete_plan.get("identity_sha256")
        and root["planning_sha256"] == _self_sha256(complete_plan),
        "power_report planning identity drift",
    )
    plan_parity = {
        "method": method["resampling"],
        "target_power": method["target_power"],
        "confidence": method["confidence"],
        "resamples": method["resamples"],
        "seed": method["seed"],
        "candidate_task_clusters": candidate_counts,
        "arms": design["arms"],
        "repetitions_per_arm": repetitions,
        "planning_alternatives": alternatives,
    }
    for field, expected_value in plan_parity.items():
        _require(complete_plan.get(field) == expected_value, f"power_report complete-plan drift: {field}")
    if evidence == "development_measurement":
        _require(resamples == PRODUCTION_RESAMPLES, "production resample count drift")
        _require(method["seed"] == PRODUCTION_SEED, "production resampling seed drift")
        _require(tuple(candidate_counts) == PRODUCTION_CANDIDATE_GRID, "production candidate grid drift")
    _, expected_calibration_resampling_sha256 = shared_cluster_resamples(
        source_clusters=calibration_clusters,
        target_clusters=calibration_clusters,
        resamples=resamples,
        seed=method["seed"],
    )
    _require(
        root["calibration_shared_resampling_sha256"]
        == expected_calibration_resampling_sha256,
        "power_report calibration resampling identity drift",
    )

    estimates = _exact_keys(
        root["calibration_estimates"],
        {"primary", "product_diagnostic"},
        "power_report.calibration_estimates",
    )
    _validate_calibration_estimate(
        estimates["primary"],
        "power_report.calibration_estimates.primary",
        expected_comparison=PRIMARY_COMPARISON,
        expected_clusters=calibration_clusters,
        expected_tasks=calibration_tasks,
    )
    _validate_calibration_estimate(
        estimates["product_diagnostic"],
        "power_report.calibration_estimates.product_diagnostic",
        expected_comparison=DIAGNOSTIC_COMPARISON,
        expected_clusters=calibration_clusters,
        expected_tasks=calibration_tasks,
    )
    candidates = root["power_candidates"]
    _require(isinstance(candidates, list) and bool(candidates), "power report candidates are missing")
    _require(
        [row.get("task_clusters") if isinstance(row, dict) else None for row in candidates]
        == candidate_counts,
        "power report candidate rows do not match the design",
    )
    pending = state == "pending"
    _require(root["status"] == ("pending" if pending else "evaluated"), "power report status/state mismatch")
    for index, row in enumerate(candidates):
        label = f"power_report.power_candidates[{index}]"
        row = _exact_keys(
            row,
            {
                "task_clusters",
                "repetitions_per_arm",
                "arms",
                "requested_cells",
                "maximum_agent_invocations",
                "agent_retry_limit",
                "replacement_cell_limit",
                "reserve_cell_limit",
                "shared_resampling_sha256",
                "primary",
                "product_diagnostic",
            },
            label,
        )
        task_count = _positive_int(row["task_clusters"], f"{label}.task_clusters")
        _require(
            _positive_int(row["repetitions_per_arm"], f"{label}.repetitions_per_arm")
            == repetitions,
            f"{label} repetition arithmetic drift",
        )
        _require(
            _positive_int(row["arms"], f"{label}.arms") == len(PRODUCT.ARMS),
            f"{label} arm arithmetic drift",
        )
        expected_cells = task_count * repetitions * len(PRODUCT.ARMS)
        _require(
            _positive_int(row["requested_cells"], f"{label}.requested_cells")
            == expected_cells,
            f"{label} requested-cell arithmetic drift",
        )
        _require(
            _positive_int(
                row["maximum_agent_invocations"],
                f"{label}.maximum_agent_invocations",
            )
            == expected_cells,
            f"{label} invocation arithmetic drift",
        )
        for field in ("agent_retry_limit", "replacement_cell_limit", "reserve_cell_limit"):
            _require(
                type(row[field]) is int and row[field] == 0,
                f"{label}.{field} must remain integer zero",
            )
        shared_hash = _sha256(row["shared_resampling_sha256"], f"{label}.shared_resampling_sha256")
        _, expected_shared_hash = shared_cluster_resamples(
            source_clusters=calibration_clusters,
            target_clusters=task_count,
            resamples=resamples,
            seed=method["seed"],
        )
        _require(shared_hash == expected_shared_hash, f"{label} resampling identity drift")
        if pending:
            _require(row["primary"] is None and row["product_diagnostic"] is None, "pending power rows must retain null results")
        else:
            _validate_power_result(
                row["primary"],
                f"{label}.primary",
                expected_comparison=PRIMARY_COMPARISON,
                expected_role="primary_development_contrast",
                expected_task_clusters=task_count,
                expected_resamples=resamples,
                expected_resampling_sha256=shared_hash,
            )
            _validate_power_result(
                row["product_diagnostic"],
                f"{label}.product_diagnostic",
                expected_comparison=DIAGNOSTIC_COMPARISON,
                expected_role="diagnostic_product_progress_only",
                expected_task_clusters=task_count,
                expected_resamples=resamples,
                expected_resampling_sha256=shared_hash,
            )

    for key, result_key, comparison, role, diagnostic in (
        (
            "benchmark_power_decision",
            "primary",
            PRIMARY_COMPARISON,
            "benchmark_primary_power_gate",
            False,
        ),
        (
            "product_improvement_gate",
            "product_diagnostic",
            DIAGNOSTIC_COMPARISON,
            "separate_product_improvement_gate",
            True,
        ),
    ):
        decision_fields = {
            "comparison",
            "role",
            "selected_task_clusters",
            "statistical_gate_met",
            "every_marginal_and_joint_power_at_least_0_80",
            "eligible_evidence",
            "passed",
            "reason",
        }
        if diagnostic:
            decision_fields.add("alters_benchmark_primary_verdict")
        decision = _exact_keys(root[key], decision_fields, f"power_report.{key}")
        selected = next(
            (
                row["task_clusters"]
                for row in candidates
                if isinstance(row.get(result_key), dict)
                and row[result_key].get("statistical_gate_met") is True
            ),
            None,
        )
        _require(decision.get("comparison") == comparison, f"power_report.{key} comparison drift")
        _require(decision.get("role") == role, f"power_report.{key} role drift")
        decision_selected = decision["selected_task_clusters"]
        _require(
            (decision_selected is None and selected is None)
            or (
                type(decision_selected) is int
                and decision_selected == selected
            ),
            f"power_report.{key} selected count drift",
        )
        statistical = selected is not None
        _require(decision.get("statistical_gate_met") is statistical, f"power_report.{key} statistical gate drift")
        _require(
            decision.get("every_marginal_and_joint_power_at_least_0_80") is statistical,
            f"power_report.{key} all-power gate drift",
        )
        eligible = False
        _require(decision.get("eligible_evidence") is eligible, f"power_report.{key} eligibility drift")
        passed = bool(statistical and eligible)
        _require(decision.get("passed") is passed, f"power_report.{key} pass drift")
        reason = (
            "passed"
            if passed
            else "synthetic_fixture_not_decision_eligible"
            if evidence == "synthetic_fixture"
            else "pending_calibration"
            if state == "pending"
            else "candidate_not_owner_approved"
            if state == "candidate"
            else "no_authenticated_owner_approval_trust_anchor"
            if state == "frozen"
            else "no_candidate_task_count_clears_every_power_gate"
        )
        _require(decision.get("reason") == reason, f"power_report.{key} reason drift")
        if diagnostic:
            _require(
                decision["alters_benchmark_primary_verdict"] is False,
                "product diagnostic may not alter the benchmark primary verdict",
            )

    integrity = _exact_keys(
        root["integrity"],
        {
            "repetitions_averaged_inside_task_arm",
            "independent_task_clusters",
            "independence_closure_algorithm",
            "complete_plan_sha256",
            "v4_implementation_lock_sha256",
            "execution_contract_sha256",
            "execution_attestations_sha256",
            "raw_artifacts_and_schemas_validated",
            "owner_approval_authenticated",
            "all_calibration_members_development_calibration",
            "optimization_members_used",
            "holdout_members_used",
            "exact_four_arm_cells_and_invocations",
            "product_cycle_contract_sha256",
            "product_cycle_schema_file_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_sha256",
            "task_population_schema_file_sha256",
            "task_population_contract_file_sha256",
            "candidate_lock_sha256",
            "source_byte_bindings_sha256",
            "locked_identities_sha256",
            "budget_authorized",
            "provider_or_model_calls_performed",
        },
        "power_report.integrity",
    )
    for field in (
        "repetitions_averaged_inside_task_arm",
        "raw_artifacts_and_schemas_validated",
        "all_calibration_members_development_calibration",
        "exact_four_arm_cells_and_invocations",
    ):
        _require(integrity[field] is True, f"power_report.integrity.{field} must be true")
    _require(
        _positive_int(
            integrity["independent_task_clusters"],
            "power_report.integrity.independent_task_clusters",
        )
        == calibration_clusters,
        "power_report integrity task-cluster count drift",
    )
    _require(
        integrity["independence_closure_algorithm"]
        == "family_fix_session_material_edge_transitive_closure_v1",
        "power_report independence closure algorithm drift",
    )
    _require(
        integrity["complete_plan_sha256"] == root["planning_sha256"],
        "power_report integrity complete-plan drift",
    )
    _require(
        integrity["execution_contract_sha256"]
        == complete_plan["execution_contract_sha256"],
        "power_report execution-contract/complete-plan drift",
    )
    _require(
        integrity["v4_implementation_lock_sha256"]
        == complete_plan["v4_implementation_lock_sha256"],
        "power_report implementation-lock/complete-plan drift",
    )
    _require(
        integrity["v4_implementation_lock_sha256"]
        == _expected_implementation_lock(bundle)["identity_sha256"],
        "power_report implementation lock does not pin current analyzer and schemas",
    )
    _require(
        integrity["owner_approval_authenticated"] is False,
        "power report may not claim authenticated approval without a trust anchor",
    )
    _require(
        type(integrity["optimization_members_used"]) is int
        and integrity["optimization_members_used"] == 0,
        "optimization evidence is prohibited",
    )
    _require(
        type(integrity["holdout_members_used"]) is int
        and integrity["holdout_members_used"] == 0,
        "holdout evidence is prohibited",
    )
    for field in (
        "product_cycle_contract_sha256",
        "product_cycle_evidence_canonical_bytes_sha256",
        "task_population_sha256",
        "task_population_contract_file_sha256",
        "candidate_lock_sha256",
        "source_byte_bindings_sha256",
        "locked_identities_sha256",
        "complete_plan_sha256",
        "v4_implementation_lock_sha256",
        "execution_contract_sha256",
        "execution_attestations_sha256",
    ):
        _sha256(integrity[field], f"power_report.integrity.{field}")
    _require(
        _sha256(
            integrity["product_cycle_schema_file_sha256"],
            "power_report.integrity.product_cycle_schema_file_sha256",
        )
        == bundle.schemas["product_cycle"].sha256,
        "power_report product-cycle schema bytes drift",
    )
    _require(
        _sha256(
            integrity["task_population_schema_file_sha256"],
            "power_report.integrity.task_population_schema_file_sha256",
        )
        == bundle.schemas["task_population"].sha256,
        "power_report task-population schema bytes drift",
    )
    _require(
        integrity["product_cycle_contract_sha256"]
        == bundle.product_cycle.value["manifest_sha256"],
        "power_report product-cycle contract identity drift",
    )
    _require(
        integrity["product_cycle_evidence_canonical_bytes_sha256"]
        == PRODUCT.value_sha256(bundle.product_cycle.value),
        "power_report product-cycle canonical bytes drift",
    )
    _require(
        integrity["task_population_sha256"]
        == bundle.task_population.value["population_sha256"],
        "power_report task-population identity drift",
    )
    _require(
        integrity["task_population_contract_file_sha256"]
        == bundle.task_population.sha256,
        "power_report task-population raw bytes drift",
    )
    _require(integrity["budget_authorized"] is False, "power v4 cannot authorize budget")
    _require(
        integrity["provider_or_model_calls_performed"] is False,
        "power v4 cannot perform provider or model calls",
    )
    digest = _sha256(root["report_sha256"], "power_report.report_sha256")
    _require(digest == _self_sha256(root, "report_sha256"), "power report identity hash mismatch")


def check_power_report(
    calibration: Any, report: Any, bundle: ArtifactBundle
) -> dict[str, Any]:
    """Recompute a report from its bound calibration and require exact equality."""
    validate_power_report(report, bundle)
    expected = analyze_calibration(calibration, bundle)
    _require(
        report == expected,
        "power report does not exactly match deterministic recomputation from calibration",
    )
    return {
        "schema": "agent-brain-power-analysis-check/v1",
        "status": "valid",
        "final_calibration_sha256": expected["final_calibration_sha256"],
        "report_sha256": expected["report_sha256"],
        "budget_authorized": False,
        "provider_or_model_calls_performed": False,
    }


def _write(value: Any) -> None:
    sys.stdout.write(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    def add_artifacts(child: argparse.ArgumentParser) -> None:
        child.add_argument("--product-cycle", type=pathlib.Path, required=True)
        child.add_argument("--task-population", type=pathlib.Path, required=True)
        child.add_argument("--review-ledger", type=pathlib.Path, required=True)
        child.add_argument("--selection-receipt", type=pathlib.Path, required=True)
        child.add_argument("--assignment-receipt", type=pathlib.Path, required=True)
        child.add_argument("--overlap-receipt", type=pathlib.Path, required=True)
        child.add_argument("--candidate-lock-receipt", type=pathlib.Path, required=True)
        child.add_argument("--owner-approval-receipt", type=pathlib.Path)

    for command in ("preflight", "analyze"):
        child = subparsers.add_parser(command)
        child.add_argument("calibration", type=pathlib.Path)
        add_artifacts(child)
    checker = subparsers.add_parser("check")
    checker.add_argument("calibration", type=pathlib.Path)
    checker.add_argument("report", type=pathlib.Path)
    add_artifacts(checker)
    args = parser.parse_args(argv)
    try:
        calibration = load_calibration(args.calibration)
        bundle = load_artifact_bundle(
            product_cycle=args.product_cycle,
            task_population=args.task_population,
            review_ledger=args.review_ledger,
            selection_receipt=args.selection_receipt,
            assignment_receipt=args.assignment_receipt,
            overlap_receipt=args.overlap_receipt,
            candidate_lock_receipt=args.candidate_lock_receipt,
            owner_approval_receipt=args.owner_approval_receipt,
        )
        if args.command == "preflight":
            result = preflight_calibration(calibration, bundle)
        elif args.command == "analyze":
            result = analyze_calibration(calibration, bundle)
        else:
            result = check_power_report(
                calibration, load_power_report(args.report), bundle
            )
    except PowerV4Error as exc:
        sys.stderr.write(f"power-v4 {args.command} failed: {exc}\n")
        return 2
    _write(result)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
