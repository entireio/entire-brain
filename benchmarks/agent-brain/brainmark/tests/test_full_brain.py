"""full_brain manifest synthesis, dry-run distill, and the frozen retrieval.

NO PAID AGENT IS INVOKED. The distill step runs with `--dry-run`, which
internal/cli reports without calling a model and without writing facts. What
this proves is the part that can actually be wrong for free: that a hand-written
manifest + transcript is READ by the real binary as a session it would distill.

That contract comes from internal/cli/distill_cmd_test.go:52
(writeSingleSessionFixture) -- one main-branch session, transcript on disk,
manifest pointing at it -- and this test reproduces it from Python.

Skips (rather than fails) when the binary has not been built, so the offline
suite stays green on a fresh checkout. Build it with:

    go build -o benchmarks/agent-brain/brainmark/bin/entire-brain ./cmd/entire-brain
"""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness  # noqa: E402
from brainmark.memsources import full_brain  # noqa: E402

BINARY = _harness.BRAINMARK_DIR / "bin" / "entire-brain"

TRANSCRIPT = "".join(json.dumps(e) + "\n" for e in [
    {"type": "assistant", "message": {"content": [
        {"type": "text",
         "text": "The trailing-newline bug is in src/widget.js in renderWidget()."}]}},
    {"type": "assistant", "message": {"content": [
        {"type": "tool_use", "name": "Read", "input": {"file_path": "src/widget.js"}}]}},
    {"type": "assistant", "message": {"content": [
        {"type": "text",
         "text": "Decision: normalize in renderWidget rather than at each call site."}]}},
]).encode("utf-8")


def _git_repo(path: pathlib.Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"}
    subprocess.run(["git", "-C", str(path), "init", "-q", "-b", "main"],
                   check=True, capture_output=True, env=env)
    (path / "src").mkdir(exist_ok=True)
    (path / "src" / "widget.js").write_text(
        "export function renderWidget(s) { return s; }\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(path), "add", "-A"], check=True,
                   capture_output=True, env=env)
    subprocess.run(["git", "-C", str(path), "commit", "-qm", "init"], check=True,
                   capture_output=True, env=env)


@unittest.skipUnless(BINARY.is_file(), f"entire-brain not built at {BINARY}")
class FullBrainContractTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.tmp.name)
        self.repo = root / "repo"
        _git_repo(self.repo)
        self.data_root = root / "braindata"
        self.env = full_brain.brain_env(self.data_root)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_brain_dir_discovery_is_isolated_under_the_scratch_root(self):
        brain_dir = full_brain.discover_brain_dir(BINARY, self.repo, self.env)
        self.assertTrue(
            str(brain_dir).startswith(str(self.data_root.resolve())),
            f"brain dir {brain_dir} escaped the scratch root {self.data_root}",
        )

    def test_synthesized_manifest_matches_the_go_contract(self):
        brain_dir = full_brain.discover_brain_dir(BINARY, self.repo, self.env)
        info = full_brain.synthesize_session(
            brain_dir, "a-test-session", TRANSCRIPT, "2026-06-04T12:00:00Z")

        manifest = json.loads((brain_dir / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["schema_version"],
                         full_brain.BRAIN_MANIFEST_SCHEMA_VERSION)
        session = manifest["sources"]["sessions"]["sessions"][0]
        self.assertEqual(session["session_id"], "a-test-session")
        self.assertEqual(session["branch"], "main")
        self.assertEqual(session["transcript_path"], "sessions/main/a-test-session.jsonl")
        self.assertEqual(session["latest_checkpoint_id"], "cp1")

        transcript = brain_dir / "sessions" / "main" / "a-test-session.jsonl"
        self.assertTrue(transcript.is_file())
        self.assertEqual(transcript.read_bytes(), TRANSCRIPT)
        self.assertEqual(info["transcript_sha256"], _harness.sha256_bytes(TRANSCRIPT))

    def test_dry_run_distill_SEES_the_synthesized_session(self):
        """The load-bearing assertion: the real binary counts our hand-written
        session as work to do. If manifest synthesis were wrong, this is 0."""
        brain_dir = full_brain.discover_brain_dir(BINARY, self.repo, self.env)
        full_brain.synthesize_session(
            brain_dir, "a-test-session", TRANSCRIPT, "2026-06-04T12:00:00Z")

        proc = subprocess.run(
            [str(BINARY), "distill", str(self.repo), "--dry-run", "--json"],
            capture_output=True, text=True, env=self.env, timeout=300, check=False,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr[:800])
        report = json.loads(proc.stdout[proc.stdout.find("{"):])

        self.assertEqual(report.get("sessions"), 1,
                         f"binary did not see the synthesized session: {report}")
        self.assertEqual(report.get("sessions_to_distill"), 1, report)
        self.assertEqual(report.get("missing_transcripts", 0), 0,
                         f"transcript path in the manifest did not resolve: {report}")
        self.assertGreater(report.get("raw_bytes", 0), 0, report)

    def test_refresh_history_and_frozen_search_produce_a_bounded_packet(self):
        """The retrieval half of the arm, end to end, with no distilled facts.

        Facts require the paid step, so results may legitimately be empty here;
        what must hold is that `search --json` yields the {"results": [...]}
        contract the envelope bounder requires, and that the packet is pinned.
        """
        brain_dir = full_brain.discover_brain_dir(BINARY, self.repo, self.env)
        full_brain.synthesize_session(
            brain_dir, "a-test-session", TRANSCRIPT, "2026-06-04T12:00:00Z")

        subprocess.run([str(BINARY), "refresh", "history", str(self.repo)],
                       capture_output=True, text=True, env=self.env, timeout=600, check=False)

        proc = subprocess.run(
            [str(BINARY), "search", "trailing newline in renderWidget", "--json", "--limit", "10"],
            capture_output=True, text=True, env=self.env, cwd=str(self.repo),
            timeout=300, check=False,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr[:800])
        payload = json.loads(proc.stdout[proc.stdout.find("{"):])
        self.assertIn("results", payload,
                      "search --json must expose a `results` array for the packet bounder")
        self.assertIsInstance(payload["results"], list)

        from brainmark.memsources import base as msbase

        packet = msbase.build_packet(
            arm="full_brain", query="trailing newline in renderWidget",
            results=msbase.normalize_results(payload["results"], 10), max_bytes=8192,
        )
        self.assertEqual(packet.sha256, _harness.sha256_text(packet.text))
        self.assertIn("results", json.loads(packet.text))


class FullBrainUnitTest(unittest.TestCase):
    """Runs without the binary."""

    def test_brain_env_isolates_all_four_plugin_dirs(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw) / "scratch"
            env = full_brain.brain_env(root, base_env={"PATH": "/usr/bin"})
            for key in ("ENTIRE_PLUGIN_DATA_DIR", "ENTIRE_PLUGIN_CONFIG_DIR",
                        "ENTIRE_PLUGIN_STATE_DIR", "ENTIRE_PLUGIN_CACHE_DIR"):
                self.assertIn(key, env)
                self.assertTrue(env[key].startswith(str(root.resolve())))
                self.assertTrue(pathlib.Path(env[key]).is_dir())

    def test_missing_binary_fails_loudly(self):
        from brainmark.memsources.base import MemorySourceError

        with self.assertRaises(MemorySourceError):
            full_brain.build(
                query="q", max_bytes=1024, top_k=5,
                binary=pathlib.Path("/nonexistent/entire-brain"),
                repo=pathlib.Path("/tmp"), data_root=pathlib.Path("/tmp"),
                transcript_bytes=b"", session_id="s", created_at="2026-01-01T00:00:00Z",
                distill_pin={"agent": "claude-code", "model": "m", "effort": "medium"},
            )


if __name__ == "__main__":
    unittest.main()
