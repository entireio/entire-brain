#!/usr/bin/env python3
"""Record retained deterministic MCP / Regression Radar tool evidence."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import pathlib
import shlex
import subprocess

import audit_radar_tool_evidence as audit


def run(repo_root: pathlib.Path, args: list[str], **kwargs):
    return subprocess.run(args, cwd=repo_root, text=True, check=True, **kwargs)


def git_output(repo_root: pathlib.Path, *args: str) -> str:
    return run(repo_root, ["git", *args], stdout=subprocess.PIPE).stdout.strip()


def require_clean_worktree(repo_root: pathlib.Path) -> None:
    status = git_output(repo_root, "status", "--porcelain")
    if status:
        raise SystemExit("refusing to record Radar tool evidence from a dirty worktree")


def test_regex() -> str:
    return "^(" + "|".join(audit.REQUIRED_TESTS) + ")$"


def load_manifest(path: pathlib.Path) -> dict:
    if not path.exists():
        return {}
    with path.open() as f:
        data = json.load(f)
    if not isinstance(data, dict):
        raise SystemExit("existing manifest must be a JSON object")
    return data


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=pathlib.Path, default=pathlib.Path("."))
    parser.add_argument(
        "--evidence-dir",
        type=pathlib.Path,
        default=pathlib.Path("benchmarks/agent-brain/evidence/radar-tool"),
    )
    args = parser.parse_args(argv)

    repo_root = args.repo_root.resolve()
    evidence_dir = (repo_root / args.evidence_dir).resolve()
    require_clean_worktree(repo_root)
    evidence_dir.mkdir(parents=True, exist_ok=True)

    artifact = evidence_dir / "go-test-internal-cli-radar.jsonl"
    regex = test_regex()
    command = ["go", "test", "-json", "./internal/cli", "-run", regex]
    with artifact.open("w") as f:
        run(repo_root, command, stdout=f)

    manifest_path = evidence_dir / "manifest.json"
    manifest = load_manifest(manifest_path)
    manifest.update({
        "schema": 1,
        "claim_scope": audit.CLAIM_SCOPE,
        "source_head": git_output(repo_root, "rev-parse", "HEAD"),
        "generated_at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "required_tests": audit.REQUIRED_TESTS,
        "artifacts": [{
            "kind": "go_test_json",
            "path": artifact.name,
            "sha256": audit.sha256_file(artifact),
        }],
        "commands": [shlex.join(command) + f" > {args.evidence_dir}/{artifact.name}"],
    })
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    print(f"recorded Radar tool evidence at {artifact}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
