import importlib.util
import json
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("mutation_check", Path(__file__).with_name("mutation_check.py"))
mutation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mutation)


class MutationEvidenceTests(unittest.TestCase):
    def test_compile_errors_and_unrelated_failures_do_not_count(self):
        case = {"test": "TestInvariant", "diagnostic": "lost exclusion"}
        for events in [[], [{"Action": "fail", "Package": "cli"}],
                       [{"Action": "fail", "Test": "TestOther", "Output": "lost exclusion"}],
                       [{"Action": "fail", "Test": "TestInvariant", "Output": "unrelated failure"}]]:
            result = subprocess.CompletedProcess([], 1, "\n".join(json.dumps(e) for e in events))
            self.assertFalse(mutation.detected(result, case))

    def test_requires_exact_test_failure_and_intended_diagnostic(self):
        case = {"test": "TestInvariant", "diagnostic": "lost exclusion"}
        events = [{"Action": "output", "Test": "TestInvariant", "Output": "lost exclusion\n"},
                  {"Action": "fail", "Test": "TestInvariant"}]
        text = "\n".join(json.dumps(e) for e in events)
        self.assertTrue(mutation.detected(subprocess.CompletedProcess([], 1, text), case))
        self.assertFalse(mutation.detected(subprocess.CompletedProcess([], 0, text), case))


if __name__ == "__main__":
    unittest.main()
