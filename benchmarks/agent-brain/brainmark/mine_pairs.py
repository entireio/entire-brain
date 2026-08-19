#!/usr/bin/env python3
"""BrainMark pair miner -- deterministic, offline, $0.

Finds ordered SWE-bench instance pairs (A, B) inside one repository where a
session that fixed A plausibly learned something reusable for B.

Gates (ALL must hold; none of them may be loosened to hit a candidate count):

  1. same repo
  2. A strictly precedes B in (created_at, instance_id) order
  3. >= `min_shared_files` files touched by BOTH gold patches
  4. `git merge-base --is-ancestor base_A base_B` in the local repo cache
     -- an UNVERIFIABLE repo (not cloned locally, or a commit missing from the
     clone) is NOT a pass. Those pairs are reported separately and never emitted.
  5. leakage screen: patch-body token overlap < `leakage_overlap_max`
     -- if A's fix already contains B's fix, B is not a second task, it is a
     lookup. Pairs at or above the cap are REJECTED. Pairs in
     [borderline_min, max) are emitted but flagged for human seal review.
  6. one best A per B

Score = w_files * Jaccard(files) + w_symbols * Jaccard(hunk-header symbols).

DETERMINISM CONTRACT: byte-identical output across runs on identical inputs.
Nothing wallclock-derived, PID-derived, hash-seed-derived, or filesystem-order-
derived may reach the output. Tests enforce this by running the miner twice,
in local-JSON mode and in pool mode.

INSTANCE SOURCES
  * local task JSONs -- `graphmark_root/tasks/*.json`, always loaded.
  * HF pools -- `--pool NAME`, read from the offline cache written by
    `pool_loaders.py` (SWE-bench / _Verified / _Multilingual). Pools are merged
    in (priority, name) order, local JSONs first, first definition of an
    instance_id wins, so neither flag order nor filesystem order can move a
    result. Each pool's HF revision sha lands in `candidates/INDEX.json`.

Usage:
    python3 mine_pairs.py                       # mine into ./candidates
    python3 mine_pairs.py --out DIR --config C
    python3 mine_pairs.py --summary-only        # counts, write nothing
    python3 mine_pairs.py --pool swe_bench_verified --pool swe_bench_multilingual
    python3 mine_pairs.py --all-pools           # every enabled pool in config
"""

from __future__ import annotations

import argparse
import collections
import json
import pathlib
import re
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, pool_loaders  # type: ignore[no-redef]
else:
    from . import _harness, pool_loaders

MINER_VERSION = 1

# `diff --git a/<path> b/<path>`; the b-side is authoritative for renames/adds.
_DIFF_GIT = re.compile(r"^diff --git a/(?P<a>.+?) b/(?P<b>.+)$", re.MULTILINE)
# `@@ -1,2 +3,4 @@ <section heading>` -- the trailing heading is the enclosing
# symbol as git's funcname heuristic saw it.
_HUNK = re.compile(r"^@@ [^@]*@@\s*(?P<sym>.*)$", re.MULTILINE)
_WORD = re.compile(r"[A-Za-z_][A-Za-z0-9_]{2,}")


# --------------------------------------------------------------------------
# patch parsing
# --------------------------------------------------------------------------


def patch_files(patch: str) -> set[str]:
    """Every path a patch touches, both sides (renames touch two)."""
    files: set[str] = set()
    for m in _DIFF_GIT.finditer(patch or ""):
        for side in ("a", "b"):
            path = m.group(side).strip()
            if path and path != "/dev/null":
                files.add(path)
    return files


def patch_symbols(patch: str) -> set[str]:
    """Identifier-ish tokens from hunk-header section headings."""
    symbols: set[str] = set()
    for m in _HUNK.finditer(patch or ""):
        for tok in _WORD.findall(m.group("sym") or ""):
            symbols.add(tok.lower())
    return symbols


def patch_body_tokens(patch: str) -> set[str]:
    """Identifier tokens from the ADDED/REMOVED lines only.

    Metadata lines (diff/index/---/+++/@@) are excluded: they carry paths, which
    the file gate already scores, and counting them would make every same-file
    pair look like a leak.
    """
    tokens: set[str] = set()
    for line in (patch or "").splitlines():
        if not line or line[0] not in "+-":
            continue
        if line.startswith(("+++", "---")):
            continue
        for tok in _WORD.findall(line[1:]):
            tokens.add(tok.lower())
    return tokens


def jaccard(left: set[str], right: set[str]) -> float:
    if not left and not right:
        return 0.0
    union = len(left | right)
    return (len(left & right) / union) if union else 0.0


def overlap_coefficient(left: set[str], right: set[str]) -> float:
    """Szymkiewicz-Simpson containment.

    The leakage question is "is A's patch body already B's patch body?", i.e.
    CONTAINMENT, not similarity. Jaccard would let a large A hide a total
    containment of a small B behind a low score, so containment is the correct
    screen and the strict one.
    """
    if not left or not right:
        return 0.0
    return len(left & right) / min(len(left), len(right))


# --------------------------------------------------------------------------
# task pool
# --------------------------------------------------------------------------


def load_instances(graphmark_root: pathlib.Path, globs: list[str]) -> tuple[dict, list[dict]]:
    """Load every task JSON into one instance pool, deterministically.

    Files are visited in sorted path order and the FIRST definition of an
    instance_id wins, so a duplicated instance across task files cannot make the
    pool depend on filesystem iteration order.
    """
    pool: dict[str, dict] = {}
    provenance: list[dict] = []
    paths: list[pathlib.Path] = []
    for pattern in sorted(globs):
        paths.extend(sorted(graphmark_root.glob(pattern)))
    for path in paths:
        payload = json.loads(path.read_text(encoding="utf-8"))
        instances = payload.get("instances")
        if not isinstance(instances, list):
            continue
        added = 0
        for inst in instances:
            iid = inst.get("instance_id")
            if not isinstance(iid, str) or not iid or iid in pool:
                continue
            pool[iid] = inst
            added += 1
        provenance.append({
            "path": str(path.relative_to(graphmark_root)),
            "sha256": _harness.sha256_file(path),
            "dataset_id": payload.get("dataset_id"),
            "instances_in_file": len(instances),
            "instances_new": added,
        })
    return pool, provenance


def cache_dir_for(repo_cache: pathlib.Path, repo: str) -> pathlib.Path:
    """graphmark convention (run_3arm.sh:56): repo-cache/<owner>_<name>."""
    return repo_cache / repo.replace("/", "_")


class AncestryOracle:
    """`git merge-base --is-ancestor` over the graphmark repo cache.

    Answers are memoized per (repo, a, b) so the miner is deterministic and
    cheap; a repo with no local clone, or a commit the clone does not have, is
    reported as UNVERIFIABLE and never counts as a pass.
    """

    def __init__(self, repo_cache: pathlib.Path) -> None:
        self.repo_cache = repo_cache
        self._ancestor: dict[tuple[str, str, str], bool] = {}
        self._has_commit: dict[tuple[str, str], bool] = {}
        self.repos_present: dict[str, bool] = {}

    def repo_available(self, repo: str) -> bool:
        if repo not in self.repos_present:
            path = cache_dir_for(self.repo_cache, repo)
            self.repos_present[repo] = (path / ".git").exists() or (path / "HEAD").exists()
        return self.repos_present[repo]

    def has_commit(self, repo: str, commit: str) -> bool:
        key = (repo, commit)
        if key not in self._has_commit:
            if not self.repo_available(repo):
                self._has_commit[key] = False
            else:
                proc = subprocess.run(
                    ["git", "-C", str(cache_dir_for(self.repo_cache, repo)),
                     "cat-file", "-e", f"{commit}^{{commit}}"],
                    capture_output=True, check=False,
                )
                self._has_commit[key] = proc.returncode == 0
        return self._has_commit[key]

    def is_ancestor(self, repo: str, older: str, newer: str) -> bool | None:
        """True / False / None where None == cannot be verified locally."""
        if not self.has_commit(repo, older) or not self.has_commit(repo, newer):
            return None
        key = (repo, older, newer)
        if key not in self._ancestor:
            proc = subprocess.run(
                ["git", "-C", str(cache_dir_for(self.repo_cache, repo)),
                 "merge-base", "--is-ancestor", older, newer],
                capture_output=True, check=False,
            )
            if proc.returncode not in (0, 1):
                return None
            self._ancestor[key] = proc.returncode == 0
        return self._ancestor[key]


# --------------------------------------------------------------------------
# mining
# --------------------------------------------------------------------------

REJECT_REASONS = (
    "no_shared_files",
    "not_ancestor",
    "unverifiable_ancestry",
    "leakage_overlap",
    "not_best_a_for_b",
)


def load_all_instances(
    config: dict, pools: list[str] | None = None
) -> tuple[dict[str, dict], list[dict]]:
    """Local task JSONs first, then the requested cached HF pools.

    First-wins dedup with a fixed source order: the hand-curated graphmark task
    JSONs outrank a bulk HF dump of the same instance_id, and the merge cannot
    depend on the order the `--pool` flags were typed.
    """
    graphmark_root = pathlib.Path(config["graphmark_root"])
    pool, sources = load_instances(graphmark_root, config["task_globs"])
    if not pools:
        return pool, sources
    hf_pool, hf_sources = pool_loaders.load_pools(config, pools)
    counted: dict[str, int] = {}
    for iid in sorted(hf_pool):
        if iid in pool:
            continue
        inst = hf_pool[iid]
        pool[iid] = inst
        name = str(inst.get("pool") or "")
        counted[name] = counted.get(name, 0) + 1
    merged_sources = list(sources)
    for prov in hf_sources:
        prov = dict(prov)
        # `instances_new` from load_pools is new-within-the-pool-merge; recount
        # against the local JSONs so the index says what each pool actually added.
        prov["instances_new"] = counted.get(str(prov.get("pool")), 0)
        merged_sources.append(prov)
    return pool, merged_sources


def mine(config: dict, oracle: AncestryOracle | None = None, _diagnostic: bool = False,
         pools: list[str] | None = None) -> dict:
    """Mine candidates from the local task JSONs plus any cached HF pools.

    `_diagnostic=True` re-runs the pipeline treating UNVERIFIABLE ancestry as a
    provisional pass, to answer the operational question "how many candidates
    would exist if the repo cache were complete?". Its output is a PLANNING
    number only: it is reported, never emitted as candidates, and never sealed.
    """
    repo_cache = pathlib.Path(config["repo_cache"])
    pool, sources = load_all_instances(config, pools)
    oracle = oracle if oracle is not None else AncestryOracle(repo_cache)
    result = mine_pool(pool, config, oracle, _diagnostic=_diagnostic)
    result["sources"] = sources
    result["pools"] = pool_loaders.sort_pools(config, pools) if pools else []
    result["pool_revisions"] = pool_loaders.pool_revisions(sources)
    return result


def mine_pool(pool: dict[str, dict], config: dict, oracle: AncestryOracle,
              _diagnostic: bool = False,
              b_filter=None) -> dict:
    """The gates. Shared verbatim with `mine_fresh.py` -- never re-implemented.

    `b_filter(instance) -> bool` restricts which instances may serve as the B
    side. The fresh-split miner uses it to require that B is a post-cutoff PR
    while still allowing A to come from the historical pool.
    """
    mining = config["mining"]
    weights = mining["score_weights"]
    min_shared = int(mining["min_shared_files"])
    leak_max = float(mining["leakage_overlap_max"])
    leak_borderline = float(mining["leakage_borderline_min"])
    require_ancestor = bool(mining["require_ancestor"])

    by_repo: dict[str, list[dict]] = collections.defaultdict(list)
    for inst in pool.values():
        by_repo[str(inst.get("repo", ""))].append(inst)

    # Precompute per-instance derived sets once.
    derived: dict[str, dict] = {}
    for iid, inst in pool.items():
        patch = inst.get("patch") or ""
        derived[iid] = {
            "files": patch_files(patch),
            "symbols": patch_symbols(patch),
            "body": patch_body_tokens(patch),
        }

    rejects: collections.Counter[str] = collections.Counter()
    per_repo: dict[str, dict] = {}
    best_for_b: dict[str, dict] = {}
    unverifiable_repos: collections.Counter[str] = collections.Counter()

    for repo in sorted(by_repo):
        instances = sorted(
            by_repo[repo],
            key=lambda i: (str(i.get("created_at") or ""), str(i.get("instance_id"))),
        )
        repo_stats = {
            "instances": len(instances),
            "ordered_pairs": 0,
            "shared_file_pairs": 0,
            "ancestor_pairs": 0,
            "unverifiable_pairs": 0,
            "leak_rejected_pairs": 0,
            "surviving_pairs": 0,
            "candidates": 0,
            "repo_cached": oracle.repo_available(repo),
        }
        repo_best: dict[str, dict] = {}

        for bi in range(len(instances)):
            inst_b = instances[bi]
            if b_filter is not None and not b_filter(inst_b):
                continue
            b_id = str(inst_b["instance_id"])
            for ai in range(bi):
                inst_a = instances[ai]
                a_id = str(inst_a["instance_id"])
                repo_stats["ordered_pairs"] += 1

                da, db = derived[a_id], derived[b_id]
                shared = da["files"] & db["files"]
                if len(shared) < min_shared:
                    rejects["no_shared_files"] += 1
                    continue
                repo_stats["shared_file_pairs"] += 1

                if require_ancestor:
                    verdict = oracle.is_ancestor(
                        repo, str(inst_a["base_commit"]), str(inst_b["base_commit"])
                    )
                    if verdict is None:
                        repo_stats["unverifiable_pairs"] += 1
                        unverifiable_repos[repo] += 1
                        if not _diagnostic:
                            rejects["unverifiable_ancestry"] += 1
                            continue
                        # DIAGNOSTIC ONLY: provisional pass so the planning
                        # count reflects a complete repo cache. Never sealed.
                        verdict = True
                    if not verdict:
                        rejects["not_ancestor"] += 1
                        continue
                    repo_stats["ancestor_pairs"] += 1

                leak = overlap_coefficient(da["body"], db["body"])
                if leak >= leak_max:
                    rejects["leakage_overlap"] += 1
                    repo_stats["leak_rejected_pairs"] += 1
                    continue
                repo_stats["surviving_pairs"] += 1

                file_j = jaccard(da["files"], db["files"])
                sym_j = jaccard(da["symbols"], db["symbols"])
                score = weights["file_jaccard"] * file_j + weights["symbol_overlap"] * sym_j

                cand = {
                    "schema_version": MINER_VERSION,
                    "pair_id": f"{a_id}__then__{b_id}",
                    "repo": repo,
                    "a": _side(inst_a, da),
                    "b": _side(inst_b, db),
                    "shared_files": sorted(shared),
                    "score": round(score, 6),
                    "score_components": {
                        "file_jaccard": round(file_j, 6),
                        "symbol_overlap": round(sym_j, 6),
                    },
                    "shared_symbols": sorted(da["symbols"] & db["symbols"]),
                    "leakage": {
                        "patch_body_overlap": round(leak, 6),
                        "cap": leak_max,
                        "borderline": leak >= leak_borderline,
                    },
                    "ancestry_verified": bool(require_ancestor),
                    "needs_human_review": leak >= leak_borderline,
                }

                prev = repo_best.get(b_id)
                if prev is None or _better(cand, prev):
                    if prev is not None:
                        rejects["not_best_a_for_b"] += 1
                    repo_best[b_id] = cand
                else:
                    rejects["not_best_a_for_b"] += 1

        repo_stats["candidates"] = len(repo_best)
        per_repo[repo] = repo_stats
        best_for_b.update(repo_best)

    candidates = sorted(best_for_b.values(), key=lambda c: (-c["score"], c["pair_id"]))

    return {
        "schema_version": MINER_VERSION,
        "miner_sha256": _harness.sha256_file(pathlib.Path(__file__)),
        "config_sha256": config.get("_config_sha256"),
        "mining_gates": {
            "min_shared_files": min_shared,
            "leakage_overlap_max": leak_max,
            "leakage_borderline_min": leak_borderline,
            "require_ancestor": require_ancestor,
            "one_a_per_b": bool(mining["one_a_per_b"]),
            "score_weights": weights,
        },
        "sources": [],
        "pools": [],
        "pool_revisions": {},
        "pool_size": len(pool),
        "per_repo": per_repo,
        "rejects": {reason: rejects.get(reason, 0) for reason in REJECT_REASONS},
        "unverifiable_repos": dict(sorted(unverifiable_repos.items())),
        "candidate_count": len(candidates),
        "candidates": candidates,
    }


def _side(inst: dict, derived: dict) -> dict:
    return {
        "instance_id": str(inst["instance_id"]),
        "base_commit": str(inst["base_commit"]),
        "created_at": str(inst.get("created_at") or ""),
        "version": str(inst.get("version") or ""),
        "language": str(inst.get("language") or ""),
        "files": sorted(derived["files"]),
        "problem_statement_sha256": _harness.sha256_text(str(inst.get("problem_statement") or "")),
        "patch_sha256": _harness.sha256_text(str(inst.get("patch") or "")),
    }


def _better(cand: dict, prev: dict) -> bool:
    """Deterministic total order: score desc, then lower leakage, then id asc."""
    key_new = (-cand["score"], cand["leakage"]["patch_body_overlap"], cand["pair_id"])
    key_old = (-prev["score"], prev["leakage"]["patch_body_overlap"], prev["pair_id"])
    return key_new < key_old


# --------------------------------------------------------------------------
# output
# --------------------------------------------------------------------------


def write_candidates(result: dict, out_dir: pathlib.Path,
                     preserve: tuple[str, ...] = ()) -> list[pathlib.Path]:
    """Rewrite `out_dir` from scratch, minus `preserve`.

    Clearing the directory is what stops a candidate that a later, stricter gate
    would reject from surviving as a stale file. `preserve` exists for the one
    file that is INPUT rather than output -- the fresh miner's pinned
    `SNAPSHOT.json`, which lives beside its candidates and must not be deleted
    by the run that reads it. Subdirectories are never touched.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    keep = set(preserve)
    for stale in sorted(out_dir.glob("*.json")):
        if stale.name in keep:
            continue
        stale.unlink()
    written: list[pathlib.Path] = []
    for cand in result["candidates"]:
        path = out_dir / f"{cand['pair_id']}.json"
        path.write_text(_harness.pretty_json(cand), encoding="utf-8")
        written.append(path)
    index = {k: v for k, v in result.items() if k != "candidates"}
    index["candidate_pair_ids"] = [c["pair_id"] for c in result["candidates"]]
    index["candidate_file_sha256"] = {
        c["pair_id"]: _harness.sha256_file(out_dir / f"{c['pair_id']}.json")
        for c in result["candidates"]
    }
    index_path = out_dir / "INDEX.json"
    index_path.write_text(_harness.pretty_json(index), encoding="utf-8")
    written.append(index_path)
    return written


def format_summary(result: dict, target: int, diagnostic: dict | None = None) -> str:
    lines = ["BrainMark pair miner", "=" * 68]
    lines.append(f"instance pool: {result['pool_size']}")
    for source in result.get("sources") or []:
        if source.get("kind") == "hf_pool":
            lines.append(
                f"  pool {str(source.get('pool')):<24} {source.get('instances_in_file'):>6} rows "
                f"(+{source.get('instances_new')} new)  rev={source.get('revision')}"
            )
    lines.append("")
    lines.append(f"{'repo':<34} {'inst':>5} {'shared':>7} {'anc':>5} {'leak':>5} {'CAND':>5}  cached")
    lines.append("-" * 78)
    for repo in sorted(result["per_repo"], key=lambda r: (-result["per_repo"][r]["candidates"], r)):
        s = result["per_repo"][repo]
        if s["shared_file_pairs"] == 0 and s["candidates"] == 0:
            continue
        lines.append(
            f"{repo:<34} {s['instances']:>5} {s['shared_file_pairs']:>7} "
            f"{s['ancestor_pairs']:>5} {s['leak_rejected_pairs']:>5} {s['candidates']:>5}  "
            f"{'yes' if s['repo_cached'] else 'NO'}"
        )
    lines.append("-" * 78)
    lines.append(f"{'TOTAL CANDIDATES':<34} {result['candidate_count']:>39}")
    lines.append("")
    lines.append("rejects: " + ", ".join(f"{k}={v}" for k, v in result["rejects"].items()))
    if result["unverifiable_repos"]:
        total_unver = sum(result["unverifiable_repos"].values())
        lines.append(
            f"UNVERIFIABLE ancestry (repo not in repo-cache): {total_unver} pairs across "
            f"{len(result['unverifiable_repos'])} repos -- excluded, never emitted."
        )
    flagged = sum(1 for c in result["candidates"] if c["needs_human_review"])
    lines.append(f"flagged for human seal review (borderline leakage): {flagged}")
    lines.append("")
    if result["candidate_count"] >= target:
        lines.append(f"GO: {result['candidate_count']} >= target {target}")
    else:
        lines.append(
            f"SHORTFALL: {result['candidate_count']} candidates < target {target}. "
            "Gates were NOT loosened."
        )
    if diagnostic is not None:
        missing = sorted(
            repo for repo, stats in result["per_repo"].items()
            if not stats["repo_cached"] and stats["shared_file_pairs"] > 0
        )
        lines.append("")
        lines.append("PLANNING DIAGNOSTIC (not candidates, never sealed):")
        lines.append(
            f"  with a COMPLETE repo cache the same gates would yield "
            f"~{diagnostic['candidate_count']} candidates "
            f"(+{diagnostic['candidate_count'] - result['candidate_count']})."
        )
        lines.append(
            f"  blocked purely by missing clones: {len(missing)} repos -> "
            + ", ".join(missing[:8]) + (" ..." if len(missing) > 8 else "")
        )
        lines.append("  ancestry there is UNVERIFIED; clone them and re-mine to confirm.")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Mine BrainMark (A, B) task pairs.")
    parser.add_argument("--config", default=None)
    parser.add_argument("--out", default=None, help="candidates directory (default ./candidates)")
    parser.add_argument("--summary-only", action="store_true")
    parser.add_argument("--json", action="store_true", help="print the index JSON to stdout")
    parser.add_argument(
        "--no-diagnostic", action="store_true",
        help="skip the 'if the repo cache were complete' planning count",
    )
    parser.add_argument(
        "--pool", action="append", default=None,
        help="also mine this cached HF pool (repeatable); see pool_loaders.py",
    )
    parser.add_argument(
        "--all-pools", action="store_true",
        help="mine every pool marked enabled in config.pools.registry",
    )
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    pools = list(args.pool or [])
    if args.all_pools:
        pools = sorted(set(pools) | set(pool_loaders.enabled_pools(config)))
    pools = pools or None

    result = mine(config, pools=pools)
    diagnostic = None if args.no_diagnostic else mine(config, _diagnostic=True, pools=pools)

    if not args.summary_only:
        out_dir = pathlib.Path(args.out) if args.out else _harness.BRAINMARK_DIR / "candidates"
        write_candidates(result, out_dir)

    if args.json:
        index = {k: v for k, v in result.items() if k != "candidates"}
        print(_harness.pretty_json(index), end="")
    else:
        print(format_summary(result, int(config["mining"]["target_candidates"]), diagnostic))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
