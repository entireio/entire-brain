#!/usr/bin/env python3
"""Fill the graphmark repo-cache so the miner's ancestry gate can be evaluated.

`mine_pairs.py` refuses to emit a pair whose `git merge-base --is-ancestor`
check cannot be run locally (an UNVERIFIABLE pair is not a passing pair). The
binding constraint on candidate supply is therefore the repo cache, not the task
pool: with 22 repositories missing, the 303-instance pool yielded 15 candidates;
with them cloned it yields 82.

This script clones every repository that has at least `clone.min_instances_per_repo`
instances in the loaded pools into

    <repo_cache>/<owner>_<name>

which is the graphmark convention (`tools/run_3arm.sh:57`) and the exact path
`mine_pairs.AncestryOracle` probes.

DISK: clones are `--filter=blob:none` PARTIAL clones. That fetches the commit
graph and trees but no historical file contents, which is all the ancestry gate
needs, and it keeps a repo like `torvalds/linux` from costing tens of GB.
`git worktree add <base_commit>` still works -- git lazily fetches the blobs for
that one checkout. Full clones are NOT used; disk here is a shared resource.

SAFETY / RESUMABILITY
  * Idempotent. A repo whose cache dir already resolves as a git repo is left
    completely alone -- including clones another agent made. Nothing is ever
    deleted from the cache.
  * A clone lands in `<dir>.tmp` and is renamed into place only on success, so
    an interrupted or timed-out clone can never leave a half-repo that the
    oracle would treat as present.
  * Per-clone timeout; a failure is recorded and skipped, never fatal.
  * The ledger is MERGED across runs and is byte-stable when nothing changed --
    re-running writes the identical file.

Usage:
    python3 clone_repos.py --dry-run
    python3 clone_repos.py                       # local task JSONs
    python3 clone_repos.py --pool swe_bench_verified --limit 20
    python3 clone_repos.py --only-repos preactjs/preact,jqlang/jq
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import pathlib
import shutil
import subprocess
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, mine_pairs, pool_loaders  # type: ignore[no-redef]
else:
    from . import _harness, mine_pairs, pool_loaders

LEDGER_VERSION = 1

STATE_OK = "ok"
STATE_FAILED = "failed"
STATE_BELOW_MIN = "below_min_instances"


def default_ledger_path(config: dict) -> pathlib.Path:
    raw = str((config.get("clone") or {}).get("ledger") or "pools/CLONE-LEDGER.json")
    path = pathlib.Path(raw)
    return path if path.is_absolute() else _harness.BRAINMARK_DIR / path


# --------------------------------------------------------------------------
# repo inventory
# --------------------------------------------------------------------------


def repo_counts(config: dict, pools: list[str] | None = None) -> tuple[dict[str, int], list[dict]]:
    """instances-per-repo across the local task JSONs plus any cached pools."""
    graphmark_root = pathlib.Path(config["graphmark_root"])
    pool, sources = mine_pairs.load_instances(graphmark_root, config["task_globs"])
    if pools:
        hf_pool, hf_sources = pool_loaders.load_pools(config, pools)
        for iid, inst in hf_pool.items():
            pool.setdefault(iid, inst)
        sources = list(sources) + list(hf_sources)
    counts: collections.Counter[str] = collections.Counter()
    for inst in pool.values():
        repo = str(inst.get("repo") or "")
        if repo:
            counts[repo] += 1
    return dict(sorted(counts.items())), sources


def is_git_repo(path: pathlib.Path) -> bool:
    """Present == git itself agrees, not just that a directory exists.

    `AncestryOracle.repo_available` only checks for `.git`/`HEAD`; a directory
    that merely looks like a repo would make the oracle report `has_commit`
    False for everything and silently starve the miner. Ask git.
    """
    if not path.is_dir():
        return False
    proc = subprocess.run(
        ["git", "-C", str(path), "rev-parse", "--git-dir"],
        capture_output=True, check=False,
    )
    return proc.returncode == 0


def clone_url(config: dict, repo: str) -> str:
    host = str((config.get("clone") or {}).get("host") or "https://github.com/")
    if not host.endswith("/"):
        host += "/"
    return f"{host}{repo}.git"


# --------------------------------------------------------------------------
# cloning
# --------------------------------------------------------------------------


def clone_one(config: dict, repo: str, dest: pathlib.Path) -> tuple[bool, str]:
    """Clone `repo` into `dest` via a staging dir. Returns (ok, message)."""
    clone_cfg = config.get("clone") or {}
    timeout = int(clone_cfg.get("timeout_sec") or 1800)
    blob_filter = str(clone_cfg.get("filter") or "blob:none")

    staging = dest.parent / f"{dest.name}.tmp"
    if staging.exists():
        shutil.rmtree(staging, ignore_errors=True)
    dest.parent.mkdir(parents=True, exist_ok=True)

    cmd = ["git", "clone", "--quiet", f"--filter={blob_filter}",
           clone_url(config, repo), str(staging)]
    env = dict(os.environ)
    # A prompt would hang the whole sweep behind an invisible password box.
    env["GIT_TERMINAL_PROMPT"] = "0"
    env.setdefault("GIT_ASKPASS", "true")
    try:
        proc = subprocess.run(cmd, capture_output=True, check=False,
                              timeout=timeout, env=env)
    except subprocess.TimeoutExpired:
        shutil.rmtree(staging, ignore_errors=True)
        return False, f"timeout after {timeout}s"
    if proc.returncode != 0:
        shutil.rmtree(staging, ignore_errors=True)
        detail = (proc.stderr or b"").decode("utf-8", "replace").strip().splitlines()
        return False, (detail[-1] if detail else f"git clone exit {proc.returncode}")
    if not is_git_repo(staging):
        shutil.rmtree(staging, ignore_errors=True)
        return False, "clone produced no usable git repo"
    if dest.exists():
        # Another agent won the race while we were cloning. Theirs wins.
        shutil.rmtree(staging, ignore_errors=True)
        return True, "already present (raced)"
    staging.rename(dest)
    return True, "cloned"


def sweep(config: dict, *, pools: list[str] | None = None,
          only_repos: list[str] | None = None, limit: int | None = None,
          dry_run: bool = False, ledger_path: pathlib.Path | None = None) -> dict:
    repo_cache = pathlib.Path(config["repo_cache"])
    clone_cfg = config.get("clone") or {}
    min_instances = int(clone_cfg.get("min_instances_per_repo") or 2)
    ledger_path = ledger_path or default_ledger_path(config)

    counts, sources = repo_counts(config, pools)
    ledger = load_ledger(ledger_path)
    entries: dict[str, dict] = dict(ledger.get("repos") or {})

    wanted = sorted(counts)
    if only_repos:
        allow = {r.strip() for r in only_repos if r.strip()}
        wanted = [r for r in wanted if r in allow]

    missing: list[str] = []
    for repo in wanted:
        dest = mine_pairs.cache_dir_for(repo_cache, repo)
        entry = dict(entries.get(repo) or {})
        entry["dir"] = _harness.display_path(dest)
        entry["instances"] = counts[repo]
        if counts[repo] < min_instances:
            # A single-instance repo can never form an (A, B) pair.
            entry["state"] = STATE_BELOW_MIN
            entry.setdefault("action", "not_needed")
            entry.setdefault("attempts", 0)
            entry.setdefault("error", None)
            entries[repo] = entry
            continue
        if is_git_repo(dest):
            entry["state"] = STATE_OK
            entry.setdefault("action", "already_present")
            entry.setdefault("attempts", 0)
            entry["error"] = None
            entries[repo] = entry
            continue
        missing.append(repo)
        entries[repo] = entry

    # Deterministic work order: most instances first (biggest pair yield per
    # clone), then repo name.
    missing.sort(key=lambda r: (-counts[r], r))
    todo = missing[:limit] if limit is not None else missing

    cloned, failed, skipped = [], [], []
    for repo in todo:
        dest = mine_pairs.cache_dir_for(repo_cache, repo)
        entry = entries[repo]
        if dry_run:
            entry["state"] = entry.get("state") or "pending"
            entry.setdefault("action", "would_clone")
            entry.setdefault("attempts", 0)
            entry.setdefault("error", None)
            skipped.append(repo)
            continue
        ok, message = clone_one(config, repo, dest)
        entry["attempts"] = int(entry.get("attempts") or 0) + 1
        if ok:
            entry["state"] = STATE_OK
            entry["action"] = message
            entry["error"] = None
            cloned.append(repo)
        else:
            entry["state"] = STATE_FAILED
            entry["action"] = "clone_failed"
            entry["error"] = message
            failed.append(repo)
        entries[repo] = entry

    not_attempted = [r for r in missing if r not in set(todo)]
    for repo in not_attempted:
        entry = entries[repo]
        entry.setdefault("state", "pending")
        entry.setdefault("action", "deferred_by_limit")
        entry.setdefault("attempts", 0)
        entry.setdefault("error", None)

    result = {
        "schema_version": LEDGER_VERSION,
        "repo_cache": _harness.display_path(repo_cache),
        "clone_filter": str(clone_cfg.get("filter") or "blob:none"),
        "min_instances_per_repo": min_instances,
        "pools": sorted(pools or []),
        "sources": sources,
        "repos": {k: _stable_entry(v) for k, v in sorted(entries.items())},
    }
    result["summary"] = summarize(result)
    if not dry_run:
        write_ledger(ledger_path, result)
    result["_run"] = {
        "cloned": sorted(cloned),
        "failed": sorted(failed),
        "deferred": sorted(not_attempted + (skipped if dry_run else [])),
        "ledger": str(ledger_path),
    }
    return result


def _stable_entry(entry: dict) -> dict:
    return {
        "dir": entry.get("dir"),
        "instances": entry.get("instances"),
        "state": entry.get("state"),
        "action": entry.get("action"),
        "attempts": int(entry.get("attempts") or 0),
        "error": entry.get("error"),
    }


def summarize(ledger: dict) -> dict:
    counter: collections.Counter[str] = collections.Counter()
    pairable = 0
    for entry in (ledger.get("repos") or {}).values():
        counter[str(entry.get("state"))] += 1
        if entry.get("state") == STATE_OK and int(entry.get("instances") or 0) >= 2:
            pairable += 1
    return {
        "repos_total": len(ledger.get("repos") or {}),
        "ok": counter.get(STATE_OK, 0),
        "failed": counter.get(STATE_FAILED, 0),
        "below_min_instances": counter.get(STATE_BELOW_MIN, 0),
        "pending": counter.get("pending", 0),
        "pairable_repos_cached": pairable,
    }


# --------------------------------------------------------------------------
# ledger I/O
# --------------------------------------------------------------------------


def load_ledger(path: pathlib.Path) -> dict:
    if not path.is_file():
        return {"schema_version": LEDGER_VERSION, "repos": {}}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        # A corrupt ledger must not stop the sweep; the cache on disk is the
        # real state and the ledger is rebuilt from it.
        return {"schema_version": LEDGER_VERSION, "repos": {}}


def write_ledger(path: pathlib.Path, ledger: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(_harness.pretty_json(ledger), encoding="utf-8")


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def format_summary(result: dict) -> str:
    run = result.get("_run") or {}
    summary = result["summary"]
    lines = ["BrainMark repo-cache sweep", "=" * 68]
    lines.append(f"cache:  {result['repo_cache']}")
    lines.append(f"filter: --filter={result['clone_filter']} (partial clone; commit graph only)")
    lines.append(f"ledger: {run.get('ledger')}")
    lines.append("")
    lines.append(
        f"repos known {summary['repos_total']:>4} | cached ok {summary['ok']:>4} | "
        f"failed {summary['failed']:>3} | pending {summary['pending']:>3} | "
        f"<{result['min_instances_per_repo']} instances {summary['below_min_instances']:>3}"
    )
    lines.append(f"pairable repos with a local clone: {summary['pairable_repos_cached']}")
    for label in ("cloned", "failed", "deferred"):
        items = run.get(label) or []
        if items:
            lines.append(f"{label}: {len(items)} -> " + ", ".join(items[:10])
                         + (" ..." if len(items) > 10 else ""))
    failures = {r: e["error"] for r, e in result["repos"].items()
                if e.get("state") == STATE_FAILED}
    if failures:
        lines.append("")
        lines.append("failures (skipped, re-runnable):")
        for repo, err in sorted(failures.items()):
            lines.append(f"  {repo:<40} {err}")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Fill the graphmark repo-cache.")
    parser.add_argument("--config", default=None)
    parser.add_argument("--pool", action="append", default=None,
                        help="also count instances from this cached HF pool (repeatable)")
    parser.add_argument("--only-repos", default=None,
                        help="comma-separated owner/name list")
    parser.add_argument("--limit", type=int, default=None,
                        help="clone at most N missing repos this run")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--ledger", default=None)
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    result = sweep(
        config,
        pools=args.pool,
        only_repos=args.only_repos.split(",") if args.only_repos else None,
        limit=args.limit,
        dry_run=args.dry_run,
        ledger_path=pathlib.Path(args.ledger) if args.ledger else None,
    )
    print(format_summary(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
