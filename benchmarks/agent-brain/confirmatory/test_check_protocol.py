from __future__ import annotations

import importlib.util
import json
import pathlib
import unittest


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("check_protocol", HERE / "check_protocol.py")
assert SPEC and SPEC.loader
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ProtocolCheckTest(unittest.TestCase):
    def test_preparation_artifacts_are_consistent(self) -> None:
        self.assertEqual(CHECK.validate(freeze=False), [])

    def test_freeze_is_fail_closed_while_dependencies_are_pending(self) -> None:
        errors = CHECK.validate(freeze=True)
        self.assertTrue(errors)
        self.assertIn("WS2-WS5 dependencies are pending", errors)
        self.assertIn("fresh holdout commitment is not frozen", errors)
        self.assertIn("paid-run checklist is not all pass", errors)

    def test_inventory_is_unique_and_contamination_is_explicit(self) -> None:
        inventory = json.loads((HERE / "task-inventory.json").read_text())
        tasks = inventory["tasks"]
        self.assertEqual(len(tasks), 46)
        self.assertEqual(len({task["task_id"] for task in tasks}), 46)
        by_short = {task["fix_commit"][:9]: task for task in tasks}
        self.assertEqual(by_short["4dd458656"]["state"], "optimization_used")
        self.assertFalse(by_short["4dd458656"]["confirmatory_eligible"])
        self.assertEqual(by_short["d9df8fcca"]["ledger"]["present"], True)
        self.assertEqual(inventory["summary"]["confirmatory_eligible"], 0)

    def test_engine_names_and_namespaces_are_exact(self) -> None:
        matrix = json.loads((HERE / "engine-matrix.json").read_text())
        arms = matrix["arms"]
        self.assertEqual([arm["id"] for arm in arms], CHECK.ARMS)
        self.assertEqual(len({arm["namespace"] for arm in arms}), 3)
        self.assertIn("--no-semantic", arms[0]["cli_flags"])
        self.assertEqual(arms[2]["environment"]["ENTIRE_BRAIN_EMBEDDER"], "ollama")


if __name__ == "__main__":
    unittest.main()
