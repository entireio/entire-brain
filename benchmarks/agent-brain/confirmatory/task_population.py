#!/usr/bin/env python3
"""Validate the representative task-population v2 contract.

The checked-in v1 inventory records exposure of legacy tasks.  This module is a
separate, deterministic contract for future development optimization,
development calibration, and commitment-only confirmatory membership.  It has
no runner or model dependency and never needs holdout plaintext.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import hmac
import json
import pathlib
from collections import Counter, defaultdict
from typing import Any

import relevance_dataset


HERE = pathlib.Path(__file__).resolve().parent
SCHEMA_DIR = HERE / "schemas"
POPULATION_SCHEMA = SCHEMA_DIR / "task-population-v2.schema.json"
REVIEW_SCHEMA = HERE.parent / "schemas" / "task-review-ledger-v2.schema.json"
MEMBERSHIPS = (
    "development_optimization",
    "development_calibration",
    "confirmatory_holdout",
)


class TaskPopulationError(ValueError):
    """Raised when a population or review contract is not trustworthy."""


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise TaskPopulationError("JSON contains a duplicate object key")
        result[key] = value
    return result


def load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise TaskPopulationError(f"cannot load JSON contract: {exc}") from exc
    if not isinstance(value, dict):
        raise TaskPopulationError("JSON contract root must be an object")
    return value


def _reject_floats(value: Any) -> None:
    if isinstance(value, float):
        raise TaskPopulationError("canonical task-population JSON forbids floating-point values")
    if isinstance(value, dict):
        for child in value.values():
            _reject_floats(child)
    elif isinstance(value, list):
        for child in value:
            _reject_floats(child)


def canonical_json_bytes(value: Any) -> bytes:
    _reject_floats(value)
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_sha256(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def task_identity_sha256(task_id: str) -> str:
    return hashlib.sha256(
        b"entire-brain/task-population-v2/task-id\0" + task_id.encode("utf-8")
    ).hexdigest()


def overlap_commitment(key: bytes, domain: str, value: bytes) -> str:
    """Return a domain-separated blind commitment for overlap detection.

    The key is owner-held and never belongs in the public population contract.
    A single key must be used across all three splits so equality remains
    detectable without exposing low-entropy task material to dictionary attack.
    """
    if len(key) < 32:
        raise TaskPopulationError("overlap commitment key must contain at least 32 bytes")
    if not domain or any(ord(char) < 0x21 or ord(char) > 0x7E for char in domain):
        raise TaskPopulationError("overlap commitment domain must be visible ASCII")
    return hmac.new(
        key,
        b"entire-brain/task-population-v2/overlap\0" + domain.encode("ascii") + b"\0" + value,
        hashlib.sha256,
    ).hexdigest()


def _self_hash(value: dict[str, Any], field: str) -> str:
    projected = copy.deepcopy(value)
    if field not in projected:
        raise TaskPopulationError(f"self-hashed object is missing {field}")
    projected[field] = None
    return canonical_sha256(projected)


def difficulty_score(value: dict[str, Any]) -> int:
    """Return the frozen bounded-components-v1 structural score."""
    return (
        min(value["touched_file_count"], 4)
        + min(value["language_count"], 3)
        + min(value["validation_target_count"], 3)
        + min(value["critical_invariant_count"], 3)
        + (2 if value["cross_package"] else 0)
        + (2 if value["requires_migration"] else 0)
    )


def difficulty_band(score: int) -> str:
    if score <= 5:
        return "low"
    if score <= 9:
        return "medium"
    return "high"


def _schema_errors(instance: dict[str, Any], schema_path: pathlib.Path, label: str) -> list[str]:
    schema = load_json(schema_path)
    errors = relevance_dataset.validate_schema_instance(instance, schema, label)
    if schema_path == POPULATION_SCHEMA and isinstance(instance.get("members"), list):
        attribute_schema = {"$defs": schema.get("$defs", {}), "$ref": "#/$defs/attributes"}
        for index, member in enumerate(instance["members"]):
            if isinstance(member, dict) and member.get("attributes") is not None:
                errors.extend(
                    relevance_dataset.validate_schema_instance(
                        member["attributes"], attribute_schema, f"{label}.members[{index}].attributes"
                    )
                )
    return errors


def _computed_summary(population: dict[str, Any]) -> dict[str, Any]:
    members = population["members"]
    active = [member for member in members if member["contamination_state"] != "excluded"]
    counts = Counter(member["membership"] for member in active)
    holdout_plaintext_fields = sum(
        int(member["task_id"] is not None) + int(member["attributes"] is not None)
        for member in members
        if member["membership"] == "confirmatory_holdout"
    )
    return {
        "total_members": len(members),
        "active_members": len(active),
        "excluded_members": len(members) - len(active),
        "membership_counts": {membership: counts[membership] for membership in MEMBERSHIPS},
        "unique_prompt_commitments": len(
            {member["artifacts"]["prompt_commitment_sha256"] for member in members}
        ),
        "unique_patch_commitments": len(
            {member["artifacts"]["patch_commitment_sha256"] for member in members}
        ),
        "related_edge_count": len(population["related_task_edges"]),
        "holdout_plaintext_fields": holdout_plaintext_fields,
    }


def seal_review_ledger(ledger: dict[str, Any]) -> dict[str, Any]:
    """Return a copy with deterministic per-review and ledger hashes filled."""
    sealed = copy.deepcopy(ledger)
    for review in sealed["reviews"]:
        review["review_sha256"] = _self_hash(review, "review_sha256")
    sealed["ledger_sha256"] = _self_hash(sealed, "ledger_sha256")
    return sealed


def seal_population(population: dict[str, Any], ledger: dict[str, Any]) -> dict[str, Any]:
    """Return a copy with deterministic derived commitments and summary filled."""
    sealed = copy.deepcopy(population)
    reviews = {review["member_ref"]: review for review in ledger["reviews"]}
    sealed["review_ledger"]["sha256"] = ledger["ledger_sha256"]
    for member in sealed["members"]:
        if member["task_id"] is not None:
            member["task_id_sha256"] = task_identity_sha256(member["task_id"])
        if member["attributes"] is not None:
            member["attributes_sha256"] = canonical_sha256(member["attributes"])
        review = reviews.get(member["member_ref"])
        if review is not None:
            member["review_commitment_sha256"] = review["review_sha256"]
    sealed["summary"] = _computed_summary(sealed)
    sealed["population_sha256"] = _self_hash(sealed, "population_sha256")
    return sealed


def _active_cross_membership(values: list[dict[str, Any]]) -> bool:
    active = [value for value in values if value["contamination_state"] != "excluded"]
    return len({value["membership"] for value in active}) > 1


def _placeholder_sha_paths(value: Any, path: str = "$") -> list[str]:
    paths: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            child_path = f"{path}.{key}"
            if key.endswith("_sha256") and child == "0" * 64:
                paths.append(child_path)
            paths.extend(_placeholder_sha_paths(child, child_path))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            paths.extend(_placeholder_sha_paths(child, f"{path}[{index}]"))
    return paths


def _check_verification_receipt(
    errors: list[str], *, label: str, status: str, receipt: str | None
) -> None:
    if status == "pending" and receipt is not None:
        errors.append(f"pending {label} cannot carry a verification receipt")
    if status == "verified" and receipt in {None, "0" * 64}:
        errors.append(f"verified {label} requires a non-placeholder receipt")


def validate_population(
    population: dict[str, Any],
    review_ledger: dict[str, Any],
    *,
    population_schema: pathlib.Path = POPULATION_SCHEMA,
    review_schema: pathlib.Path = REVIEW_SCHEMA,
) -> dict[str, Any]:
    """Validate schemas, commitments, split isolation, and review coverage."""
    errors = _schema_errors(population, population_schema, "population")
    errors.extend(_schema_errors(review_ledger, review_schema, "review_ledger"))
    if errors:
        raise TaskPopulationError("\n".join(errors))

    if population["population_sha256"] != _self_hash(population, "population_sha256"):
        errors.append("population self hash does not match canonical content")
    if review_ledger["ledger_sha256"] != _self_hash(review_ledger, "ledger_sha256"):
        errors.append("review ledger self hash does not match canonical content")
    if population["review_ledger"]["sha256"] != review_ledger["ledger_sha256"]:
        errors.append("population does not bind the supplied review ledger")
    selection = population["selection_policy"]
    if selection["eligibility_ledger_sha256"] == "0" * 64:
        errors.append("eligibility ledger receipt cannot be a placeholder")
    selection_mode = selection["selection_mode"]
    selection_seed = selection["selection_seed_commitment_sha256"]
    if selection_mode == "all_eligible":
        if selection_seed is not None:
            errors.append("all-eligible selection cannot carry a sample seed")
        if selection["eligible_task_count"] != len(population["members"]):
            errors.append("all-eligible selection must account for every eligible task")
    else:
        if selection_seed in {None, "0" * 64}:
            errors.append("seeded selection requires a non-placeholder seed commitment")
        if selection["eligible_task_count"] < len(population["members"]):
            errors.append("seeded selection cannot select more tasks than the eligibility ledger")
    _check_verification_receipt(
        errors,
        label="task selection",
        status=selection["selection_verification_status"],
        receipt=selection["selection_receipt_sha256"],
    )

    split_policy = population["split_policy"]
    if split_policy["assignment_key_sha256"] == "0" * 64:
        errors.append("split-assignment key receipt cannot be a placeholder")
    _check_verification_receipt(
        errors,
        label="split assignment",
        status=split_policy["assignment_verification_status"],
        receipt=split_policy["assignment_receipt_sha256"],
    )
    if split_policy["overlap_commitment_key_sha256"] == "0" * 64:
        errors.append("overlap commitment key receipt cannot be a placeholder")
    commitment_status = split_policy["overlap_commitment_verification_status"]
    commitment_receipt = split_policy["overlap_commitment_receipt_sha256"]
    _check_verification_receipt(
        errors,
        label="overlap commitments",
        status=commitment_status,
        receipt=commitment_receipt,
    )

    reviews: dict[str, dict[str, Any]] = {}
    for index, review in enumerate(review_ledger["reviews"]):
        ref = review["member_ref"]
        if ref in reviews:
            errors.append("review ledger contains a duplicate member_ref")
        reviews[ref] = review
        if review["review_sha256"] != _self_hash(review, "review_sha256"):
            errors.append(f"review[{index}] self hash does not match canonical content")

    members: dict[str, dict[str, Any]] = {}
    task_ids: set[str] = set()
    task_identities: set[str] = set()
    prompts: dict[str, list[dict[str, Any]]] = defaultdict(list)
    patches: dict[str, list[dict[str, Any]]] = defaultdict(list)
    fixes: dict[str, list[dict[str, Any]]] = defaultdict(list)
    sessions: dict[str, list[dict[str, Any]]] = defaultdict(list)
    related_families: dict[str, list[dict[str, Any]]] = defaultdict(list)

    for index, member in enumerate(population["members"]):
        ref = member["member_ref"]
        if ref in members:
            errors.append("population contains a duplicate member_ref")
        members[ref] = member
        task_id = member["task_id"]
        task_identity = member["task_overlap_commitment_sha256"]
        if task_identity in task_identities:
            errors.append("population contains a duplicate task-identity commitment")
        task_identities.add(task_identity)
        if task_id is not None:
            if task_id in task_ids:
                errors.append("population contains a duplicate development task_id")
            task_ids.add(task_id)
            if member["task_id_sha256"] != task_identity_sha256(task_id):
                errors.append(f"member[{index}] public task hash is not bound to its development task_id")

        holdout = member["membership"] == "confirmatory_holdout"
        attributes = member["attributes"]
        if holdout:
            if member["task_id"] is not None or member["task_id_sha256"] is not None or attributes is not None:
                errors.append(f"member[{index}] confirmatory_holdout must remain commitment-only")
            if member["contamination_state"] != "commitment_only":
                errors.append(f"member[{index}] confirmatory_holdout has a non-commitment state")
        else:
            if member["task_id"] is None or member["task_id_sha256"] is None or attributes is None:
                errors.append(f"member[{index}] development member is missing reviewed attributes")
            if member["contamination_state"] == "commitment_only":
                errors.append(f"member[{index}] development member has a holdout-only state")
        if attributes is not None:
            if member["attributes_sha256"] != canonical_sha256(attributes):
                errors.append(f"member[{index}] attributes hash does not match canonical content")
            difficulty = attributes["structural_difficulty"]
            score = difficulty_score(difficulty)
            if difficulty["score"] != score:
                errors.append(f"member[{index}] structural difficulty score is not derived")
            if difficulty["band"] != difficulty_band(score):
                errors.append(f"member[{index}] structural difficulty band is not derived")

        review = reviews.get(ref)
        if review is None:
            errors.append(f"member[{index}] has no task-validity review")
        else:
            if review["task_id"] != member["task_id"]:
                errors.append(f"member[{index}] review task identity does not match")
            if review["prompt_commitment_sha256"] != member["artifacts"]["prompt_commitment_sha256"]:
                errors.append(f"member[{index}] review prompt commitment does not match")
            if member["review_commitment_sha256"] != review["review_sha256"]:
                errors.append(f"member[{index}] review commitment does not match")
            excluded = member["contamination_state"] == "excluded"
            approved = review["disposition"] == "approved_symptom_only"
            if excluded and approved:
                errors.append(f"member[{index}] is excluded but its review is approved")
            if not excluded:
                if not approved:
                    errors.append(f"member[{index}] is active without approved symptom-only review")
                if not all(review["checks"].values()):
                    errors.append(f"member[{index}] has an incomplete task-validity checklist")
                if review["rationale_code"] != "all_checks_pass":
                    errors.append(f"member[{index}] approved review has a non-pass rationale code")

        prompts[member["artifacts"]["prompt_commitment_sha256"]].append(member)
        patches[member["artifacts"]["patch_commitment_sha256"]].append(member)
        fixes[member["artifacts"]["fix_commitment_sha256"]].append(member)
        related_families[member["family_commitment_sha256"]].append(member)
        for session in member["source_session_commitments"]:
            sessions[session].append(member)

    if set(reviews) != set(members):
        errors.append("review ledger membership does not exactly match the population")
    if any(len(values) > 1 for values in prompts.values()):
        errors.append("prompt commitments must be globally unique")
    if any(len(values) > 1 for values in patches.values()):
        errors.append("patch commitments must be globally unique")
    if any(_active_cross_membership(values) for values in fixes.values()):
        errors.append("active splits share a fix-commit commitment")
    if any(_active_cross_membership(values) for values in sessions.values()):
        errors.append("active splits share a source-session commitment")
    if any(_active_cross_membership(values) for values in related_families.values()):
        errors.append("active splits share a materially related task-family commitment")

    seen_edges: set[tuple[str, str, str]] = set()
    for index, edge in enumerate(population["related_task_edges"]):
        left_ref = edge["left_member_ref"]
        right_ref = edge["right_member_ref"]
        if left_ref == right_ref:
            errors.append(f"related_task_edges[{index}] is a self-edge")
            continue
        if left_ref not in members or right_ref not in members:
            errors.append(f"related_task_edges[{index}] references an unknown member")
            continue
        pair = tuple(sorted((left_ref, right_ref))) + (edge["relation_kind"],)
        if pair in seen_edges:
            errors.append("related task edge is duplicated")
        seen_edges.add(pair)
        left = members[left_ref]
        right = members[right_ref]
        if edge["material"] and left["membership"] != right["membership"]:
            allowed_exclusion = (
                edge["disposition"] == "exclude_one"
                and "excluded" in {left["contamination_state"], right["contamination_state"]}
            )
            if not allowed_exclusion:
                errors.append(f"related_task_edges[{index}] materially crosses population splits")

    expected_summary = _computed_summary(population)
    if population["summary"] != expected_summary:
        errors.append("population summary does not match member content")

    counts = expected_summary["membership_counts"]
    if population["status"] in {"candidate_unopened", "frozen_unopened"}:
        if selection["selection_verification_status"] != "verified":
            errors.append("candidate population requires verified treatment-blind task selection")
        if split_policy["assignment_verification_status"] != "verified":
            errors.append("candidate population requires verified deterministic split assignment")
        if commitment_status != "verified":
            errors.append("candidate population requires verified blind overlap commitments")
        if counts["development_optimization"] < 1:
            errors.append("candidate population has no development-optimization members")
        if counts["development_calibration"] < split_policy["calibration_min_independent_tasks"]:
            errors.append("candidate population has fewer than 12 independent calibration tasks")
        if counts["confirmatory_holdout"] < 1:
            errors.append("candidate population has no commitment-only confirmatory members")
        placeholders = _placeholder_sha_paths(population) + _placeholder_sha_paths(review_ledger)
        if placeholders:
            errors.append("candidate population contains placeholder SHA-256 fields: " + ", ".join(placeholders))

    if errors:
        raise TaskPopulationError("\n".join(sorted(set(errors))))
    return expected_summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("population", type=pathlib.Path)
    parser.add_argument("review_ledger", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        summary = validate_population(load_json(args.population), load_json(args.review_ledger))
    except TaskPopulationError as exc:
        parser.error(str(exc))
    print(json.dumps({"ok": True, "summary": summary}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
