import json
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
from io import StringIO
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPT_DIR))

import plan_shards  # noqa: E402
import prepare  # noqa: E402
import run_other  # noqa: E402
import run_shard  # noqa: E402


def inventory(package, argument, directory, binary, roots):
    return {
        "importPath": package,
        "packageArgument": argument,
        "packageDirectoryRelative": directory,
        "binaryName": binary,
        "binarySha256": "a" * 64,
        "binarySizeBytes": 123,
        "repositorySha": "b" * 40,
        "goVersion": "go version go1.26.7 windows/amd64",
        "targetEnvironment": dict(plan_shards.TARGET_ENVIRONMENT),
        "listingContract": {
            "arguments": ["-test.paniconexit0", "-test.list=."],
            "paniconexit0": True,
            "pwdMatchesPackageDirectory": True,
            "goToolDirectoryPrependedToPath": True,
        },
        "raceEnabled": True,
        "coverageEnabled": True,
        "coverageMode": "atomic",
        "coveragePackages": "./...",
        "testMainDeclarations": [],
        "roots": roots,
        "excludedBenchmarks": [],
    }


def two_heavy_settings(shard_count=4):
    return {
        "schema": plan_shards.SETTINGS_SCHEMA,
        "shardCount": shard_count,
        "defaultWeightSeconds": 1.0,
        "timeout": "30m",
        "commandLineLimit": 30000,
        "shuffle": "off",
        "testParallel": None,
        "goMaxProcs": None,
        "heavyPackages": [
            {
                "argument": "./heavy",
                "importPath": "example/heavy",
                "binaryName": "heavy.test.exe",
                "expectedTestMainDeclarations": [],
            },
            {
                "argument": "./heavy2",
                "importPath": "example/heavy2",
                "binaryName": "heavy2.test.exe",
                "expectedTestMainDeclarations": [],
            },
        ],
    }


def two_heavy_inventories():
    return [
        inventory(
            "example/heavy",
            "./heavy",
            "internal/heavy",
            "heavy.test.exe",
            [
                {"name": "TestA", "kind": "Test"},
                {"name": "TestB", "kind": "Test"},
                {"name": "ExampleC", "kind": "Example"},
            ],
        ),
        inventory(
            "example/heavy2",
            "./heavy2",
            "internal/heavy2",
            "heavy2.test.exe",
            [
                {"name": "TestX", "kind": "Test"},
                {"name": "TestY", "kind": "Test"},
            ],
        ),
    ]


WEIGHTS = {}
WEIGHT_METADATA = {"path": "weights.json", "sha256": "c" * 64, "entryCount": 0}


class AffectedSelectionTests(unittest.TestCase):
    def load(self, payload):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "affected.json"
            path.write_text(payload, encoding="utf-8")
            with redirect_stderr(StringIO()):
                return prepare.load_affected_selection(path)

    def test_absent_flag_selects_the_full_suite(self):
        self.assertIsNone(prepare.load_affected_selection(None))

    def test_valid_selected_decision_returns_the_package_set(self):
        decision = json.dumps({"mode": "selected", "packages": ["b", "a"]})
        self.assertEqual(self.load(decision), frozenset({"a", "b"}))

    def test_malformed_or_non_selected_decisions_fall_back_to_full(self):
        payloads = (
            "not json",
            "[]",
            json.dumps({"mode": "full", "reason": "non-Go change"}),
            json.dumps({"mode": "none"}),
            json.dumps({"mode": "selected"}),
            json.dumps({"mode": "selected", "packages": "a"}),
            json.dumps({"mode": "selected", "packages": []}),
            json.dumps({"mode": "selected", "packages": [1]}),
            json.dumps({"mode": "selected", "packages": ["a b"]}),
            json.dumps({"mode": "selected", "packages": [""]}),
        )
        for payload in payloads:
            with self.subTest(payload=payload):
                self.assertIsNone(self.load(payload))

    def test_missing_decision_file_falls_back_to_full(self):
        with redirect_stderr(StringIO()):
            self.assertIsNone(
                prepare.load_affected_selection(Path("does-not-exist/affected.json"))
            )

    def test_selection_naming_unlisted_packages_falls_back_to_full(self):
        with redirect_stderr(StringIO()):
            self.assertEqual(
                prepare.consistent_selection(frozenset({"a"}), {"a", "b"}),
                frozenset({"a"}),
            )
            self.assertIsNone(
                prepare.consistent_selection(frozenset({"a", "ghost"}), {"a", "b"})
            )
            self.assertIsNone(prepare.consistent_selection(None, {"a"}))


class SelectedPlanTests(unittest.TestCase):
    def test_no_selection_matches_the_four_argument_plan_exactly(self):
        inventories = two_heavy_inventories()
        settings = two_heavy_settings(shard_count=2)
        baseline = plan_shards.create_plan(inventories, settings, WEIGHTS, WEIGHT_METADATA)
        explicit = plan_shards.create_plan(
            inventories, settings, WEIGHTS, WEIGHT_METADATA, selected_packages=None
        )
        self.assertEqual(baseline, explicit)
        self.assertNotIn("selection", baseline)

    def test_selection_weight_accounting_describes_planned_roots_only(self):
        weights = {
            ("example/heavy", "TestA"): 30.0,
            ("example/heavy", "TestB"): 20.0,
            ("example/heavy2", "TestX"): 10.0,
        }
        metadata = dict(WEIGHT_METADATA, entryCount=len(weights))
        plan = plan_shards.create_plan(
            two_heavy_inventories(),
            two_heavy_settings(shard_count=2),
            weights,
            metadata,
            selected_packages=frozenset({"example/heavy2"}),
        )
        historical = plan["historicalWeights"]
        self.assertEqual(historical["usedCount"], 1)
        self.assertEqual(historical["defaultedCount"], plan["rootCount"] - 1)
        self.assertGreaterEqual(historical["defaultedCount"], 0)
        self.assertEqual(
            historical["unusedKeys"],
            ["example/heavy::TestA", "example/heavy::TestB"],
        )

    def test_selected_packages_filter_planned_roots_only(self):
        selected = frozenset({"example/heavy2", "example/nonheavy"})
        plan = plan_shards.create_plan(
            two_heavy_inventories(),
            two_heavy_settings(shard_count=4),
            WEIGHTS,
            WEIGHT_METADATA,
            selected_packages=selected,
        )
        self.assertEqual(plan["rootCount"], 2)
        self.assertEqual(plan["shardCount"], 2)
        self.assertEqual(
            plan["selection"],
            {"mode": "selected", "packages": ["example/heavy2", "example/nonheavy"]},
        )
        planned = {
            (assignment["importPath"], root["name"])
            for shard in plan["shards"]
            for assignment in shard["assignments"]
            for root in assignment["roots"]
        }
        self.assertEqual(
            planned, {("example/heavy2", "TestX"), ("example/heavy2", "TestY")}
        )
        # Every heavy package stays compiled and hashed in the plan evidence.
        self.assertEqual(
            {(item["importPath"], item["rootCount"]) for item in plan["packages"]},
            {("example/heavy", 3), ("example/heavy2", 2)},
        )

    def test_empty_selection_emits_a_minimal_valid_plan(self):
        plan = plan_shards.create_plan(
            two_heavy_inventories(),
            two_heavy_settings(shard_count=4),
            WEIGHTS,
            WEIGHT_METADATA,
            selected_packages=frozenset({"example/untouched"}),
        )
        self.assertEqual(plan["rootCount"], 0)
        self.assertEqual(plan["shardCount"], 1)
        self.assertEqual(
            plan["shards"],
            [
                {
                    "index": 0,
                    "estimatedWeightSeconds": 0.0,
                    "rootCount": 0,
                    "assignments": [],
                }
            ],
        )
        # The workflow's shard-matrix validation: indexes match shardCount and
        # at least one shard exists.
        indexes = [shard["index"] for shard in plan["shards"]]
        self.assertEqual(len(indexes), plan["shardCount"])
        self.assertTrue(indexes == list(range(plan["shardCount"])) and indexes)


class ExecutorSelectionTests(unittest.TestCase):
    def test_empty_shard_assignments_require_a_selected_plan(self):
        selected_plan = {"selection": {"mode": "selected", "packages": ["a"]}}
        self.assertEqual(
            run_shard.validated_assignments(selected_plan, {"assignments": []}, 0), []
        )
        with self.assertRaisesRegex(ValueError, "no assignments"):
            run_shard.validated_assignments({}, {"assignments": []}, 0)
        with self.assertRaisesRegex(ValueError, "malformed"):
            run_shard.validated_assignments(selected_plan, {"assignments": None}, 0)

    def test_non_heavy_packages_follow_the_plan_selection(self):
        packages = ["example/a", "example/b"]
        self.assertEqual(run_other.apply_plan_selection(packages, {}), packages)
        narrowed = run_other.apply_plan_selection(
            packages, {"selection": {"mode": "selected", "packages": ["example/b", "x"]}}
        )
        self.assertEqual(narrowed, ["example/b"])
        empty = run_other.apply_plan_selection(
            packages, {"selection": {"mode": "selected", "packages": ["x"]}}
        )
        self.assertEqual(empty, [])
        for selection in ({"mode": "full"}, {"mode": "selected", "packages": []}, []):
            with self.subTest(selection=selection), self.assertRaisesRegex(
                ValueError, "selection"
            ):
                run_other.apply_plan_selection(packages, {"selection": selection})


if __name__ == "__main__":
    unittest.main()
