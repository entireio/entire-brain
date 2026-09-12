"""The delivered key set must not depend on whether an arm's content overflowed.

run.py's bounder (run.py:4731) returns an under-budget packet BYTE-FOR-BYTE and
adds `_benchmark_delivery` only on the oversized path. The presence of that key
was therefore arm-correlated: `no_brain` (zero results) can never trip it, a
rich arm routinely does.

It told loudest exactly where it mattered. The placebo is size-matched by
TIGHTENING its budget to `len(treatment_packet)`, so whenever the donor pair's
packet carried more content than this pair's treatment, the placebo truncated
and its treatment did not:

    treatment keys: ['query', 'results']
    placebo   keys: ['_benchmark_delivery', 'query', 'results']

The one arm whose scientific value depends on being indistinguishable from the
treatment was the one reliably distinguishable from it.

Seeding the key into every payload makes the SHAPE constant. The VALUES still
say what really happened -- run.py's oversized path overwrites the seed rather
than merging with it -- so nothing about real truncation is hidden.
"""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark.memsources import base, empty, irrelevant  # noqa: E402

QUERY = "fix the parser crash on empty input"
MAX_BYTES = 24576


def _results(count: int, width: int) -> list[dict]:
    return base.normalize_results(
        [{"source": "fact", "id": f"f{i:03d}", "text": "x" * width,
          "score": 1.0 - i / 1000} for i in range(count)], 200)


class PacketShapeSurvivesTruncationTest(unittest.TestCase):
    def test_an_overflowing_and_a_fitting_packet_have_the_same_key_set(self):
        fits = base.build_packet(arm="mem0", query=QUERY, results=_results(3, 50),
                                 max_bytes=MAX_BYTES)
        overflows = base.build_packet(arm="mem0", query=QUERY,
                                      results=_results(200, 2000), max_bytes=MAX_BYTES)
        self.assertTrue(json.loads(overflows.text)["_benchmark_delivery"]["truncated"])
        self.assertFalse(json.loads(fits.text)["_benchmark_delivery"]["truncated"])
        self.assertEqual(sorted(json.loads(fits.text)),
                         sorted(json.loads(overflows.text)))

    def test_the_baseline_matches_a_truncated_arm_in_shape(self):
        baseline = empty.build(query=QUERY, max_bytes=MAX_BYTES, sentinel="(none)")
        overflows = base.build_packet(arm="full_brain", query=QUERY,
                                      results=_results(200, 2000), max_bytes=MAX_BYTES)
        self.assertEqual(sorted(json.loads(baseline.text)),
                         sorted(json.loads(overflows.text)))

    def test_a_size_matched_placebo_matches_its_treatment_in_shape(self):
        donor = base.build_packet(arm="full_brain", query="another pair's question",
                                  results=_results(20, 300), max_bytes=MAX_BYTES)
        treatment = base.build_packet(arm="full_brain", query=QUERY,
                                      results=_results(6, 80), max_bytes=MAX_BYTES)
        placebo = irrelevant.build(
            query=QUERY, max_bytes=MAX_BYTES, top_k=20,
            donor_packet_text=donor.text, donor_pair_id="d", recipient_pair_id="r",
            target_bytes=len(treatment.text.encode("utf-8")),
        )
        self.assertFalse(json.loads(treatment.text)["_benchmark_delivery"]["truncated"])
        self.assertEqual(sorted(json.loads(placebo.text)),
                         sorted(json.loads(treatment.text)))
        self.assertLessEqual(len(placebo.text.encode("utf-8")),
                             len(treatment.text.encode("utf-8")))

    def test_the_real_delivery_counts_are_not_faked_by_the_seed(self):
        overflows = base.build_packet(arm="cmm", query=QUERY,
                                      results=_results(200, 2000), max_bytes=MAX_BYTES)
        note = json.loads(overflows.text)["_benchmark_delivery"]
        self.assertEqual(note["original_result_count"], 200)
        self.assertLess(note["delivered_result_count"], 200)
        self.assertGreater(note["omitted_result_count"], 0)

    def test_a_fitting_packet_reports_nothing_omitted(self):
        fits = base.build_packet(arm="graphify", query=QUERY, results=_results(4, 40),
                                 max_bytes=MAX_BYTES)
        note = json.loads(fits.text)["_benchmark_delivery"]
        self.assertEqual(note["delivered_result_count"], 4)
        self.assertEqual(note["omitted_result_count"], 0)
        self.assertEqual(note["original_result_count"], 4)


if __name__ == "__main__":
    unittest.main()
