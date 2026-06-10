#!/usr/bin/env python3
"""Audit retained distill dry-run and timed-run artifacts for release claims."""
from __future__ import annotations

import argparse
import json
import pathlib
import sys
from typing import Any


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text())
    except FileNotFoundError as exc:
        raise SystemExit(f"artifact not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit(f"artifact is not valid JSON: {path}: {exc}") from exc


def get(d: Any, *path: str, default: Any = None) -> Any:
    cur = d
    for part in path:
        if not isinstance(cur, dict):
            return default
        cur = cur.get(part)
    return cur if cur is not None else default


def manifest_artifact(root: pathlib.Path, value: Any, field: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise SystemExit(f"distill perf manifest requires non-empty {field}")
    path = pathlib.Path(value)
    if path.is_absolute():
        raise SystemExit(f"distill perf manifest {field} must be relative, got {value}")
    return (root / path).resolve()


def positive_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and value > 0


def nonnegative_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and value >= 0


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("distill perf manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("distill perf manifest schema must be 1")
    for field in ("dry_run", "serial_run", "parallel_run"):
        if not isinstance(data.get(field), str) or not data.get(field):
            raise SystemExit(f"distill perf manifest requires {field}")


def config_tuple(record: dict[str, Any]) -> tuple[Any, ...]:
    return (
        record.get("agent"),
        record.get("model"),
        record.get("effort"),
        record.get("branch", ""),
        bool(record.get("force")),
        record.get("max_chunk_bytes"),
        record.get("confidence_threshold"),
    )


def audit_distill_perf_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)

    flags: list[str] = []
    notes: list[str] = []
    dry = load_json(manifest_artifact(root, manifest["dry_run"], "dry_run"))
    serial = load_json(manifest_artifact(root, manifest["serial_run"], "serial_run"))
    parallel = load_json(manifest_artifact(root, manifest["parallel_run"], "parallel_run"))

    if not isinstance(dry, dict) or not isinstance(serial, dict) or not isinstance(parallel, dict):
        raise SystemExit("distill perf artifacts must be JSON objects")

    if dry.get("schema_version") != 1:
        flags.append("dry_run: schema_version must be 1")
    for field in ("sessions", "sessions_to_distill", "chunks", "chunks_if_uncached", "raw_bytes", "preprocessed_bytes", "extraction_agent_calls", "estimated_agent_calls_upper_bound"):
        if not positive_number(dry.get(field)):
            flags.append(f"dry_run: {field} must be positive")
    if dry.get("chunks") != dry.get("extraction_agent_calls"):
        flags.append("dry_run: extraction_agent_calls must equal chunks")
    if dry.get("estimated_agent_calls_upper_bound", 0) < dry.get("extraction_agent_calls", 0):
        flags.append("dry_run: estimated_agent_calls_upper_bound must cover extraction calls")
    if not isinstance(dry.get("branches"), list) or not dry.get("branches"):
        flags.append("dry_run: branches must be non-empty")
    if not isinstance(dry.get("largest_sessions"), list) or not dry.get("largest_sessions"):
        flags.append("dry_run: largest_sessions must be non-empty")

    expected_config = config_tuple(dry)
    for label, run in (("serial_run", serial), ("parallel_run", parallel)):
        if config_tuple(run) != expected_config:
            flags.append(f"{label}: agent/model/branch/force/chunk/confidence config differs from dry_run")
        if run.get("jobs") is None or run.get("effective_extraction_jobs") is None:
            flags.append(f"{label}: jobs and effective_extraction_jobs are required")
        if not positive_number(run.get("total_seconds")):
            flags.append(f"{label}: total_seconds must be positive")
        for field in ("extraction_seconds", "reconcile_seconds", "write_seconds"):
            if not nonnegative_number(run.get(field)):
                flags.append(f"{label}: {field} must be non-negative")
        if not isinstance(run.get("extraction_agent_calls"), int) or run.get("extraction_agent_calls") <= 0:
            flags.append(f"{label}: extraction_agent_calls must be positive")
        if not isinstance(run.get("total_agent_calls"), int) or run.get("total_agent_calls") < run.get("extraction_agent_calls", 0):
            flags.append(f"{label}: total_agent_calls must cover extraction_agent_calls")
        if run.get("failed_chunks", 0) != 0:
            flags.append(f"{label}: failed_chunks must be 0")
        if run.get("preprocessed_bytes") != dry.get("preprocessed_bytes"):
            flags.append(f"{label}: preprocessed_bytes differs from dry_run")
        if run.get("chunks_scanned") != dry.get("chunks"):
            flags.append(f"{label}: chunks_scanned differs from dry_run chunks")
        if run.get("extraction_agent_calls") != dry.get("extraction_agent_calls"):
            flags.append(f"{label}: extraction_agent_calls differs from dry_run")

    if serial.get("jobs") != 1:
        flags.append("serial_run: jobs must be 1")
    if serial.get("effective_extraction_jobs") not in (0, 1):
        flags.append("serial_run: effective_extraction_jobs must be 1 or 0 when no work exists")
    if not isinstance(parallel.get("jobs"), int) or parallel.get("jobs") <= 1:
        flags.append("parallel_run: jobs must be greater than 1")
    if not isinstance(parallel.get("effective_extraction_jobs"), int) or parallel.get("effective_extraction_jobs") <= 1:
        flags.append("parallel_run: effective_extraction_jobs must be greater than 1")

    comparable_fields = ("facts", "distilled", "authored", "superseded", "proposals", "chunks_scanned", "chunks_distilled", "preprocessed_bytes", "extraction_agent_calls")
    for field in comparable_fields:
        if serial.get(field) != parallel.get(field):
            flags.append(f"serial/parallel mismatch: {field}")
    if sorted(serial.get("branches") or []) != sorted(parallel.get("branches") or []):
        flags.append("serial/parallel mismatch: branches")

    min_speedup = manifest.get("min_speedup", 1.0)
    if not isinstance(min_speedup, (int, float)) or min_speedup <= 1.0:
        raise SystemExit("distill perf manifest min_speedup must be a number greater than 1.0")
    speedup = None
    if positive_number(serial.get("total_seconds")) and positive_number(parallel.get("total_seconds")):
        speedup = float(serial["total_seconds"]) / float(parallel["total_seconds"])
        if speedup < float(min_speedup):
            flags.append(f"speedup {speedup:.3f} < required {float(min_speedup):.3f}")
    else:
        notes.append("speedup unavailable because one run lacks total_seconds")

    return {
        "schema": 1,
        "manifest": str(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "min_speedup": min_speedup,
        "speedup": speedup,
        "dry_run": {
            "sessions": dry.get("sessions"),
            "sessions_to_distill": dry.get("sessions_to_distill"),
            "chunks": dry.get("chunks"),
            "preprocessed_bytes": dry.get("preprocessed_bytes"),
            "estimated_agent_calls_upper_bound": dry.get("estimated_agent_calls_upper_bound"),
        },
        "serial_run": {
            "jobs": serial.get("jobs"),
            "effective_extraction_jobs": serial.get("effective_extraction_jobs"),
            "total_seconds": serial.get("total_seconds"),
            "total_agent_calls": serial.get("total_agent_calls"),
        },
        "parallel_run": {
            "jobs": parallel.get("jobs"),
            "effective_extraction_jobs": parallel.get("effective_extraction_jobs"),
            "total_seconds": parallel.get("total_seconds"),
            "total_agent_calls": parallel.get("total_agent_calls"),
        },
        "flags": flags,
        "notes": notes,
    }


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Distill Performance Evidence Audit",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Release evidence: **{str(report['release_evidence']).lower()}**",
        f"- Required speedup: **{report['min_speedup']}x**",
        f"- Observed speedup: **{report['speedup'] if report['speedup'] is not None else 'unavailable'}x**",
        f"- Dry-run chunks: **{get(report, 'dry_run', 'chunks', default='unset')}**",
        "",
        "## Flags",
        "",
    ]
    if report.get("flags"):
        lines.extend(f"- {flag}" for flag in report["flags"])
    else:
        lines.append("None.")
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "distill-perf-audit-report.json"
    json_path.write_text(json.dumps(report, indent=2))
    (out_dir / "distill-perf-audit-report.md").write_text(render_markdown(report))
    return json_path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit retained distill dry-run and timed-run performance artifacts.")
    parser.add_argument("--manifest", type=pathlib.Path, required=True, help="Path to distill performance evidence manifest.json")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Output directory for distill-perf-audit-report.{json,md}")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when the retained artifacts are not release evidence")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    report = audit_distill_perf_manifest(args.manifest)
    out_dir = (args.out_dir or args.manifest.parent).resolve()
    json_path = write_report(report, out_dir)
    print(render_markdown(report).split("\n## Flags", 1)[0].rstrip())
    print(f"\nWrote {json_path} and distill-perf-audit-report.md")
    if args.fail_on_flags and report["status"] != "pass":
        print("\nDistill performance artifacts are not release evidence:", file=sys.stderr)
        for flag in report["flags"]:
            print(f"- {flag}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
