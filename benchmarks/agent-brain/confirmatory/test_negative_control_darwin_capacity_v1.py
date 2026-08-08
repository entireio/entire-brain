from __future__ import annotations

import copy
import json
import pathlib
import subprocess
import tempfile
import unittest

import draft202012


ROOT = pathlib.Path(__file__).resolve().parent
SOURCE = ROOT / "negative_control_darwin_capacity_v1.swift"
SCHEMA = ROOT / "schemas" / "negative-control-darwin-capacity-observation-v1.schema.json"
REQUIRED_CAPACITY_BYTES = 16 * 1024 * 1024 * 1024
FIXED_ERROR = b"error: Darwin capacity observation unavailable\n"


class DarwinCapacityObservationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.binary = pathlib.Path(cls.temporary.name) / "capacity-observer"
        compile_result = subprocess.run(
            [
                "/usr/bin/xcrun",
                "swiftc",
                "-warnings-as-errors",
                str(SOURCE),
                "-o",
                str(cls.binary),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=120,
        )
        if compile_result.returncode != 0:
            raise AssertionError(
                compile_result.stderr.decode("utf-8", errors="replace")
            )
        cls.result = subprocess.run(
            [str(cls.binary), "/System/Volumes/Data"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
        cls.value = json.loads(cls.result.stdout)
        schema = json.loads(SCHEMA.read_text(encoding="utf-8"))
        cls.validator = draft202012.Validator(
            [draft202012.SchemaDocument(SCHEMA.name, schema)]
        )

    def test_live_observation_is_canonical_and_schema_valid(self) -> None:
        self.assertEqual(self.result.returncode, 0, self.result.stderr)
        self.assertEqual(self.result.stderr, b"")
        expected = (
            json.dumps(
                self.value,
                ensure_ascii=False,
                allow_nan=False,
                separators=(",", ":"),
                sort_keys=True,
            )
            + "\n"
        ).encode("utf-8")
        self.assertEqual(self.result.stdout, expected)
        self.validator.validate(self.value, SCHEMA.name, label="capacity observation")

    def test_threshold_semantics_and_authority_are_fail_closed(self) -> None:
        measurement = self.value["measurement"]
        total = measurement["total_capacity_bytes"]
        raw_free = measurement["available_capacity_bytes"]
        important = measurement["available_capacity_for_important_usage_bytes"]
        opportunistic = measurement[
            "available_capacity_for_opportunistic_usage_bytes"
        ]
        self.assertEqual(measurement["required_capacity_bytes"], REQUIRED_CAPACITY_BYTES)
        self.assertTrue(0 <= raw_free <= total)
        self.assertTrue(0 <= important <= total)
        self.assertTrue(0 <= opportunistic <= total)
        self.assertEqual(
            measurement["important_usage_threshold_met"],
            important >= REQUIRED_CAPACITY_BYTES,
        )
        self.assertEqual(
            measurement["raw_free_threshold_met"],
            raw_free >= REQUIRED_CAPACITY_BYTES,
        )
        expected_status = (
            "important_usage_meets_threshold_unattested"
            if important >= REQUIRED_CAPACITY_BYTES
            else "important_usage_below_threshold_unattested"
        )
        self.assertEqual(measurement["threshold_status"], expected_status)
        self.assertEqual(
            self.value["execution_status"],
            "forbidden_missing_trusted_observer_and_atomic_reservation",
        )
        self.assertEqual(self.value["reservation"]["status"], "absent_not_implemented")
        self.assertIsNone(self.value["reservation"]["reserved_bytes"])

    def test_observation_does_not_expose_the_supplied_path(self) -> None:
        self.assertNotIn(b"/System/Volumes/Data", self.result.stdout)
        self.assertNotIn(b"/Users/", self.result.stdout)

    def test_schema_rejects_inconsistent_threshold_claims(self) -> None:
        important = copy.deepcopy(self.value)
        important["measurement"][
            "available_capacity_for_important_usage_bytes"
        ] = REQUIRED_CAPACITY_BYTES - 1
        important["measurement"]["important_usage_threshold_met"] = True
        important["measurement"][
            "threshold_status"
        ] = "important_usage_meets_threshold_unattested"
        with self.assertRaises(draft202012.SchemaError):
            self.validator.validate(important, SCHEMA.name, label="capacity observation")

        raw_free = copy.deepcopy(self.value)
        raw_free["measurement"]["available_capacity_bytes"] = REQUIRED_CAPACITY_BYTES
        raw_free["measurement"]["raw_free_threshold_met"] = False
        with self.assertRaises(draft202012.SchemaError):
            self.validator.validate(raw_free, SCHEMA.name, label="capacity observation")

    def test_invalid_input_has_one_fixed_nonleaking_error(self) -> None:
        result = subprocess.run(
            [str(self.binary), "relative/private/path"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, b"")
        self.assertEqual(result.stderr, FIXED_ERROR)
        self.assertNotIn(b"relative/private/path", result.stderr)

    def test_source_has_no_process_or_execution_adapter_surface(self) -> None:
        source = SOURCE.read_text(encoding="utf-8")
        for forbidden in (
            "Process(",
            "NSTask",
            "posix_spawn",
            "system(",
            "fork(",
            "execve(",
        ):
            self.assertNotIn(forbidden, source)


if __name__ == "__main__":
    unittest.main()
