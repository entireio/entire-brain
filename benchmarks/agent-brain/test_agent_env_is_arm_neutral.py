#!/usr/bin/env python3
"""The agent's ENVIRONMENT must not name the arm.

`create_worktree` already gives every cell an opaque cwd so a session cannot
read its condition out of `pwd`. The environment is strictly more visible than
the cwd -- a bare `env` in the agent's own shell prints every value -- and the
per-cell runtime cache put the condition straight into it:

    GOCACHE                 <suite>/<task>__<runner>__<condition>__r1/runtime-cache/go-build
    GOMODCACHE              ... /runtime-cache/go-mod
    ENTIRE_PLUGIN_CACHE_DIR ... /runtime-cache/retrieval-vectors

`sanitize_harness_agent_environment` is a name-based filter and passed all
three through unchanged.
"""

from __future__ import annotations

import importlib.util
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


CONDITIONS = ("no_brain", "full_brain", "raw_history", "facts_only", "history_facts")


class CellCacheEnvIsArmNeutralTest(unittest.TestCase):
    def _cell_env(self, suite: pathlib.Path, condition: str) -> tuple[str, dict[str, str]]:
        run_id = f"astropy__1__claude-opus__{condition}__r1"
        run_dir = suite / run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        paths = run.runtime_cache_paths(suite, run_dir, "isolated_per_cell")
        return run_id, run.ensure_runtime_cache(paths)

    def test_no_cell_cache_variable_spells_the_condition(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            suite = pathlib.Path(tmp)
            for condition in CONDITIONS:
                _, env = self._cell_env(suite, condition)
                for key, value in env.items():
                    self.assertNotIn(condition, value, f"{key} names the arm: {value}")

    def test_the_sanitizer_refuses_an_environment_that_names_the_arm(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "agent environment names the arm"):
            run.sanitize_harness_agent_environment(
                {"GOCACHE": "/results/astropy__1__r__full_brain__r1/runtime-cache/go-build"},
                ("full_brain",),
            )

    def test_a_clean_cell_environment_passes_the_same_check(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            suite = pathlib.Path(tmp)
            for condition in CONDITIONS:
                run_id, env = self._cell_env(suite, condition)
                sanitized, provenance = run.sanitize_harness_agent_environment(
                    env, (condition, run_id)
                )
                self.assertEqual(sanitized, env)
                self.assertTrue(provenance["identifying_tokens_absent_from_values"])

    def test_the_token_check_is_opt_in_and_backward_compatible(self) -> None:
        dirty = {"GOCACHE": "/results/x__full_brain__r1/go-build", "PATH": "/usr/bin"}
        sanitized, provenance = run.sanitize_harness_agent_environment(dirty)
        self.assertEqual(sanitized, dirty)
        self.assertEqual(provenance["removed_keys"], [])
        self.assertEqual(provenance["identifying_token_count"], 0)
        self.assertFalse(provenance["identifying_tokens_absent_from_values"])

    def test_cells_do_not_share_a_cache_and_the_key_is_resume_stable(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            suite = pathlib.Path(tmp)
            roots = {}
            for condition in CONDITIONS:
                run_dir = suite / f"astropy__1__claude-opus__{condition}__r1"
                roots[condition] = run.runtime_cache_paths(
                    suite, run_dir, "isolated_per_cell"
                )["root"]
            self.assertEqual(len(set(map(str, roots.values()))), len(CONDITIONS))
            again = run.runtime_cache_paths(
                suite, suite / "astropy__1__claude-opus__no_brain__r1", "isolated_per_cell"
            )["root"]
            self.assertEqual(str(again), str(roots["no_brain"]))

    def test_the_cache_still_lives_inside_the_suite_for_provenance(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            suite = pathlib.Path(tmp)
            run_dir = suite / "astropy__1__claude-opus__full_brain__r1"
            paths = run.runtime_cache_paths(suite, run_dir, "isolated_per_cell")
            provenance = run.cache_path_provenance(suite, paths)
            self.assertEqual(sorted(provenance), ["GOCACHE", "GOMODCACHE", "retrieval_vector_cache", "root"])
            for entry in provenance.values():
                self.assertNotIn("full_brain", entry["path"])

    def test_the_shared_policy_is_untouched(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            suite = pathlib.Path(tmp)
            shared = run.runtime_cache_paths(suite, suite / "cell", "prewarmed_shared")
            self.assertEqual(shared["root"], suite / "runtime-cache" / "shared")
            setup = run.runtime_cache_paths(suite, None, "isolated_per_cell")
            self.assertEqual(setup["root"], suite / "runtime-cache" / "setup")


if __name__ == "__main__":
    unittest.main()
