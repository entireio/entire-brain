#!/usr/bin/env python3
"""Tests for rehoming a cached brain plugin onto a new benchmark worktree.

The brain names each repository directory <base>-<sha256(repo_path)[:12]>, so a
cached plugin carries the directory name of the cell that produced it. Reusing
that plugin without renaming the directory leaves the borrowing cell resolving a
brain path that does not exist: prep reads an empty brain and fails with
"produced no history index records", while the first cell of each cache key
passes because it builds fresh. That asymmetry silently reduced every history
treatment arm to a single usable repetition.
"""

from __future__ import annotations

import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("run", HERE / "run.py")
assert SPEC is not None and SPEC.loader is not None
run = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = run
SPEC.loader.exec_module(run)


OLD_WORKTREE = "/bench/.worktrees/repo-aaaaaaaa/repo"
NEW_WORKTREE = "/bench/.worktrees/repo-bbbbbbbb/repo"
OLD_KEY = "repo-000000000000"
NEW_KEY = "repo-111111111111"


def _build_cache_plugin(root: pathlib.Path) -> pathlib.Path:
    plugin = root / "plugin"
    for scope in ("data", "state"):
        repo_dir = plugin / scope / "repos" / "local" / OLD_KEY
        repo_dir.mkdir(parents=True)
        (repo_dir / "manifest.json").write_text(
            json.dumps(
                {
                    "repo_key": f"local/{OLD_KEY}",
                    "repo_root": OLD_WORKTREE,
                    "history_records": 11377,
                }
            )
        )
    return plugin


class CachedPluginRepoKeyTest(unittest.TestCase):
    def test_repo_directory_is_renamed_in_every_scope(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            cache_plugin = _build_cache_plugin(root / "cache")
            run_plugin = root / "run" / "plugin"

            run.copy_cached_plugin(cache_plugin, run_plugin, OLD_WORKTREE, NEW_WORKTREE, NEW_KEY)

            for scope in ("data", "state"):
                local = run_plugin / scope / "repos" / "local"
                self.assertTrue((local / NEW_KEY).is_dir(), f"{scope} repo dir was not renamed")
                self.assertFalse((local / OLD_KEY).exists(), f"{scope} kept the producing cell's name")

    def test_embedded_repo_key_and_worktree_are_rewritten(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            cache_plugin = _build_cache_plugin(root / "cache")
            run_plugin = root / "run" / "plugin"

            run.copy_cached_plugin(cache_plugin, run_plugin, OLD_WORKTREE, NEW_WORKTREE, NEW_KEY)

            manifest = json.loads(
                (run_plugin / "data" / "repos" / "local" / NEW_KEY / "manifest.json").read_text()
            )
            self.assertEqual(manifest["repo_key"], f"local/{NEW_KEY}")
            self.assertEqual(manifest["repo_root"], NEW_WORKTREE)
            self.assertEqual(manifest["history_records"], 11377)

    def test_same_worktree_reuse_is_left_untouched(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            cache_plugin = _build_cache_plugin(root / "cache")
            run_plugin = root / "run" / "plugin"

            run.copy_cached_plugin(cache_plugin, run_plugin, OLD_WORKTREE, OLD_WORKTREE, OLD_KEY)

            local = run_plugin / "data" / "repos" / "local"
            self.assertTrue((local / OLD_KEY).is_dir())

    def test_undecodable_files_are_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            cache_plugin = _build_cache_plugin(root / "cache")
            blob = OLD_WORKTREE.encode() + b"\xff\xfe binary payload"
            (cache_plugin / "data" / "repos" / "local" / OLD_KEY / "index.bin").write_bytes(blob)
            run_plugin = root / "run" / "plugin"

            run.copy_cached_plugin(cache_plugin, run_plugin, OLD_WORKTREE, NEW_WORKTREE, NEW_KEY)

            copied = (run_plugin / "data" / "repos" / "local" / NEW_KEY / "index.bin").read_bytes()
            self.assertEqual(copied, blob)


if __name__ == "__main__":
    unittest.main()
