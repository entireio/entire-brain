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
import fnmatch
import json
import pathlib
import re
import shutil
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, run_b  # type: ignore[no-redef]
else:
    from . import _harness, run_b


#: grade_tag.sh resolves the arm from the TAG, with a shell `case`. We do not
#: get to pass the arm in: whatever that case picks is the directory it then
#: reads, so an arm the case cannot name is an arm that cannot be graded.
_CASE_BLOCK = re.compile(r'case\s+"\$TAG"\s+in(.*?)esac', re.DOTALL)
_CASE_ARM = re.compile(r'^\s*([^)\s]+)\)\s*ARM=([A-Za-z0-9_\-]+)', re.MULTILINE)


def script_arm_patterns(script: pathlib.Path) -> list[tuple[str, str]]:
    """[(glob, arm)] in the order grade_tag.sh's `case` tries them."""
    block = _CASE_BLOCK.search(pathlib.Path(script).read_text(encoding="utf-8"))
    if not block:
        return []
    return [(pattern, arm) for pattern, arm in _CASE_ARM.findall(block.group(1))]


def unresolvable_arms(script: pathlib.Path, tag: str,
                      arms: list[str]) -> list[tuple[str, str]]:
    """[(arm, why)] for every arm grade_tag.sh would refuse or MISATTRIBUTE.

    Two failure modes, both silent today:
      * no pattern matches -- `cannot infer arm from tag`, exit 1, and grade.py
        records an error with no resolved_ids;
      * a pattern matches but names a DIFFERENT arm -- the script then reads
        results/<tag>/<that other arm>/, which does not exist for this run.
    """
    patterns = script_arm_patterns(script)
    if not patterns:
        return []  # not a case-dispatching script; nothing to pre-check
    problems: list[tuple[str, str]] = []
    for arm in arms:
        staged_tag = f"{tag}_{arm}"
        resolved = next(
            (name for glob, name in patterns if fnmatch.fnmatchcase(staged_tag, glob)),
            None,
        )
        if resolved is None:
            problems.append((arm, f"{script.name} cannot infer an arm from tag "
                                  f"{staged_tag!r}; it would exit 1"))
        elif resolved != arm:
            problems.append((arm, f"{script.name} resolves tag {staged_tag!r} to arm "
                                  f"{resolved!r}, not {arm!r}, and would read the "
                                  f"wrong results directory"))
    return problems


def stage_arm(results: pathlib.Path, graphmark_root: pathlib.Path,
              tag: str, arm: str) -> tuple[pathlib.Path, list[str]]:
    """Materialize results/<tag>_<arm>/<arm>/<iid>/patch.diff for grade_tag.sh.

    grade_tag.sh infers the arm from the TAG, not from this layout, so
    `unresolvable_arms()` checks up front that the two agree. They must: if the
    script picks a different arm it reads a directory this function never wrote.
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
                ok = run_b.cell_is_complete(cell, backend=meta.get("backend"))
            per_pair.setdefault(pair_dir.name, {})[arm] = ok
    clean = [p for p, arms_ok in per_pair.items() if all(arms_ok.get(a) for a in arms)]
    return sorted({iid_of[p] for p in clean if p in iid_of})


def run(results: pathlib.Path, config: dict, tag: str, arms: list[str],
        dry_run: bool = False) -> dict:
    graphmark_root = pathlib.Path(config["graphmark_root"])
    script = graphmark_root / config["grading"]["grade_tag_script"]
    if not script.is_file():
        raise SystemExit(f"grade_tag.sh not found at {script}")

    unresolvable = unresolvable_arms(script, tag, arms)
    if unresolvable:
        raise SystemExit(
            "REFUSING TO GRADE -- the grading script cannot address these arms:\n"
            + "\n".join(f"  {arm}: {why}" for arm, why in unresolvable)
            + "\n\nGrading them anyway records an error with no resolved_ids, and "
              "every instance of the arm is then scored UNRESOLVED, which turns an "
              "infrastructure failure into a measured 'memory did not help'."
        )

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
            official = graphmark_root / f"{staged_tag}.{staged_tag}.json"
            official.unlink(missing_ok=True)
            proc = subprocess.run(cmd, cwd=str(graphmark_root), capture_output=True,
                                  text=True, check=False)
            entry["returncode"] = proc.returncode
            entry["stdout_tail"] = (proc.stdout or "")[-2000:]
            entry["stderr_tail"] = (proc.stderr or "")[-2000:]
            official = graphmark_root / f"{staged_tag}.{staged_tag}.json"
            if official.is_file() and proc.returncode == 0:
                payload = json.loads(official.read_text(encoding="utf-8"))
                entry["report_path"] = str(official)
                entry["resolved_ids"] = sorted(payload.get("resolved_ids") or [])
                entry["resolved"] = len(entry["resolved_ids"])
                entry["total"] = payload.get("total_instances")
                entry["graded"] = True
            else:
                # NO resolved_ids key, and an explicit graded=False. An arm that
                # was not graded has no resolved% -- it does not have zero.
                entry["graded"] = False
                entry["error"] = (
                    f"grade_tag.sh rc={proc.returncode} and no official report at "
                    f"{official}"
                )
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
