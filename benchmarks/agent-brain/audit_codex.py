#!/usr/bin/env python3
"""Read-only auditor for Codex (and any) Entire Brain benchmark suites.

Independently re-checks every recorded run for cheating / bias WITHOUT trusting
the harness's own self-reported `ok`. It re-derives the facts from the raw
record fields, the per-run mcp-server.log, and the task definitions, then emits
a PASS/FLAG verdict per check.

It NEVER modifies records, scoring, or validation. It only writes its own report
files (codex-audit-report.json + codex-audit-report.md).

Checks per record:
  A. no_brain purity      - no_brain runs must not touch Brain/MCP/CLI/.entire.
  B. mcp authenticity     - mcp_* runs must have real mcp tool calls backed by
                            the server log's tools/call count and, for new logs,
                            server-side tool names.
  C. fairness baseline    - agent baseline commit is parentless (no history leak).
  D. score integrity      - stored score.total == clamp(sum(components)).
  E. leakage audits       - secret preflight / leak audit / history sanitization ok.
  F. validation present   - the run actually ran the task's validation (non-empty).
Suite-level:
  G. matched comparisons  - brain-positive verdicts compare equal n, same runner.
"""
from __future__ import annotations

import argparse
import fnmatch
import json
import pathlib
import re
import sys
from collections import defaultdict
from typing import Any

BENCH = pathlib.Path(__file__).resolve().parent
RESULTS = BENCH / "results"
TASK_DIR = BENCH / "tasks"
DEFAULT_PROOF_MIN_REPETITIONS = 4

MCP_CONDITIONS = {"mcp_semantic", "mcp_history", "mcp_workspace_radar"}
SEMANTIC_CONDITIONS = {"semantic_brain", "semantic_cli", "mcp_semantic"}
HISTORY_CONDITIONS = {"full_brain", "full_cli_original", "full_cli_compact", "mcp_history", "mcp_workspace_radar"}
BRAIN_CONDITIONS = SEMANTIC_CONDITIONS | HISTORY_CONDITIONS
MCP_BRAIN_TOOL_RE = r"brain_(?:stale|brief|query|search|vsearch|get|multi_get|context|impact|changes|code|tests|boundaries|regressions|review|workspace_regressions|workspace_review)"
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
HOST_PATH_RE = re.compile(
    r"(?i)(?:"
    r"/Users/[^\s\"'`]+"            # macOS home paths
    r"|/home/[^\s\"'`]+"            # Linux home paths
    r"|/private/(?:var|tmp)/[^\s\"'`]+"  # macOS temp paths
    r"|/tmp/[^\s\"'`]+"             # POSIX temp paths
    r"|[A-Z]:\\Users\\[^\s\"'`]+"   # Windows home paths
    r")"
)


def load_records(suite_dir: pathlib.Path) -> list[dict[str, Any]]:
    nd = suite_dir / "records.ndjson"
    if not nd.exists():
        return []
    recs = []
    for line in nd.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            recs.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return recs


def server_log_toolcalls(run_dir: pathlib.Path) -> int | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    text = log.read_text(errors="ignore")
    return len(re.findall(r"^message: tools/call$", text, flags=re.MULTILINE))


def server_log_tool_names(run_dir: pathlib.Path) -> list[str] | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    text = log.read_text(errors="ignore")
    return re.findall(rf"^tool: ({MCP_BRAIN_TOOL_RE})$", text, flags=re.MULTILINE)


def bare_mcp_tool_name(name: Any) -> str:
    text = str(name)
    if "__" in text:
        return text.rsplit("__", 1)[-1]
    return text


def activity_has_mcp_call_with_args(activity: dict[str, Any], tool: str, required_args: dict[str, bool]) -> bool:
    details = activity.get("mcp_tool_details") if isinstance(activity.get("mcp_tool_details"), list) else []
    for detail in details:
        if not isinstance(detail, dict):
            continue
        name = bare_mcp_tool_name(detail.get("name"))
        if name != tool:
            continue
        if detail.get("errored"):
            continue
        args = detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}
        if all(args.get(key) is value for key, value in required_args.items()):
            return True
    return False


def record_env_flags(rec: dict[str, Any]) -> dict[str, str]:
    raw = get(rec, "provenance", "run_config", "env_flags", default={})
    if not isinstance(raw, dict):
        return {}
    return {
        str(key): str(value)
        for key, value in sorted(raw.items())
        if value not in (None, "")
    }


def delivery_scope_for_record(condition: str, env_flags: dict[str, str]) -> str:
    if condition == "mcp_workspace_radar":
        return "mcp_workspace_radar_location_only"
    if condition == "mcp_history" and env_flags.get("BENCH_RADAR_LOCATION_ONLY") == "1":
        return "mcp_radar_location_only"
    if condition == "mcp_history" and env_flags.get("BENCH_REGRESSION_RADAR") == "1":
        return "mcp_radar_answer_assisted"
    if condition == "mcp_history":
        return "mcp"
    return condition


def required_server_tool_names(condition: str, delivery_scope: str) -> set[str]:
    if condition == "mcp_workspace_radar" or delivery_scope == "mcp_workspace_radar_location_only":
        return {"brain_workspace_regressions"}
    if delivery_scope in {"mcp_radar_location_only", "mcp_radar_answer_assisted"}:
        return {"brain_regressions"}
    return set()


def resolve_repo_relative_path(value: Any) -> pathlib.Path | None:
    if not isinstance(value, str) or not value:
        return None
    raw = pathlib.Path(value)
    candidates = [raw] if raw.is_absolute() else [pathlib.Path.cwd() / raw, BENCH.parent.parent / raw]
    for candidate in candidates:
        try:
            if candidate.is_file():
                return candidate
        except OSError:
            continue
    return None


def record_radar_requires_deletions(rec: dict[str, Any]) -> bool:
    embedded = get(rec, "provenance", "task", "radar_include_deletions")
    if isinstance(embedded, bool):
        return embedded
    task_path = resolve_repo_relative_path(get(rec, "provenance", "task", "path"))
    if task_path is None:
        return False
    try:
        task = json.loads(task_path.read_text())
    except (OSError, json.JSONDecodeError):
        return False
    if not isinstance(task, dict):
        return False
    return task.get("radar_include_deletions") is True


def recompute_score_total(score: dict[str, Any]) -> int | None:
    parts = ["outcome", "patch_focus", "validation_discipline", "runtime_efficiency", "brain_use"]
    if not all(isinstance(score.get(p), (int, float)) for p in parts):
        return None
    return max(0, min(100, int(sum(int(score[p]) for p in parts))))


def get(d: Any, *path, default=None):
    cur = d
    for p in path:
        if not isinstance(cur, dict):
            return default
        cur = cur.get(p)
    return cur if cur is not None else default


def portable_report_path(path: pathlib.Path) -> str:
    resolved = path.resolve()
    for base in (pathlib.Path.cwd().resolve(), BENCH.parent.parent):
        try:
            return str(resolved.relative_to(base))
        except ValueError:
            continue
    return f"[external]/{resolved.name}"


def release_host_path_leaks(value: Any, prefix: str = "$", limit: int = 10) -> list[str]:
    leaks: list[str] = []

    def visit(node: Any, path: str) -> None:
        if len(leaks) >= limit:
            return
        if isinstance(node, str):
            if HOST_PATH_RE.search(node):
                leaks.append(path)
            return
        if isinstance(node, dict):
            for key, child in node.items():
                visit(child, f"{path}.{key}")
            return
        if isinstance(node, list):
            for index, child in enumerate(node):
                visit(child, f"{path}[{index}]")

    visit(value, prefix)
    return leaks


def is_commit_sha(value: Any) -> bool:
    return isinstance(value, str) and bool(COMMIT_RE.fullmatch(value))


def is_sha256(value: Any) -> bool:
    return isinstance(value, str) and bool(SHA256_RE.fullmatch(value))


def audit_commit_metadata(prov: dict[str, Any], label: str, *path: str) -> tuple[list[str], str | None]:
    flags: list[str] = []
    meta = get(prov, *path)
    field = ".".join(path)
    if not isinstance(meta, dict):
        return [f"H:provenance_missing_{label}"], None
    if meta.get("available") is False:
        return [f"H:provenance_{label}_unavailable"], None
    commit = meta.get("commit")
    if not is_commit_sha(commit):
        flags.append(f"H:provenance_bad_{label}_commit")
        commit = None
    parents = meta.get("parents")
    parent_count = meta.get("parent_count")
    if parents is not None and not isinstance(parents, list):
        flags.append(f"H:provenance_bad_{field}_parents")
    if parent_count is not None and not isinstance(parent_count, int):
        flags.append(f"H:provenance_bad_{field}_parent_count")
    return flags, commit


def audit_record_provenance(rec: dict[str, Any]) -> tuple[list[str], list[str], dict[str, Any]]:
    flags: list[str] = []
    notes: list[str] = []
    prov = rec.get("provenance")
    if not isinstance(prov, dict):
        return ["H:provenance_missing"], [], {"present": False, "ok": False}

    if prov.get("schema") != 1:
        flags.append("H:provenance_bad_schema")

    harness_flags, harness_head = audit_commit_metadata(prov, "harness_head", "harness", "head")
    base_flags, source_base = audit_commit_metadata(prov, "source_base", "source", "base")
    head_flags, source_head = audit_commit_metadata(prov, "source_head", "source", "head")
    flags.extend(harness_flags)
    flags.extend(base_flags)
    flags.extend(head_flags)

    base_ref = get(prov, "source", "base_ref")
    base_ref_source = get(prov, "source", "base_ref_source")
    if not isinstance(base_ref, str) or not base_ref:
        flags.append("H:provenance_missing_source_base_ref")
    if base_ref_source not in {"task.base_commit", "source_head"}:
        flags.append("H:provenance_bad_source_base_ref_source")

    task_id = get(prov, "task", "id")
    if task_id != rec.get("task_id"):
        flags.append("H:provenance_task_id_mismatch")
    if not is_sha256(get(prov, "task", "config_sha256")):
        flags.append("H:provenance_missing_task_config_sha256")

    task_base = get(prov, "task", "base_commit")
    if task_base:
        if is_commit_sha(task_base):
            if source_base and task_base != source_base:
                flags.append("H:provenance_task_base_commit_mismatch")
        else:
            notes.append("H:task_base_commit_not_full_sha")
    elif source_base and source_head and source_base != source_head:
        flags.append("H:provenance_unpinned_base_head_mismatch")

    run_condition = get(prov, "run_config", "condition")
    if run_condition != rec.get("condition"):
        flags.append("H:provenance_condition_mismatch")
    if rec.get("repetition") is not None and get(prov, "run_config", "repetition") != rec.get("repetition"):
        flags.append("H:provenance_repetition_mismatch")
    rec_runner = get(rec, "runner", "id", default=rec.get("agent"))
    prov_runner = get(prov, "run_config", "runner", "id")
    if rec_runner and prov_runner and rec_runner != prov_runner:
        flags.append("H:provenance_runner_mismatch")
    if not is_sha256(get(prov, "run_config", "fingerprint")):
        flags.append("H:provenance_missing_run_config_fingerprint")
    if not is_sha256(prov.get("fingerprint")):
        flags.append("H:provenance_missing_record_fingerprint")

    for tool in ("brain", "sem", "entire"):
        if not is_sha256(get(prov, "tools", tool, "sha256")):
            flags.append(f"H:provenance_missing_{tool}_tool_sha256")

    if get(prov, "harness", "dirty", "dirty") is True:
        notes.append("H:harness_dirty")
    if get(prov, "source", "dirty", "dirty") is True:
        notes.append("H:source_dirty")

    summary = {
        "present": True,
        "ok": not any(flag.startswith("H:") for flag in flags),
        "harness_head": harness_head,
        "source_base": source_base,
        "source_head": source_head,
        "task_config_sha256": get(prov, "task", "config_sha256"),
        "run_config_fingerprint": get(prov, "run_config", "fingerprint"),
        "record_fingerprint": prov.get("fingerprint"),
    }
    return flags, notes, summary


def audit_record(rec: dict[str, Any], suite_dir: pathlib.Path) -> dict[str, Any]:
    cond = rec.get("condition", "")
    run_id = rec.get("run_id", "")
    run_dir = suite_dir / run_id
    activity = get(rec, "agent_info", "activity", default={}) or {}
    score = rec.get("score", {}) or {}
    flags: list[str] = []   # hard integrity violations / potential cheats
    notes: list[str] = []   # soft context: honest failures, missing attestation

    mcp_calls = int(activity.get("mcp_tool_calls") or 0)
    cli_calls = int(activity.get("direct_brain_cli_calls") or 0)
    used_brain = bool(activity.get("used_brain"))
    search_calls = int(activity.get("search_calls") or 0)
    slog = server_log_toolcalls(run_dir)
    slog_names = server_log_tool_names(run_dir)
    is_win = bool(rec.get("ok")) and (get(rec, "validation", "ok") is True)
    # Compact-delivery models are instructed to call brain_brief ONCE and NOT
    # brain_search (the compact brief already carries the top history hits).
    # Mirrored here independently so a correct compact run is not flagged for a
    # "missing" tool it was explicitly told not to call.
    model = (get(rec, "runner", "model") or rec.get("agent") or "").lower()
    compact = model in {"gpt-5.5", "gpt-5", "opus", "claude-opus-4-8"}
    env_flags = record_env_flags(rec)
    delivery_scope = delivery_scope_for_record(str(cond), env_flags)
    required_logged_tools = required_server_tool_names(str(cond), delivery_scope)

    # A. no_brain purity (HARD: no_brain must never touch Brain/MCP/CLI/private)
    if cond == "no_brain":
        if mcp_calls > 0:
            flags.append(f"A:no_brain_used_mcp({mcp_calls})")
        if cli_calls > 0:
            flags.append(f"A:no_brain_used_cli({cli_calls})")
        if used_brain:
            flags.append("A:no_brain_used_brain")
        prep_cond = get(rec, "brain_prep", "condition", default="no_brain")
        if prep_cond not in ("no_brain", None):
            flags.append(f"A:no_brain_prepped_brain({prep_cond})")
        if any("/.entire" in f or f.startswith(".entire") or "/.benchmark" in f for f in rec.get("changed_files", [])):
            flags.append("A:no_brain_touched_private")

    # B. mcp authenticity
    if cond in MCP_CONDITIONS:
        names = activity.get("mcp_tool_names") or []
        real = [n for n in names if re.search(rf"(?:^|__){MCP_BRAIN_TOOL_RE}$", str(n))]
        real_bare_names = {bare_mcp_tool_name(n) for n in real}
        logged_bare_names = set(slog_names or [])
        radar_requires_deletions = record_radar_requires_deletions(rec)
        if mcp_calls <= 0:
            # An mcp run with no tool calls is an HONEST FAILURE (note), UNLESS it was
            # counted as a passing/win result -> then it is a HARD flag (false win).
            (flags if is_win else notes).append(
                "B:mcp_win_without_tool_calls" if is_win else "B:mcp_no_tool_calls(honest_fail)")
        if mcp_calls > 0 and not real:
            flags.append("B:mcp_calls_without_real_brain_names")
        if cond == "mcp_history" and mcp_calls > 0:
            # Required tools are model-aware: compact-delivery models (Opus/gpt-5.5)
            # are told to call brain_brief only, so brief-only is COMPLETE for them,
            # not a partial. Other models are expected to also call brain_search.
            required = ("brain_brief",) if compact else ("brain_brief", "brain_search")
            for req in required:
                if not any(str(n).endswith(f"__{req}") or n == req for n in names):
                    notes.append(f"B:mcp_history_partial_missing_{req}")
        if cond == "mcp_workspace_radar" and mcp_calls > 0:
            req = "brain_workspace_regressions"
            if not any(str(n).endswith(f"__{req}") or n == req for n in names):
                notes.append(f"B:mcp_workspace_radar_partial_missing_{req}")
            if not activity_has_mcp_call_with_args(activity, req, {"location_only": True}):
                flags.append("B:mcp_workspace_radar_missing_location_only")
            if radar_requires_deletions and not activity_has_mcp_call_with_args(activity, req, {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_workspace_radar_missing_include_deletions")
        if cond == "mcp_history" and env_flags.get("BENCH_RADAR_LOCATION_ONLY") == "1" and mcp_calls > 0:
            if not activity_has_mcp_call_with_args(activity, "brain_regressions", {"location_only": True}):
                flags.append("B:mcp_radar_missing_location_only")
            if radar_requires_deletions and not activity_has_mcp_call_with_args(activity, "brain_regressions", {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_radar_missing_include_deletions")
        # server-log cross-check: recorded calls must be backed by real tools/call
        if mcp_calls > 0 and slog is None:
            flags.append("B:mcp_server_log_missing")
        if slog is not None and mcp_calls > 0 and slog == 0:
            flags.append("B:mcp_calls_not_in_server_log(faked_stdout)")
        if slog_names:
            missing_from_log = sorted(real_bare_names - logged_bare_names)
            if missing_from_log:
                flags.append("B:mcp_tool_names_not_in_server_log(" + ",".join(missing_from_log) + ")")
        if required_logged_tools:
            missing_required = sorted(required_logged_tools - logged_bare_names)
            if missing_required:
                flags.append("B:mcp_required_tool_names_not_in_server_log(" + ",".join(missing_required) + ")")
        if slog == 0 and mcp_calls <= 0:
            pass  # consistent honest failure

    # C. fairness baseline. create_worktree() applies a parentless reset for ALL runs;
    # only flag an ACTUAL non-zero parent. Missing in-record attestation -> note only.
    pc = get(rec, "agent_baseline_history_reset", "parent_count", default=None)
    if pc is None:
        notes.append("C:fairness_not_attested_in_record")
    elif pc != 0:
        flags.append(f"C:baseline_not_parentless({pc})")
    post = rec.get("post_brain_baseline_history_reset")
    if isinstance(post, dict) and post.get("parent_count") not in (0, None):
        flags.append(f"C:post_brain_baseline_not_parentless({post.get('parent_count')})")

    # D. score integrity (HARD: stored total must equal recomputed clamp(sum(components)))
    if isinstance(score, dict) and "total" in score:
        recomputed = recompute_score_total(score)
        if recomputed is not None and recomputed != int(score.get("total")):
            flags.append(f"D:score_total_mismatch(stored={score.get('total')},recomputed={recomputed})")
        if score.get("version") not in (2, None):
            notes.append(f"D:score_version({score.get('version')})")

    # E. leakage audits (HARD)
    if get(rec, "agent_secret_preflight", "ok") is False:
        flags.append("E:agent_secret_preflight_failed")
    if get(rec, "agent_leak_audit", "ok") is False:
        flags.append("E:agent_leak_audit_failed")
    sh = get(rec, "brain_prep", "history_sanitization")
    if isinstance(sh, dict) and sh.get("ok") is False:
        flags.append("E:history_sanitization_failed")

    # F. validation present
    vres = get(rec, "validation", "results", default=None)
    if not isinstance(vres, list):
        flags.append("F:validation_results_missing")
    elif len(vres) == 0:
        flags.append("F:no_validation_commands_run")

    # H. provenance completeness (HARD). Release evidence must name the exact
    # harness/source revisions and run config that produced each record.
    provenance_flags, provenance_notes, provenance_summary = audit_record_provenance(rec)
    flags.extend(provenance_flags)
    notes.extend(provenance_notes)
    logged_bare_names = set(slog_names or [])
    mcp_named_tool_verified = bool(required_logged_tools) and required_logged_tools.issubset(logged_bare_names)

    # Classify a record as an integrity-verified MCP proof datapoint
    mcp_verified = (
        cond in MCP_CONDITIONS and mcp_calls > 0 and bool(get(rec, "mcp_condition_audit", "ok"))
        and pc == 0 and slog is not None and slog > 0
        and not any(flag.startswith("B:") for flag in flags)
        and not (slog_names and not {bare_mcp_tool_name(n) for n in activity.get("mcp_tool_names") or []}.issubset(set(slog_names)))
    )

    audit = {
        "run_id": run_id,
        "task_id": rec.get("task_id"),
        "condition": cond,
        "delivery_scope": delivery_scope,
        "env_flags": env_flags,
        "repetition": rec.get("repetition"),
        "runner": get(rec, "runner", "id", default=rec.get("agent")),
        "model": get(rec, "runner", "model"),
        "effort": get(rec, "runner", "effort"),
        "ok": rec.get("ok"),
        "valid": get(rec, "validation", "ok"),
        "score": score.get("total"),
        "mcp_calls": mcp_calls,
        "server_toolcalls": slog,
        "search_calls": search_calls,
        "parent_count": pc,
        "mcp_verified": mcp_verified,
        "mcp_named_tool_verified": mcp_named_tool_verified,
        "required_server_tool_names": sorted(required_logged_tools),
        "provenance": provenance_summary,
        "flags": flags,
        "notes": notes,
        "pass": not flags,
    }
    if slog_names is not None:
        audit["server_tool_names"] = slog_names
    return audit


def audit_summary(suite_dir: pathlib.Path, *, min_repetitions_per_side: int = DEFAULT_PROOF_MIN_REPETITIONS) -> list[dict[str, Any]]:
    sj = suite_dir / "summary.json"
    if not sj.exists():
        return []
    try:
        data = json.loads(sj.read_text())
    except json.JSONDecodeError:
        return []
    out = []
    for comp in data.get("comparisons", []):
        flags = []
        verdict = comp.get("verdict", "")
        nc = comp.get("n_condition") or 0
        nb = comp.get("n_baseline") or 0
        if verdict in ("brain_positive",) and (nc < min_repetitions_per_side or nb < min_repetitions_per_side):
            flags.append(f"G:brain_positive_with_small_n(cond={nc},base={nb})")
        if verdict == "brain_positive" and nc != nb:
            flags.append(f"G:unmatched_n(cond={nc},base={nb})")
        if comp.get("proof_ready") and not (nc >= min_repetitions_per_side and nb >= min_repetitions_per_side):
            flags.append(f"G:proof_ready_without_n{min_repetitions_per_side}")
        if comp.get("proof_ready") and get(comp, "stability", "tag") != "brain_positive_stable":
            flags.append("G:proof_ready_without_stable_gate")
        out.append({
            "task": comp.get("task_id"),
            "runner": comp.get("runner"),
            "condition": comp.get("condition"),
            "delivery_scope": comp.get("delivery_scope"),
            "env_flags": comp.get("env_flags"),
            "verdict": verdict,
            "proof_ready": comp.get("proof_ready"),
            "delta": comp.get("delta"),
            "n_condition": nc,
            "n_baseline": nb,
            "reasons": comp.get("verdict_reasons"),
            "flags": flags,
            "pass": not flags,
        })
    return out


def proof_ready_record_backing(comp: dict[str, Any], rec_audits: list[dict[str, Any]]) -> dict[str, Any]:
    task = comp.get("task")
    runner = comp.get("runner")
    condition = comp.get("condition")
    delivery_scope = comp.get("delivery_scope")
    required_condition = int(comp.get("n_condition") or 0)
    required_baseline = int(comp.get("n_baseline") or 0)
    condition_requires_mcp_verified = condition in MCP_CONDITIONS

    def matches(record: dict[str, Any], cond: str, *, require_success: bool) -> bool:
        if not (
            record.get("pass")
            and get(record, "provenance", "ok")
            and record.get("task_id") == task
            and record.get("runner") == runner
            and record.get("condition") == cond
        ):
            return False
        if cond != "no_brain" and delivery_scope and record.get("delivery_scope") != delivery_scope:
            return False
        if require_success:
            return record.get("ok") is True and record.get("valid") is True
        return (
            "ok" in record
            and "valid" in record
        )

    condition_matches = [r for r in rec_audits if matches(r, condition, require_success=True)]
    baseline_matches = [r for r in rec_audits if matches(r, "no_brain", require_success=False)]
    condition_mcp_verified_matches = [r for r in condition_matches if r.get("mcp_verified")]
    condition_run_ids = {r.get("run_id") for r in condition_matches if r.get("run_id")}
    baseline_run_ids = {r.get("run_id") for r in baseline_matches if r.get("run_id")}
    condition_mcp_verified_run_ids = {r.get("run_id") for r in condition_mcp_verified_matches if r.get("run_id")}
    condition_repetitions = {r.get("repetition") for r in condition_matches if r.get("repetition") is not None}
    baseline_repetitions = {r.get("repetition") for r in baseline_matches if r.get("repetition") is not None}
    condition_mcp_verified_repetitions = {
        r.get("repetition") for r in condition_mcp_verified_matches if r.get("repetition") is not None
    }
    condition_records_ok = (
        len(condition_run_ids) >= required_condition
        and len(condition_repetitions) >= required_condition
        and required_condition > 0
    )
    baseline_records_ok = (
        len(baseline_run_ids) >= required_baseline
        and len(baseline_repetitions) >= required_baseline
        and required_baseline > 0
    )
    condition_mcp_verified_ok = (
        not condition_requires_mcp_verified
        or (
            len(condition_mcp_verified_run_ids) >= required_condition
            and len(condition_mcp_verified_repetitions) >= required_condition
            and required_condition > 0
        )
    )
    return {
        "condition_records": len(condition_matches),
        "baseline_records": len(baseline_matches),
        "condition_mcp_verified_records": len(condition_mcp_verified_matches),
        "condition_unique_run_ids": len(condition_run_ids),
        "baseline_unique_run_ids": len(baseline_run_ids),
        "condition_mcp_verified_unique_run_ids": len(condition_mcp_verified_run_ids),
        "condition_unique_repetitions": len(condition_repetitions),
        "baseline_unique_repetitions": len(baseline_repetitions),
        "condition_mcp_verified_unique_repetitions": len(condition_mcp_verified_repetitions),
        "required_condition": required_condition,
        "required_baseline": required_baseline,
        "required_delivery_scope": delivery_scope,
        "condition_requires_mcp_verified": condition_requires_mcp_verified,
        "condition_records_ok": condition_records_ok,
        "baseline_records_ok": baseline_records_ok,
        "condition_mcp_verified_ok": condition_mcp_verified_ok,
        "ok": condition_records_ok and baseline_records_ok and condition_mcp_verified_ok,
    }


def comparison_proof_scope(comp: dict[str, Any]) -> str:
    delivery_scope = comp.get("delivery_scope")
    if delivery_scope in {"mcp", "mcp_radar_location_only", "mcp_radar_answer_assisted", "mcp_workspace_radar_location_only"}:
        return str(delivery_scope)
    condition = comp.get("condition")
    if condition in SEMANTIC_CONDITIONS:
        return "semantic"
    if condition in HISTORY_CONDITIONS:
        return "history"
    return "other"


def suite_matches(name: str, suite_globs: list[str]) -> bool:
    return any(fnmatch.fnmatch(name, pattern) for pattern in suite_globs)


def load_release_manifest(path: pathlib.Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text())
    except FileNotFoundError as exc:
        raise SystemExit(f"release manifest not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit(f"release manifest is not valid JSON: {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise SystemExit(f"release manifest must be a JSON object: {path}")
    globs = data.get("release_citable_suite_globs")
    if not isinstance(globs, list) or not globs or not all(isinstance(g, str) and g for g in globs):
        raise SystemExit("release manifest requires non-empty release_citable_suite_globs")
    minimums = data.get("minimums")
    if not isinstance(minimums, dict):
        raise SystemExit("release manifest requires minimums")
    forbidden = data.get("forbidden_suite_globs", [])
    if not isinstance(forbidden, list) or not all(isinstance(g, str) and g for g in forbidden):
        raise SystemExit("release manifest forbidden_suite_globs must be a string list")
    min_reps = data.get("min_repetitions_per_side", DEFAULT_PROOF_MIN_REPETITIONS)
    if not isinstance(min_reps, int) or min_reps < 1:
        raise SystemExit("release manifest min_repetitions_per_side must be a positive integer")
    require_per_suite = data.get("require_proof_ready_per_suite", False)
    if not isinstance(require_per_suite, bool):
        raise SystemExit("release manifest require_proof_ready_per_suite must be a boolean")
    required_scopes = data.get("required_proof_scopes", [])
    if not isinstance(required_scopes, list) or not all(isinstance(s, str) and s for s in required_scopes):
        raise SystemExit("release manifest required_proof_scopes must be a string list")
    return data


def release_manifest_minimum(manifest: dict[str, Any], key: str, default: int) -> int:
    value = get(manifest, "minimums", key, default=default)
    if not isinstance(value, int) or value < 0:
        raise SystemExit(f"release manifest minimums.{key} must be a non-negative integer")
    return value


def selected_suites(results_dir: pathlib.Path, suite_globs: list[str], forbidden_globs: list[str] | None = None) -> list[pathlib.Path]:
    forbidden_globs = forbidden_globs or []
    suites = []
    for d in results_dir.iterdir():
        if not d.is_dir() or not (d / "records.ndjson").exists():
            continue
        if suite_matches(d.name, forbidden_globs):
            continue
        if suite_matches(d.name, suite_globs):
            suites.append(d)
    return sorted(suites)


def build_audit_report(
    results_dir: pathlib.Path,
    suite_globs: list[str],
    *,
    forbidden_globs: list[str] | None = None,
    min_repetitions_per_side: int = DEFAULT_PROOF_MIN_REPETITIONS,
    require_panel_provenance: bool = False,
) -> dict[str, Any]:
    suites = sorted(
        selected_suites(results_dir, suite_globs, forbidden_globs)
    )
    report: dict[str, Any] = {"suites": {}, "totals": {}}
    total_records = 0
    total_flags = 0
    flag_kinds: dict[str, int] = defaultdict(int)
    note_kinds: dict[str, int] = defaultdict(int)
    mcp_verified_count = 0
    provenance_ok_count = 0
    proof_ready_count = 0
    proof_ready_by_scope: dict[str, int] = defaultdict(int)
    for suite in suites:
        recs = [r for r in load_records(suite) if "__prep__" not in r.get("run_id", "")]
        if not recs:
            continue
        rec_audits = [audit_record(r, suite) for r in recs]
        if require_panel_provenance:
            for rec, audit in zip(recs, rec_audits):
                prov_suite = get(rec, "provenance", "run_config", "suite")
                if prov_suite != suite.name:
                    audit["flags"].append("H:release_suite_provenance_mismatch")
                if not isinstance(get(rec, "provenance", "run_config", "panel"), dict):
                    audit["flags"].append("H:release_panel_provenance_missing")
                host_path_leaks = release_host_path_leaks(rec)
                if host_path_leaks:
                    audit["flags"].append(f"I:release_host_path_leak(count={len(host_path_leaks)})")
                    audit["release_hygiene"] = {
                        "host_path_clean": False,
                        "leak_paths": host_path_leaks,
                    }
                else:
                    audit["release_hygiene"] = {"host_path_clean": True, "leak_paths": []}
                audit["pass"] = not audit["flags"]
        comp_audits = audit_summary(suite, min_repetitions_per_side=min_repetitions_per_side)
        for comp in comp_audits:
            comp["proof_scope"] = comparison_proof_scope(comp)
            backing = proof_ready_record_backing(comp, rec_audits)
            comp["record_backing"] = backing
            if comp.get("proof_ready") and not backing["ok"]:
                if backing.get("condition_requires_mcp_verified") and not backing.get("condition_mcp_verified_ok"):
                    comp["flags"].append("G:proof_ready_without_mcp_verified_condition_records")
                comp["flags"].append("G:proof_ready_without_matching_records")
                comp["pass"] = False
        suite_flags = [a for a in rec_audits if not a["pass"]]
        suite_proofs = sum(1 for c in comp_audits if c.get("proof_ready") and c.get("pass"))
        suite_proofs_by_scope: dict[str, int] = defaultdict(int)
        for comp in comp_audits:
            if comp.get("proof_ready") and comp.get("pass"):
                scope = comp.get("proof_scope") or "other"
                suite_proofs_by_scope[scope] += 1
                proof_ready_by_scope[scope] += 1
        total_records += len(rec_audits)
        mcp_verified_count += sum(1 for a in rec_audits if a["mcp_verified"])
        provenance_ok_count += sum(1 for a in rec_audits if get(a, "provenance", "ok"))
        proof_ready_count += suite_proofs
        for a in rec_audits:
            for f in a["flags"]:
                total_flags += 1
                flag_kinds[f.split("(")[0]] += 1
            for n in a["notes"]:
                note_kinds[n.split("(")[0]] += 1
        for c in comp_audits:
            for f in c["flags"]:
                total_flags += 1
                flag_kinds[f.split("(")[0]] += 1
        report["suites"][suite.name] = {
            "records": rec_audits,
            "comparisons": comp_audits,
            "n_records": len(rec_audits),
            "n_flagged_records": len(suite_flags),
            "n_mcp_verified": sum(1 for a in rec_audits if a["mcp_verified"]),
            "n_provenance_ok": sum(1 for a in rec_audits if get(a, "provenance", "ok")),
            "n_proof_ready_comparisons": suite_proofs,
            "n_proof_ready_comparisons_by_scope": dict(sorted(suite_proofs_by_scope.items())),
        }
    report["totals"] = {
        "suites": len(report["suites"]),
        "records": total_records,
        "hard_flags": total_flags,
        "mcp_verified_records": mcp_verified_count,
        "provenance_ok_records": provenance_ok_count,
        "proof_ready_comparisons": proof_ready_count,
        "proof_ready_comparisons_by_scope": dict(sorted(proof_ready_by_scope.items())),
        "flag_kinds": dict(sorted(flag_kinds.items(), key=lambda x: -x[1])),
        "note_kinds": dict(sorted(note_kinds.items(), key=lambda x: -x[1])),
    }
    return report


def build_gate_status(
    report: dict[str, Any],
    min_suites: int,
    min_records: int,
    min_proof_ready: int,
    min_mcp_verified: int,
    *,
    require_proof_ready_per_suite: bool = False,
    required_proof_scopes: list[str] | None = None,
) -> dict[str, Any]:
    totals = report["totals"]
    failures: list[str] = []
    suites = int(totals.get("suites") or 0)
    records = int(totals.get("records") or 0)
    proof_ready = int(totals.get("proof_ready_comparisons") or 0)
    mcp_verified = int(totals.get("mcp_verified_records") or 0)
    hard_flags = int(totals.get("hard_flags") or 0)
    if suites < min_suites:
        failures.append(f"suites {suites} < required {min_suites}")
    if records < min_records:
        failures.append(f"records {records} < required {min_records}")
    if proof_ready < min_proof_ready:
        failures.append(f"proof_ready_comparisons {proof_ready} < required {min_proof_ready}")
    if mcp_verified < min_mcp_verified:
        failures.append(f"mcp_verified_records {mcp_verified} < required {min_mcp_verified}")
    if hard_flags > 0:
        failures.append(f"hard_flags {hard_flags} > 0")
    if require_proof_ready_per_suite:
        for suite_name, suite_data in sorted((report.get("suites") or {}).items()):
            if int(suite_data.get("n_proof_ready_comparisons") or 0) <= 0:
                failures.append(f"suite {suite_name} has no proof_ready comparison")
    proof_ready_by_scope = totals.get("proof_ready_comparisons_by_scope") or {}
    for scope in required_proof_scopes or []:
        if int(proof_ready_by_scope.get(scope) or 0) <= 0:
            failures.append(f"proof_ready_comparisons[{scope}] 0 < required 1")
    return {
        "status": "fail" if failures else "pass",
        "release_evidence": not failures,
        "requirements": {
            "min_suites": min_suites,
            "min_records": min_records,
            "min_proof_ready": min_proof_ready,
            "min_mcp_verified": min_mcp_verified,
            "hard_flags": 0,
            "require_proof_ready_per_suite": require_proof_ready_per_suite,
            "required_proof_scopes": required_proof_scopes or [],
        },
        "failures": failures,
    }


def render_audit_markdown(report: dict[str, Any]) -> str:
    total_records = report["totals"]["records"]
    total_flags = report["totals"]["hard_flags"]
    mcp_verified_count = report["totals"]["mcp_verified_records"]
    provenance_ok_count = report["totals"].get("provenance_ok_records", 0)
    proof_ready_count = report["totals"].get("proof_ready_comparisons", 0)
    proof_ready_by_scope = report["totals"].get("proof_ready_comparisons_by_scope") or {}
    md = ["# Codex Benchmark Audit (independent re-check)", "",
          f"- Suites audited: **{report['totals']['suites']}**",
          f"- Agent records audited (prep excluded): **{total_records}**",
          f"- **Hard integrity flags: {total_flags}**",
          f"- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **{mcp_verified_count}**",
          f"- Records with required provenance (source base/head + harness/config/tool hashes): **{provenance_ok_count}/{total_records}**",
          f"- Stable proof-ready comparisons: **{proof_ready_count}**",
          ""]
    if proof_ready_by_scope:
        md.append("- Proof-ready comparisons by scope: " + ", ".join(
            f"`{scope}`={count}" for scope, count in sorted(proof_ready_by_scope.items())
        ))
        md.append("")
    gate_status = report.get("gate_status")
    if isinstance(gate_status, dict):
        md.append("## Release Gate")
        if gate_status.get("release_evidence"):
            md.append("**PASS.** This audit satisfies the configured release-evidence gate.")
        else:
            md.append("**NOT RELEASE EVIDENCE.** This audit does not satisfy the configured release-evidence gate.")
        for failure in gate_status.get("failures") or []:
            md.append(f"- {failure}")
        md.append("")
    if report["totals"]["flag_kinds"]:
        md.append("## Hard integrity flags (potential cheating/bias)")
        for k, v in report["totals"]["flag_kinds"].items():
            md.append(f"- `{k}`: {v}")
        md.append("")
    else:
        md.append("## Hard integrity flags\n\n**None.** No record failed an integrity re-check.\n")
    if report["totals"]["note_kinds"]:
        md.append("## Soft notes (honest failures / context, NOT cheating)")
        for k, v in report["totals"]["note_kinds"].items():
            md.append(f"- `{k}`: {v}")
        md.append("")
    md.append("## Per-suite")
    md.append("| Suite | Records | Flagged | Provenance OK | Status |")
    md.append("|---|---:|---:|---:|---|")
    for sname, sdata in report["suites"].items():
        comp_flags = sum(1 for c in sdata["comparisons"] if not c["pass"])
        status = "PASS" if sdata["n_flagged_records"] == 0 and comp_flags == 0 else "FLAG"
        md.append(f"| {sname} | {sdata['n_records']} | {sdata['n_flagged_records']} | {sdata.get('n_provenance_ok', 0)} | {status} |")
    md.append("")
    # Detail every flagged record
    md.append("## Flagged records (detail)")
    any_flag = False
    for sname, sdata in report["suites"].items():
        flagged = [r for r in sdata["records"] if not r["pass"]]
        flagged_c = [c for c in sdata["comparisons"] if not c["pass"]]
        if not flagged and not flagged_c:
            continue
        any_flag = True
        md.append(f"### {sname}")
        for r in flagged:
            md.append(f"- `{r['run_id']}` [{r['condition']}] valid={r['valid']} score={r['score']} mcp={r['mcp_calls']} -> {', '.join(r['flags'])}")
        for c in flagged_c:
            md.append(f"- COMPARISON {c['task']}/{c['runner']}/{c['condition']} verdict={c['verdict']} -> {', '.join(c['flags'])}")
    if not any_flag:
        md.append("None. All audited records passed independent re-checks.")
    return "\n".join(md) + "\n"


def write_audit_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "codex-audit-report.json"
    json_path.write_text(json.dumps(report, indent=2))
    (out_dir / "codex-audit-report.md").write_text(render_audit_markdown(report))
    return json_path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit Entire Brain benchmark records for integrity flags.")
    parser.add_argument("--results", type=pathlib.Path, default=RESULTS, help="Directory containing benchmark suite result directories")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Directory for codex-audit-report.{json,md}; defaults to --results")
    parser.add_argument("--suite-glob", action="append", default=None, help="Only audit suites whose directory name matches this glob; repeatable")
    parser.add_argument("--release-manifest", type=pathlib.Path, default=None, help="Load citable suite globs and release gate minimums from a release evidence manifest")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when any hard integrity flag is found")
    parser.add_argument("--min-suites", type=int, default=1, help="Minimum audited non-empty suites required when --fail-on-flags is set")
    parser.add_argument("--min-records", type=int, default=1, help="Minimum audited agent records required when --fail-on-flags is set")
    parser.add_argument("--min-proof-ready", type=int, default=0, help="Minimum stable proof-ready comparisons required when --fail-on-flags is set")
    parser.add_argument("--min-mcp-verified", type=int, default=0, help="Minimum integrity-verified MCP datapoints required when --fail-on-flags is set")
    parser.add_argument("--min-repetitions-per-side", type=int, default=DEFAULT_PROOF_MIN_REPETITIONS, help="Minimum repetitions per condition required for proof-ready comparisons")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    results_dir = args.results.resolve()
    out_dir = (args.out_dir or results_dir).resolve()
    suite_globs = args.suite_glob or ["*"]
    forbidden_globs: list[str] = []
    min_suites = args.min_suites
    min_records = args.min_records
    min_proof_ready = args.min_proof_ready
    min_mcp_verified = args.min_mcp_verified
    min_repetitions_per_side = args.min_repetitions_per_side
    require_panel_provenance = False
    require_proof_ready_per_suite = False
    required_proof_scopes: list[str] = []
    manifest = None
    if args.release_manifest:
        manifest = load_release_manifest(args.release_manifest.resolve())
        manifest_globs = list(manifest["release_citable_suite_globs"])
        if args.suite_glob and args.suite_glob != manifest_globs:
            raise SystemExit(
                "release manifest suite globs do not match CLI --suite-glob; "
                f"manifest={manifest_globs} cli={args.suite_glob}"
            )
        suite_globs = manifest_globs
        forbidden_globs = list(manifest.get("forbidden_suite_globs") or [])
        min_suites = release_manifest_minimum(manifest, "suites", min_suites)
        min_records = release_manifest_minimum(manifest, "records", min_records)
        min_proof_ready = release_manifest_minimum(manifest, "proof_ready_comparisons", min_proof_ready)
        min_mcp_verified = release_manifest_minimum(manifest, "mcp_verified_records", min_mcp_verified)
        min_repetitions_per_side = int(manifest.get("min_repetitions_per_side", min_repetitions_per_side))
        require_panel_provenance = bool(manifest.get("require_panel_provenance"))
        require_proof_ready_per_suite = bool(manifest.get("require_proof_ready_per_suite"))
        required_proof_scopes = list(manifest.get("required_proof_scopes") or [])
    report = build_audit_report(
        results_dir,
        suite_globs,
        forbidden_globs=forbidden_globs,
        min_repetitions_per_side=min_repetitions_per_side,
        require_panel_provenance=require_panel_provenance,
    )
    if manifest is not None:
        report["release_manifest"] = {
            "path": portable_report_path(args.release_manifest.resolve()),
            "suite_globs": suite_globs,
            "forbidden_suite_globs": forbidden_globs,
            "min_repetitions_per_side": min_repetitions_per_side,
            "min_mcp_verified": min_mcp_verified,
            "require_panel_provenance": require_panel_provenance,
            "require_proof_ready_per_suite": require_proof_ready_per_suite,
            "required_proof_scopes": required_proof_scopes,
        }
    gate_status = None
    if args.fail_on_flags:
        gate_status = build_gate_status(
            report,
            min_suites,
            min_records,
            min_proof_ready,
            min_mcp_verified,
            require_proof_ready_per_suite=require_proof_ready_per_suite,
            required_proof_scopes=required_proof_scopes,
        )
        report["gate_status"] = gate_status
    json_path = write_audit_report(report, out_dir)
    md = render_audit_markdown(report)
    print("\n".join(md.splitlines()[:12]))
    print(f"\nWrote {json_path} and codex-audit-report.md")
    if args.fail_on_flags and gate_status and gate_status["status"] != "pass":
        print("\nAudit is not release evidence:", file=sys.stderr)
        for failure in gate_status["failures"]:
            print(f"- {failure}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
