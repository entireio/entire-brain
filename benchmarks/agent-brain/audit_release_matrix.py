#!/usr/bin/env python3
"""Build and audit the release-readiness evidence matrix.

The matrix is a claim-hygiene gate. It does not declare the product fully
release-ready. It verifies that every release-readiness ask is either backed by
retained evidence or explicitly retained as no-claim / pending evidence, so docs
and CI cannot accidentally promote an unsupported claim.
"""
from __future__ import annotations

import argparse
import json
import pathlib
import sys
import tomllib
from typing import Any


REPORT_JSON = "release-matrix-report.json"
REPORT_MD = "release-matrix-report.md"


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


def manifest_path(base: pathlib.Path, value: Any, field: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise SystemExit(f"release matrix manifest requires non-empty {field}")
    path = pathlib.Path(value)
    if path.is_absolute():
        raise SystemExit(f"release matrix manifest {field} must be relative, got {value}")
    return (base / path).resolve()


def load_toml(path: pathlib.Path) -> dict[str, Any]:
    try:
        with path.open("rb") as f:
            data = tomllib.load(f)
    except FileNotFoundError as exc:
        raise SystemExit(f"artifact not found: {path}") from exc
    except tomllib.TOMLDecodeError as exc:
        raise SystemExit(f"artifact is not valid TOML: {path}: {exc}") from exc
    return data if isinstance(data, dict) else {}


def task_defined(mise: dict[str, Any], name: str) -> bool:
    tasks = mise.get("tasks")
    return isinstance(tasks, dict) and isinstance(tasks.get(name), dict)


def task_run(mise: dict[str, Any], name: str) -> str:
    tasks = mise.get("tasks")
    if not isinstance(tasks, dict) or not isinstance(tasks.get(name), dict):
        return ""
    value = tasks[name].get("run")
    return value if isinstance(value, str) else ""


def add_row(rows: list[dict[str, Any]], *, track: str, status: str, claimable: bool, evidence: str, detail: str, flags: list[str] | None = None) -> None:
    rows.append({
        "track": track,
        "status": status,
        "claimable": claimable,
        "evidence": evidence,
        "detail": detail,
        "flags": flags or [],
    })


FACTS_PROOF_CONTRACT = {
    "required_retrievers": ["facts", "history", "query", "raw-sessions"],
    "required_comparison": "raw_vs_facts",
    "a_retriever": "raw-sessions",
    "b_retriever": "facts",
    "required_metric": "useful_per_1k",
    "required_winner": "b",
    "required_evidence_basis": "proof_labels",
    "require_release_pairing_ready": True,
    "require_no_proxy": True,
    "require_same_tasks_sha256": True,
    "require_same_brain_manifest_sha256": True,
    "require_facts_status_ready": True,
    "require_include_ids": True,
    "require_retained_tasks_artifact": True,
}


def validate_facts_proof_contract(facts: dict[str, Any], flags: list[str]) -> None:
    if facts.get("proof_contract") != FACTS_PROOF_CONTRACT:
        flags.append("facts proof_contract must match the release facts-vs-raw proof contract")
    if facts.get("required_retrievers") != FACTS_PROOF_CONTRACT["required_retrievers"]:
        flags.append("facts proof must retain the canonical facts/history/query/raw-sessions retriever arms")
    tasks_artifact = facts.get("tasks_artifact") if isinstance(facts.get("tasks_artifact"), dict) else {}
    if not tasks_artifact.get("path") or not tasks_artifact.get("sha256"):
        flags.append("facts proof must retain a hashed tasks_artifact")
    elif tasks_artifact.get("sha256") != facts.get("tasks_sha256"):
        flags.append("facts proof tasks_artifact sha256 must match tasks_sha256")
    status = facts.get("facts_status") if isinstance(facts.get("facts_status"), dict) else {}
    totals = status.get("totals") if isinstance(status.get("totals"), dict) else {}
    if status.get("facts_arm_ready") is not True:
        flags.append("facts proof requires facts_status.facts_arm_ready true")
    if int(totals.get("active") or 0) <= 0:
        flags.append("facts proof requires facts_status.totals.active > 0")
    claims = facts.get("required_claims")
    if not isinstance(claims, list):
        flags.append("facts proof required_claims must be a list")
        return
    for claim in claims:
        if not isinstance(claim, dict):
            continue
        if (
            claim.get("comparison") == FACTS_PROOF_CONTRACT["required_comparison"]
            and claim.get("metric") == FACTS_PROOF_CONTRACT["required_metric"]
            and claim.get("a_retriever") == FACTS_PROOF_CONTRACT["a_retriever"]
            and claim.get("b_retriever") == FACTS_PROOF_CONTRACT["b_retriever"]
            and claim.get("winner") == FACTS_PROOF_CONTRACT["required_winner"]
            and claim.get("evidence_basis") == FACTS_PROOF_CONTRACT["required_evidence_basis"]
            and claim.get("release_claimable") is True
            and claim.get("significant") is True
            and int(claim.get("retained_n") or 0) > 0
            and claim.get("retained_release_claimable") is True
        ):
            return
    flags.append("facts proof must include a release-claimable raw-sessions vs facts useful_per_1k proof-label win")


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("release matrix manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("release matrix manifest schema must be 1")
    for field in ("repo_root", "reports", "docs", "mise"):
        if field not in data:
            raise SystemExit(f"release matrix manifest requires {field}")


def audit_manifest(manifest_file: pathlib.Path) -> dict[str, Any]:
    manifest_file = manifest_file.resolve()
    manifest = load_json(manifest_file)
    validate_manifest(manifest)
    manifest_dir = manifest_file.parent
    repo_root = manifest_path(manifest_dir, manifest["repo_root"], "repo_root")

    reports = manifest["reports"]
    docs = manifest["docs"]
    if not isinstance(reports, dict):
        raise SystemExit("release matrix manifest reports must be an object")
    if not isinstance(docs, dict):
        raise SystemExit("release matrix manifest docs must be an object")

    release = load_json(manifest_path(repo_root, reports.get("release"), "reports.release"))
    radar_tool = load_json(manifest_path(repo_root, reports.get("radar_tool"), "reports.radar_tool"))
    workspace = load_json(manifest_path(repo_root, reports.get("workspace_radar"), "reports.workspace_radar"))
    distill = load_json(manifest_path(repo_root, reports.get("distill"), "reports.distill"))
    target_distill = load_json(manifest_path(repo_root, reports.get("target_distill"), "reports.target_distill"))
    facts = load_json(manifest_path(repo_root, reports.get("facts"), "reports.facts"))
    mise = load_toml(manifest_path(repo_root, manifest.get("mise"), "mise"))
    press_path = manifest_path(repo_root, docs.get("press_release"), "docs.press_release")
    press_text = press_path.read_text()

    rows: list[dict[str, Any]] = []

    release_totals = release.get("totals") if isinstance(release.get("totals"), dict) else {}
    scopes = release_totals.get("proof_ready_comparisons_by_scope") if isinstance(release_totals.get("proof_ready_comparisons_by_scope"), dict) else {}
    named_scopes = release_totals.get("named_tool_proof_ready_comparisons_by_scope") if isinstance(release_totals.get("named_tool_proof_ready_comparisons_by_scope"), dict) else {}
    release_flags: list[str] = []
    for scope in manifest.get("required_release_proof_scopes", []):
        if int(scopes.get(scope) or 0) <= 0:
            release_flags.append(f"missing retained release proof scope {scope}")
    for scope in manifest.get("required_named_tool_proof_scopes", []):
        if int(named_scopes.get(scope) or 0) <= 0:
            release_flags.append(f"missing named-tool retained proof scope {scope}")
    if int(release_totals.get("hard_flags") or 0) != 0:
        release_flags.append("release audit has hard flags")
    add_row(
        rows,
        track="replay-lab retained agent proof",
        status="proven" if not release_flags else "invalid",
        claimable=not release_flags,
        evidence=display_path(manifest_path(repo_root, reports.get("release"), "reports.release")),
        detail=f"proof scopes: {', '.join(sorted(scopes)) or 'none'}",
        flags=release_flags,
    )

    radar_flags: list[str] = []
    if radar_tool.get("ok") is not True:
        radar_flags.append("radar tool contract report is not ok")
    if radar_tool.get("claim_scope") != "mcp_radar_tool_contract":
        radar_flags.append("radar tool claim_scope mismatch")
    radar_claims = radar_tool.get("claims") if isinstance(radar_tool.get("claims"), list) else []
    radar_tests = radar_tool.get("required_tests") if isinstance(radar_tool.get("required_tests"), list) else []
    radar_claim_text = "\n".join(str(item) for item in [*radar_claims, *radar_tests])
    if "QMD" not in radar_claim_text and "brain_search" not in radar_claim_text:
        radar_flags.append("radar tool report does not retain QMD/MCP retrieval claim")
    add_row(
        rows,
        track="QMD-inspired MCP/Radar tool contract",
        status="proven" if not radar_flags else "invalid",
        claimable=not radar_flags,
        evidence=display_path(manifest_path(repo_root, reports.get("radar_tool"), "reports.radar_tool")),
        detail="deterministic MCP/Radar Go test artifact",
        flags=radar_flags,
    )

    distill_flags: list[str] = []
    distill_target = distill.get("target") if isinstance(distill.get("target"), dict) else {}
    if distill.get("status") != "pass" or distill.get("release_evidence") is not True:
        distill_flags.append("distill evidence report is not passing release evidence")
    if "current-repo" not in str(distill_target.get("claim_scope") or ""):
        distill_flags.append("distill evidence is not scoped to current-repo scheduler proof")
    add_row(
        rows,
        track="distill local scheduler/backfill mechanics",
        status="proven-local" if not distill_flags else "invalid",
        claimable=not distill_flags,
        evidence=display_path(manifest_path(repo_root, reports.get("distill"), "reports.distill")),
        detail=str(distill_target.get("claim_scope") or ""),
        flags=distill_flags,
    )
    target_distill_flags: list[str] = []
    target_distill_claimable = False
    target_distill_status = "pending-target-evidence"
    target_distill_detail = "target frontend/large-repo dry-run and paired timed artifacts still required"
    if target_distill.get("status") != "pass" or target_distill.get("release_evidence") is not True:
        target_distill_flags.append("target distill evidence report is not passing")
    target_distill_policy = target_distill.get("claim_policy")
    if target_distill_policy == "no_release_claim":
        if target_distill.get("claimable_target_distill") is not False:
            target_distill_flags.append("target distill no-claim report unexpectedly marks target claimable")
    elif target_distill_policy == "proof_required":
        if target_distill.get("claimable_target_distill") is not True:
            target_distill_flags.append("target distill proof report must mark target claimable")
        report_target = target_distill.get("distill_report_target") if isinstance(target_distill.get("distill_report_target"), dict) else {}
        if "current-repo" in str(report_target.get("claim_scope") or ""):
            target_distill_flags.append("target distill proof must not cite the current-repo scheduler claim_scope")
        target_distill_claimable = not target_distill_flags
        target_distill_status = "proven" if target_distill_claimable else "invalid"
        target_distill_detail = str(report_target.get("claim_scope") or target_distill_detail)
    else:
        target_distill_flags.append("target distill claim_policy must be no_release_claim or proof_required")
    add_row(
        rows,
        track="target large-repo/frontend distill performance",
        status=target_distill_status if (not target_distill_flags or target_distill_policy == "proof_required") else "invalid",
        claimable=target_distill_claimable,
        evidence=display_path(manifest_path(repo_root, reports.get("target_distill"), "reports.target_distill")),
        detail=target_distill_detail,
        flags=target_distill_flags,
    )

    facts_flags: list[str] = []
    facts_claimable = False
    facts_status = "no-claim"
    facts_detail = "paired proof-labeled facts/history/query/raw-sessions eval still required"
    if facts.get("status") != "pass" or facts.get("release_evidence") is not True:
        facts_flags.append("facts evidence report is not passing")
    facts_policy = facts.get("claim_policy")
    if facts_policy == "no_release_claim":
        if facts.get("claimable_facts_vs_raw") is not False:
            facts_flags.append("facts no-claim report unexpectedly marks facts-vs-raw claimable")
    elif facts_policy == "proof_required":
        validate_facts_proof_contract(facts, facts_flags)
        if facts.get("claimable_facts_vs_raw") is not True:
            facts_flags.append("facts proof report must mark facts-vs-raw claimable")
        facts_claimable = not facts_flags
        facts_status = "proven" if facts_claimable else "invalid"
        facts_detail = "proof-labeled facts beat raw-sessions on useful_per_1k"
    else:
        facts_flags.append("facts evidence claim_policy must be no_release_claim or proof_required")
    add_row(
        rows,
        track="facts vs raw/session retrieval quality",
        status=facts_status if (not facts_flags or facts_policy == "proof_required") else "invalid",
        claimable=facts_claimable,
        evidence=display_path(manifest_path(repo_root, reports.get("facts"), "reports.facts")),
        detail=facts_detail,
        flags=facts_flags,
    )

    workspace_flags: list[str] = []
    workspace_summary = workspace.get("summary") if isinstance(workspace.get("summary"), dict) else {}
    if workspace.get("status") != "pass" or workspace.get("release_evidence") is not True:
        workspace_flags.append("workspace Radar evidence report is not passing")
    if workspace.get("claim_policy") != "no_release_claim":
        workspace_flags.append("workspace Radar must remain no_release_claim until proof-ready workspace evidence exists")
    if workspace.get("claimable_workspace_radar") is not False:
        workspace_flags.append("workspace Radar report unexpectedly marks the claim claimable")
    if int(workspace_summary.get("proof_ready") or 0) != 0:
        workspace_flags.append("workspace Radar no-claim lane unexpectedly has proof-ready comparisons")
    add_row(
        rows,
        track="workspace Radar agent lift",
        status="no-claim" if not workspace_flags else "invalid",
        claimable=False,
        evidence=display_path(manifest_path(repo_root, reports.get("workspace_radar"), "reports.workspace_radar")),
        detail="latest retained candidate is no-brain-too-easy; stronger task/runner still needed",
        flags=workspace_flags,
    )

    semantic_flags: list[str] = []
    if not task_defined(mise, "semantic:evidence"):
        semantic_flags.append("mise task semantic:evidence is missing")
    if "semantic:evidence" not in task_run(mise, "release:readiness"):
        semantic_flags.append("release:readiness does not run semantic:evidence")
    add_row(
        rows,
        track="semantic freshness/audit health",
        status="local-gated" if not semantic_flags else "invalid",
        claimable=not semantic_flags,
        evidence=display_path(manifest_path(repo_root, manifest.get("mise"), "mise")),
        detail="freshness is verified by the live semantic:evidence gate on the current checkout",
        flags=semantic_flags,
    )
    semantic_usefulness_flags: list[str] = []
    if int(scopes.get("semantic") or 0) <= 0:
        semantic_usefulness_flags.append("release evidence has no semantic proof-ready scope")
    add_row(
        rows,
        track="semantic usefulness benchmark claim",
        status="proven" if not semantic_usefulness_flags else "pending-retained-proof",
        claimable=not semantic_usefulness_flags,
        evidence=display_path(manifest_path(repo_root, reports.get("release"), "reports.release")),
        detail=(
            "retained semantic proof-ready benchmark shows semantic_brain lift"
            if not semantic_usefulness_flags
            else "release evidence has no semantic proof-ready scope; semantic usefulness remains unclaimed"
        ),
        flags=semantic_usefulness_flags,
    )

    press_flags: list[str] = []
    for required in ("entire-brain", "entire-sem", "entire-replay-lab", "Future Claims We Should Not Make Yet", "Release Checklist"):
        if required not in press_text:
            press_flags.append(f"release press release missing {required!r}")
    add_row(
        rows,
        track="release narrative / backwards-working story",
        status="documented" if not press_flags else "invalid",
        claimable=not press_flags,
        evidence=display_path(press_path),
        detail="press-release doc names shipped, proven, pending, blocked, and future claims",
        flags=press_flags,
    )

    required_tasks = manifest.get("required_mise_tasks") if isinstance(manifest.get("required_mise_tasks"), list) else []
    readiness_flags: list[str] = []
    readiness_run = task_run(mise, "release:readiness")
    for task in required_tasks:
        if not task_defined(mise, str(task)):
            readiness_flags.append(f"mise task {task} is missing")
        if str(task) != "release:readiness" and str(task) not in readiness_run:
            readiness_flags.append(f"release:readiness does not run {task}")
    add_row(
        rows,
        track="single local release-readiness gate",
        status="gated" if not readiness_flags else "invalid",
        claimable=not readiness_flags,
        evidence=display_path(manifest_path(repo_root, manifest.get("mise"), "mise")),
        detail="release:readiness runs check plus retained evidence gates",
        flags=readiness_flags,
    )

    flags = [flag for row in rows for flag in row["flags"]]
    claimable = [row["track"] for row in rows if row["claimable"]]
    no_claim = [row["track"] for row in rows if not row["claimable"]]
    return {
        "schema": 1,
        "manifest": display_path(manifest_file),
        "status": "fail" if flags else "pass",
        "release_fully_ready": False,
        "summary": {
            "tracks": len(rows),
            "claimable_tracks": len(claimable),
            "no_claim_or_pending_tracks": len(no_claim),
            "flags": len(flags),
        },
        "rows": rows,
        "flags": flags,
        "notes": [
            "This is a claim-hygiene gate, not a declaration that every release blocker is closed.",
            "release_fully_ready remains false while target large-repo distill, facts-vs-raw, workspace Radar, and broader replay proof are pending.",
        ],
    }


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Release Readiness Matrix",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Release fully ready: **{str(report['release_fully_ready']).lower()}**",
        f"- Tracks: **{report['summary']['tracks']}**",
        f"- Claimable tracks: **{report['summary']['claimable_tracks']}**",
        f"- No-claim or pending tracks: **{report['summary']['no_claim_or_pending_tracks']}**",
    ]
    if report["flags"]:
        lines.extend(["", "## Errors"])
        lines.extend(f"- {flag}" for flag in report["flags"])
    if report["notes"]:
        lines.extend(["", "## Notes"])
        lines.extend(f"- {note}" for note in report["notes"])
    lines.extend(["", "## Matrix", ""])
    lines.append("| Track | Status | Claimable | Evidence | Detail |")
    lines.append("|---|---|---:|---|---|")
    for row in report["rows"]:
        lines.append(
            "| "
            + " | ".join([
                str(row["track"]),
                str(row["status"]),
                str(row["claimable"]).lower(),
                str(row["evidence"]),
                str(row["detail"]).replace("\n", " "),
            ])
            + " |"
        )
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> None:
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / REPORT_JSON).write_text(json.dumps(report, indent=2, sort_keys=True))
    (out_dir / REPORT_MD).write_text(render_markdown(report))


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit the retained release-readiness evidence matrix.")
    parser.add_argument("--manifest", type=pathlib.Path, required=True, help="Release matrix manifest")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Directory for release-matrix-report.{json,md}")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero if matrix evidence is inconsistent")
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
