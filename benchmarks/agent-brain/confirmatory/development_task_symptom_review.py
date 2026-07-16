#!/usr/bin/env python3
"""Build and validate the development-only task symptom-review ledger.

This workflow reviews only candidates that passed the checked-in reverse-patch
negative control.  It records exact development prompts and answer-leakage
decisions, but it cannot assign benchmark population membership: owner-held
HMAC receipts and complete source-session commitments are deliberately absent.

No command in this module runs a model, a benchmark, a candidate test, or a
network request.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import os
import pathlib
import platform
import re
import subprocess
import sys
import tempfile
from collections import Counter, defaultdict
from typing import Any, Sequence

import task_eligibility
import task_negative_control
import task_population


PROFILE = "agent_brain_development_task_symptom_review_v1"
STATUS = "development_review_only_non_authoritative"
EXPOSURE = "development_only_identity_content_and_answer_inspected"
SELECTION_RULE = "exact_negative_control_eligible_for_symptom_review_v1"
POLICY_NAME = "symptom_only_no_answer_bearing_context_v1"
PROMPT_HASH_DOMAIN = b"agent-brain-development-symptom-prompt-v1\0"
REVIEW_HASH_DOMAIN = b"agent-brain-development-symptom-review-record-v1\0"
FIX_HASH_DOMAIN = b"agent-brain-development-fix-identity-v1\0"
FAMILY_HASH_DOMAIN = b"agent-brain-development-family-v1\0"
SHA256_RE = re.compile(r"[0-9a-f]{64}")
OID_RE = re.compile(r"[0-9a-f]{40}")
FAMILY_RE = re.compile(r"[a-z][a-z0-9_]{2,63}")
REVIEWER_RE = re.compile(r"/?[a-z0-9][a-z0-9_.:/-]{2,127}")
CHECKPOINT_RE = re.compile(r"^[0-9A-HJKMNP-TV-Z]{26}$")
TRAILER_RE = re.compile(r"^Entire-Checkpoint:[ \t]*(\S+)[ \t]*$", re.MULTILINE)

REQUIRED_CHECKS = (
    "symptom_only",
    "no_fix_terms",
    "no_file_hints",
    "no_function_hints",
    "no_command_or_workflow_hints",
    "no_hidden_test_details",
    "no_patch_leakage",
    "no_answer_bearing_context",
    "scope_matches_bound_change",
)

# These are high-signal implementation terms found during development review.
# They are not a substitute for semantic review; the independent reviewer must
# inspect every prompt against the bound source patch and tests.
BANNED_PROMPT_TERMS = (
    "batchmode",
    "checkpoint_remote",
    "git_ssh_command",
    "io.readall",
    "json.decoder",
    "skipfetchall",
    "issubpath",
    "worktreeid",
    "goroutine",
    "buffered channel",
    "detached process",
    "case-fold",
    "build tag",
    "environment variable",
    "full transcript scan",
)

AUTHORITY = {
    "owner_hmac_receipts": "absent_not_fabricated",
    "population_membership": "not_assigned",
    "calibration_membership": "not_assigned",
    "holdout_membership": "not_opened_or_assigned",
    "benchmark_run_authorization": "forbidden",
}

SOURCE_SESSION_REASON = (
    "Commit trailers are development evidence only and do not establish complete source-session identity; "
    "the owner-held source-session receipt is absent."
)
FIX_DISTINCT_REASON = (
    "The source-branch commit set is pairwise disjoint from every other reviewed development candidate."
)
PROMPT_DISTINCT_REASON = (
    "The domain-separated exact-prompt commitment is unique across the reviewed development candidates."
)
PATCH_DISTINCT_REASON = (
    "Both eligibility source-diff and test-diff commitments are unique across the reviewed development candidates."
)
AUTHORITATIVE_OVERLAP_REASON = (
    "Owner-held HMAC overlap commitments and complete source-session receipts are absent, so population overlap remains unresolved."
)


class SymptomReviewError(ValueError):
    """Raised when a development symptom-review artifact fails closed."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise SymptomReviewError(message)


def _sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _canonical_hash(value: Any) -> str:
    return _sha256(task_population.canonical_json_bytes(value))


def _prompt_hash(prompt: str) -> str:
    return _sha256(PROMPT_HASH_DOMAIN + prompt.encode("utf-8"))


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("ledger_sha256" in projected, "review ledger is missing ledger_sha256")
    projected["ledger_sha256"] = None
    return _canonical_hash(projected)


def _review_record_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("review_record_sha256" in projected, "review record is missing its hash")
    projected["review_record_sha256"] = None
    return _sha256(REVIEW_HASH_DOMAIN + task_population.canonical_json_bytes(projected))


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise SymptomReviewError("JSON contains a duplicate object key")
        result[key] = value
    return result


def _load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise SymptomReviewError(f"cannot load {path}: {exc}") from exc
    _require(isinstance(value, dict), f"{path} root must be an object")
    return value, raw


def _write_atomic(path: pathlib.Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    rendered = json.dumps(value, ensure_ascii=False, allow_nan=False, indent=2, sort_keys=True) + "\n"
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(rendered)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _git(repo: pathlib.Path, args: Sequence[str]) -> str:
    environment = dict(os.environ)
    environment["GIT_NO_REPLACE_OBJECTS"] = "1"
    try:
        completed = subprocess.run(
            ["git", *args],
            cwd=repo,
            env=environment,
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except OSError as exc:
        raise SymptomReviewError(f"cannot execute git: {exc}") from exc
    _require(completed.returncode == 0, f"git command failed: {completed.stderr.strip() or 'unknown error'}")
    return completed.stdout


def _implementation_identity() -> dict[str, Any]:
    schema = pathlib.Path(__file__).parent / "schemas" / "development-task-symptom-review-v1.schema.json"
    files = [
        {"role": "symptom_review_workflow", "sha256": _sha256(pathlib.Path(__file__).read_bytes())},
        {"role": "symptom_review_schema", "sha256": _sha256(schema.read_bytes())},
        {"role": "eligibility_scanner", "sha256": _sha256(pathlib.Path(task_eligibility.__file__).read_bytes())},
        {"role": "negative_control_runner", "sha256": _sha256(pathlib.Path(task_negative_control.__file__).read_bytes())},
        {"role": "population_contract_validator", "sha256": _sha256(pathlib.Path(task_population.__file__).read_bytes())},
    ]
    return {
        "files": files,
        "aggregate_sha256": _canonical_hash(files),
        "python_implementation": platform.python_implementation(),
        "python_version": platform.python_version(),
    }


def _validate_timestamp(value: Any, field: str) -> None:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise SymptomReviewError(f"{field} is invalid") from exc
    _require(parsed.tzinfo is not None and parsed.utcoffset() is not None, f"{field} lacks an offset")


def _valid_sha(value: Any) -> bool:
    return isinstance(value, str) and SHA256_RE.fullmatch(value) is not None and value != "0" * 64


def _valid_oid(value: Any) -> bool:
    return isinstance(value, str) and OID_RE.fullmatch(value) is not None and value != "0" * 40


def _answer_bearing_syntax_findings(text: str) -> list[str]:
    findings: list[str] = []
    if text != text.strip() or "\n" in text or "\r" in text:
        findings.append("not_single_trimmed_paragraph")
    if "`" in text:
        findings.append("inline_code_syntax")
    if re.search(r"(?:[A-Za-z0-9_.-]+[/\\]){1,}[A-Za-z0-9_.-]+", text):
        findings.append("path_syntax")
    if re.search(r"\b[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)", text):
        findings.append("function_call_syntax")
    if re.search(r"\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+\b", text):
        findings.append("identifier_syntax")
    if re.search(r"(?:^|\s)--[a-z0-9-]+|(?:^|\s)\$\s|\b(?:go test|git (?:checkout|fetch|push|config|worktree))\b", text, re.I):
        findings.append("command_syntax")
    lowered = text.casefold()
    for term in BANNED_PROMPT_TERMS:
        if term in lowered:
            findings.append(f"answer_bearing_term:{term}")
    return findings


def _prompt_static_findings(prompt: str) -> list[str]:
    findings = _answer_bearing_syntax_findings(prompt)
    if not 80 <= len(prompt) <= 800:
        findings.append("prompt_length_outside_policy")
    return findings


def _source_history(repo: pathlib.Path, candidate: dict[str, Any]) -> tuple[dict[str, Any], set[str]]:
    commit = candidate["commit_oid"]
    parents = _git(repo, ["show", "-s", "--format=%P", commit]).split()
    _require(bool(parents) and parents[0] == candidate["parent_oid"], f"{commit}: first parent drift")
    _require(len(parents) == candidate["merge_parent_count"], f"{commit}: parent count drift")
    _require(len(parents) == 2, f"{commit}: symptom-review source lineage requires a two-parent merge")
    commits = [line for line in _git(repo, ["rev-list", "--reverse", f"{parents[0]}..{parents[1]}"]).splitlines() if line]
    _require(bool(commits), f"{commit}: source branch has no commits outside first parent")
    commit_set = set(commits)
    _require(len(commit_set) == len(commits), f"{commit}: duplicate source commit")
    trailers: list[str] = []
    valid_trailer_commits = 0
    for source_commit in commits:
        message = _git(repo, ["show", "-s", "--format=%B", source_commit])
        matches = TRAILER_RE.findall(message)
        if len(matches) == 1 and CHECKPOINT_RE.fullmatch(matches[0]) is not None:
            valid_trailer_commits += 1
            trailers.append(matches[0])
    history = {
        "source_commit_count": len(commits),
        "source_commit_set_sha256": _canonical_hash(sorted(commits)),
        "checkpoint_trailer_commit_count": valid_trailer_commits,
        "checkpoint_trailer_set_sha256": _canonical_hash(sorted(set(trailers))),
        "checkpoint_trailer_coverage": "complete" if valid_trailer_commits == len(commits) else "incomplete",
        "source_session_decision": "unresolved_no_owner_source_session_receipt",
        "source_session_reason": SOURCE_SESSION_REASON,
    }
    return history, commit_set


def _fix_identity(repository_id: str, commit_oid: str, source_commit_set_sha256: str) -> str:
    payload = task_population.canonical_json_bytes(
        {
            "repository_id": repository_id,
            "commit_oid": commit_oid,
            "source_commit_set_sha256": source_commit_set_sha256,
        }
    )
    return _sha256(FIX_HASH_DOMAIN + payload)


def _family_commitment(repository_id: str, family_id: str) -> str:
    return _sha256(FAMILY_HASH_DOMAIN + f"{repository_id}\0{family_id}".encode("utf-8"))


def _review_policy() -> dict[str, Any]:
    return {
        "name": POLICY_NAME,
        "prompt_hash": "sha256_domain_separated_utf8_v1",
        "required_checks": list(REQUIRED_CHECKS),
        "static_banned_terms_sha256": _canonical_hash(list(BANNED_PROMPT_TERMS)),
        "independent_read_only_audit_required": True,
        "semantic_patch_and_test_comparison_required": True,
        "review_reason_static_leakage_scan_required": True,
    }


def _validate_reviewer(value: Any, field: str) -> None:
    expected = {"reviewer_id", "role", "tool_identity", "identity_attestation", "independence"}
    _require(isinstance(value, dict) and set(value) == expected, f"{field} fields differ")
    _require(
        isinstance(value["reviewer_id"], str) and REVIEWER_RE.fullmatch(value["reviewer_id"]) is not None,
        f"{field}.reviewer_id is invalid",
    )
    _require(
        isinstance(value["role"], str) and value["role"] in {"draft_author", "independent_read_only_auditor"},
        f"{field}.role is invalid",
    )
    _require(isinstance(value["tool_identity"], str) and bool(value["tool_identity"]), f"{field}.tool_identity is invalid")
    _require(
        value["identity_attestation"] == "self_reported_local_process_not_cryptographically_attested",
        f"{field}.identity_attestation differs",
    )
    expected_independence = {
        "draft_author": "inspected_source_and_authored_prompts",
        "independent_read_only_auditor": "did_not_edit_draft_or_source",
    }
    _require(value["independence"] == expected_independence[value["role"]], f"{field}.independence differs")


def _validate_review_record(
    value: Any,
    *,
    field: str,
    expected_reviewer: str,
    expected_prompt_sha256: str,
) -> None:
    expected = {
        "reviewer_id", "prompt_sha256", "decision", "checks", "reason_code", "reason",
        "review_record_sha256",
    }
    _require(isinstance(value, dict) and set(value) == expected, f"{field} fields differ")
    _require(value["reviewer_id"] == expected_reviewer, f"{field}.reviewer_id differs")
    _require(value["prompt_sha256"] == expected_prompt_sha256, f"{field}.prompt_sha256 differs")
    _require(
        isinstance(value["decision"], str) and value["decision"] in {"accept", "reject"},
        f"{field}.decision is invalid",
    )
    checks = value["checks"]
    _require(isinstance(checks, dict) and set(checks) == set(REQUIRED_CHECKS), f"{field}.checks differ")
    _require(all(type(checks[name]) is bool for name in REQUIRED_CHECKS), f"{field}.checks values are invalid")
    if value["decision"] == "accept":
        _require(all(checks.values()), f"{field}: accepted review contains a failed check")
    else:
        _require(not all(checks.values()), f"{field}: rejected review must identify a failed check")
    _require(
        isinstance(value["reason_code"], str) and FAMILY_RE.fullmatch(value["reason_code"]) is not None,
        f"{field}.reason_code is invalid",
    )
    _require(isinstance(value["reason"], str) and 20 <= len(value["reason"]) <= 500, f"{field}.reason is invalid")
    _require(
        not _answer_bearing_syntax_findings(value["reason"]),
        f"{field}.reason contains answer-bearing syntax: {_answer_bearing_syntax_findings(value['reason'])}",
    )
    _require(_valid_sha(value["review_record_sha256"]), f"{field}.review_record_sha256 is invalid")
    _require(value["review_record_sha256"] == _review_record_hash(value), f"{field} hash differs")


def _validate_inputs(
    ledger: dict[str, Any],
    ledger_raw: bytes,
    negative: dict[str, Any],
    negative_raw: bytes,
) -> tuple[dict[int, dict[str, Any]], dict[int, dict[str, Any]]]:
    try:
        task_eligibility.validate_scan(ledger)
        task_negative_control.validate_receipt(negative)
    except (task_eligibility.EligibilityError, task_negative_control.NegativeControlError) as exc:
        raise SymptomReviewError(str(exc)) from exc
    _require(negative["repository_id"] == ledger["repository_id"], "input repository IDs differ")
    _require(negative["base_oid"] == ledger["base_oid"] and negative["head_oid"] == ledger["head_oid"], "input ranges differ")
    _require(negative["input_ledger_sha256"] == ledger["ledger_sha256"], "negative control binds another ledger")
    _require(negative["input_ledger_file_sha256"] == _sha256(ledger_raw), "negative-control ledger file hash differs")
    candidates = {item["first_parent_position"]: item for item in ledger["candidates"]}
    results = {item["first_parent_position"]: item for item in negative["results"]}
    _require(len(candidates) == len(ledger["candidates"]), "duplicate candidate position")
    _require(len(results) == len(negative["results"]), "duplicate negative-control position")
    _require(set(candidates) == set(results), "negative-control coverage differs from eligibility candidates")
    for position in candidates:
        candidate = candidates[position]
        result = results[position]
        for field in ("candidate_ref", "commit_oid", "parent_oid", "source_file_count", "source_paths_sha256", "test_file_count"):
            _require(candidate[field] == result[field], f"position {position}: input {field} differs")
    _require(_sha256(negative_raw) != "0" * 64, "negative-control file hash is invalid")
    return candidates, results


def validate_review(
    value: dict[str, Any],
    *,
    ledger: dict[str, Any],
    ledger_raw: bytes,
    negative: dict[str, Any],
    negative_raw: bytes,
    source_repo: pathlib.Path | None = None,
) -> None:
    candidates, results = _validate_inputs(ledger, ledger_raw, negative, negative_raw)
    if source_repo is not None:
        try:
            task_negative_control._validate_repository_binding(source_repo, ledger)
            for candidate_index, candidate in enumerate(ledger["candidates"]):
                task_negative_control._candidate_inputs(source_repo, candidate, candidate_index)
        except task_negative_control.NegativeControlError as exc:
            raise SymptomReviewError(str(exc)) from exc
    expected_root = {
        "schema_version", "profile", "status", "exposure", "authority", "repository_id", "base_oid",
        "head_oid", "generated_at", "inputs", "selection", "implementation", "review_policy", "reviewers",
        "summary", "items", "ledger_sha256",
    }
    _require(set(value) == expected_root, "review ledger root fields differ")
    _require(type(value["schema_version"]) is int and value["schema_version"] == 1, "schema version differs")
    _require(value["profile"] == PROFILE and value["status"] == STATUS, "review profile/status differs")
    _require(value["exposure"] == EXPOSURE, "review exposure differs")
    _require(value["authority"] == AUTHORITY, "review authority must remain non-authoritative")
    _require(value["repository_id"] == ledger["repository_id"], "review repository differs")
    _require(value["base_oid"] == ledger["base_oid"] and value["head_oid"] == ledger["head_oid"], "review range differs")
    _validate_timestamp(value["generated_at"], "generated_at")
    _require(_valid_sha(value["ledger_sha256"]), "ledger_sha256 is invalid")
    _require(value["ledger_sha256"] == _self_hash(value), "review ledger self hash differs")

    inputs = value["inputs"]
    expected_inputs = {
        "eligibility_ledger_path", "eligibility_ledger_sha256", "eligibility_ledger_file_sha256",
        "negative_control_path", "negative_control_receipt_sha256", "negative_control_file_sha256",
    }
    _require(isinstance(inputs, dict) and set(inputs) == expected_inputs, "input bindings differ")
    _require(inputs["eligibility_ledger_path"] == "development-task-eligibility-scan-v1.json", "eligibility path differs")
    _require(inputs["negative_control_path"] == "development-task-negative-control-v1.json", "negative-control path differs")
    _require(inputs["eligibility_ledger_sha256"] == ledger["ledger_sha256"], "eligibility self hash differs")
    _require(inputs["eligibility_ledger_file_sha256"] == _sha256(ledger_raw), "eligibility file hash differs")
    _require(inputs["negative_control_receipt_sha256"] == negative["receipt_sha256"], "negative-control self hash differs")
    _require(inputs["negative_control_file_sha256"] == _sha256(negative_raw), "negative-control file hash differs")

    eligible_positions = [position for position, result in results.items() if result["classification"] == "eligible_for_symptom_review"]
    eligible_positions.sort()
    excluded: dict[str, list[int]] = {name: [] for name in task_negative_control.CLASSIFICATIONS if name != "eligible_for_symptom_review"}
    for position, result in sorted(results.items()):
        if result["classification"] != "eligible_for_symptom_review":
            excluded[result["classification"]].append(position)
    selection = value["selection"]
    _require(
        isinstance(selection, dict)
        and set(selection) == {"rule", "selected_first_parent_positions", "excluded_first_parent_positions"},
        "selection fields differ",
    )
    _require(selection["rule"] == SELECTION_RULE, "selection rule differs")
    _require(selection["selected_first_parent_positions"] == eligible_positions, "selected positions differ")
    _require(selection["excluded_first_parent_positions"] == excluded, "excluded positions differ")
    _require(value["implementation"] == _implementation_identity(), "implementation identity differs from current bytes")
    _require(value["review_policy"] == _review_policy(), "review policy differs")

    reviewers = value["reviewers"]
    _require(isinstance(reviewers, list) and len(reviewers) == 2, "exactly two reviewers are required")
    for index, reviewer in enumerate(reviewers):
        _validate_reviewer(reviewer, f"reviewers[{index}]")
    roles = {reviewer["role"]: reviewer for reviewer in reviewers}
    _require(set(roles) == {"draft_author", "independent_read_only_auditor"}, "reviewer roles differ")
    _require(
        roles["draft_author"]["reviewer_id"] != roles["independent_read_only_auditor"]["reviewer_id"],
        "independent reviewer must differ from draft author",
    )

    items = value["items"]
    _require(isinstance(items, list) and len(items) == len(eligible_positions), "review item coverage differs")
    positions = [item.get("first_parent_position") for item in items if isinstance(item, dict)]
    _require(positions == eligible_positions, "review items are not exact eligible first-parent order")
    prompts: Counter[str] = Counter()
    source_diffs: Counter[str] = Counter()
    test_diffs: Counter[str] = Counter()
    families: Counter[str] = Counter()
    dispositions: Counter[str] = Counter()
    source_commit_sets: dict[int, set[str]] = {}
    expected_item_fields = {
        "first_parent_position", "candidate_ref", "commit_oid", "parent_oid", "tree_oid", "commitments",
        "source_history", "prompt", "prompt_sha256", "draft_review", "independent_review", "disposition",
        "relationships",
    }
    for index, item in enumerate(items):
        position = eligible_positions[index]
        candidate = candidates[position]
        result = results[position]
        _require(set(item) == expected_item_fields, f"item[{index}] fields differ")
        for field in ("first_parent_position", "candidate_ref", "commit_oid", "parent_oid", "tree_oid"):
            _require(item[field] == candidate[field], f"item[{index}].{field} differs")
        commitments = item["commitments"]
        expected_commitments = {
            "source_paths_sha256": candidate["source_paths_sha256"],
            "test_paths_sha256": candidate["test_paths_sha256"],
            "source_diff_sha256": candidate["source_diff_sha256"],
            "test_diff_sha256": candidate["test_diff_sha256"],
            "test_command_sha256": result["test_command_sha256"],
            "baseline_output_sha256": result["baseline"]["output_sha256"],
            "reversed_source_output_sha256": result["reversed_source"]["output_sha256"],
        }
        _require(commitments == expected_commitments, f"item[{index}] commitments differ")
        source_diffs[commitments["source_diff_sha256"]] += 1
        test_diffs[commitments["test_diff_sha256"]] += 1

        prompt = item["prompt"]
        _require(isinstance(prompt, str), f"item[{index}].prompt is invalid")
        _require(not _prompt_static_findings(prompt), f"item[{index}] prompt static leakage: {_prompt_static_findings(prompt)}")
        _require(item["prompt_sha256"] == _prompt_hash(prompt), f"item[{index}] prompt hash differs")
        prompts[item["prompt_sha256"]] += 1
        _validate_review_record(
            item["draft_review"],
            field=f"item[{index}].draft_review",
            expected_reviewer=roles["draft_author"]["reviewer_id"],
            expected_prompt_sha256=item["prompt_sha256"],
        )
        _validate_review_record(
            item["independent_review"],
            field=f"item[{index}].independent_review",
            expected_reviewer=roles["independent_read_only_auditor"]["reviewer_id"],
            expected_prompt_sha256=item["prompt_sha256"],
        )
        expected_disposition = (
            "accepted_for_development_only"
            if item["draft_review"]["decision"] == item["independent_review"]["decision"] == "accept"
            else "rejected_from_development_review"
        )
        _require(item["disposition"] == expected_disposition, f"item[{index}] disposition differs")
        dispositions[expected_disposition] += 1

        history = item["source_history"]
        expected_history_fields = {
            "source_commit_count", "source_commit_set_sha256", "checkpoint_trailer_commit_count",
            "checkpoint_trailer_set_sha256", "checkpoint_trailer_coverage", "source_session_decision",
            "source_session_reason",
        }
        _require(isinstance(history, dict) and set(history) == expected_history_fields, f"item[{index}] source history fields differ")
        _require(
            history["source_session_decision"] == "unresolved_no_owner_source_session_receipt",
            f"item[{index}] invents a source-session decision",
        )
        _require(history["source_session_reason"] == SOURCE_SESSION_REASON, f"item[{index}] source-session reason differs")
        if source_repo is not None:
            expected_history, commit_set = _source_history(source_repo, candidate)
            _require(history == expected_history, f"item[{index}] source history differs from repository")
            source_commit_sets[position] = commit_set
        else:
            _require(type(history["source_commit_count"]) is int and history["source_commit_count"] > 0, f"item[{index}] source commit count is invalid")
            _require(_valid_sha(history["source_commit_set_sha256"]), f"item[{index}] source commit hash is invalid")
            _require(type(history["checkpoint_trailer_commit_count"]) is int and 0 <= history["checkpoint_trailer_commit_count"] <= history["source_commit_count"], f"item[{index}] checkpoint count is invalid")
            _require(_valid_sha(history["checkpoint_trailer_set_sha256"]), f"item[{index}] checkpoint hash is invalid")
            expected_coverage = "complete" if history["checkpoint_trailer_commit_count"] == history["source_commit_count"] else "incomplete"
            _require(history["checkpoint_trailer_coverage"] == expected_coverage, f"item[{index}] checkpoint coverage differs")

        relationships = item["relationships"]
        expected_relationships = {
            "fix_identity_sha256", "fix_overlap_decision", "fix_overlap_reason", "family_id",
            "family_commitment_sha256", "family_overlap_decision", "family_overlap_reason",
            "prompt_overlap_decision", "prompt_overlap_reason", "patch_overlap_decision",
            "patch_overlap_reason", "source_session_overlap_decision", "source_session_overlap_reason",
            "authoritative_overlap_decision", "authoritative_overlap_reason",
        }
        _require(isinstance(relationships, dict) and set(relationships) == expected_relationships, f"item[{index}] relationship fields differ")
        _require(
            relationships["fix_identity_sha256"]
            == _fix_identity(value["repository_id"], item["commit_oid"], history["source_commit_set_sha256"]),
            f"item[{index}] fix identity differs",
        )
        family = relationships["family_id"]
        _require(isinstance(family, str) and FAMILY_RE.fullmatch(family) is not None, f"item[{index}] family ID is invalid")
        _require(relationships["family_commitment_sha256"] == _family_commitment(value["repository_id"], family), f"item[{index}] family commitment differs")
        families[family] += 1
        _require(
            relationships["source_session_overlap_decision"] == "unresolved_no_owner_source_session_receipt"
            and relationships["authoritative_overlap_decision"] == "unresolved_owner_hmac_and_source_session_receipts_absent",
            f"item[{index}] overclaims authoritative overlap",
        )
        _require(relationships["source_session_overlap_reason"] == SOURCE_SESSION_REASON, f"item[{index}] source-session overlap reason differs")
        _require(relationships["authoritative_overlap_reason"] == AUTHORITATIVE_OVERLAP_REASON, f"item[{index}] authoritative overlap reason differs")

    for index, item in enumerate(items):
        relationships = item["relationships"]
        expected_prompt_overlap = "exact_duplicate_rejected" if prompts[item["prompt_sha256"]] > 1 else "no_exact_duplicate_observed"
        _require(relationships["prompt_overlap_decision"] == expected_prompt_overlap, f"item[{index}] prompt overlap differs")
        _require(relationships["prompt_overlap_reason"] == PROMPT_DISTINCT_REASON, f"item[{index}] prompt overlap reason differs")
        source_duplicate = source_diffs[item["commitments"]["source_diff_sha256"]] > 1
        test_duplicate = test_diffs[item["commitments"]["test_diff_sha256"]] > 1
        expected_patch_overlap = "exact_patch_duplicate_rejected" if source_duplicate or test_duplicate else "no_exact_source_or_test_diff_duplicate_observed"
        _require(relationships["patch_overlap_decision"] == expected_patch_overlap, f"item[{index}] patch overlap differs")
        _require(relationships["patch_overlap_reason"] == PATCH_DISTINCT_REASON, f"item[{index}] patch overlap reason differs")
        expected_family_overlap = "shared_development_family_requires_owner_dedup" if families[relationships["family_id"]] > 1 else "no_shared_development_family_observed"
        _require(relationships["family_overlap_decision"] == expected_family_overlap, f"item[{index}] family overlap differs")
        expected_family_reason = (
            f"The cleartext development family label occurs {families[relationships['family_id']]} times; "
            "an owner-key deduplication decision is still required."
            if families[relationships["family_id"]] > 1
            else "The cleartext development family label occurs once in this review."
        )
        _require(relationships["family_overlap_reason"] == expected_family_reason, f"item[{index}] family overlap reason differs")
        _require(
            relationships["fix_overlap_decision"] == "distinct_source_commit_set_observed",
            f"item[{index}] fix overlap must remain the accepted disjoint state",
        )
        _require(relationships["fix_overlap_reason"] == FIX_DISTINCT_REASON, f"item[{index}] fix overlap reason differs")

    _require(not any(count > 1 for count in prompts.values()), "exact duplicate prompt must fail closed")
    _require(not any(count > 1 for count in source_diffs.values()), "exact duplicate source patch must fail closed")
    _require(not any(count > 1 for count in test_diffs.values()), "exact duplicate test patch must fail closed")
    if source_repo is not None:
        all_positions = sorted(source_commit_sets)
        for offset, position in enumerate(all_positions):
            for other_position in all_positions[offset + 1 :]:
                _require(
                    not source_commit_sets[position] & source_commit_sets[other_position],
                    f"positions {position} and {other_position} share source commits",
                )

    summary = value["summary"]
    expected_summary = {
        "selected_count": len(items),
        "accepted_count": dispositions["accepted_for_development_only"],
        "rejected_count": dispositions["rejected_from_development_review"],
        "shared_family_item_count": sum(count for count in families.values() if count > 1),
        "unresolved_source_session_count": len(items),
        "authoritative_population_member_count": 0,
    }
    _require(summary == expected_summary, "review summary differs")


def _seal_review_record(record: dict[str, Any]) -> dict[str, Any]:
    sealed = copy.deepcopy(record)
    sealed["review_record_sha256"] = None
    sealed["review_record_sha256"] = _review_record_hash(sealed)
    return sealed


def build_review(
    *,
    repo: pathlib.Path,
    ledger: dict[str, Any],
    ledger_raw: bytes,
    negative: dict[str, Any],
    negative_raw: bytes,
    draft: dict[str, Any],
) -> dict[str, Any]:
    candidates, results = _validate_inputs(ledger, ledger_raw, negative, negative_raw)
    expected_draft = {"generated_at", "reviewers", "items"}
    _require(set(draft) == expected_draft, "draft root fields differ")
    _validate_timestamp(draft["generated_at"], "draft.generated_at")
    reviewers = draft["reviewers"]
    _require(isinstance(reviewers, list) and len(reviewers) == 2, "draft requires two reviewers")
    for index, reviewer in enumerate(reviewers):
        _validate_reviewer(reviewer, f"draft.reviewers[{index}]")
    roles = {reviewer["role"]: reviewer for reviewer in reviewers}
    _require(set(roles) == {"draft_author", "independent_read_only_auditor"}, "draft reviewer roles differ")
    eligible_positions = sorted(position for position, result in results.items() if result["classification"] == "eligible_for_symptom_review")
    definitions = draft["items"]
    _require(isinstance(definitions, list), "draft.items must be an array")
    expected_definition = {"first_parent_position", "family_id", "prompt", "draft_review", "independent_review"}
    for index, definition in enumerate(definitions):
        _require(isinstance(definition, dict) and set(definition) == expected_definition, f"draft item[{index}] fields differ")
        _require(
            isinstance(definition["family_id"], str) and FAMILY_RE.fullmatch(definition["family_id"]) is not None,
            f"draft item[{index}].family_id is invalid",
        )
        _require(isinstance(definition["prompt"], str), f"draft item[{index}].prompt is invalid")
        _require(isinstance(definition["draft_review"], dict), f"draft item[{index}].draft_review is invalid")
        _require(isinstance(definition["independent_review"], dict), f"draft item[{index}].independent_review is invalid")
    _require([item["first_parent_position"] for item in definitions] == eligible_positions, "draft item positions differ")
    family_counts = Counter(item.get("family_id") for item in definitions)
    source_histories: dict[int, dict[str, Any]] = {}
    source_sets: dict[int, set[str]] = {}
    for position in eligible_positions:
        source_histories[position], source_sets[position] = _source_history(repo, candidates[position])
    for index, position in enumerate(eligible_positions):
        for other_position in eligible_positions[index + 1 :]:
            _require(not source_sets[position] & source_sets[other_position], f"positions {position} and {other_position} share source commits")

    items: list[dict[str, Any]] = []
    for definition, position in zip(definitions, eligible_positions, strict=True):
        candidate = candidates[position]
        result = results[position]
        prompt = definition["prompt"]
        _require(isinstance(prompt, str) and not _prompt_static_findings(prompt), f"draft position {position} prompt static leakage: {_prompt_static_findings(prompt) if isinstance(prompt, str) else ['not_string']}")
        prompt_sha = _prompt_hash(prompt)
        draft_review = copy.deepcopy(definition["draft_review"])
        independent_review = copy.deepcopy(definition["independent_review"])
        for record, reviewer in (
            (draft_review, roles["draft_author"]),
            (independent_review, roles["independent_read_only_auditor"]),
        ):
            record["reviewer_id"] = reviewer["reviewer_id"]
            record["prompt_sha256"] = prompt_sha
            record["review_record_sha256"] = None
        draft_review = _seal_review_record(draft_review)
        independent_review = _seal_review_record(independent_review)
        disposition = (
            "accepted_for_development_only"
            if draft_review["decision"] == independent_review["decision"] == "accept"
            else "rejected_from_development_review"
        )
        family = definition["family_id"]
        source_history = source_histories[position]
        items.append(
            {
                "first_parent_position": position,
                "candidate_ref": candidate["candidate_ref"],
                "commit_oid": candidate["commit_oid"],
                "parent_oid": candidate["parent_oid"],
                "tree_oid": candidate["tree_oid"],
                "commitments": {
                    "source_paths_sha256": candidate["source_paths_sha256"],
                    "test_paths_sha256": candidate["test_paths_sha256"],
                    "source_diff_sha256": candidate["source_diff_sha256"],
                    "test_diff_sha256": candidate["test_diff_sha256"],
                    "test_command_sha256": result["test_command_sha256"],
                    "baseline_output_sha256": result["baseline"]["output_sha256"],
                    "reversed_source_output_sha256": result["reversed_source"]["output_sha256"],
                },
                "source_history": source_history,
                "prompt": prompt,
                "prompt_sha256": prompt_sha,
                "draft_review": draft_review,
                "independent_review": independent_review,
                "disposition": disposition,
                "relationships": {
                    "fix_identity_sha256": _fix_identity(ledger["repository_id"], candidate["commit_oid"], source_history["source_commit_set_sha256"]),
                    "fix_overlap_decision": "distinct_source_commit_set_observed",
                    "fix_overlap_reason": FIX_DISTINCT_REASON,
                    "family_id": family,
                    "family_commitment_sha256": _family_commitment(ledger["repository_id"], family),
                    "family_overlap_decision": (
                        "shared_development_family_requires_owner_dedup"
                        if family_counts[family] > 1
                        else "no_shared_development_family_observed"
                    ),
                    "family_overlap_reason": (
                        f"The cleartext development family label occurs {family_counts[family]} times; "
                        "an owner-key deduplication decision is still required."
                        if family_counts[family] > 1
                        else "The cleartext development family label occurs once in this review."
                    ),
                    "prompt_overlap_decision": "no_exact_duplicate_observed",
                    "prompt_overlap_reason": PROMPT_DISTINCT_REASON,
                    "patch_overlap_decision": "no_exact_source_or_test_diff_duplicate_observed",
                    "patch_overlap_reason": PATCH_DISTINCT_REASON,
                    "source_session_overlap_decision": "unresolved_no_owner_source_session_receipt",
                    "source_session_overlap_reason": SOURCE_SESSION_REASON,
                    "authoritative_overlap_decision": "unresolved_owner_hmac_and_source_session_receipts_absent",
                    "authoritative_overlap_reason": AUTHORITATIVE_OVERLAP_REASON,
                },
            }
        )
    dispositions = Counter(item["disposition"] for item in items)
    excluded: dict[str, list[int]] = {name: [] for name in task_negative_control.CLASSIFICATIONS if name != "eligible_for_symptom_review"}
    for position, result in sorted(results.items()):
        if result["classification"] != "eligible_for_symptom_review":
            excluded[result["classification"]].append(position)
    review = {
        "schema_version": 1,
        "profile": PROFILE,
        "status": STATUS,
        "exposure": EXPOSURE,
        "authority": copy.deepcopy(AUTHORITY),
        "repository_id": ledger["repository_id"],
        "base_oid": ledger["base_oid"],
        "head_oid": ledger["head_oid"],
        "generated_at": draft["generated_at"],
        "inputs": {
            "eligibility_ledger_path": "development-task-eligibility-scan-v1.json",
            "eligibility_ledger_sha256": ledger["ledger_sha256"],
            "eligibility_ledger_file_sha256": _sha256(ledger_raw),
            "negative_control_path": "development-task-negative-control-v1.json",
            "negative_control_receipt_sha256": negative["receipt_sha256"],
            "negative_control_file_sha256": _sha256(negative_raw),
        },
        "selection": {
            "rule": SELECTION_RULE,
            "selected_first_parent_positions": eligible_positions,
            "excluded_first_parent_positions": excluded,
        },
        "implementation": _implementation_identity(),
        "review_policy": _review_policy(),
        "reviewers": copy.deepcopy(reviewers),
        "summary": {
            "selected_count": len(items),
            "accepted_count": dispositions["accepted_for_development_only"],
            "rejected_count": dispositions["rejected_from_development_review"],
            "shared_family_item_count": sum(count for count in family_counts.values() if count > 1),
            "unresolved_source_session_count": len(items),
            "authoritative_population_member_count": 0,
        },
        "items": items,
        "ledger_sha256": None,
    }
    review["ledger_sha256"] = _self_hash(review)
    validate_review(
        review,
        ledger=ledger,
        ledger_raw=ledger_raw,
        negative=negative,
        negative_raw=negative_raw,
        source_repo=repo,
    )
    return review


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    build_parser = subparsers.add_parser("build", help="build a sealed development-only review from audited prompt definitions")
    build_parser.add_argument("--repo", required=True, type=pathlib.Path)
    build_parser.add_argument("--ledger", required=True, type=pathlib.Path)
    build_parser.add_argument("--negative-control", required=True, type=pathlib.Path)
    build_parser.add_argument("--draft", required=True, type=pathlib.Path)
    build_parser.add_argument("--output", required=True, type=pathlib.Path)
    check_parser = subparsers.add_parser("check", help="validate the sealed review and exact local source bindings")
    check_parser.add_argument("--repo", required=True, type=pathlib.Path)
    check_parser.add_argument("--ledger", required=True, type=pathlib.Path)
    check_parser.add_argument("--negative-control", required=True, type=pathlib.Path)
    check_parser.add_argument("review", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        ledger, ledger_raw = _load_json(args.ledger)
        negative, negative_raw = _load_json(args.negative_control)
        if args.command == "build":
            draft, _ = _load_json(args.draft)
            review = build_review(
                repo=args.repo,
                ledger=ledger,
                ledger_raw=ledger_raw,
                negative=negative,
                negative_raw=negative_raw,
                draft=draft,
            )
            _write_atomic(args.output, review)
        else:
            review, _ = _load_json(args.review)
            validate_review(
                review,
                ledger=ledger,
                ledger_raw=ledger_raw,
                negative=negative,
                negative_raw=negative_raw,
                source_repo=args.repo,
            )
    except SymptomReviewError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
