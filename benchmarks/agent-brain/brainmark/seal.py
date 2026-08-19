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

SEAL v2 (plan 0.7): dual independent review. `review-template` now writes a
REVIEW.json with TWO named raters per pair (`--raters id1,id2`, default
`rater_1,rater_2`); see SEAL_PROTOCOL.md for the accept/reject criteria codes,
rater instructions, and the disclosure template (raters = user + one teammate,
project-affiliated). `promote` auto-detects the schema (a REVIEW.json with a
truthy top-level `raters` dict is v2; anything else falls back to the original
v1 single-reviewer path unchanged, so old fixtures/tests keep working) and, on
the v2 path, REFUSES to promote unless:
    - every pair present in REVIEW.json has a verdict from BOTH raters,
    - every reject verdict cites at least one criteria code,
    - every rater/rater disagreement has a named adjudicator's resolution,
    - Cohen's kappa can be computed across the dually-reviewed set (>=2 pairs).
Cohen's kappa (observed vs chance-expected agreement) is stored in
SEAL-MANIFEST.json under `seal_v2` alongside the raters and their affiliations.
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
SEAL_SCHEMA_VERSION = 1     # v1 manifest schema (single reviewer, no kappa)
SEAL_SCHEMA_VERSION_V2 = 2  # v2 manifest schema (dual review + Cohen's kappa)

# Written accept/reject criteria (plan 0.7; full definitions in SEAL_PROTOCOL.md).
# A reject verdict MUST cite >=1 of these; an unknown code anywhere is refused --
# criteria that are not from this fixed vocabulary cannot be aggregated or audited.
CRITERIA_CODES: dict[str, str] = {
    "DEP": "genuine A->B dependence -- resolving B requires understanding what "
           "A's session actually changed, not merely sharing a file",
    "LEAK": "leakage -- A's patch (or B's problem statement/tests) already "
            "contains B's fix; B would be a lookup, not rediscovery",
    "TRIV": "triviality -- B's fix is trivial (rename/typo/one-line) and would "
            "carry no rediscovery signal regardless of memory",
    "REVEAL": "answer-revealing -- B's problem statement or A's artifacts name "
              "the exact file/symbol/line B must edit, short-circuiting the "
              "mechanism under test",
}


def cohens_kappa(rated_pairs: list[tuple[str, str]]) -> dict:
    """Cohen's (1960) kappa for two raters over a fixed, unordered category set.

    `rated_pairs` is `[(rater_a_verdict, rater_b_verdict), ...]`, one entry per
    dually-reviewed item; categories are inferred from the data (this makes the
    function reusable for the 2-category seal verdict AND validity/'s 3-category
    yes/no/unclear labels -- see validity/analyze_validity.py).

        kappa = (po - pe) / (1 - pe)
        po = observed agreement = P(rater_a == rater_b)
        pe = chance agreement   = sum_c P(rater_a == c) * P(rater_b == c)

    Raises ValueError on zero items (kappa is undefined, not zero -- callers
    must treat that as "kappa missing", never silently substitute 0). When raters
    agree on every item AND all agreement falls in one category (pe == 1, po ==
    1), kappa is conventionally reported as 1.0 rather than the formula's 0/0.
    """
    n = len(rated_pairs)
    if n == 0:
        raise ValueError("cannot compute kappa over zero dually-reviewed items")
    categories = sorted({v for pair in rated_pairs for v in pair})
    po = sum(1 for a, b in rated_pairs if a == b) / n
    a_counts = {c: 0 for c in categories}
    b_counts = {c: 0 for c in categories}
    for a, b in rated_pairs:
        a_counts[a] += 1
        b_counts[b] += 1
    pe = sum((a_counts[c] / n) * (b_counts[c] / n) for c in categories)
    if pe >= 1.0 - 1e-12:
        kappa = 1.0 if po >= 1.0 - 1e-12 else 0.0
    else:
        kappa = (po - pe) / (1 - pe)
    return {
        "kappa": kappa, "n": n, "observed_agreement": po, "expected_agreement": pe,
        "categories": categories,
    }


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
    """Write a SEAL v2 REVIEW.json: two named raters, independent per-pair verdicts.

    Superseded the v1 single-reviewer template (plan 0.7). `promote` still reads
    a v1-shaped REVIEW.json unchanged for backward compatibility (see
    `_promote_v1`); this command only ever writes the new shape, because a fresh
    seal should always go through dual review.
    """
    candidates_dir = pathlib.Path(args.candidates)
    candidates = load_candidates(candidates_dir)
    rater_ids = [r.strip() for r in args.raters.split(",") if r.strip()]
    if len(rater_ids) != 2:
        raise SystemExit(
            f"--raters must name exactly two rater ids (e.g. --raters alice,bob), "
            f"got {rater_ids!r}. SEAL v2 dual review is not optional -- see SEAL_PROTOCOL.md."
        )

    template = {
        "schema_version": SEAL_SCHEMA_VERSION_V2,
        "_instructions": (
            "SEAL v2 dual independent review -- read SEAL_PROTOCOL.md FIRST for the "
            "accept/reject criteria codes and rater instructions. Fill in each rater's "
            "name and disclosed affiliation below. For every pair, BOTH raters "
            "independently set verdict to 'accept' or 'reject' and cite >=1 criteria "
            "code from criteria_codes (required on reject; do not discuss a pair with "
            "the other rater before both verdicts are recorded). Where the two "
            "verdicts disagree, set adjudication.resolved_verdict ('accept'/'reject') "
            "and adjudication.adjudicator (a named person, may be one of the raters). "
            "Only set 'split' ('sealed' or 'dev') on pairs that end up accepted. "
            "`seal.py promote` REFUSES to run if any pair is missing a review from "
            "either rater, if a reject has no criteria code, if a disagreement has no "
            "adjudication, or if Cohen's kappa cannot be computed across the reviewed set."
        ),
        "criteria_codes": dict(CRITERIA_CODES),
        "raters": {rid: {"name": "", "affiliation": ""} for rid in rater_ids},
        "pairs": {
            c["pair_id"]: {
                "needs_review": c["needs_human_review"],
                "score": c["score"],
                "patch_body_overlap": c["leakage"]["patch_body_overlap"],
                "shared_files": c["shared_files"],
                "reviews": {
                    rid: {"verdict": "", "criteria": [], "notes": ""} for rid in rater_ids
                },
                "adjudication": {"resolved_verdict": None, "adjudicator": "", "note": ""},
                "split": "",
            }
            for c in candidates
        },
    }
    out = candidates_dir.parent / REVIEW_NAME if args.out is None else pathlib.Path(args.out)
    if out.exists() and not args.force:
        raise SystemExit(f"{out} exists; pass --force to overwrite (this discards review work)")
    out.write_text(_harness.pretty_json(template), encoding="utf-8")
    print(f"wrote {out} (seal v2, raters={rater_ids}) with {len(candidates)} pairs awaiting dual review")
    return 0


def cmd_promote(args) -> int:
    """Dispatch on REVIEW.json's shape: a truthy `raters` dict is SEAL v2 (dual
    review, plan 0.7); anything else is the original v1 single-reviewer path,
    kept byte-for-byte so existing v1 fixtures/tests are unaffected.
    """
    review_path = pathlib.Path(args.review)
    if not review_path.is_file():
        raise SystemExit(
            f"{review_path} not found. Sealing REQUIRES human review: "
            "run `seal.py review-template` and fill it in."
        )
    review = json.loads(review_path.read_text(encoding="utf-8"))
    if review.get("raters"):
        return _promote_v2(args, review)
    return _promote_v1(args, review)


def _promote_v1(args, review: dict) -> int:
    config = _harness.load_config(args.config)
    root = _harness.BRAINMARK_DIR
    candidates_dir = pathlib.Path(args.candidates)
    review_path = pathlib.Path(args.review)
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


def _promote_v2(args, review: dict) -> int:
    """SEAL v2 (plan 0.7): dual independent review + Cohen's kappa, mandatory.

    REFUSES (raises SystemExit) if: fewer/more than two raters; a rater has no
    disclosed name/affiliation; any pair is missing a verdict from either rater;
    a reject cites no criteria code; a disagreement has no named adjudication;
    or kappa cannot be computed (fewer than two dually-reviewed pairs). Every
    refusal fires BEFORE any file is written, so a rejected promote never leaves
    a half-sealed tree.
    """
    config = _harness.load_config(args.config)
    root = _harness.BRAINMARK_DIR
    candidates_dir = pathlib.Path(args.candidates)
    review_path = pathlib.Path(args.review)

    raters = review.get("raters") or {}
    rater_ids = sorted(raters)
    if len(rater_ids) != 2:
        raise SystemExit(
            f"seal v2 requires exactly two raters in REVIEW.json['raters'], found "
            f"{len(rater_ids)} ({rater_ids}). Dual independent review is not optional."
        )
    for rid in rater_ids:
        meta = raters[rid] or {}
        if not str(meta.get("name") or "").strip():
            raise SystemExit(f"rater {rid!r} has no name; refusing to seal an unattributed review")
        if not str(meta.get("affiliation") or "").strip():
            raise SystemExit(
                f"rater {rid!r} has no disclosed affiliation; SEAL_PROTOCOL.md requires "
                "every rater's affiliation to be disclosed before sealing"
            )
    r1, r2 = rater_ids

    candidates = {c["pair_id"]: c for c in load_candidates(candidates_dir)}
    pairs = review.get("pairs") or {}
    if not pairs:
        raise SystemExit("REVIEW.json has no pairs; nothing to seal")

    incomplete: list[str] = []
    bad_criteria: list[str] = []
    unadjudicated: list[str] = []
    kappa_input: list[tuple[str, str]] = []
    n_adjudicated = 0
    accepted: dict[str, list[dict]] = {"sealed": [], "dev": []}

    for pair_id, verdict in sorted(pairs.items()):
        reviews = verdict.get("reviews") or {}
        v1_verdict = (reviews.get(r1) or {}).get("verdict")
        v2_verdict = (reviews.get(r2) or {}).get("verdict")
        if v1_verdict not in ("accept", "reject") or v2_verdict not in ("accept", "reject"):
            incomplete.append(pair_id)
            continue
        kappa_input.append((v1_verdict, v2_verdict))

        for rid, rv in ((r1, reviews[r1]), (r2, reviews[r2])):
            criteria = rv.get("criteria") or []
            unknown = [c for c in criteria if c not in CRITERIA_CODES]
            if unknown:
                bad_criteria.append(f"{pair_id}/{rid}: unknown criteria code(s) {unknown}")
            if rv.get("verdict") == "reject" and not criteria:
                bad_criteria.append(f"{pair_id}/{rid}: reject verdict cites no criteria code")

        if v1_verdict == v2_verdict == "accept":
            is_accept = True
        elif v1_verdict == v2_verdict == "reject":
            is_accept = False
        else:
            adj = verdict.get("adjudication") or {}
            resolved = adj.get("resolved_verdict")
            adjudicator = str(adj.get("adjudicator") or "").strip()
            if resolved not in ("accept", "reject") or not adjudicator:
                unadjudicated.append(pair_id)
                continue
            n_adjudicated += 1
            is_accept = resolved == "accept"

        if not is_accept:
            continue
        split = verdict.get("split")
        if split not in accepted:
            raise SystemExit(
                f"pair {pair_id}: accepted but split must be 'sealed' or 'dev', got {split!r}"
            )
        if pair_id not in candidates:
            raise SystemExit(f"review accepts {pair_id} which is not in {candidates_dir}")
        pair = dict(candidates[pair_id])
        pair["review"] = {
            "schema_version": SEAL_SCHEMA_VERSION_V2,
            "raters": {r1: reviews[r1], r2: reviews[r2]},
            "adjudication": verdict.get("adjudication") or {},
            "split": split,
        }
        accepted[split].append(pair)

    if incomplete:
        raise SystemExit(
            "seal v2 REFUSES promotion: missing a review from one or both raters "
            f"({r1}, {r2}) on pair(s): {sorted(incomplete)}"
        )
    if bad_criteria:
        raise SystemExit(
            "seal v2 REFUSES promotion: criteria problems:\n  " + "\n  ".join(sorted(bad_criteria))
        )
    if unadjudicated:
        raise SystemExit(
            "seal v2 REFUSES promotion: raters disagreed and no named adjudication resolved "
            f"pair(s): {sorted(unadjudicated)}"
        )
    if len(kappa_input) < 2:
        raise SystemExit(
            "seal v2 REFUSES promotion: kappa is missing (fewer than 2 dually-reviewed "
            "pairs) -- Cohen's kappa cannot be estimated, and promoting without it is "
            "exactly the failure mode dual review exists to prevent"
        )
    kappa_result = cohens_kappa(kappa_input)

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
    for split, pairs_list in accepted.items():
        split_dir = root / "tasks" / split
        split_dir.mkdir(parents=True, exist_ok=True)
        for stale in sorted(split_dir.glob("*.json")):
            stale.unlink()
        for pair in sorted(pairs_list, key=lambda p: p["pair_id"]):
            path = split_dir / f"{pair['pair_id']}.json"
            path.write_text(_harness.pretty_json(pair), encoding="utf-8")
            entries[f"{split}/{pair['pair_id']}.json"] = {
                "sha256": _harness.sha256_file(path), "split": split, "bytes": path.stat().st_size,
            }

    prereg_path = root / config["seal"]["prereg_path"]
    if not prereg_path.is_file():
        raise SystemExit(f"{prereg_path} missing; the pre-registration must exist before sealing")

    manifest = {
        "schema_version": SEAL_SCHEMA_VERSION_V2,
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
        "reviewer": f"{r1}+{r2} (dual review; see seal_v2 for raters/kappa)",
        "repo_commit": _git_commit(root),
        "repo_dirty_at_seal": _dirty(root),
        "seal_v2": {
            "raters": raters,
            "n_dually_reviewed_pairs": kappa_result["n"],
            "cohens_kappa": round(kappa_result["kappa"], 4),
            "observed_agreement": round(kappa_result["observed_agreement"], 4),
            "expected_agreement": round(kappa_result["expected_agreement"], 4),
            "disagreements_adjudicated": n_adjudicated,
        },
    }
    manifest_path = root / MANIFEST_NAME
    manifest_path.write_text(_harness.pretty_json(manifest), encoding="utf-8")
    print(
        f"[seal v2] sealed {manifest['sealed_count']} + dev {manifest['dev_count']} "
        f"(kappa={manifest['seal_v2']['cohens_kappa']}) -> {manifest_path}"
    )
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
    p_tpl.add_argument("--raters", default="rater_1,rater_2",
                       help="two comma-separated rater ids, e.g. --raters alice,bob")
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
