#!/usr/bin/env python3
"""Audit retained workspace-Radar evidence for release claims.

Workspace Radar is a stronger claim than single-repo Radar: the agent must use
the workspace MCP tool and bind the call to the intended workspace. This auditor
keeps the current retained evidence honest. It supports a no-claim policy for
failed/saturated workspace candidates, and a proof-required policy for future
retained workspace proof.
"""
from __future__ import annotations

import argparse
import json
import pathlib
import sys
from typing import Any

import audit_radar_evidence


CLAIM_POLICY_PROOF = "proof_required"
CLAIM_POLICY_NO_CLAIM = "no_release_claim"
WORKSPACE_SCOPE = "mcp_workspace_radar_location_only"
REPORT_JSON = "workspace-radar-audit-report.json"
REPORT_MD = "workspace-radar-audit-report.md"


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text())
    except FileNotFoundError as exc:
        raise SystemExit(f"artifact not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit(f"artifact is not valid JSON: {path}: {exc}") from exc


def display_path(path: pathlib.Path) -> str:
    resolved = path.resolve()
    try:
        return str(resolved.relative_to(pathlib.Path.cwd().resolve()))
    except ValueError:
        return str(resolved)


def manifest_artifact(root: pathlib.Path, value: Any, field: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise SystemExit(f"workspace Radar manifest requires non-empty {field}")
    path = pathlib.Path(value)
    if path.is_absolute():
        raise SystemExit(f"workspace Radar manifest {field} must be relative, got {value}")
    if ".." in path.parts:
        raise SystemExit(f"workspace Radar manifest {field} must stay under the evidence directory")
    return (root / path).resolve()


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("workspace Radar manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("workspace Radar manifest schema must be 1")
    policy = data.get("claim_policy")
    if policy not in (CLAIM_POLICY_PROOF, CLAIM_POLICY_NO_CLAIM):
        raise SystemExit("workspace Radar manifest claim_policy must be proof_required or no_release_claim")
    suite_globs = data.get("suite_globs")
    if not isinstance(suite_globs, list) or not suite_globs:
        raise SystemExit("workspace Radar manifest requires non-empty suite_globs")
    for pattern in suite_globs:
        if not isinstance(pattern, str) or not pattern:
            raise SystemExit("workspace Radar manifest suite_globs entries must be non-empty strings")
        if "workspace-radar" not in pattern:
            raise SystemExit("workspace Radar manifest suite_globs must be workspace-specific")


def audit_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)

    results_dir = manifest_artifact(root, manifest.get("results_dir", "."), "results_dir")
    suite_globs = [str(item) for item in manifest["suite_globs"]]
    codex_audit = None
    has_codex_audit_report = bool(manifest.get("codex_audit_report"))
    if has_codex_audit_report:
        codex_audit = audit_radar_evidence.load_codex_audit(
            manifest_artifact(root, manifest.get("codex_audit_report"), "codex_audit_report")
        )
    radar_report = audit_radar_evidence.build_report(results_dir, suite_globs, codex_audit)

    flags: list[str] = []
    notes: list[str] = []
    totals = radar_report["totals"]
    comparisons = radar_report.get("comparisons") if isinstance(radar_report.get("comparisons"), list) else []
    status_counts = totals.get("status_counts") if isinstance(totals.get("status_counts"), dict) else {}
    policy = manifest["claim_policy"]
    required_proof_ready = int(manifest.get("required_proof_ready") or 1)

    if not comparisons:
        flags.append("workspace Radar evidence must retain at least one audited workspace candidate")

    for comp in comparisons:
        if not isinstance(comp, dict):
            continue
        suite = str(comp.get("suite") or "")
        scope = str(comp.get("delivery_scope") or "")
        if "workspace-radar" not in suite and scope != WORKSPACE_SCOPE:
            flags.append(f"non-workspace Radar row selected: suite={suite} scope={scope}")

    expected_status_counts = manifest.get("expected_status_counts")
    if isinstance(expected_status_counts, dict) and expected_status_counts != status_counts:
        flags.append(
            "workspace Radar status_counts mismatch: "
            f"expected {expected_status_counts}, got {status_counts}"
        )

    proof_ready = int(totals.get("proof_ready") or 0)
    promotable = int(totals.get("promotable_or_proof") or 0)
    if policy == CLAIM_POLICY_NO_CLAIM:
        if proof_ready:
            flags.append("no_release_claim evidence must not contain proof-ready workspace Radar comparisons")
        if promotable:
            flags.append("no_release_claim evidence must not contain promotable workspace Radar pilots")
        notes.append(
            "workspace Radar is not release-claimable from this retained evidence; "
            "the retained candidate documents why it was rejected"
        )
    else:
        if not has_codex_audit_report:
            flags.append(
                "proof_required workspace Radar evidence requires codex_audit_report "
                "with MCP named-tool backing"
            )
        if radar_report.get("codex_audit_required") is not True:
            flags.append("proof_required workspace Radar evidence must run with codex audit backing")
        if proof_ready < required_proof_ready:
            flags.append(f"proof_required workspace Radar evidence has {proof_ready} proof-ready comparison(s), require {required_proof_ready}")
        backed_proofs = [
            comp for comp in comparisons
            if isinstance(comp, dict)
            and comp.get("delivery_scope") == WORKSPACE_SCOPE
            and isinstance(comp.get("radar_gate"), dict)
            and comp["radar_gate"].get("proof_ready") is True
            and isinstance(comp["radar_gate"].get("codex_audit_record_backing"), dict)
            and comp["radar_gate"]["codex_audit_record_backing"].get("condition_mcp_verified_ok") is True
            and comp["radar_gate"]["codex_audit_record_backing"].get("condition_mcp_named_tool_verified_ok") is True
            and comp["radar_gate"]["codex_audit_record_backing"].get("condition_mcp_named_tool_completed_ok") is True
        ]
        if len(backed_proofs) < required_proof_ready:
            flags.append(
                "proof_required workspace Radar evidence lacks enough Codex-audited "
                "brain_workspace_regressions named-tool completions"
            )

    return {
        "schema": 1,
        "manifest": display_path(manifest_path),
        "results": display_path(results_dir),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "claim_policy": policy,
        "claimable_workspace_radar": policy == CLAIM_POLICY_PROOF and not flags,
        "required_proof_ready": required_proof_ready if policy == CLAIM_POLICY_PROOF else 0,
        "summary": {
            "radar_comparisons": totals.get("radar_comparisons"),
            "proof_ready": proof_ready,
            "promotable_or_proof": promotable,
            "status_counts": status_counts,
        },
        "radar_report": radar_report,
        "flags": flags,
        "notes": notes,
    }


def render_markdown(report: dict[str, Any]) -> str:
    summary = report["summary"]
    lines = [
        "# Workspace Radar Evidence Audit",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Claim policy: **{report['claim_policy']}**",
        f"- Workspace Radar claimable: **{str(report['claimable_workspace_radar']).lower()}**",
        f"- Radar comparisons: **{summary['radar_comparisons']}**",
        f"- Proof-ready comparisons: **{summary['proof_ready']}**",
        f"- Promotable pilots or proof: **{summary['promotable_or_proof']}**",
    ]
    status_counts = summary.get("status_counts") if isinstance(summary.get("status_counts"), dict) else {}
    if status_counts:
        lines.append("- Status counts: " + ", ".join(f"`{key}`={value}" for key, value in status_counts.items()))
    if report["flags"]:
        lines.extend(["", "## Errors"])
        lines.extend(f"- {flag}" for flag in report["flags"])
    if report["notes"]:
        lines.extend(["", "## Notes"])
        lines.extend(f"- {note}" for note in report["notes"])
    lines.extend(["", "## Radar Rows", ""])
    lines.append("| Suite | Task | Runner | Scope | Status | Recommendation |")
    lines.append("|---|---|---|---|---|---|")
    for comp in report["radar_report"].get("comparisons") or []:
        gate = comp.get("radar_gate") if isinstance(comp.get("radar_gate"), dict) else {}
        lines.append(
            "| "
            + " | ".join(
                [
                    str(comp.get("suite") or ""),
                    str(comp.get("task_id") or ""),
                    str(comp.get("runner") or ""),
                    str(comp.get("delivery_scope") or ""),
                    str(gate.get("status") or ""),
                    str(gate.get("recommendation") or ""),
                ]
            )
            + " |"
        )
    if not report["radar_report"].get("comparisons"):
        lines.append("| _none_ | | | | | |")
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> None:
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / REPORT_JSON).write_text(json.dumps(report, indent=2, sort_keys=True))
    (out_dir / REPORT_MD).write_text(render_markdown(report))


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit retained workspace-Radar evidence.")
    parser.add_argument("--manifest", type=pathlib.Path, required=True, help="Workspace Radar evidence manifest")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Directory for workspace-radar-audit-report.{json,md}")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when the evidence audit has errors")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    report = audit_manifest(args.manifest)
    out_dir = (args.out_dir or args.manifest.parent).resolve()
    write_report(report, out_dir)
    print(render_markdown(report).split("\n\n", 1)[0])
    print(f"\nWrote {out_dir / REPORT_JSON} and {REPORT_MD}")
    if args.fail_on_flags and report["flags"]:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
