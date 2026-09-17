import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("coverage_report", Path(__file__).with_name("report.py"))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class CoverageReportTests(unittest.TestCase):
    def profiles(self, *contents):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        paths = []
        for i, content in enumerate(contents):
            path = Path(directory.name) / str(i)
            path.write_text(content)
            paths.append(path)
        return paths

    def test_union_counts_shared_blocks_once_and_binary_adds_hits(self):
        paths = self.profiles(
            "mode: atomic\nmodule/a.go:1.1,3.2 3 0\nmodule/a.go:5.1,8.2 5 2\n",
            "mode: atomic\nmodule/a.go:1.1,3.2 3 1\nmodule/a.go:5.1,8.2 5 0\nmodule/b.go:1.1,2.2 2 0\n",
        )
        result = report.summarize(report.read_profiles(paths))
        self.assertEqual(result["totals"], {"statements": 10, "covered": 8, "percent": 80.0})

    def test_incompatible_profiles_cannot_silently_change_denominator(self):
        paths = self.profiles("mode: set\na.go:1.1,2.2 1 1\n", "mode: set\na.go:1.1,2.2 2 1\n")
        with self.assertRaisesRegex(ValueError, "incompatible"):
            report.read_profiles(paths)

    def test_empty_or_corrupt_profile_is_not_full_coverage(self):
        for text in ("", "mode: atomic\n", "garbage\n", "mode: set\na.go:1.1,2.2 1 -1\n"):
            with self.subTest(text=text), self.assertRaises(ValueError):
                report.read_profiles(self.profiles(text))

    def test_comparison_detects_regression_and_rejects_different_builds(self):
        environment = dict(go_version="go1.27.1", platform="Linux/amd64", tags="", race=True, scope="all", tests_exit_code=0)
        baseline = {"environment": environment, "totals": {"percent": 82}}
        current = {"environment": environment.copy(), "totals": {"percent": 81}}
        self.assertEqual(report.compare(current, baseline), -1)
        for key in ("go_version", "platform", "tags", "race", "scope"):
            with self.subTest(key=key):
                current["environment"] = dict(environment, **{key: "different"})
                with self.assertRaisesRegex(ValueError, key):
                    report.compare(current, baseline)
        current["environment"] = dict(environment, tests_exit_code=1)
        with self.assertRaisesRegex(ValueError, "passing test runs"):
            report.compare(current, baseline)

    def test_windows_filename_keeps_drive_letter(self):
        result = report.summarize(report.read_profiles(self.profiles("mode: set\nC:/src/a.go:1.1,2.2 1 1\n")))
        self.assertIn("C:/src/a.go", result["files"])


if __name__ == "__main__":
    unittest.main()
