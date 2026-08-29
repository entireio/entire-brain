"""Prompt symmetry across ALL SIX arms, including the placebo (plan 0.4).

tests/test_prompts_and_packets.py covers the original five. This file extends
the gate to the sixth: a placebo arm that changed the SCAFFOLD -- an extra
"(this memory may be about other code)" hint, a different envelope, a different
delimiter -- would be an arm-asymmetric INSTRUCTION and would destroy the
comparison it exists to enable. Only the packet BYTES may differ.
"""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import memsources, prompts  # noqa: E402
from brainmark.memsources import base as msbase  # noqa: E402
from brainmark.memsources import irrelevant  # noqa: E402

MAX_BYTES = 4096
QUERY = "The widget renderer drops the trailing newline when input is empty."


def stub_packet(arm: str, n_results: int = 3) -> msbase.MemoryPacket:
    if arm == "no_brain":
        return memsources.ARMS["no_brain"](
            query=QUERY, max_bytes=MAX_BYTES,
            sentinel="(no prior-session memory available)")
    if arm == irrelevant.ARM:
        donor = msbase.build_packet(
            arm="full_brain", query="a DIFFERENT pair's problem statement",
            results=msbase.normalize_results(
                [{"memory": f"donor detail {i} about src/other.py",
                  "score": 1.0 - i * 0.1, "id": f"d-{i}"} for i in range(n_results)], 20),
            max_bytes=MAX_BYTES).text
        return irrelevant.build(query=QUERY, max_bytes=MAX_BYTES, top_k=20,
                                donor_packet_text=donor, donor_pair_id="other",
                                recipient_pair_id="this")
    results = [
        {"memory": f"{arm} recalled detail {i} about src/widget.py",
         "score": 1.0 - i * 0.1, "id": f"{arm}-{i}", "source": arm}
        for i in range(n_results)
    ]
    return msbase.build_packet(
        arm=arm, query=QUERY, results=msbase.normalize_results(results, 20),
        max_bytes=MAX_BYTES, prep={"seconds": 0.0})


class SixArmSymmetryTest(unittest.TestCase):
    def test_all_six_arms_share_one_symmetry_sha(self):
        built = {arm: prompts.build_prompt(QUERY, stub_packet(arm).text)
                 for arm in prompts.ARMS}
        self.assertEqual(len(built), 6)
        shas = {arm: prompts.symmetry_sha(text) for arm, text in built.items()}
        self.assertEqual(
            len(set(shas.values())), 1,
            f"arms received different INSTRUCTIONS, not just different packets: {shas}")
        self.assertEqual(prompts.assert_symmetric(built), next(iter(shas.values())))

    def test_the_placebo_matches_the_five_arm_sha_exactly(self):
        """Adding the 6th arm must not have moved the other five's scaffold."""
        five = {arm: prompts.build_prompt(QUERY, stub_packet(arm).text)
                for arm in prompts.ARMS if arm != prompts.PLACEBO_ARM}
        placebo = prompts.build_prompt(QUERY, stub_packet(prompts.PLACEBO_ARM).text)
        self.assertEqual(prompts.symmetry_sha(placebo), prompts.assert_symmetric(five))

    def test_all_six_packets_actually_differ(self):
        texts = {arm: stub_packet(arm).text for arm in prompts.ARMS}
        self.assertEqual(len(set(texts.values())), 6, "the gate must not pass by identity")

    def test_the_placebo_packet_is_about_other_code(self):
        payload = json.loads(stub_packet(prompts.PLACEBO_ARM).text)
        self.assertTrue(all("other.py" in r["text"] for r in payload["results"]))
        self.assertFalse(any("widget.py" in r["text"] for r in payload["results"]))

    def test_an_asymmetric_placebo_hint_is_detected(self):
        """MUTATION TARGET: give the placebo its own wording and this must raise."""
        built = {arm: prompts.build_prompt(QUERY, stub_packet(arm).text)
                 for arm in prompts.ARMS}
        built[prompts.PLACEBO_ARM] = built[prompts.PLACEBO_ARM].replace(
            "--- MEMORY ---", "--- MEMORY (possibly about unrelated code) ---")
        with self.assertRaises(ValueError) as ctx:
            prompts.assert_symmetric(built)
        self.assertIn("SYMMETRY VIOLATION", str(ctx.exception))

    def test_arm_constants_agree(self):
        self.assertEqual(prompts.BASELINE_ARM, "no_brain")
        self.assertEqual(prompts.HEADLINE_ARM, "full_brain")
        self.assertIn(prompts.BASELINE_ARM, prompts.ARMS)
        self.assertIn(prompts.HEADLINE_ARM, prompts.ARMS)
        self.assertIn(prompts.PLACEBO_ARM, prompts.ARMS)

    def test_policy_names_no_arm(self):
        """The policy text must not identify any memory source, placebo included.

        Three words are deliberately NOT in this list, and each for a reason
        that would otherwise look like a violation:
          * "irrelevant" -- the clause "the packet may be empty, stale, or
            irrelevant" predates the placebo arm, goes verbatim to all six arms,
            and describes a possibility rather than naming a source;
          * "mem0"/"graphify"/"cmm" -- they appear only in the do-NOT-run tool
            list, which every arm receives identically. Naming a tool that is
            forbidden to everyone is symmetric; naming the tool that produced
            THIS arm's packet would not be.
        What must never appear is a word that tells an agent WHICH source fed it.
        """
        for token in ("placebo", "full_brain", "no_brain",
                      "another pair", "donor", "prior pair", "control arm"):
            with self.subTest(token=token):
                self.assertNotIn(token, prompts.POLICY.lower())

    def test_the_packet_never_reveals_which_arm_produced_it_to_the_prompt(self):
        """This test's NAME was always the right invariant; its body used to
        assert the opposite -- that `"arm":"irrelevant"` IS in packet.text,
        "for auditing". Auditing reads the artifact, not the wire: the arm now
        lives on MemoryPacket.arm and in `meta`, and NOTHING the agent reads
        names the condition."""
        packet = stub_packet(prompts.PLACEBO_ARM)
        # Scoped to the PACKET, not the whole prompt: POLICY names mem0 /
        # graphify / cmm in a forbid-list every arm receives identically, which
        # test_policy_names_no_arm already pins as symmetric.
        for arm in prompts.ARMS:
            with self.subTest(arm=arm):
                self.assertNotIn(arm, packet.text)
        self.assertNotIn("arm", json.loads(packet.text))
        self.assertEqual(packet.arm, "irrelevant")


if __name__ == "__main__":
    unittest.main()
