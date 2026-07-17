#!/usr/bin/env python3
"""Pure, fail-closed gate primitives for the pending negative-control plan.

This module can describe and verify an external offline Go cache seed and can
evaluate injected filesystem capacity.  It deliberately has no Git, Go,
network, worktree, candidate, model, or benchmark execution surface.  A
successful primitive receipt remains non-authoritative and cannot change the
pending run plan's execution boundary.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import pathlib
import unicodedata
from collections import Counter
from typing import Any, Mapping, Sequence, cast

import draft202012
import task_negative_control_plan_v2 as run_plan_v2


CACHE_MANIFEST_PROFILE = "content_addressed_offline_go_cache_seed_manifest_v1"
PREFLIGHT_RECEIPT_PROFILE = "agent_brain_negative_control_gate_primitive_receipt_v1"
SCHEMA_VERSION = 1
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
ARTIFACT_RENDER_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
MANIFEST_STATUS = "external_input_identity_only_execution_forbidden"
RECEIPT_STATUS = "primitive_checks_passed_execution_forbidden"
FILESYSTEM_OBSERVATION_KIND = "injected_filesystem_stats_unattested"
CACHE_ARCHIVE_OBSERVATION_KIND = "injected_archive_identity_unattested"
CACHE_BINDING_STATUS = "external_manifest_not_bound_by_pending_plan"
CACHE_ROOTS = ("gocache", "gomodcache")
REPOSITORY_KEYS = tuple(run_plan_v2.REPOSITORY_ORDER)
CHECKS = [
    "pending_authority_boundary_verified",
    "cache_manifest_identity_verified",
    "injected_cache_archive_identity_observation_matches_manifest",
    "resource_budget_arithmetic_verified",
    "injected_free_space_threshold_verified",
]
RESIDUAL_GATES = [
    "owner_execution_approval_receipt_and_trust_mechanism",
    "authorized_plan_binding_for_actual_cache_seed",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_filesystem_observer_and_atomic_resource_reservation",
    "negative_control_executor_and_reversal_proof",
    "classification_truth_table_and_receipt_checker",
    "private_content_addressed_log_writer_and_checker",
    "fail_closed_cleanup_and_no_receipt_on_interruption",
]


class GatePrimitiveError(ValueError):
    """Raised when a manifest, observation, or primitive receipt differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise GatePrimitiveError(message)


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _reject_floats(value: Any) -> None:
    if isinstance(value, float):
        raise GatePrimitiveError("canonical gate JSON forbids floating-point values")
    if isinstance(value, dict):
        for child in value.values():
            _reject_floats(child)
    elif isinstance(value, list):
        for child in value:
            _reject_floats(child)


def _canonical_json_bytes(value: Any) -> bytes:
    _reject_floats(value)
    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise GatePrimitiveError(f"value is not canonical JSON: {exc}") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_json_bytes(value))


def _render(value: dict[str, Any]) -> bytes:
    _reject_floats(value)
    try:
        return (
            json.dumps(
                value,
                allow_nan=False,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise GatePrimitiveError(f"value is not renderable canonical JSON: {exc}") from exc


def _self_hash(value: dict[str, Any], field: str) -> str:
    projected = copy.deepcopy(value)
    _require(field in projected, f"{field} is missing")
    projected[field] = None
    return _canonical_hash(projected)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise GatePrimitiveError(f"cannot load JSON: {exc}") from exc
    _require(isinstance(value, dict), "JSON root must be an object")
    return value, raw


def _valid_sha(value: Any) -> bool:
    return (
        isinstance(value, str)
        and run_plan_v2.eligibility.SHA256_RE.fullmatch(value) is not None
        and value != "0" * 64
    )


def _integer(value: Any, field: str, *, minimum: int = 0) -> int:
    _require(type(value) is int and value >= minimum, f"{field} is invalid")
    return cast(int, value)


def _artifact_file(value: Any, field: str) -> str:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    value = cast(str, value)
    _require(
        pathlib.PurePosixPath(value).name == value
        and value not in {".", ".."}
        and "\\" not in value
        and "\x00" not in value
        and not any(unicodedata.category(character).startswith("C") for character in value),
        f"{field} is not a canonical file name",
    )
    _require(
        len(value.encode("utf-8")) <= 255
        and unicodedata.normalize("NFC", value) == value,
        f"{field} encoding is not canonical",
    )
    return value


def _cache_path(value: Any, field: str) -> str:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    value = cast(str, value)
    pure = pathlib.PurePosixPath(value)
    _require(
        not pure.is_absolute()
        and str(pure) == value
        and "\\" not in value
        and "\x00" not in value
        and all(part not in {"", ".", ".."} for part in pure.parts),
        f"{field} is not a canonical relative path",
    )
    _require(pure.parts[0] in CACHE_ROOTS, f"{field} is outside the isolated cache roots")
    _require(
        len(value.encode("utf-8")) <= 1024
        and unicodedata.normalize("NFC", value) == value,
        f"{field} encoding is not canonical",
    )
    _require(
        all(len(part.encode("utf-8")) <= 255 for part in pure.parts)
        and not any(
            unicodedata.category(character).startswith("C") for character in value
        ),
        f"{field} component encoding is not canonical",
    )
    return value


def _pending_authority(plan: dict[str, Any]) -> dict[str, Any]:
    try:
        run_plan_v2.validate_plan(plan)
    except run_plan_v2.RunPlanError as exc:
        raise GatePrimitiveError(f"run plan is invalid: {exc}") from exc
    _require(plan["status"] == "pending_owner_authorization", "run plan is not pending")
    authority = plan["authority"]
    _require(
        authority["candidate_execution"] == "forbidden_plan_unexecutable"
        and authority["benchmark_execution"] == "forbidden_pending_separate_owner_approval"
        and authority["model_provider_execution"] == "forbidden_not_authorized"
        and authority["paid_execution"] == "forbidden_not_authorized",
        "run plan execution authority differs",
    )
    cache_seed = plan["cache_seed"]
    _require(
        cache_seed["manifest_sha256"] is None
        and cache_seed["archive_sha256"] is None
        and cache_seed["source_locator"] is None
        and cache_seed["unpacked_byte_count"] is None,
        "pending plan unexpectedly binds a cache seed",
    )
    return {
        "benchmark_execution": authority["benchmark_execution"],
        "candidate_execution": authority["candidate_execution"],
        "model_provider_execution": authority["model_provider_execution"],
        "paid_execution": authority["paid_execution"],
        "plan_status": plan["status"],
    }


def _toolchain_projection(plan: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        {
            "go": copy.deepcopy(repository["toolchain"]["go"]),
            "repository_key": repository["key"],
        }
        for repository in plan["repositories"]
    ]


def _manifest_implementation(schema_path: pathlib.Path) -> dict[str, Any]:
    try:
        builder_hash = _sha256(pathlib.Path(__file__).read_bytes())
        plan_builder_hash = _sha256(pathlib.Path(run_plan_v2.__file__).read_bytes())
        schema_validator_hash = _sha256(pathlib.Path(draft202012.__file__).read_bytes())
        schema_hash = _sha256(schema_path.read_bytes())
    except OSError as exc:
        raise GatePrimitiveError(f"cannot read manifest implementation dependency: {exc}") from exc
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "builder_sha256": builder_hash,
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "manifest_schema_sha256": schema_hash,
        "run_plan_builder_sha256": plan_builder_hash,
        "schema_validator_sha256": schema_validator_hash,
    }


def _receipt_implementation(
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
) -> dict[str, Any]:
    implementation = _manifest_implementation(manifest_schema_path)
    try:
        receipt_schema_hash = _sha256(receipt_schema_path.read_bytes())
    except OSError as exc:
        raise GatePrimitiveError(f"cannot read receipt schema: {exc}") from exc
    return {**implementation, "receipt_schema_sha256": receipt_schema_hash}


def _validate_against_schema(
    value: dict[str, Any],
    schema_path: pathlib.Path,
    label: str,
) -> None:
    schema, _ = _load_json(schema_path)
    try:
        validator = draft202012.Validator(
            [draft202012.SchemaDocument(schema_path.name, schema)]
        )
        validator.validate(value, schema_path.name, label=label)
    except draft202012.SchemaError as exc:
        raise GatePrimitiveError(f"{label} schema validation failed: {exc}") from exc


def build_cache_seed_manifest(
    *,
    plan: dict[str, Any],
    manifest_schema_path: pathlib.Path,
    archive_file: str,
    archive_sha256: str,
    archive_byte_count: int,
    entries: Sequence[Mapping[str, Any]],
) -> dict[str, Any]:
    authority = _pending_authority(plan)
    _require(
        all(isinstance(entry, Mapping) for entry in entries),
        "manifest entries must be objects",
    )
    normalized_entries = [copy.deepcopy(dict(entry)) for entry in entries]
    counts = Counter(
        key if isinstance(key, str) else None
        for entry in normalized_entries
        for key in (entry.get("repository_key"),)
    )
    contents = {
        "entries": normalized_entries,
        "file_count": len(normalized_entries),
        "inventory_sha256": _canonical_hash(normalized_entries),
        "repository_file_counts": {key: counts[key] for key in REPOSITORY_KEYS},
        "unpacked_byte_count": sum(
            entry.get("byte_count", 0)
            for entry in normalized_entries
            if type(entry.get("byte_count")) is int
        ),
    }
    manifest = {
        "archive": {
            "artifact_file": archive_file,
            "byte_count": archive_byte_count,
            "sha256": archive_sha256,
        },
        "authority": authority,
        "contents": contents,
        "host_shared_cache_reuse": "forbidden",
        "implementation": _manifest_implementation(manifest_schema_path),
        "manifest_sha256": None,
        "network_dependency_resolution": "forbidden",
        "profile": CACHE_MANIFEST_PROFILE,
        "run_plan": {
            "plan_sha256": plan["plan_sha256"],
            "profile": plan["profile"],
            "schema_version": plan["schema_version"],
            "toolchain_bindings_sha256": _canonical_hash(_toolchain_projection(plan)),
        },
        "schema_version": SCHEMA_VERSION,
        "status": MANIFEST_STATUS,
    }
    manifest["manifest_sha256"] = _self_hash(manifest, "manifest_sha256")
    validate_cache_seed_manifest(
        manifest,
        plan=plan,
        manifest_schema_path=manifest_schema_path,
    )
    return manifest


def validate_cache_seed_manifest(
    value: dict[str, Any],
    *,
    plan: dict[str, Any],
    manifest_schema_path: pathlib.Path,
) -> None:
    authority = _pending_authority(plan)
    expected_root = {
        "archive", "authority", "contents", "host_shared_cache_reuse", "implementation",
        "manifest_sha256", "network_dependency_resolution", "profile", "run_plan",
        "schema_version", "status",
    }
    _require(isinstance(value, dict) and set(value) == expected_root, "manifest root fields differ")
    _require(value["profile"] == CACHE_MANIFEST_PROFILE and value["schema_version"] == 1, "manifest profile differs")
    _require(value["status"] == MANIFEST_STATUS, "manifest status differs")
    _require(value["authority"] == authority, "manifest authority boundary differs")
    _require(value["host_shared_cache_reuse"] == "forbidden", "host cache reuse is not forbidden")
    _require(value["network_dependency_resolution"] == "forbidden", "network resolution is not forbidden")
    _require(_valid_sha(value["manifest_sha256"]), "manifest_sha256 is invalid")
    _require(value["manifest_sha256"] == _self_hash(value, "manifest_sha256"), "manifest self hash mismatch")

    _require(value["implementation"] == _manifest_implementation(manifest_schema_path), "manifest implementation binding differs")
    expected_run_plan = {
        "plan_sha256": plan["plan_sha256"],
        "profile": plan["profile"],
        "schema_version": plan["schema_version"],
        "toolchain_bindings_sha256": _canonical_hash(_toolchain_projection(plan)),
    }
    _require(value["run_plan"] == expected_run_plan, "manifest run-plan binding differs")

    archive = value["archive"]
    _require(isinstance(archive, dict) and set(archive) == {"artifact_file", "byte_count", "sha256"}, "manifest archive fields differ")
    _artifact_file(archive["artifact_file"], "archive.artifact_file")
    _require(_valid_sha(archive["sha256"]), "archive.sha256 is invalid")
    archive_bytes = _integer(archive["byte_count"], "archive.byte_count", minimum=1)

    contents = value["contents"]
    _require(
        isinstance(contents, dict)
        and set(contents)
        == {"entries", "file_count", "inventory_sha256", "repository_file_counts", "unpacked_byte_count"},
        "manifest contents fields differ",
    )
    entries = contents["entries"]
    _require(isinstance(entries, list) and bool(entries), "manifest entries must be a non-empty list")
    expected_entry_fields = {"byte_count", "path", "repository_key", "sha256"}
    identities: list[tuple[str, str]] = []
    archive_paths: list[str] = []
    unpacked_bytes = 0
    repository_counts: Counter[str] = Counter()
    for index, entry in enumerate(entries):
        _require(isinstance(entry, dict) and set(entry) == expected_entry_fields, f"entry[{index}] fields differ")
        key = entry["repository_key"]
        _require(key in REPOSITORY_KEYS, f"entry[{index}].repository_key is invalid")
        path = _cache_path(entry["path"], f"entry[{index}].path")
        _require(_valid_sha(entry["sha256"]), f"entry[{index}].sha256 is invalid")
        unpacked_bytes += _integer(entry["byte_count"], f"entry[{index}].byte_count")
        identities.append((key, path))
        archive_paths.append(path)
        repository_counts[key] += 1
    _require(identities == sorted(identities), "manifest entries are not canonical")
    _require(len(archive_paths) == len(set(archive_paths)), "manifest archive paths collide globally")
    _require(all(repository_counts[key] > 0 for key in REPOSITORY_KEYS), "manifest does not cover every repository cache")
    expected_counts = {key: repository_counts[key] for key in REPOSITORY_KEYS}
    _require(contents["repository_file_counts"] == expected_counts, "manifest repository counts differ")
    _require(contents["file_count"] == len(entries), "manifest file count differs")
    _require(contents["unpacked_byte_count"] == unpacked_bytes, "manifest unpacked byte count differs")
    _require(contents["inventory_sha256"] == _canonical_hash(entries), "manifest inventory hash differs")

    maximum_seed_bytes = _integer(plan["resource_budget"]["max_cache_seed_bytes"], "max_cache_seed_bytes", minimum=1)
    _require(archive_bytes <= maximum_seed_bytes, "cache archive exceeds the frozen byte ceiling")
    _require(unpacked_bytes <= maximum_seed_bytes, "unpacked cache seed exceeds the frozen byte ceiling")
    _validate_against_schema(value, manifest_schema_path, "cache manifest")


def _validate_archive_observation(
    observation: Mapping[str, Any],
    manifest: dict[str, Any],
) -> dict[str, Any]:
    expected_fields = {
        "archive_sha256", "archive_byte_count", "content_inventory_sha256",
        "file_count", "observation_kind", "unpacked_byte_count",
    }
    _require(
        isinstance(observation, Mapping) and set(observation) == expected_fields,
        "cache archive observation fields differ",
    )
    archive = manifest["archive"]
    contents = manifest["contents"]
    expected = {
        "archive_byte_count": archive["byte_count"],
        "archive_sha256": archive["sha256"],
        "content_inventory_sha256": contents["inventory_sha256"],
        "file_count": contents["file_count"],
        "observation_kind": CACHE_ARCHIVE_OBSERVATION_KIND,
        "unpacked_byte_count": contents["unpacked_byte_count"],
    }
    observed = dict(observation)
    _require(observed == expected, "cache archive observation differs from manifest")
    return expected


def evaluate_injected_resource_stats(
    plan: dict[str, Any],
    filesystem_stats: Mapping[str, Any],
) -> dict[str, Any]:
    _pending_authority(plan)
    _require(
        isinstance(filesystem_stats, Mapping)
        and set(filesystem_stats) == {"available_blocks", "fragment_size_bytes"},
        "filesystem stats fields differ",
    )
    fragment_size = _integer(filesystem_stats["fragment_size_bytes"], "fragment_size_bytes", minimum=1)
    available_blocks = _integer(filesystem_stats["available_blocks"], "available_blocks")
    free_bytes = fragment_size * available_blocks
    budget = plan["resource_budget"]
    reserve = _integer(budget["minimum_free_disk_reserve_bytes"], "minimum_free_disk_reserve_bytes", minimum=1)
    staging = _integer(budget["max_total_staging_bytes"], "max_total_staging_bytes", minimum=1)
    required = _integer(budget["minimum_free_disk_before_staging_bytes"], "minimum_free_disk_before_staging_bytes", minimum=1)
    _require(required == reserve + staging, "frozen free-space arithmetic differs")
    _require(free_bytes >= required, "free space is below the frozen 16 GiB preflight threshold")
    _require(free_bytes - staging >= reserve, "staging would consume the frozen free-space reserve")
    return {
        "available_blocks": available_blocks,
        "free_bytes": free_bytes,
        "fragment_size_bytes": fragment_size,
        "headroom_bytes": free_bytes - required,
        "minimum_required_bytes": required,
        "observation_kind": FILESYSTEM_OBSERVATION_KIND,
        "reserve_bytes": reserve,
        "staging_ceiling_bytes": staging,
    }


def build_preflight_receipt(
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    manifest: dict[str, Any],
    manifest_raw: bytes,
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    archive_observation: Mapping[str, Any],
    filesystem_stats: Mapping[str, Any],
) -> dict[str, Any]:
    authority = _pending_authority(plan)
    _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
    validate_cache_seed_manifest(manifest, plan=plan, manifest_schema_path=manifest_schema_path)
    _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
    cache = _validate_archive_observation(archive_observation, manifest)
    resource = evaluate_injected_resource_stats(plan, filesystem_stats)
    receipt = {
        "authority": authority,
        "cache_seed": {
            **cache,
            "binding_status": CACHE_BINDING_STATUS,
            "manifest_sha256": manifest["manifest_sha256"],
        },
        "checks": list(CHECKS),
        "execution_status": "forbidden_missing_remaining_gates",
        "implementation": _receipt_implementation(manifest_schema_path, receipt_schema_path),
        "inputs": {
            "manifest_file_sha256": _sha256(manifest_raw),
            "plan_file_sha256": _sha256(plan_raw),
            "plan_sha256": plan["plan_sha256"],
        },
        "profile": PREFLIGHT_RECEIPT_PROFILE,
        "receipt_sha256": None,
        "residual_gates": list(RESIDUAL_GATES),
        "resource": resource,
        "schema_version": SCHEMA_VERSION,
        "status": RECEIPT_STATUS,
    }
    receipt["receipt_sha256"] = _self_hash(receipt, "receipt_sha256")
    validate_preflight_receipt(
        receipt,
        plan=plan,
        plan_raw=plan_raw,
        manifest=manifest,
        manifest_raw=manifest_raw,
        manifest_schema_path=manifest_schema_path,
        receipt_schema_path=receipt_schema_path,
    )
    return receipt


def validate_preflight_receipt(
    value: dict[str, Any],
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    manifest: dict[str, Any],
    manifest_raw: bytes,
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
) -> None:
    authority = _pending_authority(plan)
    _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
    validate_cache_seed_manifest(manifest, plan=plan, manifest_schema_path=manifest_schema_path)
    _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
    expected_root = {
        "authority", "cache_seed", "checks", "execution_status", "implementation", "inputs",
        "profile", "receipt_sha256", "residual_gates", "resource", "schema_version", "status",
    }
    _require(isinstance(value, dict) and set(value) == expected_root, "preflight receipt root fields differ")
    _require(value["profile"] == PREFLIGHT_RECEIPT_PROFILE and value["schema_version"] == 1, "preflight receipt profile differs")
    _require(value["status"] == RECEIPT_STATUS, "preflight receipt status differs")
    _require(value["authority"] == authority, "preflight receipt authority differs")
    _require(value["checks"] == CHECKS, "preflight receipt checks differ")
    _require(value["execution_status"] == "forbidden_missing_remaining_gates", "preflight receipt overclaims execution authority")
    _require(value["residual_gates"] == RESIDUAL_GATES, "preflight receipt residual gates differ")
    _require(value["implementation"] == _receipt_implementation(manifest_schema_path, receipt_schema_path), "preflight receipt implementation differs")
    _require(value["inputs"] == {
        "manifest_file_sha256": _sha256(manifest_raw),
        "plan_file_sha256": _sha256(plan_raw),
        "plan_sha256": plan["plan_sha256"],
    }, "preflight receipt input bindings differ")
    cache_seed = value["cache_seed"]
    _require(isinstance(cache_seed, dict), "preflight receipt cache binding is invalid")
    expected_cache = {
        **_validate_archive_observation(
            {
                "archive_byte_count": cache_seed.get("archive_byte_count"),
                "archive_sha256": cache_seed.get("archive_sha256"),
                "content_inventory_sha256": cache_seed.get("content_inventory_sha256"),
                "file_count": cache_seed.get("file_count"),
                "observation_kind": cache_seed.get("observation_kind"),
                "unpacked_byte_count": cache_seed.get("unpacked_byte_count"),
            },
            manifest,
        ),
        "binding_status": CACHE_BINDING_STATUS,
        "manifest_sha256": manifest["manifest_sha256"],
    }
    _require(value["cache_seed"] == expected_cache, "preflight receipt cache binding differs")
    resource = value["resource"]
    _require(isinstance(resource, dict), "preflight receipt resource is invalid")
    observed_resource = evaluate_injected_resource_stats(
        plan,
        {
            "available_blocks": resource.get("available_blocks"),
            "fragment_size_bytes": resource.get("fragment_size_bytes"),
        },
    )
    _require(resource == observed_resource, "preflight receipt resource arithmetic differs")
    _require(_valid_sha(value["receipt_sha256"]), "receipt_sha256 is invalid")
    _require(value["receipt_sha256"] == _self_hash(value, "receipt_sha256"), "preflight receipt self hash mismatch")
    _validate_against_schema(value, receipt_schema_path, "preflight receipt")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    manifest_check = subparsers.add_parser(
        "check-manifest",
        help="check canonical cache-manifest bytes and exact dependencies",
    )
    manifest_check.add_argument("manifest", type=pathlib.Path)
    manifest_check.add_argument("--plan", required=True, type=pathlib.Path)
    manifest_check.add_argument("--manifest-schema", required=True, type=pathlib.Path)
    receipt_check = subparsers.add_parser(
        "check-receipt",
        help="check canonical primitive-receipt bytes and exact inputs",
    )
    receipt_check.add_argument("receipt", type=pathlib.Path)
    receipt_check.add_argument("--plan", required=True, type=pathlib.Path)
    receipt_check.add_argument("--manifest", required=True, type=pathlib.Path)
    receipt_check.add_argument("--manifest-schema", required=True, type=pathlib.Path)
    receipt_check.add_argument("--receipt-schema", required=True, type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        plan, plan_raw = _load_json(args.plan)
        _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
        manifest, manifest_raw = _load_json(args.manifest)
        _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
        if args.command == "check-manifest":
            validate_cache_seed_manifest(
                manifest,
                plan=plan,
                manifest_schema_path=args.manifest_schema,
            )
        else:
            receipt, receipt_raw = _load_json(args.receipt)
            _require(receipt_raw == _render(receipt), "preflight receipt bytes are not canonical")
            validate_preflight_receipt(
                receipt,
                plan=plan,
                plan_raw=plan_raw,
                manifest=manifest,
                manifest_raw=manifest_raw,
                manifest_schema_path=args.manifest_schema,
                receipt_schema_path=args.receipt_schema,
            )
    except (GatePrimitiveError, run_plan_v2.RunPlanError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
