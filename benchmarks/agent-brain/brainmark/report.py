#!/usr/bin/env python3
"""REPORT.md / REPORT.json -- the only sanctioned way to read a BrainMark run.

REFUSES TO AGGREGATE unless, in this order:

  1. the seal verifies -- every sealed task, the miner, prompts.py, mechmetrics.py
     and the vendored estimator hash to what SEAL-MANIFEST.json recorded;
  2. every cell's packet.sha256 still matches the packet bytes on disk;
  3. every arm of a pair shares one prompt-symmetry sha (re-derived from
     prompt.txt, not trusted from the recorded file);
  4. the pair passes the all-arms-clean gate.

Each refusal is a real failure mode, not ceremony: (1) is post-hoc task editing,
(2) is reporting a packet that was never delivered, (3) is arms that got
different instructions, (4) is asymmetric dropping.

PRIMARY (pre-registered): paired ratio of `locate_calls_pre_edit`, on (count+1),
full_brain vs no_brain. Reported as geometric mean with a 20k bootstrap CI AND
the median beside it, because a mean over log-ratios is not robust at n=30 and
memory records an estimator choice flipping a sign.

Secondary: resolved% (McNemar), cost, tokens, duration, turns, prep cost.

The competitor table is the same estimator against mem0 / graphify / cmm.

MDE at n=30 is roughly 25 points. A non-significant result at this n is NOT
evidence of no effect, and the report says so rather than letting a reader
infer a win from a point estimate.
"""

from __future__ import annotations

import argparse
import json
import math
import pathlib
import statistics
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, prompts, seal  # type: ignore[no-redef]
    from brainmark.mechmetrics import RATIO_OFFSET, session_metrics  # type: ignore[no-redef]
else:
    from . import _harness, prompts, seal
    from .mechmetrics import RATIO_OFFSET, session_metrics

sys.path.insert(0, str(_harness.BRAINMARK_DIR / "vendor"))

BASELINE = "no_brain"
HEADLINE = "full_brain"


def _metrics_module():
    import graphmark_metrics  # noqa: PLC0415 - vendored, path-injected above

    return graphmark_metrics


# --------------------------------------------------------------------------
# integrity
# --------------------------------------------------------------------------


def check_integrity(results: pathlib.Path, arms: list[str],
                    require_seal: bool = True) -> tuple[list[str], list[str]]:
    """Returns (blocking_problems, warnings)."""
    problems: list[str] = []
    warnings: list[str] = []

    ok, seal_problems = seal.verify()
    if not ok:
        target = problems if require_seal else warnings
        target.extend(f"seal: {p}" for p in seal_problems)

    for pair_dir in sorted(results.iterdir()):
        if not pair_dir.is_dir():
            continue
        shas: dict[str, str] = {}
        for arm in arms:
            cell = pair_dir / arm
            if not cell.is_dir():
                continue

            packet = cell / "packet.txt"
            pinned = cell / "packet.sha256"
            if packet.is_file() and pinned.is_file():
                actual = _harness.sha256_file(packet)
                expected = pinned.read_text(encoding="utf-8").strip()
                if actual != expected:
                    problems.append(
                        f"{pair_dir.name}/{arm}: packet.sha256 mismatch "
                        f"(pinned {expected[:12]}, on disk {actual[:12]})"
                    )

            prompt_file = cell / "prompt.txt"
            if prompt_file.is_file():
                # RE-DERIVE, never trust the recorded sha.
                shas[arm] = prompts.symmetry_sha(prompt_file.read_text(encoding="utf-8"))

        if len(set(shas.values())) > 1:
            groups: dict[str, list[str]] = {}
            for arm, sha in sorted(shas.items()):
                groups.setdefault(sha, []).append(arm)
            problems.append(
                f"{pair_dir.name}: PROMPT SYMMETRY VIOLATION -- "
                + "; ".join(f"{s[:12]}={a}" for s, a in sorted(groups.items()))
            )
    return problems, warnings


# --------------------------------------------------------------------------
# collection
# --------------------------------------------------------------------------


def collect(results: pathlib.Path, arms: list[str],
            grading: dict | None = None) -> tuple[list[str], dict]:
    """-> (clean_pair_ids, {pair_id: {arm: cellrecord}})."""
    table: dict[str, dict[str, dict]] = {}
    for pair_dir in sorted(results.iterdir()):
        if not pair_dir.is_dir():
            continue
        for arm in arms:
            cell = pair_dir / arm
            meta_path = cell / "meta.json"
            if not meta_path.is_file():
                continue
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            stream = cell / "stream.jsonl"
            mech = meta.get("mechmetrics") or (
                session_metrics(stream) if stream.is_file() else {}
            )
            patch = cell / "patch.diff"
            result_event = meta.get("result_event") or {}
            usd = float(result_event.get("total_cost_usd") or 0)
            table.setdefault(pair_dir.name, {})[arm] = {
                "instance_id": meta.get("instance_id"),
                "usd": usd,
                # The session's own terminal status, recorded but NOT gated on:
                # the pre-registered gate is usd>0 + non-empty patch and is not
                # changed here. See `errored_cells` in build_report().
                "subtype": result_event.get("subtype"),
                "patch_bytes": patch.stat().st_size if patch.is_file() else 0,
                "mech": mech,
                "prep": (meta.get("packet_provenance") or {}).get("prep", {}),
                "clean": usd > 0 and patch.is_file() and patch.stat().st_size > 0,
            }

    clean = sorted(
        pair for pair, cells in table.items()
        if all(cells.get(arm, {}).get("clean") for arm in arms)
    )

    if grading:
        for arm, entry in (grading.get("per_arm") or {}).items():
            resolved = set(entry.get("resolved_ids") or [])
            for pair, cells in table.items():
                if arm in cells:
                    cells[arm]["resolved"] = cells[arm]["instance_id"] in resolved
    return clean, table


# --------------------------------------------------------------------------
# statistics
# --------------------------------------------------------------------------


BOOTSTRAP_DRAWS = 20000


def _log_ratios(table: dict, clean: list[str], treatment: str, control: str,
                extract) -> list[float]:
    """Paired log-ratios. Pairs where the control value is unusable are skipped
    for that metric only -- never for the primary, which uses the +1 offset
    precisely so it can never be undefined."""
    logs: list[float] = []
    for pair in clean:
        t_value = extract(table[pair][treatment])
        c_value = extract(table[pair][control])
        if not t_value or not c_value or t_value <= 0 or c_value <= 0:
            continue
        logs.append(math.log(t_value / c_value))
    return logs


def compare(table: dict, clean: list[str], treatment: str, control: str,
            seed: int = 20260815) -> dict:
    """All estimators come from the VENDORED graphmark metrics, not from here.

    `geo` and `median_ratio` return FRACTIONS (ratio - 1), and `bootstrap_ci`
    takes a statistic over resampled INDICES -- matching graphmark exactly is the
    point of vendoring, so the call shapes below follow its API rather than a
    convenient local one.
    """
    gm = _metrics_module()
    logs: list[float] = []
    deltas: list[int] = []

    for pair in clean:
        t_locate = int(table[pair][treatment]["mech"].get("locate_calls_pre_edit", 0))
        c_locate = int(table[pair][control]["mech"].get("locate_calls_pre_edit", 0))
        logs.append(math.log((t_locate + RATIO_OFFSET) / (c_locate + RATIO_OFFSET)))
        deltas.append(t_locate - c_locate)

    out: dict = {
        "treatment": treatment, "control": control, "n": len(clean),
        "primary_metric": "locate_calls_pre_edit",
        "ratio_offset": RATIO_OFFSET,
        "bootstrap_draws": BOOTSTRAP_DRAWS,
        "bootstrap_seed": seed,
    }
    if not logs:
        out["error"] = "no clean pairs"
        return out

    out["geomean_pct"] = round(gm.geo(logs) * 100, 3)
    out["median_pct"] = round(gm.median_ratio(logs) * 100, 3)
    out["mean_delta"] = round(statistics.fmean(deltas), 4)
    out["median_delta"] = statistics.median(deltas)

    ci = gm.bootstrap_ci(
        lambda idx: gm.geo([logs[i] for i in idx]), len(logs), BOOTSTRAP_DRAWS, seed
    )
    if ci.get("lo") is not None and ci.get("hi") is not None:
        out["bootstrap_ci_pct"] = [round(ci["lo"] * 100, 3), round(ci["hi"] * 100, 3)]
        out["straddles_zero"] = ci["lo"] <= 0 <= ci["hi"]
    out["bootstrap_valid_draws"] = ci.get("valid")

    # Leave-one-out: if dropping any single pair moves the headline across zero,
    # the result is one instance, not an effect.
    loo = gm.loo_range(logs, gm.geo)
    out["loo_range_pct"] = {
        k: (round(v * 100, 3) if isinstance(v, (int, float)) else v)
        for k, v in loo.items()
    }

    for label, extract in (
        ("cost", lambda cell: cell["usd"]),
        ("tokens", lambda cell: (cell["mech"].get("tokens") or {}).get("total_tokens") or 0),
        ("duration", lambda cell: cell["mech"].get("duration_ms") or 0),
        ("turns", lambda cell: cell["mech"].get("num_turns") or 0),
    ):
        sub = _log_ratios(table, clean, treatment, control, extract)
        if sub:
            out[f"{label}_geomean_pct"] = round(gm.geo(sub) * 100, 3)
            out[f"{label}_median_pct"] = round(gm.median_ratio(sub) * 100, 3)
            out[f"{label}_n"] = len(sub)

    if all("resolved" in table[p][treatment] and "resolved" in table[p][control]
           for p in clean):
        res_t = {p: bool(table[p][treatment]["resolved"]) for p in clean}
        res_c = {p: bool(table[p][control]["resolved"]) for p in clean}
        mcnemar = gm.mcnemar_exact(res_t, res_c, clean)
        out["resolved"] = {
            "treatment": sum(res_t.values()),
            "control": sum(res_c.values()),
            **mcnemar,
        }

    prep = [table[p][treatment]["prep"].get("seconds") for p in clean]
    prep = [x for x in prep if isinstance(x, (int, float))]
    if prep:
        out["prep_seconds_median"] = round(statistics.median(prep), 3)
        out["prep_seconds_total"] = round(sum(prep), 3)
    return out


# --------------------------------------------------------------------------
# rendering
# --------------------------------------------------------------------------


def render_markdown(report: dict) -> str:
    lines = ["# BrainMark report", ""]
    lines.append(f"- pairs collected: **{report['pairs_collected']}**")
    lines.append(f"- pairs passing the all-arms-clean gate: **{report['n_clean']}**")
    lines.append(f"- arms: {', '.join(report['arms'])}")
    lines.append(f"- seal: {'VERIFIED' if report['seal_ok'] else 'NOT VERIFIED'}")
    lines.append("")

    if report.get("refused"):
        lines.append("## REFUSED")
        lines.append("")
        lines.append("This run was **not aggregated**. Blocking problems:")
        lines.append("")
        for problem in report["problems"]:
            lines.append(f"- {problem}")
        lines.append("")
        return "\n".join(lines) + "\n"

    n = report["n_clean"]
    lines.append("## Headline -- full_brain vs no_brain")
    lines.append("")
    lines.append(_comparison_table([report["headline"]]))
    lines.append("")
    lines.append("## Competitors (same estimator, same baseline)")
    lines.append("")
    lines.append(_comparison_table(list(report["competitors"].values())))
    lines.append("")

    if report.get("errored_cells"):
        lines.append("## Sensitivity -- cells that did not end in success")
        lines.append("")
        lines.append(
            f"{sum(len(v) for v in report['errored_cells'].values())} cell(s) ended "
            f"with a non-success subtype yet passed the pre-registered gate "
            f"(`usd>0` + non-empty patch). Per arm: "
            + ", ".join(f"`{arm}`={len(pairs)}"
                        for arm, pairs in sorted(report["errored_cells"].items()))
            + f". Pairs where every arm succeeded: "
              f"**{report.get('n_clean_all_arms_success')}**."
        )
        lines.append("")
        if report.get("headline_excluding_errored"):
            lines.append(_comparison_table([report["headline_excluding_errored"]]))
            lines.append("")

    if report.get("no_edit_cells"):
        lines.append("## Sensitivity -- cells with no EDIT event")
        lines.append("")
        lines.append(
            f"{sum(len(v) for v in report['no_edit_cells'].values())} cell(s) emitted "
            f"no EDIT event, so their locate count spans the whole session rather "
            f"than the pre-edit prefix. They still pass the gate, because an edit "
            f"made through the shell produces a patch. Per arm: "
            + ", ".join(f"`{arm}`={len(pairs)}"
                        for arm, pairs in sorted(report["no_edit_cells"].items()))
            + f". Pairs where every arm emitted a real edit: "
              f"**{report.get('n_clean_all_arms_edited')}**."
        )
        lines.append("")
        if report.get("headline_excluding_no_edit"):
            lines.append(_comparison_table([report["headline_excluding_no_edit"]]))
            lines.append("")

    lines.append("## Power")
    lines.append("")
    lines.append(
        f"n = {n}. The minimum detectable effect at this n is roughly 25 points; "
        "a confidence interval that straddles zero is **not** evidence of no effect, "
        "and no sub-group of fewer than ~30 pairs inside this cell should be read at all."
    )
    lines.append("")
    if report.get("warnings"):
        lines.append("## Warnings")
        lines.append("")
        for warning in report["warnings"]:
            lines.append(f"- {warning}")
        lines.append("")

    lines.append("## Provenance")
    lines.append("")
    for key, value in sorted(report["provenance"].items()):
        lines.append(f"- `{key}`: `{value}`")
    lines.append("")
    return "\n".join(lines) + "\n"


def _comparison_table(entries: list[dict]) -> str:
    head = (
        "| comparison | n | locate geomean % | median % | 95% CI (%) | straddles 0 | "
        "cost % | tokens % | resolved t/c | McNemar p |"
    )
    sep = "|---|---:|---:|---:|---|---|---:|---:|---:|---:|"
    rows = [head, sep]
    for entry in entries:
        label = f"{entry['treatment']} vs {entry['control']}"
        if entry.get("error"):
            rows.append(f"| {label} | - | {entry['error']} | | | | | | | |")
            continue
        ci = entry.get("bootstrap_ci_pct")
        resolved = entry.get("resolved") or {}

        def _pct(key: str) -> str:
            value = entry.get(key)
            return f"{value:+.2f}" if isinstance(value, (int, float)) else "n/a"

        rows.append(
            f"| {label} "
            f"| {entry['n']} "
            f"| {_pct('geomean_pct')} "
            f"| {_pct('median_pct')} "
            f"| {f'[{ci[0]:+.1f}, {ci[1]:+.1f}]' if ci else 'n/a'} "
            f"| {'yes' if entry.get('straddles_zero') else 'no'} "
            f"| {_pct('cost_geomean_pct')} "
            f"| {_pct('tokens_geomean_pct')} "
            f"| {resolved.get('treatment', '-')}/{resolved.get('control', '-')} "
            f"| {resolved.get('p_two_sided', '-')} |"
        )
    return "\n".join(rows)


def build_report(results: pathlib.Path, config: dict, arms: list[str],
                 require_seal: bool = True) -> dict:
    grading_path = results / "grading.json"
    grading = json.loads(grading_path.read_text(encoding="utf-8")) if grading_path.is_file() else None

    problems, warnings = check_integrity(results, arms, require_seal=require_seal)
    clean, table = collect(results, arms, grading)
    seal_ok, _ = seal.verify()

    report: dict = {
        "arms": arms,
        "pairs_collected": len(table),
        "n_clean": len(clean),
        "clean_pairs": clean,
        "seal_ok": seal_ok,
        "problems": problems,
        "warnings": warnings,
        "refused": bool(problems),
        "provenance": {
            "config_sha256": config.get("_config_sha256"),
            "prompts_sha256": _harness.sha256_file(_harness.BRAINMARK_DIR / "prompts.py"),
            "mechmetrics_sha256": _harness.sha256_file(_harness.BRAINMARK_DIR / "mechmetrics.py"),
            "vendored_metrics_sha256": _harness.sha256_file(
                _harness.BRAINMARK_DIR / "vendor" / "graphmark_metrics.py"),
            "results_dir": str(results),
        },
    }
    if problems:
        return report

    # NO-EDIT EXPOSURE. `locate_calls_pre_edit` counts locate calls before the
    # first EDIT event; a session with none has no cutoff, so the metric is
    # measured over the WHOLE session. An edit made through the shell (`sed
    # -i`, a heredoc) is not an EDIT event on either backend but still produces
    # a non-empty patch, so such a cell PASSES the pair gate. The gate is
    # pre-registered and stays exactly as written -- this only makes the
    # exposure visible, and prices it.
    report["no_edit_cells"] = {
        arm: cells
        for arm in arms
        if (cells := sorted(p for p in clean
                            if (table[p][arm]["mech"] or {}).get("no_edit")))
    }
    fully_edited = [
        pair for pair in clean
        if not any((table[pair][arm]["mech"] or {}).get("no_edit") for arm in arms)
    ]
    report["n_clean_all_arms_edited"] = len(fully_edited)

    # NON-SUCCESS EXPOSURE. The pre-registered per-session gate is `usd > 0`
    # and a non-empty patch (PREREGISTRATION.md section 5) -- it does NOT
    # include subtype=="success". On the primary backend that is looser than it
    # reads: codex reports no cost, so its `total_cost_usd` is tokens x the
    # UNVERIFIED_PLACEHOLDER rate card in backends.json, and `usd > 0` reduces
    # to "burned tokens". A session killed at the wall-clock timeout
    # (returncode 124) or ending error_max_turns still writes a stream, still
    # has tokens, and collect_patch still collects its partial edits -- so it
    # passes, and its locate count is measured on a truncated session.
    #
    # That is an arm confound whenever failure rate tracks the arm: a bigger
    # packet is a slower session is a likelier timeout. The gate stays as
    # pre-registered; this makes the exposure countable.
    report["errored_cells"] = {
        arm: cells
        for arm in arms
        if (cells := sorted(
            pair for pair in clean
            if (table[pair][arm].get("subtype") or "success") != "success"))
    }
    successful = [
        pair for pair in clean
        if all((table[pair][arm].get("subtype") or "success") == "success"
               for arm in arms)
    ]
    report["n_clean_all_arms_success"] = len(successful)

    report["headline"] = compare(table, clean, HEADLINE, BASELINE)
    report["competitors"] = {
        arm: compare(table, clean, HEADLINE, arm)
        for arm in arms if arm not in (BASELINE, HEADLINE)
    }
    report["arm_vs_baseline"] = {
        arm: compare(table, clean, arm, BASELINE)
        for arm in arms if arm != BASELINE
    }

    if report["errored_cells"]:
        affected = sum(len(v) for v in report["errored_cells"].values())
        warnings.append(
            f"subtype: {affected} cell(s) across {sorted(report['errored_cells'])} "
            f"did NOT end subtype==success but still passed the pre-registered "
            f"gate (usd>0 + non-empty patch). On codex `usd` is tokens x a "
            f"placeholder rate card, so that gate does not test success. See "
            f"`headline_excluding_errored`."
        )
        if successful:
            report["headline_excluding_errored"] = compare(
                table, successful, HEADLINE, BASELINE)

    if report["no_edit_cells"]:
        affected = sum(len(v) for v in report["no_edit_cells"].values())
        warnings.append(
            f"no_edit: {affected} cell(s) across "
            f"{sorted(report['no_edit_cells'])} emitted no EDIT event, so their "
            f"locate count was measured over the WHOLE session rather than up to "
            f"a first edit. They passed the gate (non-empty patch), which the "
            f"mechmetrics docstring said they could not. See "
            f"`headline_excluding_no_edit` for the estimate without them."
        )
        if fully_edited:
            report["headline_excluding_no_edit"] = compare(
                table, fully_edited, HEADLINE, BASELINE)
            report["competitors_excluding_no_edit"] = {
                arm: compare(table, fully_edited, HEADLINE, arm)
                for arm in arms if arm not in (BASELINE, HEADLINE)
            }
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Aggregate a BrainMark run.")
    parser.add_argument("--results", required=True)
    parser.add_argument("--config", default=None)
    parser.add_argument("--arms", default=None)
    parser.add_argument("--out", default=None)
    parser.add_argument(
        "--allow-unsealed", action="store_true",
        help="demote seal failures to warnings. ONLY for pilot/dev runs -- a "
             "confirmatory number produced with this flag is not quotable.",
    )
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    arms = args.arms.split(",") if args.arms else list(config["arms"])
    results = pathlib.Path(args.results)
    report = build_report(results, config, arms, require_seal=not args.allow_unsealed)

    out_dir = pathlib.Path(args.out) if args.out else results
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "REPORT.json").write_text(_harness.pretty_json(report), encoding="utf-8")
    (out_dir / "REPORT.md").write_text(render_markdown(report), encoding="utf-8")
    print(render_markdown(report))
    return 1 if report["refused"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
