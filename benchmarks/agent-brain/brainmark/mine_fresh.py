#!/usr/bin/env python3
"""Fresh-split miner -- SWE-rebench-style pairs from POST-TRAINING-CUTOFF PRs.

Why this exists (plan 0.2 / 0.3). Every SWE-bench instance predates every model
we will run, so a reviewer can ask whether a result is memorization rather than
memory. The structural answer is that contamination hits both arms of a pair
identically and therefore biases the WITHIN-PAIR delta toward null -- but the
empirical answer is a split the models cannot have seen. This miner builds it
from merged pull requests after `fresh.training_cutoff`, using the same gates as
`mine_pairs.py`, over repositories that are ALREADY cloned in the repo cache.

The output is a ROBUSTNESS split. It is pre-registered as analyzed separately
and is NEVER pooled with the main candidates: it lives in `candidates/fresh/`,
carries `"split": "fresh"` in every record, and `write_candidates` is called on
its own directory.

Gates, in order:
  1. merged after the training cutoff, into the repo's default branch
  2. a LINKED ISSUE (a GitHub closing keyword in the PR body/title) -- without a
     problem statement written independently of the fix there is no task
  3. a TEST-FILE DELTA -- without tests there is nothing to grade B against
  4. then the identical `mine_pairs.mine_pool` gates: shared file with an
     earlier task, `git merge-base --is-ancestor`, leakage cap, one best A per B

DETERMINISM BOUNDARY -- read this before quoting a fresh number.
  GitHub is a moving target: the same query re-run tomorrow returns more PRs.
  So the FIRST run writes `candidates/fresh/SNAPSHOT.json` -- the pinned PR list
  with every diff it used -- and every later run reads that file and never
  contacts GitHub. The miner is byte-deterministic GIVEN THE SNAPSHOT; the
  snapshot itself is the (dated, sha256-pinned, committed) boundary. Re-freeze
  deliberately with `--refresh-snapshot`, which is a prereg-visible act, not a
  routine one.

`gh` is EXEC-GUARDED: with no `gh` on PATH, or no authenticated account, this
script prints a SKIP and exits 0. Nothing else in BrainMark depends on it.

OPERATIONAL PREREQUISITE. The ancestry gate runs against the LOCAL clone, and a
clone made months ago does not contain a PR merged last week -- those pairs are
reported as `unverifiable_ancestry` and dropped, exactly as they should be. Fetch
the cache before mining the fresh split:

    for d in <repo-cache>/*/; do git -C "$d" fetch --all --quiet; done

Usage:
    python3 mine_fresh.py --snapshot-only     # build/refresh the PR snapshot
    python3 mine_fresh.py                     # mine from the snapshot
    python3 mine_fresh.py --refresh-snapshot  # re-freeze, then mine
    python3 mine_fresh.py --summary-only
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import shutil
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, mine_pairs  # type: ignore[no-redef]
else:
    from . import _harness, mine_pairs

FRESH_VERSION = 1
SNAPSHOT_NAME = "SNAPSHOT.json"

# GitHub's own closing keywords (docs: "Linking a pull request to an issue").
_CLOSING = re.compile(
    r"\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+"
    r"(?:https?://github\.com/[\w.-]+/[\w.-]+/issues/(\d+)|#(\d+))",
    re.IGNORECASE,
)

_DEFAULT_TEST_PATTERNS = (
    r"(^|/)tests?/",
    r"(^|/)spec/",
    r"(^|/)__tests__/",
    r"(^|/)testing/",
    r"(^|/)test_[^/]+\.py$",
    r"[^/]+_test\.(py|go|rb|cc|cpp|c|rs|java|ts|js)$",
    r"[^/]+\.test\.(js|jsx|ts|tsx)$",
    r"[^/]+\.spec\.(js|jsx|ts|tsx)$",
    r"[^/]+_spec\.rb$",
    r"[^/]+Test\.(java|php|cs)$",
    r"[^/]+Tests\.(java|php|cs)$",
)


class FreshUnavailable(RuntimeError):
    """`gh` is absent or unauthenticated -- a SKIP, never a silent empty split."""


# --------------------------------------------------------------------------
# config helpers
# --------------------------------------------------------------------------


def fresh_config(config: dict) -> dict:
    cfg = dict(config.get("fresh") or {})
    cfg.setdefault("training_cutoff", "2026-01-01")
    cfg.setdefault("max_prs_per_repo", 25)
    cfg.setdefault("max_files_per_pr", 60)
    cfg.setdefault("require_linked_issue", True)
    cfg.setdefault("require_test_delta", True)
    cfg.setdefault("out_dir", "candidates/fresh")
    cfg.setdefault("gh_timeout_sec", 120)
    cfg.setdefault("test_path_patterns", list(_DEFAULT_TEST_PATTERNS))
    return cfg


def out_dir(config: dict) -> pathlib.Path:
    raw = str(fresh_config(config)["out_dir"])
    path = pathlib.Path(raw)
    return path if path.is_absolute() else _harness.BRAINMARK_DIR / path


def test_matchers(config: dict) -> list[re.Pattern]:
    return [re.compile(p) for p in fresh_config(config)["test_path_patterns"]]


def is_test_path(path: str, matchers: list[re.Pattern]) -> bool:
    return any(m.search(path) for m in matchers)


def linked_issues(title: str, body: str) -> list[int]:
    found: set[int] = set()
    for text in (title or "", body or ""):
        for match in _CLOSING.finditer(text):
            number = match.group(1) or match.group(2)
            if number:
                found.add(int(number))
    return sorted(found)


# --------------------------------------------------------------------------
# gh (the ONLY networked path; exec-guarded)
# --------------------------------------------------------------------------


def gh_available() -> tuple[bool, str]:
    if not shutil.which("gh"):
        return False, "gh is not on PATH"
    proc = subprocess.run(["gh", "auth", "status"], capture_output=True, check=False)
    if proc.returncode != 0:
        return False, "gh is not authenticated (`gh auth login`)"
    return True, "ok"


def _gh_json(args: list[str], timeout: int):
    proc = subprocess.run(["gh", *args], capture_output=True, check=False, timeout=timeout)
    if proc.returncode != 0:
        detail = (proc.stderr or b"").decode("utf-8", "replace").strip().splitlines()
        raise FreshUnavailable(f"gh {' '.join(args[:3])}: {detail[-1] if detail else 'failed'}")
    text = (proc.stdout or b"").decode("utf-8", "replace").strip()
    if not text:
        return None
    return json.loads(text)


def search_merged_prs(repo: str, cutoff: str, limit: int, timeout: int) -> list[int]:
    """PR numbers merged into `repo` on/after `cutoff`, ascending, capped."""
    query = f"repo:{repo} is:pr is:merged merged:>={cutoff}"
    payload = _gh_json(
        ["api", "-X", "GET", "search/issues", "-f", f"q={query}",
         "-f", "per_page=100", "-f", "sort=created", "-f", "order=asc",
         "--paginate", "--slurp"],
        timeout,
    )
    numbers: set[int] = set()
    pages = payload if isinstance(payload, list) else [payload]
    for page in pages:
        if not isinstance(page, dict):
            continue
        for item in page.get("items") or []:
            number = item.get("number")
            if isinstance(number, int):
                numbers.add(number)
    return sorted(numbers)[:limit]


def fetch_pr(repo: str, number: int, max_files: int, timeout: int) -> dict | None:
    pr = _gh_json(["api", f"repos/{repo}/pulls/{number}"], timeout)
    if not isinstance(pr, dict) or not pr.get("merged_at"):
        return None
    # `-X GET` is load-bearing: `gh api` turns a request with `-f` fields into a
    # POST unless the method is explicit, and POST to /files is a 404.
    files_payload = _gh_json(
        ["api", "-X", "GET", f"repos/{repo}/pulls/{number}/files",
         "-f", "per_page=100", "--paginate", "--slurp"],
        timeout,
    )
    files: list[dict] = []
    pages = files_payload if isinstance(files_payload, list) else [files_payload]
    for page in pages:
        if isinstance(page, list):
            files.extend(f for f in page if isinstance(f, dict))
        elif isinstance(page, dict):
            files.append(page)
    files = sorted(files, key=lambda f: str(f.get("filename") or ""))[:max_files]
    return {
        "repo": repo,
        "number": number,
        "title": str(pr.get("title") or ""),
        "body": str(pr.get("body") or ""),
        "merged_at": str(pr.get("merged_at") or ""),
        "merge_commit_sha": str(pr.get("merge_commit_sha") or ""),
        "base_sha": str(((pr.get("base") or {}).get("sha")) or ""),
        "base_ref": str(((pr.get("base") or {}).get("ref")) or ""),
        "head_sha": str(((pr.get("head") or {}).get("sha")) or ""),
        "files": [
            {
                "filename": str(f.get("filename") or ""),
                "status": str(f.get("status") or ""),
                "previous_filename": str(f.get("previous_filename") or ""),
                "patch": str(f.get("patch") or ""),
            }
            for f in files
        ],
    }


def build_snapshot(config: dict, repos: list[str]) -> dict:
    """Freeze the PR list. The ONLY function here that talks to GitHub."""
    ok, why = gh_available()
    if not ok:
        raise FreshUnavailable(why)
    cfg = fresh_config(config)
    cutoff = str(cfg["training_cutoff"])
    timeout = int(cfg["gh_timeout_sec"])
    prs: list[dict] = []
    per_repo: dict[str, dict] = {}
    attempted = 0
    errors = 0
    for repo in sorted(repos):
        # A search failure is an auth/quota signal and must be fatal; a single
        # PR failing (deleted fork, 404 on files) must not lose the sweep.
        numbers = search_merged_prs(repo, cutoff, int(cfg["max_prs_per_repo"]), timeout)
        kept, failed = 0, 0
        for number in numbers:
            attempted += 1
            try:
                record = fetch_pr(repo, number, int(cfg["max_files_per_pr"]), timeout)
            except (FreshUnavailable, subprocess.TimeoutExpired, json.JSONDecodeError):
                failed += 1
                errors += 1
                continue
            if record is None:
                continue
            prs.append(record)
            kept += 1
        per_repo[repo] = {"searched": len(numbers), "fetched": kept, "failed": failed}
    if attempted and errors == attempted:
        raise FreshUnavailable(
            f"every one of {attempted} PR fetches failed -- this is an auth or "
            "quota problem, not a data problem; no snapshot written"
        )
    prs.sort(key=lambda p: (p["repo"], p["number"]))
    return {
        "schema_version": FRESH_VERSION,
        "training_cutoff": cutoff,
        "max_prs_per_repo": int(cfg["max_prs_per_repo"]),
        "max_files_per_pr": int(cfg["max_files_per_pr"]),
        "repos": dict(sorted(per_repo.items())),
        "pr_fetch_errors": errors,
        "pr_count": len(prs),
        "prs": prs,
    }


# --------------------------------------------------------------------------
# snapshot -> instances (pure, offline, deterministic)
# --------------------------------------------------------------------------


def rebuild_diff(files: list[dict]) -> str:
    """Reassemble a unified diff from the GitHub files API.

    The API hands back one `patch` hunk-set per file with no `diff --git`
    header; `mine_pairs.patch_files` keys off exactly that header, so it is
    re-synthesized here. Files with no `patch` (binary, or too large for the
    API) contribute a header and no hunks -- they still count as touched, which
    is the conservative choice for the shared-file gate.
    """
    chunks: list[str] = []
    for entry in files:
        name = str(entry.get("filename") or "")
        if not name:
            continue
        old = str(entry.get("previous_filename") or "") or name
        status = str(entry.get("status") or "")
        a_path = "/dev/null" if status == "added" else f"a/{old}"
        b_path = "/dev/null" if status == "removed" else f"b/{name}"
        chunks.append(f"diff --git a/{old} b/{name}")
        chunks.append(f"--- {a_path}")
        chunks.append(f"+++ {b_path}")
        patch = str(entry.get("patch") or "")
        if patch:
            chunks.append(patch.rstrip("\n"))
    return ("\n".join(chunks) + "\n") if chunks else ""


def pr_to_instance(pr: dict, matchers: list[re.Pattern], cfg: dict) -> tuple[dict | None, str]:
    """One snapshot PR -> one fresh instance, or (None, reject_reason)."""
    issues = linked_issues(pr.get("title", ""), pr.get("body", ""))
    if cfg["require_linked_issue"] and not issues:
        return None, "no_linked_issue"

    files = list(pr.get("files") or [])
    test_files = [f for f in files if is_test_path(str(f.get("filename") or ""), matchers)]
    code_files = [f for f in files if f not in test_files]
    if cfg["require_test_delta"] and not test_files:
        return None, "no_test_delta"
    if not code_files:
        # Test-only PR: nothing for a coding agent to fix.
        return None, "no_code_delta"
    if not pr.get("base_sha"):
        return None, "no_base_sha"

    owner, _, name = str(pr["repo"]).partition("/")
    instance = {
        "instance_id": f"{owner}__{name}-pr{int(pr['number'])}",
        "repo": str(pr["repo"]),
        "base_commit": str(pr["base_sha"]),
        # SWE-bench orders on a naive 'YYYY-MM-DD HH:MM:SS' string; GitHub's
        # RFC3339 must be normalized or fresh and historical instances would
        # sort into two disjoint blocks instead of one timeline.
        "created_at": str(pr.get("merged_at") or "").replace("T", " ").rstrip("Z"),
        "patch": rebuild_diff(code_files),
        "test_patch": rebuild_diff(test_files),
        "problem_statement": (str(pr.get("title") or "") + "\n\n"
                              + str(pr.get("body") or "")).strip(),
        "version": f"pr{int(pr['number'])}",
        "language": "",
        "split": "fresh",
        "pool": "fresh",
        "fresh_provenance": {
            "pr_number": int(pr["number"]),
            "merged_at": str(pr.get("merged_at") or ""),
            "merge_commit_sha": str(pr.get("merge_commit_sha") or ""),
            "base_ref": str(pr.get("base_ref") or ""),
            "head_sha": str(pr.get("head_sha") or ""),
            "linked_issues": issues,
            "test_files": sorted(str(f.get("filename") or "") for f in test_files),
            "code_files": sorted(str(f.get("filename") or "") for f in code_files),
        },
    }
    if not instance["patch"]:
        return None, "empty_code_patch"
    return instance, ""


FRESH_REJECTS = ("no_linked_issue", "no_test_delta", "no_code_delta",
                 "no_base_sha", "empty_code_patch")


def snapshot_instances(config: dict, snapshot: dict) -> tuple[dict[str, dict], dict[str, int]]:
    cfg = fresh_config(config)
    matchers = test_matchers(config)
    instances: dict[str, dict] = {}
    rejects = {reason: 0 for reason in FRESH_REJECTS}
    for pr in sorted(snapshot.get("prs") or [],
                     key=lambda p: (str(p.get("repo")), int(p.get("number") or 0))):
        instance, reason = pr_to_instance(pr, matchers, cfg)
        if instance is None:
            if reason in rejects:
                rejects[reason] += 1
            continue
        instances.setdefault(instance["instance_id"], instance)
    return instances, rejects


# --------------------------------------------------------------------------
# mining
# --------------------------------------------------------------------------


def mine_fresh(config: dict, snapshot: dict,
               oracle: mine_pairs.AncestryOracle | None = None,
               snapshot_sha256: str | None = None) -> dict:
    """Pair fresh Bs against fresh-or-historical As, under mine_pairs' gates."""
    repo_cache = pathlib.Path(config["repo_cache"])
    oracle = oracle if oracle is not None else mine_pairs.AncestryOracle(repo_cache)

    fresh, fresh_rejects = snapshot_instances(config, snapshot)
    historical, sources = mine_pairs.load_all_instances(config, None)

    combined: dict[str, dict] = {}
    for iid in sorted(historical):
        combined[iid] = historical[iid]
    for iid in sorted(fresh):
        combined[iid] = fresh[iid]  # fresh wins on an id collision (it cannot happen)

    fresh_ids = set(fresh)
    result = mine_pairs.mine_pool(
        combined, config, oracle,
        b_filter=lambda inst: str(inst.get("instance_id")) in fresh_ids,
    )

    for cand in result["candidates"]:
        b_id = cand["b"]["instance_id"]
        a_id = cand["a"]["instance_id"]
        cand["split"] = "fresh"
        cand["never_pool_with_main"] = True
        cand["a_side_split"] = "fresh" if a_id in fresh_ids else "historical"
        cand["fresh_provenance"] = fresh[b_id]["fresh_provenance"]
        cand["snapshot_sha256"] = snapshot_sha256
        cand["training_cutoff"] = str(fresh_config(config)["training_cutoff"])

    result["split"] = "fresh"
    result["never_pool_with_main"] = True
    result["sources"] = sources
    result["training_cutoff"] = str(fresh_config(config)["training_cutoff"])
    result["snapshot_sha256"] = snapshot_sha256
    result["snapshot_pr_count"] = len(snapshot.get("prs") or [])
    result["fresh_instances"] = len(fresh)
    result["fresh_rejects"] = fresh_rejects
    result["fresh_miner_sha256"] = _harness.sha256_file(pathlib.Path(__file__))
    return result


# --------------------------------------------------------------------------
# snapshot I/O + CLI
# --------------------------------------------------------------------------


def snapshot_path(config: dict) -> pathlib.Path:
    return out_dir(config) / SNAPSHOT_NAME


def read_snapshot(config: dict) -> tuple[dict, str]:
    path = snapshot_path(config)
    if not path.is_file():
        raise FreshUnavailable(
            f"no pinned PR snapshot at {path}. Build it once (needs `gh`):\n"
            f"    python3 {pathlib.Path(__file__).name} --snapshot-only"
        )
    return json.loads(path.read_text(encoding="utf-8")), _harness.sha256_file(path)


def write_snapshot(config: dict, snapshot: dict) -> str:
    path = snapshot_path(config)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(_harness.pretty_json(snapshot), encoding="utf-8")
    return _harness.sha256_file(path)


def cached_repos(config: dict) -> list[str]:
    """Repos that are BOTH in the instance pool AND already cloned locally.

    Cloning for the fresh split is out of scope here: `clone_repos.py` owns
    that, and mining a repo we cannot ancestry-check would just manufacture
    unverifiable pairs.
    """
    repo_cache = pathlib.Path(config["repo_cache"])
    pool, _ = mine_pairs.load_all_instances(config, None)
    repos = sorted({str(i.get("repo") or "") for i in pool.values() if i.get("repo")})
    oracle = mine_pairs.AncestryOracle(repo_cache)
    return [r for r in repos if oracle.repo_available(r)]


def format_summary(result: dict) -> str:
    lines = ["BrainMark FRESH-split miner (post-cutoff, ROBUSTNESS split)", "=" * 68]
    lines.append(f"training cutoff:   {result['training_cutoff']}")
    lines.append(f"snapshot sha256:   {result.get('snapshot_sha256')}")
    lines.append(f"PRs in snapshot:   {result['snapshot_pr_count']}")
    lines.append(f"fresh instances:   {result['fresh_instances']}")
    lines.append("instance rejects:  " + ", ".join(
        f"{k}={v}" for k, v in result["fresh_rejects"].items()))
    lines.append("")
    lines.append(f"FRESH CANDIDATE PAIRS: {result['candidate_count']}")
    lines.append("pair rejects: " + ", ".join(
        f"{k}={v}" for k, v in result["rejects"].items()))
    if result["rejects"].get("unverifiable_ancestry"):
        lines.append(
            "  NOTE: unverifiable ancestry on a CACHED repo means the clone predates "
            "the PR's base commit.\n"
            "        `git -C <repo-cache>/<owner>_<name> fetch --all` and re-mine; "
            "the gate is not relaxed for freshness.")
    a_hist = sum(1 for c in result["candidates"] if c["a_side_split"] == "historical")
    lines.append(f"  A side historical: {a_hist}   A side fresh: "
                 f"{result['candidate_count'] - a_hist}")
    lines.append("")
    lines.append("This split is analyzed SEPARATELY and never pooled with the main "
                 "candidates (prereg 0.2/0.3).")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Mine the fresh post-cutoff split.")
    parser.add_argument("--config", default=None)
    parser.add_argument("--snapshot-only", action="store_true",
                        help="build/refresh the pinned PR snapshot and stop")
    parser.add_argument("--refresh-snapshot", action="store_true",
                        help="re-freeze the snapshot from GitHub, then mine")
    parser.add_argument("--only-repos", default=None, help="comma-separated owner/name")
    parser.add_argument("--summary-only", action="store_true")
    parser.add_argument("--out", default=None)
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    dest = pathlib.Path(args.out) if args.out else out_dir(config)

    have_snapshot = snapshot_path(config).is_file()
    if args.summary_only and not have_snapshot and not (
            args.snapshot_only or args.refresh_snapshot):
        # --summary-only must never silently trigger a multi-thousand-call
        # GitHub sweep AND freeze the determinism boundary as a side effect.
        print(f"SKIP: no pinned PR snapshot at {snapshot_path(config)}. "
              f"Freeze it deliberately with --snapshot-only.")
        return 0

    need_build = args.snapshot_only or args.refresh_snapshot or not have_snapshot
    if need_build:
        ok, why = gh_available()
        if not ok:
            print(f"SKIP: {why}. The fresh split needs `gh` once, to freeze the PR "
                  f"snapshot; everything after that is offline.")
            return 0
        repos = cached_repos(config)
        if args.only_repos:
            allow = {r.strip() for r in args.only_repos.split(",") if r.strip()}
            repos = [r for r in repos if r in allow]
        try:
            snapshot = build_snapshot(config, repos)
        except FreshUnavailable as exc:
            print(f"SKIP: {exc}")
            return 0
        sha = write_snapshot(config, snapshot)
        print(f"snapshot: {snapshot['pr_count']} PRs across {len(repos)} repos "
              f"-> {snapshot_path(config)} (sha256 {sha[:12]})")
        if args.snapshot_only:
            return 0

    try:
        snapshot, sha = read_snapshot(config)
    except FreshUnavailable as exc:
        print(f"SKIP: {exc}")
        return 0

    result = mine_fresh(config, snapshot, snapshot_sha256=sha)
    if not args.summary_only:
        # SNAPSHOT.json is INPUT to this run; write_candidates must not eat it.
        mine_pairs.write_candidates(result, dest, preserve=(SNAPSHOT_NAME,))
    print(format_summary(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
