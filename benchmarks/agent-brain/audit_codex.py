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
  B. mcp authenticity     - mcp_* runs must have real mcp tool calls that match
                            the server log's tools/call count.
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

MCP_CONDITIONS = {"mcp_semantic", "mcp_history"}
SEMANTIC_CONDITIONS = {"semantic_brain", "semantic_cli", "mcp_semantic"}
HISTORY_CONDITIONS = {"full_brain", "full_cli_original", "full_cli_compact", "mcp_history"}
BRAIN_CONDITIONS = SEMANTIC_CONDITIONS | HISTORY_CONDITIONS


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
    is_win = bool(rec.get("ok")) and (get(rec, "validation", "ok") is True)
    # Compact-delivery models are instructed to call brain_brief ONCE and NOT
    # brain_search (the compact brief already carries the top history hits).
    # Mirrored here independently so a correct compact run is not flagged for a
    # "missing" tool it was explicitly told not to call.
    model = (get(rec, "runner", "model") or rec.get("agent") or "").lower()
    compact = model in {"gpt-5.5", "gpt-5", "opus", "claude-opus-4-8"}

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
        real = [n for n in names if re.search(r"(?:^|__)brain_(?:brief|query|search|vsearch|get|multi_get|context|impact|changes|stale|regressions|review|workspace_regressions|workspace_review)$", str(n))]
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
        # server-log cross-check: recorded calls must be backed by real tools/call
        if slog is not None and mcp_calls > 0 and slog == 0:
            flags.append("B:mcp_calls_not_in_server_log(faked_stdout)")
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
    if vres is not None and len(vres) == 0 and cond != "no_brain":
        notes.append("F:no_validation_commands_run")

    # Classify a record as an integrity-verified MCP proof datapoint
    mcp_verified = (
        cond in MCP_CONDITIONS and mcp_calls > 0 and bool(get(rec, "mcp_condition_audit", "ok"))
        and pc == 0 and (slog is None or slog > 0)
    )

    return {
        "run_id": run_id,
        "condition": cond,
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
        "flags": flags,
        "notes": notes,
        "pass": not flags,
    }


def audit_summary(suite_dir: pathlib.Path) -> list[dict[str, Any]]:
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
        if verdict in ("brain_positive",) and (nc < 3 or nb < 3):
            flags.append(f"G:brain_positive_with_small_n(cond={nc},base={nb})")
        if verdict == "brain_positive" and nc != nb:
            flags.append(f"G:unmatched_n(cond={nc},base={nb})")
        if comp.get("proof_ready") and not (nc >= 3 and nb >= 3):
            flags.append("G:proof_ready_without_n3")
        out.append({
            "task": comp.get("task_id"),
            "runner": comp.get("runner"),
            "condition": comp.get("condition"),
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


def suite_matches(name: str, suite_globs: list[str]) -> bool:
    return any(fnmatch.fnmatch(name, pattern) for pattern in suite_globs)


def build_audit_report(results_dir: pathlib.Path, suite_globs: list[str]) -> dict[str, Any]:
    suites = sorted(
        d for d in results_dir.iterdir()
        if d.is_dir() and (d / "records.ndjson").exists() and suite_matches(d.name, suite_globs)
    )
    report: dict[str, Any] = {"suites": {}, "totals": {}}
    total_records = 0
    total_flags = 0
    flag_kinds: dict[str, int] = defaultdict(int)
    note_kinds: dict[str, int] = defaultdict(int)
    mcp_verified_count = 0
    for suite in suites:
        recs = [r for r in load_records(suite) if "__prep__" not in r.get("run_id", "")]
        if not recs:
            continue
        rec_audits = [audit_record(r, suite) for r in recs]
        comp_audits = audit_summary(suite)
        suite_flags = [a for a in rec_audits if not a["pass"]]
        total_records += len(rec_audits)
        mcp_verified_count += sum(1 for a in rec_audits if a["mcp_verified"])
        for a in rec_audits:
            for f in a["flags"]:
                total_flags += 1
                flag_kinds[f.split("(")[0]] += 1
            for n in a["notes"]:
                note_kinds[n.split("(")[0]] += 1
        report["suites"][suite.name] = {
            "records": rec_audits,
            "comparisons": comp_audits,
            "n_records": len(rec_audits),
            "n_flagged_records": len(suite_flags),
            "n_mcp_verified": sum(1 for a in rec_audits if a["mcp_verified"]),
        }
    report["totals"] = {
        "suites": len(report["suites"]),
        "records": total_records,
        "hard_flags": total_flags,
        "mcp_verified_records": mcp_verified_count,
        "flag_kinds": dict(sorted(flag_kinds.items(), key=lambda x: -x[1])),
        "note_kinds": dict(sorted(note_kinds.items(), key=lambda x: -x[1])),
    }
    return report


def render_audit_markdown(report: dict[str, Any]) -> str:
    total_records = report["totals"]["records"]
    total_flags = report["totals"]["hard_flags"]
    mcp_verified_count = report["totals"]["mcp_verified_records"]
    md = ["# Codex Benchmark Audit (independent re-check)", "",
          f"- Suites audited: **{report['totals']['suites']}**",
          f"- Agent records audited (prep excluded): **{total_records}**",
          f"- **Hard integrity flags: {total_flags}**",
          f"- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **{mcp_verified_count}**",
          ""]
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
    md.append("| Suite | Records | Flagged | Status |")
    md.append("|---|---|---|---|")
    for sname, sdata in report["suites"].items():
        comp_flags = sum(1 for c in sdata["comparisons"] if not c["pass"])
        status = "PASS" if sdata["n_flagged_records"] == 0 and comp_flags == 0 else "FLAG"
        md.append(f"| {sname} | {sdata['n_records']} | {sdata['n_flagged_records']} | {status} |")
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
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when any hard integrity flag is found")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    results_dir = args.results.resolve()
    out_dir = (args.out_dir or results_dir).resolve()
    report = build_audit_report(results_dir, args.suite_glob or ["*"])
    json_path = write_audit_report(report, out_dir)
    md = render_audit_markdown(report)
    print("\n".join(md.splitlines()[:12]))
    print(f"\nWrote {json_path} and codex-audit-report.md")
    if args.fail_on_flags and report["totals"]["hard_flags"] > 0:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
