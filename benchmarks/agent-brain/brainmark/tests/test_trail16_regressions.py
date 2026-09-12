"""Behavioral regressions from Trail 16 review."""
import json
import pathlib
import sys
import tempfile
import unittest
from unittest import mock
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
from brainmark import _repo, mechmetrics, power_analysis, run_b
from brainmark.agents.claude_adapter import ClaudeAdapter

class ReviewRegressions(unittest.TestCase):
    def test_lock_timeout_preserves_other_owner(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            lock = root / '.locks/cache.wt.lock'
            lock.mkdir(parents=True)
            with mock.patch.object(_repo, 'LOCK_TIMEOUT_S', -1):
                with self.assertRaises(TimeoutError):
                    with _repo.cache_lock(root, root / 'cache'):
                        self.fail('entered without ownership')
            self.assertTrue(lock.is_dir())

    def test_failed_patch_collection_discards_stale_patch(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            (root / 'tools').mkdir()
            (root / 'tools/collect_patch.sh').write_text('exit 1\n')
            patch = root / 'patch.diff'
            patch.write_text('stale')
            with self.assertRaises(RuntimeError):
                _repo.collect_patch(root, root / 'cache', root, patch)
            self.assertFalse(patch.exists())

    def test_worktree_namespaced_by_output(self):
        with mock.patch.dict('os.environ', {run_b.WORKTREE_IN_CELL_ENV: '0'}):
            self.assertNotEqual(run_b.worktree_path(pathlib.Path('/a/p/full_brain'), 'p', 'full_brain', 0),
                                run_b.worktree_path(pathlib.Path('/b/p/full_brain'), 'p', 'full_brain', 0))

    def test_mde_custom_target(self):
        curve = power_analysis.mde_curve(0.5, [30], reduction=0.5)
        self.assertTrue(curve[0]['meets_target'])

    def test_claude_failed_exit_overrides_success_event(self):
        with tempfile.TemporaryDirectory() as raw:
            stream = pathlib.Path(raw) / 'stream.jsonl'
            stream.write_text(json.dumps({'type':'result', 'subtype':'success', 'total_cost_usd':1})+'\n')
            result, usage, _ = ClaudeAdapter({}).normalize_result(stream, 'stub', 124, 1)
            self.assertTrue(result['is_error'])
            self.assertFalse(usage['complete'])

class EvidenceRegressions(unittest.TestCase):
    def test_failed_and_incomplete_codex_metrics_are_errors(self):
        for event in ({"type": "turn.failed"}, {"type": "thread.started"}):
            metrics = mechmetrics.session_metrics([json.dumps(event)], backend="codex")
            self.assertTrue(metrics["is_error"])

    def test_missing_validity_evidence_fails_gate(self):
        from brainmark.validity.analyze_validity import build_report
        self.assertTrue(build_report({}, {}, pathlib.Path("/missing"))["validity_gate_fires"])

    def test_candidate_traversal_rejected_before_deleting_output(self):
        from brainmark.mine_pairs import write_candidates
        with tempfile.TemporaryDirectory() as raw:
            output = pathlib.Path(raw) / "candidates"
            output.mkdir()
            existing = output / "keep.json"
            existing.write_text("keep")
            for key in ("../escape", "/absolute", "nested/file", "INDEX"):
                with self.assertRaises(ValueError):
                    write_candidates({"candidates": [{"pair_id": key}]}, output)
            self.assertEqual(existing.read_text(), "keep")

    def test_release_scrubs_nonstandard_text_extensions(self):
        from brainmark.release.make_release import build_release, scan_tree
        with tempfile.TemporaryDirectory() as raw:
            src, dst = pathlib.Path(raw)/"src", pathlib.Path(raw)/"dst"
            src.mkdir()
            for name in (".gitignore", "fixture.jsonl", "notes.custom"):
                (src/name).write_text("entire-brain /Users/suhaan/private")
            self.assertEqual(len(scan_tree(src)), 3)
            build_release(src, dst)
            self.assertFalse(scan_tree(dst))
            for file in dst.iterdir():
                self.assertNotIn("suhaan", file.read_text())

    def test_rai_free_text_is_anonymized(self):
        from brainmark.release.croissant_gen import build_responsible_ai
        result = build_responsible_ai({}, {"Uses": "entire-brain by suhaan", "Limitations": "entire-brain"})
        self.assertNotIn("entire-brain", json.dumps(result))

    def test_empty_raters_cannot_fall_back_to_single_review(self):
        from brainmark import seal
        with tempfile.TemporaryDirectory() as raw:
            path = pathlib.Path(raw)/"REVIEW.json"
            path.write_text(json.dumps({"raters": {}}))
            args = type("Args", (), {"review": str(path)})()
            with mock.patch.object(seal, "_promote_v2", side_effect=ValueError("dual")):
                with self.assertRaisesRegex(ValueError, "dual"):
                    seal.cmd_promote(args)

    def test_review_and_index_tampering_invalidates_seal(self):
        from test_seal_and_report import SealHarness
        from brainmark import seal
        for file in ("REVIEW.json", "candidates/INDEX.json"):
            with self.subTest(file=file), tempfile.TemporaryDirectory() as raw:
                harness = SealHarness(pathlib.Path(raw))
                harness.promote()
                self.assertTrue(seal.verify(harness.root)[0])
                (harness.root/file).write_text("{}")
                self.assertFalse(seal.verify(harness.root)[0])

    def test_placebo_cannot_be_bounded_to_empty(self):
        from brainmark.memsources.irrelevant import build
        from brainmark.memsources.base import MemorySourceError
        with self.assertRaises(MemorySourceError):
            build(query="q", max_bytes=1024, top_k=10, donor_pair_id="donor",
                  donor_packet_text=json.dumps({"results":[{"text":"long memory "*100}]}), target_bytes=200)

    def test_packet_metadata_does_not_disclose_donor(self):
        from brainmark.memsources.base import normalize_results
        result = normalize_results([{"text":"memory", "session_id":"donor", "created_at":"yesterday"}], 1)
        self.assertNotIn("session_id", result[0])
        self.assertNotIn("created_at", result[0])

    def test_legacy_competitor_does_not_mutate_parent_environment(self):
        import os
        from brainmark.memsources import cmm_source, graphify_source, _isolated
        before = dict(os.environ)
        with mock.patch.object(_isolated, "build", return_value=None) as call:
            for module in (cmm_source, graphify_source):
                module.build("q", 100, 1, b"transcript", "u", pins={"binary":"one", "bridge":"two"})
                module.build("q", 100, 1, b"transcript", "u")
                self.assertEqual(call.call_args.args[1], {})
        self.assertEqual(dict(os.environ), before)

    def test_public_sampling_seed_cannot_predict_sheet_id(self):
        from brainmark.validity.render_session import opaque_session_id, blind_text
        self.assertNotEqual(opaque_session_id(1,"p","cmm"), opaque_session_id(1,"p","cmm"))
        self.assertNotIn("cmm", blind_text("rg cmm /home/me/full_brain/db"))
        self.assertNotIn("full_brain", blind_text("rg cmm /home/me/full_brain/db"))

    def test_pr_visible_before_cutoff_is_rejected(self):
        from brainmark.mine_fresh import pr_to_instance
        result, reason = pr_to_instance({"created_at":"2025-01-01", "merged_at":"2026-02-01"}, [], {"training_cutoff":"2026-01-01"})
        self.assertIsNone(result)
        self.assertIn("cutoff", reason)
        result, reason = pr_to_instance({"created_at":"2026-02-01", "files_truncated":True}, [], {"training_cutoff":"2026-01-01"})
        self.assertEqual(reason, "files_truncated")

    def test_mem0_receives_supplied_timing_metadata(self):
        import asyncio
        from brainmark.memsources.mem0_source import Mem0Client
        client = Mem0Client()
        client._memory = mock.Mock()
        asyncio.run(client.add([{"role":"user", "content":"x"}], "u", observation_date="date", timestamp=123))
        self.assertEqual(client._memory.add.call_args.kwargs["metadata"], {"observation_date":"date", "timestamp":123})

class ExecutionRegressions(unittest.TestCase):
    def test_codex_grading_uses_completion_and_deduplicates_ids(self):
        from brainmark.grade import eligible_ids
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            for pair in ("p1", "p2"):
                cell = root/pair/"no_brain"
                cell.mkdir(parents=True)
                (cell/"meta.json").write_text(json.dumps({"instance_id":"same", "backend":"codex"}))
                (cell/"cc_out.json").write_text(json.dumps({"subtype":"success", "usage":{"output_tokens":1}}))
                (cell/"patch.diff").write_text("patch")
            self.assertEqual(eligible_ids(root, ["no_brain"]), ["same"])
            for cell in root.glob("*/no_brain"):
                (cell/"cc_out.json").write_text(json.dumps({"subtype":"error", "usage":{"output_tokens":1}}))
            self.assertEqual(eligible_ids(root, ["no_brain"]), [])

    def test_null_control_invokes_real_builder(self):
        from brainmark import memsources
        from brainmark.memsources.base import build_packet
        builder = mock.Mock(return_value=build_packet("mem0", "q", [{"text":"memory"}], 1024, prep={"seconds":3}))
        config = {"packet":{"max_bytes":1024,"top_k":1,"empty_sentinel":"none"},"competitors":{}}
        with mock.patch.dict(memsources.ARMS, {"mem0":builder}):
            packets = run_b.build_packets(config, {"pair_id":"p","a":{"instance_id":"a"}}, "q", b"transcript", ["mem0"], "date", force_empty_packet=True)
        builder.assert_called_once()
        self.assertEqual(json.loads(packets["mem0"].text)["results"], [])
        self.assertEqual(packets["mem0"].meta["prep"]["seconds"], 3)

    def test_shim_is_private_even_when_overwriting_public_file(self):
        from test_codex_provider_shim import AZURE
        from brainmark.agents.codex_provider_shim import install
        import os
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            real = root/"real"
            real.mkdir()
            (real/"codex").write_text("#!/bin/sh\nexit 0\n")
            (real/"codex").chmod(0o755)
            shim = root/"shim"
            shim.mkdir()
            (shim/"codex").write_text("old")
            (shim/"codex").chmod(0o755)
            install(shim, AZURE, {**os.environ,"PATH":str(real)})
            self.assertEqual((shim/"codex").stat().st_mode & 0o777, 0o700)

    @unittest.skipUnless(sys.platform != "win32", "POSIX process group test")
    def test_agent_timeout_kills_tool_descendants(self):
        import os, time
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            marker = root/"late-write"
            child = "import time,pathlib; time.sleep(0.6); pathlib.Path("+repr(str(marker))+").write_text('late')"
            program = "import subprocess,sys,time; subprocess.Popen([sys.executable,'-c',"+repr(child)+"]); time.sleep(10)"
            adapter = ClaudeAdapter({})
            with mock.patch.object(adapter, "prepare_env", return_value=(dict(os.environ), {})), mock.patch.object(adapter, "build_command", return_value=[sys.executable,"-c",program]):
                result = adapter.run(prompt="p", worktree=root, model="stub", timeout=0.2, out_dir=root/"out", env=dict(os.environ))
            self.assertEqual(result.returncode,124)
            time.sleep(0.7)
            self.assertFalse(marker.exists())

if __name__ == '__main__':
    unittest.main()
