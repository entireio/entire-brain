"""Real product orchestration; only model output is deterministic and local.

CI supplies binaries built from this checkout and the pinned Graph revision.
Set ENTIRE_BENCHMARK_REQUIRE_COMPONENTS=1 to make missing binaries a failure.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent / "brainmark" / "tests"))
from brainmark import _harness, run_b
from brainmark.memsources import full_brain
import test_run_b_dryrun as fixtures

FACT = "project.widget.rendering\tThe widget renderer uses the cobalt sentinel in STUB.md.\n"


class RealFullBrainIntegrationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.brain = Path(os.environ.get("ENTIRE_BENCHMARK_BRAIN_BINARY", "/missing/entire-brain")).resolve()
        cls.graph = Path(os.environ.get("ENTIRE_BENCHMARK_GRAPH_BINARY", "/missing/entire-graph")).resolve()
        if not all(p.is_file() for p in (cls.brain, cls.graph)):
            message = "build Brain and Graph; set ENTIRE_BENCHMARK_{BRAIN,GRAPH}_BINARY"
            if os.environ.get("ENTIRE_BENCHMARK_REQUIRE_COMPONENTS") == "1":
                raise RuntimeError(message)
            raise unittest.SkipTest(message)

    def setUp(self):
        self.fixture = fixtures.RunBDryRunTest()
        self.fixture.setUp()
        self.addCleanup(self.fixture.tearDown)
        self.root = Path(self.fixture.tmp.name)
        # Keep product state and provider discovery independent of the host.
        env = {k: v for k, v in os.environ.items() if not k.startswith(("ENTIRE_", "GIT_", "ANTHROPIC_", "OPENAI_", "AZURE_"))}
        for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"):
            env[key] = str(self.root / key)
            Path(env[key]).mkdir()
        env[run_b.WORKTREE_ROOT_ENV] = str(self.fixture.wt_root)
        self.env_patch = mock.patch.dict(os.environ, env, clear=True)
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        self.calls = self.root / "provider-inputs.jsonl"
        provider = self.fixture.stubdir / "claude"
        # Exercise Brain's actual Claude command/response adapter. The measured
        # agent uses the existing stream fixture; no product binary is replaced.
        original = provider.read_text()
        measured = self.fixture.stubdir / "measured-agent"
        measured.write_text(original)
        measured.chmod(0o755)
        provider.write_text("#!" + sys.executable + "\n" +
            "import json, os, pathlib, sys\n" + f"FACT = {FACT!r}\n" +
            "if '--output-format' in sys.argv and sys.argv[sys.argv.index('--output-format')+1] == 'json':\n" +
            "    text = sys.stdin.read()\n" +
            f"    with open({str(self.calls)!r}, 'a') as out: out.write(json.dumps(text)+'\\n')\n" +
            f"    print(json.dumps({{'type':'result','subtype':'success','is_error':False,'result':FACT,'usage':{{'input_tokens':10,'output_tokens':5}}}}))\n" +
            "else:\n" + f"    os.execv({str(measured)!r}, [{str(measured)!r}, *sys.argv[1:]])\n")
        provider.chmod(0o755)

    def test_brainmark_run_pair_materializes_and_delivers_real_facts(self):
        f = self.fixture
        f.config["brain"]["binary"] = str(self.brain)
        f.config["brain"]["distill"] = {"agent": "claude-code", "model": "fixture", "effort": "low"}
        f.pair["b"]["problem_statement"] = "widget renderer cobalt sentinel"
        summary = run_b.run_pair(f.config, f.pair, f.a_dir, f.out_root,
                                 tier="pilot", arms=["full_brain"])
        root = f.out_root / f.pair["pair_id"]
        packet = (root / "full_brain" / "packet.txt").read_text()
        delivered = json.loads(packet)
        self.assertGreater(delivered["_benchmark_delivery"]["delivered_result_count"], 0)
        self.assertTrue(any(FACT.split("\t", 1)[1].strip() in json.dumps(row)
                            for row in delivered["results"]), delivered)
        prepared_repo = root / "brain-repo"
        self.assertEqual(subprocess.check_output(
            ["git", "-C", str(prepared_repo), "log", "--first-parent", "--format=%H"], text=True).strip(),
            f.pair["b"]["base_commit"])
        self.assertIn(packet, (root / "full_brain" / "prompt.txt").read_text())
        self.assertTrue(self.calls.is_file(), "distillation provider was never called")
        self.assertIn("STUB.md", self.calls.read_text())
        prep = summary["packet_prep"]["full_brain"]
        manifest = json.loads((Path(prep["brain_dir"]) / "manifest.json").read_text())
        self.assertGreater(manifest["sources"]["facts"]["facts"], 0)
        self.assertGreater(manifest["sources"]["facts"]["chunks_distilled"], 0)
        self.assertEqual(manifest["sources"]["facts"].get("failed_chunks", 0), 0)
        self.assertTrue(manifest["sources"]["history"])
        self.assertEqual(prep["binary_sha256"], _harness.sha256_file(self.brain))

    def test_runner_prepares_sessions_facts_seed_and_real_semantics(self):
        run = _harness.harness()
        repo = self.fixture.repo_cache / "o_r"
        git = lambda *args: subprocess.check_output(["git", "-C", str(repo), *args], text=True, stderr=subprocess.PIPE).strip()
        git("remote", "add", "origin", "https://github.com/fixture/widget")
        (repo / "widget.go").write_text('package widget\nfunc RenderWidget(s string) string { return s }\n')
        git("add", "widget.go")
        git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "widget")
        main = git("rev-parse", "HEAD")
        # Real checkpoint branch consumed by refresh sessions, not a fabricated
        # exported Brain manifest or a replacement subprocess runner.
        git("checkout", "--orphan", "entire/checkpoints/v1")
        git("rm", "-rf", ".")
        cp = "aa/aaaaaaaaaa"
        d = repo / cp / "0"
        d.mkdir(parents=True)
        (d.parent / "metadata.json").write_text(json.dumps({"branch":"main","sessions":[{"metadata":f"/{cp}/0/metadata.json","transcript":f"/{cp}/0/full.jsonl"}]}))
        (d / "metadata.json").write_text(json.dumps({"checkpoint_id":"aaaaaaaaaaaa","session_id":"session-a","branch":"main","created_at":"2026-01-01T00:00:00Z"}))
        (d / "full.jsonl").write_bytes((self.fixture.a_dir / "session_transcript.jsonl").read_bytes())
        git("add", ".")
        git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "checkpoint")
        checkpoint = git("rev-parse", "HEAD")
        git("checkout", "main")
        run.ignore_benchmark_plugin(repo)
        wrapper = self.fixture.stubdir / "entire"
        wrapper.write_text('#!/bin/sh\ncase "$1" in\n graph) shift; exec '+shlex.quote(str(self.graph))+' "$@";;\n brain) shift; exec '+shlex.quote(str(self.brain))+' "$@";;\n *) exit 97;;\nesac\n')
        wrapper.chmod(0o755)
        tools = {"bin":self.fixture.stubdir,"brain":self.brain,"graph":self.graph,"entire":wrapper}
        task = {"id":"real-prep","repo_path":str(repo),"base_commit":main,"history_excerpt":False,
                "memory_bundle":{"role":"development","checkpoint_ref_commit":checkpoint,
                                 "cutoff_at":"2026-01-02T00:00:00Z","retrieval_branch":"main",
                                 "session_ids":["session-a"],"distill":{"agent":"claude-code","model":"fixture","effort":"low"}}}
        env, prep = run.prepare_brain(task, "full_brain", repo, self.root / "prep", tools, 0, use_cache=False)
        brain_dir = run.benchmark_brain_dir(repo, env, tools)
        manifest = json.loads((brain_dir / "manifest.json").read_text())
        for source in ("sessions", "facts", "seed", "semantic", "history"):
            self.assertTrue(manifest["sources"].get(source), source)
        self.assertGreater(manifest["sources"]["facts"]["facts"], 0)
        self.assertGreater(manifest["sources"]["facts"]["chunks_distilled"], 0)
        self.assertEqual(manifest["sources"]["facts"].get("failed_chunks", 0), 0)
        semantic = manifest["sources"]["semantic"]
        self.assertIn("RenderWidget", (brain_dir / semantic["snapshot_path"]).read_text())
        self.assertTrue(self.calls.is_file(), "distillation provider was never called")
        self.assertIn("STUB.md", self.calls.read_text())
        self.assertEqual(len(prep["commands"]), 5)
        self.assertTrue(all(step["returncode"] == 0 for step in prep["commands"]))
        result = subprocess.run([str(self.brain), "search", "cobalt sentinel", "--json"], cwd=repo, env=env, text=True, capture_output=True, check=True)
        self.assertIn(FACT.split("\t", 1)[1].strip(), result.stdout)


if __name__ == "__main__":
    unittest.main()
