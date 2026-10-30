import json
import re
import unittest
from pathlib import Path


REPOSITORY = Path(__file__).resolve().parents[4]
WORKFLOW = REPOSITORY / ".github" / "workflows" / "test.yml"
SETTINGS = Path(__file__).resolve().parents[1] / "settings.json"


class WorkflowContractTests(unittest.TestCase):
    def test_windows_monolith_is_replaced_but_cross_build_remains(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        test_block = workflow.split("  test:\n", 1)[1].split("  windows-test-prepare:\n", 1)[0]
        build_block = workflow.split("  build:\n", 1)[1].split("  test:\n", 1)[0]
        self.assertIn("os: [ubuntu-latest, macos-latest]", test_block)
        self.assertNotIn("windows-latest", test_block)
        self.assertIn("goos: [darwin, linux, windows]", build_block)

    def test_statement_coverage_reuses_windows_shards_without_a_second_suite(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        self.assertNotIn("\n  coverage-windows:", workflow)
        windows = workflow.split("  windows-test-prepare:\n", 1)[1].split("  benchmark-corpus:\n", 1)[0]
        coverage_commands = [line.strip() for line in windows.splitlines() if "tools/coverage/run.py" in line]
        self.assertEqual(len(coverage_commands), 1)
        self.assertIn("--race --binary-only", coverage_commands[0])
        self.assertNotIn("--test-timeout 30m", windows)
        self.assertIn('--coverage-output "${{ runner.temp }}/windows-statement-coverage"', windows)
        self.assertIn("name: coverage-windows-sharded", windows)
        self.assertIn("windows-statement-coverage/combined.out", windows)

    def test_dynamic_matrix_and_stable_aggregate_check_are_present(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        settings = json.loads(SETTINGS.read_text(encoding="utf-8"))
        self.assertEqual(settings["shardCount"], 6)
        self.assertIn("fromJSON(needs.windows-test-prepare.outputs.shard-matrix)", workflow)
        self.assertIn("name: test (windows-latest)", workflow)
        self.assertIn("windows-test-verify.result", workflow)

    def test_windows_components_follow_the_existing_test_name_scheme(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        component_names = [
            "compile and plan",
            "shard ${{ matrix.shard }}",
            "non-heavy packages",
            "vet",
            "coverage and integrity",
        ]
        for component_name in component_names:
            self.assertIn(
                f"name: test (windows-latest) / {component_name}", workflow
            )
        self.assertNotIn("name: Windows test /", workflow)

    def test_workflow_uses_only_ephemeral_hosted_runners_and_no_baseline(self):
        workflow = WORKFLOW.read_text(encoding="utf-8").lower()
        self.assertNotIn("self-hosted", workflow)
        self.assertNotIn("monolithic-baseline", workflow)
        self.assertNotIn("baseline-events", workflow)
        self.assertIn("permissions:\n  contents: read", workflow)
        self.assertNotIn("-test.bench", workflow)

    def test_shards_skip_module_cache_and_verifier_runs_after_prepare(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        shard_block = workflow.split("  windows-test-shards:\n", 1)[1].split(
            "  windows-test-other:\n", 1
        )[0]
        verify_block = workflow.split("  windows-test-verify:\n", 1)[1].split(
            "  windows-test:\n", 1
        )[0]
        self.assertIn("timeout-minutes: 35", shard_block)
        # Shards execute precompiled binaries from the bundle: they need the Go
        # toolchain (test2json), never the module/build caches, so setup-go's
        # ~25s cache restore stays off.
        self.assertIn("cache: false", shard_block)
        self.assertNotIn("cache-dependency-path: go.sum", shard_block)
        self.assertIn("needs.windows-test-prepare.result == 'success'", verify_block)
        self.assertNotIn("needs.windows-test-shards.result == 'success'", verify_block)

    def test_windows_prepare_selects_affected_packages_on_pull_requests_only(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        prepare_block = workflow.split("  windows-test-prepare:\n", 1)[1].split(
            "  windows-test-shards:\n", 1
        )[0]
        self.assertIn("fetch-depth: 2", prepare_block)
        self.assertIn("if: github.event_name == 'pull_request'", prepare_block)
        self.assertIn(
            "python -B tools/coverage/affected.py --base HEAD^1 --head HEAD",
            prepare_block,
        )
        self.assertIn("'${{ github.event_name }}' -eq 'pull_request'", prepare_block)
        self.assertIn("'--affected'", prepare_block)
        # Shard and non-heavy jobs take selection from the bundled plan only.
        downstream = workflow.split("  windows-test-shards:\n", 1)[1].split(
            "  windows-test-vet:\n", 1
        )[0]
        self.assertNotIn("--affected", downstream)
        self.assertNotIn("affected.py", downstream)

    def test_every_workflow_python_command_disables_bytecode_writes(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")
        commands = [
            line.strip()
            for line in workflow.splitlines()
            if re.search(r"(?:^|[& ])python(?: |$)", line)
        ]
        self.assertTrue(commands)
        for command in commands:
            self.assertRegex(command, r"(?:^|[& ])python -B(?: |$)")


if __name__ == "__main__":
    unittest.main()
