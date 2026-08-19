#!/usr/bin/env python3
"""Grade B patches with the official SWE-bench harness (free, Docker).

EXTERNAL GRADING IS THE POINT. B is graded by ITS OWN FAIL_TO_PASS tests from
the upstream dataset, executed by `swebench.harness.run_evaluation` in the
official per-instance Docker image. Nothing in this repo decides whether a patch
is correct, which is what makes the result quotable.

This wraps graphmark's tools/grade_tag.sh, which already encodes the layout the
harness expects (`preds_<tag>.json`) plus the settle loop for cost>0 sessions
whose patch has not landed yet. We reshape BrainMark's per-arm results into the
`results/<tag>/<arm>/<iid>/patch.diff` layout it reads, one tag per arm, then
collect each arm's report.

Note the instance graded is B's instance_id: a pair is (A, B) but only B is a
measured task.

Usage:
    python3 grade.py --results results/B/pilot --tag bm_pilot [--arms a,b]
    python3 grade.py --results results/B/pilot --tag bm_pilot --dry-run
"""

from __future__ import annotations

import argparse
import json
import pathlib
import shutil
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from . import _harness


def stage_arm(results: pathlib.Path, graphmark_root: pathlib.Path,
              tag: str, arm: str) -> tuple[pathlib.Path, list[str]]:
    """Materialize results/<tag>_<arm>/<arm>/<iid>/patch.diff for grade_tag.sh.

    grade_tag.sh infers the arm from the tag SUFFIX, so the tag must end in a
    string it recognizes; we pass the arm through explicitly via the directory
    layout and give the tag an arm-suffixed name.
    """
    staged_tag = f"{tag}_{arm}"
    staged_root = graphmark_root / "results" / staged_tag / arm
    if staged_root.exists():
        shutil.rmtree(staged_root)
    staged_root.mkdir(parents=True, exist_ok=True)

    instance_ids: list[str] = []
    for pair_dir in sorted(results.iterdir()):
        cell = pair_dir / arm
        if not cell.is_dir():
            continue
        meta_path = cell / "meta.json"
        if not meta_path.is_file():
            continue
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        iid = meta["instance_id"]
        dest = staged_root / iid
        dest.mkdir(parents=True, exist_ok=True)
        for name in ("patch.diff", "cc_out.json"):
            src = cell / name
            if src.is_file():
                shutil.copyfile(src, dest / name)
        instance_ids.append(iid)
    return staged_root, sorted(set(instance_ids))


def eligible_ids(results: pathlib.Path, arms: list[str]) -> list[str]:
    """ALL-ARMS-CLEAN pair gate, applied symmetrically.

    A pair counts only if EVERY arm produced usd>0 AND a non-empty patch. A pair
    that fails in one arm is dropped from ALL arms -- dropping it only where it
    failed would silently delete that arm's hard cases and invent an effect.
    """
    per_pair: dict[str, dict[str, bool]] = {}
    iid_of: dict[str, str] = {}
    for pair_dir in sorted(results.iterdir()):
        if not pair_dir.is_dir():
            continue
        for arm in arms:
            cell = pair_dir / arm
            meta_path = cell / "meta.json"
            patch = cell / "patch.diff"
            ok = False
            if meta_path.is_file():
                meta = json.loads(meta_path.read_text(encoding="utf-8"))
                iid_of[pair_dir.name] = meta["instance_id"]
                usd = float((meta.get("result_event") or {}).get("total_cost_usd") or 0)
                ok = usd > 0 and patch.is_file() and patch.stat().st_size > 0
            per_pair.setdefault(pair_dir.name, {})[arm] = ok
    clean = [p for p, arms_ok in per_pair.items() if all(arms_ok.get(a) for a in arms)]
    return sorted(iid_of[p] for p in clean if p in iid_of)


def run(results: pathlib.Path, config: dict, tag: str, arms: list[str],
        dry_run: bool = False) -> dict:
    graphmark_root = pathlib.Path(config["graphmark_root"])
    script = graphmark_root / config["grading"]["grade_tag_script"]
    if not script.is_file():
        raise SystemExit(f"grade_tag.sh not found at {script}")

    elig = eligible_ids(results, arms)
    report: dict = {"tag": tag, "arms": arms, "eligible_instances": elig,
                    "eligible_count": len(elig), "dry_run": dry_run, "per_arm": {}}
    if not elig:
        report["error"] = "no pair passed the all-arms-clean gate; nothing to grade"
        return report

    elig_file = graphmark_root / f"elig_{tag}.txt"
    elig_file.write_text("\n".join(elig) + "\n", encoding="utf-8")
    report["elig_file"] = str(elig_file)

    for arm in arms:
        staged_root, ids = stage_arm(results, graphmark_root, tag, arm)
        staged_tag = f"{tag}_{arm}"
        cmd = ["bash", str(script), staged_tag, str(elig_file)]
        entry: dict = {"staged_root": str(staged_root), "instances": len(ids), "cmd": cmd}
        if dry_run:
            entry["skipped"] = "dry run: Docker grading not invoked"
        else:
            proc = subprocess.run(cmd, cwd=str(graphmark_root), capture_output=True,
                                  text=True, check=False)
            entry["returncode"] = proc.returncode
            entry["stdout_tail"] = (proc.stdout or "")[-2000:]
            entry["stderr_tail"] = (proc.stderr or "")[-2000:]
            official = graphmark_root / f"{staged_tag}.{staged_tag}.json"
            if official.is_file():
                payload = json.loads(official.read_text(encoding="utf-8"))
                entry["report_path"] = str(official)
                entry["resolved_ids"] = sorted(payload.get("resolved_ids") or [])
                entry["resolved"] = len(entry["resolved_ids"])
                entry["total"] = payload.get("total_instances")
            else:
                entry["error"] = "grade_tag.sh produced no official report"
        report["per_arm"][arm] = entry
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Grade BrainMark B sessions.")
    parser.add_argument("--results", required=True, help="results/B/<tier> directory")
    parser.add_argument("--tag", required=True)
    parser.add_argument("--config", default=None)
    parser.add_argument("--arms", default=None)
    parser.add_argument("--out", default=None)
    parser.add_argument("--dry-run", action="store_true",
                        help="stage predictions and print the plan; never starts Docker")
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    arms = args.arms.split(",") if args.arms else list(config["arms"])
    report = run(pathlib.Path(args.results), config, args.tag, arms, dry_run=args.dry_run)

    out = pathlib.Path(args.out) if args.out else (pathlib.Path(args.results) / "grading.json")
    out.write_text(_harness.pretty_json(report), encoding="utf-8")
    print(_harness.pretty_json({
        "tag": report["tag"],
        "eligible_count": report["eligible_count"],
        "per_arm": {a: {k: v for k, v in e.items() if k in ("resolved", "total", "skipped", "error")}
                    for a, e in report["per_arm"].items()},
    }), end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
