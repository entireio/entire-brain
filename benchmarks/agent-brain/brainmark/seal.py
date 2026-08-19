#!/usr/bin/env python3
"""Seal mined candidates into the frozen task set. Human-in-the-loop.

WHY SEALING EXISTS: the single most common way a benchmark launders a null
result into a headline is to keep adjusting the task set after seeing scores.
Sealing makes that mechanically visible. Every sealed task's bytes are hashed
into SEAL-MANIFEST.json together with the miner's source hash, the config hash,
and the pre-registration's hash. report.py recomputes all of it and REFUSES to
aggregate on any mismatch.

    tasks/sealed/   n >= 30  confirmatory. Touched once, at the end.
    tasks/dev/      10       harness debugging, pilot, null test. Burn freely.

The two splits are DISJOINT by construction (a pair cannot be in both, and the
dev split is drawn first so that sealing cannot quietly reuse a debugged pair).

Human review is REQUIRED, not advisory: `promote` refuses to run without a
review file that explicitly accepts each pair. Candidates the miner flagged
(borderline patch-body overlap) must be accepted individually.

Commands:
    seal.py review-template   -> write REVIEW.json for a human to edit
    seal.py promote           -> candidates + REVIEW.json -> sealed/ + dev/ + manifest
    seal.py verify            -> recompute every hash; exit 1 on mismatch
"""

from __future__ import annotations

import argparse
import json
import pathlib
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from . import _harness

MANIFEST_NAME = "SEAL-MANIFEST.json"
REVIEW_NAME = "REVIEW.json"
SEAL_SCHEMA_VERSION = 1


def _git_commit(path: pathlib.Path) -> str | None:
    proc = subprocess.run(
        ["git", "-C", str(path), "rev-parse", "HEAD"],
        capture_output=True, text=True, check=False,
    )
    return proc.stdout.strip() or None if proc.returncode == 0 else None


def _dirty(path: pathlib.Path) -> bool:
    proc = subprocess.run(
        ["git", "-C", str(path), "status", "--porcelain"],
        capture_output=True, text=True, check=False,
    )
    return bool(proc.stdout.strip())


def load_candidates(candidates_dir: pathlib.Path) -> list[dict]:
    index_path = candidates_dir / "INDEX.json"
    if not index_path.is_file():
        raise SystemExit(f"no INDEX.json in {candidates_dir}; run mine_pairs.py first")
    index = json.loads(index_path.read_text(encoding="utf-8"))
    out = []
    for pair_id in index["candidate_pair_ids"]:
        path = candidates_dir / f"{pair_id}.json"
        if not path.is_file():
            raise SystemExit(f"INDEX lists {pair_id} but {path} is missing")
        out.append(json.loads(path.read_text(encoding="utf-8")))
    return out


def cmd_review_template(args) -> int:
    candidates_dir = pathlib.Path(args.candidates)
    candidates = load_candidates(candidates_dir)
    template = {
        "_instructions": (
            "Set accept=true for each pair you have READ and judged to be a genuine "
            "(prior work, later related task) pair. Pairs with needs_review=true were "
            "flagged by the miner for borderline patch-body overlap and must be judged "
            "individually. Set split to 'dev' for the 10 pairs you are willing to burn "
            "on harness debugging, and 'sealed' for the confirmatory set. Anything left "
            "accept=false is excluded."
        ),
        "reviewer": "",
        "reviewed_at": "",
        "pairs": {
            c["pair_id"]: {
                "accept": False,
                "split": "",
                "needs_review": c["needs_human_review"],
                "score": c["score"],
                "patch_body_overlap": c["leakage"]["patch_body_overlap"],
                "shared_files": c["shared_files"],
                "note": "",
            }
            for c in candidates
        },
    }
    out = candidates_dir.parent / REVIEW_NAME if args.out is None else pathlib.Path(args.out)
    if out.exists() and not args.force:
        raise SystemExit(f"{out} exists; pass --force to overwrite (this discards review work)")
    out.write_text(_harness.pretty_json(template), encoding="utf-8")
    print(f"wrote {out} with {len(candidates)} pairs awaiting review")
    return 0


def cmd_promote(args) -> int:
    config = _harness.load_config(args.config)
    root = _harness.BRAINMARK_DIR
    candidates_dir = pathlib.Path(args.candidates)
    review_path = pathlib.Path(args.review)
    if not review_path.is_file():
        raise SystemExit(
            f"{review_path} not found. Sealing REQUIRES human review: "
            "run `seal.py review-template` and fill it in."
        )

    review = json.loads(review_path.read_text(encoding="utf-8"))
    if not str(review.get("reviewer") or "").strip():
        raise SystemExit("REVIEW.json has no reviewer; refusing to seal an unattributed task set")

    candidates = {c["pair_id"]: c for c in load_candidates(candidates_dir)}
    accepted: dict[str, list[dict]] = {"sealed": [], "dev": []}
    for pair_id, verdict in sorted(review.get("pairs", {}).items()):
        if not verdict.get("accept"):
            continue
        split = verdict.get("split")
        if split not in accepted:
            raise SystemExit(f"pair {pair_id}: split must be 'sealed' or 'dev', got {split!r}")
        if pair_id not in candidates:
            raise SystemExit(f"review accepts {pair_id} which is not in {candidates_dir}")
        pair = dict(candidates[pair_id])
        pair["review"] = {
            "reviewer": review["reviewer"],
            "reviewed_at": review.get("reviewed_at", ""),
            "note": verdict.get("note", ""),
            "split": split,
        }
        accepted[split].append(pair)

    overlap = {p["pair_id"] for p in accepted["sealed"]} & {p["pair_id"] for p in accepted["dev"]}
    if overlap:
        raise SystemExit(f"splits must be disjoint; both contain {sorted(overlap)}")

    sealed_min = int(config["seal"]["sealed_min"])
    dev_target = int(config["seal"]["dev_split"])
    problems = []
    if len(accepted["sealed"]) < sealed_min:
        problems.append(f"sealed split has {len(accepted['sealed'])}, needs >= {sealed_min}")
    if len(accepted["dev"]) != dev_target:
        problems.append(f"dev split has {len(accepted['dev'])}, expected {dev_target}")
    if problems and not args.allow_short:
        raise SystemExit(
            "; ".join(problems)
            + ". Pass --allow-short ONLY to seal a deliberately under-powered pilot set, and "
              "record the reduced power in PREREGISTRATION.md -- never to make a number look better."
        )

    entries: dict[str, dict] = {}
    for split, pairs in accepted.items():
        split_dir = root / "tasks" / split
        split_dir.mkdir(parents=True, exist_ok=True)
        for stale in sorted(split_dir.glob("*.json")):
            stale.unlink()
        for pair in sorted(pairs, key=lambda p: p["pair_id"]):
            path = split_dir / f"{pair['pair_id']}.json"
            path.write_text(_harness.pretty_json(pair), encoding="utf-8")
            entries[f"{split}/{pair['pair_id']}.json"] = {
                "sha256": _harness.sha256_file(path),
                "split": split,
                "bytes": path.stat().st_size,
            }

    prereg_path = root / config["seal"]["prereg_path"]
    if not prereg_path.is_file():
        raise SystemExit(f"{prereg_path} missing; the pre-registration must exist before sealing")

    manifest = {
        "schema_version": SEAL_SCHEMA_VERSION,
        "sealed_count": len(accepted["sealed"]),
        "dev_count": len(accepted["dev"]),
        "under_powered": bool(problems),
        "under_powered_reasons": problems,
        "tasks": dict(sorted(entries.items())),
        "miner_sha256": _harness.sha256_file(root / "mine_pairs.py"),
        "config_sha256": config["_config_sha256"],
        "prereg_sha256": _harness.sha256_file(prereg_path),
        "prompts_sha256": _harness.sha256_file(root / "prompts.py"),
        "mechmetrics_sha256": _harness.sha256_file(root / "mechmetrics.py"),
        "vendored_metrics_sha256": _harness.sha256_file(root / "vendor" / "graphmark_metrics.py"),
        "candidates_index_sha256": _harness.sha256_file(candidates_dir / "INDEX.json"),
        "review_sha256": _harness.sha256_file(review_path),
        "reviewer": review["reviewer"],
        "repo_commit": _git_commit(root),
        "repo_dirty_at_seal": _dirty(root),
    }
    manifest_path = root / MANIFEST_NAME
    manifest_path.write_text(_harness.pretty_json(manifest), encoding="utf-8")
    print(f"sealed {manifest['sealed_count']} + dev {manifest['dev_count']} -> {manifest_path}")
    if manifest["repo_dirty_at_seal"]:
        print("WARNING: repo was dirty at seal time; the code hashes may not be reproducible")
    return 0


def verify(root: pathlib.Path | None = None) -> tuple[bool, list[str]]:
    """Recompute every hash in the manifest. Returns (ok, problems)."""
    root = root or _harness.BRAINMARK_DIR
    manifest_path = root / MANIFEST_NAME
    if not manifest_path.is_file():
        return False, [f"{MANIFEST_NAME} not found -- the task set was never sealed"]
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    problems: list[str] = []

    for rel, entry in sorted(manifest.get("tasks", {}).items()):
        path = root / "tasks" / rel
        if not path.is_file():
            problems.append(f"sealed task missing: {rel}")
            continue
        actual = _harness.sha256_file(path)
        if actual != entry["sha256"]:
            problems.append(
                f"sealed task MODIFIED: {rel} (sealed {entry['sha256'][:12]}, now {actual[:12]})"
            )

    for split in ("sealed", "dev"):
        split_dir = root / "tasks" / split
        if not split_dir.is_dir():
            continue
        on_disk = {f"{split}/{p.name}" for p in split_dir.glob("*.json")}
        extra = on_disk - set(manifest.get("tasks", {}))
        if extra:
            problems.append(f"UNSEALED tasks present in {split}/: {sorted(extra)}")

    for key, rel in (
        ("miner_sha256", "mine_pairs.py"),
        ("prompts_sha256", "prompts.py"),
        ("mechmetrics_sha256", "mechmetrics.py"),
        ("vendored_metrics_sha256", "vendor/graphmark_metrics.py"),
    ):
        path = root / rel
        if not path.is_file():
            problems.append(f"{rel} missing")
            continue
        actual = _harness.sha256_file(path)
        if manifest.get(key) and actual != manifest[key]:
            problems.append(
                f"{rel} CHANGED since seal (sealed {manifest[key][:12]}, now {actual[:12]})"
            )

    prereg = root / "PREREGISTRATION.md"
    if prereg.is_file() and manifest.get("prereg_sha256"):
        actual = _harness.sha256_file(prereg)
        if actual != manifest["prereg_sha256"]:
            problems.append(
                f"PREREGISTRATION.md CHANGED since seal "
                f"(sealed {manifest['prereg_sha256'][:12]}, now {actual[:12]})"
            )

    return (not problems), problems


def cmd_verify(args) -> int:
    ok, problems = verify(pathlib.Path(args.root) if args.root else None)
    if ok:
        print("SEAL OK: every sealed task and pinned file matches the manifest")
        return 0
    print("SEAL VERIFICATION FAILED:")
    for problem in problems:
        print(f"  - {problem}")
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Seal BrainMark task pairs.")
    sub = parser.add_subparsers(dest="cmd", required=True)

    default_candidates = str(_harness.BRAINMARK_DIR / "candidates")

    p_tpl = sub.add_parser("review-template")
    p_tpl.add_argument("--candidates", default=default_candidates)
    p_tpl.add_argument("--out", default=None)
    p_tpl.add_argument("--force", action="store_true")
    p_tpl.set_defaults(func=cmd_review_template)

    p_pro = sub.add_parser("promote")
    p_pro.add_argument("--candidates", default=default_candidates)
    p_pro.add_argument("--review", default=str(_harness.BRAINMARK_DIR / REVIEW_NAME))
    p_pro.add_argument("--config", default=None)
    p_pro.add_argument("--allow-short", action="store_true")
    p_pro.set_defaults(func=cmd_promote)

    p_ver = sub.add_parser("verify")
    p_ver.add_argument("--root", default=None)
    p_ver.set_defaults(func=cmd_verify)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
