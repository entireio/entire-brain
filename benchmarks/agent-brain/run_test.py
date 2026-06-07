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

    def test_mcp_history_audit_matches_compact_prompt_history_tool_requirement(self):
        # Compact-delivery models are told to call brain_brief ONCE and NOT brain_history;
        # the audit must not then fail them for skipping brain_history.
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="o", agent="claude", model="opus")), ("brain_brief",))
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="g", agent="codex", model="gpt-5.5")), ("brain_brief",))
        self.assertEqual(run.mcp_history_required_tools(run.RunnerSpec(id="s", agent="claude", model="sonnet")), ("brain_brief", "brain_history"))

        brief_only = {"mcp": {"enabled": True}, "activity": {"mcp_tool_calls": 1, "mcp_tool_names": ["mcp__entire_brain__brain_brief"]}}
        # Opus (compact): brief-only is a clean pass.
        opus_audit = run.mcp_condition_audit("mcp_history", brief_only, run.RunnerSpec(id="o", agent="claude", model="opus"))
        self.assertTrue(opus_audit["ok"], opus_audit)
        # Sonnet (non-compact): brief-only must still be flagged for missing brain_history.
        sonnet_audit = run.mcp_condition_audit("mcp_history", brief_only, run.RunnerSpec(id="s", agent="claude", model="sonnet"))
        self.assertFalse(sonnet_audit["ok"])
        self.assertIn("brain_history", [f.get("tool") for f in sonnet_audit["findings"]])

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
        self.assertIn("brain_history", prompt)
        self.assertIn("mcp__entire_brain__brain_brief", prompt)
        self.assertIn("before any shell search or file reads", prompt)
        self.assertIn("run exactly one `mcp__entire_brain__brain_history`", prompt)
        self.assertIn("apply the fix there before any additional MCP calls", prompt)
        self.assertIn("MCP_TOOLS_MISSING", prompt)
        self.assertIn("Do not run the `entire brain` CLI", prompt)
        self.assertIn("do not read `.benchmark/brain-history-excerpt.md`", prompt)

    def test_mcp_history_disciplined_delivery_for_gpt5x(self):
        # gpt-5.5 under-contexts on MCP (brief names the file but not the invariant, and the old
        # brief-only prompt banned brain_history). It now gets the "disciplined MCP" delivery:
        # brief once + ONE targeted brain_history for the invariant + hard stop. Validated to lift
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
        self.assertIn("brain_history", prompt)  # the invariant lookup, restored (not banned)
        self.assertIn("Hard stop", prompt)
        self.assertIn("MCP_TOOLS_MISSING", prompt)
        self.assertNotIn("do NOT need a separate `brain_history` call", prompt)  # old brief-only is gone
        # A model NOT in the disciplined/compact set keeps the generic history delivery.
        guided = run.prompt_for(task, "mcp_history", run.parse_runner_spec("claude:sonnet:medium"))
        self.assertIn("run exactly one `mcp__entire_brain__brain_history`", guided)
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
                                    "name": "mcp__entire-brain__brain_history",
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
                    "mcp_tool_names": ["mcp__entire_brain__brain_brief", "mcp__entire_brain__brain_history"],
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
            sanitization = run.sanitize_agent_worktree(repo)
            self.assertIn("benchmarks/agent-brain/tasks", sanitization["removed_paths"])
            self.assertTrue(sanitization["committed"])
            after = run.agent_secret_preflight(repo)
            self.assertTrue(after["ok"])
            self.assertFalse(task_dir.exists())

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
            (brain / "sessions").mkdir(parents=True)
            session = brain / "sessions" / "one.jsonl"
            session.write_text(
                '{"message":"useful history"}\n'
                '{"message":"benchmarks/agent-brain/tasks/secret.json contains validation"}\n'
            )
            index = brain / "history.json"
            index.write_text(json.dumps({"items": ["useful", "benchmarks/agent-brain/results/run/record.json"]}))

            summary = run.sanitize_brain_history(plugin)
            self.assertEqual(summary["files_scrubbed"], 2)
            self.assertIn("useful history", session.read_text())
            self.assertNotIn("benchmarks/agent-brain/tasks", session.read_text())
            self.assertIn("[redacted benchmark scaffold]", index.read_text())

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
                "n_condition": 3,
                "n_baseline": 3,
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
        self.assertIn("do NOT call `brain_history`", p)    # no forced history blob
        self.assertIn("finite budget", p)
        # other Claude models keep the standard MCP delivery (forced brain_history)
        son = run.prompt_for(self.TASK, "mcp_history", run.parse_runner_spec("claude:sonnet:high"))
        self.assertIn("run exactly one `mcp__entire_brain__brain_history`", son)
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


if __name__ == "__main__":
    unittest.main()
