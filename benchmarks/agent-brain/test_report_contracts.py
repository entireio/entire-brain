"""Synthetic input-to-table contracts; plotting/rendering is outside this suite."""
import copy
import csv
import importlib.util
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock


def load_report(name):
    # These scripts import plotting libraries and create output directories at
    # import time. Isolate that boundary: all data loading, aggregation and table
    # writing below use the real code and real temporary files. A chart call would
    # fail because the placeholder modules deliberately have no drawing API.
    matplotlib = types.ModuleType("matplotlib")
    matplotlib.use = lambda backend: None
    pyplot = types.ModuleType("matplotlib.pyplot")
    mplot3d = types.ModuleType("mpl_toolkits.mplot3d")
    mplot3d.Axes3D = object
    dependencies = {
        "matplotlib": matplotlib,
        "matplotlib.pyplot": pyplot,
        "mpl_toolkits": types.ModuleType("mpl_toolkits"),
        "mpl_toolkits.mplot3d": mplot3d,
        "numpy": types.ModuleType("numpy"),
    }
    path = Path(__file__).with_name(name + ".py")
    spec = importlib.util.spec_from_file_location("report_contract_" + name, path)
    module = importlib.util.module_from_spec(spec)
    with mock.patch.dict("sys.modules", dependencies), mock.patch.object(Path, "mkdir"):
        spec.loader.exec_module(module)
    return module


def record(run_id="same-id", condition="no_brain", score=40, tokens=1000):
    return {
        "run_id": run_id,
        "task_id": "entireio-cli-review-base-flag-scope",
        "agent": "codex",
        "runner": {"model": "gpt-5.5", "effort": "low"},
        "condition": condition,
        "validation": {"ok": True},
        "score": {"total": score},
        "agent_info": {
            "returncode": 0, "seconds": 20,
            "usage": {"total_tokens": tokens, "cost_usd": 0.1},
            "activity": {"mcp_tool_calls": 2, "search_calls": 4},
        },
    }


class ReportContracts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def module(self, name):
        module = load_report(name)
        module.RESULTS = self.root
        module.OUT = self.root / (name + "-output")
        module.OUT.mkdir()
        return module

    def suite(self, name, records):
        directory = self.root / name
        directory.mkdir()
        path = directory / "records.ndjson"
        path.write_text("\n".join(json.dumps(r) for r in records) + "\n\n{broken json\n")
        return path

    def collect(self, module):
        return module.collect() if hasattr(module, "collect") else module.load_rows()

    def test_integrity_flags_are_scoped_to_suite_and_inputs_are_unchanged(self):
        a = self.suite("cliproof-tr-a", [record()])
        b = self.suite("cliproof-tr-b", [record()])
        audit = self.root / "codex-audit-report.json"
        audit.write_text(json.dumps({"suites": {
            "cliproof-tr-a": {"records": [{"run_id": "same-id", "flags": ["integrity"]}]},
            "cliproof-tr-b": {"records": [{"run_id": "same-id", "flags": []}]},
        }}))
        before = {path: path.read_bytes() for path in (a, b, audit)}
        for name in ("make_report", "full_report"):
            with self.subTest(report=name):
                rows = self.collect(self.module(name))
                self.assertEqual(len(rows), 1, "a flag must not exclude another suite's same run ID")
                self.assertEqual(rows[0]["score"], 40)
                if name == "make_report":
                    self.assertEqual(rows[0]["suite"], "cliproof-tr-b")
        self.assertEqual(before, {path: path.read_bytes() for path in before})

    def test_infrastructure_failures_are_excluded_but_real_zero_scores_remain(self):
        cases = []
        for kind in ("prep", "exit", "limit", "429", "empty", "no-score"):
            r = record(kind)
            if kind == "prep":
                r["run_id"] = "run__prep__fixture"
            elif kind == "exit":
                r["agent_info"]["returncode"] = 1
            elif kind == "limit":
                r["agent_info"]["stdout_tail"] = "session limit"
            elif kind == "429":
                r["agent_info"]["stderr_tail"] = '{"api_error_status":429}'
            elif kind == "empty":
                r["agent_info"].update(seconds=0, usage={"total_tokens": 0})
            else:
                r["score"]["total"] = None
                r["agent_info"]["usage"]["total_tokens"] = 0
            cases.append(r)
        real_failure = record("real-failure", score=0)
        real_failure["validation"]["ok"] = False
        cases.append(real_failure)
        self.suite("cliproof-rv-failures", cases)
        for name in ("make_report", "full_report"):
            with self.subTest(report=name):
                rows = self.collect(self.module(name))
                self.assertEqual(len(rows), 1)
                self.assertEqual(rows[0]["score"], 0)
                self.assertFalse(rows[0]["valid"])
                self.assertEqual(rows[0]["tokens"], 1000)

    def test_optional_record_fields_have_stable_fallbacks(self):
        r = record()
        del r["runner"]
        del r["agent_info"]["activity"]
        self.suite("cliproof-tr-minimal", [r])
        for name in ("make_report", "full_report"):
            with self.subTest(report=name):
                rows = self.collect(self.module(name))
                self.assertEqual(len(rows), 1)
                self.assertEqual((rows[0]["model"], rows[0]["effort"], rows[0]["mcp"], rows[0]["search"]),
                                 ("codex", "na", 0, 0))

    def test_corrupt_audit_is_not_silently_treated_as_clean(self):
        self.suite("cliproof-tr-a", [record()])
        (self.root / "codex-audit-report.json").write_text("{broken")
        for name in ("make_report", "full_report"):
            with self.subTest(report=name), self.assertRaises(json.JSONDecodeError):
                self.collect(self.module(name))

    def test_collected_runs_produce_independently_expected_tables(self):
        records = [record("base", score=40), record("base2", score=60)]
        records.append(record("cli", "semantic_history_cli_compact", 80, 2000))
        records.append(record("mcp", "mcp_history", 90, 3000))
        records[1]["validation"]["ok"] = False
        self.suite("cliproof-tr-tables", records)
        for name in ("make_report", "full_report"):
            with self.subTest(report=name):
                module = self.module(name)
                rows = self.collect(module)
                original = copy.deepcopy(rows)
                if name == "make_report":
                    module.build_master_table(rows, module.agg(rows))
                    csv_path = module.OUT / "master_table.csv"
                else:
                    module.build_tables(rows)
                    csv_path = module.OUT / "table1_full_master.csv"
                with csv_path.open(newline="") as stream:
                    table = list(csv.DictReader(stream))
                self.assertEqual(len(table), 3)
                base = next(r for r in table if r["condition"].startswith("no_brain"))
                self.assertEqual((base["n"], base["valid"]), ("2", "1/2"))
                self.assertEqual(float(base["score"]), 50)
                cli = next(r for r in table if "CLI" in r["condition"] or r["condition"] == "semantic_history_cli_compact")
                self.assertEqual(float(cli.get("delta", cli.get("Δscore"))), 30)
                self.assertIn("80", csv_path.with_suffix(".md").read_text())
                self.assertEqual(rows, original, "report writing must not mutate records")


if __name__ == "__main__":
    unittest.main()
