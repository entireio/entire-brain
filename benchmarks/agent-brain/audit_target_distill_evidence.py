#!/usr/bin/env python3
"""Audit the target large-repo/frontend distill evidence lane.

This lane is separate from the current-repo distill scheduler proof. It exists
so release materials cannot accidentally reuse a local scheduler artifact as
evidence for Thomas's frontend/large-session-repo performance complaint.
"""
from __future__ import annotations

import argparse
import json
import pathlib
import sys
from typing import Any


CLAIM_POLICY_PROOF = "proof_required"
CLAIM_POLICY_NO_CLAIM = "no_release_claim"
REPORT_JSON = "target-distill-audit-report.json"
REPORT_MD = "target-distill-audit-report.md"
COMMIT_RE = "0123456789abcdef"
CURRENT_REPO = "github.com/ashtom/entire-brain"
CURRENT_REPO_KEY = "gh/ashtom/entire-brain"


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
        raise SystemExit(f"target distill manifest requires non-empty {field}")
    path = pathlib.Path(value)
    if path.is_absolute():
        raise SystemExit(f"target distill manifest {field} must be relative, got {value}")
    return (root / path).resolve()


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("target distill manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("target distill manifest schema must be 1")
    policy = data.get("claim_policy", CLAIM_POLICY_NO_CLAIM)
    if policy not in (CLAIM_POLICY_NO_CLAIM, CLAIM_POLICY_PROOF):
        raise SystemExit("target distill manifest claim_policy must be proof_required or no_release_claim")
    target = data.get("target")
    if not isinstance(target, dict):
        raise SystemExit("target distill manifest requires target object")


def target_flags(target: dict[str, Any]) -> list[str]:
    flags: list[str] = []
    for field in ("repo", "claim_scope"):
        if not isinstance(target.get(field), str) or not target.get(field):
            flags.append(f"target.{field} must be a non-empty string")
    return flags


def is_commit(value: Any) -> bool:
    return isinstance(value, str) and len(value) == 40 and all(ch in COMMIT_RE for ch in value)


def is_sha256_value(value: Any) -> bool:
    return isinstance(value, str) and len(value) == len("sha256:") + 64 and value.startswith("sha256:") and all(ch in COMMIT_RE for ch in value[len("sha256:"):])


def proof_report_target_flags(target: dict[str, Any]) -> list[str]:
    flags: list[str] = []
    if not isinstance(target.get("repo_key"), str) or not target.get("repo_key"):
        flags.append("target distill_report target.repo_key must be a non-empty string")
    if not is_commit(target.get("source_head")):
        flags.append("target distill_report target.source_head must be a 40-character git commit")
    if not is_sha256_value(target.get("brain_manifest_sha256")):
        flags.append("target distill_report target.brain_manifest_sha256 must be sha256:<64 hex>")
    repo = str(target.get("repo") or "")
    repo_key = str(target.get("repo_key") or "")
    claim_scope = str(target.get("claim_scope") or "")
    if repo == CURRENT_REPO or repo_key == CURRENT_REPO_KEY or "current-repo" in claim_scope:
        flags.append("target distill proof must not cite the current-repo scheduler evidence")
    return flags


def audit_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)

    flags: list[str] = []
    notes: list[str] = []
    policy = manifest.get("claim_policy", CLAIM_POLICY_NO_CLAIM)
    target = manifest.get("target") if isinstance(manifest.get("target"), dict) else {}
    flags.extend(target_flags(target))
    distill_report_path = ""
    distill_report: dict[str, Any] = {}

    if policy == CLAIM_POLICY_NO_CLAIM:
        missing = manifest.get("missing_evidence")
        if not isinstance(missing, list) or not missing or not all(isinstance(item, str) and item for item in missing):
            flags.append("no_release_claim target distill manifest requires non-empty missing_evidence")
        if manifest.get("distill_report") not in (None, ""):
            flags.append("no_release_claim target distill manifest must not cite a distill_report")
        notes.append("target/frontend distill performance remains unclaimed until retained target artifacts exist")
    else:
        distill_report_path_obj = manifest_artifact(root, manifest.get("distill_report"), "distill_report")
        distill_report_path = str(distill_report_path_obj.relative_to(root))
        loaded = load_json(distill_report_path_obj)
        if not isinstance(loaded, dict):
            flags.append("distill_report must be a JSON object")
        else:
            distill_report = loaded
            report_target = loaded.get("target") if isinstance(loaded.get("target"), dict) else {}
            if loaded.get("status") != "pass" or loaded.get("release_evidence") is not True:
                flags.append("target distill_report is not passing release evidence")
            flags.extend(proof_report_target_flags(report_target))
            if target.get("repo") and report_target.get("repo") != target.get("repo"):
                flags.append("target distill_report target.repo does not match manifest target.repo")
            if target.get("claim_scope") and report_target.get("claim_scope") != target.get("claim_scope"):
                flags.append("target distill_report target.claim_scope does not match manifest target.claim_scope")

    return {
        "schema": 1,
        "manifest": display_path(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "claim_policy": policy,
        "claimable_target_distill": policy == CLAIM_POLICY_PROOF and not flags,
        "target": target,
        "distill_report": distill_report_path,
        "distill_report_target": distill_report.get("target") if isinstance(distill_report.get("target"), dict) else {},
        "missing_evidence": manifest.get("missing_evidence") if isinstance(manifest.get("missing_evidence"), list) else [],
        "flags": flags,
        "notes": notes,
    }


def render_markdown(report: dict[str, Any]) -> str:
    target = report.get("target") if isinstance(report.get("target"), dict) else {}
    lines = [
        "# Target Distill Evidence Audit",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Release evidence: **{str(report['release_evidence']).lower()}**",
        f"- Claim policy: **{report.get('claim_policy')}**",
        f"- Claimable target distill: **{str(report.get('claimable_target_distill', False)).lower()}**",
        f"- Target repo: `{target.get('repo') or 'unset'}`",
        f"- Claim scope: `{target.get('claim_scope') or 'unset'}`",
        f"- Distill report: `{report.get('distill_report') or 'none'}`",
        "",
    ]
    if report.get("missing_evidence"):
        lines.extend(["## Missing Evidence", ""])
        for item in report["missing_evidence"]:
            lines.append(f"- {item}")
        lines.append("")
    if report.get("flags"):
        lines.extend(["## Flags", ""])
        for flag in report["flags"]:
            lines.append(f"- {flag}")
    else:
        lines.extend(["## Flags", "", "None."])
    if report.get("notes"):
        lines.extend(["", "## Notes", ""])
        for note in report["notes"]:
            lines.append(f"- {note}")
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / REPORT_JSON
    json_path.write_text(json.dumps(report, indent=2))
    (out_dir / REPORT_MD).write_text(render_markdown(report))
    return json_path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit target/frontend distill evidence status.")
    parser.add_argument("--manifest", type=pathlib.Path, required=True, help="Path to target-distill evidence manifest.json")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help=f"Output directory for {REPORT_JSON}/{REPORT_MD}")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when the retained target distill lane is invalid")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    report = audit_manifest(args.manifest)
    out_dir = (args.out_dir or args.manifest.parent).resolve()
    json_path = write_report(report, out_dir)
    print(render_markdown(report).split("\n## Flags", 1)[0].rstrip())
    print(f"\nWrote {json_path} and {REPORT_MD}")
    if args.fail_on_flags and report["status"] != "pass":
        print("\nTarget distill artifacts are not release evidence:", file=sys.stderr)
        for flag in report["flags"]:
            print(f"- {flag}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
