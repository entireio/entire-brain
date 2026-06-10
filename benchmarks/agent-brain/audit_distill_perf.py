#!/usr/bin/env python3
"""Audit retained distill dry-run and timed-run artifacts for release claims."""
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from typing import Any


COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_VALUE_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text())
    except FileNotFoundError as exc:
        raise SystemExit(f"artifact not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit(f"artifact is not valid JSON: {path}: {exc}") from exc


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            digest.update(chunk)
    return "sha256:" + digest.hexdigest()


def display_path(path: pathlib.Path) -> str:
    resolved = path.resolve()
    try:
        return str(resolved.relative_to(pathlib.Path.cwd().resolve()))
    except ValueError:
        return str(resolved)


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
    target = data.get("target")
    if not isinstance(target, dict):
        raise SystemExit("distill perf manifest requires target")
    for field in ("dry_run", "serial_run", "parallel_run"):
        if not isinstance(data.get(field), str) or not data.get(field):
            raise SystemExit(f"distill perf manifest requires {field}")
    hashes = data.get("artifact_sha256")
    if not isinstance(hashes, dict):
        raise SystemExit("distill perf manifest requires artifact_sha256")
    commands = data.get("commands")
    if not isinstance(commands, dict):
        raise SystemExit("distill perf manifest requires commands")


def validate_target(manifest: dict[str, Any], flags: list[str]) -> dict[str, Any]:
    target = manifest.get("target")
    if not isinstance(target, dict):
        flags.append("target must be an object")
        return {}
    for field in ("repo", "repo_key", "claim_scope"):
        if not isinstance(target.get(field), str) or not target.get(field).strip():
            flags.append(f"target.{field} must be a non-empty string")
    source_head = target.get("source_head")
    if not isinstance(source_head, str) or not COMMIT_RE.fullmatch(source_head):
        flags.append("target.source_head must be a 40-character git commit")
    brain_sha = target.get("brain_manifest_sha256")
    if not isinstance(brain_sha, str) or not SHA256_VALUE_RE.fullmatch(brain_sha):
        flags.append("target.brain_manifest_sha256 must be sha256:<64 hex>")
    return {
        "repo": target.get("repo"),
        "repo_key": target.get("repo_key"),
        "source_head": source_head,
        "brain_manifest_sha256": brain_sha,
        "claim_scope": target.get("claim_scope"),
    }


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


def command_tokens(manifest: dict[str, Any], key: str, flags: list[str]) -> list[str]:
    tokens = get(manifest, "commands", key)
    if not isinstance(tokens, list) or not all(isinstance(t, str) and t for t in tokens):
        flags.append(f"commands.{key} must be a non-empty string array")
        return []
    return tokens


def flag_value(tokens: list[str], name: str) -> str | None:
    for index, token in enumerate(tokens):
        if token == name:
            if index + 1 >= len(tokens):
                return ""
            return tokens[index + 1]
        prefix = name + "="
        if token.startswith(prefix):
            return token[len(prefix):]
    return None


def require_flag(tokens: list[str], name: str, label: str, flags: list[str]) -> None:
    if name not in tokens and not any(token.startswith(name + "=") for token in tokens):
        flags.append(f"{label}: retained command must include {name}")


def validate_command(key: str, tokens: list[str], record: dict[str, Any], flags: list[str], *, dry_run: bool) -> None:
    label = f"commands.{key}"
    if not tokens:
        return
    if "distill" not in tokens:
        flags.append(f"{label}: retained command must invoke distill")
    require_flag(tokens, "--json", label, flags)
    if dry_run:
        require_flag(tokens, "--dry-run", label, flags)
    agent = record.get("agent")
    if agent in ("", None, "auto"):
        flags.append(f"{key}: agent must be explicit, not {agent!r}")
    if flag_value(tokens, "--agent") != agent:
        flags.append(f"{label}: --agent must match artifact agent")
    if record.get("model") and flag_value(tokens, "--model") != record.get("model"):
        flags.append(f"{label}: --model must match artifact model")
    if record.get("effort") and flag_value(tokens, "--effort") != record.get("effort"):
        flags.append(f"{label}: --effort must match artifact effort")
    if record.get("branch") and flag_value(tokens, "--branch") != record.get("branch"):
        flags.append(f"{label}: --branch must match artifact branch")
    if record.get("force") is True:
        require_flag(tokens, "--force", label, flags)
    if str(record.get("jobs")) != str(flag_value(tokens, "--jobs")):
        flags.append(f"{label}: --jobs must match artifact jobs")
    if str(record.get("max_chunk_bytes")) != str(flag_value(tokens, "--max-chunk-bytes")):
        flags.append(f"{label}: --max-chunk-bytes must match artifact max_chunk_bytes")
    if str(record.get("confidence_threshold")) != str(flag_value(tokens, "--confidence")):
        flags.append(f"{label}: --confidence must match artifact confidence_threshold")


def validate_artifact_hashes(manifest: dict[str, Any], paths: dict[str, pathlib.Path], flags: list[str]) -> None:
    for key, path in paths.items():
        expected = get(manifest, "artifact_sha256", key)
        if not isinstance(expected, str) or not expected.startswith("sha256:"):
            flags.append(f"artifact_sha256.{key} must be present")
            continue
        actual = file_sha256(path)
        if actual != expected:
            flags.append(f"artifact_sha256.{key} mismatch")


def validate_branch_sums(dry: dict[str, Any], flags: list[str]) -> None:
    branches = dry.get("branches")
    if not isinstance(branches, list) or not branches:
        return
    for field in ("sessions", "cached_sessions", "sessions_to_distill", "chunks", "chunks_if_uncached", "preprocessed_bytes"):
        total = sum(branch.get(field, 0) for branch in branches if isinstance(branch, dict))
        if total != dry.get(field):
            flags.append(f"dry_run: branch {field} sum {total} != total {dry.get(field)}")


def audit_distill_perf_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)

    flags: list[str] = []
    notes: list[str] = []
    paths = {
        "dry_run": manifest_artifact(root, manifest["dry_run"], "dry_run"),
        "serial_run": manifest_artifact(root, manifest["serial_run"], "serial_run"),
        "parallel_run": manifest_artifact(root, manifest["parallel_run"], "parallel_run"),
    }
    target = validate_target(manifest, flags)
    validate_artifact_hashes(manifest, paths, flags)
    dry = load_json(paths["dry_run"])
    serial = load_json(paths["serial_run"])
    parallel = load_json(paths["parallel_run"])

    if not isinstance(dry, dict) or not isinstance(serial, dict) or not isinstance(parallel, dict):
        raise SystemExit("distill perf artifacts must be JSON objects")

    if dry.get("schema_version") != 1:
        flags.append("dry_run: schema_version must be 1")
    for field in ("sessions", "sessions_to_distill", "chunks", "chunks_if_uncached", "raw_bytes", "preprocessed_bytes", "extraction_agent_calls", "estimated_agent_calls_upper_bound", "max_chunk_bytes"):
        if not positive_number(dry.get(field)):
            flags.append(f"dry_run: {field} must be positive")
    if dry.get("missing_transcripts") != 0:
        flags.append("dry_run: missing_transcripts must be 0")
    if dry.get("warnings"):
        flags.append("dry_run: warnings must be empty")
    if dry.get("chunks") != dry.get("extraction_agent_calls"):
        flags.append("dry_run: extraction_agent_calls must equal chunks")
    if dry.get("estimated_agent_calls_upper_bound") != dry.get("extraction_agent_calls", 0) + dry.get("reconcile_agent_calls_upper_bound", 0):
        flags.append("dry_run: estimated_agent_calls_upper_bound must equal extraction + reconcile upper bound")
    if not isinstance(dry.get("branches"), list) or not dry.get("branches"):
        flags.append("dry_run: branches must be non-empty")
    if not isinstance(dry.get("largest_sessions"), list) or not dry.get("largest_sessions"):
        flags.append("dry_run: largest_sessions must be non-empty")
    validate_branch_sums(dry, flags)

    dry_tokens = command_tokens(manifest, "dry_run", flags)
    serial_tokens = command_tokens(manifest, "serial_run", flags)
    parallel_tokens = command_tokens(manifest, "parallel_run", flags)
    validate_command("dry_run", dry_tokens, dry, flags, dry_run=True)
    validate_command("serial_run", serial_tokens, serial, flags, dry_run=False)
    validate_command("parallel_run", parallel_tokens, parallel, flags, dry_run=False)
    if not (dry.get("force") and serial.get("force") and parallel.get("force")) and manifest.get("cache_state") != "cleared_before_each_run":
        flags.append("cache_state: non-force evidence requires cache_state=cleared_before_each_run")

    expected_config = config_tuple(dry)
    for label, run in (("serial_run", serial), ("parallel_run", parallel)):
        if config_tuple(run) != expected_config:
            flags.append(f"{label}: agent/model/branch/force/chunk/confidence config differs from dry_run")
        if run.get("jobs") is None or run.get("effective_extraction_jobs") is None:
            flags.append(f"{label}: jobs and effective_extraction_jobs are required")
        if not positive_number(run.get("max_chunk_bytes")):
            flags.append(f"{label}: max_chunk_bytes must be positive")
        if not positive_number(run.get("total_seconds")):
            flags.append(f"{label}: total_seconds must be positive")
        for field in ("extraction_seconds", "reconcile_seconds", "write_seconds"):
            if not nonnegative_number(run.get(field)):
                flags.append(f"{label}: {field} must be non-negative")
        component_seconds = sum(float(run.get(field) or 0) for field in ("extraction_seconds", "reconcile_seconds", "write_seconds"))
        if positive_number(run.get("total_seconds")) and component_seconds > float(run["total_seconds"]) + 0.01:
            flags.append(f"{label}: timing components exceed total_seconds")
        if not isinstance(run.get("extraction_agent_calls"), int) or run.get("extraction_agent_calls") <= 0:
            flags.append(f"{label}: extraction_agent_calls must be positive")
        if run.get("total_agent_calls") != run.get("extraction_agent_calls", 0) + run.get("reconcile_agent_calls", 0):
            flags.append(f"{label}: total_agent_calls must equal extraction + reconcile calls")
        if run.get("reconcile_agent_calls", 0) > dry.get("reconcile_agent_calls_upper_bound", 0):
            flags.append(f"{label}: reconcile_agent_calls exceeds dry-run upper bound")
        if int(run.get("cache_hits") or 0) != int(dry.get("cached_sessions") or 0):
            flags.append(f"{label}: cache_hits differs from dry_run cached_sessions")
        if run.get("failed_chunks", 0) != 0:
            flags.append(f"{label}: failed_chunks must be 0")
        if run.get("warnings"):
            flags.append(f"{label}: warnings must be empty")
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
        "manifest": display_path(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "target": target,
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
        f"- Target: **{get(report, 'target', 'repo', default='unset')}**",
        f"- Claim scope: **{get(report, 'target', 'claim_scope', default='unset')}**",
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
