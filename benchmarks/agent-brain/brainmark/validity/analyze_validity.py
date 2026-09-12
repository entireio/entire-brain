#!/usr/bin/env python3
"""Construct validity analysis (plan 0.8): does the primary metric measure
what it claims to?

Joins the machine metric (`locate_calls_pre_edit`, recomputed from each
sampled session's `stream.jsonl` via mechmetrics -- never trusted from a
cached number, for the same reason report.py never trusts a cached sha) with
human labels (`render_session.py` sheets, transcribed into `labels.json`
files, one per rater -- see LABEL_PROTOCOL.md) and reports:

  1. Spearman's rho between the machine count and the labeled re-derivation
     count (yes-labeled calls per session), across every session at least one
     rater fully labeled.
  2. Per-rater agreement (percent agreement + Cohen's kappa, reusing
     `seal.cohens_kappa` -- it is already generic over the category set, so
     the 3-category yes/no/unclear label reuses the exact same statistic the
     dual-review seal uses for its 2-category accept/reject verdicts) on
     whichever calls both raters labeled.

PREREGISTRATION.md's validity gate (dated amendment, 2026-08-19): if rho < 0.5,
a dated amendment of the primary metric is required BEFORE any confirmatory
run. This module reports the number; it does not decide the amendment.

Stdlib only -- no numpy/scipy. `spearman_rho` is a from-scratch Pearson
correlation of average ranks (the standard construction; ties get the mean of
their tied rank positions).
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
    from brainmark.mechmetrics import session_metrics  # type: ignore[no-redef]
    from brainmark.seal import cohens_kappa  # type: ignore[no-redef]
else:
    from .. import _harness
    from ..mechmetrics import session_metrics
    from ..seal import cohens_kappa

VERDICTS = frozenset({"yes", "no", "unclear"})


# --------------------------------------------------------------------------
# Spearman's rho -- stdlib only
# --------------------------------------------------------------------------


def _average_ranks(values: list[float]) -> list[float]:
    """1-indexed ranks; tied values share the mean of their tied positions."""
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        avg_rank = (i + j) / 2 + 1
        for k in range(i, j + 1):
            ranks[order[k]] = avg_rank
        i = j + 1
    return ranks


def spearman_rho(xs: list[float], ys: list[float]) -> dict:
    """Spearman's rank correlation (Pearson correlation of average ranks).

    Returns {"rho": float, "n": int}, or {"rho": None, "n": n, "note": ...} if
    one series has zero variance (rho is undefined, not zero -- a caller that
    substitutes 0 would silently misreport "no correlation" for "undefined").
    """
    if len(xs) != len(ys):
        raise ValueError("xs and ys must be the same length")
    n = len(xs)
    if n < 2:
        raise ValueError("need at least 2 paired observations for a correlation")
    rx = _average_ranks(xs)
    ry = _average_ranks(ys)
    mean_rx = sum(rx) / n
    mean_ry = sum(ry) / n
    cov = sum((a - mean_rx) * (b - mean_ry) for a, b in zip(rx, ry))
    var_x = sum((a - mean_rx) ** 2 for a in rx)
    var_y = sum((b - mean_ry) ** 2 for b in ry)
    if var_x == 0 or var_y == 0:
        return {"rho": None, "n": n, "note": "zero variance in one series; rho undefined"}
    return {"rho": cov / (var_x * var_y) ** 0.5, "n": n}


# --------------------------------------------------------------------------
# labels.json loading + validation
# --------------------------------------------------------------------------


def validate_labels(payload: dict, source: str = "<labels>") -> None:
    """Raises ValueError with a precise complaint on the first shape problem.

    Malformed input is refused, never silently skipped -- a rater's mistyped
    session id or verdict should surface as an error, not a quietly-dropped
    data point that shifts the sample without anyone noticing.
    """
    if not isinstance(payload, dict):
        raise ValueError(f"{source}: top level must be an object")
    if not str(payload.get("rater") or "").strip():
        raise ValueError(f"{source}: missing non-empty 'rater'")
    sessions = payload.get("sessions")
    if not isinstance(sessions, dict) or not sessions:
        raise ValueError(f"{source}: 'sessions' must be a non-empty object")
    for session_id, entry in sessions.items():
        if not isinstance(entry, dict) or not isinstance(entry.get("calls"), list):
            raise ValueError(f"{source}/{session_id}: 'calls' must be a list")
        for call in entry["calls"]:
            if not isinstance(call, dict):
                raise ValueError(f"{source}/{session_id}: each call must be an object")
            if not isinstance(call.get("call_index"), int):
                raise ValueError(f"{source}/{session_id}: call missing integer 'call_index'")
            if call.get("verdict") not in VERDICTS:
                raise ValueError(
                    f"{source}/{session_id}/call {call.get('call_index')}: "
                    f"verdict must be one of {sorted(VERDICTS)}, got {call.get('verdict')!r}"
                )


def load_labels(path: pathlib.Path) -> dict:
    payload = json.loads(path.read_text(encoding="utf-8"))
    validate_labels(payload, source=str(path))
    return payload


def session_yes_count(entry: dict) -> int:
    """Raw count of yes-labeled calls. `unclear` and `no` both contribute 0 --
    this is the count correlated against the machine metric (plan 0.8)."""
    return sum(1 for call in entry["calls"] if call["verdict"] == "yes")


def session_yes_ratio(entry: dict) -> float | None:
    """yes / (yes + no); `unclear` calls are excluded from BOTH the numerator
    and the denominator (LABEL_PROTOCOL.md), not folded into `no`. A secondary,
    session-normalized look alongside the primary raw-count correlation --
    useful because the raw count is mechanically larger in longer sessions."""
    yes = sum(1 for call in entry["calls"] if call["verdict"] == "yes")
    no = sum(1 for call in entry["calls"] if call["verdict"] == "no")
    if yes + no == 0:
        return None
    return yes / (yes + no)


# --------------------------------------------------------------------------
# joining machine metric + labels + arm (post-hoc unblind)
# --------------------------------------------------------------------------


def machine_locate_count(results_root: pathlib.Path, pair_id: str, arm: str) -> int | None:
    stream = results_root / pair_id / arm / "stream.jsonl"
    if not stream.is_file():
        return None
    return session_metrics(stream)["locate_calls_pre_edit"]


def build_joined_table(labels: dict, unblind_map: dict, results_root: pathlib.Path) -> list[dict]:
    """One row per session BOTH the labels file and the unblind map know about."""
    rows: list[dict] = []
    sessions_meta = unblind_map.get("sessions", {})
    for session_id, entry in sorted(labels["sessions"].items()):
        meta = sessions_meta.get(session_id)
        if meta is None:
            continue
        machine = machine_locate_count(results_root, meta["pair_id"], meta["arm"])
        if machine is None:
            continue
        rows.append({
            "session_id": session_id,
            "pair_id": meta["pair_id"],
            "arm": meta["arm"],
            "machine_locate_calls_pre_edit": machine,
            "labeled_yes_count": session_yes_count(entry),
            "labeled_yes_ratio": session_yes_ratio(entry),
        })
    return rows


# --------------------------------------------------------------------------
# per-rater agreement
# --------------------------------------------------------------------------


def per_rater_agreement(labels_a: dict, labels_b: dict) -> dict:
    """Percent agreement + Cohen's kappa over calls BOTH raters labeled."""
    pairs: list[tuple[str, str]] = []
    shared_sessions = set(labels_a["sessions"]) & set(labels_b["sessions"])
    for session_id in sorted(shared_sessions):
        calls_a = {c["call_index"]: c["verdict"] for c in labels_a["sessions"][session_id]["calls"]}
        calls_b = {c["call_index"]: c["verdict"] for c in labels_b["sessions"][session_id]["calls"]}
        for call_index in sorted(set(calls_a) & set(calls_b)):
            pairs.append((calls_a[call_index], calls_b[call_index]))
    if not pairs:
        return {"n": 0, "percent_agreement": None, "kappa": None,
               "note": "no calls labeled by both raters"}
    percent_agreement = sum(1 for a, b in pairs if a == b) / len(pairs)
    kappa_result = cohens_kappa(pairs)
    return {
        "n": len(pairs),
        "percent_agreement": percent_agreement,
        "kappa": kappa_result["kappa"],
        "observed_agreement": kappa_result["observed_agreement"],
        "expected_agreement": kappa_result["expected_agreement"],
    }


# --------------------------------------------------------------------------
# top-level report
# --------------------------------------------------------------------------


VALIDITY_GATE_RHO = 0.5


def build_report(labels_by_rater: dict[str, dict], unblind_map: dict,
                 results_root: pathlib.Path) -> dict:
    """labels_by_rater: {rater_id: labels_payload}. Primary rho uses the FIRST
    rater (sorted by id) as primary and every other rater as a robustness
    check -- both are reported, never only the one that clears the gate."""
    rater_ids = sorted(labels_by_rater)
    per_rater_rows = {
        rid: build_joined_table(labels_by_rater[rid], unblind_map, results_root)
        for rid in rater_ids
    }

    correlations: dict[str, dict] = {}
    for rid, rows in per_rater_rows.items():
        if len(rows) < 2:
            correlations[rid] = {"rho": None, "n": len(rows), "note": "fewer than 2 joined sessions"}
            continue
        xs = [r["machine_locate_calls_pre_edit"] for r in rows]
        ys = [r["labeled_yes_count"] for r in rows]
        correlations[rid] = spearman_rho(xs, ys)

    agreement = None
    if len(rater_ids) >= 2:
        agreement = per_rater_agreement(labels_by_rater[rater_ids[0]], labels_by_rater[rater_ids[1]])

    primary_rho = correlations.get(rater_ids[0], {}).get("rho") if rater_ids else None
    gate_fires = primary_rho is None or primary_rho < VALIDITY_GATE_RHO

    return {
        "raters": rater_ids,
        "n_sessions_by_rater": {rid: len(rows) for rid, rows in per_rater_rows.items()},
        "correlations_by_rater": correlations,
        "primary_rater": rater_ids[0] if rater_ids else None,
        "primary_rho": primary_rho,
        "validity_gate_threshold": VALIDITY_GATE_RHO,
        "validity_gate_fires": gate_fires,
        "per_rater_agreement": agreement,
        "rows_by_rater": per_rater_rows,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Analyze BrainMark validity labels (plan 0.8).")
    parser.add_argument("--labels", nargs="+", required=True, help="one labels.json per rater")
    parser.add_argument("--unblind-map", required=True, help="UNBLIND-MAP.json from render_session.py batch")
    parser.add_argument("--results", required=True, help="B results root (contains <pair_id>/<arm>/)")
    parser.add_argument("--out", default=None)
    args = parser.parse_args(argv)

    labels_by_rater: dict[str, dict] = {}
    for path_str in args.labels:
        payload = load_labels(pathlib.Path(path_str))
        labels_by_rater[payload["rater"]] = payload

    unblind_map = json.loads(pathlib.Path(args.unblind_map).read_text(encoding="utf-8"))
    report = build_report(labels_by_rater, unblind_map, pathlib.Path(args.results))

    rendered = _harness.pretty_json(report)
    if args.out:
        pathlib.Path(args.out).write_text(rendered, encoding="utf-8")
    print(rendered, end="")

    if report["validity_gate_fires"]:
        print(
            f"\nVALIDITY GATE FIRES: primary rho={report['primary_rho']} (undefined or below "
            f"{VALIDITY_GATE_RHO}) -- PREREGISTRATION.md requires a dated amendment of the "
            "primary metric BEFORE any confirmatory run.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
