#!/usr/bin/env python3
"""`role: "sealed"` must mean something.

`memory_bundle.role` is copied verbatim into every memory record, into the prep
provenance, and from there into the run record a reviewer reads. It was
validated only against the set {development, sealed} -- nothing required a
sealed bundle to carry the pins it claims. `source_artifact` was optional and
`validate_temporal_source_artifact` returns on its first line when it is
missing, so a bundle could declare itself sealed and have zero of its
transcript, history, and fact hashes compared.

The same hole opened on a typo: `memory_bundle` had no closed key set, so
`source_artifacts` (plural) parsed, validated, and disabled every content check
in silence.
"""

from __future__ import annotations

import copy
import importlib.util
import pathlib
import sys
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("run", HERE / "run.py")
assert SPEC is not None and SPEC.loader is not None
run = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = run
SPEC.loader.exec_module(run)


def _sealed_bundle() -> dict:
    return {
        "role": "sealed",
        "checkpoint_ref_commit": "a" * 40,
        "cutoff_at": "2026-01-01T00:00:00Z",
        "session_ids": ["session-1"],
        "retrieval_branch": "main",
        "require_facts": True,
        "source_artifact": {
            "cache_key": "b" * 24,
            "transcript_sha256": ["c" * 64],
            "history_sha256": "d" * 64,
            "fact_artifact_sha256": ["e" * 64],
        },
        "distill": {"agent": "codex", "model": "gpt-x", "effort": "high"},
    }


def _task(bundle: dict) -> dict:
    return {"id": "t", "memory_bundle": bundle}


class SealedBundleMustCarryItsPinsTest(unittest.TestCase):
    def test_a_well_formed_sealed_bundle_still_validates(self) -> None:
        bundle = _sealed_bundle()
        self.assertIs(run.memory_bundle_config(_task(bundle)), bundle)

    def test_sealed_without_a_source_artifact_is_refused(self) -> None:
        bundle = _sealed_bundle()
        del bundle["source_artifact"]
        with self.assertRaisesRegex(ValueError, "role=sealed requires a source_artifact"):
            run.memory_bundle_config(_task(bundle))

    def test_a_misspelled_pin_fails_closed_instead_of_disabling_every_check(self) -> None:
        bundle = _sealed_bundle()
        bundle["source_artifacts"] = bundle.pop("source_artifact")
        with self.assertRaisesRegex(ValueError, r"unknown fields: \['source_artifacts'\]"):
            run.memory_bundle_config(_task(bundle))

    def test_a_misspelled_hash_field_fails_closed(self) -> None:
        bundle = _sealed_bundle()
        bundle["source_artifact"]["fact_artifacts_sha256"] = bundle["source_artifact"].pop(
            "fact_artifact_sha256"
        )
        with self.assertRaisesRegex(ValueError, "source_artifact"):
            run.memory_bundle_config(_task(bundle))

    def test_a_misspelled_distill_pin_fails_closed(self) -> None:
        bundle = _sealed_bundle()
        bundle["distill"]["models"] = bundle["distill"].pop("model")
        with self.assertRaisesRegex(ValueError, r"distill has unknown fields: \['models'\]"):
            run.memory_bundle_config(_task(bundle))

    def test_development_bundles_stay_unpinned_by_design(self) -> None:
        bundle = _sealed_bundle()
        bundle["role"] = "development"
        del bundle["source_artifact"]
        self.assertIs(run.memory_bundle_config(_task(bundle)), bundle)

    def test_a_sealed_bundle_reaches_the_content_checks(self) -> None:
        # With the pin present, validate_temporal_source_artifact compares rather
        # than returning; a record that does not match the pins is rejected.
        task = _task(_sealed_bundle())
        self.assertIsNotNone(run.temporal_source_artifact_config(task))
        with self.assertRaisesRegex(RuntimeError, "pinned temporal source artifact failed validation"):
            run.validate_temporal_source_artifact(
                task,
                "b" * 24,
                {"key": "b" * 24},
                {
                    "checkpoint_ref_commit": "a" * 40,
                    "cutoff_at": "2026-01-01T00:00:00Z",
                    "selected_sessions": [{"session_id": "session-1", "transcript_sha256": "f" * 64}],
                    "facts": {"artifacts": [{"sha256": "e" * 64}]},
                    "history_index": {"sha256": "d" * 64},
                },
            )

    def test_every_committed_task_bundle_still_parses(self) -> None:
        import json

        checked = 0
        for path in sorted((HERE / "tasks").rglob("*.json")):
            try:
                payload = json.loads(path.read_text())
            except json.JSONDecodeError:
                continue
            if not isinstance(payload, dict) or not isinstance(payload.get("memory_bundle"), dict):
                continue
            checked += 1
            run.memory_bundle_config(copy.deepcopy(payload))
        self.assertGreater(checked, 0)


if __name__ == "__main__":
    unittest.main()
