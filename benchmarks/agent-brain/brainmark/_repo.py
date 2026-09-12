"""Worktree, lock, netjail and patch-collection plumbing.

Patterns are taken from graphmark's run_3arm.sh (the harness that has already
been debugged against 300-instance runs); the reasons below are its scars, not
speculation.

  * PER-CACHE LOCK (run_3arm.sh:84). `git worktree add/remove` and patch
    collection all mutate the shared cache repo. Without one lock covering all
    three, a concurrent `worktree remove --force` corrupts another instance's
    `git diff` and yields a 0-byte patch while the driver logs a real one --
    measured on 3 apache/druid instances, one of which was byte-identical to a
    patch that graded RESOLVED elsewhere. That is pure collection loss, not agent
    failure, and it is invisible unless you look for it.

  * NETJAIL FIRST ON PATH, FOR EVERY ARM (run_3arm.sh:1064). The repo cache holds
    refs PAST base_commit, so a session can `git log` its way to the real fix, and
    graphmark#53 caught sessions curl-ing the upstream patch. Symmetric for all
    arms or the comparison is meaningless.

  * PATCH COLLECTION delegates to graphmark's collect_patch.sh: it `git add -N`s
    new SOURCE files (a fix that adds a file was silently dropped by plain
    `git diff`) while excluding lockfiles, vendor/build output, and test/repro
    files (SWE-bench forbids submitting tests, and an edit to a shared test
    helper can spuriously flip PASS_TO_PASS).
"""

from __future__ import annotations

import contextlib
import os
import pathlib
import re
import shutil
import subprocess
import time
from typing import Iterator

LOCK_TIMEOUT_S = 120.0
LOCK_POLL_S = 0.2


def cache_dir_for(repo_cache: pathlib.Path, repo: str) -> pathlib.Path:
    """graphmark convention (run_3arm.sh:56): repo-cache/<owner>_<name>."""
    if (not isinstance(repo, str)
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+", repo)
            or repo.split("/")[1] in {".", ".."}):
        raise ValueError(f"invalid repository name (expected owner/repository): {repo!r}")
    return pathlib.Path(repo_cache) / repo.replace("/", "_")


@contextlib.contextmanager
def cache_lock(graphmark_root: pathlib.Path, cache: pathlib.Path) -> Iterator[None]:
    """mkdir-based spinlock, one per cache repo (run_3arm.sh:85).

    mkdir is atomic on every filesystem that matters here, needs no daemon, and
    leaves a visible artifact if a run dies holding it.
    """
    locks = pathlib.Path(graphmark_root) / ".locks"
    locks.mkdir(parents=True, exist_ok=True)
    lockdir = locks / f"{cache.name}.wt.lock"
    deadline = time.monotonic() + LOCK_TIMEOUT_S
    while True:
        try:
            lockdir.mkdir()
            break
        except FileExistsError:
            if time.monotonic() > deadline:
                raise TimeoutError(f"timed out waiting for cache lock {lockdir}")
            time.sleep(LOCK_POLL_S)
    try:
        yield
    finally:
        with contextlib.suppress(OSError):
            lockdir.rmdir()


def worktree_add(graphmark_root: pathlib.Path, cache: pathlib.Path,
                 worktree: pathlib.Path, commit: str) -> None:
    with cache_lock(graphmark_root, cache):
        subprocess.run(["git", "-C", str(cache), "worktree", "remove", "--force", str(worktree)],
                       capture_output=True, check=False)
        shutil.rmtree(worktree, ignore_errors=True)
        worktree.parent.mkdir(parents=True, exist_ok=True)
        proc = subprocess.run(
            ["git", "-C", str(cache), "worktree", "add", "--force", "--detach",
             str(worktree), commit],
            capture_output=True, text=True, check=False,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                f"worktree add failed for {commit} in {cache}: {proc.stderr.strip()[:400]}"
            )


def worktree_remove(graphmark_root: pathlib.Path, cache: pathlib.Path,
                    worktree: pathlib.Path) -> None:
    with cache_lock(graphmark_root, cache):
        subprocess.run(["git", "-C", str(cache), "worktree", "remove", "--force", str(worktree)],
                       capture_output=True, check=False)
        shutil.rmtree(worktree, ignore_errors=True)


def collect_patch(graphmark_root: pathlib.Path, cache: pathlib.Path,
                  worktree: pathlib.Path, out_patch: pathlib.Path) -> int:
    """Run collect_patch.sh under the SAME lock as worktree mutation."""
    script = pathlib.Path(graphmark_root) / "tools" / "collect_patch.sh"
    if not script.is_file():
        raise RuntimeError(f"collect_patch.sh not found at {script}")
    out_patch.parent.mkdir(parents=True, exist_ok=True)
    with cache_lock(graphmark_root, cache):
        out_patch.unlink(missing_ok=True)
        proc = subprocess.run(["bash", str(script), str(worktree), str(out_patch)],
                              capture_output=True, text=True, check=False)
        if proc.returncode != 0 or not out_patch.is_file():
            out_patch.unlink(missing_ok=True)
            raise RuntimeError(f"patch collection failed ({proc.returncode}): {proc.stderr[:400]}")
        return out_patch.stat().st_size


def netjail_dir(graphmark_root: pathlib.Path) -> pathlib.Path:
    path = pathlib.Path(graphmark_root) / "tools" / "netjail"
    if not path.is_dir():
        raise RuntimeError(f"netjail not found at {path}")
    return path


def netjail_path(graphmark_root: pathlib.Path, base_path: str | None = None) -> str:
    base = base_path if base_path is not None else os.environ.get("PATH", "")
    return f"{netjail_dir(graphmark_root)}{os.pathsep}{base}"
