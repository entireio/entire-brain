#!/usr/bin/env python3
"""A harness-owned isolation failure is an infrastructure non-outcome, not a 0.

`summarize()` documents this already:

    Infrastructure non-outcomes (harness delivery/isolation failed, agent never
    produced a real score) are tallied but kept OUT of groups/metrics so their
    synthetic zeros never enter arm means, deltas, or p-values.

Only the harness-retrieval half was implemented. `treatment_started` is set
before delivery isolation runs, so a failure in `remove_agent_visible_brain_store`,
`remove_agent_visible_git_remotes`, the env sanitizer or the seatbelt profile
reached run_one's generic handler, which writes `score: {"total": 0}` and no
`analysis_excluded` -- and `is_executed_run` then counts it. Those steps are
harness work and are structurally arm-correlated (only a treatment arm has a
brain store to remove), so the zero lands in one arm only.
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

from analysis.common import is_executed_run  # noqa: E402


def _record(condition: str, index: int, *, failure: str | None = None) -> dict:
    record = {
        "run_id": f"t__codex-x__{condition}__r{index}",
        "task_id": "t",
        "repo": "repo",
        "agent": "codex",
        "runner": {"id": "codex-x", "agent": "codex", "model": "m", "effort": "high"},
        "condition": condition,
        "repetition": index,
        "delivery_mode": "harness",
        "treatment_started": True,
        "agent_ran": failure is None,
        "ok": failure is None,
        "score": {"total": 0 if failure else 10, "version": 2},
        "validation": {"ok": failure is None},
        "agent_info": {"usage": {"total_tokens": 100}},
        "timing": {"harness_agent_interval_wall_seconds": 1.0},
    }
    if failure is not None:
        record["error"] = failure
        exclusion = run.harness_failure_analysis_exclusion(
            run.HarnessIsolationError(failure)
        )
        if exclusion is not None:
            record["analysis_excluded"] = exclusion
    return record


class IsolationFailureClassificationTest(unittest.TestCase):
    def test_isolation_failures_are_classified_as_infrastructure(self) -> None:
        self.assertEqual(
            run.harness_failure_analysis_exclusion(run.HarnessIsolationError("boom")),
            {"reason": "harness_delivery_isolation_failed"},
        )
        self.assertEqual(
            run.harness_failure_analysis_exclusion(run.MemoryDeliveryError("boom", {})),
            {"reason": "harness_memory_delivery_failed"},
        )

    def test_a_product_failure_is_still_a_real_outcome(self) -> None:
        # Validation failures, bad patches and integrity failures the agent
        # caused stay in the arm's denominator.
        self.assertIsNone(run.harness_failure_analysis_exclusion(RuntimeError("bad patch")))
        self.assertIsNone(run.harness_failure_analysis_exclusion(ValueError("nope")))

    def test_delivery_isolation_raises_the_harness_failure_type(self) -> None:
        delivery: dict = {"ok": True}
        original = run.remove_agent_visible_brain_store
        try:
            def explode(_worktree):
                raise RuntimeError("brain plugin store still present")

            run.remove_agent_visible_brain_store = explode
            with self.assertRaises(run.HarnessIsolationError) as ctx:
                run.complete_harness_delivery_isolation(
                    delivery,
                    pathlib.Path("/worktree"),
                    pathlib.Path("/source"),
                    {"PATH": "/usr/bin"},
                    {"bin": pathlib.Path("/frozen-bin")},
                )
        finally:
            run.remove_agent_visible_brain_store = original
        self.assertIn("brain_store_removal", str(ctx.exception))
        self.assertFalse(delivery["ok"])
        self.assertEqual(delivery["isolation_error"]["stage"], "brain_store_removal")

    def test_standard_cell_isolation_raises_the_harness_failure_type(self) -> None:
        original = run.remove_agent_visible_git_remotes
        try:
            def explode(_worktree):
                raise RuntimeError("agent-visible git remotes remain after isolation")

            run.remove_agent_visible_git_remotes = explode
            with self.assertRaises(run.HarnessIsolationError) as ctx:
                run.standard_cell_read_isolation(
                    pathlib.Path("/worktree"),
                    pathlib.Path("/source"),
                    {"PATH": "/usr/bin"},
                    {"bin": pathlib.Path("/frozen-bin")},
                )
        finally:
            run.remove_agent_visible_git_remotes = original
        self.assertIn("git_remote_isolation", str(ctx.exception))


class IsolationFailureNeverEntersAnArmMeanTest(unittest.TestCase):
    def test_summarize_excludes_the_isolation_failure_from_the_arm(self) -> None:
        records = [
            _record("no_brain", 1),
            _record("no_brain", 2),
            _record("history_facts", 1),
            _record("history_facts", 2, failure="harness delivery isolation failed at environment_isolation"),
        ]
        self.assertFalse(is_executed_run(records[-1]))
        with tempfile.TemporaryDirectory() as tmp:
            summary = run.summarize(records, pathlib.Path(tmp))
        comparisons = [
            item for item in summary["comparisons"] if item["condition"] == "history_facts"
        ]
        self.assertEqual(len(comparisons), 1)
        comparison = comparisons[0]
        self.assertEqual(comparison["n_condition"], 1)
        self.assertEqual(comparison["n_infrastructure_excluded_condition"], 1)
        # The surviving cell scored 10; the excluded zero must not halve it.
        self.assertEqual(comparison["mean_condition"], 10.0)
        self.assertEqual(comparison["delta"], 0.0)

    def test_an_agent_caused_failure_still_counts_against_its_arm(self) -> None:
        failed = _record("history_facts", 2)
        failed.update({"ok": False, "score": {"total": 0, "version": 2}, "validation": {"ok": False}})
        records = [
            _record("no_brain", 1),
            _record("history_facts", 1),
            failed,
        ]
        self.assertTrue(is_executed_run(failed))
        with tempfile.TemporaryDirectory() as tmp:
            summary = run.summarize(records, pathlib.Path(tmp))
        comparison = [
            item for item in summary["comparisons"] if item["condition"] == "history_facts"
        ][0]
        self.assertEqual(comparison["n_condition"], 2)
        self.assertEqual(comparison["n_infrastructure_excluded_condition"], 0)
        self.assertEqual(comparison["mean_condition"], 5.0)


if __name__ == "__main__":
    unittest.main()
