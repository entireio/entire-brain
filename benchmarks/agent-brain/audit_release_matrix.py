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


def as_int(value: Any) -> int:
    """Tolerant int coercion for externally-supplied report fields: a real int
    (not bool) passes through, a numeric string is parsed, anything else (None,
    list, dict, non-numeric string) becomes 0. Keeps a malformed/tampered report
    from crashing the gate with a raw traceback instead of failing cleanly."""
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    try:
        return int(str(value).strip())
    except (TypeError, ValueError):
        return 0


def add_row(rows: list[dict[str, Any]], *, track: str, status: str, claimable: bool, evidence: str, detail: str, flags: list[str] | None = None) -> None:
    rows.append({
        "track": track,
        "status": status,
        "claimable": claimable,
        "evidence": evidence,
        "detail": detail,
        "flags": flags or [],
    })


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("release matrix manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("release matrix manifest schema must be 1")
    for field in ("repo_root", "reports", "docs", "mise"):
        if field not in data:
            raise SystemExit(f"release matrix manifest requires {field}")
    guardrails = data.get("required_press_guardrails", [])
    if guardrails is not None and (
        not isinstance(guardrails, list)
        or not all(isinstance(item, str) and item for item in guardrails)
    ):
        raise SystemExit("release matrix manifest required_press_guardrails must be a string list")


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
    facts = load_json(manifest_path(repo_root, reports.get("facts"), "reports.facts"))
    # Optional lanes: present in the real manifest, absent in minimal fixtures.
    # When a report is not referenced the corresponding track falls back to its
    # pre-registration (pending / not-added) form rather than failing the load.
    distill_large = (
        load_json(manifest_path(repo_root, reports.get("distill_large"), "reports.distill_large"))
        if reports.get("distill_large") else None
    )
    replay_lab_clean = (
        load_json(manifest_path(repo_root, reports.get("replay_lab_clean"), "reports.replay_lab_clean"))
        if reports.get("replay_lab_clean") else None
    )
    mise = load_toml(manifest_path(repo_root, manifest.get("mise"), "mise"))
    press_path = manifest_path(repo_root, docs.get("press_release"), "docs.press_release")
    press_text = press_path.read_text()

    rows: list[dict[str, Any]] = []

    release_totals = release.get("totals") if isinstance(release.get("totals"), dict) else {}
    scopes = release_totals.get("proof_ready_comparisons_by_scope") if isinstance(release_totals.get("proof_ready_comparisons_by_scope"), dict) else {}
    named_scopes = release_totals.get("named_tool_proof_ready_comparisons_by_scope") if isinstance(release_totals.get("named_tool_proof_ready_comparisons_by_scope"), dict) else {}
    release_gate = release.get("gate_status") if isinstance(release.get("gate_status"), dict) else {}
    release_claim_policy = release_gate.get("claim_policy") or (release.get("release_manifest") or {}).get("claim_policy") or "proof_required"
    release_flags: list[str] = []
    if release_claim_policy == "no_release_claim":
        if release_gate.get("status") != "pass":
            release_flags.append("release no-claim audit gate is not passing")
        if release_gate.get("release_evidence") is True:
            release_flags.append("release no-claim audit unexpectedly marks release_evidence true")
        if int(release_totals.get("proof_ready_comparisons") or 0) != 0:
            release_flags.append("release no-claim audit unexpectedly retains proof-ready comparisons")
        release_status = "no-claim"
        release_claimable = False
        if int(release_totals.get("hard_flags") or 0) == 0:
            release_detail = "B1 clean reruns are retained and audit-clean, but no comparison survived the proof-ready gate"
        else:
            release_detail = "B1 retained query-hint/task-hash confound is detected; clean replay-lab reruns are required before citing agent lift"
    else:
        for scope in manifest.get("required_release_proof_scopes", []):
            if int(scopes.get(scope) or 0) <= 0:
                release_flags.append(f"missing retained release proof scope {scope}")
        for scope in manifest.get("required_named_tool_proof_scopes", []):
            if int(named_scopes.get(scope) or 0) <= 0:
                release_flags.append(f"missing named-tool retained proof scope {scope}")
        if int(release_totals.get("hard_flags") or 0) != 0:
            release_flags.append("release audit has hard flags")
        release_status = "proven"
        release_claimable = not release_flags
        release_detail = f"proof scopes: {', '.join(sorted(scopes)) or 'none'}"
    add_row(
        rows,
        track="replay-lab retained agent proof",
        status=release_status if not release_flags else "invalid",
        claimable=release_claimable and not release_flags,
        evidence=display_path(manifest_path(repo_root, reports.get("release"), "reports.release")),
        detail=release_detail,
        flags=release_flags,
    )

    # Optional: only emit the clean-proof track when the lane is registered.
    if replay_lab_clean is not None:
        rlc_flags: list[str] = []
        rlc_gate = replay_lab_clean.get("gate_status") if isinstance(replay_lab_clean.get("gate_status"), dict) else {}
        rlc_totals = replay_lab_clean.get("totals") if isinstance(replay_lab_clean.get("totals"), dict) else {}
        rlc_scopes = rlc_totals.get("proof_ready_comparisons_by_scope") if isinstance(rlc_totals.get("proof_ready_comparisons_by_scope"), dict) else {}
        if rlc_gate.get("status") != "pass" or rlc_gate.get("release_evidence") is not True:
            rlc_flags.append("clean replay-lab proof report is not passing release evidence")
        if rlc_gate.get("claim_policy") != "proof_required":
            rlc_flags.append("clean replay-lab proof must use proof_required claim policy")
        if as_int(rlc_totals.get("proof_ready_comparisons")) < 1:
            rlc_flags.append("clean replay-lab proof has no proof-ready comparison")
        if as_int(rlc_scopes.get("history")) < 1:
            rlc_flags.append("clean replay-lab proof missing history-scope proof-ready comparison")
        # hard_flags is an integrity field: a malformed/non-int value must FAIL
        # closed (flag), not coerce-to-0 and silently pass — so as_int's
        # coerce-to-0 is deliberately NOT used here.
        hard_flags = rlc_totals.get("hard_flags")
        if not isinstance(hard_flags, int) or isinstance(hard_flags, bool) or hard_flags != 0:
            rlc_flags.append("clean replay-lab proof has hard integrity flags")
        add_row(
            rows,
            track="replay-lab clean correctness-axis agent lift (history channel)",
            status="proven" if not rlc_flags else "invalid",
            claimable=not rlc_flags,
            evidence=display_path(manifest_path(repo_root, reports.get("replay_lab_clean"), "reports.replay_lab_clean")),
            detail=f"history-channel correctness brain-lift: {as_int(rlc_scopes.get('history'))} proof-ready history comparison(s), brain_positive_stable (per-arm counts in the lane codex-audit-report); mcp/radar/codex scopes remain no_release_claim",
            flags=rlc_flags,
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
    if distill_large is not None:
        distill_large_flags: list[str] = []
        distill_large_target = distill_large.get("target") if isinstance(distill_large.get("target"), dict) else {}
        if distill_large.get("status") != "pass" or distill_large.get("release_evidence") is not True:
            distill_large_flags.append("large-repo distill evidence report is not passing release evidence")
        if "large-repo" not in str(distill_large_target.get("claim_scope") or ""):
            distill_large_flags.append("large-repo distill evidence is not scoped to a large-repo scheduler proof")
        add_row(
            rows,
            track="large-repo distill extraction-scheduler speedup",
            status="proven-local" if not distill_large_flags else "invalid",
            claimable=not distill_large_flags,
            evidence=display_path(manifest_path(repo_root, reports.get("distill_large"), "reports.distill_large")),
            detail=str(distill_large_target.get("claim_scope") or ""),
            flags=distill_large_flags,
        )
        add_row(
            rows,
            track="frontend/hosted-model distill latency and fact quality",
            status="pending-target-evidence",
            claimable=False,
            evidence=display_path(manifest_path(repo_root, reports.get("distill_large"), "reports.distill_large")),
            detail="retained large-repo speedup uses a deterministic command-agent for scheduler mechanics; hosted-model end-to-end latency and fact quality still need their own retained artifacts",
        )
    else:
        add_row(
            rows,
            track="target large-repo/frontend distill performance",
            status="pending-target-evidence",
            claimable=False,
            evidence=display_path(manifest_path(repo_root, reports.get("distill"), "reports.distill")),
            detail="current retained speedup is current-repo command-agent scheduler proof, not the frontend/large-repo claim",
        )

    facts_flags: list[str] = []
    facts_policy = facts.get("claim_policy")
    facts_claimable = facts.get("claimable_facts_vs_raw") is True
    if facts.get("status") != "pass" or facts.get("release_evidence") is not True:
        facts_flags.append("facts evidence report is not passing")
    if facts_policy == "no_release_claim":
        if facts_claimable:
            facts_flags.append("no_release_claim facts report unexpectedly marks facts-vs-raw claimable")
        facts_status = "no-claim"
        facts_row_claimable = False
        facts_detail = "paired proof-labeled facts/history/query/raw-sessions eval still required"
    elif facts_policy == "proof_required":
        if facts.get("claim_scope") != "release":
            facts_flags.append("proof_required facts evidence must have release claim_scope")
        if not facts_claimable:
            facts_flags.append("proof_required facts evidence must mark facts-vs-raw claimable")
        facts_status = "proven"
        facts_row_claimable = not facts_flags
        facts_detail = "paired proof-labeled facts/history/query/raw-sessions eval passed"
    else:
        facts_flags.append("facts evidence claim_policy must be no_release_claim or proof_required")
        facts_status = "invalid"
        facts_row_claimable = False
        facts_detail = "facts evidence claim_policy is invalid"
    add_row(
        rows,
        track="facts vs raw/session retrieval quality",
        status=facts_status if not facts_flags else "invalid",
        claimable=facts_row_claimable,
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
        claimable=False,
        evidence=display_path(manifest_path(repo_root, manifest.get("mise"), "mise")),
        detail="freshness must be verified by the live semantic:evidence gate on the final clean release checkout",
        flags=semantic_flags,
    )
    add_row(
        rows,
        track="semantic usefulness benchmark claim",
        status="pending-retained-proof",
        claimable=False,
        evidence=display_path(manifest_path(repo_root, reports.get("release"), "reports.release")),
        detail="release evidence has no semantic proof-ready scope; semantic usefulness remains unclaimed",
    )

    press_flags: list[str] = []
    for required in ("entire-brain", "entire-graph", "entire-replay-lab", "Future Claims We Should Not Make Yet", "Release Checklist"):
        if required not in press_text:
            press_flags.append(f"release press release missing {required!r}")
    press_text_lower = press_text.lower()
    for required in manifest.get("required_press_guardrails", []):
        if required.lower() not in press_text_lower:
            press_flags.append(f"release press release missing guardrail phrase {required!r}")
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
            "release_fully_ready remains false while frontend/hosted-model distill latency, facts-vs-raw, semantic usefulness, workspace Radar, and broader replay proof are pending.",
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
