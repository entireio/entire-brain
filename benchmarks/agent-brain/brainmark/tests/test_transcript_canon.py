"""codex ThreadEvent JSONL -> the canonical agent JSONL every memory arm reads.

The failure this guards against is SILENT AND TOTAL: a raw codex stream is
parsed by neither entire-brain's distill nor brainmark's competitor ingest, so
the full_brain arm distills zero facts (without erroring) and the mem0 arm
raises. A results table cannot tell "the transcript was unreadable" apart from
"memory did not help", so the property asserted here is not that the translator
runs -- it is that BOTH consumers' parse rules actually match its output.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, transcript_canon  # noqa: E402
from brainmark.memsources._competitor import transcript_to_messages  # noqa: E402

FIXTURES = pathlib.Path(__file__).resolve().parent / "fixtures"

#: The distill preprocessor's allowlist (internal/cli/distill_cmd.go:1502-1543).
#: Anything outside it blanks to "" and contributes nothing.
DISTILL_TOP_LEVEL_TYPES = frozenset({
    "agent_message", "event_msg", "response_item", "assistant", "user", "message",
})


class TranslationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.raw = (FIXTURES / "codex_stream_0148_shape.jsonl").read_bytes()
        self.events = transcript_canon.parse_stream(self.raw)

    def test_a_raw_codex_stream_is_invisible_to_both_consumers(self):
        """The premise. If this ever stops holding, the translator is obsolete."""
        for line in self.raw.decode().splitlines():
            top = json.loads(line).get("type")
            self.assertNotIn(top, DISTILL_TOP_LEVEL_TYPES,
                             f"raw codex line {top!r} is unexpectedly distillable")
        self.assertEqual(transcript_to_messages(self.raw), [],
                         "raw codex stream unexpectedly yields competitor messages")

    def test_translated_lines_are_readable_by_both_consumers(self):
        lines, _ = transcript_canon.translate(self.events, worktree=None)
        body = transcript_canon.render(lines).encode("utf-8")

        for obj in lines:
            self.assertIn(obj["type"], DISTILL_TOP_LEVEL_TYPES)
            self.assertIsInstance(obj["message"], dict,
                                  "competitor ingest requires a `message` dict")

        messages = transcript_to_messages(body)
        self.assertGreater(len(messages), 0,
                           "the mem0 arm would raise 'produced no messages'")

    def test_every_item_kind_is_carried_over(self):
        lines, stats = transcript_canon.translate(self.events, worktree=None)
        self.assertEqual(stats["command_execution"], 6)
        self.assertEqual(stats["file_change"], 1)
        self.assertEqual(stats["agent_message"], 1)
        self.assertEqual(stats["reasoning"], 1)
        # Each shell call becomes a tool_use PLUS its tool_result.
        names = [b["name"] for o in lines for b in o["message"]["content"]
                 if b.get("type") == "tool_use"]
        self.assertEqual(names.count("Bash"), 6)
        self.assertEqual(names.count("Edit"), 1)

    def test_shell_wrappers_are_unwrapped_like_mechmetrics_does(self):
        """One definition of 'the command the agent ran', not two."""
        lines, _ = transcript_canon.translate(self.events, worktree=None)
        commands = [b["input"]["command"] for o in lines
                    for b in o["message"]["content"]
                    if b.get("type") == "tool_use" and b["name"] == "Bash"]
        self.assertEqual(commands[0], "ls -la src")
        self.assertNotIn("/usr/bin/bash", " ".join(commands))

    def test_started_and_completed_are_not_double_counted(self):
        """it_1 and it_3 appear twice in the fixture; the corpus must see one."""
        lines, stats = transcript_canon.translate(self.events, worktree=None)
        ids = [b["id"] for o in lines for b in o["message"]["content"]
               if b.get("type") == "tool_use"]
        self.assertEqual(len(ids), len(set(ids)), ids)
        self.assertEqual(stats["command_execution"], 6)

    def test_the_a_prompt_becomes_the_opening_user_turn(self):
        lines, _ = transcript_canon.translate(
            self.events, worktree=None, first_user_text="THE ISSUE TEXT")
        self.assertEqual(lines[0]["type"], "user")
        self.assertEqual(lines[0]["message"]["content"][0]["text"], "THE ISSUE TEXT")

    def test_harness_paths_are_scrubbed(self):
        wt = "/harness/results/A/pilot/p1/worktree"
        events = [{"type": "item.completed", "item": {
            "id": "x", "type": "command_execution",
            "command": f"/usr/bin/bash -lc 'cat {wt}/src/a.py'",
            "aggregated_output": f"{wt}/src/a.py:1: hello"}}]
        lines, _ = transcript_canon.translate(events, worktree=wt)
        blob = json.dumps(lines)
        self.assertNotIn(wt, blob, "the harness worktree path leaked into the corpus")
        self.assertNotIn("results/A", blob)
        self.assertIn("src/a.py", blob)

    def test_truncated_tail_does_not_lose_the_session(self):
        truncated = self.raw.decode().rsplit("\n", 2)[0] + '\n{"type":"turn.compl'
        lines, stats = transcript_canon.translate(
            transcript_canon.parse_stream(truncated.encode()), worktree=None)
        self.assertGreater(len(lines), 0)
        self.assertEqual(stats["file_change"], 1)


class CanonicalizeTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self.tmp.name)
        self.raw = (FIXTURES / "codex_stream_0148_shape.jsonl").read_bytes()

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def _canonicalize(self):
        return transcript_canon.canonicalize(
            self.dir, self.raw, self.dir / transcript_canon.CANONICAL_NAME,
            worktree=self.dir / "worktree", a_prompt="ISSUE")

    def test_raw_stream_is_kept_and_hashed_beside_the_canonical_one(self):
        prov = self._canonicalize()
        kept = pathlib.Path(prov["raw_codex_path"])
        self.assertEqual(kept.read_bytes(), self.raw, "the raw stream must survive")
        self.assertEqual(prov["raw_codex_sha256"], _harness.sha256_bytes(self.raw))
        canon = pathlib.Path(prov["canonical_path"])
        self.assertEqual(_harness.sha256_file(canon), prov["canonical_sha256"])
        self.assertNotEqual(prov["raw_codex_sha256"], prov["canonical_sha256"])

    def test_the_translation_is_deterministic(self):
        """Every arm gets byte-identical input, and a reviewer can reproduce it."""
        first = self._canonicalize()["canonical_sha256"]
        second = self._canonicalize()["canonical_sha256"]
        self.assertEqual(first, second)

    def test_an_empty_translation_is_refused_not_pinned(self):
        """Pinning zero bytes would give every arm an empty corpus, silently."""
        with self.assertRaises(RuntimeError) as ctx:
            transcript_canon.canonicalize(
                self.dir, b'{"type":"thread.started"}\n',
                self.dir / transcript_canon.CANONICAL_NAME)
        self.assertIn("ZERO lines", str(ctx.exception))


class CanonicalizeDirTest(unittest.TestCase):
    """The post-hoc pass over an already-harvested A dir."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self.tmp.name)
        raw = (FIXTURES / "codex_stream_0148_shape.jsonl").read_bytes()
        self.transcript = self.dir / "session_transcript.jsonl"
        self.transcript.write_bytes(raw)
        (self.dir / "meta.json").write_text(_harness.pretty_json({
            "pair_id": "a1__then__b1",
            "transcript_path": str(self.transcript),
            "transcript_sha256": _harness.sha256_bytes(raw),
            "transcript_source": "codex_event_stream",
        }), encoding="utf-8")

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def _meta(self) -> dict:
        return json.loads((self.dir / "meta.json").read_text(encoding="utf-8"))

    def test_the_pin_moves_onto_the_canonical_bytes(self):
        transcript_canon.canonicalize_dir(self.dir)
        meta = self._meta()
        self.assertEqual(meta["transcript_source"], transcript_canon.TRANSLATED_SOURCE)
        canon = pathlib.Path(meta["transcript_path"])
        self.assertEqual(canon.name, transcript_canon.CANONICAL_NAME)
        self.assertEqual(meta["transcript_sha256"], _harness.sha256_file(canon))
        # ...and the original pin is preserved for audit, not overwritten.
        self.assertEqual(meta["transcript_canonicalized"]["raw_codex_sha256"],
                         _harness.sha256_bytes(self.transcript.read_bytes()))

    def test_it_is_idempotent(self):
        first = transcript_canon.canonicalize_dir(self.dir)
        meta_after_first = self._meta()
        again = transcript_canon.canonicalize_dir(self.dir)
        self.assertEqual(again.get("skipped"), "already canonicalized")
        self.assertEqual(self._meta(), meta_after_first)
        self.assertIn("canonical_sha256", first)

    def test_a_tampered_transcript_is_refused(self):
        """Translating an unpinned file would launder it into the corpus."""
        self.transcript.write_bytes(b'{"type":"item.completed","item":{}}\n')
        with self.assertRaises(RuntimeError) as ctx:
            transcript_canon.canonicalize_dir(self.dir)
        self.assertIn("pin mismatch", str(ctx.exception))


class SessionAWiringTest(unittest.TestCase):
    """session_a re-pins onto the canonical bytes -- the pin ARMS consume."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self.tmp.name)
        self.raw = (FIXTURES / "codex_stream_0148_shape.jsonl").read_bytes()
        self.transcript = self.dir / "session_transcript.jsonl"
        self.transcript.write_bytes(self.raw)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_meta_fields_point_at_the_canonical_transcript(self):
        from brainmark import session_a

        fields = session_a.canonicalize_codex_transcript(
            self.dir, self.transcript, self.dir / "worktree", "ISSUE TEXT")

        self.assertEqual(fields["transcript_source"],
                         transcript_canon.TRANSLATED_SOURCE)
        canon = pathlib.Path(fields["transcript_path"])
        self.assertEqual(canon.name, transcript_canon.CANONICAL_NAME)
        self.assertEqual(fields["transcript_sha256"], _harness.sha256_file(canon))

        # The canonical bytes are what a memory arm would actually ingest...
        self.assertGreater(len(transcript_to_messages(canon.read_bytes())), 0)
        # ...and the raw codex stream is still on disk, hashed, for audit.
        prov = fields["transcript_canonicalized"]
        self.assertEqual(pathlib.Path(prov["raw_codex_path"]).read_bytes(), self.raw)
        self.assertEqual(prov["raw_codex_sha256"], _harness.sha256_bytes(self.raw))
        self.assertTrue(prov["a_prompt_prepended"])


if __name__ == "__main__":
    unittest.main()
