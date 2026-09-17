import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("go_coverage_runner", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


@unittest.skipUnless(shutil.which("go") and shutil.which("git"), "requires Go and Git")
class GoCoverageRunnerTests(unittest.TestCase):
    def test_binary_only_skips_suite_and_full_mode_preserves_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            hooks = root / "empty-hooks"
            hooks.mkdir()
            subprocess.run(["git", "-C", str(root), "-c", f"core.hooksPath={hooks}",
                            "-c", "commit.gpgsign=false", "-c", "user.name=Coverage Test",
                            "-c", "user.email=test@example.invalid", "commit", "--allow-empty",
                            "-qm", "fixture"], check=True)
            (root / "go.mod").write_text("module example.com/binary-fixture\n\ngo 1.20\n")
            command = root / "cmd/entire-brain"
            command.mkdir(parents=True)
            (command / "main.go").write_text('''package main
import ("fmt"; "os")
func main() {
    switch os.Args[1] {
    case "--help": fmt.Println("Usage: fixture")
    case "version": fmt.Println("dev")
    case "capabilities": fmt.Println(`{"schema_version":1,"build":{"brain_cgo":false}}`)
    default: fmt.Fprintln(os.Stderr, "unknown command"); os.Exit(1)
    }
}
''')
            (command / "main_test.go").write_text('''package main
import "testing"
func TestMustFailIfSuiteRuns(t *testing.T) { t.Fatal("intentional suite failure") }
''')
            for binary_only in (True, False):
                with self.subTest(binary_only=binary_only):
                    output = root / ("binary-report" if binary_only else "full-report")
                    arguments = ["run.py", "--output", str(output)]
                    if binary_only:
                        arguments.append("--binary-only")
                    with contextlib.redirect_stdout(io.StringIO()), mock.patch.object(runner, "ROOT", root), mock.patch("sys.argv", arguments):
                        result = runner.main()
                    self.assertEqual(result, 0 if binary_only else 1)
                    self.assertEqual((output / "tests.out").exists(), not binary_only)
                    self.assertEqual((output / "tests.jsonl").exists(), not binary_only)
                    summary = json.loads((output / "summary.json").read_text())
                    environment = json.loads((output / "environment.json").read_text())
                    self.assertEqual(summary["environment"], environment)
                    self.assertEqual(environment["tests_exit_code"], result)
                    self.assertEqual(environment["profileSha256"], hashlib.sha256((output / "binary.out").read_bytes()).hexdigest())
                    self.assertGreater(summary["totals"]["covered"], 0)
                    self.assertTrue((output / "coverage.html").is_file())
                    self.assertFalse((output / "entire-brain").exists())
                    self.assertFalse((output / "entire-brain.exe").exists())
                    self.assertFalse((output / "binary-counters").exists())
                    if binary_only:
                        self.assertEqual(environment["scope"], "instrumented binary contracts")


if __name__ == "__main__":
    unittest.main()
