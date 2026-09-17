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


if __name__ == "__main__":
    unittest.main()
