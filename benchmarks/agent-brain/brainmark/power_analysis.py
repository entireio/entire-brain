#!/usr/bin/env python3
"""Power analysis for the pre-committed n rule (plan 0.6). Pure stdlib.

WHAT IT IS FOR. The plan freezes, BEFORE any confirmatory spend, the smallest n
that gives 80% power to detect a 15% reduction in the primary metric
(`locate_calls_pre_edit`). That number cannot be guessed: it is set by the
per-pair variance of the paired log-ratio, which only the pilot can measure.
This module turns a pilot into that number, plus the MDE curve that shows what
every other n would have bought, plus a variance decomposition that says WHERE
the noise lives (pair / arm / rep) -- because if most of it is rep-level, more
reps are cheaper than more pairs, and if most of it is pair-level, reps are
nearly worthless (devenv memory: "instance identity predicts nothing",
"never replicate an instance; always add instances" -- this module is how that
advice gets re-tested against BrainMark's own data instead of assumed).

THE PRE-COMMITTED RULE (frozen; changing it needs a dated prereg amendment):

    metric      locate_calls_pre_edit, ratio taken on (count + 1)
    scale       natural log of the paired ratio, treatment vs no_brain
    effect      15% reduction  ->  delta = |ln(0.85)| = 0.1625 log units
    alpha       0.05, two-sided
    power       0.80
    n           smallest integer with  n >= ((z_{1-a/2} + z_{1-b}) * sd / delta)^2
    cap         min(n, sealed supply); if the cap binds, the report says the
                study is UNDERPOWERED rather than quietly shrinking the effect
                it claims to detect.

    sd is the standard deviation of the PER-PAIR log-ratio. With r replicates
    per cell the per-pair statistic is the mean over reps, so the variance used
    is  var_pair + var_resid / r  -- reps shrink only the residual term. This is
    the whole reason the decomposition is part of the same tool.

APPROXIMATION, STATED. The normal approximation is used for the quantiles. For
a paired t-test the usual small-sample correction is +2 subjects; `required_n`
reports both `n` and `n_t_corrected` so the difference is visible rather than
buried.

INPUT. Either a report JSON carrying per-pair observations, or a results tree:

    python3 power_analysis.py --report REPORT.json
    python3 power_analysis.py --results results/B/pilot
    python3 power_analysis.py --results results/B/pilot --reps 2 --sealed-n 30

Accepted JSON shapes (first match wins):
    [ {pair_id, arm, rep, locate_calls_pre_edit}, ... ]
    {"observations": [ ... same ... ]}
    {"table": {pair_id: {arm: {"mech": {"locate_calls_pre_edit": n}}}}}
    {"per_pair": {pair_id: {arm: n}}}
report.py's current REPORT.json aggregates away the per-pair values, so the
results tree is the reliable input; the JSON shapes exist so a future
per-pair-emitting report can be read without touching this file.
"""

from __future__ import annotations

import argparse
import json
import math
import pathlib
import statistics
from typing import Any, Iterable, Sequence

RATIO_OFFSET = 1
PRIMARY_METRIC = "locate_calls_pre_edit"

#: FROZEN pre-committed rule.
TARGET_REDUCTION = 0.15
ALPHA = 0.05
POWER = 0.80


def _z(p: float) -> float:
    return statistics.NormalDist().inv_cdf(p)


def target_delta(reduction: float = TARGET_REDUCTION) -> float:
    """A `reduction` fraction expressed on the log-ratio scale (positive)."""
    if not 0 < reduction < 1:
        raise ValueError(f"reduction must be in (0,1), got {reduction}")
    return abs(math.log(1.0 - reduction))


# --------------------------------------------------------------------------
# observations
# --------------------------------------------------------------------------


def _obs(pair_id: str, arm: str, rep: Any, value: Any) -> dict | None:
    if value is None:
        return None
    try:
        count = int(value)
    except (TypeError, ValueError):
        return None
    return {"pair_id": str(pair_id), "arm": str(arm),
            "rep": 0 if rep is None else int(rep), PRIMARY_METRIC: count}


def load_observations(payload: Any) -> list[dict]:
    """Normalize any accepted shape to [{pair_id, arm, rep, metric}]."""
    if isinstance(payload, (str, pathlib.Path)):
        payload = json.loads(pathlib.Path(payload).read_text(encoding="utf-8"))

    rows: list[dict] = []
    if isinstance(payload, list):
        candidates: Iterable[Any] = payload
    elif isinstance(payload, dict) and isinstance(payload.get("observations"), list):
        candidates = payload["observations"]
    elif isinstance(payload, dict) and isinstance(payload.get("table"), dict):
        for pair_id, arms in payload["table"].items():
            for arm, cell in (arms or {}).items():
                mech = (cell or {}).get("mech") or {}
                row = _obs(pair_id, arm, (cell or {}).get("rep"), mech.get(PRIMARY_METRIC))
                if row:
                    rows.append(row)
        return rows
    elif isinstance(payload, dict) and isinstance(payload.get("per_pair"), dict):
        for pair_id, arms in payload["per_pair"].items():
            for arm, value in (arms or {}).items():
                row = _obs(pair_id, arm, None,
                           value.get(PRIMARY_METRIC) if isinstance(value, dict) else value)
                if row:
                    rows.append(row)
        return rows
    else:
        raise ValueError(
            "unrecognized input: expected a list of observations, or a dict with "
            "`observations` / `table` / `per_pair`"
        )

    for item in candidates:
        if not isinstance(item, dict):
            continue
        value = item.get(PRIMARY_METRIC)
        if value is None:
            value = ((item.get("mech") or {}) or {}).get(PRIMARY_METRIC)
        row = _obs(item.get("pair_id"), item.get("arm"), item.get("rep"), value)
        if row:
            rows.append(row)
    return rows


def observations_from_results(results: str | pathlib.Path) -> list[dict]:
    """Walk `[rep<k>/]<pair>/<arm>/meta.json`. Reads the RECORDED metric only.

    It never recomputes from stream.jsonl: the recorded value is what a report
    would use, so a power analysis based on anything else would be answering a
    question about a different number.
    """
    root = pathlib.Path(results)
    rows: list[dict] = []
    for meta_path in sorted(root.rglob("meta.json")):
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            continue
        mech = meta.get("mechmetrics") or {}
        if PRIMARY_METRIC not in mech:
            continue
        rep = meta.get("rep")
        if rep is None:
            # Layout without --rep, or an older tree: infer from the path.
            for part in meta_path.parts:
                if part.startswith("rep") and part[3:].isdigit():
                    rep = int(part[3:])
                    break
        row = _obs(meta.get("pair_id") or meta_path.parents[1].name,
                   meta.get("arm") or meta_path.parent.name,
                   rep, mech.get(PRIMARY_METRIC))
        if row:
            rows.append(row)
    return rows


# --------------------------------------------------------------------------
# paired log-ratios
# --------------------------------------------------------------------------


def pair_log_ratios(observations: Sequence[dict], treatment: str,
                    control: str) -> dict[str, float]:
    """pair_id -> mean over reps of log((t+1)/(c+1)).

    Averaging WITHIN a pair before taking the variance ACROSS pairs is what
    makes the pair the unit of analysis; pooling every (pair, rep) row instead
    would treat replicates as independent subjects and understate the sd.
    """
    by_pair: dict[str, dict[int, dict[str, int]]] = {}
    for row in observations:
        by_pair.setdefault(row["pair_id"], {}).setdefault(row["rep"], {})[row["arm"]] = (
            row[PRIMARY_METRIC]
        )
    out: dict[str, float] = {}
    for pair_id, reps in by_pair.items():
        logs = [
            math.log((cells[treatment] + RATIO_OFFSET) / (cells[control] + RATIO_OFFSET))
            for cells in reps.values()
            if treatment in cells and control in cells
        ]
        if logs:
            out[pair_id] = statistics.fmean(logs)
    return out


# --------------------------------------------------------------------------
# the rule
# --------------------------------------------------------------------------


def required_n(sd: float, reduction: float = TARGET_REDUCTION,
               alpha: float = ALPHA, power: float = POWER,
               sealed_n: int | None = None) -> dict:
    """Smallest n for `power` at a `reduction` effect. THE PRE-COMMITTED RULE."""
    if sd <= 0:
        raise ValueError("sd must be positive; a zero-variance pilot cannot size a study")
    delta = target_delta(reduction)
    z_sum = _z(1 - alpha / 2) + _z(power)
    exact = (z_sum * sd / delta) ** 2
    n = max(2, math.ceil(exact - 1e-12))
    out = {
        "rule": "n >= ((z_{1-alpha/2} + z_{power}) * sd / |ln(1-reduction)|)^2",
        "sd_log_ratio": round(sd, 6),
        "reduction": reduction,
        "delta_log": round(delta, 6),
        "alpha": alpha,
        "power": power,
        "z_sum": round(z_sum, 6),
        "n_exact": round(exact, 4),
        "n": n,
        # Standard small-sample correction for a paired t-test.
        "n_t_corrected": n + 2,
        "approximation": "normal quantiles; +2 is the usual paired-t correction",
    }
    if sealed_n is not None:
        out["sealed_supply"] = int(sealed_n)
        out["n_capped"] = min(n, int(sealed_n))
        out["underpowered"] = n > int(sealed_n)
        out["power_at_sealed_n"] = round(
            achieved_power(sd, int(sealed_n), reduction, alpha), 4
        )
    return out


def achieved_power(sd: float, n: int, reduction: float = TARGET_REDUCTION,
                   alpha: float = ALPHA) -> float:
    """Power of a two-sided paired test at this n. Normal approximation."""
    if sd <= 0 or n < 2:
        return float("nan")
    delta = target_delta(reduction)
    z_alpha = _z(1 - alpha / 2)
    ncp = delta * math.sqrt(n) / sd
    normal = statistics.NormalDist()
    return normal.cdf(ncp - z_alpha) + normal.cdf(-ncp - z_alpha)


def mde_curve(sd: float, n_grid: Sequence[int] | None = None,
              alpha: float = ALPHA, power: float = POWER) -> list[dict]:
    """For each n: the smallest detectable reduction, in log units and percent."""
    grid = list(n_grid) if n_grid else [10, 20, 30, 40, 50, 60, 80, 100, 150, 200, 300]
    z_sum = _z(1 - alpha / 2) + _z(power)
    curve: list[dict] = []
    for n in grid:
        if n < 2:
            continue
        delta = z_sum * sd / math.sqrt(n)
        curve.append({
            "n": int(n),
            "mde_log": round(delta, 6),
            # A `delta` decrease on the log scale is a (1 - e^-delta) reduction.
            "mde_reduction_pct": round((1 - math.exp(-delta)) * 100, 3),
            "meets_target": delta <= target_delta(),
        })
    return curve


# --------------------------------------------------------------------------
# variance decomposition
# --------------------------------------------------------------------------


def variance_components(observations: Sequence[dict]) -> dict:
    """Random-effects components of log(metric+1) over pair x arm x rep.

    Balanced-design expected-mean-squares estimator:

        sigma2_resid = MS_resid
        sigma2_pair  = (MS_pair - MS_resid) / (A*R)
        sigma2_arm   = (MS_arm  - MS_resid) / (P*R)
        sigma2_rep   = (MS_rep  - MS_resid) / (P*A)

    Negative estimates are clamped to 0 and FLAGGED (a negative component means
    the between-group spread is smaller than chance, i.e. that component is
    indistinguishable from zero -- reporting a negative variance would be
    nonsense, and silently clamping without saying so would hide it).

    UNBALANCED designs are refused rather than approximated: the estimator above
    is only unbiased when every (pair, arm, rep) cell exists exactly once, and a
    resumed run with a few missing cells is exactly the case where a quiet
    approximation would mislead.
    """
    pairs = sorted({row["pair_id"] for row in observations})
    arms = sorted({row["arm"] for row in observations})
    reps = sorted({row["rep"] for row in observations})
    P, A, R = len(pairs), len(arms), len(reps)
    if min(P, A, R) < 1:
        raise ValueError("no observations")

    cells: dict[tuple[str, str, int], float] = {}
    for row in observations:
        key = (row["pair_id"], row["arm"], row["rep"])
        if key in cells:
            raise ValueError(f"duplicate observation for {key}")
        cells[key] = math.log(row[PRIMARY_METRIC] + RATIO_OFFSET)
    expected = P * A * R
    if len(cells) != expected:
        missing = expected - len(cells)
        raise ValueError(
            f"unbalanced design: {len(cells)} observations for {P} pairs x {A} arms "
            f"x {R} reps ({missing} missing). Refusing to estimate variance "
            "components on an unbalanced grid -- drop incomplete pairs first."
        )

    values = list(cells.values())
    grand = statistics.fmean(values)

    def group_mean(key_index: int, key_value: Any) -> float:
        return statistics.fmean(
            [v for k, v in cells.items() if k[key_index] == key_value]
        )

    ss_pair = A * R * sum((group_mean(0, p) - grand) ** 2 for p in pairs)
    ss_arm = P * R * sum((group_mean(1, a) - grand) ** 2 for a in arms)
    ss_rep = P * A * sum((group_mean(2, r) - grand) ** 2 for r in reps)
    ss_total = sum((v - grand) ** 2 for v in values)
    ss_resid = ss_total - ss_pair - ss_arm - ss_rep

    df_pair, df_arm, df_rep = P - 1, A - 1, R - 1
    df_resid = (P * A * R - 1) - df_pair - df_arm - df_rep
    if df_resid <= 0:
        raise ValueError(
            f"no residual degrees of freedom ({P} pairs x {A} arms x {R} reps): "
            "add pairs or reps before decomposing variance"
        )

    ms_resid = ss_resid / df_resid
    ms_pair = ss_pair / df_pair if df_pair else 0.0
    ms_arm = ss_arm / df_arm if df_arm else 0.0
    ms_rep = ss_rep / df_rep if df_rep else 0.0

    raw = {
        "pair": (ms_pair - ms_resid) / (A * R) if df_pair else 0.0,
        "arm": (ms_arm - ms_resid) / (P * R) if df_arm else 0.0,
        "rep": (ms_rep - ms_resid) / (P * A) if df_rep else 0.0,
        "residual": ms_resid,
    }
    clamped = sorted(k for k, v in raw.items() if v < 0)
    components = {k: max(v, 0.0) for k, v in raw.items()}
    total = sum(components.values())
    return {
        "n_pairs": P, "n_arms": A, "n_reps": R,
        "grand_mean_log": round(grand, 6),
        "mean_squares": {"pair": round(ms_pair, 8), "arm": round(ms_arm, 8),
                         "rep": round(ms_rep, 8), "residual": round(ms_resid, 8)},
        "components": {k: round(v, 8) for k, v in components.items()},
        "components_raw": {k: round(v, 8) for k, v in raw.items()},
        "share": {k: (round(v / total, 4) if total > 0 else None)
                  for k, v in components.items()},
        "clamped_to_zero": clamped,
        "note": ("a negative raw component is not distinguishable from zero; it is "
                 "clamped and listed in clamped_to_zero"),
    }


def sd_with_reps(components: dict, reps: int) -> float:
    """Per-pair log-ratio sd implied by the components at `reps` replicates.

    A paired log-ratio differences two arms, so both arms' residuals enter:
    var = 2 * (var_arm_interaction + var_resid/r). With only main effects
    estimated here, `arm` is a constant shift that cancels in the pairing and
    the usable term is the residual, doubled for the difference.
    """
    if reps < 1:
        raise ValueError("reps must be >= 1")
    resid = float(components["components"]["residual"])
    return math.sqrt(2.0 * resid / reps)


# --------------------------------------------------------------------------
# report
# --------------------------------------------------------------------------


def analyze(observations: Sequence[dict], treatment: str = "full_brain",
            control: str = "no_brain", reps: int = 1,
            sealed_n: int | None = None,
            reduction: float = TARGET_REDUCTION) -> dict:
    ratios = pair_log_ratios(observations, treatment, control)
    out: dict[str, Any] = {
        "primary_metric": PRIMARY_METRIC,
        "ratio_offset": RATIO_OFFSET,
        "treatment": treatment,
        "control": control,
        "n_pairs_with_both_arms": len(ratios),
        "pre_committed_rule": {
            "reduction": reduction, "alpha": ALPHA, "power": POWER,
            "frozen_before": "any confirmatory spend (plan 0.6)",
        },
    }
    if len(ratios) < 2:
        out["error"] = (
            f"need >=2 pairs with both {treatment} and {control}; got {len(ratios)}"
        )
        return out

    values = list(ratios.values())
    sd = statistics.stdev(values)
    out["observed"] = {
        "mean_log_ratio": round(statistics.fmean(values), 6),
        "median_log_ratio": round(statistics.median(values), 6),
        "sd_log_ratio": round(sd, 6),
        "observed_reduction_pct": round((1 - math.exp(statistics.fmean(values))) * 100, 3),
    }
    if sd <= 0:
        out["error"] = "zero variance across pairs; a study cannot be sized from it"
        return out

    out["required_n"] = required_n(sd, reduction, sealed_n=sealed_n)
    out["mde_curve"] = mde_curve(sd)

    try:
        out["variance_decomposition"] = variance_components(observations)
        out["sd_by_reps"] = {
            str(r): round(sd_with_reps(out["variance_decomposition"], r), 6)
            for r in (1, 2, 3, 4)
        }
        out["required_n_by_reps"] = {
            str(r): required_n(sd_with_reps(out["variance_decomposition"], r),
                               reduction, sealed_n=sealed_n)["n"]
            for r in (1, 2, 3, 4)
            if sd_with_reps(out["variance_decomposition"], r) > 0
        }
    except ValueError as exc:
        out["variance_decomposition"] = {"error": str(exc)}
    if reps != 1:
        out["requested_reps"] = reps
    return out


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Pilot -> pre-committed n for BrainMark (plan 0.6).")
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--report", help="pilot report / observations JSON")
    source.add_argument("--results", help="results root to walk for meta.json")
    parser.add_argument("--treatment", default="full_brain")
    parser.add_argument("--control", default="no_brain")
    parser.add_argument("--reps", type=int, default=1)
    parser.add_argument("--sealed-n", type=int, default=None,
                        help="sealed supply; the n rule is capped by it")
    parser.add_argument("--reduction", type=float, default=TARGET_REDUCTION)
    parser.add_argument("--out", default=None)
    args = parser.parse_args(argv)

    observations = (
        observations_from_results(args.results) if args.results
        else load_observations(args.report)
    )
    report = analyze(observations, args.treatment, args.control,
                     reps=args.reps, sealed_n=args.sealed_n,
                     reduction=args.reduction)
    text = json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    if args.out:
        pathlib.Path(args.out).write_text(text, encoding="utf-8")
    print(text, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
