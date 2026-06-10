import argparse
import importlib.util
import json
import os
import pathlib
import sys
import tempfile
import unittest


RUN_PATH = pathlib.Path(__file__).with_name("run.py")
SPEC = importlib.util.spec_from_file_location("agent_brain_run", RUN_PATH)
run = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = run
SPEC.loader.exec_module(run)

AUDIT_PATH = pathlib.Path(__file__).with_name("audit_codex.py")
AUDIT_SPEC = importlib.util.spec_from_file_location("agent_brain_audit", AUDIT_PATH)
audit_codex = importlib.util.module_from_spec(AUDIT_SPEC)
assert AUDIT_SPEC.loader is not None
sys.modules[AUDIT_SPEC.name] = audit_codex
AUDIT_SPEC.loader.exec_module(audit_codex)


class RunnerAndConditionTests(unittest.TestCase):
    def test_parse_runner_spec_accepts_codex_claude_and_rejects_gemini(self):
        self.assertEqual(run.parse_runner_spec("codex:gpt-5.4-mini:xhigh").agent, "codex")
        self.assertEqual(run.parse_runner_spec("claude:sonnet:max").agent, "claude")
        with self.assertRaises(ValueError):
            run.parse_runner_spec("gemini:gemini-3-flash-preview:max")

    def test_condition_prep_kind_separates_delivery_from_brain_artifacts(self):
        self.assertEqual(run.condition_prep_kind("full_cli_original"), "full_brain")
        self.assertEqual(run.condition_prep_kind("full_cli_compact"), "full_brain")
        self.assertEqual(run.condition_prep_kind("mcp_history"), "full_brain")
        self.assertEqual(run.condition_prep_kind("mcp_semantic"), "semantic_brain")
        self.assertTrue(run.condition_writes_history_excerpt("full_cli_original"))
        self.assertFalse(run.condition_writes_history_excerpt("full_cli_compact"))
        self.assertFalse(run.condition_writes_history_excerpt("mcp_history"))
        self.assertFalse(run.condition_copies_entire_history("no_brain"))
        self.assertTrue(run.condition_copies_entire_history("full_cli_compact"))

    def test_manifest_source_counts_extracts_sessions_and_history_records(self):
        counts = run.manifest_source_counts(
            {
                "sources": {
                    "sessions": {"sessions": [{"id": "s1"}, {"id": "s2"}]},
                    "history": {"records": 7},
                }
            }
        )
        self.assertEqual(counts, {"sessions": 2, "history_records": 7})

    def test_assert_brain_state_ready_requires_full_history_for_full_brain(self):
        task = {"prepare_semantic": True}
        ready = {
            "manifest": {
                "has_semantic": True,
                "session_count": 2,
                "history_records": 5,
            }
        }
        run.assert_brain_state_ready(task, "full_cli_compact", ready)

        missing_history = {"manifest": {"has_semantic": True, "session_count": 2, "history_records": 0}}
        with self.assertRaisesRegex(RuntimeError, "no history index records"):
            run.assert_brain_state_ready(task, "full_cli_compact", missing_history)

        missing_semantic = {"manifest": {"has_semantic": False, "session_count": 2, "history_records": 5}}
        with self.assertRaisesRegex(RuntimeError, "semantic source"):
            run.assert_brain_state_ready(task, "full_cli_compact", missing_semantic)

    def test_mcp_history_audit_matches_compact_prompt_history_tool_requirement(self):
        # Compact-delivery models are told to call brain_brief ONCE and NOT brain_search;
        # the audit must not then fail them for skipping brain_search.
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="o", agent="claude", model="opus")), ("brain_brief",))
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="g", agent="codex", model="gpt-5.5")), ("brain_brief",))
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="s", agent="claude", model="sonnet")), ("brain_brief", "brain_search"))
        brief_only = {"mcp": {"enabled": True}, "activity": {"mcp_tool_calls": 1, "mcp_tool_names": ["mcp__entire_brain__brain_brief"]}}
        # Opus (compact): brief-only is a clean pass.
        opus_audit = run.mcp_condition_audit("mcp_history", brief_only, run.RunnerSpec(id="o", agent="claude", model="opus"))
        self.assertTrue(opus_audit["ok"], opus_audit)
        # Sonnet (non-compact): brief-only must still be flagged for missing brain_search.
        sonnet_audit = run.mcp_condition_audit("mcp_history", brief_only, run.RunnerSpec(id="s", agent="claude", model="sonnet"))
        self.assertFalse(sonnet_audit["ok"])
        self.assertIn("brain_search", [f.get("tool") for f in sonnet_audit["findings"]])

    def test_validate_fails_tasks_with_no_validation_commands(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run.validate({"id": "t", "validation": []}, pathlib.Path(tmp), os.environ.copy())
        self.assertFalse(result["ok"])
        self.assertEqual(result["results"], [])
        self.assertIn("no validation commands", result["error"])

    def test_mcp_configs_include_local_brain_server_and_repo_env(self):
        env = {
            "ENTIRE_REPO_ROOT": "/repo",
            "ENTIRE_PLUGIN_CONFIG_DIR": "/plugin/config",
            "ENTIRE_PLUGIN_DATA_DIR": "/plugin/data",
            "ENTIRE_PLUGIN_STATE_DIR": "/plugin/state",
            "ENTIRE_PLUGIN_CACHE_DIR": "/plugin/cache",
            "ENTIRE_BRAIN_MCP_DEBUG_LOG": "/tmp/mcp.log",
        }
        tools = {"brain": pathlib.Path("/tmp/entire-brain")}
        claude_config = run.claude_mcp_config(tools, env)
        self.assertIn('"command":"/tmp/entire-brain"', claude_config)
        self.assertIn('"args":["mcp"]', claude_config)
        self.assertIn('"ENTIRE_REPO_ROOT":"/repo"', claude_config)

        codex_args = run.codex_mcp_config_args(tools, env)
        joined = "\n".join(codex_args)
        self.assertIn('mcp_servers.entire_brain.command="/tmp/entire-brain"', joined)
        self.assertIn('mcp_servers.entire_brain.args=["mcp"]', joined)
        self.assertIn("mcp_servers.entire_brain.required=true", joined)
        self.assertIn("mcp_servers.entire_brain.enabled_tools=", joined)
        self.assertIn('mcp_servers.entire_brain.default_tools_approval_mode="approve"', joined)
        self.assertIn("mcp_servers.entire_brain.startup_timeout_sec=120", joined)
        self.assertIn("mcp_servers.entire_brain.tool_timeout_sec=120", joined)
        self.assertIn('mcp_servers.entire_brain.env.ENTIRE_REPO_ROOT="/repo"', joined)
        self.assertIn('mcp_servers.entire_brain.env.ENTIRE_BRAIN_MCP_DEBUG_LOG="/tmp/mcp.log"', joined)

    def test_mcp_history_prompt_requires_mcp_and_avoids_excerpt_shortcut(self):
        task = {
            "id": "task",
            "prompt": "Fix the regression.",
            "brain_queries": ["history term"],
            "expected_files": ["pkg/file.ts"],
            "validation": ["npm test"],
        }
        prompt = run.prompt_for(task, "mcp_history")
        self.assertIn("brain_brief", prompt)
        self.assertIn("brain_search", prompt)
        self.assertIn("mcp__entire_brain__brain_brief", prompt)
        self.assertIn("before any shell search or file reads", prompt)
        self.assertIn("run exactly one `mcp__entire_brain__brain_search`", prompt)
        self.assertIn("apply the fix there before any additional MCP calls", prompt)
        self.assertIn("MCP_TOOLS_MISSING", prompt)
        self.assertIn("Do not run the `entire brain` CLI", prompt)
        self.assertIn("do not read `.benchmark/brain-history-excerpt.md`", prompt)

    def test_mcp_history_disciplined_delivery_for_gpt5x(self):
        # gpt-5.5 under-contexts on MCP (brief names the file but not the invariant, and the old
        # brief-only prompt banned brain_search). It now gets the "disciplined MCP" delivery:
        # brief once + ONE targeted brain_search for the invariant + hard stop. Validated to lift
        # gpt-5.5 mcp_history pass-rate 88%->100% with no score regression.
        task = {
            "id": "task",
            "prompt": "Fix the regression.",
            "brain_queries": ["history term"],
            "expected_files": ["pkg/file.ts"],
            "validation": ["npm test"],
        }
        prompt = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.5:medium"))
        self.assertIn("Step 1", prompt)
        self.assertIn("brain_brief", prompt)
        self.assertIn("Step 2", prompt)
        self.assertIn("brain_search", prompt)  # the invariant lookup, restored (not banned)
        self.assertIn("Hard stop", prompt)
        self.assertIn("MCP_TOOLS_MISSING", prompt)
        self.assertNotIn("do NOT need a separate `brain_search` call", prompt)  # old brief-only is gone
        # A model NOT in the disciplined/compact set keeps the generic history delivery.
        guided = run.prompt_for(task, "mcp_history", run.parse_runner_spec("claude:sonnet:medium"))
        self.assertIn("run exactly one `mcp__entire_brain__brain_search`", guided)
        self.assertNotIn("Hard stop", guided)

    def test_disciplined_mcp_is_effort_aware_for_mini(self):
        # gpt-5.4-mini gets the generic delivery at low/medium (it wins there) but the disciplined
        # hard-stop at high/xhigh, where it spirals (token bloat). Validated: -32% tokens pooled,
        # no validation regression.
        task = {"id": "t", "prompt": "Fix.", "brain_queries": ["q"], "expected_files": ["f.go"], "validation": ["go test ./..."]}
        low = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.4-mini:low"))
        high = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.4-mini:high"))
        xhigh = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.4-mini:xhigh"))
        self.assertNotIn("Hard stop", low)   # generic at low effort
        self.assertIn("Hard stop", high)     # disciplined at high
        self.assertIn("Hard stop", xhigh)    # disciplined at xhigh
        self.assertTrue(run.wants_disciplined_mcp(run.parse_runner_spec("codex:gpt-5.4-mini:high")))
        self.assertFalse(run.wants_disciplined_mcp(run.parse_runner_spec("codex:gpt-5.4-mini:medium")))

    def test_full_brain_prompt_uses_query_terms_in_initial_brief(self):
        task = {
            "id": "task",
            "prompt": "Fix the regression.",
            "brain_queries": ["ExactSymbol", "important invariant"],
            "expected_files": ["pkg/file.ts"],
            "validation": ["npm test"],
            "prepare_semantic": True,
        }
        prompt = run.prompt_for(task, "full_cli_original")
        self.assertIn('entire brain brief "task: Fix the regression. | ExactSymbol, important invariant" --json', prompt)
        self.assertIn("Your first context command must be", prompt)
        self.assertIn("prefer `action_checklist`", prompt)
        self.assertIn("Read `.benchmark/brain-history-excerpt.md` only if", prompt)
        self.assertIn("likely_edit_files", prompt)
        self.assertIn("likely_test_files", prompt)
        self.assertIn("Do not run top-level `entire doctor`", prompt)
        self.assertIn("Do not edit tests unless the task explicitly asks", prompt)

    def test_no_brain_prompt_forbids_history_artifacts(self):
        prompt = run.prompt_for(
            {
                "id": "task",
                "prompt": "Fix the regression.",
                "brain_queries": [],
                "expected_files": ["pkg/file.ts"],
                "validation": ["npm test"],
            },
            "no_brain",
        )
        self.assertIn("Do not inspect `.entire`, `.benchmark`, or Brain/session/checkpoint artifacts", prompt)

    def test_compact_full_brain_prompt_has_no_raw_excerpt(self):
        prompt = run.prompt_for(
            {
                "id": "task",
                "prompt": "Fix the regression.",
                "brain_queries": ["ExactSymbol"],
                "expected_files": ["pkg/file.ts"],
                "validation": ["npm test"],
                "prepare_semantic": True,
            },
            "full_cli_compact",
        )
        self.assertIn("treat `action_checklist` as the first-pass current-code inventory", prompt)
        self.assertIn("--json --limit 4", prompt)
        self.assertIn("Avoid broad `rg`/`grep`/`find` unless", prompt)
        self.assertIn("Prefer `likely_test_files` for one focused validation command", prompt)
        self.assertIn("provides no raw history excerpt", prompt)
        self.assertIn("do not inspect checkpoint/session files directly", prompt)

    def test_apply_task_env_prepends_path_prefix(self):
        env = run.apply_task_env({"PATH": "/usr/bin"}, {"path_prefix": "/node24/bin"})
        self.assertEqual(env["PATH"], "/node24/bin:/usr/bin")

    def test_shell_cmd_preserves_prepared_path(self):
        env = {"PATH": "/node24/bin:/usr/bin"}
        proc = run.shell_cmd('printf "%s" "$PATH"', cwd=RUN_PATH.parent, env=env)
        self.assertEqual(proc.stdout, "/node24/bin:/usr/bin")

    def test_activity_counts_actual_tools_not_prompt_text(self):
        stdout = json.dumps(
            {
                "type": "user",
                "message": {"content": "Do not run the `entire brain` CLI; use brain_brief."},
            }
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["activity_source"], "protocol_json")
        self.assertEqual(activity["direct_brain_cli_calls"], 0)
        self.assertEqual(activity["mcp_tool_calls"], 0)

    def test_activity_counts_mcp_and_shell_tool_events(self):
        stdout = "\n".join(
            [
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "content": [
                                {
                                    "type": "tool_use",
                                    "name": "mcp__entire-brain__brain_search",
                                    "input": {"query": "media playback"},
                                }
                            ]
                        },
                    }
                ),
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "content": [
                                {
                                    "type": "tool_use",
                                    "name": "Bash",
                                    "input": {"command": "entire brain brief && rg requiresPlaybackVerification && npm test"},
                                }
                            ]
                        },
                    }
                ),
            ]
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["activity_source"], "protocol_json")
        self.assertEqual(activity["mcp_tool_calls"], 1)
        self.assertEqual(activity["direct_brain_cli_calls"], 1)
        self.assertEqual(activity["search_calls"], 1)
        self.assertTrue(activity["ran_tests"])

    def test_activity_counts_codex_mcp_tool_call_events(self):
        stdout = json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "type": "mcp_tool_call",
                    "server": "entire_brain",
                    "tool": "brain_brief",
                    "arguments": {"task": "self contained browser loop"},
                },
            }
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["activity_source"], "protocol_json")
        self.assertTrue(activity["used_mcp"])
        self.assertEqual(activity["mcp_tool_calls"], 1)
        self.assertIn("mcp__entire_brain__brain_brief", activity["mcp_tool_names"])
        self.assertEqual(activity["direct_brain_cli_calls"], 0)
        self.assertTrue(activity["checked_brief"])

    def test_activity_counts_brain_regressions_and_review_mcp_calls(self):
        # Regression guard: the radar tools (brain_regressions / brain_review) must be counted as
        # MCP tool calls. They were omitted from the parser allowlist, so every radar arm scored
        # mcp_tool_calls=0 and the condition audit falsely reported "no_mcp_tool_calls" even though
        # the agent had called brain_regressions (proved by the MCP server logs).
        for tool in ("brain_regressions", "brain_review", "brain_workspace_review"):
            stdout = json.dumps(
                {
                    "type": "assistant",
                    "message": {
                        "content": [
                            {
                                "type": "tool_use",
                                "name": f"mcp__entire_brain__{tool}",
                                "input": {"query": "scopeBaseRef base scope review"},
                            }
                        ]
                    },
                }
            )
            activity = run.extract_agent_activity(stdout, "")
            self.assertEqual(activity["mcp_tool_calls"], 1, tool)
            self.assertIn(f"mcp__entire_brain__{tool}", activity["mcp_tool_names"])
            self.assertTrue(activity["used_mcp"], tool)

    def test_radar_mcp_history_audit_requires_brain_regressions(self):
        # Under a radar delivery, the agent calls brain_regressions (not brain_brief). The audit's
        # required-tool floor must follow suit, or a correct radar run is mis-flagged as a failure.
        os.environ["BENCH_RADAR_LOCATION_ONLY"] = "1"
        try:
            runner = run.RunnerSpec(id="o", agent="claude", model="opus")
            agent_info = {
                "mcp": {"enabled": True},
                "activity": {"mcp_tool_calls": 1, "mcp_tool_names": ["mcp__entire_brain__brain_regressions"]},
            }
            audit = run.mcp_condition_audit("mcp_history", agent_info, runner)
            self.assertTrue(audit["ok"], audit)
            self.assertEqual(run.mcp_history_required_tools(runner), ("brain_regressions",))
        finally:
            del os.environ["BENCH_RADAR_LOCATION_ONLY"]

    def test_activity_counts_codex_command_execution_events(self):
        stdout = json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "type": "command_execution",
                    "command": "/bin/zsh -lc 'entire brain brief task --json && rg needle && npm test'",
                    "exit_code": 0,
                    "status": "completed",
                },
            }
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["activity_source"], "protocol_json")
        self.assertIn("brief", activity["brain_commands"])
        self.assertEqual(activity["direct_brain_cli_calls"], 1)
        self.assertEqual(activity["search_calls"], 1)
        self.assertTrue(activity["ran_tests"])

    def test_mcp_condition_audit_requires_mcp_calls_and_blocks_cli(self):
        self.assertTrue(run.mcp_condition_audit("no_brain", {})["ok"])
        missing = run.mcp_condition_audit(
            "mcp_history",
            {"mcp": {"enabled": True}, "activity": {"mcp_tool_calls": 0, "direct_brain_cli_calls": 0}},
        )
        self.assertFalse(missing["ok"])
        self.assertEqual(missing["findings"][0]["kind"], "no_mcp_tool_calls")
        ok = run.mcp_condition_audit(
            "mcp_history",
            {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 2,
                    "mcp_tool_names": ["mcp__entire_brain__brain_brief", "mcp__entire_brain__brain_search"],
                    "direct_brain_cli_calls": 0,
                },
            },
        )
        self.assertTrue(ok["ok"])
        missing_history = run.mcp_condition_audit(
            "mcp_history",
            {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_brief"],
                    "direct_brain_cli_calls": 0,
                },
            },
        )
        self.assertFalse(missing_history["ok"])
        self.assertIn("missing_required_mcp_history_tool", {finding["kind"] for finding in missing_history["findings"]})

    def test_text_activity_is_marked_as_fallback(self):
        activity = run.extract_agent_activity("I would run rg needle and entire brain brief.", "")
        self.assertEqual(activity["activity_source"], "text_fallback")
        self.assertEqual(activity["structured_tool_event_count"], 0)

    def test_private_benchmark_paths_are_excluded_from_patch_accounting(self):
        self.assertTrue(run.benchmark_private_path(".entire/settings.json"))
        self.assertTrue(run.benchmark_private_path(".benchmark/plugin/state.json"))
        self.assertTrue(run.benchmark_private_path("./.codex/config.toml"))
        self.assertFalse(run.benchmark_private_path("apps/desktop/src/main/agentic-decider.ts"))

    def test_sanitize_agent_worktree_removes_hidden_harness_scaffolding(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = pathlib.Path(tmp)
            run.run_cmd(["git", "init"], cwd=repo, check=True)
            task_dir = repo / "benchmarks" / "agent-brain" / "tasks"
            task_dir.mkdir(parents=True)
            (task_dir / "secret.json").write_text('{"validation":["hidden"],"hide_validation_from_agent":true}\n')
            (repo / "benchmarks" / "agent-brain" / "run.py").write_text("hide_validation_from_agent\n")
            (repo / "internal").mkdir()
            (repo / "internal" / "code.go").write_text("package internal\n")
            run.run_cmd(["git", "add", "-A"], cwd=repo, check=True)
            run.run_cmd(
                ["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init"],
                cwd=repo,
                env=run.benchmark_git_env(),
                check=True,
            )

            before = run.agent_secret_preflight(repo)
            self.assertFalse(before["ok"])
            sanitization = run.sanitize_agent_worktree(repo, {"agent_hidden_paths": ["benchmarks/agent-brain"]})
            self.assertIn("benchmarks/agent-brain", sanitization["removed_paths"])
            self.assertTrue(sanitization["committed"])
            after = run.agent_secret_preflight(repo)
            self.assertTrue(after["ok"])
            self.assertFalse((repo / "benchmarks" / "agent-brain").exists())

    def test_agent_hidden_paths_reject_path_traversal(self):
        with self.assertRaises(ValueError):
            run.agent_hidden_paths({"agent_hidden_paths": ["../outside"]})

    def test_reset_agent_history_to_root_hides_setup_commit_and_preserves_diff(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = pathlib.Path(tmp)
            run.run_cmd(["git", "init"], cwd=repo, check=True)
            target = repo / "target.txt"
            target.write_text("fixed behavior\n")
            run.run_cmd(["git", "add", "target.txt"], cwd=repo, check=True)
            run.run_cmd(
                ["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "real fix"],
                cwd=repo,
                env=run.benchmark_git_env(),
                check=True,
            )

            target.write_text("regressed behavior\n")
            run.run_cmd(["git", "add", "target.txt"], cwd=repo, check=True)
            run.run_cmd(
                [
                    "git",
                    "-c",
                    "user.name=Entire Brain Benchmark",
                    "-c",
                    "user.email=benchmark@example.invalid",
                    "commit",
                    "-m",
                    "Benchmark setup",
                ],
                cwd=repo,
                env=run.benchmark_git_env(),
                check=True,
            )
            setup_commit = run.run_cmd(["git", "rev-parse", "HEAD"], cwd=repo, check=True).stdout.strip()
            setup_patch = run.run_cmd(["git", "show", "--format=", "HEAD"], cwd=repo, check=True).stdout
            self.assertIn("-fixed behavior", setup_patch)
            self.assertIn("+regressed behavior", setup_patch)

            reset = run.reset_agent_history_to_root(repo, "Benchmark agent baseline")
            self.assertEqual(reset["parent_count"], 0)
            parents = run.run_cmd(["git", "show", "--format=%P", "--no-patch", "HEAD"], cwd=repo, check=True).stdout.strip()
            self.assertEqual(parents, "")
            baseline_patch = run.run_cmd(["git", "show", "--format=", "HEAD"], cwd=repo, check=True).stdout
            self.assertNotIn("-fixed behavior", baseline_patch)
            self.assertIn("+regressed behavior", baseline_patch)
            reflog = run.run_cmd(["git", "reflog"], cwd=repo, check=True).stdout
            self.assertNotIn("Benchmark setup", reflog)
            old_commit = run.run_cmd(["git", "cat-file", "-e", f"{setup_commit}^{{commit}}"], cwd=repo)
            self.assertNotEqual(old_commit.returncode, 0)

            target.write_text("agent repair\n")
            diff = run.run_cmd(["git", "diff", "--", "target.txt"], cwd=repo, check=True).stdout
            self.assertIn("-regressed behavior", diff)
            self.assertIn("+agent repair", diff)

    def test_create_worktree_uses_synthetic_repo_without_source_history(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = pathlib.Path(tmp) / "source"
            source.mkdir()
            run.run_cmd(["git", "init"], cwd=source, check=True)
            run.run_cmd(["git", "remote", "add", "origin", "https://github.com/example/source.git"], cwd=source, check=True)
            target = source / "target.txt"
            target.write_text("fixed behavior\n")
            run.run_cmd(["git", "add", "target.txt"], cwd=source, check=True)
            run.run_cmd(
                ["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "real fix"],
                cwd=source,
                env=run.benchmark_git_env(),
                check=True,
            )
            source_head = run.run_cmd(["git", "rev-parse", "HEAD"], cwd=source, check=True).stdout.strip()
            task = {
                "id": "synthetic-history",
                "repo_path": str(source),
                "setup_replacements": [
                    {"path": "target.txt", "old": "fixed behavior\n", "new": "regressed behavior\n"}
                ],
            }
            run_dir = pathlib.Path(tmp) / "run"
            run_dir.mkdir()

            worktree = run.create_worktree(task, run_dir)
            parents = run.run_cmd(["git", "show", "--format=%P", "--no-patch", "HEAD"], cwd=worktree, check=True).stdout.strip()
            self.assertEqual(parents, "")
            setup_patch = run.run_cmd(["git", "show", "--format=", "HEAD", "--", "target.txt"], cwd=worktree, check=True).stdout
            self.assertNotIn("-fixed behavior", setup_patch)
            self.assertIn("+regressed behavior", setup_patch)
            remote = run.run_cmd(["git", "remote", "get-url", "origin"], cwd=worktree, check=True).stdout.strip()
            self.assertEqual(remote, "https://github.com/example/source.git")
            source_commit = run.run_cmd(["git", "cat-file", "-e", f"{source_head}^{{commit}}"], cwd=worktree)
            self.assertNotEqual(source_commit.returncode, 0)

    def test_sanitize_brain_history_scrubs_benchmark_scaffolding(self):
        with tempfile.TemporaryDirectory() as tmp:
            plugin = pathlib.Path(tmp) / "plugin"
            brain = plugin / "data" / "brain" / "repo"
            current_brain = plugin / "data" / "repos" / "gh" / "example" / "repo"
            (brain / "sessions").mkdir(parents=True)
            (current_brain / "sessions").mkdir(parents=True)
            session = brain / "sessions" / "one.jsonl"
            session.write_text(
                '{"message":"useful history"}\n'
                '{"message":"benchmarks/agent-brain/tasks/secret.json contains validation"}\n'
            )
            current_session = current_brain / "sessions" / "two.jsonl"
            current_session.write_text(
                '{"message":"useful current history"}\n'
                '{"message":"benchmarks/agent-brain/tasks/current-secret.json contains validation"}\n'
            )
            index = brain / "history.json"
            index.write_text(json.dumps({"items": ["useful", "benchmarks/agent-brain/results/run/record.json"]}))
            current_index = current_brain / "history.json"
            current_index.write_text(
                json.dumps({"items": ["useful", "benchmarks/agent-brain/results/current/record.json"]})
            )

            summary = run.sanitize_brain_history(plugin)
            self.assertEqual(summary["files_scrubbed"], 4)
            self.assertIn("useful history", session.read_text())
            self.assertIn("useful current history", current_session.read_text())
            self.assertNotIn("benchmarks/agent-brain/tasks", session.read_text())
            self.assertNotIn("benchmarks/agent-brain/tasks", current_session.read_text())
            self.assertIn("[redacted benchmark scaffold]", index.read_text())
            self.assertIn("[redacted benchmark scaffold]", current_index.read_text())

    def test_agent_output_leak_audit_flags_hidden_validation_text(self):
        task = {
            "hide_validation_from_agent": True,
            "validation": ["node ./hidden-validator.js --secret-case=metadata-step"],
        }
        audit = run.agent_output_leak_audit(
            task,
            "I ran node ./hidden-validator.js --secret-case=metadata-step",
            "",
        )
        self.assertFalse(audit["ok"])
        self.assertEqual(audit["findings"][0]["kind"], "hidden_validation_marker_in_output")

    def test_agent_output_leak_audit_uses_explicit_canaries_when_present(self):
        task = {
            "hide_validation_from_agent": True,
            "leak_markers": ["release-canary-hidden-validation-12345"],
            "validation": ["go test ./internal/cli -run 'TestDiscoveredByAgent'"],
        }
        clean = run.agent_output_leak_audit(
            task,
            "I ran go test ./internal/cli -run 'TestDiscoveredByAgent'",
            "",
        )
        self.assertTrue(clean["ok"])
        leaked = run.agent_output_leak_audit(task, "release-canary-hidden-validation-12345", "")
        self.assertFalse(leaked["ok"])
        self.assertEqual(leaked["findings"][0]["kind"], "hidden_validation_marker_in_output")

    def test_agent_output_leak_audit_explicit_canaries_ignore_generic_task_paths(self):
        task = {
            "hide_validation_from_agent": True,
            "leak_markers": ["release-canary-hidden-validation-12345"],
            "validation": ["go test ./internal/cli -run 'TestHiddenValidation'"],
        }
        audit = run.agent_output_leak_audit(
            task,
            "diagnostic path benchmarks/agent-brain/tasks/task.json appeared in a tool transcript",
            "",
        )
        self.assertTrue(audit["ok"])

    def test_agent_output_leak_audit_allows_result_paths_and_setup_text(self):
        task = {
            "hide_validation_from_agent": True,
            "validation": ["node ./hidden-validator.js --secret-case=metadata-step"],
            "setup_replacements": [{"old": "previousResponseId: null", "new": "previousResponseId"}],
        }
        audit = run.agent_output_leak_audit(
            task,
            "cwd=/tmp/benchmarks/agent-brain/results/suite/run/worktree\nfixed previousResponseId: null",
            "",
        )
        self.assertTrue(audit["ok"])

    def test_changed_files_ignores_private_benchmark_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = pathlib.Path(tmp)
            run.run_cmd(["git", "init"], cwd=repo, check=True)
            (repo / ".entire").mkdir()
            (repo / ".entire" / "settings.json").write_text("{}")
            (repo / ".benchmark").mkdir()
            (repo / ".benchmark" / "scratch.json").write_text("{}")
            (repo / ".codex").mkdir()
            (repo / ".codex" / "config.toml").write_text("")
            (repo / "src.ts").write_text("export const ok = true;\n")
            self.assertEqual(run.changed_files(repo), ["src.ts"])

    def test_remove_agent_visible_entire_history_deletes_copied_source_history(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = pathlib.Path(tmp)
            (repo / ".entire" / "metadata").mkdir(parents=True)
            (repo / ".entire" / "settings.json").write_text("{}")
            self.assertTrue(run.remove_agent_visible_entire_history(repo))
            self.assertFalse((repo / ".entire").exists())
            self.assertFalse(run.remove_agent_visible_entire_history(repo))

    def test_brain_verdict_flags_saturated_overhead(self):
        verdict = run.brain_comparison_verdict(
            {
                "n_condition": 1,
                "n_baseline": 1,
                "delta": 0,
                "success_rate_condition": 1.0,
                "success_rate_baseline": 1.0,
                "mean_agent_seconds_condition": 78,
                "mean_agent_seconds_baseline": 80,
                "mean_total_tokens_condition": 860000,
                "mean_total_tokens_baseline": 690000,
                "mean_search_calls_condition": 6,
                "mean_search_calls_baseline": 10,
            }
        )
        self.assertEqual(verdict["verdict"], "saturated/overhead_negative")
        self.assertFalse(verdict["proof_ready"])
        self.assertIn("search_calls_dropped>=20%", verdict["verdict_reasons"])

    def test_brain_verdict_accepts_repeated_efficiency_win(self):
        verdict = run.brain_comparison_verdict(
            {
                "n_condition": 4,
                "n_baseline": 4,
                "delta": 0,
                "success_rate_condition": 1.0,
                "success_rate_baseline": 1.0,
                "mean_agent_seconds_condition": 60,
                "mean_agent_seconds_baseline": 100,
                "mean_total_tokens_condition": 500000,
                "mean_total_tokens_baseline": 500000,
                "mean_search_calls_condition": 4,
                "mean_search_calls_baseline": 8,
            }
        )
        self.assertEqual(verdict["verdict"], "brain_positive")
        self.assertTrue(verdict["proof_ready"])

    def test_brain_verdict_rejects_efficiency_win_when_brain_validation_is_flaky(self):
        verdict = run.brain_comparison_verdict(
            {
                "n_condition": 3,
                "n_baseline": 3,
                "delta": 0.3,
                "success_rate_condition": 2 / 3,
                "success_rate_baseline": 2 / 3,
                "mean_agent_seconds_condition": 80,
                "mean_agent_seconds_baseline": 100,
                "mean_total_tokens_condition": 600000,
                "mean_total_tokens_baseline": 1000000,
                "mean_search_calls_condition": 5,
                "mean_search_calls_baseline": 15,
            }
        )
        self.assertNotEqual(verdict["verdict"], "brain_positive")
        self.assertFalse(verdict["proof_ready"])
        self.assertIn("brain_validation_not_clean", verdict["verdict_reasons"])


class OpusCompactModeTests(unittest.TestCase):
    TASK = {"id": "t", "prompt": "Fix it.", "brain_queries": ["X"], "expected_files": ["a.go"], "validation": ["go test ./..."]}

    def test_opus_mcp_is_compact_and_bounded(self):
        p = run.prompt_for(self.TASK, "mcp_history", run.parse_runner_spec("claude:opus:high"))
        self.assertIn("limit: 3", p)                       # tiny brief
        self.assertIn("do NOT call `brain_search`", p)     # no forced search call
        self.assertIn("finite budget", p)
        # other Claude models keep the standard MCP delivery (forced brain_search)
        son = run.prompt_for(self.TASK, "mcp_history", run.parse_runner_spec("claude:sonnet:high"))
        self.assertIn("run exactly one `mcp__entire_brain__brain_search`", son)
        self.assertNotIn("limit: 3", son)

    def test_opus_cli_uses_tiny_limit(self):
        p = run.prompt_for(self.TASK, "full_cli_compact", run.parse_runner_spec("claude:opus:high"))
        self.assertIn("--limit 2", p)
        # haiku keeps the standard --limit 4 CLI delivery
        hai = run.prompt_for(self.TASK, "full_cli_compact", run.parse_runner_spec("claude:haiku:high"))
        self.assertIn("--limit 4", hai)
        self.assertNotIn("--limit 2", hai)


class StatsAndAttributionTests(unittest.TestCase):
    def test_welch_p_value_is_t_test_not_normal(self):
        # Clearly separated 3-vs-3 should be small but NOT the absurd ~0 the old
        # z/erfc gave; overlapping samples must be clearly non-significant.
        sep = run.welch_p_value([97.0, 97.0, 98.0], [84.0, 85.0, 86.0])
        self.assertIsNotNone(sep)
        self.assertLess(sep, 0.05)
        self.assertGreater(sep, 1e-4)  # t-distribution at ~4 df, not a near-0 z-value
        overlap = run.welch_p_value([95, 96, 94], [93, 95, 96])
        self.assertGreater(overlap, 0.1)
        self.assertEqual(run.welch_p_value([90, 90, 90], [90, 90, 90]), 1.0)
        self.assertIsNone(run.welch_p_value([1.0], [2.0]))  # n<2

    def test_extract_resolved_model(self):
        # Claude exposes the resolved model via modelUsage keys.
        claude = '{"type":"result","modelUsage":{"claude-haiku-4-5":{"inputTokens":10}}}'
        self.assertEqual(run.extract_resolved_model(claude), "claude-haiku-4-5")
        # Generic "model":"X" is picked up too.
        self.assertEqual(run.extract_resolved_model('{"model":"gpt-x"}'), "gpt-x")
        # Codex exec --json exposes no model field -> None (honest: not confirmed).
        self.assertIsNone(run.extract_resolved_model('{"type":"item","text":"done"}'))
        self.assertIsNone(run.extract_resolved_model(""))

    def test_record_provenance_captures_source_base_head_and_config(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            repo = root / "repo"
            repo.mkdir()
            run.run_cmd(["git", "init"], cwd=repo, check=True)
            (repo / "file.txt").write_text("one\n")
            run.run_cmd(["git", "add", "file.txt"], cwd=repo, check=True)
            run.run_cmd(
                ["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init"],
                cwd=repo,
                env=run.benchmark_git_env(),
                check=True,
            )
            head = run.run_cmd(["git", "rev-parse", "HEAD"], cwd=repo, check=True).stdout.strip()
            tools_dir = root / "tools"
            tools_dir.mkdir()
            tools = {}
            for name in ("brain", "sem", "entire"):
                path = tools_dir / name
                path.write_text(f"{name}\n")
                tools[name] = path
            tools["bin"] = tools_dir
            task = {
                "id": "t",
                "repo": "tmp",
                "repo_path": str(repo),
                "conditions": ["no_brain"],
                "prompt": "Fix it.",
                "validation": [],
            }
            bound, source, base = run.bind_task_base_commit(task)
            args = argparse.Namespace(
                checkpoint_limit=17,
                no_brain_cache=False,
                refresh_brain_cache=False,
                timeout=123,
                claude_budget=0.0,
                stop_after_no_brain_score=None,
                pricing_file=None,
                pricing_json=None,
            )
            prov = run.build_record_provenance(
                bound,
                run.RunnerSpec(id="codex-gpt-test-low", agent="codex", model="gpt-test", effort="low"),
                "no_brain",
                1,
                root / "suite",
                tools,
                args,
                command="run",
                source=source,
                base=base,
            )
            self.assertEqual(prov["source"]["base"]["commit"], head)
            self.assertEqual(prov["source"]["head"]["commit"], head)
            self.assertEqual(prov["source"]["base_ref_source"], "source_head")
            self.assertEqual(prov["run_config"]["checkpoint_limit"], 17)
            self.assertRegex(prov["run_config"]["fingerprint"], r"^[0-9a-f]{64}$")
            self.assertRegex(prov["tools"]["brain"]["sha256"], r"^[0-9a-f]{64}$")

            summary = run.suite_provenance([{"provenance": prov}])
            self.assertEqual(summary["records_with_provenance"], 1)
            self.assertEqual(summary["sources"][0]["base_commit"], head)
            self.assertEqual(summary["run_configs"][0]["condition"], "no_brain")


def _rec(tokens, *, ok=True, score=90):
    return {
        "task_id": "t",
        "agent": "claude",
        "runner": {"id": "claude-sonnet-high"},
        "condition": None,  # set by caller
        "agent_info": {"usage": {"total_tokens": tokens}},
        "score": {"total": score},
        "validation": {"ok": ok},
        "ok": True,
    }


class PanelAndStabilityTests(unittest.TestCase):
    def test_panel_preflight_rejects_unpinned_and_thin_panels(self):
        errors = run.panel_preflight(
            {"runners": ["codex", "claude:claude-sonnet-4-6:high"], "tasks": [], "conditions": ["full_brain"], "repetitions": 1}
        )
        joined = " | ".join(errors)
        self.assertIn("not fully pinned", joined)  # unpinned codex runner
        self.assertIn("no tasks", joined)
        self.assertIn("proof_minimum", joined)  # repetitions < 4
        self.assertIn("no_brain baseline", joined)  # missing baseline

    def test_panel_preflight_accepts_committed_full_manifest(self):
        panel = run.load_panel("full")
        self.assertEqual(run.panel_preflight(panel), [])
        self.assertGreaterEqual(len(run.load_tasks(panel["tasks"])), 1)
        for spec in panel["runners"]:
            rs = run.parse_runner_spec(spec)
            self.assertTrue(rs.model and rs.effort, f"{spec} must be pinned")
        for task in run.load_tasks(panel["tasks"]):
            task_conditions = set(panel["conditions"]) & set(task.get("conditions", []))
            if any(run.condition_prepares_history(condition) for condition in task_conditions):
                self.assertTrue(
                    task.get("copy_checkpoint_ref_from_source") or task.get("copy_entire_history_from_source"),
                    f"{task['id']} must declare a local history source",
                )

    def test_panel_preflight_rejects_history_tasks_without_source(self):
        errors = run.panel_preflight(
            {
                "runners": ["codex:gpt-test:low"],
                "tasks": ["entire-brain-history-bundle-sha256.json"],
                "conditions": ["no_brain", "full_brain"],
                "repetitions": 4,
            }
        )
        self.assertIn("has no local history source", " | ".join(errors))

    def test_full_panel_declares_cross_repo_workspace_coverage(self):
        # WS4: the multi-repo coverage gap is declared in the manifest (not silently missing), and the
        # extra field is inert for the runner (preflight still passes).
        panel = run.load_panel("full")
        ws = panel.get("workspace_tasks", [])
        self.assertGreaterEqual(len(ws), 1)
        self.assertTrue(any("status" in t for t in ws))
        self.assertEqual(run.panel_preflight(panel), [])

    def test_run_config_provenance_includes_panel_manifest_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            panel_path = root / "release-panel.json"
            panel_path.write_text(json.dumps({
                "name": "release-panel",
                "tasks": ["t"],
                "runners": ["codex:gpt-test:low"],
                "conditions": ["no_brain", "full_brain"],
                "repetitions": 4,
            }))
            args = argparse.Namespace(
                tasks=["t"],
                agents="",
                runners="codex:gpt-test:low",
                conditions="no_brain,full_brain",
                repetitions=4,
                checkpoint_limit=None,
                no_brain_cache=False,
                refresh_brain_cache=False,
                timeout=120,
                claude_budget=None,
                stop_after_no_brain_score=None,
                pricing_file=None,
                pricing_json=None,
                panel_name="release-panel",
                panel_path=run.display_path(panel_path),
                panel_config_sha256=run.file_sha256(panel_path),
            )
            payload = run.run_config_provenance(
                "run",
                run.parse_runner_spec("codex:gpt-test:low"),
                "full_brain",
                1,
                root / "suite",
                args,
            )
            self.assertEqual(payload["panel"]["name"], "release-panel")
            self.assertEqual(payload["panel"]["path"], run.display_path(panel_path))
            self.assertEqual(payload["panel"]["config_sha256"], run.file_sha256(panel_path))

            args.panel_config_sha256 = "f" * 64
            changed = run.run_config_provenance(
                "run",
                run.parse_runner_spec("codex:gpt-test:low"),
                "full_brain",
                1,
                root / "suite",
                args,
            )
            self.assertNotEqual(payload["fingerprint"], changed["fingerprint"])

    def test_coefficient_of_variation_and_drop_one(self):
        self.assertIsNone(run.coefficient_of_variation([5.0]))  # n<2
        self.assertEqual(run.coefficient_of_variation([10.0, 10.0, 10.0]), 0.0)
        self.assertGreater(run.coefficient_of_variation([10.0, 20.0, 30.0]), 0.0)
        # A token win that is wide enough survives dropping the most favourable rep from each arm.
        self.assertTrue(run.delta_survives_drop_one([400, 410, 420, 405], [900, 950, 910, 940], lower_is_better=True))
        # A coin-flip overlap does not.
        self.assertFalse(run.delta_survives_drop_one([400, 999], [401, 998], lower_is_better=True))

    def test_comparison_stability_tags(self):
        # Stable token win: brain cheaper, significant, survives drop-one -> brain_positive_stable.
        cond = [_rec(t) for t in (400, 420, 410, 405)]
        base = [_rec(t) for t in (900, 950, 910, 940)]
        comp = {
            "mean_total_tokens_condition": 408.75,
            "mean_total_tokens_baseline": 925.0,
            "p_value_total_tokens": run.welch_p_value([400, 420, 410, 405], [900, 950, 910, 940]),
            "pass_rate_condition": 1.0,
            "pass_rate_baseline": 1.0,
        }
        self.assertEqual(run.comparison_stability(cond, base, comp)["tag"], "brain_positive_stable")

        # Saturated: both pass 100%, tokens equal/noisy -> not a win, just saturated.
        cond2 = [_rec(t) for t in (500, 900, 500, 900)]
        base2 = [_rec(t) for t in (500, 900, 500, 900)]
        comp2 = {"mean_total_tokens_condition": 700.0, "mean_total_tokens_baseline": 700.0,
                 "p_value_total_tokens": 0.9, "pass_rate_condition": 1.0, "pass_rate_baseline": 1.0}
        self.assertEqual(run.comparison_stability(cond2, base2, comp2)["tag"], "saturated")

        # Noisy: one lucky pass (1/4) that does NOT survive drop-one, tokens not significant, headroom left.
        cond3 = [_rec(t, ok=(i == 0)) for i, t in enumerate((500, 520, 510, 505))]
        base3 = [_rec(t, ok=False) for t in (800, 820, 810, 805)]
        comp3 = {"mean_total_tokens_condition": 508.75, "mean_total_tokens_baseline": 808.75,
                 "p_value_total_tokens": 0.4, "pass_rate_condition": 0.25, "pass_rate_baseline": 0.0}
        self.assertEqual(run.comparison_stability(cond3, base3, comp3)["tag"], "noisy")

    def test_stability_gates_on_holm_not_raw_p(self):
        # The sharpest attack on the gate: a raw-significant token win that loses significance under
        # family-wise (Holm) correction must NOT be tagged brain_positive_stable.
        cond = [_rec(t) for t in (400, 420, 410, 405)]
        base = [_rec(t) for t in (900, 950, 910, 940)]
        comp = {
            "mean_total_tokens_condition": 408.75,
            "mean_total_tokens_baseline": 925.0,
            "p_value_total_tokens": 0.04,       # raw: significant
            "p_value_total_tokens_holm": 0.9,   # family-wise: NOT significant
            "pass_rate_condition": 1.0,
            "pass_rate_baseline": 1.0,
        }
        st = run.comparison_stability(cond, base, comp)
        self.assertFalse(st["tokens_significant_p_lt_0_05"])
        self.assertFalse(st["token_win_survives_drop_one"])
        self.assertNotEqual(st["tag"], "brain_positive_stable")
        # Same data, Holm also significant -> the win is real and tags stable.
        comp["p_value_total_tokens_holm"] = 0.04
        self.assertEqual(run.comparison_stability(cond, base, comp)["tag"], "brain_positive_stable")

    def test_summarize_attaches_stability_and_is_reproducible(self):
        records = []
        for tokens in (400, 420, 410, 405):
            r = _rec(tokens)
            r["condition"] = "full_brain"
            records.append(r)
        for tokens in (900, 950, 910, 940):
            r = _rec(tokens)
            r["condition"] = "no_brain"
            records.append(r)
        with tempfile.TemporaryDirectory() as d1, tempfile.TemporaryDirectory() as d2:
            s1 = run.summarize(records, pathlib.Path(d1))
            s2 = run.summarize(records, pathlib.Path(d2))  # report re-summarizes stored records
        self.assertEqual(len(s1["comparisons"]), 1)
        stab = s1["comparisons"][0]["stability"]
        self.assertEqual(stab["tag"], "brain_positive_stable")
        self.assertIsNotNone(stab["coefficient_of_variation_total_tokens_condition"])
        self.assertIsNotNone(stab["coefficient_of_variation_total_tokens_baseline"])
        self.assertEqual(s1["stability_tags"], {"brain_positive_stable": 1})
        # Reproducible: same records -> identical stability verdict.
        self.assertEqual(s1["comparisons"][0]["stability"], s2["comparisons"][0]["stability"])

    def test_summarize_requires_stable_tag_for_proof_ready(self):
        records = []
        for tokens in (200, 2000, 200, 2000):
            r = _rec(tokens)
            r["condition"] = "full_brain"
            records.append(r)
        for tokens in (1500, 1500, 1500, 1500):
            r = _rec(tokens)
            r["condition"] = "no_brain"
            records.append(r)
        with tempfile.TemporaryDirectory() as d:
            summary = run.summarize(records, pathlib.Path(d))
        comp = summary["comparisons"][0]
        self.assertEqual(comp["verdict"], "brain_positive")
        self.assertTrue(comp["candidate_proof_ready"])
        self.assertNotEqual(comp["stability"]["tag"], "brain_positive_stable")
        self.assertFalse(comp["proof_ready"])

    def test_existing_phase2_proofs_require_stable_clean_audit(self):
        old_result_dir = run.RESULT_DIR
        try:
            with tempfile.TemporaryDirectory() as d:
                result_dir = pathlib.Path(d)
                run.RESULT_DIR = result_dir
                suite = result_dir / "suite-a"
                suite.mkdir()
                comparison = {
                    "task_id": "t",
                    "runner": "claude-sonnet-high",
                    "agent": "claude",
                    "condition": "full_brain",
                    "proof_ready": True,
                    "stability": {"tag": "brain_positive_stable"},
                    "success_rate_condition": 1.0,
                    "success_rate_baseline": 0.0,
                    "delta": 10.0,
                    "p_value_approx_holm": 0.01,
                    "mean_total_tokens_condition": 100,
                    "mean_total_tokens_baseline": 200,
                    "p_value_total_tokens_holm": 0.01,
                }
                (suite / "summary.json").write_text(json.dumps({"comparisons": [comparison]}))
                self.assertEqual(run.load_existing_phase2_proofs(), [])
                (result_dir / "codex-audit-report.json").write_text(json.dumps({
                    "suites": {
                        "suite-a": {
                            "n_records": 4,
                            "n_flagged_records": 0,
                            "n_provenance_ok": 4,
                            "comparisons": [{"flags": [], "pass": True}],
                        }
                    }
                }))
                proofs = run.load_existing_phase2_proofs()
                self.assertEqual(len(proofs), 1)
                self.assertEqual(proofs[0]["proof_level"], "existing_repeated_run")
        finally:
            run.RESULT_DIR = old_result_dir


class CodexAuditScriptTests(unittest.TestCase):
    SOURCE_SHA = "1" * 40
    TOOL_SHA = "2" * 64
    TASK_SHA = "3" * 64
    CONFIG_SHA = "4" * 64
    RECORD_SHA = "5" * 64
    HARNESS_SHA = "6" * 40

    def _write_records(self, root: pathlib.Path, suite: str, records: list[dict]) -> pathlib.Path:
        suite_dir = root / suite
        suite_dir.mkdir(parents=True, exist_ok=True)
        (suite_dir / "records.ndjson").write_text("\n".join(json.dumps(r) for r in records) + "\n")
        return suite_dir

    def _provenance(self) -> dict:
        return {
            "schema": 1,
            "fingerprint": self.RECORD_SHA,
            "harness": {
                "head": {"available": True, "commit": self.HARNESS_SHA, "parents": [], "parent_count": 0},
                "dirty": {"available": True, "dirty": False},
            },
            "source": {
                "repo": "example",
                "repo_path_input": "/repo",
                "repo_path_resolved": "/repo",
                "base_ref": "HEAD",
                "base_ref_source": "source_head",
                "base": {"available": True, "commit": self.SOURCE_SHA, "parents": [], "parent_count": 0},
                "head": {"available": True, "commit": self.SOURCE_SHA, "parents": [], "parent_count": 0},
                "dirty": {"available": True, "dirty": False},
            },
            "task": {
                "id": "t",
                "path": "benchmarks/agent-brain/tasks/t.json",
                "config_sha256": self.TASK_SHA,
                "base_commit": None,
            },
            "run_config": {
                "command": "run",
                "condition": "no_brain",
                "repetition": 1,
                "runner": {"id": "codex", "agent": "codex", "model": "gpt-test", "effort": "low"},
                "fingerprint": self.CONFIG_SHA,
            },
            "tools": {
                "brain": {"sha256": self.TOOL_SHA},
                "sem": {"sha256": self.TOOL_SHA},
            },
        }

    def _record(self, *, used_brain: bool = False, condition: str = "no_brain", repetition: int = 1, run_id: str = "r1") -> dict:
        provenance = self._provenance()
        provenance["run_config"]["condition"] = condition
        provenance["run_config"]["repetition"] = repetition
        return {
            "run_id": run_id,
            "task_id": "t",
            "condition": condition,
            "repetition": repetition,
            "agent": "codex",
            "runner": {"id": "codex", "agent": "codex", "model": "gpt-test", "effort": "low"},
            "ok": True,
            "validation": {"ok": True, "results": [{"command": "go test ./...", "ok": True}]},
            "agent_info": {"activity": {
                "used_brain": used_brain,
                "mcp_tool_calls": 0,
                "direct_brain_cli_calls": 0,
                "search_calls": 0,
            }},
            "agent_baseline_history_reset": {"parent_count": 0},
            "agent_secret_preflight": {"ok": True},
            "agent_leak_audit": {"ok": True},
            "brain_prep": {"condition": condition},
            "changed_files": ["internal/cli/example.go"],
            "score": {
                "version": 2,
                "outcome": 40,
                "patch_focus": 20,
                "validation_discipline": 20,
                "runtime_efficiency": 10,
                "brain_use": 10,
                "total": 100,
            },
            "provenance": provenance,
        }

    def _release_record(self, suite: str, *, condition: str, repetition: int, run_id: str) -> dict:
        record = self._record(condition=condition, repetition=repetition, run_id=run_id)
        record["provenance"]["run_config"]["suite"] = suite
        record["provenance"]["run_config"]["panel"] = {
            "name": "release-panel",
            "path": "benchmarks/agent-brain/panels/release-panel.json",
            "config_sha256": self.CONFIG_SHA,
        }
        return record

    def _write_release_manifest(self, root: pathlib.Path) -> pathlib.Path:
        manifest = root / "manifest.json"
        manifest.write_text(json.dumps({
            "schema": 1,
            "release_citable_suite_globs": ["release-candidate-*"],
            "forbidden_suite_globs": ["release-local-*"],
            "min_repetitions_per_side": 4,
            "require_panel_provenance": True,
            "require_proof_ready_per_suite": True,
            "minimums": {
                "suites": 1,
                "records": 1,
                "proof_ready_comparisons": 1,
                "hard_flags": 0,
            },
        }))
        return manifest

    def test_audit_codex_supports_subset_output_and_fail_on_flags(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            self._write_records(results_dir, "clean-suite", [self._record()])
            self._write_records(results_dir, "dirty-suite", [self._record(used_brain=True)])

            clean = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(clean["totals"]["suites"], 1)
            self.assertEqual(clean["totals"]["hard_flags"], 0)
            self.assertEqual(clean["totals"]["proof_ready_comparisons"], 0)
            audit_codex.write_audit_report(clean, out_dir)
            self.assertTrue((out_dir / "codex-audit-report.json").exists())
            self.assertEqual(audit_codex.main(["--results", str(results_dir), "--suite-glob", "clean-*", "--out-dir", str(out_dir), "--fail-on-flags"]), 0)
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--suite-glob", "clean-*", "--out-dir", str(out_dir), "--fail-on-flags", "--min-proof-ready", "1"]),
                1,
            )
            thin_gate = json.loads((out_dir / "codex-audit-report.json").read_text())["gate_status"]
            self.assertFalse(thin_gate["release_evidence"])
            self.assertIn("proof_ready_comparisons 0 < required 1", thin_gate["failures"])
            self.assertIn("NOT RELEASE EVIDENCE", (out_dir / "codex-audit-report.md").read_text())

            clean_suite = results_dir / "clean-suite"
            (clean_suite / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 3,
                    "n_baseline": 3,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))
            stale_summary = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(stale_summary["totals"]["proof_ready_comparisons"], 0)
            flags = stale_summary["suites"]["clean-suite"]["comparisons"][0]["flags"]
            self.assertIn("G:proof_ready_without_matching_records", flags)
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--suite-glob", "clean-*", "--out-dir", str(out_dir), "--fail-on-flags", "--min-proof-ready", "1"]),
                1,
            )

            duplicate_records = []
            for i in range(3):
                duplicate_records.append(self._record(condition="no_brain", repetition=1, run_id=f"dup-base-{i}"))
                duplicate_records.append(self._record(condition="full_brain", repetition=1, run_id=f"dup-brain-{i}"))
            self._write_records(results_dir, "clean-suite", duplicate_records)
            duplicate_report = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(duplicate_report["totals"]["proof_ready_comparisons"], 0)
            duplicate_backing = duplicate_report["suites"]["clean-suite"]["comparisons"][0]["record_backing"]
            self.assertEqual(duplicate_backing["condition_unique_repetitions"], 1)

            proof_records = []
            for i in range(1, 5):
                proof_records.append(self._record(condition="no_brain", repetition=i, run_id=f"base-{i}"))
                proof_records.append(self._record(condition="full_brain", repetition=i, run_id=f"brain-{i}"))
            self._write_records(results_dir, "clean-suite", proof_records)
            (clean_suite / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))
            proof_clean = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(proof_clean["totals"]["hard_flags"], 0)
            self.assertEqual(proof_clean["totals"]["proof_ready_comparisons"], 1)
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--suite-glob", "clean-*", "--out-dir", str(out_dir), "--fail-on-flags", "--min-proof-ready", "1"]),
                0,
            )
            passing_gate = json.loads((out_dir / "codex-audit-report.json").read_text())["gate_status"]
            self.assertTrue(passing_gate["release_evidence"])
            self.assertEqual(passing_gate["failures"], [])

            failed_baseline_records = []
            for i in range(1, 5):
                base = self._record(condition="no_brain", repetition=i, run_id=f"failed-base-{i}")
                base["ok"] = False
                base["validation"]["ok"] = False
                for result in base["validation"]["results"]:
                    result["ok"] = False
                failed_baseline_records.append(base)
                failed_baseline_records.append(self._record(condition="full_brain", repetition=i, run_id=f"clean-brain-{i}"))
            self._write_records(results_dir, "clean-suite", failed_baseline_records)
            failed_baseline_clean = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(failed_baseline_clean["totals"]["hard_flags"], 0)
            self.assertEqual(failed_baseline_clean["totals"]["proof_ready_comparisons"], 1)
            failed_backing = failed_baseline_clean["suites"]["clean-suite"]["comparisons"][0]["record_backing"]
            self.assertEqual(failed_backing["baseline_records"], 4)

            unvalidated_records = []
            for i in range(1, 5):
                base = self._record(condition="no_brain", repetition=i, run_id=f"noval-base-{i}")
                brain = self._record(condition="full_brain", repetition=i, run_id=f"noval-brain-{i}")
                base["validation"] = {"ok": True, "results": []}
                brain["validation"] = {"ok": True, "results": []}
                unvalidated_records.extend([base, brain])
            self._write_records(results_dir, "clean-suite", unvalidated_records)
            unvalidated_report = audit_codex.build_audit_report(results_dir, ["clean-*"])
            self.assertEqual(unvalidated_report["totals"]["proof_ready_comparisons"], 0)
            self.assertGreater(unvalidated_report["totals"]["hard_flags"], 0)
            self.assertIn("F:no_validation_commands_run", unvalidated_report["suites"]["clean-suite"]["records"][0]["flags"])
            self.assertIn(
                "G:proof_ready_without_matching_records",
                unvalidated_report["suites"]["clean-suite"]["comparisons"][0]["flags"],
            )
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--suite-glob", "clean-*", "--out-dir", str(out_dir), "--fail-on-flags", "--min-proof-ready", "1"]),
                1,
            )

            dirty = audit_codex.build_audit_report(results_dir, ["dirty-*"])
            self.assertEqual(dirty["totals"]["hard_flags"], 1)
            self.assertEqual(audit_codex.main(["--results", str(results_dir), "--suite-glob", "dirty-*", "--out-dir", str(out_dir), "--fail-on-flags"]), 1)

            self.assertEqual(audit_codex.main(["--results", str(results_dir), "--suite-glob", "missing-*", "--out-dir", str(out_dir), "--fail-on-flags"]), 1)

    def test_release_manifest_excludes_local_pilots_and_requires_four_reps(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            self._write_records(results_dir, "release-local-negative-pilot", [
                self._record(used_brain=True, run_id="dirty-local-pilot"),
            ])

            suite = "release-candidate-semantic-proof"
            proof_records = []
            for i in range(1, 4):
                proof_records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"candidate-base-{i}"))
                proof_records.append(self._release_record(suite, condition="full_brain", repetition=i, run_id=f"candidate-brain-{i}"))
            candidate_dir = self._write_records(results_dir, suite, proof_records)
            (candidate_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 3,
                    "n_baseline": 3,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            too_thin = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(set(too_thin["suites"].keys()), {suite})
            self.assertNotIn("release-local-negative-pilot", too_thin["suites"])
            self.assertIn("G:proof_ready_without_n4", too_thin["suites"][suite]["comparisons"][0]["flags"])

            proof_records = []
            for i in range(1, 5):
                proof_records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"candidate-base-{i}"))
                proof_records.append(self._release_record(suite, condition="full_brain", repetition=i, run_id=f"candidate-brain-{i}"))
            self._write_records(results_dir, suite, proof_records)
            (candidate_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                0,
            )
            passing = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertTrue(passing["gate_status"]["release_evidence"])
            self.assertEqual(passing["totals"]["proof_ready_comparisons"], 1)
            self.assertEqual(passing["totals"]["hard_flags"], 0)

            with self.assertRaises(SystemExit):
                audit_codex.main([
                    "--results", str(results_dir),
                    "--release-manifest", str(manifest),
                    "--suite-glob", "release-*",
                    "--out-dir", str(out_dir),
                    "--fail-on-flags",
                ])

    def test_release_manifest_rejects_host_path_leaks(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            suite = "release-candidate-host-path-leak"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                brain = self._release_record(suite, condition="full_brain", repetition=i, run_id=f"brain-{i}")
                brain["stderr_tail"] = "tool warning path=/Users/alice/.codex/private-plugin/plugin.json"
                records.append(brain)
            suite_dir = self._write_records(results_dir, suite, records)
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            exploratory = audit_codex.build_audit_report(results_dir, ["release-candidate-*"])
            self.assertEqual(exploratory["totals"]["hard_flags"], 0)

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            release_report = json.loads((out_dir / "codex-audit-report.json").read_text())
            flags = release_report["suites"][suite]["records"][1]["flags"]
            self.assertTrue(any(flag.startswith("I:release_host_path_leak") for flag in flags))
            self.assertFalse(release_report["suites"][suite]["records"][1]["release_hygiene"]["host_path_clean"])
            self.assertEqual(release_report["release_manifest"]["path"], "[external]/manifest.json")

    def test_release_manifest_requires_proof_ready_per_suite(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)

            proof_suite = "release-candidate-history-proof"
            proof_records = []
            for i in range(1, 5):
                proof_records.append(self._release_record(proof_suite, condition="no_brain", repetition=i, run_id=f"proof-base-{i}"))
                proof_records.append(self._release_record(proof_suite, condition="full_brain", repetition=i, run_id=f"proof-brain-{i}"))
            proof_dir = self._write_records(results_dir, proof_suite, proof_records)
            (proof_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            weak_suite = "release-candidate-semantic-no-signal"
            weak_records = []
            for i in range(1, 5):
                weak_records.append(self._release_record(weak_suite, condition="no_brain", repetition=i, run_id=f"weak-base-{i}"))
                weak_records.append(self._release_record(weak_suite, condition="full_brain", repetition=i, run_id=f"weak-brain-{i}"))
            weak_dir = self._write_records(results_dir, weak_suite, weak_records)
            (weak_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "saturated/no_signal",
                    "proof_ready": False,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "saturated"},
                }]
            }))

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["proof_ready_comparisons"], 1)
            self.assertEqual(report["totals"]["hard_flags"], 0)
            self.assertIn(
                f"suite {weak_suite} has no proof_ready comparison",
                report["gate_status"]["failures"],
            )

    def test_audit_codex_fails_missing_and_inconsistent_provenance(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            legacy = self._record()
            legacy.pop("provenance")
            self._write_records(results_dir, "legacy-suite", [legacy])

            legacy_report = audit_codex.build_audit_report(results_dir, ["legacy-*"])
            self.assertEqual(legacy_report["totals"]["hard_flags"], 1)
            self.assertIn("H:provenance_missing", legacy_report["suites"]["legacy-suite"]["records"][0]["flags"])

            mismatch = self._record()
            mismatch["provenance"]["source"]["head"]["commit"] = "7" * 40
            self._write_records(results_dir, "mismatch-suite", [mismatch])
            mismatch_report = audit_codex.build_audit_report(results_dir, ["mismatch-*"])
            flags = mismatch_report["suites"]["mismatch-suite"]["records"][0]["flags"]
            self.assertIn("H:provenance_unpinned_base_head_mismatch", flags)

    def test_audit_codex_flags_proof_ready_without_stability(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            proof_records = []
            for i in range(1, 5):
                proof_records.append(self._record(condition="no_brain", repetition=i, run_id=f"base-{i}"))
                proof_records.append(self._record(condition="full_brain", repetition=i, run_id=f"brain-{i}"))
            suite_dir = self._write_records(results_dir, "proof-suite", proof_records)
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "full_brain",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "noisy"},
                }]
            }))

            report = audit_codex.build_audit_report(results_dir, ["proof-*"])
            self.assertEqual(report["totals"]["hard_flags"], 1)
            flags = report["suites"]["proof-suite"]["comparisons"][0]["flags"]
            self.assertIn("G:proof_ready_without_stable_gate", flags)
            self.assertEqual(audit_codex.main(["--results", str(results_dir), "--suite-glob", "proof-*", "--out-dir", str(out_dir), "--fail-on-flags"]), 1)


if __name__ == "__main__":
    unittest.main()
