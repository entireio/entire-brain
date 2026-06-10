import argparse
import copy
import hashlib
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

AUDIT_FACTS_EVAL_PATH = pathlib.Path(__file__).with_name("audit_facts_eval.py")
AUDIT_FACTS_EVAL_SPEC = importlib.util.spec_from_file_location("agent_brain_audit_facts_eval", AUDIT_FACTS_EVAL_PATH)
audit_facts_eval = importlib.util.module_from_spec(AUDIT_FACTS_EVAL_SPEC)
assert AUDIT_FACTS_EVAL_SPEC.loader is not None
sys.modules[AUDIT_FACTS_EVAL_SPEC.name] = audit_facts_eval
AUDIT_FACTS_EVAL_SPEC.loader.exec_module(audit_facts_eval)

AUDIT_DISTILL_PERF_PATH = pathlib.Path(__file__).with_name("audit_distill_perf.py")
AUDIT_DISTILL_PERF_SPEC = importlib.util.spec_from_file_location("agent_brain_audit_distill_perf", AUDIT_DISTILL_PERF_PATH)
audit_distill_perf = importlib.util.module_from_spec(AUDIT_DISTILL_PERF_SPEC)
assert AUDIT_DISTILL_PERF_SPEC.loader is not None
sys.modules[AUDIT_DISTILL_PERF_SPEC.name] = audit_distill_perf
AUDIT_DISTILL_PERF_SPEC.loader.exec_module(audit_distill_perf)

AUDIT_RADAR_EVIDENCE_PATH = pathlib.Path(__file__).with_name("audit_radar_evidence.py")
AUDIT_RADAR_EVIDENCE_SPEC = importlib.util.spec_from_file_location("agent_brain_audit_radar_evidence", AUDIT_RADAR_EVIDENCE_PATH)
audit_radar_evidence = importlib.util.module_from_spec(AUDIT_RADAR_EVIDENCE_SPEC)
assert AUDIT_RADAR_EVIDENCE_SPEC.loader is not None
sys.modules[AUDIT_RADAR_EVIDENCE_SPEC.name] = audit_radar_evidence
AUDIT_RADAR_EVIDENCE_SPEC.loader.exec_module(audit_radar_evidence)

AUDIT_RADAR_TOOL_PATH = pathlib.Path(__file__).with_name("audit_radar_tool_evidence.py")
AUDIT_RADAR_TOOL_SPEC = importlib.util.spec_from_file_location("agent_brain_audit_radar_tool", AUDIT_RADAR_TOOL_PATH)
audit_radar_tool_evidence = importlib.util.module_from_spec(AUDIT_RADAR_TOOL_SPEC)
assert AUDIT_RADAR_TOOL_SPEC.loader is not None
sys.modules[AUDIT_RADAR_TOOL_SPEC.name] = audit_radar_tool_evidence
AUDIT_RADAR_TOOL_SPEC.loader.exec_module(audit_radar_tool_evidence)


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
        self.assertEqual(run.condition_prep_kind("mcp_workspace_radar"), "full_brain")
        self.assertEqual(run.condition_prep_kind("mcp_semantic"), "semantic_brain")
        self.assertTrue(run.condition_writes_history_excerpt("full_cli_original"))
        self.assertFalse(run.condition_writes_history_excerpt("full_cli_compact"))
        self.assertFalse(run.condition_writes_history_excerpt("mcp_history"))
        self.assertFalse(run.condition_writes_history_excerpt("mcp_workspace_radar"))
        self.assertFalse(run.condition_copies_entire_history("no_brain"))
        self.assertTrue(run.condition_copies_entire_history("full_cli_compact"))
        self.assertTrue(run.condition_copies_entire_history("mcp_workspace_radar"))

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

    def test_workspace_radar_prep_creates_local_workspace(self):
        task = {
            "repo": "entire-cli",
            "repo_path": "cli-bench",
            "prepare_semantic": False,
            "workspace_name": "release-radar",
        }
        tools = {
            "brain": pathlib.Path("/tmp/entire-brain"),
            "entire": pathlib.Path("/tmp/entire"),
            "sem": pathlib.Path("/tmp/entire-sem"),
        }
        worktree = pathlib.Path("/tmp/worktree")
        commands = run.brain_prep_commands(task, "mcp_workspace_radar", worktree, tools, 200)
        self.assertEqual(commands[0], ["/tmp/entire-brain", "export", "--checkpoint-limit", "200", "--history-index"])
        self.assertIn(["/tmp/entire-brain", "workspace", "create", "release-radar"], commands)
        self.assertIn(
            ["/tmp/entire-brain", "workspace", "add", "release-radar", "/tmp/worktree", "--name", "entire-cli"],
            commands,
        )

    def test_workspace_radar_rejects_invalid_workspace_name(self):
        with self.assertRaisesRegex(ValueError, "invalid benchmark workspace_name"):
            run.benchmark_workspace_name({"workspace_name": "../oops"})

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

    def test_validate_materializes_validation_fixture_and_cleans_up(self):
        with tempfile.TemporaryDirectory() as tmp:
            worktree = pathlib.Path(tmp)
            task = {
                "id": "t",
                "validation_files": [
                    {
                        "path": "hidden/fixture_test.go",
                        "fixture": "entireio-cli/manual_attribution_no_trailer_realign_test.go.fixture",
                    }
                ],
                "validation": [
                    "test -f hidden/fixture_test.go && grep -q TestPostCommitNoTrailerRealignsAttributionBaseHidden hidden/fixture_test.go"
                ],
            }
            result = run.validate(task, worktree, os.environ.copy())
            self.assertTrue(result["ok"], result)
            self.assertFalse((worktree / "hidden" / "fixture_test.go").exists())

    def test_validate_restores_existing_validation_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            worktree = pathlib.Path(tmp)
            target = worktree / "hidden.txt"
            target.write_text("original")
            task = {
                "id": "t",
                "validation_files": [{"path": "hidden.txt", "content": "temporary secret"}],
                "validation": ["grep -q 'temporary secret' hidden.txt"],
            }
            result = run.validate(task, worktree, os.environ.copy())
            self.assertTrue(result["ok"], result)
            self.assertEqual(target.read_text(), "original")

    def test_transient_agent_failure_reason_only_matches_infra_failures(self):
        self.assertEqual(
            run.transient_agent_failure_reason(
                1,
                '{"type":"error","message":"Selected model is at capacity. Please try a different model."}',
                "",
            ),
            "selected_model_at_capacity",
        )
        self.assertEqual(
            run.transient_agent_failure_reason(1, "", "request was rate limited upstream"),
            "rate_limited",
        )
        self.assertIsNone(run.transient_agent_failure_reason(0, "Selected model is at capacity", ""))
        self.assertIsNone(run.transient_agent_failure_reason(1, "validation failed", ""))

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
        for tool in ("brain_regressions", "brain_review", "brain_workspace_regressions", "brain_workspace_review"):
            self.assertIn(tool, joined)
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
        for tool in ("brain_regressions", "brain_review", "brain_workspace_regressions", "brain_workspace_review"):
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

    def test_activity_preserves_safe_mcp_boolean_arguments_only(self):
        stdout = json.dumps(
            {
                "type": "item.completed",
                "item": {
                    "type": "mcp_tool_call",
                    "server": "entire_brain",
                    "tool": "brain_regressions",
                    "arguments": {"query": "secret query text", "location_only": True, "include_deletions": False},
                    "status": "completed",
                },
            }
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["mcp_tool_calls"], 1)
        self.assertEqual(activity["mcp_tool_details"][0]["arguments"], {"include_deletions": False, "location_only": True})
        self.assertNotIn("secret query text", json.dumps(activity["mcp_tool_details"]))

    def test_radar_mcp_history_audit_requires_brain_regressions(self):
        # Under a radar delivery, the agent calls brain_regressions (not brain_brief). The audit's
        # required-tool floor must follow suit, or a correct radar run is mis-flagged as a failure.
        os.environ["BENCH_RADAR_LOCATION_ONLY"] = "1"
        try:
            runner = run.RunnerSpec(id="o", agent="claude", model="opus")
            agent_info = {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
                    "mcp_tool_details": [{"name": "mcp__entire_brain__brain_regressions", "arguments": {"location_only": True}, "errored": False}],
                },
            }
            audit = run.mcp_condition_audit("mcp_history", agent_info, runner)
            self.assertTrue(audit["ok"], audit)
            self.assertEqual(run.mcp_history_required_tools(runner), ("brain_regressions",))
            self.assertEqual(run.mcp_required_tools("mcp_history", runner), ("brain_regressions",))
            missing_args = {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
                    "mcp_tool_details": [{"name": "mcp__entire_brain__brain_regressions", "arguments": {}, "errored": False}],
                },
            }
            missing_audit = run.mcp_condition_audit("mcp_history", missing_args, runner)
            self.assertFalse(missing_audit["ok"])
            self.assertIn("missing_required_mcp_argument", [finding["kind"] for finding in missing_audit["findings"]])
        finally:
            del os.environ["BENCH_RADAR_LOCATION_ONLY"]

    def test_radar_mcp_history_audit_requires_deletion_argument_for_deletion_tasks(self):
        os.environ["BENCH_RADAR_LOCATION_ONLY"] = "1"
        task = {"radar_include_deletions": True}
        runner = run.parse_runner_spec("codex:gpt-5.4-mini:low")
        try:
            missing_deletions = {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
                    "mcp_tool_details": [
                        {
                            "name": "mcp__entire_brain__brain_regressions",
                            "arguments": {"location_only": True},
                            "errored": False,
                        }
                    ],
                },
            }
            audit = run.mcp_condition_audit("mcp_history", missing_deletions, runner, task)
            self.assertFalse(audit["ok"])
            self.assertIn("include_deletions", [finding.get("argument") for finding in audit["findings"]])

            ok_info = copy.deepcopy(missing_deletions)
            ok_info["activity"]["mcp_tool_details"][0]["arguments"]["include_deletions"] = True
            ok = run.mcp_condition_audit("mcp_history", ok_info, runner, task)
            self.assertTrue(ok["ok"], ok)
        finally:
            del os.environ["BENCH_RADAR_LOCATION_ONLY"]

    def test_radar_prompt_can_request_deletion_signals(self):
        task = {
            "id": "task",
            "prompt": "Fix the transcript regression.",
            "brain_queries": ["resolveTranscriptPath", "TranscriptPath"],
            "radar_include_deletions": True,
        }
        os.environ["BENCH_RADAR_LOCATION_ONLY"] = "1"
        try:
            prompt = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.4-mini:medium"))
        finally:
            del os.environ["BENCH_RADAR_LOCATION_ONLY"]
        self.assertIn("brain_regressions", prompt)
        self.assertIn("location_only: true", prompt)
        self.assertIn("include_deletions", prompt)
        self.assertIn("deleted assignments", prompt)
        self.assertIn("symbol", prompt)
        self.assertIn("related_locations", prompt)

    def test_manual_attribution_radar_task_declares_deletion_signals(self):
        task_path = RUN_PATH.with_name("tasks") / "entireio-cli-manual-commit-attribution-base.json"
        task = json.loads(task_path.read_text())
        self.assertIs(task.get("radar_include_deletions"), True)
        removed = "\n".join(str(rep.get("old", "")) for rep in task.get("setup_replacements", []))
        self.assertIn("RealignAttributionBase", removed)
        os.environ["BENCH_RADAR_LOCATION_ONLY"] = "1"
        try:
            prompt = run.prompt_for(task, "mcp_history", run.parse_runner_spec("codex:gpt-5.4-mini:medium"))
        finally:
            del os.environ["BENCH_RADAR_LOCATION_ONLY"]
        self.assertIn("include_deletions: true", prompt)

    def test_workspace_radar_prompt_uses_workspace_regressions(self):
        task = {
            "id": "task",
            "prompt": "Fix the cross-repo regression.",
            "brain_queries": ["scopeBaseRef", "base scope"],
            "workspace_name": "related",
            "radar_include_deletions": True,
        }
        prompt = run.prompt_for(task, "mcp_workspace_radar", run.parse_runner_spec("codex:gpt-5.4-mini:medium"))
        self.assertIn("brain_workspace_regressions", prompt)
        self.assertIn('workspace: "related"', prompt)
        self.assertIn("location_only: true", prompt)
        self.assertIn("include_deletions: true", prompt)
        self.assertIn("symbol", prompt)
        self.assertIn("related_locations", prompt)
        self.assertIn("WORKSPACE_RADAR_NO_FINDINGS", prompt)

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
        workspace_ok = run.mcp_condition_audit(
            "mcp_workspace_radar",
            {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_workspace_regressions"],
                    "mcp_tool_details": [{"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {"location_only": True}, "errored": False}],
                    "direct_brain_cli_calls": 0,
                },
            },
        )
        self.assertTrue(workspace_ok["ok"], workspace_ok)
        workspace_deletion_missing = run.mcp_condition_audit(
            "mcp_workspace_radar",
            {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_workspace_regressions"],
                    "mcp_tool_details": [{"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {"location_only": True}, "errored": False}],
                    "direct_brain_cli_calls": 0,
                },
            },
            task={"radar_include_deletions": True},
        )
        self.assertFalse(workspace_deletion_missing["ok"])
        self.assertIn("include_deletions", [finding.get("argument") for finding in workspace_deletion_missing["findings"]])
        workspace_missing = run.mcp_condition_audit(
            "mcp_workspace_radar",
            {
                "mcp": {"enabled": True},
                "activity": {
                    "mcp_tool_calls": 1,
                    "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
                    "direct_brain_cli_calls": 0,
                },
            },
        )
        self.assertFalse(workspace_missing["ok"])
        self.assertIn("brain_workspace_regressions", [f.get("tool") for f in workspace_missing["findings"]])
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
        self.assertIn("missing_required_mcp_tool", {finding["kind"] for finding in missing_history["findings"]})

    def test_text_activity_is_marked_as_fallback(self):
        activity = run.extract_agent_activity("I would run rg needle and entire brain brief.", "")
        self.assertEqual(activity["activity_source"], "text_fallback")
        self.assertEqual(activity["structured_tool_event_count"], 0)

    def test_text_fallback_counts_current_mcp_tool_names(self):
        stdout = "\n".join(
            [
                "mcp__entire_brain__brain_search",
                "mcp__entire_brain__brain_vsearch",
                "mcp__entire_brain__brain_get",
                "mcp__entire_brain__brain_multi_get",
                "mcp__entire_brain__brain_regressions",
                "mcp__entire_brain__brain_workspace_regressions",
            ]
        )
        activity = run.extract_agent_activity(stdout, "")
        self.assertEqual(activity["activity_source"], "text_fallback")
        self.assertEqual(activity["mcp_tool_calls"], 6)
        for tool in ("brain_search", "brain_vsearch", "brain_get", "brain_multi_get", "brain_regressions", "brain_workspace_regressions"):
            self.assertIn(f"mcp__entire_brain__{tool}", activity["mcp_tool_names"])

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
                "radar_include_deletions": True,
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
            self.assertIs(prov["task"]["radar_include_deletions"], True)
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

    def test_release_panel_preflight_requires_harness_hygiene(self):
        errors = run.panel_preflight(
            {
                "name": "release-missing-hygiene",
                "runners": ["codex:gpt-test:low"],
                "tasks": ["entire-brain-query-default-limit.json"],
                "conditions": ["no_brain", "semantic_brain"],
                "repetitions": 4,
            }
        )
        joined = " | ".join(errors)
        self.assertIn("must hide benchmarks/agent-brain", joined)

        task_path = run.TASK_DIR / "release-hygiene-fixture.json"
        try:
            task_path.write_text(json.dumps({
                "id": "release-hygiene-fixture",
                "repo": "entire-brain",
                "repo_path": "entire-brain",
                "conditions": ["no_brain", "semantic_brain"],
                "prompt": "Fix the regression.",
                "hide_validation_from_agent": True,
                "validation": ["go test ./internal/cli -run TestHiddenReleaseFixture"],
                "agent_hidden_paths": ["benchmarks/agent-brain"],
            }))
            errors = run.panel_preflight(
                {
                    "name": "release-missing-canary",
                    "runners": ["codex:gpt-test:low"],
                    "tasks": [task_path.name],
                    "conditions": ["no_brain", "semantic_brain"],
                    "repetitions": 4,
                }
            )
        finally:
            task_path.unlink(missing_ok=True)
        self.assertIn("has no explicit leak_markers canary", " | ".join(errors))

    def test_committed_release_panels_pass_preflight(self):
        for path in sorted((run.BENCH_ROOT / "panels").glob("release-*.json")):
            panel = json.loads(path.read_text())
            self.assertEqual(run.panel_preflight(panel), [], path.name)

    def test_panel_preflight_validates_env_values(self):
        panel = {
            "name": "release-env-fixture",
            "runners": ["codex:gpt-test:low"],
            "tasks": ["entire-brain-history-codex-schema-contract.json"],
            "conditions": ["no_brain", "full_brain"],
            "repetitions": 4,
            "env": {"BENCH_RADAR_LOCATION_ONLY": "1"},
        }
        self.assertEqual(run.panel_preflight(panel), [])

        bad = dict(panel)
        bad["env"] = {"BENCH_RADAR_LOCATION_ONLY": 1}
        self.assertIn("panel env keys and values must be strings", " | ".join(run.panel_preflight(bad)))

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

    def _write_mcp_server_log(
        self,
        suite_dir: pathlib.Path,
        run_id: str,
        tool: str | None = None,
        *,
        tool_args: dict | None = None,
        response: bool = True,
        result_status: str | None = "ok",
    ) -> None:
        run_dir = suite_dir / run_id
        run_dir.mkdir(exist_ok=True)
        lines = [
            "start",
            "message: initialize",
            "response: initialize",
            "message: tools/list",
            "response: tools/list",
            "message: tools/call",
        ]
        if tool:
            lines.append(f"tool: {tool}")
        if tool and tool_args is not None:
            lines.append("tool_args: " + json.dumps(tool_args, sort_keys=True, separators=(",", ":")))
        if response:
            lines.append("response: tools/call")
        if tool and response and result_status:
            lines.append(f"tool_result: {tool} {result_status}")
        (run_dir / "mcp-server.log").write_text("\n".join(lines) + "\n")

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
                "radar_include_deletions": False,
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
                "entire": {"sha256": self.TOOL_SHA},
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

    def _mcp_release_record(self, suite: str, *, repetition: int, run_id: str) -> dict:
        record = self._release_record(suite, condition="mcp_history", repetition=repetition, run_id=run_id)
        record["provenance"]["run_config"]["env_flags"] = {"BENCH_RADAR_LOCATION_ONLY": "1"}
        record["agent_info"]["activity"].update({
            "used_brain": True,
            "mcp_tool_calls": 1,
            "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
            "mcp_tool_details": [{"name": "mcp__entire_brain__brain_regressions", "arguments": {"location_only": True}, "errored": False}],
        })
        record["mcp_condition_audit"] = {
            "ok": True,
            "required": True,
            "mcp_tool_calls": 1,
            "mcp_tool_names": ["mcp__entire_brain__brain_regressions"],
            "mcp_tool_details": [{"name": "mcp__entire_brain__brain_regressions", "arguments": {"location_only": True}, "errored": False}],
            "direct_brain_cli_calls": 0,
            "findings": [],
        }
        return record

    def _generic_mcp_release_record(self, suite: str, *, repetition: int, run_id: str) -> dict:
        record = self._release_record(suite, condition="mcp_history", repetition=repetition, run_id=run_id)
        names = ["mcp__entire_brain__brain_brief", "mcp__entire_brain__brain_search"]
        details = [{"name": name, "arguments": {}, "errored": False} for name in names]
        record["agent_info"]["activity"].update({
            "used_brain": True,
            "mcp_tool_calls": 2,
            "mcp_tool_names": names,
            "mcp_tool_details": details,
        })
        record["mcp_condition_audit"] = {
            "ok": True,
            "required": True,
            "mcp_tool_calls": 2,
            "mcp_tool_names": names,
            "mcp_tool_details": details,
            "direct_brain_cli_calls": 0,
            "findings": [],
        }
        return record

    def _workspace_mcp_release_record(self, suite: str, *, repetition: int, run_id: str) -> dict:
        record = self._release_record(suite, condition="mcp_workspace_radar", repetition=repetition, run_id=run_id)
        record["agent_info"]["activity"].update({
            "used_brain": True,
            "mcp_tool_calls": 1,
            "mcp_tool_names": ["mcp__entire_brain__brain_workspace_regressions"],
            "mcp_tool_details": [{"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {"location_only": True}, "errored": False}],
        })
        record["mcp_condition_audit"] = {
            "ok": True,
            "required": True,
            "mcp_tool_calls": 1,
            "mcp_tool_names": ["mcp__entire_brain__brain_workspace_regressions"],
            "mcp_tool_details": [{"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {"location_only": True}, "errored": False}],
            "direct_brain_cli_calls": 0,
            "findings": [],
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
            "required_proof_scopes": ["history"],
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
            self.assertEqual(passing["totals"]["proof_ready_comparisons_by_scope"], {"history": 1})
            self.assertEqual(passing["release_manifest"]["required_proof_scopes"], ["history"])
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

    def test_release_manifest_requires_configured_proof_scope(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["required_proof_scopes"] = ["semantic"]
            manifest.write_text(json.dumps(manifest_data))

            suite = "release-candidate-history-only"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                records.append(self._release_record(suite, condition="full_brain", repetition=i, run_id=f"brain-{i}"))
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

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["proof_ready_comparisons_by_scope"], {"history": 1})
            self.assertIn(
                "proof_ready_comparisons[semantic] 0 < required 1",
                report["gate_status"]["failures"],
            )

    def test_release_manifest_requires_mcp_verified_records(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["minimums"]["mcp_verified_records"] = 1
            manifest.write_text(json.dumps(manifest_data))

            suite = "release-candidate-history-no-mcp"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                records.append(self._release_record(suite, condition="full_brain", repetition=i, run_id=f"brain-{i}"))
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

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["mcp_verified_records"], 0)
            self.assertIn(
                "mcp_verified_records 0 < required 1",
                report["gate_status"]["failures"],
            )

    def test_audit_codex_counts_radar_delivery_scope_and_mcp_verified_records(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["required_proof_scopes"] = ["mcp_radar_location_only"]
            manifest_data["minimums"]["mcp_verified_records"] = 4
            manifest.write_text(json.dumps(manifest_data))

            suite = "release-candidate-radar-mcp"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                records.append(self._mcp_release_record(suite, repetition=i, run_id=f"radar-{i}"))
            suite_dir = self._write_records(results_dir, suite, records)
            for i in range(1, 5):
                self._write_mcp_server_log(suite_dir, f"radar-{i}", "brain_regressions", tool_args={"location_only": True})
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_history",
                    "delivery_scope": "mcp_radar_location_only",
                    "env_flags": {"BENCH_RADAR_LOCATION_ONLY": "1"},
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
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["mcp_verified_records"], 4)
            self.assertEqual(report["totals"]["mcp_named_tool_verified_records"], 4)
            self.assertEqual(report["totals"]["mcp_named_tool_completed_records"], 4)
            self.assertEqual(report["totals"]["proof_ready_comparisons_by_scope"], {"mcp_radar_location_only": 1})
            self.assertEqual(report["totals"]["named_tool_proof_ready_comparisons_by_scope"], {"mcp_radar_location_only": 1})
            self.assertEqual(report["gate_status"]["requirements"]["min_mcp_verified"], 4)

    def test_release_manifest_can_require_named_mcp_tool_records(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["required_proof_scopes"] = ["mcp"]
            manifest_data["minimums"]["mcp_verified_records"] = 4
            manifest_data["minimums"]["mcp_named_tool_verified_records"] = 1
            manifest.write_text(json.dumps(manifest_data))

            suite = "release-candidate-legacy-generic-mcp"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                records.append(self._generic_mcp_release_record(suite, repetition=i, run_id=f"mcp-{i}"))
            suite_dir = self._write_records(results_dir, suite, records)
            for i in range(1, 5):
                # Legacy retained MCP logs prove tools/call happened, but do not name
                # the specific brain tool handled by the server.
                self._write_mcp_server_log(suite_dir, f"mcp-{i}")
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_history",
                    "delivery_scope": "mcp",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["mcp_verified_records"], 4)
            self.assertEqual(report["totals"]["mcp_named_tool_verified_records"], 0)
            self.assertEqual(report["totals"]["mcp_named_tool_completed_records"], 0)
            self.assertIn(
                "mcp_named_tool_verified_records 0 < required 1",
                report["gate_status"]["failures"],
            )

    def test_release_manifest_requires_named_mcp_tool_proof_in_configured_scope(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["required_proof_scopes"] = ["mcp", "mcp_radar_location_only"]
            manifest_data["required_named_tool_proof_scopes"] = ["mcp"]
            manifest_data["minimums"]["mcp_verified_records"] = 8
            manifest_data["minimums"]["mcp_named_tool_verified_records"] = 4
            manifest.write_text(json.dumps(manifest_data))

            generic_suite = "release-candidate-legacy-generic-mcp"
            generic_records = []
            for i in range(1, 5):
                generic_records.append(self._release_record(generic_suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                generic_records.append(self._generic_mcp_release_record(generic_suite, repetition=i, run_id=f"mcp-{i}"))
            generic_dir = self._write_records(results_dir, generic_suite, generic_records)
            for i in range(1, 5):
                self._write_mcp_server_log(generic_dir, f"mcp-{i}")
            (generic_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_history",
                    "delivery_scope": "mcp",
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            radar_suite = "release-candidate-radar-mcp"
            radar_records = []
            for i in range(1, 5):
                radar_records.append(self._release_record(radar_suite, condition="no_brain", repetition=i, run_id=f"radar-base-{i}"))
                radar_records.append(self._mcp_release_record(radar_suite, repetition=i, run_id=f"radar-{i}"))
            radar_dir = self._write_records(results_dir, radar_suite, radar_records)
            for i in range(1, 5):
                self._write_mcp_server_log(radar_dir, f"radar-{i}", "brain_regressions", tool_args={"location_only": True})
            (radar_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_history",
                    "delivery_scope": "mcp_radar_location_only",
                    "env_flags": {"BENCH_RADAR_LOCATION_ONLY": "1"},
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                1,
            )
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["mcp_verified_records"], 8)
            self.assertEqual(report["totals"]["mcp_named_tool_verified_records"], 4)
            self.assertEqual(report["totals"]["mcp_named_tool_completed_records"], 4)
            self.assertEqual(report["totals"]["proof_ready_comparisons_by_scope"], {"mcp": 1, "mcp_radar_location_only": 1})
            self.assertEqual(report["totals"]["named_tool_proof_ready_comparisons_by_scope"], {"mcp_radar_location_only": 1})
            self.assertIn(
                "named_tool_proof_ready_comparisons[mcp] 0 < required 1",
                report["gate_status"]["failures"],
            )

            manifest_data["required_named_tool_proof_scopes"] = ["mcp_radar_location_only"]
            manifest.write_text(json.dumps(manifest_data))
            self.assertEqual(
                audit_codex.main(["--results", str(results_dir), "--release-manifest", str(manifest), "--out-dir", str(out_dir), "--fail-on-flags"]),
                0,
            )
            passing = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(passing["gate_status"]["requirements"]["required_named_tool_proof_scopes"], ["mcp_radar_location_only"])

    def test_audit_codex_radar_backing_requires_named_tool_records(self):
        comp = {
            "task": "t",
            "runner": "codex",
            "condition": "mcp_history",
            "delivery_scope": "mcp_radar_location_only",
            "n_condition": 4,
            "n_baseline": 4,
        }
        rec_audits = []
        for i in range(1, 5):
            rec_audits.append({
                "pass": True,
                "ok": False,
                "valid": False,
                "provenance": {"ok": True},
                "task_id": "t",
                "runner": "codex",
                "condition": "no_brain",
                "run_id": f"base-{i}",
                "repetition": i,
            })
            rec_audits.append({
                "pass": True,
                "ok": True,
                "valid": True,
                "provenance": {"ok": True},
                "task_id": "t",
                "runner": "codex",
                "condition": "mcp_history",
                "delivery_scope": "mcp_radar_location_only",
                "run_id": f"radar-{i}",
                "repetition": i,
                "mcp_verified": True,
                "mcp_named_tool_verified": False,
                "mcp_named_tool_completed": False,
            })

        backing = audit_codex.proof_ready_record_backing(comp, rec_audits)

        self.assertFalse(backing["ok"], backing)
        self.assertTrue(backing["condition_mcp_verified_ok"], backing)
        self.assertTrue(backing["condition_requires_mcp_named_tool_verified"], backing)
        self.assertFalse(backing["condition_mcp_named_tool_verified_ok"], backing)
        self.assertFalse(backing["condition_mcp_named_tool_completed_ok"], backing)
        self.assertFalse(backing["condition_mcp_named_tool_required_ok"], backing)

    def test_audit_codex_radar_backing_requires_completed_named_tool_records(self):
        comp = {
            "task": "t",
            "runner": "codex",
            "condition": "mcp_history",
            "delivery_scope": "mcp_radar_location_only",
            "n_condition": 4,
            "n_baseline": 4,
        }
        rec_audits = []
        for i in range(1, 5):
            rec_audits.append({
                "pass": True,
                "ok": False,
                "valid": False,
                "provenance": {"ok": True},
                "task_id": "t",
                "runner": "codex",
                "condition": "no_brain",
                "run_id": f"base-{i}",
                "repetition": i,
            })
            rec_audits.append({
                "pass": True,
                "ok": True,
                "valid": True,
                "provenance": {"ok": True},
                "task_id": "t",
                "runner": "codex",
                "condition": "mcp_history",
                "delivery_scope": "mcp_radar_location_only",
                "run_id": f"radar-{i}",
                "repetition": i,
                "mcp_verified": True,
                "mcp_named_tool_verified": True,
                "mcp_named_tool_completed": False,
            })

        backing = audit_codex.proof_ready_record_backing(comp, rec_audits)

        self.assertFalse(backing["ok"], backing)
        self.assertTrue(backing["condition_mcp_verified_ok"], backing)
        self.assertTrue(backing["condition_mcp_named_tool_verified_ok"], backing)
        self.assertFalse(backing["condition_mcp_named_tool_completed_ok"], backing)
        self.assertFalse(backing["condition_mcp_named_tool_required_ok"], backing)

    def test_audit_codex_requires_mcp_verified_records_for_mcp_comparison_backing(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-radar-unverified-mcp"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                mcp = self._mcp_release_record(suite, repetition=i, run_id=f"radar-{i}")
                mcp["mcp_condition_audit"]["ok"] = False
                mcp["mcp_condition_audit"]["findings"] = [{"kind": "not_really_mcp_verified"}]
                records.append(mcp)
            suite_dir = self._write_records(results_dir, suite, records)
            for i in range(1, 5):
                self._write_mcp_server_log(suite_dir, f"radar-{i}", "brain_regressions", tool_args={"location_only": True})
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_history",
                    "delivery_scope": "mcp_radar_location_only",
                    "env_flags": {"BENCH_RADAR_LOCATION_ONLY": "1"},
                    "verdict": "brain_positive",
                    "proof_ready": True,
                    "n_condition": 4,
                    "n_baseline": 4,
                    "stability": {"tag": "brain_positive_stable"},
                }]
            }))

            report = audit_codex.build_audit_report(results_dir, [suite])
            comparison = report["suites"][suite]["comparisons"][0]
            self.assertFalse(comparison["pass"], comparison)
            self.assertEqual(report["totals"]["proof_ready_comparisons"], 0)
            self.assertFalse(comparison["record_backing"]["condition_mcp_verified_ok"])
            self.assertEqual(comparison["record_backing"]["condition_mcp_verified_records"], 0)
            self.assertIn("G:proof_ready_without_mcp_verified_condition_records", comparison["flags"])

    def test_audit_codex_requires_mcp_server_response_for_verified_record(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-radar-no-response"
            record = self._mcp_release_record(suite, repetition=1, run_id="radar-1")
            suite_dir = self._write_records(results_dir, suite, [record])
            self._write_mcp_server_log(suite_dir, "radar-1", "brain_regressions", tool_args={"location_only": True}, response=False)

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]

            self.assertFalse(audited["pass"], audited)
            self.assertFalse(audited["mcp_verified"], audited)
            self.assertFalse(audited["mcp_named_tool_completed"], audited)
            self.assertIn("B:mcp_server_log_missing_tool_responses", ",".join(audited["flags"]))

    def test_audit_codex_requires_named_tool_result_for_radar_completion(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-radar-no-tool-result"
            record = self._mcp_release_record(suite, repetition=1, run_id="radar-1")
            suite_dir = self._write_records(results_dir, suite, [record])
            self._write_mcp_server_log(suite_dir, "radar-1", "brain_regressions", tool_args={"location_only": True}, result_status="")

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]

            self.assertFalse(audited["pass"], audited)
            self.assertFalse(audited["mcp_verified"], audited)
            self.assertFalse(audited["mcp_named_tool_completed"], audited)
            self.assertFalse(audited["mcp_named_tool_result_verified"], audited)
            self.assertIn("B:mcp_required_tool_results_missing(brain_regressions)", audited["flags"])

    def test_audit_codex_requires_include_deletions_for_deletion_radar_task(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            task_path = results_dir / "deletion-radar-task.json"
            task_path.write_text(json.dumps({"id": "t", "radar_include_deletions": True}))

            missing_suite = "release-candidate-radar-deletions-missing"
            missing = self._mcp_release_record(missing_suite, repetition=1, run_id="radar-1")
            missing["provenance"]["task"]["path"] = str(task_path)
            missing["provenance"]["task"]["radar_include_deletions"] = True
            missing_dir = self._write_records(results_dir, missing_suite, [missing])
            self._write_mcp_server_log(missing_dir, "radar-1", "brain_regressions", tool_args={"location_only": True})
            missing_report = audit_codex.build_audit_report(results_dir, [missing_suite])
            missing_record = missing_report["suites"][missing_suite]["records"][0]
            self.assertFalse(missing_record["pass"], missing_record)
            self.assertFalse(missing_record["mcp_verified"], missing_record)
            self.assertIn("B:mcp_radar_missing_include_deletions", missing_record["flags"])
            self.assertIn("B:mcp_radar_server_missing_include_deletions", missing_record["flags"])

            ok_suite = "release-candidate-radar-deletions-ok"
            ok = self._mcp_release_record(ok_suite, repetition=1, run_id="radar-1")
            ok["provenance"]["task"]["path"] = str(task_path)
            ok["provenance"]["task"]["radar_include_deletions"] = True
            ok["agent_info"]["activity"]["mcp_tool_details"][0]["arguments"]["include_deletions"] = True
            ok["mcp_condition_audit"]["mcp_tool_details"][0]["arguments"]["include_deletions"] = True
            ok_dir = self._write_records(results_dir, ok_suite, [ok])
            self._write_mcp_server_log(ok_dir, "radar-1", "brain_regressions", tool_args={"include_deletions": True, "location_only": True})
            ok_report = audit_codex.build_audit_report(results_dir, [ok_suite])
            ok_record = ok_report["suites"][ok_suite]["records"][0]
            self.assertTrue(ok_record["pass"], ok_record)
            self.assertTrue(ok_record["mcp_verified"], ok_record)
            self.assertEqual(ok_record["server_tool_args"], [{"tool": "brain_regressions", "arguments": {"include_deletions": True, "location_only": True}}])

    def test_audit_codex_requires_required_args_and_success_on_same_server_call(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-radar-mixed-server-calls"
            record = self._mcp_release_record(suite, repetition=1, run_id="radar-1")
            record["provenance"]["task"]["radar_include_deletions"] = True
            record["agent_info"]["activity"]["mcp_tool_calls"] = 2
            record["agent_info"]["activity"]["mcp_tool_details"] = [
                {"name": "mcp__entire_brain__brain_regressions", "arguments": {"include_deletions": True, "location_only": True}, "errored": False},
                {"name": "mcp__entire_brain__brain_regressions", "arguments": {}, "errored": False},
            ]
            record["mcp_condition_audit"]["mcp_tool_calls"] = 2
            record["mcp_condition_audit"]["mcp_tool_details"] = copy.deepcopy(record["agent_info"]["activity"]["mcp_tool_details"])
            suite_dir = self._write_records(results_dir, suite, [record])
            run_dir = suite_dir / "radar-1"
            run_dir.mkdir(exist_ok=True)
            run_dir.joinpath("mcp-server.log").write_text(
                "\n".join([
                    "start",
                    "message: tools/call",
                    "response: tools/call",
                    "tool: brain_regressions",
                    'tool_args: {"include_deletions":true,"location_only":true}',
                    "tool_result: brain_regressions error",
                    "message: tools/call",
                    "response: tools/call",
                    "tool: brain_regressions",
                    "tool_result: brain_regressions ok",
                ]) + "\n"
            )

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]

            self.assertFalse(audited["pass"], audited)
            self.assertFalse(audited["mcp_verified"], audited)
            self.assertFalse(audited["mcp_named_tool_result_verified"], audited)
            self.assertIn("B:mcp_required_tool_results_not_ok(brain_regressions)", audited["flags"])

    def test_audit_codex_requires_workspace_required_args_and_success_on_same_server_call(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-workspace-radar-mixed-server-calls"
            record = self._workspace_mcp_release_record(suite, repetition=1, run_id="workspace-radar-1")
            record["provenance"]["task"]["radar_include_deletions"] = True
            record["agent_info"]["activity"]["mcp_tool_calls"] = 2
            record["agent_info"]["activity"]["mcp_tool_details"] = [
                {"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {"include_deletions": True, "location_only": True}, "errored": False},
                {"name": "mcp__entire_brain__brain_workspace_regressions", "arguments": {}, "errored": False},
            ]
            record["mcp_condition_audit"]["mcp_tool_calls"] = 2
            record["mcp_condition_audit"]["mcp_tool_details"] = copy.deepcopy(record["agent_info"]["activity"]["mcp_tool_details"])
            suite_dir = self._write_records(results_dir, suite, [record])
            run_dir = suite_dir / "workspace-radar-1"
            run_dir.mkdir(exist_ok=True)
            run_dir.joinpath("mcp-server.log").write_text(
                "\n".join([
                    "start",
                    "message: tools/call",
                    "response: tools/call",
                    "tool: brain_workspace_regressions",
                    'tool_args: {"include_deletions":true,"location_only":true}',
                    "tool_result: brain_workspace_regressions error",
                    "message: tools/call",
                    "response: tools/call",
                    "tool: brain_workspace_regressions",
                    "tool_result: brain_workspace_regressions ok",
                ]) + "\n"
            )

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]

            self.assertFalse(audited["pass"], audited)
            self.assertFalse(audited["mcp_verified"], audited)
            self.assertFalse(audited["mcp_named_tool_result_verified"], audited)
            self.assertIn("B:mcp_required_tool_results_not_ok(brain_workspace_regressions)", audited["flags"])

    def test_audit_codex_flags_unsafe_server_tool_args_without_leaking_values(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = "release-candidate-radar-unsafe-server-args"
            record = self._mcp_release_record(suite, repetition=1, run_id="radar-1")
            suite_dir = self._write_records(results_dir, suite, [record])
            self._write_mcp_server_log(
                suite_dir,
                "radar-1",
                "brain_regressions",
                tool_args={"location_only": True, "query": "release-secret", "include_deletions": "true"},
            )

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]
            serialized = json.dumps(audited, sort_keys=True)

            self.assertFalse(audited["pass"], audited)
            self.assertFalse(audited["mcp_verified"], audited)
            self.assertIn("E:mcp_server_log_unsafe_tool_args(include_deletions,query)", audited["flags"])
            self.assertIn({"tool": "brain_regressions", "arguments": {"location_only": True}}, audited["server_tool_args"])
            self.assertNotIn("release-secret", serialized)

    def test_audit_codex_requires_embedded_radar_deletion_policy(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            task_path = results_dir / "deletion-radar-task.json"
            task_path.write_text(json.dumps({"id": "t", "radar_include_deletions": True}))

            suite = "release-candidate-radar-deletions-policy-missing"
            record = self._mcp_release_record(suite, repetition=1, run_id="radar-1")
            record["provenance"]["task"]["path"] = str(task_path)
            record["provenance"]["task"].pop("radar_include_deletions", None)
            suite_dir = self._write_records(results_dir, suite, [record])
            self._write_mcp_server_log(suite_dir, "radar-1", "brain_regressions", tool_args={"location_only": True})

            report = audit_codex.build_audit_report(results_dir, [suite])
            audited = report["suites"][suite]["records"][0]

            self.assertFalse(audited["pass"], audited)
            self.assertIn("H:provenance_missing_radar_include_deletions_policy", audited["flags"])
            self.assertNotIn("B:mcp_radar_missing_include_deletions", audited["flags"])

    def test_audit_codex_counts_workspace_radar_delivery_scope(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            manifest = self._write_release_manifest(out_dir)
            manifest_data = json.loads(manifest.read_text())
            manifest_data["required_proof_scopes"] = ["mcp_workspace_radar_location_only"]
            manifest_data["minimums"]["mcp_verified_records"] = 4
            manifest.write_text(json.dumps(manifest_data))

            suite = "release-candidate-workspace-radar-mcp"
            records = []
            for i in range(1, 5):
                records.append(self._release_record(suite, condition="no_brain", repetition=i, run_id=f"base-{i}"))
                records.append(self._workspace_mcp_release_record(suite, repetition=i, run_id=f"workspace-radar-{i}"))
            suite_dir = self._write_records(results_dir, suite, records)
            for i in range(1, 5):
                self._write_mcp_server_log(suite_dir, f"workspace-radar-{i}", "brain_workspace_regressions", tool_args={"location_only": True})
            (suite_dir / "summary.json").write_text(json.dumps({
                "comparisons": [{
                    "task_id": "t",
                    "runner": "codex",
                    "condition": "mcp_workspace_radar",
                    "delivery_scope": "mcp_workspace_radar_location_only",
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
            report = json.loads((out_dir / "codex-audit-report.json").read_text())
            self.assertEqual(report["totals"]["mcp_verified_records"], 4)
            self.assertEqual(report["totals"]["proof_ready_comparisons_by_scope"], {"mcp_workspace_radar_location_only": 1})

    def test_audit_codex_cross_checks_named_mcp_server_log(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)

            good_suite = "named-log-suite"
            good = self._mcp_release_record(good_suite, repetition=1, run_id="radar-1")
            good_dir = self._write_records(results_dir, good_suite, [good])
            self._write_mcp_server_log(good_dir, "radar-1", "brain_regressions", tool_args={"location_only": True})
            good_report = audit_codex.build_audit_report(results_dir, ["named-log-*"])
            good_record = good_report["suites"][good_suite]["records"][0]
            self.assertTrue(good_record["pass"], good_record)
            self.assertTrue(good_record["mcp_verified"], good_record)
            self.assertEqual(good_record["server_tool_names"], ["brain_regressions"])
            self.assertEqual(good_record["server_tool_args"], [{"tool": "brain_regressions", "arguments": {"location_only": True}}])

            bad_suite = "mismatched-log-suite"
            bad = self._mcp_release_record(bad_suite, repetition=1, run_id="radar-1")
            bad_dir = self._write_records(results_dir, bad_suite, [bad])
            self._write_mcp_server_log(bad_dir, "radar-1", "brain_brief")
            bad_report = audit_codex.build_audit_report(results_dir, ["mismatched-log-*"])
            bad_record = bad_report["suites"][bad_suite]["records"][0]
            self.assertFalse(bad_record["pass"], bad_record)
            self.assertFalse(bad_record["mcp_verified"], bad_record)
            self.assertIn("B:mcp_tool_names_not_in_server_log(brain_regressions)", bad_record["flags"])

            missing_suite = "missing-log-suite"
            missing = self._mcp_release_record(missing_suite, repetition=1, run_id="radar-1")
            self._write_records(results_dir, missing_suite, [missing])
            missing_report = audit_codex.build_audit_report(results_dir, ["missing-log-*"])
            missing_record = missing_report["suites"][missing_suite]["records"][0]
            self.assertFalse(missing_record["pass"], missing_record)
            self.assertFalse(missing_record["mcp_verified"], missing_record)
            self.assertIn("B:mcp_server_log_missing", missing_record["flags"])

            nameless_suite = "nameless-radar-log-suite"
            nameless = self._mcp_release_record(nameless_suite, repetition=1, run_id="radar-1")
            nameless_dir = self._write_records(results_dir, nameless_suite, [nameless])
            self._write_mcp_server_log(nameless_dir, "radar-1")
            nameless_report = audit_codex.build_audit_report(results_dir, ["nameless-radar-*"])
            nameless_record = nameless_report["suites"][nameless_suite]["records"][0]
            self.assertFalse(nameless_record["pass"], nameless_record)
            self.assertFalse(nameless_record["mcp_verified"], nameless_record)
            self.assertIn("B:mcp_required_tool_names_not_in_server_log(brain_regressions)", nameless_record["flags"])

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


class FactsEvalAuditScriptTests(unittest.TestCase):
    TASKS_SHA = "sha256:" + "a" * 64
    BRAIN_SHA = "sha256:" + "b" * 64

    def _write_facts_eval_fixture(self, root: pathlib.Path, *, claimable: bool = True, proxy: bool = False) -> pathlib.Path:
        summaries = {}
        for retriever in ("facts", "history", "query", "raw-sessions"):
            path = f"{retriever}.json"
            summaries[retriever] = path
            (root / path).write_text(json.dumps({
                "retriever": retriever,
                "run_config": {
                    "tasks_sha256": self.TASKS_SHA,
                    "brain_manifest_sha256": self.BRAIN_SHA,
                },
                "results": [{
                    "id": "task-1",
                    "labeled": True,
                    "relevance_source": "explicit_label",
                    "label_source": "human",
                }],
            }))
        (root / "raw-vs-facts.compare.json").write_text(json.dumps({
            "n": 12,
            "alpha": 0.05,
            "a_retriever": "raw-sessions",
            "b_retriever": "facts",
            "a_tasks_sha256": self.TASKS_SHA,
            "b_tasks_sha256": self.TASKS_SHA,
            "a_brain_manifest_sha256": self.BRAIN_SHA,
            "b_brain_manifest_sha256": self.BRAIN_SHA,
            "release_pairing_ready": True,
            "release_claimable": claimable and not proxy,
            "allow_proxy_comparison": proxy,
            "allow_missing_tasks": False,
            "allow_task_hash_mismatch": False,
            "allow_brain_manifest_mismatch": False,
            "missing_from_a": [],
            "missing_from_b": [],
            "metrics": [{
                "metric": "useful_per_1k",
                "evidence_basis": "proxy_or_mixed" if proxy else "proof_labels",
                "n": 12,
                "significant": claimable,
                "release_claimable": claimable,
                "winner": "b",
            }],
        }))
        manifest = root / "manifest.json"
        manifest.write_text(json.dumps({
            "schema": 1,
            "required_retrievers": ["facts", "history", "query", "raw-sessions"],
            "summaries": summaries,
            "comparisons": {"raw_vs_facts": "raw-vs-facts.compare.json"},
            "required_claims": [{
                "comparison": "raw_vs_facts",
                "metric": "useful_per_1k",
                "a_retriever": "raw-sessions",
                "b_retriever": "facts",
                "winner": "b",
                "evidence_basis": "proof_labels",
            }],
        }))
        return manifest

    def test_facts_eval_audit_accepts_release_claimable_paired_proof(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            manifest = self._write_facts_eval_fixture(pathlib.Path(root))
            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertTrue(report["release_evidence"], report)
            self.assertEqual(report["required_claims"][0]["b_retriever"], "facts")
            self.assertEqual(audit_facts_eval.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 0)
            self.assertTrue((pathlib.Path(out) / "facts-eval-audit-report.json").exists())

    def test_facts_eval_audit_accepts_no_release_claim_status(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            root_path = pathlib.Path(root)
            (root_path / "facts-status.json").write_text(json.dumps({
                "schema_version": 1,
                "facts_arm_ready": False,
                "totals": {"active": 0, "facts": 0},
                "warnings": ["facts retriever has no active facts; facts-vs-raw release proof cannot be collected yet"],
            }))
            manifest = root_path / "manifest.json"
            manifest.write_text(json.dumps({
                "schema": 1,
                "claim_policy": "no_release_claim",
                "facts_status": "facts-status.json",
                "summaries": {},
                "comparisons": {},
                "required_claims": [],
            }))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertTrue(report["release_evidence"], report)
            self.assertFalse(report["claimable_facts_vs_raw"])
            self.assertEqual(report["claim_policy"], "no_release_claim")
            self.assertEqual(audit_facts_eval.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 0)

    def test_facts_eval_audit_rejects_no_release_claim_when_facts_ready(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            (root_path / "facts-status.json").write_text(json.dumps({
                "schema_version": 1,
                "facts_arm_ready": True,
                "totals": {"active": 3, "facts": 3},
                "warnings": [],
            }))
            manifest = root_path / "manifest.json"
            manifest.write_text(json.dumps({
                "schema": 1,
                "claim_policy": "no_release_claim",
                "facts_status": "facts-status.json",
            }))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertFalse(report["release_evidence"], report)
            self.assertIn("facts_status.facts_arm_ready must be false for no_release_claim evidence", report["flags"])
            self.assertIn("facts_status.totals.active must be 0 for no_release_claim evidence", report["flags"])

    def test_facts_eval_audit_rejects_proxy_or_nonclaimable_comparison(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            manifest = self._write_facts_eval_fixture(pathlib.Path(root), proxy=True)
            self.assertEqual(audit_facts_eval.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            proxy_report = json.loads((pathlib.Path(out) / "facts-eval-audit-report.json").read_text())
            self.assertIn("raw_vs_facts: allow_proxy_comparison must be false for release proof", proxy_report["flags"])
            self.assertIn("raw_vs_facts: useful_per_1k evidence_basis is 'proxy_or_mixed', want 'proof_labels'", proxy_report["flags"])

            manifest = self._write_facts_eval_fixture(pathlib.Path(root), claimable=False)
            self.assertEqual(audit_facts_eval.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            weak_report = json.loads((pathlib.Path(out) / "facts-eval-audit-report.json").read_text())
            self.assertIn("raw_vs_facts: useful_per_1k is not release_claimable", weak_report["flags"])

    def test_facts_eval_audit_rejects_missing_retriever_summary(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            data = json.loads(manifest.read_text())
            del data["summaries"]["query"]
            manifest.write_text(json.dumps(data))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertFalse(report["release_evidence"], report)
            self.assertIn("missing summary for retriever query", report["flags"])

    def test_facts_eval_audit_rejects_shortened_required_retrievers(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            data = json.loads(manifest.read_text())
            data["required_retrievers"] = ["facts", "raw-sessions"]
            manifest.write_text(json.dumps(data))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertFalse(report["release_evidence"], report)
            self.assertIn("required_retrievers missing canonical retrievers: history, query", report["flags"])

    def test_facts_eval_audit_rejects_stale_compare_claiming_proof_labels(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            facts = json.loads((root_path / "facts.json").read_text())
            facts["results"][0]["labeled"] = False
            facts["results"][0]["relevance_source"] = "source_match"
            facts["results"][0]["label_source"] = "provenance_silver"
            (root_path / "facts.json").write_text(json.dumps(facts))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertFalse(report["release_evidence"], report)
            self.assertTrue(any(flag.startswith("facts: 1 result(s) are not human/judge_refined explicit proof labels") for flag in report["flags"]), report["flags"])

    def test_facts_eval_audit_rejects_required_proxy_basis(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            compare_path = root_path / "raw-vs-facts.compare.json"
            comp = json.loads(compare_path.read_text())
            comp["release_claimable"] = True
            comp["metrics"][0]["evidence_basis"] = "proxy_or_mixed"
            comp["metrics"][0]["release_claimable"] = True
            compare_path.write_text(json.dumps(comp))
            data = json.loads(manifest.read_text())
            data["required_claims"][0]["evidence_basis"] = "proxy_or_mixed"
            manifest.write_text(json.dumps(data))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)

            self.assertFalse(report["release_evidence"], report)
            self.assertIn("raw_vs_facts: useful_per_1k required claim evidence_basis must be 'proof_labels'", report["flags"])

    def test_facts_eval_audit_rejects_missing_release_pairing_ready(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            compare_path = root_path / "raw-vs-facts.compare.json"
            comp = json.loads(compare_path.read_text())
            comp["release_pairing_ready"] = False
            compare_path.write_text(json.dumps(comp))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)

            self.assertFalse(report["release_evidence"], report)
            self.assertIn("raw_vs_facts: release_pairing_ready must be true", report["flags"])

    def test_facts_eval_audit_requires_facts_beats_raw_claim(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            compare_path = root_path / "raw-vs-facts.compare.json"
            comp = json.loads(compare_path.read_text())
            comp["metrics"][0]["winner"] = "a"
            compare_path.write_text(json.dumps(comp))
            data = json.loads(manifest.read_text())
            data["required_claims"][0]["winner"] = "a"
            manifest.write_text(json.dumps(data))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)

            self.assertFalse(report["release_evidence"], report)
            self.assertFalse(report["claimable_facts_vs_raw"], report)
            self.assertIn("required_claims must include a proof-label claim where facts beats raw-sessions", report["flags"])

    def test_facts_eval_audit_rejects_summary_hash_mismatch(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_facts_eval_fixture(root_path)
            facts = json.loads((root_path / "facts.json").read_text())
            facts["run_config"]["brain_manifest_sha256"] = "sha256:" + "c" * 64
            (root_path / "facts.json").write_text(json.dumps(facts))

            report = audit_facts_eval.audit_facts_eval_manifest(manifest)
            self.assertFalse(report["release_evidence"], report)
            self.assertIn("eval summaries have differing brain_manifest_sha256 values", report["flags"])


class DistillPerfAuditScriptTests(unittest.TestCase):
    def _sha256(self, path: pathlib.Path) -> str:
        return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()

    def _write_distill_perf_fixture(self, root: pathlib.Path, *, speedup: float = 2.0, mismatch: bool = False) -> pathlib.Path:
        dry = {
            "schema_version": 1,
            "generated_at": "2026-06-10T00:00:00Z",
            "brain_path": "/portable/brain",
            "branch": "",
            "force": True,
            "agent": "ollama",
            "model": "llama3.2",
            "effort": "",
            "jobs": 1,
            "effective_extraction_jobs": 1,
            "max_chunk_bytes": 32000,
            "confidence_threshold": 0.75,
            "sessions": 12,
            "cached_sessions": 0,
            "sessions_to_distill": 12,
            "missing_transcripts": 0,
            "raw_bytes": 900000,
            "preprocessed_bytes": 250000,
            "chunks": 20,
            "chunks_if_uncached": 20,
            "extraction_agent_calls": 20,
            "reconcile_agent_calls_upper_bound": 20,
            "estimated_agent_calls_upper_bound": 40,
            "branches": [{"branch": "main", "sessions": 12, "cached_sessions": 0, "sessions_to_distill": 12, "chunks": 20, "chunks_if_uncached": 20, "preprocessed_bytes": 250000}],
            "largest_sessions": [{"session_id": "s1", "branch": "main", "transcript": "sessions/s1.md", "cached": False, "chunks": 3, "chunks_if_uncached": 3, "raw_bytes": 1000, "preprocessed_bytes": 700}],
        }
        serial_seconds = 120.0
        parallel_seconds = serial_seconds / speedup

        def run(jobs: int, effective_jobs: int, total_seconds: float) -> dict:
            return {
                "generated_at": "2026-06-10T00:10:00Z",
                "facts": 80,
                "distilled": 80,
                "authored": 0,
                "superseded": 0,
                "branches": ["main"],
                "proposals": 0,
                "chunks_scanned": 20,
                "chunks_distilled": 20,
                "cache_hits": 0,
                "failed_chunks": 0,
                "preprocessed_bytes": 250000,
                "agent": "ollama",
                "model": "llama3.2",
                "effort": "",
                "branch": "",
                "force": True,
                "jobs": jobs,
                "effective_extraction_jobs": effective_jobs,
                "max_chunk_bytes": 32000,
                "confidence_threshold": 0.75,
                "extraction_agent_calls": 20,
                "reconcile_agent_calls": 4,
                "total_agent_calls": 24,
                "extraction_seconds": total_seconds * 0.8,
                "reconcile_seconds": total_seconds * 0.15,
                "write_seconds": total_seconds * 0.05,
                "total_seconds": total_seconds,
            }

        serial = run(1, 1, serial_seconds)
        parallel = run(4, 4, parallel_seconds)
        if mismatch:
            parallel["facts"] = 79
        dry_path = root / "dry-run.json"
        serial_path = root / "jobs-1.json"
        parallel_path = root / "jobs-4.json"
        dry_path.write_text(json.dumps(dry))
        serial_path.write_text(json.dumps(serial))
        parallel_path.write_text(json.dumps(parallel))
        manifest = root / "manifest.json"
        manifest.write_text(json.dumps({
            "schema": 1,
            "target": {
                "repo": "github.com/example/large-repo",
                "repo_key": "gh/example/large-repo",
                "source_head": "1" * 40,
                "brain_manifest_sha256": "sha256:" + "2" * 64,
                "claim_scope": "large-repo distill backfill speedup",
            },
            "dry_run": "dry-run.json",
            "serial_run": "jobs-1.json",
            "parallel_run": "jobs-4.json",
            "artifact_sha256": {
                "dry_run": self._sha256(dry_path),
                "serial_run": self._sha256(serial_path),
                "parallel_run": self._sha256(parallel_path),
            },
            "commands": {
                "dry_run": ["entire", "brain", "distill", "--dry-run", "--json", "--agent", "ollama", "--model", "llama3.2", "--force", "--jobs", "1", "--max-chunk-bytes", "32000", "--confidence", "0.75"],
                "serial_run": ["entire", "brain", "distill", "--json", "--agent", "ollama", "--model", "llama3.2", "--force", "--jobs", "1", "--max-chunk-bytes", "32000", "--confidence", "0.75"],
                "parallel_run": ["entire", "brain", "distill", "--json", "--agent", "ollama", "--model", "llama3.2", "--force", "--jobs", "4", "--max-chunk-bytes", "32000", "--confidence", "0.75"],
            },
            "min_speedup": 1.25,
        }))
        return manifest

    def test_distill_perf_audit_accepts_comparable_speedup(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            manifest = self._write_distill_perf_fixture(pathlib.Path(root))
            report = audit_distill_perf.audit_distill_perf_manifest(manifest)
            self.assertTrue(report["release_evidence"], report)
            self.assertGreaterEqual(report["speedup"], 1.25)
            self.assertEqual(report["target"]["repo"], "github.com/example/large-repo")
            self.assertEqual(audit_distill_perf.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 0)
            self.assertTrue((pathlib.Path(out) / "distill-perf-audit-report.json").exists())

    def test_distill_perf_audit_accepts_omitted_zero_cache_hits(self):
        with tempfile.TemporaryDirectory() as root:
            root_path = pathlib.Path(root)
            manifest = self._write_distill_perf_fixture(root_path)
            for name in ("jobs-1.json", "jobs-4.json"):
                path = root_path / name
                data = json.loads(path.read_text())
                data.pop("cache_hits")
                path.write_text(json.dumps(data))
            manifest_data = json.loads(manifest.read_text())
            manifest_data["artifact_sha256"]["serial_run"] = self._sha256(root_path / "jobs-1.json")
            manifest_data["artifact_sha256"]["parallel_run"] = self._sha256(root_path / "jobs-4.json")
            manifest.write_text(json.dumps(manifest_data))

            report = audit_distill_perf.audit_distill_perf_manifest(manifest)
            self.assertTrue(report["release_evidence"], report)

    def test_distill_perf_audit_rejects_weak_speedup_or_mismatched_output(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            manifest = self._write_distill_perf_fixture(pathlib.Path(root), speedup=1.05)
            self.assertEqual(audit_distill_perf.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            weak = json.loads((pathlib.Path(out) / "distill-perf-audit-report.json").read_text())
            self.assertTrue(any(flag.startswith("speedup ") for flag in weak["flags"]))

            manifest = self._write_distill_perf_fixture(pathlib.Path(root), mismatch=True)
            self.assertEqual(audit_distill_perf.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            mismatch = json.loads((pathlib.Path(out) / "distill-perf-audit-report.json").read_text())
            self.assertIn("serial/parallel mismatch: facts", mismatch["flags"])

    def test_distill_perf_audit_rejects_bad_hash_or_command_provenance(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            root_path = pathlib.Path(root)
            manifest = self._write_distill_perf_fixture(root_path)
            data = json.loads(manifest.read_text())
            data["artifact_sha256"]["serial_run"] = "sha256:" + "0" * 64
            data["commands"]["parallel_run"] = ["entire", "brain", "distill", "--json", "--agent", "auto", "--force", "--jobs", "4", "--confidence", "0.75"]
            manifest.write_text(json.dumps(data))
            self.assertEqual(audit_distill_perf.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            report = json.loads((pathlib.Path(out) / "distill-perf-audit-report.json").read_text())
            self.assertIn("artifact_sha256.serial_run mismatch", report["flags"])
            self.assertIn("commands.parallel_run: --agent must match artifact agent", report["flags"])
            self.assertIn("commands.parallel_run: --max-chunk-bytes must match artifact max_chunk_bytes", report["flags"])

    def test_distill_perf_audit_requires_target_provenance(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            root_path = pathlib.Path(root)
            manifest = self._write_distill_perf_fixture(root_path)
            data = json.loads(manifest.read_text())
            data.pop("target")
            manifest.write_text(json.dumps(data))
            with self.assertRaises(SystemExit):
                audit_distill_perf.audit_distill_perf_manifest(manifest)

            manifest = self._write_distill_perf_fixture(root_path)
            data = json.loads(manifest.read_text())
            data["target"]["source_head"] = "not-a-commit"
            data["target"]["brain_manifest_sha256"] = "sha256:not64hex"
            data["target"]["claim_scope"] = ""
            manifest.write_text(json.dumps(data))
            self.assertEqual(audit_distill_perf.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]), 1)
            report = json.loads((pathlib.Path(out) / "distill-perf-audit-report.json").read_text())
            self.assertIn("target.source_head must be a 40-character git commit", report["flags"])
            self.assertIn("target.brain_manifest_sha256 must be sha256:<64 hex>", report["flags"])
            self.assertIn("target.claim_scope must be a non-empty string", report["flags"])


class RadarEvidenceAuditScriptTests(unittest.TestCase):
    def _write_radar_summary(
        self,
        root: pathlib.Path,
        suite: str,
        *,
        baseline_pass: float,
        condition_pass: float,
        proof_ready: bool = False,
        stability_tag: str = "noisy",
        n: int = 1,
    ) -> pathlib.Path:
        suite_dir = root / suite
        suite_dir.mkdir(parents=True)
        (suite_dir / "summary.json").write_text(json.dumps({
            "comparisons": [{
                "task_id": "radar-task",
                "runner": "codex-mini-low",
                "condition": "mcp_history",
                "delivery_scope": "mcp_radar_location_only",
                "n_condition": n,
                "n_baseline": n,
                "pass_rate_condition": condition_pass,
                "pass_rate_baseline": baseline_pass,
                "mean_total_tokens_condition": 12000,
                "mean_total_tokens_baseline": 24000,
                "verdict": "brain_positive" if proof_ready else "saturated/no_signal",
                "proof_ready": proof_ready,
                "stability": {"tag": stability_tag},
            }]
        }))
        return suite_dir

    def _write_backed_radar_codex_audit(
        self,
        path: pathlib.Path,
        suite: str,
        *,
        baseline_valid: list[bool] | None = None,
        condition_valid: list[bool] | None = None,
        named_tool_backed: bool = True,
        comparison_proof_ready: bool = True,
        comparison_pass: bool = True,
        record_pass: bool = True,
        record_flags: list[str] | None = None,
    ) -> None:
        baseline_valid = baseline_valid or [True, False, False, False]
        condition_valid = condition_valid or [True, True, True, True]
        record_flags = record_flags or []
        records = []
        for index, valid in enumerate(baseline_valid, start=1):
            records.append({
                "run_id": f"base-{index}",
                "task_id": "radar-task",
                "runner": "codex-mini-low",
                "condition": "no_brain",
                "delivery_scope": "no_brain",
                "valid": valid,
                "pass": True,
                "provenance": {"ok": True},
                "mcp_verified": False,
            })
        for index, valid in enumerate(condition_valid, start=1):
            records.append({
                "run_id": f"radar-{index}",
                "task_id": "radar-task",
                "runner": "codex-mini-low",
                "condition": "mcp_history",
                "delivery_scope": "mcp_radar_location_only",
                "valid": valid,
                "pass": record_pass,
                "provenance": {"ok": True},
                "mcp_verified": record_pass,
                "mcp_named_tool_verified": named_tool_backed,
                "mcp_named_tool_completed": named_tool_backed,
                "flags": record_flags,
            })
        path.write_text(json.dumps({
            "suites": {
                suite: {
                    "records": records,
                    "comparisons": [{
                        "task": "radar-task",
                        "runner": "codex-mini-low",
                        "condition": "mcp_history",
                        "delivery_scope": "mcp_radar_location_only",
                        "proof_scope": "mcp_radar_location_only",
                        "proof_ready": comparison_proof_ready,
                        "pass": comparison_pass,
                        "record_backing": {
                            "ok": True,
                            "condition_mcp_verified_ok": True,
                            "condition_mcp_named_tool_verified_ok": named_tool_backed,
                            "condition_mcp_named_tool_completed_ok": named_tool_backed,
                        },
                    }],
                }
            }
        }))

    def test_radar_audit_marks_saturated_pilots_not_promotable(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            self._write_radar_summary(results_dir, "pilot-radar-saturated", baseline_pass=1.0, condition_pass=1.0, stability_tag="saturated")
            report = audit_radar_evidence.build_report(results_dir, ["pilot-radar-*"])
            self.assertEqual(report["totals"]["status_counts"], {"saturated": 1})
            self.assertEqual(report["totals"]["promotable_or_proof"], 0)
            self.assertEqual(
                audit_radar_evidence.main(["--results", str(results_dir), "--suite-glob", "pilot-radar-*", "--out-dir", out, "--fail-when-no-promotable"]),
                1,
            )

    def test_radar_audit_accepts_promotable_pilot_with_baseline_headroom(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            self._write_radar_summary(results_dir, "pilot-radar-headroom", baseline_pass=0.0, condition_pass=1.0)
            report = audit_radar_evidence.build_report(results_dir, ["pilot-radar-*"])
            gate = report["comparisons"][0]["radar_gate"]
            self.assertEqual(gate["status"], "promotable-pilot")
            self.assertTrue(gate["baseline_headroom"])
            self.assertEqual(audit_radar_evidence.main(["--results", str(results_dir), "--suite-glob", "pilot-radar-*", "--out-dir", out, "--fail-when-no-promotable"]), 0)

    def test_radar_audit_demotes_dirty_promotable_pilot(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "pilot-radar-headroom-dirty"
            self._write_radar_summary(results_dir, suite, baseline_pass=0.0, condition_pass=1.0, n=1)
            backed_audit = out_dir / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(
                backed_audit,
                suite,
				baseline_valid=[False],
				condition_valid=[True],
				comparison_proof_ready=False,
				comparison_pass=False,
				record_pass=False,
				record_flags=["H:provenance_missing_radar_include_deletions_policy"],
			)

            report = audit_radar_evidence.build_report(
                results_dir,
                ["pilot-radar-*"],
                audit_radar_evidence.load_codex_audit(backed_audit),
            )
            gate = report["comparisons"][0]["radar_gate"]
            self.assertEqual(gate["status"], "promotable-audit-gap")
            self.assertFalse(gate["promotable"])
            self.assertEqual(report["totals"]["promotable_or_proof"], 0)
            self.assertIn("missing policy", audit_radar_evidence.render_markdown(report))
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "pilot-radar-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(backed_audit),
                    "--fail-when-no-promotable",
                ]),
                1,
            )

    def test_radar_audit_reports_early_stopped_no_brain_too_easy_suites(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            suite = results_dir / "pilot-radar-too-easy"
            suite.mkdir()
            (suite / "summary.json").write_text(json.dumps({"comparisons": []}))
            (suite / "records.ndjson").write_text(json.dumps({
                "task_id": "easy-radar-task",
                "condition": "no_brain",
                "runner": {"id": "codex-mini-low"},
                "score": {"total": 97},
                "valid": True,
                "provenance": {"run_config": {"stop_after_no_brain_score": 90}},
            }) + "\n")
            report = audit_radar_evidence.build_report(results_dir, ["pilot-radar-*"])
            self.assertEqual(report["totals"]["status_counts"], {"no-brain-too-easy": 1})
            self.assertEqual(report["totals"]["promotable_or_proof"], 0)
            row = report["comparisons"][0]
            self.assertEqual(row["task_id"], "easy-radar-task")
            self.assertEqual(row["radar_gate"]["baseline_score"], 97)
            self.assertIn("harder target", row["radar_gate"]["recommendation"])
            rendered = audit_radar_evidence.render_markdown(report)
            self.assertIn("score 97 > 90", rendered)
            self.assertEqual(
                audit_radar_evidence.main(["--results", str(results_dir), "--suite-glob", "pilot-radar-*", "--out-dir", out, "--fail-when-no-promotable"]),
                1,
            )

    def test_radar_audit_reports_incomplete_matching_suites(self):
        with tempfile.TemporaryDirectory() as results:
            results_dir = pathlib.Path(results)
            suite = results_dir / "release-candidate-radar-incomplete"
            suite.mkdir()
            (suite / "records.ndjson").write_text(json.dumps({
                "task_id": "radar-task",
                "condition": "mcp_history",
                "runner": {"id": "codex-mini-low"},
                "valid": False,
            }) + "\n")

            report = audit_radar_evidence.build_report(results_dir, ["release-candidate-*"])
            self.assertEqual(report["totals"]["status_counts"], {"incomplete-suite": 1})
            row = report["comparisons"][0]
            self.assertEqual(row["delivery_scope"], "incomplete")
            self.assertIn("no summary.json", row["radar_gate"]["reasons"][0])
            self.assertIn("incomplete-suite", audit_radar_evidence.render_markdown(report))

    def test_radar_audit_shows_mcp_backing_for_negative_comparison(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-negative"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=1.0,
                condition_pass=0.5,
                proof_ready=False,
                stability_tag="noisy",
                n=4,
            )
            backed_audit = out_dir / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(
                backed_audit,
                suite,
                baseline_valid=[True, True, True, True],
                condition_valid=[True, False, False, True],
                comparison_proof_ready=False,
            )

            report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(backed_audit),
            )
            gate = report["comparisons"][0]["radar_gate"]
            backing = gate["codex_audit_record_backing"]
            self.assertEqual(gate["status"], "brain-not-clean")
            self.assertTrue(gate["codex_audit_backed"], gate)
            self.assertEqual(backing["condition_named_tool_records"], 4)
            self.assertEqual(backing["condition_completed_named_tool_records"], 4)
            self.assertTrue(backing["summary_consistency_ok"], backing)
            self.assertIn("4/4", audit_radar_evidence.render_markdown(report))

    def test_radar_audit_shows_attempted_mcp_backing_for_wrong_args(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-wrong-args"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=1.0,
                condition_pass=0.5,
                proof_ready=False,
                stability_tag="noisy",
                n=4,
            )
            backed_audit = out_dir / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(
                backed_audit,
                suite,
                baseline_valid=[True, True, True, True],
                condition_valid=[True, False, False, True],
                comparison_proof_ready=False,
                record_pass=False,
                record_flags=["B:mcp_radar_missing_include_deletions"],
            )

            report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(backed_audit),
            )
            backing = report["comparisons"][0]["radar_gate"]["codex_audit_record_backing"]
            self.assertEqual(backing["condition_named_tool_records"], 0)
            self.assertEqual(backing["condition_attempted_named_tool_records"], 4)
            rendered = audit_radar_evidence.render_markdown(report)
            self.assertIn("0/0 clean; 4/4 attempted", rendered)
            self.assertIn("wrong args", rendered)

    def test_radar_audit_shows_attempted_mcp_backing_for_missing_policy(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-missing-policy"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=1.0,
                condition_pass=0.5,
                proof_ready=False,
                stability_tag="noisy",
                n=4,
            )
            backed_audit = out_dir / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(
                backed_audit,
                suite,
                baseline_valid=[True, True, True, True],
                condition_valid=[True, False, False, True],
                comparison_proof_ready=False,
                record_pass=False,
                record_flags=["H:provenance_missing_radar_include_deletions_policy"],
            )

            report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(backed_audit),
            )
            rendered = audit_radar_evidence.render_markdown(report)
            self.assertIn("0/0 clean; 4/4 attempted", rendered)
            self.assertIn("missing policy", rendered)
            self.assertNotIn("wrong args", rendered)

    def test_radar_audit_requires_stable_proof_for_proof_gate(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            self._write_radar_summary(
                results_dir,
                "release-candidate-radar-proof",
                baseline_pass=0.25,
                condition_pass=1.0,
                proof_ready=True,
                stability_tag="brain_positive_stable",
                n=4,
            )
            report = audit_radar_evidence.build_report(results_dir, ["release-candidate-*"])
            self.assertEqual(report["totals"]["proof_ready"], 1)
            self.assertEqual(audit_radar_evidence.main(["--results", str(results_dir), "--suite-glob", "release-candidate-*", "--out-dir", out, "--fail-when-no-proof"]), 1)

            backed_audit = pathlib.Path(out) / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(backed_audit, "release-candidate-radar-proof")
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "release-candidate-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(backed_audit),
                    "--fail-when-no-proof",
                ]),
                0,
            )

    def test_radar_audit_requires_codex_audit_backing_when_supplied(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-proof"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=0.25,
                condition_pass=1.0,
                proof_ready=True,
                stability_tag="brain_positive_stable",
                n=4,
            )
            missing_audit = out_dir / "missing-codex-audit.json"
            missing_audit.write_text(json.dumps({"suites": {}}))
            missing_report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(missing_audit),
            )
            self.assertEqual(missing_report["totals"]["proof_ready"], 0)
            self.assertEqual(missing_report["totals"]["status_counts"], {"audit-missing": 1})
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "release-candidate-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(missing_audit),
                    "--fail-when-no-proof",
                ]),
                1,
            )

            backed_audit = out_dir / "backed-codex-audit.json"
            self._write_backed_radar_codex_audit(backed_audit, suite)
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "release-candidate-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(backed_audit),
                    "--fail-when-no-proof",
                ]),
                0,
            )

    def test_radar_audit_requires_named_tool_codex_backing(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-proof"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=0.25,
                condition_pass=1.0,
                proof_ready=True,
                stability_tag="brain_positive_stable",
                n=4,
            )
            generic_mcp_audit = out_dir / "generic-mcp-codex-audit.json"
            self._write_backed_radar_codex_audit(generic_mcp_audit, suite, named_tool_backed=False)

            report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(generic_mcp_audit),
            )
            gate = report["comparisons"][0]["radar_gate"]
            self.assertEqual(gate["status"], "audit-backing-gap")
            self.assertEqual(report["totals"]["proof_ready"], 0)
            self.assertIn("not audit-clean", " | ".join(gate["reasons"]))
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "release-candidate-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(generic_mcp_audit),
                    "--fail-when-no-proof",
                ]),
                1,
            )

    def test_radar_audit_rejects_codex_audit_summary_mismatch(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            out_dir = pathlib.Path(out)
            suite = "release-candidate-radar-proof"
            self._write_radar_summary(
                results_dir,
                suite,
                baseline_pass=0.25,
                condition_pass=1.0,
                proof_ready=True,
                stability_tag="brain_positive_stable",
                n=4,
            )
            stale_audit = out_dir / "stale-codex-audit.json"
            self._write_backed_radar_codex_audit(stale_audit, suite, baseline_valid=[True, True, True, True])

            report = audit_radar_evidence.build_report(
                results_dir,
                ["release-candidate-*"],
                audit_radar_evidence.load_codex_audit(stale_audit),
            )
            gate = report["comparisons"][0]["radar_gate"]
            self.assertEqual(gate["status"], "audit-mismatch")
            self.assertEqual(report["totals"]["proof_ready"], 0)
            self.assertIn("pass_rate_baseline", " | ".join(gate["reasons"]))
            self.assertFalse(gate["codex_audit_record_backing"]["summary_consistency_ok"])
            self.assertEqual(
                audit_radar_evidence.main([
                    "--results", str(results_dir),
                    "--suite-glob", "release-candidate-*",
                    "--out-dir", out,
                    "--codex-audit-report", str(stale_audit),
                    "--fail-when-no-proof",
                ]),
                1,
            )

    def test_radar_audit_rejects_saturated_token_only_proof(self):
        with tempfile.TemporaryDirectory() as results, tempfile.TemporaryDirectory() as out:
            results_dir = pathlib.Path(results)
            self._write_radar_summary(
                results_dir,
                "release-candidate-radar-token-only",
                baseline_pass=1.0,
                condition_pass=1.0,
                proof_ready=True,
                stability_tag="brain_positive_stable",
                n=4,
            )
            report = audit_radar_evidence.build_report(results_dir, ["release-candidate-*"])
            self.assertEqual(report["totals"]["proof_ready"], 0)
            self.assertEqual(report["totals"]["status_counts"], {"saturated": 1})
            self.assertEqual(audit_radar_evidence.main(["--results", str(results_dir), "--suite-glob", "release-candidate-*", "--out-dir", out, "--fail-when-no-proof"]), 1)

    def _write_radar_tool_manifest(self, root: pathlib.Path, tests: list[str], source_head: str = "a" * 40) -> pathlib.Path:
        artifact = root / "go-test-radar.jsonl"
        events = []
        for test in tests:
            events.append({"Action": "run", "Package": "github.com/ashtom/entire-brain/internal/cli", "Test": test})
            events.append({"Action": "pass", "Package": "github.com/ashtom/entire-brain/internal/cli", "Test": test})
        events.append({"Action": "pass", "Package": "github.com/ashtom/entire-brain/internal/cli"})
        artifact.write_text("\n".join(json.dumps(event, sort_keys=True) for event in events) + "\n")
        manifest = root / "manifest.json"
        manifest.write_text(json.dumps({
            "schema": 1,
            "claim_scope": audit_radar_tool_evidence.CLAIM_SCOPE,
            "source_head": source_head,
            "required_tests": audit_radar_tool_evidence.REQUIRED_TESTS,
            "limitations": ["Tool-contract proof only; this is not agent lift proof."],
            "artifacts": [{
                "path": artifact.name,
                "sha256": audit_radar_tool_evidence.sha256_file(artifact),
            }],
        }))
        return manifest

    def test_radar_tool_evidence_accepts_required_go_tests(self):
        with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryDirectory() as out:
            root = pathlib.Path(tmp)
            manifest = self._write_radar_tool_manifest(root, audit_radar_tool_evidence.REQUIRED_TESTS)
            report = audit_radar_tool_evidence.audit_manifest(manifest)
            self.assertTrue(report["ok"], report)
            self.assertEqual(
                audit_radar_tool_evidence.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]),
                0,
            )
            self.assertTrue((pathlib.Path(out) / "radar-tool-audit-report.json").exists())

    def test_radar_tool_evidence_rejects_missing_required_go_test(self):
        with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryDirectory() as out:
            root = pathlib.Path(tmp)
            manifest = self._write_radar_tool_manifest(root, audit_radar_tool_evidence.REQUIRED_TESTS[:-1])
            report = audit_radar_tool_evidence.audit_manifest(manifest)
            self.assertFalse(report["ok"])
            self.assertIn("missing required passed tests", " | ".join(report["errors"]))
            self.assertEqual(
                audit_radar_tool_evidence.main(["--manifest", str(manifest), "--out-dir", out, "--fail-on-flags"]),
                1,
            )

    def test_radar_tool_evidence_rejects_changed_source_after_source_head(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            run.run_cmd(["git", "init"], cwd=root, check=True)
            run.run_cmd(["git", "config", "user.email", "bench@example.com"], cwd=root, check=True)
            run.run_cmd(["git", "config", "user.name", "Benchmark"], cwd=root, check=True)
            source = root / "internal" / "cli"
            source.mkdir(parents=True)
            (source / "regression.go").write_text("package cli\nconst radarFixture = 1\n")
            run.run_cmd(["git", "add", "."], cwd=root, check=True)
            run.run_cmd(["git", "commit", "-m", "base"], cwd=root, check=True)
            source_head = run.run_cmd(["git", "rev-parse", "HEAD"], cwd=root, check=True).stdout.strip()
            manifest = self._write_radar_tool_manifest(root, audit_radar_tool_evidence.REQUIRED_TESTS, source_head)

            (source / "regression.go").write_text("package cli\nconst radarFixture = 2\n")
            run.run_cmd(["git", "add", "."], cwd=root, check=True)
            run.run_cmd(["git", "commit", "-m", "change radar source"], cwd=root, check=True)

            report = audit_radar_tool_evidence.audit_manifest(manifest, repo_root=root)
            self.assertFalse(report["ok"])
            self.assertIn("internal/cli/regression.go", " | ".join(report["errors"]))
            self.assertEqual(report["source_drift_paths"], ["internal/cli/regression.go"])


if __name__ == "__main__":
    unittest.main()
