#!/usr/bin/env python3
"""Audit retained deterministic MCP / Regression Radar tool evidence.

This is deliberately separate from agent A/B proof. It answers a narrower
release-readiness question: do the local MCP tools and Radar detector pass their
contract tests, including QMD-inspired retrieval, location-only redaction, deletion
opt-in, workspace Radar, and server-side tool-result logging?
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import subprocess
import sys
from typing import Any


CLAIM_SCOPE = "mcp_radar_tool_contract"

RADAR_TOOL_SOURCE_PATHS = [
    "benchmarks/agent-brain/audit_radar_tool_evidence.py",
    "benchmarks/agent-brain/record_radar_tool_evidence.py",
    "internal/cli/retrieve.go",
    "internal/cli/retrieve_cmd.go",
    "internal/cli/retrieve_test.go",
    "internal/cli/regression.go",
    "internal/cli/regression_test.go",
    "internal/cli/regression_unix_test.go",
    "internal/cli/mcp.go",
    "internal/cli/mcp_test.go",
    "internal/cli/workspace.go",
    "internal/cli/workspace_test.go",
]

REQUIRED_TESTS = [
    "TestRegressionDetectsChangedOperand",
    "TestRegressionChangedOperandReportsMissingHintedFileDespiteIntactPeer",
    "TestRegressionChangedOperandReportsEachRegressedHintedFile",
    "TestInspectRegressionsCommandJSONAndLocationOnly",
    "TestRegressionDeletionIsOptIn",
    "TestRegressionDeletionRanksCallLocusWithHistoryFileHint",
    "TestRegressionDeletionReportsEachMissingAnchoredCallSite",
    "TestRegressionAssignmentDeletionReportsMissingHintedFileDespiteIntactPeer",
    "TestRegressionAssignmentDeletionReportsEachMissingHintedSymbol",
    "TestRegressionAssignmentDeletionRelatedLocationsStayOnSameInvariant",
    "TestRegressionAssignmentDeletionRelatedLocationsDistinguishSameIdentifierDifferentRHS",
    "TestRegressionAssignmentDeletionDoesNotLetIntactSiblingMaskRHSOnlySite",
    "TestRegressionAssignmentDeletionReportsSameNameReceiverMethods",
    "TestRegressionAssignmentDeletionReportsOneMissingLocusInsideSameFunction",
    "TestRegressionDedupeRankPrefersBoostedCallDeletion",
    "TestRegressionScanHistoryClosesFilesDuringLargeWalk",
    "TestMCPInitializeAndToolsList",
    "TestMCPToolsListIncludesRegressions",
    "TestMCPToolsListIncludesQMDRetrievalSurface",
    "TestMCPToolsListAdvertisesStaleBlindSpots",
    "TestMCPToolSchemasRejectAdditionalProperties",
    "TestMCPToolSchemasMatchArgumentValidator",
    "TestMCPQMDRetrievalSchemasExposeBranchAndNonEmptyMultiGet",
    "TestMCPRejectsInvalidBooleanArguments",
    "TestMCPRejectsInvalidStringAndUnknownArguments",
    "TestMCPDebugLogIncludesToolCallNameAndSafeArgsOnly",
    "TestMCPDebugLogIncludesSuccessfulWorkspaceRadarResult",
    "TestMCPDebugLogDoesNotTreatNotificationsAsExecutedTools",
    "TestMCPDebugLogReviewToolsRedactAndLogSuccess",
    "TestMCPBrainRegressionsTool",
    "TestMCPBrainRegressionsDeletionLocationOnlyKeepsAllAssignmentSites",
    "TestMCPBrainReviewTool",
    "TestMCPBrainWorkspaceReviewTool",
    "TestMCPBrainWorkspaceRegressionsDeletionLocationOnlyAndReviewRedaction",
    "TestMCPBrainWorkspaceToolRequiresWorkspace",
    "TestMCPInitializeEchoesClientProtocolVersion",
    "TestMCPInitializeSupportsJSONLineFraming",
    "TestMCPBrainQueryToolUsesLocalSemanticJSON",
    "TestMCPQMDRetrievalToolsUseLocalFacts",
    "TestMCPBrainContextImpactAndChangesToolsUseLocalSemanticJSON",
    "TestMCPBrainBriefAndQueryToolsUseIndexedHistory",
    "TestMCPToolCallRejectsInvalidIntegerArguments",
    "TestMCPToolCallRejectsInvalidDepth",
    "TestMCPRecoversFromMalformedJSONLineFrame",
    "TestMCPRejectsOversizedAndNegativeFrames",
    "TestMCPBrainStaleUsesEnvRepoRoot",
    "TestWorkspaceRegressionsAggregatesAndToleratesMissingBrain",
    "TestWorkspaceRegressionsDeletionLocationOnlyReportsRelatedLoci",
    "TestWorkspaceRegressionsSkipsUnsafeRepo",
    "TestWorkspaceRegressionsSkipsUnsafeSessionsOnlyRepo",
    "TestWorkspaceRegressionsSkipsUnverifiableRemoteKeyHint",
]


def sha256_file(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def load_manifest(path: pathlib.Path) -> dict[str, Any]:
    with path.open() as f:
        data = json.load(f)
    if not isinstance(data, dict):
        raise ValueError("manifest must be a JSON object")
    return data


def parse_go_test_json(path: pathlib.Path) -> tuple[set[str], list[str], bool]:
    passed: set[str] = set()
    failures: list[str] = []
    package_passed = False
    with path.open() as f:
        for line_no, line in enumerate(f, 1):
            if not line.strip():
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError as exc:
                failures.append(f"{path}:{line_no}: invalid go test JSON: {exc}")
                continue
            if not isinstance(event, dict):
                failures.append(f"{path}:{line_no}: event is not an object")
                continue
            action = event.get("Action")
            test = event.get("Test")
            if action == "pass" and isinstance(test, str):
                passed.add(test)
            if action in {"fail", "panic"}:
                failures.append(f"go test event failed: package={event.get('Package')} test={test or '<package>'}")
            if action == "pass" and test in (None, ""):
                package_passed = True
    return passed, failures, package_passed


def git_output(repo_root: pathlib.Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args],
        cwd=repo_root,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout.strip()


def source_head_drift(repo_root: pathlib.Path, source_head: str) -> tuple[str | None, list[str], list[str]]:
    errors: list[str] = []
    try:
        current_head = git_output(repo_root, "rev-parse", "HEAD")
        git_output(repo_root, "cat-file", "-e", f"{source_head}^{{commit}}")
    except (OSError, subprocess.CalledProcessError) as exc:
        return None, [], [f"unable to verify source_head against git: {exc}"]

    if source_head == current_head:
        return current_head, [], []

    try:
        subprocess.run(
            ["git", "merge-base", "--is-ancestor", source_head, "HEAD"],
            cwd=repo_root,
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except subprocess.CalledProcessError:
        errors.append("source_head must be an ancestor of HEAD")

    try:
        raw = git_output(repo_root, "diff", "--name-only", f"{source_head}..HEAD", "--", *RADAR_TOOL_SOURCE_PATHS)
    except (OSError, subprocess.CalledProcessError) as exc:
        return current_head, [], errors + [f"unable to compare Radar source drift: {exc}"]
    changed = [line for line in raw.splitlines() if line.strip()]
    return current_head, changed, errors


def source_file_hash_drift(repo_root: pathlib.Path, source_files: Any) -> tuple[list[str], list[str], list[str]]:
    errors: list[str] = []
    warnings: list[str] = []
    changed: list[str] = []
    if not isinstance(source_files, dict):
        return [], [], ["source_files must be an object when present"]
    expected_paths = set(RADAR_TOOL_SOURCE_PATHS)
    extra = sorted(str(path) for path in source_files if str(path) not in expected_paths)
    if extra:
        warnings.append("source_files includes extra paths: " + ", ".join(extra[:10]))
    for rel in RADAR_TOOL_SOURCE_PATHS:
        expected = source_files.get(rel)
        digest = expected.removeprefix("sha256:") if isinstance(expected, str) else ""
        if (
            not isinstance(expected, str)
            or not expected.startswith("sha256:")
            or len(digest) != 64
            or any(c not in "0123456789abcdef" for c in digest.lower())
        ):
            errors.append(f"source_files.{rel} must be a sha256:<64 hex> string")
            continue
        path = repo_root / rel
        if not path.is_file():
            errors.append(f"source file missing for Radar tool contract: {rel}")
            changed.append(rel)
            continue
        actual = sha256_file(path)
        if actual != expected:
            changed.append(rel)
    return changed, errors, warnings


def audit_manifest(
    manifest_path: pathlib.Path,
    *,
    repo_root: pathlib.Path | None = None,
    allow_stale_source_head: bool = False,
) -> dict[str, Any]:
    manifest = load_manifest(manifest_path)
    root = manifest_path.parent
    errors: list[str] = []
    warnings: list[str] = []
    source_drift_paths: list[str] = []

    if manifest.get("schema") != 1:
        errors.append("schema must be 1")
    if manifest.get("claim_scope") != CLAIM_SCOPE:
        errors.append(f"claim_scope must be {CLAIM_SCOPE}")
    limitations = manifest.get("limitations")
    if not isinstance(limitations, list) or not any("agent" in str(item).lower() for item in limitations):
        errors.append("limitations must explicitly state this is not agent lift proof")
    source_head = str(manifest.get("source_head") or "")
    if len(source_head) != 40 or any(c not in "0123456789abcdef" for c in source_head.lower()):
        errors.append("source_head must be a 40-character git commit")
    elif repo_root is not None and not allow_stale_source_head:
        source_files = manifest.get("source_files")
        if isinstance(source_files, dict):
            source_drift_paths, hash_errors, hash_warnings = source_file_hash_drift(repo_root, source_files)
            errors.extend(hash_errors)
            warnings.extend(hash_warnings)
            if source_drift_paths:
                errors.append(
                    "Radar tool source file hashes are stale: " + ", ".join(source_drift_paths)
                )
        else:
            _, source_drift_paths, git_errors = source_head_drift(repo_root, source_head)
            errors.extend(git_errors)
        if source_drift_paths and not any("Radar tool source file hashes are stale" in err for err in errors):
            errors.append(
                "source_head is stale for Radar tool contract; changed paths since source_head: "
                + ", ".join(source_drift_paths)
            )

    required = manifest.get("required_tests", REQUIRED_TESTS)
    if required != REQUIRED_TESTS:
        errors.append("required_tests must match the committed Radar tool contract list")

    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or len(artifacts) != 1:
        errors.append("artifacts must contain exactly one go_test_json artifact")
        artifacts = []

    artifact_report: dict[str, Any] | None = None
    if artifacts:
        artifact = artifacts[0]
        if not isinstance(artifact, dict):
            errors.append("artifact entry must be an object")
        else:
            rel = pathlib.Path(str(artifact.get("path") or ""))
            if rel.is_absolute() or ".." in rel.parts:
                errors.append("artifact path must be relative and stay under the manifest directory")
            else:
                artifact_path = root / rel
                if not artifact_path.exists():
                    errors.append(f"artifact missing: {rel}")
                else:
                    actual_hash = sha256_file(artifact_path)
                    expected_hash = artifact.get("sha256")
                    if actual_hash != expected_hash:
                        errors.append(f"artifact sha256 mismatch for {rel}: {actual_hash} != {expected_hash}")
                    passed, parse_failures, package_passed = parse_go_test_json(artifact_path)
                    errors.extend(parse_failures)
                    missing = [name for name in REQUIRED_TESTS if name not in passed]
                    if missing:
                        errors.append("missing required passed tests: " + ", ".join(missing))
                    if not package_passed:
                        errors.append("go test package-level pass event missing")
                    required_set = set(REQUIRED_TESTS)
                    extra = sorted(name for name in passed - required_set if name.split("/", 1)[0] not in required_set)
                    if extra:
                        warnings.append("artifact includes additional passing tests: " + ", ".join(extra[:10]))
                    artifact_report = {
                        "path": str(rel),
                        "sha256": actual_hash,
                        "package_passed": package_passed,
                        "passed_required_tests": sorted(set(REQUIRED_TESTS) & passed),
                        "extra_passed_tests": extra,
                    }

    return {
        "schema": 1,
        "manifest": str(manifest_path),
        "ok": not errors,
        "errors": errors,
        "warnings": warnings,
        "claim_scope": manifest.get("claim_scope"),
        "source_head": manifest.get("source_head"),
        "source_drift_paths": source_drift_paths,
        "required_tests": REQUIRED_TESTS,
        "artifact": artifact_report,
        "limitations": limitations if isinstance(limitations, list) else [],
    }


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# MCP/Radar Tool Evidence Audit",
        "",
        f"- Status: **{'PASS' if report['ok'] else 'FAIL'}**",
        f"- Claim scope: **{report.get('claim_scope') or ''}**",
        f"- Required tests: **{len(report['required_tests'])}**",
    ]
    artifact = report.get("artifact") if isinstance(report.get("artifact"), dict) else None
    if artifact:
        lines.extend([
            f"- Artifact: `{artifact['path']}`",
            f"- Artifact hash: `{artifact['sha256']}`",
            f"- Package pass event: **{artifact['package_passed']}**",
        ])
    if report["errors"]:
        lines.append("")
        lines.append("## Errors")
        lines.extend(f"- {err}" for err in report["errors"])
    if report["warnings"]:
        lines.append("")
        lines.append("## Warnings")
        lines.extend(f"- {warn}" for warn in report["warnings"])
    limitations = report.get("limitations") or []
    if limitations:
        lines.append("")
        lines.append("## Limitations")
        lines.extend(f"- {item}" for item in limitations)
    lines.append("")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=pathlib.Path, required=True)
    parser.add_argument("--out-dir", type=pathlib.Path, default=None)
    parser.add_argument("--fail-on-flags", action="store_true")
    parser.add_argument("--repo-root", type=pathlib.Path, default=None, help="Git repository root used to reject stale source_head drift")
    parser.add_argument("--allow-stale-source-head", action="store_true", help="Skip source_head drift checks against --repo-root")
    args = parser.parse_args(argv)

    report = audit_manifest(
        args.manifest,
        repo_root=args.repo_root,
        allow_stale_source_head=args.allow_stale_source_head,
    )
    out_dir = args.out_dir or args.manifest.parent
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "radar-tool-audit-report.json"
    json_path.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    (out_dir / "radar-tool-audit-report.md").write_text(render_markdown(report))
    print(render_markdown(report))
    print(f"Wrote {json_path} and radar-tool-audit-report.md")
    if args.fail_on_flags and not report["ok"]:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
