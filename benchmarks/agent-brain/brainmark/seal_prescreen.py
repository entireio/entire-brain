#!/usr/bin/env python3
"""Deterministic review-slate prescreen: candidates/ -> review/ (seal session prep).

WHY THIS EXISTS: the full-pool miner emits far more candidate pairs than two
humans can read at 2-3 minutes each. Choosing *which* pairs the raters see is a
selection decision, and an undocumented selection decision made by a tired
operator the morning of the review session is exactly the kind of unfalsifiable
step SEAL_PROTOCOL.md exists to remove. So the slate is chosen HERE, by a pure
function of the mined candidates plus a handful of named parameters, with:

  * a total order that is byte-stable across runs (no wallclock, no PID, no
    hash-seed, no filesystem-iteration order reaches the output),
  * one rationale code on every candidate -- selected AND excluded -- so the
    question "why was this pair never reviewed?" has a recorded answer,
  * caps that are enforced against the ACHIEVED slate, not the requested one.

It writes nothing into the sealed tree. `seal.py` remains the only thing that
promotes, and the raters' verdicts remain the only thing that accepts a pair;
this script only decides what lands in front of them, and says so on the record.

Outputs (all under `review/`, none of it sealed):

    review/SLATE.json           the slate + every exclusion + full provenance
    review/REVIEW-TEMPLATE.json seal.py v2 REVIEW.json, prefilled, empty verdicts
    review/REVIEW-<rater>.json  one copy per rater (independence at judgment time)
    review/sheets/NNN_<pair>.md one 2-3 minute review sheet per slated pair
    review/sheets/INDEX.md      the session checklist
    review/patches/<iid>.patch  A's and B's gold patches, sha256-verified
    review/statements/<iid>.txt full problem statements, sha256-verified

Selection ladder (each step is a rationale code in SLATE.json):

    1. DROP_DUP_A / DROP_DUP_B  a pair whose A (or B) instance already appears
                                `--dup-cap` times is a near-duplicate of a
                                higher-ranked pair; the rater would be judging
                                the same session twice.
    2. SEL_STRATUM_FLOOR        every (pool, language) stratum with any eligible
                                pair gets its top `--stratum-floor` pairs, so a
                                small language is not erased by score rank.
    3. SEL_RANK                 miner score order, subject to the repo cap and
                                the stratum ceiling.
    4. SEL_RELAX_STRATUM        only if the slate is still short: same, ceiling
                                dropped. Recorded in `relaxations`.
    5. DROP_REPO_CAP / DROP_RANK / DROP_REPO_CAP_FINAL   everything else.

The repo cap is NEVER relaxed. If the slate cannot reach `--slate-size` without
one repository exceeding `--repo-cap-frac` of it, the slate comes out SHORT and
says so (`slate_short: true`) -- a short slate is a fact about the candidate
pool, whereas a quietly over-concentrated one is a fact about this script that
nobody would have noticed.

Usage:
    python3 seal_prescreen.py                       # everything, default knobs
    python3 seal_prescreen.py --slate-size 350 --dev-size 20
    python3 seal_prescreen.py --no-sheets           # SLATE.json + template only
    python3 seal_prescreen.py --summary-only        # decide, write nothing

After the raters have each filled in their own copy:

    python3 seal_prescreen.py --merge-reviews review/REVIEW-rater_1.json \\
                                             review/REVIEW-rater_2.json
    python3 seal.py promote
"""

from __future__ import annotations

import argparse
import collections
import contextlib
import io
import json
import math
import pathlib
import re
import shutil
import sys
import tempfile

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, mine_pairs, pool_loaders, seal  # type: ignore[no-redef]
else:
    from . import _harness, mine_pairs, pool_loaders, seal

PRESCREEN_VERSION = 1

DEFAULT_SLATE_SIZE = 350
DEFAULT_DEV_SIZE = 20
DEFAULT_REPO_CAP_FRAC = 0.12
DEFAULT_DUP_CAP = 2
DEFAULT_STRATUM_FLOOR = 2
DEFAULT_STRATUM_CAP_FRAC = 0.40

# Rendering budgets for the human sheets. The full text always lands on disk
# (review/statements, review/patches) and the sheet names the path, so a
# truncated sheet is a reading convenience, never a loss of evidence.
PROBLEM_CHARS = 6000
PATCH_CHARS = 6000

RATIONALE_CODES: dict[str, str] = {
    "SEL_STRATUM_FLOOR": "selected to guarantee its (pool, language) stratum a minimum "
                         "presence on the slate, ahead of pure score rank",
    "SEL_RANK": "selected in miner-score order, within the repo cap and stratum ceiling",
    "SEL_RELAX_STRATUM": "selected after the stratum ceiling was relaxed because the "
                         "slate was short; the repo cap still held",
    "DROP_DUP_A": "near-duplicate: this pair's A instance already appears dup_cap times "
                  "in higher-ranked slated pairs",
    "DROP_DUP_B": "near-duplicate: this pair's B instance already appears dup_cap times "
                  "in higher-ranked slated pairs",
    "DROP_REPO_CAP": "its repository had already reached the per-repo share cap",
    "DROP_REPO_CAP_FINAL": "trimmed after selection: the achieved slate was short, so the "
                           "per-repo cap recomputed against the achieved size excluded it",
    "DROP_STRATUM_CAP": "its (pool, language) stratum had reached the stratum ceiling and "
                        "the slate filled without needing to relax it",
    "DROP_RANK": "ranked below the slate cutoff",
    "DEV_PROPOSED": "proposed for the dev split (spans repos/languages; the raters and "
                    "the split assignment remain free to overrule this)",
}

# The ONLY keys seal_prescreen adds to seal.py's review template. Named
# explicitly rather than matched by a `_` prefix: seal.py's own template already
# has an `_instructions` key, and a prefix rule would strip that too and then
# "prove" a match that had quietly dropped the raters' instructions.
PRESCREEN_TEMPLATE_KEYS = ("_prescreen",)
PRESCREEN_PAIR_KEYS = ("_slate_rank", "_sheet", "_repo", "_language", "_proposed_split")

# Pools whose instances carry no `language` field are Python-only SWE-bench
# dumps; recording that as "unspecified" would collapse three quarters of the
# candidate pool into one meaningless stratum. The inference is per-entry
# flagged (`language_source`) so nothing downstream mistakes it for ground truth.
PYTHON_ONLY_POOLS = ("swe_bench", "swe_bench_verified")


# --------------------------------------------------------------------------
# inputs
# --------------------------------------------------------------------------


def load_index(candidates_dir: pathlib.Path) -> dict:
    index_path = candidates_dir / "INDEX.json"
    if not index_path.is_file():
        raise SystemExit(
            f"no INDEX.json in {candidates_dir}; run `mine_pairs.py --all-pools` first"
        )
    return json.loads(index_path.read_text(encoding="utf-8"))


def check_pool_integrity(config: dict, index: dict) -> list[dict]:
    """The cached pools on disk must be the ones the candidates were mined from.

    A silently re-downloaded pool would change every problem statement and patch
    the raters read while leaving candidate ids untouched -- the review would be
    of a different dataset than the one sealed. Cheap to check, so check it.
    """
    checks: list[dict] = []
    for name, rec in sorted((index.get("pool_revisions") or {}).items()):
        data_path, _ = pool_loaders.pool_paths(config, name)
        try:
            display = str(data_path.relative_to(_harness.BRAINMARK_DIR))
        except ValueError:
            display = _harness.display_path(data_path)
        entry = {
            "pool": name,
            "path": display,
            "revision": rec.get("revision"),
            "index_sha256": rec.get("sha256"),
            "on_disk_sha256": None,
            "ok": False,
        }
        if data_path.is_file():
            entry["on_disk_sha256"] = _harness.sha256_file(data_path)
            entry["ok"] = entry["on_disk_sha256"] == rec.get("sha256")
        checks.append(entry)
    return checks


def instance_index(config: dict, pools: list[str] | None) -> dict[str, dict]:
    """instance_id -> instance, merged exactly as the miner merged them."""
    instances, _sources = mine_pairs.load_all_instances(config, pools)
    return instances


def pool_membership(config: dict, pools: list[str] | None) -> dict[str, list[str]]:
    """instance_id -> every cached pool that contains it.

    The miner's merge is first-wins, so an instance present in BOTH a graphmark
    task JSON and a Python-only HF dump is attributed to `local` and loses the
    only evidence of what language it is. Membership is kept separately so the
    stratification can use that evidence without disturbing the attribution.
    """
    out: dict[str, list[str]] = {}
    for name in pool_loaders.sort_pools(config, list(pools or [])):
        pool, _prov = pool_loaders.load_pool(config, name)
        for iid in sorted(pool):
            out.setdefault(iid, []).append(name)
    return out


def language_of(inst: dict, member_pools: list[str] | None = None) -> tuple[str, str]:
    """(language, source) -- 'instance' when the row declares one, else inferred.

    Never guesses from the repository name: the only inference allowed is
    membership of a dataset that is Python-only by construction, and it is
    labelled so nothing downstream mistakes it for a declared field.
    """
    declared = str(inst.get("language") or "").strip()
    if declared:
        return declared, "instance"
    pool = str(inst.get("pool") or "local")
    if pool in PYTHON_ONLY_POOLS:
        return "Python", "pool_default"
    for name in member_pools or ():
        if name in PYTHON_ONLY_POOLS:
            return "Python", "pool_membership"
    return "unspecified", "absent"


def enrich(candidate: dict, instances: dict[str, dict],
           membership: dict[str, list[str]] | None = None) -> dict:
    """Candidate + the pool/language facts the selection and the sheets need.

    The miner's candidate JSON deliberately stores only sha256 of the problem
    statement and the patch, so the enrichment is looked up from the same pool
    cache the miner read and the sha256 is re-checked (see `verify_side`).
    """
    a_id = candidate["a"]["instance_id"]
    b_id = candidate["b"]["instance_id"]
    a_inst = instances.get(a_id) or {}
    b_inst = instances.get(b_id) or {}
    membership = membership or {}
    a_lang, a_lang_src = (language_of(a_inst, membership.get(a_id))
                          if a_inst else ("unspecified", "missing"))
    b_lang, b_lang_src = (language_of(b_inst, membership.get(b_id))
                          if b_inst else ("unspecified", "missing"))
    # B is the task actually graded, so B's language/pool define the stratum.
    pool = str(b_inst.get("pool") or "local") if b_inst else "unknown"
    return {
        "pair_id": candidate["pair_id"],
        "repo": candidate["repo"],
        "score": float(candidate["score"]),
        "file_jaccard": float(candidate["score_components"]["file_jaccard"]),
        "symbol_overlap": float(candidate["score_components"]["symbol_overlap"]),
        "patch_body_overlap": float(candidate["leakage"]["patch_body_overlap"]),
        "needs_human_review": bool(candidate["needs_human_review"]),
        "shared_files": list(candidate["shared_files"]),
        "shared_symbols": list(candidate.get("shared_symbols") or []),
        "a_id": a_id,
        "b_id": b_id,
        "pool": pool,
        "language": b_lang,
        "language_source": b_lang_src,
        "a_language": a_lang,
        "a_language_source": a_lang_src,
        "stratum": f"{pool}/{b_lang}",
        "instances_resolved": bool(a_inst) and bool(b_inst),
        "_candidate": candidate,
    }


# --------------------------------------------------------------------------
# selection (pure -- this is the part the tests pin)
# --------------------------------------------------------------------------


def rank_key(entry: dict) -> tuple:
    """Total order: score desc, then leakage asc, then pair_id asc.

    Identical to `mine_pairs._better`, on purpose: two places that rank the same
    objects by different rules is a bug waiting for a tie.
    """
    return (-entry["score"], entry["patch_body_overlap"], entry["pair_id"])


def _max_compliant_size(capacities: list[int], frac: float, upper: int) -> int:
    """Largest n <= upper for which the per-repo share cap is satisfiable.

    A slate of size n admits at most k = floor(n * frac) pairs per repository, so
    the repositories can supply sum_r min(count_r, k) pairs; the cap is
    satisfiable exactly when that is >= n. Returns 0 when no n works -- i.e. the
    candidate pool is too concentrated for this cap at any size, which is a fact
    to report, not a slate to shrink. `floor` (not `max(1, floor)`) on purpose:
    a cap of "one pair per repo" for an 8-pair slate is not a 12% cap.
    """
    for n in range(upper, 0, -1):
        k = math.floor(n * frac)
        if k < 1:
            continue
        if sum(min(c, k) for c in capacities) >= n:
            return n
    return 0


def select_slate(entries: list[dict], *, slate_size: int, repo_cap_frac: float,
                 dup_cap: int, stratum_floor: int, stratum_cap_frac: float) -> dict:
    """Choose the review slate. Pure, deterministic, no I/O.

    Returns {"slate": [...], "excluded": [...], "relaxations": [...],
             "repo_cap": int, "stratum_cap": int, "slate_short": bool}.
    Every input entry appears exactly once across `slate` and `excluded`, each
    carrying a non-empty `rationale` list.
    """
    if slate_size < 0:
        raise ValueError("slate_size must be >= 0")
    if dup_cap < 1:
        raise ValueError("dup_cap must be >= 1")

    ordered = sorted(entries, key=rank_key)

    # -- 1. near-duplicate screen ------------------------------------------
    eligible: list[dict] = []
    excluded: list[dict] = []
    a_seen: collections.Counter[str] = collections.Counter()
    b_seen: collections.Counter[str] = collections.Counter()
    for entry in ordered:
        codes = []
        if a_seen[entry["a_id"]] >= dup_cap:
            codes.append("DROP_DUP_A")
        if b_seen[entry["b_id"]] >= dup_cap:
            codes.append("DROP_DUP_B")
        if codes:
            excluded.append({**entry, "rationale": codes})
            continue
        a_seen[entry["a_id"]] += 1
        b_seen[entry["b_id"]] += 1
        eligible.append(entry)

    repo_cap = max(1, math.floor(slate_size * repo_cap_frac))
    stratum_cap = max(1, math.floor(slate_size * stratum_cap_frac))

    chosen: dict[str, list[str]] = {}
    order: list[dict] = []
    repo_n: collections.Counter[str] = collections.Counter()
    stratum_n: collections.Counter[str] = collections.Counter()
    relaxations: list[str] = []

    def take(entry: dict, code: str) -> None:
        chosen[entry["pair_id"]] = [code]
        order.append(entry)
        repo_n[entry["repo"]] += 1
        stratum_n[entry["stratum"]] += 1

    # -- 2. stratum floor ---------------------------------------------------
    by_stratum: dict[str, list[dict]] = collections.defaultdict(list)
    for entry in eligible:
        by_stratum[entry["stratum"]].append(entry)
    for stratum in sorted(by_stratum):
        for entry in by_stratum[stratum]:
            if len(order) >= slate_size or stratum_n[stratum] >= stratum_floor:
                break
            if repo_n[entry["repo"]] >= repo_cap:
                continue
            take(entry, "SEL_STRATUM_FLOOR")

    # -- 3. rank fill, both caps -------------------------------------------
    for entry in eligible:
        if len(order) >= slate_size:
            break
        if entry["pair_id"] in chosen:
            continue
        if repo_n[entry["repo"]] >= repo_cap:
            continue
        if stratum_n[entry["stratum"]] >= stratum_cap:
            continue
        take(entry, "SEL_RANK")

    # -- 4. relax the stratum ceiling only if short -------------------------
    if len(order) < slate_size:
        before = len(order)
        for entry in eligible:
            if len(order) >= slate_size:
                break
            if entry["pair_id"] in chosen:
                continue
            if repo_n[entry["repo"]] >= repo_cap:
                continue
            take(entry, "SEL_RELAX_STRATUM")
        if len(order) > before:
            relaxations.append(
                f"STRATUM_CAP_RELAXED: the stratum ceiling ({stratum_cap}) left the slate "
                f"at {before}/{slate_size}; {len(order) - before} further pairs were taken "
                "in rank order with the repo cap still enforced"
            )

    # -- 5. enforce the repo cap against the ACHIEVED slate ------------------
    #
    # `repo_cap` was computed from the REQUESTED size. If the slate came out
    # short, that cap is a larger share of what was actually selected, so a repo
    # could sit above `repo_cap_frac` of the real slate while every step above
    # believed it was compliant.
    #
    # The fix is NOT an iterative trim: trimming to a fixpoint collapses a
    # concentrated pool (two repos with 30 pairs each converge to a two-pair
    # slate), which trades a disclosed cap breach for a silently useless slate.
    # Instead, ask first whether the cap is satisfiable AT ALL for this pool --
    # a slate of size n needs sum_r min(count_r, floor(n * frac)) >= n -- take
    # the largest n that is, and trim once to it. When no n works the pool is
    # simply too concentrated: the slate is left intact and the breach is
    # reported (`repo_cap_respected: false`), because a disclosed breach is a
    # fact about the candidate pool and a collapsed slate is a fabrication.
    trimmed: list[dict] = []
    effective_repo_cap = repo_cap
    counts = collections.Counter(e["repo"] for e in order)
    if order and max(counts.values()) > math.floor(len(order) * repo_cap_frac):
        feasible = _max_compliant_size(sorted(counts.values()), repo_cap_frac, len(order))
        if feasible:
            effective_repo_cap = math.floor(feasible * repo_cap_frac)
            keep: list[dict] = []
            seen: collections.Counter[str] = collections.Counter()
            for entry in order:  # already in rank order
                if seen[entry["repo"]] >= effective_repo_cap:
                    trimmed.append({**entry, "rationale": ["DROP_REPO_CAP_FINAL"]})
                    # NOTE: deliberately NOT popped from `chosen`. `chosen` is
                    # what step 6 uses to decide "was this already accounted
                    # for?", and popping made a trimmed pair appear twice in
                    # `excluded` -- once as DROP_REPO_CAP_FINAL and again as
                    # DROP_RANK. The slate is built from `order`, not `chosen`.
                    continue
                seen[entry["repo"]] += 1
                keep.append(entry)
            order = keep
            relaxations.append(
                f"REPO_CAP_RETIGHTENED: the slate came out short, so the per-repo cap was "
                f"recomputed against the achieved size ({repo_cap} -> {effective_repo_cap}) "
                f"and {len(trimmed)} pair(s) were trimmed"
            )

    # -- 6. rationale for everything not selected ---------------------------
    final_repo_counts = collections.Counter(e["repo"] for e in order)
    final_stratum_counts = collections.Counter(e["stratum"] for e in order)
    stratum_bound = not any(r.startswith("STRATUM_CAP_RELAXED") for r in relaxations)
    for entry in eligible:
        if entry["pair_id"] in chosen:
            continue
        if final_repo_counts[entry["repo"]] >= effective_repo_cap:
            code = "DROP_REPO_CAP"
        elif stratum_bound and final_stratum_counts[entry["stratum"]] >= stratum_cap:
            code = "DROP_STRATUM_CAP"
        else:
            code = "DROP_RANK"
        excluded.append({**entry, "rationale": [code]})
    excluded.extend(trimmed)

    slate = [{**entry, "rationale": chosen[entry["pair_id"]]} for entry in order]
    slate.sort(key=rank_key)
    excluded.sort(key=rank_key)
    for rank, entry in enumerate(slate, start=1):
        entry["rank"] = rank

    top_share = (max(final_repo_counts.values()) / len(slate)) if slate else 0.0
    over = sorted(r for r, c in final_repo_counts.items()
                  if c > math.floor(len(slate) * repo_cap_frac))
    return {
        "slate": slate,
        "excluded": excluded,
        "relaxations": relaxations,
        "repo_cap": repo_cap,
        "repo_cap_effective": effective_repo_cap,
        "stratum_cap": stratum_cap,
        "slate_short": len(slate) < slate_size,
        "top_repo_share": top_share,
        "repo_cap_respected": not over,
        "repo_cap_violations": over,
    }


def propose_dev_split(slate: list[dict], dev_size: int) -> list[str]:
    """Pick `dev_size` slated pairs that span repositories and languages.

    Round-robin over languages (sorted), taking each language's best-ranked pair
    from a repository not yet used. When distinct repositories run out the
    constraint relaxes to two-per-repo, then to none -- each relaxation is
    deterministic and only ever fires because the slate itself is concentrated.

    This is a PROPOSAL. The dev split is drawn first and burned freely, so it
    must be representative rather than optimal; the raters may overrule it, and
    `seal.py` enforces disjointness regardless of what is proposed here.
    """
    if dev_size <= 0 or not slate:
        return []
    by_lang: dict[str, list[dict]] = collections.defaultdict(list)
    for entry in slate:
        by_lang[entry["language"]].append(entry)
    langs = sorted(by_lang)

    picked: list[str] = []
    picked_set: set[str] = set()
    repo_use: collections.Counter[str] = collections.Counter()
    for repo_limit in (1, 2, len(slate)):
        while len(picked) < dev_size:
            progress = False
            for lang in langs:
                if len(picked) >= dev_size:
                    break
                for entry in by_lang[lang]:
                    if entry["pair_id"] in picked_set:
                        continue
                    if repo_use[entry["repo"]] >= repo_limit:
                        continue
                    picked.append(entry["pair_id"])
                    picked_set.add(entry["pair_id"])
                    repo_use[entry["repo"]] += 1
                    progress = True
                    break
            if not progress:
                break
        if len(picked) >= dev_size:
            break
    return picked


def histogram(entries: list[dict], key: str) -> dict[str, int]:
    counts = collections.Counter(str(e[key]) for e in entries)
    return dict(sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])))


# --------------------------------------------------------------------------
# artifacts
# --------------------------------------------------------------------------


def verify_side(side: dict, inst: dict) -> dict:
    """Re-derive the sha256 the miner recorded from the bytes we are about to show.

    A mismatch means the sheet the rater reads is not the text the candidate was
    mined from; that is a hard stop, not a warning, because the seal hashes the
    candidate and the rater's verdict is about the sheet.
    """
    statement = str(inst.get("problem_statement") or "")
    patch = str(inst.get("patch") or "")
    return {
        "instance_id": side["instance_id"],
        "problem_statement_ok": _harness.sha256_text(statement) == side["problem_statement_sha256"],
        "patch_ok": _harness.sha256_text(patch) == side["patch_sha256"],
    }


def _fenced(text: str, lang: str) -> list[str]:
    """Fence a block with MORE backticks than the longest run inside it.

    SWE-bench problem statements are GitHub issue bodies and routinely contain
    ``` code fences of their own; a fixed 3-backtick fence would end the block
    mid-statement and the rater would silently read a mangled task.
    """
    longest = max((len(m) for m in re.findall(r"`+", text)), default=0)
    fence = "`" * max(3, longest + 1)
    return [fence + lang, text, fence]


def _truncate(text: str, limit: int, full_path: str) -> str:
    if len(text) <= limit:
        return text
    return (
        text[:limit]
        + f"\n\n[... truncated at {limit} of {len(text)} chars -- full text: {full_path}]"
    )


def write_side_artifacts(out_root: pathlib.Path, instances: dict[str, dict],
                         instance_ids: list[str]) -> dict[str, dict]:
    """Full problem statements and patches to disk, one file per instance."""
    patches = out_root / "patches"
    statements = out_root / "statements"
    patches.mkdir(parents=True, exist_ok=True)
    statements.mkdir(parents=True, exist_ok=True)
    # Clear first, like mine_pairs.write_candidates: a stale patch from a previous
    # slate that no sheet references any more is an artifact nobody can date.
    for stale in sorted(patches.glob("*.patch")) + sorted(statements.glob("*.txt")):
        stale.unlink()
    written: dict[str, dict] = {}
    for iid in sorted(set(instance_ids)):
        inst = instances.get(iid) or {}
        (patches / f"{iid}.patch").write_text(str(inst.get("patch") or ""), encoding="utf-8")
        (statements / f"{iid}.txt").write_text(
            str(inst.get("problem_statement") or ""), encoding="utf-8")
        # Sheets reference these RELATIVE to review/sheets/, so a sheet renders
        # the same whoever checked the tree out and wherever it sits on disk.
        written[iid] = {
            "patch": f"../patches/{iid}.patch",
            "statement": f"../statements/{iid}.txt",
        }
    return written


CRITERIA_BLOCK = """\
### Criteria (SEAL_PROTOCOL.md -- a reject MUST cite at least one code)

| code | accept when... | reject when... |
|---|---|---|
| `DEP` | resolving B really needs what A's session learned -- same mechanism, contract or decision, not merely the same file | the shared file is incidental; B could be solved having never seen A |
| `LEAK` | A's patch does not already contain B's fix | A's patch (or B's statement/tests) already IS B's answer -- B becomes a lookup, not rediscovery |
| `TRIV` | B needs real navigation to fix | B is a rename/typo/one-liner no memory source could help with |
| `REVEAL` | B's statement does not name the exact file/symbol/line to edit | the prompt hands over the location, so `locate_calls_pre_edit` cannot be interpreted |

Accept only if `DEP` reasonably holds AND none of `LEAK` / `TRIV` / `REVEAL`
applies strongly enough to null the comparison.
"""


def sheet_markdown(entry: dict, candidate: dict, a_inst: dict, b_inst: dict,
                   paths: dict[str, dict], dev_proposed: bool, slate_total: int) -> str:
    rank = entry["rank"]
    a = candidate["a"]
    b = candidate["b"]
    a_paths = paths[entry["a_id"]]
    b_paths = paths[entry["b_id"]]
    flags = []
    if dev_proposed:
        flags.append("**DEV-SPLIT PROPOSAL**")
    if entry["needs_human_review"]:
        flags.append("**FLAGGED: borderline patch-body overlap -- judge `LEAK` individually**")

    lines: list[str] = []
    lines.append(f"# {rank:03d} / {slate_total} -- `{entry['pair_id']}`")
    lines.append("")
    if flags:
        lines.append(" &nbsp; ".join(flags))
        lines.append("")
    lines.append(
        f"repo **`{entry['repo']}`** &nbsp;·&nbsp; pool `{entry['pool']}` &nbsp;·&nbsp; "
        f"language `{entry['language']}` ({entry['language_source']}) &nbsp;·&nbsp; "
        f"proposed split **{'dev' if dev_proposed else 'sealed'}**"
    )
    lines.append("")
    lines.append("| miner signal | value |")
    lines.append("|---|---|")
    lines.append(f"| score (file jaccard + symbol overlap) | **{entry['score']:.4f}** |")
    lines.append(f"| file jaccard | {entry['file_jaccard']:.4f} |")
    lines.append(f"| symbol overlap | {entry['symbol_overlap']:.4f} |")
    lines.append(
        f"| patch-body overlap (leakage) | **{entry['patch_body_overlap']:.4f}** "
        f"(the miner auto-rejects at {candidate['leakage']['cap']}) |"
    )
    lines.append(f"| ancestry verified (base A is an ancestor of base B) | "
                 f"{'yes' if candidate['ancestry_verified'] else 'NO'} |")
    lines.append(f"| miner flagged for individual `LEAK` judgment | "
                 f"{'YES' if entry['needs_human_review'] else 'no'} |")
    lines.append("")
    lines.append("## Files both patches touch")
    lines.append("")
    for path in entry["shared_files"]:
        lines.append(f"- `{path}`")
    if entry["shared_symbols"]:
        lines.append("")
        lines.append("shared hunk-header symbols: "
                     + ", ".join(f"`{s}`" for s in entry["shared_symbols"][:24]))
    lines.append("")
    lines.append(f"A touches: {', '.join(f'`{p}`' for p in a['files'])}")
    lines.append("")
    lines.append(f"B touches: {', '.join(f'`{p}`' for p in b['files'])}")
    lines.append("")

    for label, side, inst, side_paths in (
        ("A (the first session -- memory is derived from THIS)", a, a_inst, a_paths),
        ("B (the second session -- the graded task)", b, b_inst, b_paths),
    ):
        lines.append(f"## {label}")
        lines.append("")
        lines.append(
            f"`{side['instance_id']}` &nbsp; base `{side['base_commit'][:12]}` &nbsp; "
            f"created {side['created_at']}"
        )
        lines.append("")
        lines.append("### problem statement")
        lines.append("")
        lines.extend(_fenced(_truncate(str(inst.get("problem_statement") or "(empty)"),
                                       PROBLEM_CHARS, str(side_paths["statement"])), "text"))
        lines.append("")
        lines.append(f"### gold patch &nbsp; (sha256 `{side['patch_sha256'][:12]}`, "
                     f"full: `{side_paths['patch']}`)")
        lines.append("")
        lines.extend(_fenced(_truncate(str(inst.get("patch") or "(empty)"),
                                       PATCH_CHARS, str(side_paths["patch"])), "diff"))
        lines.append("")

    lines.append("## Verdict")
    lines.append("")
    lines.append(CRITERIA_BLOCK)
    lines.append("")
    lines.append(
        "Record in `review/REVIEW-TEMPLATE.json` (copy it to `REVIEW.json` when done) "
        f"under `pairs[\"{entry['pair_id']}\"]`. Do not read the other rater's verdict first."
    )
    lines.append("")
    lines.append("```")
    lines.append("rater_1   verdict: accept / reject    criteria: [            ]")
    lines.append("          note:")
    lines.append("")
    lines.append("rater_2   verdict: accept / reject    criteria: [            ]")
    lines.append("          note:")
    lines.append("")
    lines.append("if accepted -> split: sealed / dev"
                 + ("   (proposed: dev)" if dev_proposed else "   (proposed: sealed)"))
    lines.append("```")
    lines.append("")
    return "\n".join(lines) + "\n"


def sheets_index_markdown(slate: list[dict], dev_ids: set[str],
                          sheet_names: dict[str, str]) -> str:
    lines = [
        "# Review session checklist",
        "",
        f"{len(slate)} pairs. Budget 2-3 min each. `D` marks the proposed dev split "
        f"({len(dev_ids)} pairs), `!` marks a pair the miner flagged for individual "
        "`LEAK` judgment.",
        "",
        "| # | | pair | repo | lang | score | overlap | sheet |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for entry in slate:
        marks = ("D" if entry["pair_id"] in dev_ids else "") + \
                ("!" if entry["needs_human_review"] else "")
        lines.append(
            f"| {entry['rank']:03d} | {marks} | `{entry['pair_id']}` | `{entry['repo']}` | "
            f"{entry['language']} | {entry['score']:.3f} | {entry['patch_body_overlap']:.3f} | "
            f"[sheet](./{sheet_names[entry['pair_id']]}) |"
        )
    lines.append("")
    return "\n".join(lines) + "\n"


def build_review_template(candidates_dir: pathlib.Path, slate: list[dict], dev_ids: set[str],
                          sheet_names: dict[str, str], out_path: pathlib.Path,
                          raters: str) -> dict:
    """Emit REVIEW-TEMPLATE.json BY CALLING seal.py's own template writer.

    Hand-rolling the template here would mean two definitions of the REVIEW.json
    schema, and the one the raters spend an afternoon filling in would be the one
    that is not parsed. So a temporary candidates directory holding exactly the
    slated pairs is handed to `seal.cmd_review_template`, and the only additions
    afterwards are `_`-prefixed reader aids that `seal.py` ignores by
    construction (it reads `reviews`, `adjudication` and `split`, nothing else).
    """
    with tempfile.TemporaryDirectory() as raw:
        staged = pathlib.Path(raw) / "candidates"
        staged.mkdir(parents=True)
        for entry in slate:
            src = candidates_dir / f"{entry['pair_id']}.json"
            shutil.copyfile(src, staged / src.name)
        (staged / "INDEX.json").write_text(
            _harness.pretty_json({"candidate_pair_ids": [e["pair_id"] for e in slate]}),
            encoding="utf-8",
        )
        tmp_out = pathlib.Path(raw) / "REVIEW-TEMPLATE.json"
        args = argparse.Namespace(
            candidates=str(staged), out=str(tmp_out), force=True, raters=raters,
        )
        with contextlib.redirect_stdout(io.StringIO()):  # seal's progress line, not ours
            seal.cmd_review_template(args)
        template = json.loads(tmp_out.read_text(encoding="utf-8"))

    ranks = {e["pair_id"]: e for e in slate}
    for pair_id, block in template["pairs"].items():
        entry = ranks[pair_id]
        block["_slate_rank"] = entry["rank"]
        block["_sheet"] = f"sheets/{sheet_names[pair_id]}"
        block["_repo"] = entry["repo"]
        block["_language"] = entry["language"]
        block["_proposed_split"] = "dev" if pair_id in dev_ids else "sealed"
    template["_prescreen"] = {
        "note": "Prefilled by seal_prescreen.py. Keys prefixed with '_' are reader aids "
                "that seal.py ignores; everything else is seal.py's own review-template "
                "output, byte-for-byte. Copy this file to brainmark/REVIEW.json to submit.",
        "slate_size": len(slate),
        "dev_proposed": sorted(dev_ids),
    }
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(_harness.pretty_json(template), encoding="utf-8")
    return template


def merge_reviews(paths: list[pathlib.Path]) -> dict:
    """Merge two single-rater REVIEW files into one v2 REVIEW.json.

    WHY A MERGE STEP AT ALL: SEAL_PROTOCOL.md requires each rater to record a
    verdict before seeing the other's. One shared file cannot give that -- the
    second rater reads the first's answer on the way to their own row, and the
    kappa becomes agreement-after-anchoring. So each rater fills their OWN copy
    of the template and the files are combined mechanically here.

    Each input file is authoritative for exactly ONE rater: the rater id it has
    non-empty verdicts for. Zero or two owners is refused, because that is either
    an unstarted file or one rater filling in the other's column. A pair only
    reaches the output when BOTH raters judged it; pairs neither reached (the
    slate is deliberately longer than the review budget) are dropped and LISTED,
    never left blank -- `seal.py promote` refuses a blank verdict, and it should.
    """
    if len(paths) != 2:
        raise SystemExit(f"--merge-reviews takes exactly two files, got {len(paths)}")
    docs = []
    for path in paths:
        if not path.is_file():
            raise SystemExit(f"{path} not found")
        docs.append(json.loads(path.read_text(encoding="utf-8")))

    rater_ids = sorted((docs[0].get("raters") or {}))
    for doc, path in zip(docs, paths):
        if sorted(doc.get("raters") or {}) != rater_ids:
            raise SystemExit(
                f"{path} names raters {sorted(doc.get('raters') or {})}, expected {rater_ids}; "
                "both raters must fill in copies of the SAME template"
            )
    if len(rater_ids) != 2:
        raise SystemExit(f"expected exactly two rater ids in the template, got {rater_ids}")

    owners: list[str] = []
    for doc, path in zip(docs, paths):
        filled = sorted({
            rid for block in (doc.get("pairs") or {}).values()
            for rid, rv in (block.get("reviews") or {}).items()
            if str((rv or {}).get("verdict") or "").strip()
        })
        if len(filled) != 1:
            raise SystemExit(
                f"{path} has verdicts for {filled or 'no rater'}; each rater's file must "
                "carry verdicts for exactly one rater id (fill in YOUR row only -- the "
                "other rater's column is filled in their own copy)"
            )
        owners.append(filled[0])
    if sorted(owners) != rater_ids:
        raise SystemExit(
            f"the two files are owned by {owners}; expected one file per rater {rater_ids}"
        )

    merged = json.loads(json.dumps(docs[0]))
    merged["raters"] = {}
    for doc, owner in zip(docs, owners):
        merged["raters"][owner] = (doc.get("raters") or {}).get(owner) or {}

    all_pairs = sorted(set(docs[0].get("pairs") or {}) | set(docs[1].get("pairs") or {}))
    out_pairs: dict[str, dict] = {}
    dropped: list[str] = []
    per_rater: dict[str, int] = {rid: 0 for rid in rater_ids}
    conflicts: list[str] = []
    for pair_id in all_pairs:
        blocks = [(doc.get("pairs") or {}).get(pair_id) or {} for doc in docs]
        base = json.loads(json.dumps(blocks[0] or blocks[1]))
        verdicts = {}
        for doc, owner in zip(docs, owners):
            rv = (((doc.get("pairs") or {}).get(pair_id) or {}).get("reviews") or {}).get(owner)
            if rv and str(rv.get("verdict") or "").strip():
                verdicts[owner] = rv
                per_rater[owner] += 1
        if len(verdicts) != 2:
            dropped.append(pair_id)
            continue
        base["reviews"] = {rid: verdicts[rid] for rid in rater_ids}
        base["split"] = _one_of([b.get("split") for b in blocks], pair_id, "split", conflicts)
        adj = {}
        for key in ("resolved_verdict", "adjudicator", "note"):
            adj[key] = _one_of([(b.get("adjudication") or {}).get(key) for b in blocks],
                               pair_id, f"adjudication.{key}", conflicts)
        base["adjudication"] = adj
        out_pairs[pair_id] = base
    if conflicts:
        raise SystemExit("the two review files disagree on fields only one of them should "
                         "set:\n  " + "\n  ".join(conflicts))

    merged["pairs"] = out_pairs
    merged["_merge"] = {
        "sources": [str(p) for p in paths],
        "source_sha256": [_harness.sha256_file(p) for p in paths],
        "rater_file_owner": dict(zip(rater_ids, [str(paths[owners.index(r)]) for r in rater_ids])),
        "pairs_in_template": len(all_pairs),
        "pairs_dually_reviewed": len(out_pairs),
        "pairs_dropped_unreviewed": dropped,
        "verdicts_per_rater": per_rater,
        "note": "Pairs neither rater reached are DROPPED here, not left blank: the slate is "
                "longer than the review budget on purpose, and seal.py rightly refuses a "
                "blank verdict. The dropped list is the record of what was not reached, and "
                "SLATE.json's rank order is the pre-committed order it was worked in.",
    }
    return merged


def _one_of(values: list, pair_id: str, field: str, conflicts: list[str]):
    """The single non-empty value among `values`, or a recorded conflict."""
    present = [v for v in values if v not in (None, "", [])]
    if not present:
        return values[0] if values else ""
    first = present[0]
    if any(v != first for v in present[1:]):
        conflicts.append(f"{pair_id}: {field} = {present!r}")
    return first


def template_matches_seal_schema(template: dict, candidates_dir: pathlib.Path,
                                 slate: list[dict], raters: str) -> tuple[bool, list[str]]:
    """Strip the `_` aids and require byte-equality with seal.py's own template.

    This is the check that a mismatched template cannot waste the human session:
    it fails here, in a test, rather than at `promote` time after the verdicts
    are already written.
    """
    with tempfile.TemporaryDirectory() as raw:
        staged = pathlib.Path(raw) / "candidates"
        staged.mkdir(parents=True)
        for entry in slate:
            src = candidates_dir / f"{entry['pair_id']}.json"
            shutil.copyfile(src, staged / src.name)
        (staged / "INDEX.json").write_text(
            _harness.pretty_json({"candidate_pair_ids": [e["pair_id"] for e in slate]}),
            encoding="utf-8",
        )
        tmp_out = pathlib.Path(raw) / "ref.json"
        with contextlib.redirect_stdout(io.StringIO()):
            seal.cmd_review_template(argparse.Namespace(
                candidates=str(staged), out=str(tmp_out), force=True, raters=raters))
        reference = json.loads(tmp_out.read_text(encoding="utf-8"))

    stripped = {k: v for k, v in template.items() if k not in PRESCREEN_TEMPLATE_KEYS}
    stripped["pairs"] = {
        pid: {k: v for k, v in block.items() if k not in PRESCREEN_PAIR_KEYS}
        for pid, block in template["pairs"].items()
    }
    problems: list[str] = []
    if _harness.pretty_json(stripped) != _harness.pretty_json(reference):
        ref_keys = set(reference)
        got_keys = set(stripped)
        if ref_keys != got_keys:
            problems.append(f"top-level keys differ: missing {sorted(ref_keys - got_keys)}, "
                            f"extra {sorted(got_keys - ref_keys)}")
        for pid in sorted(reference["pairs"]):
            r_block = reference["pairs"][pid]
            g_block = stripped["pairs"].get(pid)
            if g_block is None:
                problems.append(f"pair {pid} missing from template")
            elif _harness.pretty_json(g_block) != _harness.pretty_json(r_block):
                problems.append(f"pair {pid} block differs from seal.py's template")
        if not problems:
            problems.append("template differs from seal.py's review-template output")
    return (not problems), problems


# --------------------------------------------------------------------------
# driver
# --------------------------------------------------------------------------


def run(args) -> dict:
    config = _harness.load_config(args.config)
    candidates_dir = pathlib.Path(args.candidates)
    out_root = pathlib.Path(args.out_dir)

    index = load_index(candidates_dir)
    candidates = seal.load_candidates(candidates_dir)
    pools = list(index.get("pools") or []) or None
    instances = instance_index(config, pools)

    membership = pool_membership(config, pools)
    entries = [enrich(c, instances, membership) for c in candidates]
    unresolved = sorted(e["pair_id"] for e in entries if not e["instances_resolved"])
    if unresolved:
        raise SystemExit(
            f"{len(unresolved)} candidate(s) reference instances absent from the pools "
            f"recorded in INDEX.json (pools={pools}): {unresolved[:5]}. Re-mine, or point "
            "--config at the config the candidates were mined with."
        )

    result = select_slate(
        entries,
        slate_size=args.slate_size,
        repo_cap_frac=args.repo_cap_frac,
        dup_cap=args.dup_cap,
        stratum_floor=args.stratum_floor,
        stratum_cap_frac=args.stratum_cap_frac,
    )
    slate = result["slate"]
    dev_ids = propose_dev_split(slate, args.dev_size)
    dev_set = set(dev_ids)
    for entry in slate:
        if entry["pair_id"] in dev_set:
            entry["rationale"] = entry["rationale"] + ["DEV_PROPOSED"]

    sheet_names = {
        e["pair_id"]: f"{e['rank']:03d}_{e['pair_id']}.md" for e in slate
    }

    repo_hist = histogram(slate, "repo")
    top_repo_share = (max(repo_hist.values()) / len(slate)) if slate else 0.0

    integrity = check_pool_integrity(config, index)
    by_id = {c["pair_id"]: c for c in candidates}

    # sha256 re-derivation of every byte the raters will read.
    side_checks = []
    for entry in slate:
        cand = by_id[entry["pair_id"]]
        side_checks.append(verify_side(cand["a"], instances[entry["a_id"]]))
        side_checks.append(verify_side(cand["b"], instances[entry["b_id"]]))
    bad_sides = [c for c in side_checks
                 if not (c["problem_statement_ok"] and c["patch_ok"])]

    slate_doc = {
        "schema_version": PRESCREEN_VERSION,
        "prescreen_sha256": _harness.sha256_file(pathlib.Path(__file__)),
        "miner_sha256": index.get("miner_sha256"),
        "config_sha256": config["_config_sha256"],
        "candidates_index_sha256": _harness.sha256_file(candidates_dir / "INDEX.json"),
        "candidates_dir": _harness.display_path(candidates_dir),
        "candidate_count": len(candidates),
        "pools_mined": pools or [],
        "pool_revisions": index.get("pool_revisions") or {},
        "pool_integrity": integrity,
        "pool_integrity_ok": all(c["ok"] for c in integrity) if integrity else True,
        "mining_gates": index.get("mining_gates") or {},
        "parameters": {
            "slate_size": args.slate_size,
            "dev_size": args.dev_size,
            "repo_cap_frac": args.repo_cap_frac,
            "repo_cap_requested": result["repo_cap"],
            "repo_cap_effective": result["repo_cap_effective"],
            "dup_cap": args.dup_cap,
            "stratum_floor": args.stratum_floor,
            "stratum_cap_frac": args.stratum_cap_frac,
            "stratum_cap": result["stratum_cap"],
            "stratum_definition": "(pool, language of B)",
            "rank_key": "score desc, patch_body_overlap asc, pair_id asc",
        },
        "rationale_codes": dict(RATIONALE_CODES),
        "relaxations": result["relaxations"],
        "slate_short": result["slate_short"],
        "slate_size_achieved": len(slate),
        "composition": {
            "by_repo": repo_hist,
            "by_pool": histogram(slate, "pool"),
            "by_language": histogram(slate, "language"),
            "by_stratum": histogram(slate, "stratum"),
            "distinct_repos": len(repo_hist),
            "top_repo": next(iter(repo_hist), None),
            "top_repo_count": max(repo_hist.values()) if repo_hist else 0,
            "top_repo_share": round(top_repo_share, 6),
            "repo_cap_respected": result["repo_cap_respected"],
            "repo_cap_violations": result["repo_cap_violations"],
            "flagged_needs_human_review": sum(1 for e in slate if e["needs_human_review"]),
        },
        "candidate_pool_composition": {
            "by_repo": histogram(entries, "repo"),
            "by_pool": histogram(entries, "pool"),
            "by_language": histogram(entries, "language"),
        },
        "dev_split_proposal": {
            "size": len(dev_ids),
            "requested": args.dev_size,
            "pair_ids": sorted(dev_ids),
            "by_repo": histogram([e for e in slate if e["pair_id"] in dev_set], "repo"),
            "by_language": histogram([e for e in slate if e["pair_id"] in dev_set], "language"),
            "rule": "round-robin over languages, best-ranked pair from an unused repo; "
                    "PROPOSAL ONLY -- the raters assign the split, seal.py enforces "
                    "sealed/dev disjointness",
        },
        "sha256_recheck": {
            "sides_checked": len(side_checks),
            "mismatches": bad_sides,
            "ok": not bad_sides,
        },
        "exclusion_summary": dict(sorted(collections.Counter(
            code for e in result["excluded"] for code in e["rationale"]
        ).items())),
        # No `sheet` field when --no-sheets: a recorded path to a file that was
        # never written is worse than no path.
        "slate": [_public(e, None if args.no_sheets else sheet_names.get(e["pair_id"]))
                  for e in slate],
        "excluded": [_public(e, None) for e in result["excluded"]],
    }

    if args.summary_only:
        return {"slate_doc": slate_doc, "slate": slate, "dev_ids": dev_ids,
                "template": None, "template_ok": None}

    if bad_sides:
        raise SystemExit(
            f"REFUSING to write review sheets: {len(bad_sides)} problem-statement/patch "
            "sha256 mismatches between the pool cache and the mined candidates. The sheets "
            "would show text the candidates were not mined from."
        )

    out_root.mkdir(parents=True, exist_ok=True)
    sheets_dir = out_root / "sheets"

    template = None
    template_ok = None
    template_problems: list[str] = []
    if not args.no_template:
        template = build_review_template(
            candidates_dir, slate, dev_set, sheet_names,
            out_root / "REVIEW-TEMPLATE.json", args.raters,
        )
        template_ok, template_problems = template_matches_seal_schema(
            template, candidates_dir, slate, args.raters)
        # One pre-copied file per rater. Independence at the point of judgment is
        # a SEAL_PROTOCOL requirement that a single shared file cannot deliver:
        # the second rater would read the first's verdict on the way to their own
        # row. `--merge-reviews` recombines them.
        rater_files = []
        for rid in [r.strip() for r in args.raters.split(",") if r.strip()]:
            per_rater = out_root / f"REVIEW-{rid}.json"
            shutil.copyfile(out_root / "REVIEW-TEMPLATE.json", per_rater)
            rater_files.append(per_rater.name)
        slate_doc["review_template"] = {
            # Relative on purpose: SLATE.json must not change bytes just because
            # --out-dir moved, or the determinism test proves nothing.
            "path": "REVIEW-TEMPLATE.json",
            "raters": args.raters.split(","),
            "per_rater_copies": rater_files,
            "matches_seal_review_template": template_ok,
            "problems": template_problems,
        }

    if not args.no_sheets:
        if sheets_dir.exists():
            for stale in sorted(sheets_dir.glob("*.md")):
                stale.unlink()
        sheets_dir.mkdir(parents=True, exist_ok=True)
        paths = write_side_artifacts(
            out_root, instances,
            [e["a_id"] for e in slate] + [e["b_id"] for e in slate],
        )
        for entry in slate:
            cand = by_id[entry["pair_id"]]
            text = sheet_markdown(
                entry, cand, instances[entry["a_id"]], instances[entry["b_id"]],
                paths, entry["pair_id"] in dev_set, len(slate),
            )
            (sheets_dir / sheet_names[entry["pair_id"]]).write_text(text, encoding="utf-8")
        (sheets_dir / "INDEX.md").write_text(
            sheets_index_markdown(slate, dev_set, sheet_names), encoding="utf-8")

    (out_root / "SLATE.json").write_text(_harness.pretty_json(slate_doc), encoding="utf-8")
    if template_ok is False:
        raise SystemExit(
            "REVIEW-TEMPLATE.json does not match seal.py's own review-template output:\n  "
            + "\n  ".join(template_problems)
        )
    return {"slate_doc": slate_doc, "slate": slate, "dev_ids": dev_ids,
            "template": template, "template_ok": template_ok}


def _public(entry: dict, sheet: str | None) -> dict:
    out = {
        "pair_id": entry["pair_id"],
        "repo": entry["repo"],
        "pool": entry["pool"],
        "language": entry["language"],
        "language_source": entry["language_source"],
        "stratum": entry["stratum"],
        "score": entry["score"],
        "file_jaccard": entry["file_jaccard"],
        "symbol_overlap": entry["symbol_overlap"],
        "patch_body_overlap": entry["patch_body_overlap"],
        "needs_human_review": entry["needs_human_review"],
        "shared_files": entry["shared_files"],
        "a_instance_id": entry["a_id"],
        "b_instance_id": entry["b_id"],
        "rationale": entry["rationale"],
    }
    if "rank" in entry:
        out["rank"] = entry["rank"]
    if sheet:
        out["sheet"] = f"sheets/{sheet}"
    return out


def format_summary(doc: dict) -> str:
    comp = doc["composition"]
    lines = ["BrainMark seal prescreen", "=" * 68]
    lines.append(f"candidates in           {doc['candidate_count']}")
    lines.append(f"slate                   {doc['slate_size_achieved']} "
                 f"(requested {doc['parameters']['slate_size']}"
                 f"{', SHORT' if doc['slate_short'] else ''})")
    lines.append(f"distinct repos          {comp['distinct_repos']}")
    lines.append(f"top repo                {comp['top_repo']} = {comp['top_repo_count']} "
                 f"({comp['top_repo_share'] * 100:.2f}%, cap "
                 f"{doc['parameters']['repo_cap_frac'] * 100:.0f}%) -> "
                 f"{'OK' if comp['repo_cap_respected'] else 'VIOLATED'}")
    lines.append(f"flagged (borderline)    {comp['flagged_needs_human_review']}")
    lines.append(f"dev-split proposal      {doc['dev_split_proposal']['size']} pairs across "
                 f"{len(doc['dev_split_proposal']['by_repo'])} repos, "
                 f"{len(doc['dev_split_proposal']['by_language'])} languages")
    lines.append("")
    lines.append("by pool:     " + ", ".join(f"{k}={v}" for k, v in comp["by_pool"].items()))
    lines.append("by language: " + ", ".join(f"{k}={v}" for k, v in comp["by_language"].items()))
    lines.append("")
    lines.append("top repos:   " + ", ".join(
        f"{k}={v}" for k, v in list(comp["by_repo"].items())[:10]))
    lines.append("")
    lines.append("exclusions:  " + ", ".join(
        f"{k}={v}" for k, v in doc["exclusion_summary"].items()))
    for note in doc["relaxations"]:
        lines.append(f"RELAXED: {note}")
    if doc.get("review_template"):
        lines.append("")
        lines.append("review template vs seal.py parser: "
                     + ("MATCH" if doc["review_template"]["matches_seal_review_template"]
                        else "MISMATCH " + "; ".join(doc["review_template"]["problems"])))
    lines.append("")
    lines.append("sha256 recheck of every statement/patch shown: "
                 + ("OK" if doc["sha256_recheck"]["ok"]
                    else f"{len(doc['sha256_recheck']['mismatches'])} MISMATCHES"))
    lines.append("pool cache integrity vs INDEX.json: "
                 + ("OK" if doc["pool_integrity_ok"] else "MISMATCH"))
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Deterministically choose the BrainMark human-review slate.")
    parser.add_argument("--config", default=None)
    parser.add_argument("--candidates", default=str(_harness.BRAINMARK_DIR / "candidates"))
    parser.add_argument("--out-dir", default=str(_harness.BRAINMARK_DIR / "review"))
    parser.add_argument("--slate-size", type=int, default=DEFAULT_SLATE_SIZE)
    parser.add_argument("--dev-size", type=int, default=DEFAULT_DEV_SIZE)
    parser.add_argument("--repo-cap-frac", type=float, default=DEFAULT_REPO_CAP_FRAC)
    parser.add_argument("--dup-cap", type=int, default=DEFAULT_DUP_CAP)
    parser.add_argument("--stratum-floor", type=int, default=DEFAULT_STRATUM_FLOOR)
    parser.add_argument("--stratum-cap-frac", type=float, default=DEFAULT_STRATUM_CAP_FRAC)
    parser.add_argument("--raters", default="rater_1,rater_2")
    parser.add_argument("--no-sheets", action="store_true")
    parser.add_argument("--no-template", action="store_true")
    parser.add_argument("--summary-only", action="store_true",
                        help="decide and print; write nothing")
    parser.add_argument("--json", action="store_true", help="print SLATE.json to stdout")
    parser.add_argument("--merge-reviews", nargs=2, metavar=("RATER_A_FILE", "RATER_B_FILE"),
                        default=None,
                        help="merge two single-rater review files into one v2 REVIEW.json "
                             "and exit; nothing else is read or written")
    parser.add_argument("--merge-out", default=str(_harness.BRAINMARK_DIR / "REVIEW.json"))
    args = parser.parse_args(argv)

    if args.merge_reviews:
        merged = merge_reviews([pathlib.Path(p) for p in args.merge_reviews])
        out_path = pathlib.Path(args.merge_out)
        if out_path.exists():
            raise SystemExit(f"{out_path} exists; move it aside rather than overwriting a "
                             "review file that may already be hashed into a seal")
        out_path.write_text(_harness.pretty_json(merged), encoding="utf-8")
        info = merged["_merge"]
        print(f"merged -> {out_path}")
        print(f"  dually reviewed : {info['pairs_dually_reviewed']} of "
              f"{info['pairs_in_template']} template pairs")
        print(f"  per rater       : "
              + ", ".join(f"{k}={v}" for k, v in sorted(info["verdicts_per_rater"].items())))
        print(f"  dropped (neither rater reached): {len(info['pairs_dropped_unreviewed'])}")
        print("  next: python3 seal.py promote  (add --allow-short only for a declared pilot)")
        return 0

    out = run(args)
    if args.json:
        print(_harness.pretty_json(out["slate_doc"]), end="")
    else:
        print(format_summary(out["slate_doc"]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
