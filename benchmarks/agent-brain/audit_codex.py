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
  C. fairness baseline    - ordinary Git history is available with private paths filtered.
  D. score integrity      - stored score.total == clamp(sum(components)).
  E. leakage audits       - secret preflight / leak audit / history sanitization ok.
  F. validation present   - the run actually ran the task's validation (non-empty).
Suite-level:
  G. matched comparisons  - brain-positive verdicts compare equal n, same runner.
"""
from __future__ import annotations

import argparse
import fnmatch
import importlib.util
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
MCP_NAMED_TOOL_REQUIRED_SCOPES = {
    "mcp_radar_location_only",
    "mcp_radar_answer_assisted",
    "mcp_workspace_radar_location_only",
}
MCP_BRAIN_TOOL_RE = r"brain_(?:stale|brief|query|search|vsearch|get|multi_get|context|impact|changes|code|tests|boundaries|regressions|review|workspace_regressions|workspace_review)"
SAFE_SERVER_BOOL_TOOL_ARGS = {"blind_spots", "include_deletions", "location_only"}
SAFE_SERVER_STRING_TOOL_ARGS_BY_TOOL = {
    "brain_workspace_regressions": {"workspace"},
    "brain_workspace_review": {"workspace"},
}
WORKSPACE_NAME_RE = re.compile(r"^[A-Za-z0-9._-]+$")
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


def valid_workspace_name(value: Any) -> bool:
    return isinstance(value, str) and bool(WORKSPACE_NAME_RE.fullmatch(value)) and value not in {".", ".."} and bool(value.strip("."))


def sanitize_server_tool_args(raw_args: Any, tool: str | None = None) -> tuple[dict[str, Any], list[dict[str, str]]]:
    if not isinstance(raw_args, dict):
        return {}, [{"key": "<non-object>", "type": type(raw_args).__name__}]
    safe_args: dict[str, Any] = {}
    unsafe_args: list[dict[str, str]] = []
    safe_string_args = SAFE_SERVER_STRING_TOOL_ARGS_BY_TOOL.get(str(tool or ""), set())
    for key, value in raw_args.items():
        key_text = str(key)
        if key_text in SAFE_SERVER_BOOL_TOOL_ARGS and isinstance(value, bool):
            safe_args[key_text] = value
        elif key_text in safe_string_args and valid_workspace_name(value):
            safe_args[key_text] = value
        else:
            unsafe_args.append({"key": key_text, "type": type(value).__name__})
    return safe_args, unsafe_args


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


def server_log_toolcall_responses(run_dir: pathlib.Path) -> int | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    text = log.read_text(errors="ignore")
    return len(re.findall(r"^response: tools/call$", text, flags=re.MULTILINE))


def server_log_tool_names(run_dir: pathlib.Path) -> list[str] | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    text = log.read_text(errors="ignore")
    return re.findall(rf"^tool: ({MCP_BRAIN_TOOL_RE})$", text, flags=re.MULTILINE)


def server_log_tool_results(run_dir: pathlib.Path) -> list[dict[str, str]] | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    text = log.read_text(errors="ignore")
    return [
        {"tool": tool, "status": status}
        for tool, status in re.findall(rf"^tool_result: ({MCP_BRAIN_TOOL_RE}) (ok|error)$", text, flags=re.MULTILINE)
    ]


def server_log_tool_args(run_dir: pathlib.Path) -> list[dict[str, Any]] | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    out: list[dict[str, Any]] = []
    current_tool: str | None = None
    for line in log.read_text(errors="ignore").splitlines():
        tool_match = re.fullmatch(rf"tool: ({MCP_BRAIN_TOOL_RE})", line)
        if tool_match:
            current_tool = tool_match.group(1)
            continue
        args_match = re.fullmatch(r"tool_args: (\{.*\})", line)
        if not args_match or current_tool is None:
            continue
        try:
            raw_args = json.loads(args_match.group(1))
        except json.JSONDecodeError:
            raw_args = {}
        safe_args, _ = sanitize_server_tool_args(raw_args, current_tool)
        out.append({"tool": current_tool, "arguments": safe_args})
    return out


def server_log_tool_call_records(run_dir: pathlib.Path) -> list[dict[str, Any]] | None:
    log = run_dir / "mcp-server.log"
    if not log.exists():
        return None
    out: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None

    def flush_current() -> None:
        nonlocal current
        if current is not None:
            out.append(current)
            current = None

    for line in log.read_text(errors="ignore").splitlines():
        tool_match = re.fullmatch(rf"tool: ({MCP_BRAIN_TOOL_RE})", line)
        if tool_match:
            flush_current()
            current = {"tool": tool_match.group(1), "arguments": {}, "status": ""}
            continue
        args_match = re.fullmatch(r"tool_args: (\{.*\})", line)
        if args_match and current is not None:
            try:
                raw_args = json.loads(args_match.group(1))
            except json.JSONDecodeError:
                raw_args = {}
            safe_args, unsafe_args = sanitize_server_tool_args(raw_args, str(current.get("tool") or ""))
            current["arguments"] = safe_args
            if unsafe_args:
                current["unsafe_arguments"] = unsafe_args
            continue
        result_match = re.fullmatch(rf"tool_result: ({MCP_BRAIN_TOOL_RE}) (ok|error)", line)
        if result_match:
            tool, status = result_match.groups()
            if current is None or current.get("tool") != tool:
                flush_current()
                current = {"tool": tool, "arguments": {}, "status": status}
            else:
                current["status"] = status
            flush_current()
    flush_current()
    return out


def bare_mcp_tool_name(name: Any) -> str:
    text = str(name)
    if "__" in text:
        return text.rsplit("__", 1)[-1]
    return text


def arg_matches(actual: Any, expected: Any) -> bool:
    if isinstance(expected, bool):
        return actual is expected
    if isinstance(expected, str):
        return isinstance(actual, str) and actual == expected
    return actual == expected


def args_match_required(args: dict[str, Any], required_args: dict[str, Any]) -> bool:
    return all(arg_matches(args.get(key), value) for key, value in required_args.items())


def args_match_forbidden(args: dict[str, Any], forbidden_args: dict[str, bool]) -> bool:
    return any(args.get(key) is value for key, value in forbidden_args.items())


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
        if args_match_required(args, required_args):
            return True
    return False


def activity_has_mcp_call_with_forbidden_args(activity: dict[str, Any], tool: str, forbidden_args: dict[str, bool]) -> bool:
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
        if args_match_forbidden(args, forbidden_args):
            return True
    return False


def server_log_has_tool_with_args(tool_calls: list[dict[str, Any]] | None, tool: str, required_args: dict[str, bool]) -> bool:
    if tool_calls is None:
        return False
    for detail in tool_calls:
        if not isinstance(detail, dict) or detail.get("tool") != tool:
            continue
        args = detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}
        if args_match_required(args, required_args):
            return True
    return False


def server_log_has_tool_with_forbidden_args(tool_calls: list[dict[str, Any]] | None, tool: str, forbidden_args: dict[str, bool]) -> bool:
    if tool_calls is None:
        return False
    for detail in tool_calls:
        if not isinstance(detail, dict) or detail.get("tool") != tool:
            continue
        args = detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}
        if args_match_forbidden(args, forbidden_args):
            return True
    return False


def server_log_has_successful_tool_with_args(
    tool_calls: list[dict[str, Any]] | None,
    tool: str,
    required_args: dict[str, bool],
    forbidden_args: dict[str, bool] | None = None,
) -> bool:
    if tool_calls is None:
        return False
    forbidden_args = forbidden_args or {}
    for detail in tool_calls:
        if not isinstance(detail, dict) or detail.get("tool") != tool or detail.get("status") != "ok":
            continue
        args = detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}
        if args_match_required(args, required_args) and not args_match_forbidden(args, forbidden_args):
            return True
    return False


def server_log_unsafe_tool_args(tool_calls: list[dict[str, Any]] | None) -> list[dict[str, Any]]:
    if tool_calls is None:
        return []
    out: list[dict[str, Any]] = []
    for detail in tool_calls:
        if not isinstance(detail, dict):
            continue
        unsafe = detail.get("unsafe_arguments")
        if not isinstance(unsafe, list) or not unsafe:
            continue
        out.append({
            "tool": detail.get("tool"),
            "unsafe_arguments": unsafe,
        })
    return out


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


def record_workspace_name(rec: dict[str, Any]) -> str | None:
    raw = get(rec, "provenance", "run_config", "workspace_name")
    return raw if valid_workspace_name(raw) else None


def required_server_tool_args(
    condition: str,
    delivery_scope: str,
    radar_requires_deletions: bool,
    workspace_name: str | None = None,
) -> dict[str, dict[str, Any]]:
    required: dict[str, dict[str, Any]] = {}
    if condition == "mcp_workspace_radar" or delivery_scope == "mcp_workspace_radar_location_only":
        required["brain_workspace_regressions"] = {"location_only": True}
        if workspace_name is not None:
            required["brain_workspace_regressions"]["workspace"] = workspace_name
    elif delivery_scope == "mcp_radar_location_only":
        required["brain_regressions"] = {"location_only": True}
    elif delivery_scope == "mcp_radar_answer_assisted":
        required["brain_regressions"] = {}
    if radar_requires_deletions:
        for args in required.values():
            args["include_deletions"] = True
    return required


def forbidden_server_tool_args(condition: str, delivery_scope: str) -> dict[str, dict[str, bool]]:
    if condition == "mcp_history" and delivery_scope == "mcp_radar_answer_assisted":
        return {"brain_regressions": {"location_only": True}}
    return {}


def record_radar_deletion_policy(rec: dict[str, Any]) -> tuple[bool, bool]:
    embedded = get(rec, "provenance", "task", "radar_include_deletions")
    if isinstance(embedded, bool):
        return embedded, True
    return False, False


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


def task_path_candidates(rec: dict[str, Any]) -> list[pathlib.Path]:
    raw_path = get(rec, "provenance", "task", "path")
    task_id = str(rec.get("task_id") or get(rec, "provenance", "task", "id") or "")
    candidates: list[pathlib.Path] = []
    if isinstance(raw_path, str) and raw_path:
        path = pathlib.Path(raw_path)
        candidates.append(path)
        if not path.is_absolute():
            candidates.append((pathlib.Path.cwd() / path).resolve())
            candidates.append((BENCH.parent.parent / path).resolve())
    if task_id:
        candidates.append(TASK_DIR / f"{task_id}.json")
        for path in TASK_DIR.glob("*.json"):
            if path.name == f"{task_id}.json":
                continue
            candidates.append(path)
    seen: set[pathlib.Path] = set()
    out: list[pathlib.Path] = []
    for path in candidates:
        resolved = path.resolve() if not path.is_absolute() else path
        if resolved in seen:
            continue
        seen.add(resolved)
        out.append(resolved)
    return out


def load_task_for_record(rec: dict[str, Any]) -> tuple[dict[str, Any] | None, pathlib.Path | None, str | None]:
    task_id = str(rec.get("task_id") or get(rec, "provenance", "task", "id") or "")
    fallback_mismatches: list[str] = []
    for path in task_path_candidates(rec):
        if not path.exists() or not path.is_file():
            continue
        try:
            data = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(data, dict):
            continue
        if task_id and data.get("id") != task_id:
            fallback_mismatches.append(str(path))
            continue
        return data, path, None
    if fallback_mismatches:
        return None, None, "task_id_mismatch"
    return None, None, "task_not_found"


def file_sha256(path: pathlib.Path | None) -> str | None:
    if path is None:
        return None
    try:
        import hashlib
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError:
        return None


def _run_module():
    """run.py owns the hardened brain-query leak auditor (the B1 confound fix);
    audit-side enforcement delegates to it so the two can never diverge again —
    a diverged, weaker copy living here is exactly how the original B1 leaks
    kept passing this audit after run.py was hardened. run_test.py registers
    run.py under this module name; standalone audit runs load it on demand."""
    existing = sys.modules.get("agent_brain_run")
    if existing is not None:
        return existing
    spec = importlib.util.spec_from_file_location("agent_brain_run", pathlib.Path(__file__).with_name("run.py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def brain_query_leak_findings(task: dict[str, Any]) -> list[dict[str, Any]]:
    """The audit-side B1 leak check, in the legacy finding shape redaction and
    reports expect ({query_index, source, kind, term}). Detection itself is
    run.py's brain_query_leak_audit: identifier-fold and split-identifier
    matching, 3-gram phrase windows, and the always-hidden fix texts the old
    local detector never looked at."""
    queries = [str(query) for query in task.get("brain_queries", []) or []]
    findings: list[dict[str, Any]] = []
    for found in _run_module().brain_query_leak_audit(task)["findings"]:
        query = str(found.get("query", ""))
        findings.append({
            "query_index": queries.index(query) if query in queries else None,
            "source": found.get("where"),
            "kind": found.get("kind"),
            "term": str(found.get("token") or found.get("phrase") or ""),
        })
    return findings


def redact_leak_findings(findings: list[dict[str, Any]], limit: int = 8) -> list[dict[str, Any]]:
    import hashlib
    redacted: list[dict[str, Any]] = []
    for finding in findings[:limit]:
        term = str(finding.get("term", ""))
        redacted.append({
            "query_index": finding.get("query_index"),
            "source": finding.get("source"),
            "kind": finding.get("kind"),
            "term_hash": hashlib.sha256(term.encode()).hexdigest()[:16],
            "term_preview": term[:96],
        })
    return redacted


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

    tools = get(prov, "tools")
    for tool in ("brain", "entire"):
        if not is_sha256(get(prov, "tools", tool, "sha256")):
            flags.append(f"H:provenance_missing_{tool}_tool_sha256")
    provider_tools = []
    if isinstance(tools, dict):
        provider_tools = [
            name
            for name, metadata in tools.items()
            if name not in {"bin", "brain", "entire"}
            and isinstance(metadata, dict)
            and is_sha256(metadata.get("sha256"))
        ]
    if not provider_tools:
        flags.append("H:provenance_missing_semantic_provider_tool_sha256")
    elif len(provider_tools) > 1:
        flags.append("H:provenance_ambiguous_semantic_provider_tools")

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
    slog_responses = server_log_toolcall_responses(run_dir)
    slog_names = server_log_tool_names(run_dir)
    slog_results = server_log_tool_results(run_dir)
    slog_args = server_log_tool_args(run_dir)
    slog_tool_calls = server_log_tool_call_records(run_dir)
    slog_unsafe_args = server_log_unsafe_tool_args(slog_tool_calls)
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
    radar_requires_deletions, radar_policy_attested = record_radar_deletion_policy(rec)
    workspace_name = record_workspace_name(rec)
    required_logged_tool_args = required_server_tool_args(str(cond), delivery_scope, radar_requires_deletions, workspace_name)
    forbidden_logged_tool_args = forbidden_server_tool_args(str(cond), delivery_scope)
    task_data, task_path, task_load_error = load_task_for_record(rec)
    task_hygiene: dict[str, Any] = {
        "loaded": task_data is not None,
        "path": portable_report_path(task_path) if task_path else None,
    }
    if task_load_error:
        task_hygiene["load_error"] = task_load_error
        # A record that CLAIMS a task identity but whose config cannot be
        # resolved gets NO leak check and NO sha-drift check — that must be a
        # hard failure, not a silently weaker audit: deleting or renaming a
        # leaky task file would otherwise launder its records into flag-free,
        # proof-countable evidence. A record with no task provenance at all is
        # already hard-flagged by the provenance checks; no second flag here.
        if get(rec, "provenance", "task", "path") or get(rec, "provenance", "task", "id"):
            flags.append(f"H:{task_load_error}")
    recorded_task_sha = get(rec, "provenance", "task", "config_sha256")
    actual_task_sha = file_sha256(task_path)
    if actual_task_sha:
        task_hygiene["sha256"] = actual_task_sha
    if recorded_task_sha and actual_task_sha and actual_task_sha != recorded_task_sha:
        flags.append("H:task_config_sha256_mismatch")

    # A. no_brain purity (HARD: no_brain must never touch Brain/MCP/CLI/private)
    # Cross-check the record's claimed MCP call count against the server log.
    # A mismatch is a NOTE, not a flag: retained evidence stays auditable, but
    # client-side double-counting (observed: records claiming 2 calls where
    # the log shows one tools/call) must be visible in the report instead of
    # silently passing a "records are cross-checked" story.
    if slog is not None and mcp_calls > 0 and slog != mcp_calls:
        notes.append(f"N:mcp_call_count_mismatch(record={mcp_calls},server_log={slog})")

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
            extra_activity_tools = sorted(real_bare_names - {req})
            if extra_activity_tools:
                flags.append("B:mcp_workspace_radar_extra_brain_tools(" + ",".join(extra_activity_tools) + ")")
            if slog_names is not None:
                extra_server_tools = sorted(logged_bare_names - {req})
                if extra_server_tools:
                    flags.append("B:mcp_workspace_radar_server_extra_brain_tools(" + ",".join(extra_server_tools) + ")")
            if not radar_policy_attested:
                flags.append("H:provenance_missing_radar_include_deletions_policy")
            if workspace_name is None:
                flags.append("H:provenance_missing_workspace_name")
            if not any(str(n).endswith(f"__{req}") or n == req for n in names):
                notes.append(f"B:mcp_workspace_radar_partial_missing_{req}")
            if not activity_has_mcp_call_with_args(activity, req, {"location_only": True}):
                flags.append("B:mcp_workspace_radar_missing_location_only")
            if slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, req, {"location_only": True}):
                flags.append("B:mcp_workspace_radar_server_missing_location_only")
            if workspace_name is not None and not activity_has_mcp_call_with_args(activity, req, {"location_only": True, "workspace": workspace_name}):
                flags.append("B:mcp_workspace_radar_missing_workspace")
            if workspace_name is not None and slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, req, {"location_only": True, "workspace": workspace_name}):
                flags.append("B:mcp_workspace_radar_server_missing_workspace")
            if radar_requires_deletions and not activity_has_mcp_call_with_args(activity, req, {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_workspace_radar_missing_include_deletions")
            if radar_requires_deletions and slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, req, {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_workspace_radar_server_missing_include_deletions")
        if cond == "mcp_history" and env_flags.get("BENCH_RADAR_LOCATION_ONLY") == "1" and mcp_calls > 0:
            if not radar_policy_attested:
                flags.append("H:provenance_missing_radar_include_deletions_policy")
            if not activity_has_mcp_call_with_args(activity, "brain_regressions", {"location_only": True}):
                flags.append("B:mcp_radar_missing_location_only")
            if slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, "brain_regressions", {"location_only": True}):
                flags.append("B:mcp_radar_server_missing_location_only")
            if radar_requires_deletions and not activity_has_mcp_call_with_args(activity, "brain_regressions", {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_radar_missing_include_deletions")
            if radar_requires_deletions and slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, "brain_regressions", {"location_only": True, "include_deletions": True}):
                flags.append("B:mcp_radar_server_missing_include_deletions")
        if cond == "mcp_history" and env_flags.get("BENCH_REGRESSION_RADAR") == "1" and mcp_calls > 0:
            forbidden = forbidden_logged_tool_args.get("brain_regressions", {})
            if forbidden and activity_has_mcp_call_with_forbidden_args(activity, "brain_regressions", forbidden):
                flags.append("B:mcp_radar_answer_assisted_used_location_only")
            if forbidden and slog_tool_calls is not None and server_log_has_tool_with_forbidden_args(slog_tool_calls, "brain_regressions", forbidden):
                flags.append("B:mcp_radar_answer_assisted_server_used_location_only")
            if radar_requires_deletions and not activity_has_mcp_call_with_args(activity, "brain_regressions", {"include_deletions": True}):
                flags.append("B:mcp_radar_answer_assisted_missing_include_deletions")
            if radar_requires_deletions and slog_tool_calls is not None and not server_log_has_tool_with_args(slog_tool_calls, "brain_regressions", {"include_deletions": True}):
                flags.append("B:mcp_radar_answer_assisted_server_missing_include_deletions")
        # server-log cross-check: recorded calls must be backed by real tools/call
        if mcp_calls > 0 and slog is None:
            flags.append("B:mcp_server_log_missing")
        if slog is not None and mcp_calls > 0 and slog == 0:
            flags.append("B:mcp_calls_not_in_server_log(faked_stdout)")
        if slog is not None and slog > 0 and int(slog_responses or 0) < slog:
            flags.append(f"B:mcp_server_log_missing_tool_responses({slog_responses or 0}/{slog})")
        if slog_names:
            missing_from_log = sorted(real_bare_names - logged_bare_names)
            if missing_from_log:
                flags.append("B:mcp_tool_names_not_in_server_log(" + ",".join(missing_from_log) + ")")
        if required_logged_tools:
            missing_required = sorted(required_logged_tools - logged_bare_names)
            if missing_required:
                flags.append("B:mcp_required_tool_names_not_in_server_log(" + ",".join(missing_required) + ")")
            missing_success = sorted(
                tool
                for tool in required_logged_tools
                if not server_log_has_successful_tool_with_args(
                    slog_tool_calls,
                    tool,
                    required_logged_tool_args.get(tool, {}),
                    forbidden_logged_tool_args.get(tool, {}),
                )
            )
            if missing_success:
                if slog_results:
                    flags.append("B:mcp_required_tool_results_not_ok(" + ",".join(missing_success) + ")")
                else:
                    flags.append("B:mcp_required_tool_results_missing(" + ",".join(missing_success) + ")")
        if slog == 0 and mcp_calls <= 0:
            pass  # consistent honest failure

    # C. fairness baseline. New runs retain ordinary source Git history after filtering
    # benchmark/Entire-private paths from every visible revision. Historical evidence used
    # a parentless baseline; keep it auditable as legacy evidence without treating it as a
    # compliant run under the current contract.
    history = rec.get("agent_baseline_history")
    pc = None
    history_compliant = False
    if isinstance(history, dict):
        pc = history.get("parent_count")
        if history.get("mode") != "filtered_source_history":
            flags.append(f"C:unexpected_history_mode({history.get('mode')})")
        if history.get("source_history_available") is not True:
            flags.append("C:source_history_unavailable")
        if history.get("private_paths_filtered") is not True:
            flags.append("C:private_history_paths_visible")
        if int(history.get("parent_count") or 0) < 1:
            flags.append(f"C:baseline_has_no_parent({history.get('parent_count')})")
        history_compliant = (
            history.get("mode") == "filtered_source_history"
            and history.get("source_history_available") is True
            and history.get("private_paths_filtered") is True
            and int(history.get("parent_count") or 0) >= 1
        )
    else:
        legacy = rec.get("agent_baseline_history_reset")
        pc = legacy.get("parent_count") if isinstance(legacy, dict) else None
        if isinstance(legacy, dict) and legacy.get("parent_count") == 0:
            notes.append("C:legacy_parentless_baseline")
            history_compliant = True
        else:
            notes.append("C:fairness_not_attested_in_record")
    post = rec.get("post_brain_baseline_history")
    if isinstance(post, dict):
        if post.get("source_history_available") is not True:
            flags.append("C:post_brain_source_history_unavailable")
        if post.get("private_paths_filtered") is not True:
            flags.append("C:post_brain_private_history_paths_visible")

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
    if get(rec, "brain_cli_condition_audit", "ok") is False:
        flags.append("E:brain_cli_condition_audit_failed")
    sh = get(rec, "brain_prep", "history_sanitization")
    if isinstance(sh, dict) and sh.get("ok") is False:
        flags.append("E:history_sanitization_failed")
    if slog_unsafe_args:
        unsafe_keys = sorted({
            str(arg.get("key"))
            for entry in slog_unsafe_args
            for arg in (entry.get("unsafe_arguments") if isinstance(entry.get("unsafe_arguments"), list) else [])
            if isinstance(arg, dict)
        })
        flags.append("E:mcp_server_log_unsafe_tool_args(" + ",".join(unsafe_keys) + ")")
    if cond in BRAIN_CONDITIONS and isinstance(task_data, dict):
        query_leaks = brain_query_leak_findings(task_data)
        if query_leaks:
            flags.append(f"J:answer_bearing_brain_queries(count={len(query_leaks)})")
            task_hygiene["answer_bearing_brain_queries"] = redact_leak_findings(query_leaks)

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
    if slog_results:
        mcp_named_tool_result_verified = (
            bool(required_logged_tools)
            and all(
                server_log_has_successful_tool_with_args(
                    slog_tool_calls,
                    tool,
                    required_logged_tool_args.get(tool, {}),
                    forbidden_logged_tool_args.get(tool, {}),
                )
                for tool in required_logged_tools
            )
        )
    else:
        mcp_named_tool_result_verified = False
    if required_logged_tools:
        mcp_named_tool_completed = mcp_named_tool_result_verified
    else:
        mcp_named_tool_completed = (
            mcp_named_tool_result_verified
            or (
                mcp_named_tool_verified
                and int(slog or 0) > 0
                and int(slog_responses or 0) >= int(slog or 0)
                and rec.get("ok") is True
                and get(rec, "validation", "ok") is True
            )
        )

    # Classify a record as an integrity-verified MCP proof datapoint
    mcp_verified = (
        cond in MCP_CONDITIONS and mcp_calls > 0 and bool(get(rec, "mcp_condition_audit", "ok"))
        and history_compliant and slog is not None and slog > 0 and int(slog_responses or 0) >= int(slog or 0)
        and not flags
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
        "server_toolcall_responses": slog_responses,
        "search_calls": search_calls,
        "parent_count": pc,
        "mcp_verified": mcp_verified,
        "mcp_named_tool_verified": mcp_named_tool_verified,
        "mcp_named_tool_completed": mcp_named_tool_completed,
        "mcp_named_tool_result_verified": mcp_named_tool_result_verified,
        "required_server_tool_names": sorted(required_logged_tools),
        "provenance": provenance_summary,
        "flags": flags,
        "notes": notes,
        "pass": not flags,
    }
    if task_hygiene.get("loaded") or task_hygiene.get("load_error"):
        audit["task_hygiene"] = task_hygiene
    if slog_names is not None:
        audit["server_tool_names"] = slog_names
    if slog_results is not None:
        audit["server_tool_results"] = slog_results
    if slog_args:
        audit["server_tool_args"] = slog_args
    if slog_unsafe_args:
        audit["server_tool_unsafe_args"] = slog_unsafe_args
    if slog_tool_calls:
        audit["server_tool_calls"] = slog_tool_calls
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
    condition_requires_mcp_named_tool_verified = delivery_scope in MCP_NAMED_TOOL_REQUIRED_SCOPES

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
    condition_mcp_named_tool_verified_matches = [r for r in condition_matches if r.get("mcp_named_tool_verified")]
    condition_mcp_named_tool_completed_matches = [r for r in condition_matches if r.get("mcp_named_tool_completed")]
    condition_run_ids = {r.get("run_id") for r in condition_matches if r.get("run_id")}
    baseline_run_ids = {r.get("run_id") for r in baseline_matches if r.get("run_id")}
    condition_mcp_verified_run_ids = {r.get("run_id") for r in condition_mcp_verified_matches if r.get("run_id")}
    condition_mcp_named_tool_verified_run_ids = {
        r.get("run_id") for r in condition_mcp_named_tool_verified_matches if r.get("run_id")
    }
    condition_mcp_named_tool_completed_run_ids = {
        r.get("run_id") for r in condition_mcp_named_tool_completed_matches if r.get("run_id")
    }
    condition_repetitions = {r.get("repetition") for r in condition_matches if r.get("repetition") is not None}
    baseline_repetitions = {r.get("repetition") for r in baseline_matches if r.get("repetition") is not None}
    condition_mcp_verified_repetitions = {
        r.get("repetition") for r in condition_mcp_verified_matches if r.get("repetition") is not None
    }
    condition_mcp_named_tool_verified_repetitions = {
        r.get("repetition") for r in condition_mcp_named_tool_verified_matches if r.get("repetition") is not None
    }
    condition_mcp_named_tool_completed_repetitions = {
        r.get("repetition") for r in condition_mcp_named_tool_completed_matches if r.get("repetition") is not None
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
    condition_mcp_named_tool_verified_ok = (
        len(condition_mcp_named_tool_verified_run_ids) >= required_condition
        and len(condition_mcp_named_tool_verified_repetitions) >= required_condition
        and required_condition > 0
    )
    condition_mcp_named_tool_completed_ok = (
        len(condition_mcp_named_tool_completed_run_ids) >= required_condition
        and len(condition_mcp_named_tool_completed_repetitions) >= required_condition
        and required_condition > 0
    )
    condition_mcp_named_tool_required_ok = (
        not condition_requires_mcp_named_tool_verified
        or (condition_mcp_named_tool_verified_ok and condition_mcp_named_tool_completed_ok)
    )
    return {
        "condition_records": len(condition_matches),
        "baseline_records": len(baseline_matches),
        "condition_mcp_verified_records": len(condition_mcp_verified_matches),
        "condition_mcp_named_tool_verified_records": len(condition_mcp_named_tool_verified_matches),
        "condition_mcp_named_tool_completed_records": len(condition_mcp_named_tool_completed_matches),
        "condition_unique_run_ids": len(condition_run_ids),
        "baseline_unique_run_ids": len(baseline_run_ids),
        "condition_mcp_verified_unique_run_ids": len(condition_mcp_verified_run_ids),
        "condition_mcp_named_tool_verified_unique_run_ids": len(condition_mcp_named_tool_verified_run_ids),
        "condition_mcp_named_tool_completed_unique_run_ids": len(condition_mcp_named_tool_completed_run_ids),
        "condition_unique_repetitions": len(condition_repetitions),
        "baseline_unique_repetitions": len(baseline_repetitions),
        "condition_mcp_verified_unique_repetitions": len(condition_mcp_verified_repetitions),
        "condition_mcp_named_tool_verified_unique_repetitions": len(condition_mcp_named_tool_verified_repetitions),
        "condition_mcp_named_tool_completed_unique_repetitions": len(condition_mcp_named_tool_completed_repetitions),
        "required_condition": required_condition,
        "required_baseline": required_baseline,
        "required_delivery_scope": delivery_scope,
        "condition_requires_mcp_verified": condition_requires_mcp_verified,
        "condition_requires_mcp_named_tool_verified": condition_requires_mcp_named_tool_verified,
        "condition_records_ok": condition_records_ok,
        "baseline_records_ok": baseline_records_ok,
        "condition_mcp_verified_ok": condition_mcp_verified_ok,
        "condition_mcp_named_tool_verified_ok": condition_mcp_named_tool_verified_ok,
        "condition_mcp_named_tool_completed_ok": condition_mcp_named_tool_completed_ok,
        "condition_mcp_named_tool_required_ok": condition_mcp_named_tool_required_ok,
        "ok": (
            condition_records_ok
            and baseline_records_ok
            and condition_mcp_verified_ok
            and condition_mcp_named_tool_required_ok
        ),
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
    required_named_scopes = data.get("required_named_tool_proof_scopes", [])
    if not isinstance(required_named_scopes, list) or not all(isinstance(s, str) and s for s in required_named_scopes):
        raise SystemExit("release manifest required_named_tool_proof_scopes must be a string list")
    claim_policy = data.get("claim_policy", "proof_required")
    if claim_policy not in {"proof_required", "no_release_claim"}:
        raise SystemExit("release manifest claim_policy must be proof_required or no_release_claim")
    allowed_no_claim_flags = data.get("allowed_no_claim_flag_kinds", [])
    if not isinstance(allowed_no_claim_flags, list) or not all(isinstance(s, str) and s for s in allowed_no_claim_flags):
        raise SystemExit("release manifest allowed_no_claim_flag_kinds must be a string list")
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
    mcp_named_tool_verified_count = 0
    mcp_named_tool_completed_count = 0
    provenance_ok_count = 0
    proof_ready_count = 0
    proof_ready_by_scope: dict[str, int] = defaultdict(int)
    named_tool_proof_ready_by_scope: dict[str, int] = defaultdict(int)
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
                if (
                    backing.get("condition_requires_mcp_named_tool_verified")
                    and not backing.get("condition_mcp_named_tool_verified_ok")
                ):
                    comp["flags"].append("G:proof_ready_without_named_mcp_tool_condition_records")
                if (
                    backing.get("condition_requires_mcp_named_tool_verified")
                    and not backing.get("condition_mcp_named_tool_completed_ok")
                ):
                    comp["flags"].append("G:proof_ready_without_completed_named_mcp_tool_condition_records")
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
                backing = comp.get("record_backing") if isinstance(comp.get("record_backing"), dict) else {}
                if backing.get("condition_mcp_named_tool_verified_ok") and backing.get("condition_mcp_named_tool_completed_ok"):
                    named_tool_proof_ready_by_scope[scope] += 1
        total_records += len(rec_audits)
        mcp_verified_count += sum(1 for a in rec_audits if a["mcp_verified"])
        mcp_named_tool_verified_count += sum(1 for a in rec_audits if a["mcp_named_tool_verified"])
        mcp_named_tool_completed_count += sum(1 for a in rec_audits if a["mcp_named_tool_completed"])
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
            "n_mcp_named_tool_verified": sum(1 for a in rec_audits if a["mcp_named_tool_verified"]),
            "n_mcp_named_tool_completed": sum(1 for a in rec_audits if a["mcp_named_tool_completed"]),
            "n_provenance_ok": sum(1 for a in rec_audits if get(a, "provenance", "ok")),
            "n_proof_ready_comparisons": suite_proofs,
            "n_proof_ready_comparisons_by_scope": dict(sorted(suite_proofs_by_scope.items())),
        }
    report["totals"] = {
        "suites": len(report["suites"]),
        "records": total_records,
        "hard_flags": total_flags,
        "mcp_verified_records": mcp_verified_count,
        "mcp_named_tool_verified_records": mcp_named_tool_verified_count,
        "mcp_named_tool_completed_records": mcp_named_tool_completed_count,
        "provenance_ok_records": provenance_ok_count,
        "proof_ready_comparisons": proof_ready_count,
        "proof_ready_comparisons_by_scope": dict(sorted(proof_ready_by_scope.items())),
        "named_tool_proof_ready_comparisons_by_scope": dict(sorted(named_tool_proof_ready_by_scope.items())),
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
    min_mcp_named_tool_verified: int,
    *,
    claim_policy: str = "proof_required",
    allowed_no_claim_flag_kinds: list[str] | None = None,
    require_proof_ready_per_suite: bool = False,
    required_proof_scopes: list[str] | None = None,
    required_named_tool_proof_scopes: list[str] | None = None,
) -> dict[str, Any]:
    totals = report["totals"]
    failures: list[str] = []
    suites = int(totals.get("suites") or 0)
    records = int(totals.get("records") or 0)
    proof_ready = int(totals.get("proof_ready_comparisons") or 0)
    mcp_verified = int(totals.get("mcp_verified_records") or 0)
    mcp_named_tool_verified = int(totals.get("mcp_named_tool_verified_records") or 0)
    hard_flags = int(totals.get("hard_flags") or 0)
    flag_kinds = totals.get("flag_kinds") if isinstance(totals.get("flag_kinds"), dict) else {}
    allowed_no_claim_flag_kinds = allowed_no_claim_flag_kinds or []
    if claim_policy == "no_release_claim":
        if suites < min_suites:
            failures.append(f"suites {suites} < required {min_suites}")
        if records < min_records:
            failures.append(f"records {records} < required {min_records}")
        if proof_ready > 0:
            failures.append(f"no_release_claim has proof_ready_comparisons {proof_ready} > 0")
        unexpected = sorted(kind for kind in flag_kinds if kind not in set(allowed_no_claim_flag_kinds))
        if unexpected:
            failures.append("unexpected no_release_claim flag kinds: " + ", ".join(unexpected))
        return {
            "status": "fail" if failures else "pass",
            "release_evidence": False,
            "claim_policy": claim_policy,
            "requirements": {
                "min_suites": min_suites,
                "min_records": min_records,
                "proof_ready_comparisons": 0,
                "allowed_no_claim_flag_kinds": allowed_no_claim_flag_kinds,
            },
            "failures": failures,
        }
    if suites < min_suites:
        failures.append(f"suites {suites} < required {min_suites}")
    if records < min_records:
        failures.append(f"records {records} < required {min_records}")
    if proof_ready < min_proof_ready:
        failures.append(f"proof_ready_comparisons {proof_ready} < required {min_proof_ready}")
    if mcp_verified < min_mcp_verified:
        failures.append(f"mcp_verified_records {mcp_verified} < required {min_mcp_verified}")
    if mcp_named_tool_verified < min_mcp_named_tool_verified:
        failures.append(
            f"mcp_named_tool_verified_records {mcp_named_tool_verified} < required {min_mcp_named_tool_verified}"
        )
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
    named_tool_proof_ready_by_scope = totals.get("named_tool_proof_ready_comparisons_by_scope") or {}
    for scope in required_named_tool_proof_scopes or []:
        if int(named_tool_proof_ready_by_scope.get(scope) or 0) <= 0:
            failures.append(f"named_tool_proof_ready_comparisons[{scope}] 0 < required 1")
    return {
        "status": "fail" if failures else "pass",
        "release_evidence": not failures,
        "claim_policy": claim_policy,
        "requirements": {
            "min_suites": min_suites,
            "min_records": min_records,
            "min_proof_ready": min_proof_ready,
            "min_mcp_verified": min_mcp_verified,
            "min_mcp_named_tool_verified": min_mcp_named_tool_verified,
            "hard_flags": 0,
            "require_proof_ready_per_suite": require_proof_ready_per_suite,
            "required_proof_scopes": required_proof_scopes or [],
            "required_named_tool_proof_scopes": required_named_tool_proof_scopes or [],
        },
        "failures": failures,
    }


def render_audit_markdown(report: dict[str, Any]) -> str:
    total_records = report["totals"]["records"]
    total_flags = report["totals"]["hard_flags"]
    mcp_verified_count = report["totals"]["mcp_verified_records"]
    mcp_named_tool_verified_count = report["totals"].get("mcp_named_tool_verified_records", 0)
    mcp_named_tool_completed_count = report["totals"].get("mcp_named_tool_completed_records", 0)
    provenance_ok_count = report["totals"].get("provenance_ok_records", 0)
    proof_ready_count = report["totals"].get("proof_ready_comparisons", 0)
    proof_ready_by_scope = report["totals"].get("proof_ready_comparisons_by_scope") or {}
    named_tool_proof_ready_by_scope = report["totals"].get("named_tool_proof_ready_comparisons_by_scope") or {}
    md = ["# Codex Benchmark Audit (independent re-check)", "",
          f"- Suites audited: **{report['totals']['suites']}**",
          f"- Agent records audited (prep excluded): **{total_records}**",
          f"- **Hard integrity flags: {total_flags}**",
          f"- Integrity-verified MCP datapoints (real calls + isolated Git baseline + server-log backed): **{mcp_verified_count}**",
          f"- Named-tool MCP datapoints (server log names the required brain tool): **{mcp_named_tool_verified_count}**",
          f"- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **{mcp_named_tool_completed_count}**",
          f"- Records with required provenance (source base/head + harness/config/tool hashes): **{provenance_ok_count}/{total_records}**",
          f"- Stable proof-ready comparisons: **{proof_ready_count}**",
          ""]
    if proof_ready_by_scope:
        md.append("- Proof-ready comparisons by scope: " + ", ".join(
            f"`{scope}`={count}" for scope, count in sorted(proof_ready_by_scope.items())
        ))
        md.append("")
    if named_tool_proof_ready_by_scope:
        md.append("- Named-tool proof-ready comparisons by scope: " + ", ".join(
            f"`{scope}`={count}" for scope, count in sorted(named_tool_proof_ready_by_scope.items())
        ))
        md.append("")
    gate_status = report.get("gate_status")
    if isinstance(gate_status, dict):
        md.append("## Release Gate")
        if gate_status.get("claim_policy") == "no_release_claim" and gate_status.get("status") == "pass":
            md.append("**PASS (NO RELEASE CLAIM).** This audit satisfies the retained no-claim gate and is not citable proof.")
        elif gate_status.get("release_evidence"):
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
    parser.add_argument("--min-mcp-named-tool-verified", type=int, default=0, help="Minimum MCP datapoints whose server log names the required brain tool")
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
    min_mcp_named_tool_verified = args.min_mcp_named_tool_verified
    min_repetitions_per_side = args.min_repetitions_per_side
    require_panel_provenance = False
    require_proof_ready_per_suite = False
    required_proof_scopes: list[str] = []
    required_named_tool_proof_scopes: list[str] = []
    claim_policy = "proof_required"
    allowed_no_claim_flag_kinds: list[str] = []
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
        min_mcp_named_tool_verified = release_manifest_minimum(
            manifest, "mcp_named_tool_verified_records", min_mcp_named_tool_verified
        )
        min_repetitions_per_side = int(manifest.get("min_repetitions_per_side", min_repetitions_per_side))
        require_panel_provenance = bool(manifest.get("require_panel_provenance"))
        require_proof_ready_per_suite = bool(manifest.get("require_proof_ready_per_suite"))
        required_proof_scopes = list(manifest.get("required_proof_scopes") or [])
        required_named_tool_proof_scopes = list(manifest.get("required_named_tool_proof_scopes") or [])
        claim_policy = str(manifest.get("claim_policy", claim_policy))
        allowed_no_claim_flag_kinds = list(manifest.get("allowed_no_claim_flag_kinds") or [])
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
            "min_mcp_named_tool_verified": min_mcp_named_tool_verified,
            "require_panel_provenance": require_panel_provenance,
            "require_proof_ready_per_suite": require_proof_ready_per_suite,
            "required_proof_scopes": required_proof_scopes,
            "required_named_tool_proof_scopes": required_named_tool_proof_scopes,
            "claim_policy": claim_policy,
            "allowed_no_claim_flag_kinds": allowed_no_claim_flag_kinds,
        }
    gate_status = None
    if args.fail_on_flags:
        gate_status = build_gate_status(
            report,
            min_suites,
            min_records,
            min_proof_ready,
            min_mcp_verified,
            min_mcp_named_tool_verified,
            claim_policy=claim_policy,
            allowed_no_claim_flag_kinds=allowed_no_claim_flag_kinds,
            require_proof_ready_per_suite=require_proof_ready_per_suite,
            required_proof_scopes=required_proof_scopes,
            required_named_tool_proof_scopes=required_named_tool_proof_scopes,
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
