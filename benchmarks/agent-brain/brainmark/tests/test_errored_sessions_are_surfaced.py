"""A session that did NOT end in success still passes the pre-registered gate.

PREREGISTRATION.md section 5 defines the per-session gate as `usd > 0` and a
non-empty patch. It does not include `subtype == "success"`.

On the primary backend that is looser than it reads. codex reports no cost at
all, so `total_cost_usd` is tokens x the rate card in agents/backends.json --
which currently ships `"_price_status": "UNVERIFIED_PLACEHOLDER"` -- and
`usd > 0` therefore reduces to "burned tokens". A session killed at the
wall-clock timeout (returncode 124) or ending `error_max_turns` still writes a
stream, still has tokens, and collect_patch still collects its partial edits.
It passes, and its locate count is measured on a TRUNCATED session.

Verified on this checkout: a cell with
`result_event.subtype == "error_max_turns"` and a non-empty patch is returned
by report.collect() with `clean == True`.

That is an arm confound whenever failure rate tracks the arm: a bigger packet
is a slower session is a likelier timeout. THE GATE IS NOT CHANGED here --
changing it would be an undated amendment to the pre-registration. What changes
is that the exposure is counted, warned about, and priced with a sensitivity
estimate.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, report  # noqa: E402

ARMS = ["no_brain", "full_brain"]


def _cell(cell: pathlib.Path, instance_id: str, subtype: str, locate: int) -> None:
    cell.mkdir(parents=True, exist_ok=True)
    packet = '{"query":"q","results":[]}'
    (cell / "packet.txt").write_text(packet, encoding="utf-8")
    (cell / "packet.sha256").write_text(_harness.sha256_text(packet) + "\n", encoding="utf-8")
    (cell / "prompt.txt").write_text(
        f"T\n\n--- MEMORY ---\n<frozen-memory-packet>\n{packet}\n</frozen-memory-packet>\n",
        encoding="utf-8")
    (cell / "patch.diff").write_text("diff --git a/x b/x\n", encoding="utf-8")
    (cell / "meta.json").write_text(_harness.pretty_json({
        "instance_id": instance_id,
        "result_event": {"total_cost_usd": 0.02, "subtype": subtype,
                         "is_error": subtype != "success"},
        "mechmetrics": {"locate_calls_pre_edit": locate, "no_edit": False,
                        "tokens": {"total_tokens": 100}},
    }), encoding="utf-8")


class ErroredSessionsAreSurfacedTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.results = self.tmp / "B"
        for index in (0, 1):
            for offset, arm in enumerate(ARMS):
                _cell(self.results / f"a{index}__then__b{index}" / arm,
                      f"b{index}", "success", 3 + offset)
        _cell(self.results / "a2__then__b2" / "no_brain", "b2", "error_max_turns", 11)
        _cell(self.results / "a2__then__b2" / "full_brain", "b2", "success", 2)
        self.config = {"_config_sha256": "x"}

    def _report(self) -> dict:
        return report.build_report(self.results, self.config, ARMS, require_seal=False)

    def test_the_errored_cell_still_passes_the_pre_registered_gate(self):
        """The premise. The gate is not changed; it is made visible."""
        self.assertIn("a2__then__b2", self._report()["clean_pairs"])

    def test_errored_cells_are_counted_per_arm(self):
        self.assertEqual(self._report()["errored_cells"],
                         {"no_brain": ["a2__then__b2"]})

    def test_the_exposure_is_warned_about(self):
        built = self._report()
        self.assertTrue(any("subtype" in w for w in built["warnings"]), built["warnings"])

    def test_a_sensitivity_estimate_excludes_them(self):
        built = self._report()
        self.assertEqual(built["n_clean_all_arms_success"], 2)
        self.assertEqual(built["headline_excluding_errored"]["n"], 2)
        self.assertNotEqual(built["headline_excluding_errored"]["geomean_pct"],
                            built["headline"]["geomean_pct"])

    def test_an_all_success_run_carries_no_sensitivity_block(self):
        _cell(self.results / "a2__then__b2" / "no_brain", "b2", "success", 3)
        built = self._report()
        self.assertEqual(built["errored_cells"], {})
        self.assertNotIn("headline_excluding_errored", built)
        self.assertEqual(built["n_clean_all_arms_success"], built["n_clean"])

    def test_a_cell_with_no_recorded_subtype_is_treated_as_success(self):
        """Legacy cells predate the field; they must not all become 'errored'."""
        cell = self.results / "a0__then__b0" / "no_brain"
        meta = _harness.pretty_json({
            "instance_id": "b0",
            "result_event": {"total_cost_usd": 0.02},
            "mechmetrics": {"locate_calls_pre_edit": 3, "no_edit": False,
                            "tokens": {"total_tokens": 100}},
        })
        (cell / "meta.json").write_text(meta, encoding="utf-8")
        self.assertNotIn("a0__then__b0", self._report()["errored_cells"].get("no_brain", []))

    def test_the_markdown_shows_the_exposure(self):
        text = report.render_markdown(self._report())
        self.assertIn("did not end in success", text)


if __name__ == "__main__":
    unittest.main()
