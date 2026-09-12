"""The DELIVERED packet must not name the experimental condition.

prompts.py takes the fairness rule seriously for the scaffold: it drops
run.py's `source_description` interpolation precisely because it "tells the
agent WHICH competitor produced its packet", and `assert_symmetric` refuses to
spawn a pair whose arms received different instructions.

The packet BYTES then put the condition name back. The harness -- not the
memory tool -- writes it, in three places:

    {"arm":"no_brain","note":"(no prior-session memory available)","query":...}
    {"arm":"full_brain","query":...,"results":[...]}
    ..."results":[{"source":"mem0",...},{"source":"mem0",...}, x20]

A model reading its own memory block therefore learns, in the first twenty
bytes, that it is the untreated control (and is additionally told, in prose,
that no prior-session memory is available), or that it is the product arm, or
which competitor it is standing in for. That is unblinding of the subject, and
it fails the standard test: the sentence "no prior-session memory available"
would obviously change the behaviour of an arm that has no memory.

The DISTINCTION this file draws, and the reason it does not simply demand
identical packets: differences that are the memory tool's own OUTPUT are
legitimate and expected (how many results, what they say, what a backend calls
its own `source` -- entire-brain says "fact"/"history"/"doc" because those are
real record kinds). What is forbidden is a label the HARNESS authored that
names the arm. An empty `results` array is honest tool output for an arm with
no memory; the string "no_brain" is not.
"""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import prompts  # noqa: E402
from brainmark.memsources import base, empty, irrelevant  # noqa: E402

QUERY = "fix the parser crash on empty input"
MAX_BYTES = 24576
SENTINEL = "(no prior-session memory available)"

HITS = [
    {"source": "fact", "id": "f1", "text": "the parser lives in src/p.py", "score": 0.91},
    {"source": "history", "id": "h1", "text": "empty input was patched once", "score": 0.42},
]


def _packet(arm: str):
    if arm == "no_brain":
        return empty.build(query=QUERY, max_bytes=MAX_BYTES, sentinel=SENTINEL)
    return base.build_packet(
        arm=arm, query=QUERY, results=base.normalize_results(HITS, 20), max_bytes=MAX_BYTES
    )


class PacketIsArmBlindTest(unittest.TestCase):
    def test_no_packet_contains_its_own_arm_name(self):
        for arm in prompts.ARMS:
            with self.subTest(arm=arm):
                self.assertNotIn(
                    arm, _packet(arm).text,
                    f"the delivered packet names the condition: an agent in the "
                    f"{arm!r} arm can read {arm!r} out of its own memory block",
                )

    def test_no_packet_contains_any_arm_name(self):
        """Not just its own: naming ANY arm identifies the condition space."""
        for arm in prompts.ARMS:
            text = _packet(arm).text
            for other in prompts.ARMS:
                with self.subTest(arm=arm, names=other):
                    self.assertNotIn(other, text)

    def test_every_arm_delivers_the_same_top_level_key_set(self):
        """A key only the baseline carries is a tell even if its value is bland."""
        keys = {arm: sorted(json.loads(_packet(arm).text)) for arm in prompts.ARMS}
        distinct = {tuple(v) for v in keys.values()}
        self.assertEqual(len(distinct), 1, f"packet shape differs per arm: {keys}")

    def test_the_baseline_is_not_told_in_prose_that_it_has_no_memory(self):
        packet = _packet("no_brain")
        self.assertNotIn(SENTINEL, packet.text)
        self.assertEqual(json.loads(packet.text)["results"], [],
                         "an empty results array IS the honest tool output")

    def test_the_arm_is_still_recorded_for_audit_off_the_wire(self):
        """Removing the label from the BYTES must not lose it from the ARTIFACT."""
        for arm in prompts.ARMS:
            packet = _packet(arm)
            with self.subTest(arm=arm):
                self.assertEqual(packet.arm, arm)
                self.assertEqual(packet.to_provenance()["arm"], arm)
        self.assertEqual(_packet("no_brain").meta.get("empty_sentinel"), SENTINEL)

    def test_a_competitor_result_is_not_stamped_with_the_arm_name(self):
        """_competitor.py defaults a missing `source` to the ARM name, repeating
        it once per result. A backend's own source string is tool output; a
        harness-supplied default that spells the condition is not."""
        from brainmark.memsources import _competitor

        self.assertNotEqual(_competitor.DEFAULT_RESULT_SOURCE, "mem0")
        for arm in ("mem0", "graphify", "cmm"):
            with self.subTest(arm=arm):
                self.assertNotIn(arm, _competitor.DEFAULT_RESULT_SOURCE)

    def test_the_placebo_packet_is_indistinguishable_in_shape(self):
        donor = base.build_packet(
            arm="full_brain", query="another pair's question",
            results=base.normalize_results(HITS, 20), max_bytes=MAX_BYTES,
        )
        placebo = irrelevant.build(
            query=QUERY, max_bytes=MAX_BYTES, top_k=20,
            donor_packet_text=donor.text, donor_pair_id="other", recipient_pair_id="mine",
        )
        treatment = _packet("full_brain")
        self.assertEqual(sorted(json.loads(placebo.text)),
                         sorted(json.loads(treatment.text)))
        self.assertNotIn("irrelevant", placebo.text)


if __name__ == "__main__":
    unittest.main()
