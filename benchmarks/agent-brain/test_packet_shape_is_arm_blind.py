#!/usr/bin/env python3
"""The delivered memory packet's SHAPE must not encode how much memory an arm had.

`bound_memory_packet` is the one bounder every arm of every benchmark that
imports this harness is delivered through. It used to return an under-budget
response byte-for-byte and add `_benchmark_delivery` only on the oversized
path, so the presence of that key -- and the canonical-vs-source serialization
that came with it -- was a harness-authored marker correlated with arm content.

These tests pin the invariant at the source: same top-level key set, same
serialization, whether or not the packet truncated.
"""

from __future__ import annotations

import importlib.util
import json
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


def _canonical(value) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=True)


def _response(count: int, text_len: int, query: str = "q") -> str:
    return _canonical(
        {
            "query": query,
            "results": [
                {"source": "memory", "id": f"r{index}", "score": 1.0, "text": "x" * text_len}
                for index in range(count)
            ],
        }
    )


MAX_BYTES = 2048


class PacketShapeIsArmBlindTest(unittest.TestCase):
    def test_fitting_and_overflowing_packets_share_a_key_set(self) -> None:
        fits, fit_meta = run.bound_memory_packet(_response(1, 20), MAX_BYTES)
        overflows, over_meta = run.bound_memory_packet(_response(20, 400), MAX_BYTES)
        self.assertFalse(fit_meta["truncated"])
        self.assertTrue(over_meta["truncated"])
        self.assertEqual(
            sorted(json.loads(fits)), sorted(json.loads(overflows))
        )
        self.assertIn("_benchmark_delivery", json.loads(fits))

    def test_an_empty_arm_is_shape_identical_to_a_truncated_one(self) -> None:
        # The baseline case that can never truncate, against the richest arm.
        empty, _ = run.bound_memory_packet(_response(0, 0), MAX_BYTES)
        rich, meta = run.bound_memory_packet(_response(40, 500), MAX_BYTES)
        self.assertTrue(meta["truncated"])
        self.assertEqual(sorted(json.loads(empty)), sorted(json.loads(rich)))

    def test_a_size_matched_placebo_is_shape_identical_to_its_treatment(self) -> None:
        # A placebo is size-matched by tightening its budget to the treatment's
        # delivered length, which is exactly when it truncates and its treatment
        # does not. That pair must still be indistinguishable by shape.
        treatment, t_meta = run.bound_memory_packet(_response(3, 120), MAX_BYTES)
        self.assertFalse(t_meta["truncated"])
        placebo, p_meta = run.bound_memory_packet(
            _response(9, 300, query="q"), len(treatment.encode("utf-8"))
        )
        self.assertTrue(p_meta["truncated"])
        self.assertEqual(sorted(json.loads(treatment)), sorted(json.loads(placebo)))

    def test_the_delivered_bytes_are_canonical_on_both_paths(self) -> None:
        # A source response that is pretty-printed (Go's encoder, a hand-written
        # fixture) must not be handed through verbatim while the truncating path
        # emits compact canonical JSON: whitespace and key order are shape too.
        pretty = json.dumps({"results": [{"id": "a", "text": "short"}], "query": "q"}, indent=2)
        delivered, meta = run.bound_memory_packet(pretty, MAX_BYTES)
        self.assertFalse(meta["truncated"])
        self.assertEqual(delivered, _canonical(json.loads(delivered)))
        truncated, t_meta = run.bound_memory_packet(_response(20, 400), MAX_BYTES)
        self.assertTrue(t_meta["truncated"])
        self.assertEqual(truncated, _canonical(json.loads(truncated)))

    def test_the_note_reports_the_real_delivery_not_a_constant(self) -> None:
        fits, _ = run.bound_memory_packet(_response(4, 20), MAX_BYTES)
        note = json.loads(fits)["_benchmark_delivery"]
        self.assertEqual(
            note,
            {
                "max_bytes": MAX_BYTES,
                "original_result_count": 4,
                "delivered_result_count": 4,
                "omitted_result_count": 0,
                "partial_last_result": False,
                "truncated": False,
            },
        )
        overflows, _ = run.bound_memory_packet(_response(20, 400), MAX_BYTES)
        over_note = json.loads(overflows)["_benchmark_delivery"]
        self.assertTrue(over_note["truncated"])
        self.assertEqual(over_note["original_result_count"], 20)
        self.assertGreater(over_note["omitted_result_count"], 0)
        self.assertEqual(
            over_note["delivered_result_count"] + over_note["omitted_result_count"], 20
        )

    def test_the_note_is_paid_for_out_of_the_budget_by_every_arm(self) -> None:
        # The note is not free: a packet that fit only because the note was
        # absent must truncate rather than overrun the preregistered budget.
        source = _response(2, 200)
        exact = len(source.encode("utf-8"))
        delivered, meta = run.bound_memory_packet(source, exact)
        self.assertLessEqual(len(delivered.encode("utf-8")), exact)
        self.assertTrue(meta["truncated"])
        self.assertIn("_benchmark_delivery", json.loads(delivered))

    def test_metadata_that_cannot_fit_still_fails_closed(self) -> None:
        with self.assertRaisesRegex(ValueError, "metadata exceeds the packet byte budget"):
            run.bound_memory_packet(_response(3, 100), 64)


if __name__ == "__main__":
    unittest.main()
