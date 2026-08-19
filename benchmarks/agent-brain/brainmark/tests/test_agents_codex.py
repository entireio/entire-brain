"""The codex/Azure adapter, and run_b end-to-end against a STUBBED `codex`.

No paid calls, no network. Mirrors tests/test_run_b_dryrun.py (the claude stub
proof) so both backends are exercised by the same shape of test: the only thing
substituted is the agent binary. Everything else -- Azure argv, packet build,
symmetry gate, worktree, patch collection, the SYNTHESIZED cc_out.json, the
backend-aware resume guard, mechmetrics, rep layout -- is the real code.
"""

from __future__ import annotations

import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, agents, run_b  # noqa: E402
from brainmark.agents import base as abase  # noqa: E402
from brainmark.agents.codex_adapter import CodexAdapter  # noqa: E402

ARMS = ["no_brain", "mem0"]

CODEX_STUB = textwrap.dedent("""\
    #!/usr/bin/env bash
    # Stubbed `codex`: emits a canned codex-exec JSON stream and makes one edit,
    # so collect_patch.sh has something real to collect.
    set -u
    printf '%s\\n' '{"type":"thread.started","thread_id":"th_stub01"}'
    printf '%s\\n' '{"type":"turn.started"}'
    printf '%s\\n' '{"item":{"id":"i1","item_type":"command_execution","command":["bash","-lc","rg -n stub ."],"status":"in_progress"},"type":"item.started"}'
    printf '%s\\n' '{"item":{"id":"i1","item_type":"command_execution","command":["bash","-lc","rg -n stub ."],"exit_code":0,"status":"completed"},"type":"item.completed"}'
    printf '%s\\n' '{"item":{"id":"i2","item_type":"command_execution","command":["bash","-lc","cat STUB.md"],"exit_code":0,"status":"completed"},"type":"item.completed"}'
    printf '%s\\n' '{"item":{"id":"i3","item_type":"file_change","changes":[{"path":"STUB_EDIT.txt","kind":"add"}],"status":"completed"},"type":"item.completed"}'
    echo "brainmark codex stub edit" >> STUB_EDIT.txt
    printf '%s\\n' '{"type":"turn.completed","usage":{"input_tokens":12000,"cached_input_tokens":9000,"output_tokens":700,"reasoning_output_tokens":250}}'
    exit 0
""")


def make_git_repo(path: pathlib.Path) -> str:
    path.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"}

    def run(*a):
        return subprocess.run(["git", "-C", str(path), *a], check=True,
                              capture_output=True, env=env)

    run("init", "-q", "-b", "main")
    (path / "STUB.md").write_text("hello\n", encoding="utf-8")
    run("add", "-A")
    run("commit", "-qm", "init")
    out = subprocess.run(["git", "-C", str(path), "rev-parse", "HEAD"],
                         capture_output=True, text=True, check=True)
    return out.stdout.strip()


# --------------------------------------------------------------------------
# adapter unit tests (no subprocess)
# --------------------------------------------------------------------------


class BackendsFileTest(unittest.TestCase):
    def test_backends_json_is_loadable_and_pinned(self):
        backends = agents.load_backends()
        self.assertEqual(backends["default_backend"], "codex")
        self.assertEqual(len(backends["_backends_sha256"]), 64)
        self.assertIn("codex", backends["backends"])
        self.assertIn("claude", backends["backends"])

    def test_placeholder_prices_are_labelled_as_such(self):
        """A placeholder rate card must never be quotable as a measured cost."""
        prices = agents.load_backends()["prices"]
        for model in ("gpt-5", "gpt-5.6-sol", "gpt-5.6-terra"):
            with self.subTest(model=model):
                self.assertEqual(prices[model]["status"], "UNVERIFIED_PLACEHOLDER")

    def test_model_roles_resolve(self):
        self.assertEqual(agents.resolve_model("codex", "primary"), "gpt-5.6-sol")
        self.assertEqual(agents.resolve_model("codex", "generality"), "gpt-5.6-terra")
        with self.assertRaises(agents.AgentBackendError):
            agents.resolve_model("codex", "nope")

    def test_unknown_backend_is_refused(self):
        with self.assertRaises(agents.AgentBackendError):
            agents.get_adapter("gemini")


class AzureWiringTest(unittest.TestCase):
    def setUp(self):
        self.spec = dict(agents.load_backends()["backends"]["codex"])
        self.adapter = CodexAdapter(self.spec, {})
        self.env = {
            "AZURE_AI_ENDPOINT": "https://example-resource.openai.azure.com/",
            "AZURE_AI_API_KEY": "sk-test-not-real",
            "AZURE_AI_API_VERSION": "2025-04-01-preview",
        }

    def test_endpoint_gets_the_openai_suffix_once(self):
        azure = self.adapter.azure_settings(self.env)
        self.assertEqual(azure["base_url"],
                         "https://example-resource.openai.azure.com/openai")
        already = dict(self.env,
                       AZURE_AI_ENDPOINT="https://x.openai.azure.com/openai")
        self.assertEqual(self.adapter.azure_settings(already)["base_url"],
                         "https://x.openai.azure.com/openai")

    def test_missing_credentials_fail_loudly(self):
        for drop in ("AZURE_AI_ENDPOINT", "AZURE_AI_API_KEY"):
            with self.subTest(drop=drop):
                partial = {k: v for k, v in self.env.items() if k != drop}
                spec = dict(self.spec)
                spec["azure"] = dict(spec["azure"], env_file="/nonexistent/.azure_env")
                with self.assertRaises(agents.AgentBackendError):
                    CodexAdapter(spec, {}).azure_settings(partial)

    def test_env_file_is_parsed_and_process_env_wins(self):
        with tempfile.TemporaryDirectory() as tmp:
            env_file = pathlib.Path(tmp) / ".azure_env"
            env_file.write_text(
                '# comment\nexport AZURE_AI_ENDPOINT="https://from-file.openai.azure.com"\n'
                "AZURE_AI_API_KEY=file-key\nAZURE_AI_API_VERSION=2024-05-01-preview\n",
                encoding="utf-8")
            spec = dict(self.spec)
            spec["azure"] = dict(spec["azure"], env_file=str(env_file))
            adapter = CodexAdapter(spec, {})

            from_file = adapter.azure_settings({})
            self.assertEqual(from_file["base_url"],
                             "https://from-file.openai.azure.com/openai")
            self.assertEqual(from_file["api_key"], "file-key")

            overridden = adapter.azure_settings(
                {"AZURE_AI_ENDPOINT": "https://from-env.openai.azure.com"})
            self.assertEqual(overridden["base_url"],
                             "https://from-env.openai.azure.com/openai")
            self.assertEqual(overridden["api_key"], "file-key")

    def test_command_carries_the_azure_provider_and_no_mcp(self):
        cmd = self.adapter.build_command(
            "PROMPT", pathlib.Path("/w"), "gpt-5.6-sol", env=self.env)
        self.assertEqual(cmd[:3], ["codex", "exec", "--ephemeral"])
        self.assertEqual(cmd[-1], "PROMPT", "the prompt must be last")
        joined = " ".join(cmd)
        self.assertIn('model_provider="azure"', joined)
        self.assertIn(
            'model_providers.azure.base_url="https://example-resource.openai.azure.com/openai"',
            joined)
        self.assertIn('model_providers.azure.env_key="AZURE_AI_API_KEY"', joined)
        self.assertIn('{"api-version"="2025-04-01-preview"}', joined)
        self.assertIn("--ignore-user-config", cmd)
        self.assertIn("--json", cmd)
        self.assertIn("gpt-5.6-sol", cmd)
        # BrainMark delivers memory as a prompt packet; NO arm gets a live MCP.
        self.assertNotIn("mcp_servers", joined)
        # The key is never on the command line -- codex reads it from env_key.
        self.assertNotIn("sk-test-not-real", joined)

    def test_prepare_env_injects_the_key_but_provenance_redacts_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            env, prov = self.adapter.prepare_env(dict(self.env), pathlib.Path(tmp))
        self.assertEqual(env["AZURE_AI_API_KEY"], "sk-test-not-real")
        self.assertTrue(env["CODEX_HOME"].endswith("codex-home"))
        self.assertTrue(prov["AZURE_AI_API_KEY"].startswith("sha256:"))
        self.assertNotIn("sk-test-not-real", json.dumps(prov))


class PricingTest(unittest.TestCase):
    """The two provider conventions must be priced differently. This is the
    honest-mapping test: applying claude's formula to a codex vector
    double-counts every cached token."""

    PRICES = {
        "m": {"input_per_million": 1.0, "cache_read_per_million": 0.1,
              "cache_creation_per_million": 1.25, "output_per_million": 10.0,
              "status": "test"},
    }

    def test_codex_treats_cached_input_as_a_subset(self):
        tokens = {"input_tokens": 1_000_000, "cache_read_tokens": 900_000,
                  "output_tokens": 100_000}
        out = abase.compute_usd("codex", "m", tokens, self.PRICES)
        # (1_000_000-900_000)*1.0 + 900_000*0.1 + 100_000*10.0, per million
        self.assertAlmostEqual(out["usd"], 0.1 + 0.09 + 1.0, places=9)
        self.assertEqual(out["accounting_basis"],
                         "openai_cached_input_is_subset_of_input")
        self.assertTrue(out["estimated"])

    def test_claude_treats_the_counters_as_disjoint(self):
        tokens = {"input_tokens": 1_000_000, "cache_read_tokens": 900_000,
                  "cache_creation_tokens": 200_000, "output_tokens": 100_000}
        out = abase.compute_usd("claude", "m", tokens, self.PRICES)
        self.assertAlmostEqual(out["usd"], 1.0 + 0.09 + 0.25 + 1.0, places=9)
        self.assertEqual(out["accounting_basis"],
                         "anthropic_disjoint_input_cacheread_cachewrite")

    def test_missing_price_row_yields_none_not_zero(self):
        out = abase.compute_usd("codex", "unknown-model", {"input_tokens": 1}, self.PRICES)
        self.assertIsNone(out["usd"])
        self.assertEqual(out["price_status"], "no_price_row")


class NormalizeResultTest(unittest.TestCase):
    def test_synthesized_cc_out_mirrors_the_claude_result_shape(self):
        adapter = CodexAdapter(agents.load_backends()["backends"]["codex"],
                               agents.load_backends()["prices"])
        fixtures = pathlib.Path(__file__).resolve().parent / "fixtures"
        cc_out, usage, session_id = adapter.normalize_result(
            fixtures / "codex_stream_locate_heavy.jsonl", "gpt-5.6-sol", 0, 12.5)
        self.assertEqual(session_id, "th_9f21c0")
        self.assertEqual(cc_out["type"], "result")
        self.assertEqual(cc_out["subtype"], "success")
        self.assertEqual(cc_out["duration_ms"], 12500)
        self.assertEqual(cc_out["_synthesized_by"], "brainmark.agents.codex_adapter")
        self.assertTrue(cc_out["_usd_is_estimated"])
        self.assertEqual(cc_out["_usd_price_status"], "UNVERIFIED_PLACEHOLDER")
        self.assertEqual(usage["tokens"]["input_tokens"], 42000)
        self.assertEqual(usage["tokens"]["output_tokens"], 900)
        self.assertEqual(usage["tokens"]["total_tokens"], 42900)
        self.assertIsNone(usage["tokens"]["cache_creation_tokens"],
                          "codex exposes no cache-write counter; None != 0")
        self.assertGreater(usage["usd"], 0)
        self.assertTrue(usage["estimated"])

    def test_a_failed_turn_is_not_success(self):
        adapter = CodexAdapter(agents.load_backends()["backends"]["codex"], {})
        with tempfile.TemporaryDirectory() as tmp:
            stream = pathlib.Path(tmp) / "stream.jsonl"
            stream.write_text(
                '{"type":"thread.started","thread_id":"t"}\n'
                '{"type":"turn.failed","error":{"message":"boom"}}\n', encoding="utf-8")
            cc_out, usage, _ = adapter.normalize_result(stream, "gpt-5.6-sol", 1, 1.0)
        self.assertEqual(cc_out["subtype"], "error")
        self.assertTrue(cc_out["is_error"])
        self.assertFalse(usage["complete"])

    def test_legacy_session_id(self):
        adapter = CodexAdapter(agents.load_backends()["backends"]["codex"], {})
        fixtures = pathlib.Path(__file__).resolve().parent / "fixtures"
        _cc, _usage, session_id = adapter.normalize_result(
            fixtures / "codex_stream_legacy_exec.jsonl", "gpt-5.6-sol", 0, 1.0)
        self.assertEqual(session_id, "sess-legacy-1")


class ResumeGuardTest(unittest.TestCase):
    """The guard must be backend-aware or codex cells re-run forever."""

    def _cell(self, tmp: pathlib.Path, cc_out: dict, patch: str = "diff --git a b\n"):
        cell = tmp / "cell"
        cell.mkdir(parents=True, exist_ok=True)
        (cell / "cc_out.json").write_text(json.dumps(cc_out), encoding="utf-8")
        (cell / "patch.diff").write_text(patch, encoding="utf-8")
        return cell

    def test_codex_cell_with_no_cost_is_still_complete(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = self._cell(pathlib.Path(tmp), {
                "subtype": "success", "total_cost_usd": None,
                "usage": {"output_tokens": 700},
                "_synthesized_by": "brainmark.agents.codex_adapter"})
            self.assertTrue(run_b.cell_is_complete(cell, "codex"))
            self.assertTrue(run_b.cell_is_complete(cell), "backend inferred from cc_out")
            # ...and the claude guard would have wrongly re-run it:
            self.assertFalse(run_b.cell_is_complete(cell, "claude"))

    def test_codex_cell_with_no_output_tokens_is_incomplete(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = self._cell(pathlib.Path(tmp), {
                "subtype": "success", "usage": {"output_tokens": 0},
                "_synthesized_by": "brainmark.agents.codex_adapter"})
            self.assertFalse(run_b.cell_is_complete(cell, "codex"))

    def test_empty_patch_is_incomplete_on_both_backends(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = self._cell(pathlib.Path(tmp), {
                "subtype": "success", "total_cost_usd": 0.5,
                "usage": {"output_tokens": 9}}, patch="")
            self.assertFalse(run_b.cell_is_complete(cell, "claude"))
            self.assertFalse(run_b.cell_is_complete(cell, "codex"))

    def test_claude_guard_is_unchanged(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = self._cell(pathlib.Path(tmp), {"total_cost_usd": 0.01})
            self.assertTrue(run_b.cell_is_complete(cell, "claude"))
            zero = self._cell(pathlib.Path(tmp), {"total_cost_usd": 0})
            self.assertFalse(run_b.cell_is_complete(zero, "claude"))


class RepLayoutTest(unittest.TestCase):
    def test_no_rep_keeps_the_legacy_root(self):
        root = pathlib.Path("/r")
        self.assertEqual(run_b.rep_root(root, None), root)

    def test_rep_is_a_directory_above_the_pair(self):
        """Each rep root must itself be a valid results root for report.py."""
        self.assertEqual(run_b.rep_root(pathlib.Path("/r"), 2), pathlib.Path("/r/rep2"))

    def test_rep_seed_is_derived_from_the_master_seed(self):
        config = {"seeds": {"master": 20260815}}
        self.assertEqual(run_b.rep_seed(config, 0), 20260815000)
        self.assertEqual(run_b.rep_seed(config, 3), 20260815003)
        self.assertEqual(run_b.rep_seed(config, None), 20260815000)


# --------------------------------------------------------------------------
# run_b end-to-end against the stub
# --------------------------------------------------------------------------


class CodexRunBStubTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        tmp = pathlib.Path(self.tmp.name)

        self.graphmark = tmp / "graphmark"
        (self.graphmark / "tools").mkdir(parents=True)
        real_graphmark = pathlib.Path(_harness.load_config()["graphmark_root"])
        shutil.copyfile(real_graphmark / "tools" / "collect_patch.sh",
                        self.graphmark / "tools" / "collect_patch.sh")
        shutil.copytree(real_graphmark / "tools" / "netjail",
                        self.graphmark / "tools" / "netjail", symlinks=True)

        self.repo_cache = tmp / "repo-cache"
        self.repo_cache.mkdir()
        self.commit = make_git_repo(self.repo_cache / "o_r")

        self.stubdir = tmp / "stubbin"
        self.stubdir.mkdir()
        stub = self.stubdir / "codex"
        stub.write_text(CODEX_STUB, encoding="utf-8")
        stub.chmod(0o755)
        self._old_path = os.environ["PATH"]
        os.environ["PATH"] = f"{self.stubdir}{os.pathsep}{self._old_path}"

        self._old_env = {k: os.environ.get(k) for k in
                         ("AZURE_AI_ENDPOINT", "AZURE_AI_API_KEY", "AZURE_AI_API_VERSION")}
        os.environ["AZURE_AI_ENDPOINT"] = "https://stub.openai.azure.com"
        os.environ["AZURE_AI_API_KEY"] = "stub-key-not-real"
        os.environ["AZURE_AI_API_VERSION"] = "2024-05-01-preview"

        self.config = json.loads((_harness.BRAINMARK_DIR / "config.json").read_text())
        self.config.update({
            "graphmark_root": str(self.graphmark),
            "repo_cache": str(self.repo_cache),
            "arms": ARMS,
            "_config_sha256": "test",
        })
        self.config["agent"]["timeout_sec"] = 120
        self.config["agent"]["backend"] = "codex"

        self.pair = {
            "pair_id": "a1__then__b1", "repo": "o/r",
            "a": {"instance_id": "a1", "base_commit": self.commit,
                  "problem_statement": "earlier issue"},
            "b": {"instance_id": "b1", "base_commit": self.commit,
                  "problem_statement": "later related issue"},
        }

        self.a_dir = tmp / "A"
        self.a_dir.mkdir()
        transcript = self.a_dir / "session_transcript.jsonl"
        transcript.write_text("".join(json.dumps(e) + "\n" for e in [
            {"type": "assistant", "message": {"content": [
                {"type": "text", "text": "The widget renderer lives in STUB.md."}]}},
        ]), encoding="utf-8")
        (self.a_dir / "meta.json").write_text(_harness.pretty_json({
            "pair_id": "a1__then__b1",
            "transcript_path": str(transcript),
            "transcript_sha256": _harness.sha256_file(transcript),
            "created_at": "2026-01-01T00:00:00Z",
        }), encoding="utf-8")

        self.out_root = tmp / "results"

        from brainmark import memsources
        from brainmark.memsources import base as msbase

        self._real_mem0 = memsources.ARMS["mem0"]
        memsources.ARMS["mem0"] = lambda query, max_bytes, top_k, **kw: msbase.build_packet(
            arm="mem0", query=query,
            results=msbase.normalize_results(
                [{"memory": "prior session read STUB.md", "score": 1.0, "id": "m1"}], top_k),
            max_bytes=max_bytes, prep={"seconds": 0.01},
        )

    def tearDown(self) -> None:
        from brainmark import memsources

        memsources.ARMS["mem0"] = self._real_mem0
        os.environ["PATH"] = self._old_path
        for key, value in self._old_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self.tmp.cleanup()

    def test_full_results_layout_under_a_rep_dir(self):
        summary = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                                 tier="pilot", arms=ARMS, rep=0)
        self.assertEqual(summary["backend"], "codex")
        self.assertEqual(summary["rep"], 0)

        pair_root = self.out_root / "rep0" / self.pair["pair_id"]
        self.assertTrue((pair_root / "pair_summary.json").is_file())
        for arm in ARMS:
            cell = pair_root / arm
            with self.subTest(arm=arm):
                for name in ("cc_out.json", "stream.jsonl", "patch.diff", "packet.txt",
                             "packet.sha256", "prompt_sym.sha256", "prompt.txt",
                             "meta.json", "cc_err.log"):
                    self.assertTrue((cell / name).is_file(), f"{arm}: missing {name}")

                self.assertEqual(
                    (cell / "packet.sha256").read_text(encoding="utf-8").strip(),
                    _harness.sha256_file(cell / "packet.txt"))

                cc_out = json.loads((cell / "cc_out.json").read_text(encoding="utf-8"))
                self.assertEqual(cc_out["subtype"], "success")
                self.assertEqual(cc_out["session_id"], "th_stub01")
                self.assertTrue(cc_out["_usd_is_estimated"])
                self.assertGreater(cc_out["total_cost_usd"], 0)

                meta = json.loads((cell / "meta.json").read_text(encoding="utf-8"))
                self.assertEqual(meta["backend"], "codex")
                self.assertEqual(meta["model"], "gpt-5.6-sol")
                self.assertEqual(meta["rep"], 0)
                self.assertEqual(meta["rep_seed"], run_b.rep_seed(self.config, 0))
                self.assertEqual(meta["mechmetrics"]["backend"], "codex")
                # rg + cat before the file_change -> 2 locate calls pre-edit.
                self.assertEqual(meta["mechmetrics"]["locate_calls_pre_edit"], 2)
                self.assertFalse(meta["mechmetrics"]["no_edit"])
                self.assertGreater(meta["patch_bytes"], 0,
                                   "collect_patch produced nothing")
                self.assertNotIn("stub-key-not-real", json.dumps(meta),
                                 "the API key must never reach meta.json")
                self.assertIn("<PROMPT>", meta["cmd"])

    def test_all_arms_share_one_symmetry_sha(self):
        summary = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                                 tier="pilot", arms=ARMS, rep=0)
        shas = {
            arm: (self.out_root / "rep0" / self.pair["pair_id"] / arm /
                  "prompt_sym.sha256").read_text(encoding="utf-8").strip()
            for arm in ARMS
        }
        self.assertEqual(len(set(shas.values())), 1, shas)
        self.assertEqual(set(shas.values()), {summary["prompt_sym_sha256"]})

    def test_reps_are_separate_trees_and_resume_independently(self):
        for rep in (0, 1):
            run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                           tier="pilot", arms=ARMS, rep=rep)
        for rep in (0, 1):
            self.assertTrue(
                (self.out_root / f"rep{rep}" / self.pair["pair_id"] / "no_brain"
                 / "meta.json").is_file())

        again = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                               tier="pilot", arms=ARMS, rep=1, resume=True)
        for arm in ARMS:
            self.assertEqual(again["cells"][arm].get("skipped"), "already complete")

        fresh = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                               tier="pilot", arms=ARMS, rep=2, resume=True)
        for arm in ARMS:
            self.assertNotIn("skipped", fresh["cells"][arm])

    def test_explicit_model_override(self):
        summary = run_b.run_pair(self.config, self.pair, self.a_dir, self.out_root,
                                 tier="pilot", arms=["no_brain"], rep=0,
                                 model="gpt-5.6-terra")
        self.assertEqual(summary["model"], "gpt-5.6-terra")
        meta = json.loads((self.out_root / "rep0" / self.pair["pair_id"] / "no_brain"
                           / "meta.json").read_text(encoding="utf-8"))
        self.assertIn("gpt-5.6-terra", meta["cmd"])

    def test_a_per_backend_tier_key_wins_over_backends_json(self):
        config = dict(self.config)
        config["agent"] = dict(self.config["agent"])
        config["agent"]["session_b"] = dict(self.config["agent"]["session_b"],
                                            codex_tier_pilot="gpt-5")
        summary = run_b.run_pair(config, self.pair, self.a_dir, self.out_root,
                                 tier="pilot", arms=["no_brain"], rep=0)
        self.assertEqual(summary["model"], "gpt-5")

    def test_a_claude_tier_value_is_never_handed_to_codex(self):
        """config.json ships claude model names under the plain tier keys."""
        self.assertEqual(self.config["agent"]["session_b"]["tier_pilot"],
                         "claude-sonnet-4-6")
        backends = agents.load_backends()
        self.assertEqual(
            run_b.resolve_model(self.config, "codex", "pilot", backends),
            "gpt-5.6-sol")

    def test_claude_config_still_selects_the_claude_backend(self):
        """A config that names the claude CLI must not silently become codex."""
        config = dict(self.config)
        config["agent"] = dict(self.config["agent"])
        config["agent"].pop("backend")
        _adapter, name, _backends = run_b.resolve_backend(config)
        self.assertEqual(name, "claude")


if __name__ == "__main__":
    unittest.main()
