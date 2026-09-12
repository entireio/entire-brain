"""The B agent's ENVIRONMENT must not tell it which arm it is in.

run_b.py already treats this as a fairness property for the agent's `pwd`:

    "a worktree at <cell>/worktree puts the ARM NAME in the agent's own `pwd`
    -- and the agent's shell output contains its cwd. [...] That is an
    arm-asymmetric CUE delivered outside the prompt, which is exactly what the
    symmetry gate exists to prevent."

The same argument applies verbatim to the agent's environment, and the
environment is strictly more visible than the cwd: `CODEX_HOME` and
`CLAUDE_CONFIG_DIR` are handed to the model's own shell, so `env | grep HOME`
prints `.../<pair_id>/<arm>/codex-home`. That is the arm name, the pair id, AND
a path straight into the results tree where every sibling arm's packet.txt and
prompt.txt live.

These tests pin the environment to the same standard the worktree already
meets: opaque, stable across a resume, distinct per arm, outside the results
tree, and with the debug opt-in flagged as NOT neutral.
"""

from __future__ import annotations

import json
import os
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import run_b  # noqa: E402
from brainmark.agents.claude_adapter import ClaudeAdapter  # noqa: E402
from brainmark.agents.codex_adapter import CodexAdapter  # noqa: E402

PAIR_ID = "astropy__1__then__astropy__2"
ARMS = ("no_brain", "full_brain", "mem0", "graphify", "cmm", "irrelevant")

AZURE = {"AZURE_AI_ENDPOINT": "https://example.invalid", "AZURE_AI_API_KEY": "k"}


def _cell(root: pathlib.Path, arm: str) -> pathlib.Path:
    cell = root / "results" / "B" / "pilot" / PAIR_ID / arm
    cell.mkdir(parents=True, exist_ok=True)
    return cell


class AdapterEnvIsArmNeutralTest(unittest.TestCase):
    """Every adapter, every arm: no env VALUE may name the arm or the pair."""

    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self._saved = {k: os.environ.pop(k, None)
                       for k in (run_b.WORKTREE_ROOT_ENV, run_b.AGENT_STATE_IN_CELL_ENV)}
        os.environ[run_b.WORKTREE_ROOT_ENV] = str(self.tmp / "scratch")

    def tearDown(self) -> None:
        os.environ.pop(run_b.WORKTREE_ROOT_ENV, None)
        for key, value in self._saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def _adapters(self):
        yield "claude", ClaudeAdapter({}), {"PATH": "/usr/bin"}
        yield "codex", CodexAdapter({"azure": {"env_file": "/nonexistent"}}), {
            "PATH": "/usr/bin", **AZURE}

    def test_no_env_value_names_the_arm_or_the_pair(self):
        for backend, adapter, base_env in self._adapters():
            for arm in ARMS:
                cell = _cell(self.tmp / backend, arm)
                state, neutral = run_b.agent_state_path(cell, PAIR_ID, arm, None)
                env, provenance = adapter.prepare_env(dict(base_env), cell, state)
                self.assertTrue(neutral)
                for key, value in env.items():
                    with self.subTest(backend=backend, arm=arm, key=key):
                        self.assertNotIn(arm, value,
                                         f"{key} leaks the ARM NAME to the agent: {value}")
                        self.assertNotIn(PAIR_ID, value,
                                         f"{key} leaks the pair id to the agent: {value}")
                # ...and the provenance the harness records must be equally clean
                # of anything the agent could read back out of its own env.
                blob = json.dumps(provenance)
                self.assertNotIn(arm, blob)

    def test_agent_state_is_outside_the_results_tree(self):
        results = (self.tmp / "claude" / "results").resolve()
        for arm in ARMS:
            cell = _cell(self.tmp / "claude", arm)
            state, _ = run_b.agent_state_path(cell, PAIR_ID, arm, None)
            with self.subTest(arm=arm):
                self.assertFalse(
                    str(state.resolve()).startswith(str(results)),
                    "agent state inside the results tree puts sibling arms' "
                    f"packet.txt within reach of the agent: {state}",
                )

    def test_independent_result_runs_do_not_share_agent_state(self):
        a = _cell(self.tmp / "run-a", "no_brain")
        b = _cell(self.tmp / "run-b", "no_brain")
        state_a, _ = run_b.agent_state_path(a, PAIR_ID, "no_brain", 0)
        state_b, _ = run_b.agent_state_path(b, PAIR_ID, "no_brain", 0)
        self.assertNotEqual(state_a, state_b)
        state_a.mkdir(parents=True)
        (state_a / "session.json").write_text("old session")
        self.assertFalse((state_b / "session.json").exists())
        self.assertEqual(state_a, run_b.agent_state_path(a, PAIR_ID, "no_brain", 0)[0])

    def test_arms_get_distinct_state_dirs_and_the_key_is_stable(self):
        cell = _cell(self.tmp / "claude", "no_brain")
        paths = {arm: run_b.agent_state_path(cell, PAIR_ID, arm, None)[0] for arm in ARMS}
        self.assertEqual(len(set(paths.values())), len(ARMS), paths)
        # Stable, so a resumed cell reuses its own auth/state.
        self.assertEqual(run_b.agent_state_path(cell, PAIR_ID, "mem0", 1)[0],
                         run_b.agent_state_path(cell, PAIR_ID, "mem0", 1)[0])
        self.assertNotEqual(run_b.agent_state_path(cell, PAIR_ID, "mem0", 1)[0],
                            run_b.agent_state_path(cell, PAIR_ID, "mem0", 2)[0])
        # ...and it must not collide with the worktree of the same cell.
        self.assertNotEqual(run_b.agent_state_path(cell, PAIR_ID, "mem0", 1)[0],
                            run_b.worktree_path(cell, PAIR_ID, "mem0", 1)[0])

    def test_debug_opt_in_restores_the_in_cell_layout_and_is_flagged(self):
        os.environ[run_b.AGENT_STATE_IN_CELL_ENV] = "1"
        cell = _cell(self.tmp / "claude", "full_brain")
        path, neutral = run_b.agent_state_path(cell, PAIR_ID, "full_brain", None)
        self.assertFalse(neutral, "an in-cell agent home must NEVER report as neutral")
        self.assertTrue(str(path).startswith(str(cell)), path)

    def test_session_a_keeps_its_predictable_per_pair_config_dir(self):
        """Session A has no arms, and harvest_native_jsonl() looks in out_dir.

        Passing no state_dir must leave the old behaviour byte-identical, or
        the claude A transcript silently degrades to the stream fallback.
        """
        out_dir = self.tmp / "A" / PAIR_ID
        out_dir.mkdir(parents=True)
        env, _ = ClaudeAdapter({}).prepare_env({"PATH": "/usr/bin"}, out_dir)
        self.assertEqual(env["CLAUDE_CONFIG_DIR"], str(out_dir / "claude-config"))


if __name__ == "__main__":
    unittest.main()
