import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("python_coverage_runner", Path(__file__).with_name("python_run.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


@unittest.skipUnless(importlib.util.find_spec("coverage"), "requires the CI-pinned coverage tracer")
class PythonCoverageRunnerTests(unittest.TestCase):
    def test_failed_suite_retains_failure_and_subprocess_coverage(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            hooks = root / "empty-hooks"
            hooks.mkdir()
            subprocess.run(["git", "-C", str(root), "-c", f"core.hooksPath={hooks}",
                            "-c", "commit.gpgsign=false", "-c", "user.name=Coverage Test",
                            "-c", "user.email=test@example.invalid", "commit", "--allow-empty",
                            "-qm", "fixture"], check=True)
            bench = root / "benchmarks/agent-brain"
            bench.mkdir(parents=True)
            (bench / "child.py").write_text('print("child traced")\n')
            (bench / "ci_tests.py").write_text(
                'import subprocess, sys\n'
                'subprocess.run([sys.executable, "benchmarks/agent-brain/child.py"], check=True)\n'
                'print("FAIL            fixture_failure.py")\n'
                'raise SystemExit(1)\n'
            )
            output = root / "report"
            with contextlib.redirect_stdout(io.StringIO()), mock.patch.object(runner, "ROOT", root), mock.patch(
                "sys.argv", ["python_run.py", "--output", str(output)]
            ):
                self.assertEqual(runner.main(), 1)
            coverage = json.loads((output / "coverage.json").read_text())
            self.assertTrue(any(name.endswith("child.py") and data["summary"]["covered_lines"] > 0
                                for name, data in coverage["files"].items()))
            outcomes = json.loads((output / "test-results.json").read_text())
            self.assertEqual(outcomes["exit_code"], 1)
            self.assertIn("FAIL            fixture_failure.py", outcomes["modules"])
            self.assertTrue((output / "html/index.html").exists())


class PythonCoverageFailureReportingTests(unittest.TestCase):
    def test_reporting_failures_preserve_suite_result_and_write_outcomes(self):
        for suite_exit, combine_exit, json_exit, html_exit, expected in (
            (7, 0, 2, 2, 7),
            (0, 0, 0, 9, 9),
            (0, 3, 2, 2, 3),
            (7, 3, 2, 2, 7),
        ):
            with self.subTest(codes=(suite_exit, combine_exit, json_exit, html_exit)):
                with tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    output = root / "report"
                    calls = []
                    def run(command, **kwargs):
                        operation = command[4]
                        calls.append(operation)
                        if operation == "run":
                            kwargs["stdout"].write("FAIL fixture.py\n" if suite_exit else "ok fixture.py\n")
                            if combine_exit:
                                (output / ".coverage.child").write_bytes(b"invalid coverage fragment")
                        code = {"run": suite_exit, "combine": combine_exit,
                                "json": json_exit, "html": html_exit}[operation]
                        self.assertFalse(kwargs.get("check", False), "report failure must not bypass outcome writing")
                        return subprocess.CompletedProcess(command, code)
                    with contextlib.redirect_stdout(io.StringIO()), mock.patch.object(runner, "ROOT", root), mock.patch(
                        "sys.argv", ["python_run.py", "--output", str(output)]
                    ), mock.patch.object(runner.subprocess, "check_output", side_effect=["fixture-sha", "Coverage fixture"]), mock.patch.object(
                        runner.subprocess, "run", side_effect=run
                    ):
                        self.assertEqual(runner.main(), expected)
                    outcomes = json.loads((output / "test-results.json").read_text())
                    self.assertEqual(outcomes["exit_code"], suite_exit)
                    self.assertEqual(outcomes["modules"], ["FAIL fixture.py" if suite_exit else "ok fixture.py"])
                    self.assertEqual(calls, ["run", *(["combine"] if combine_exit else []), "json", "html"])


if __name__ == "__main__":
    unittest.main()
