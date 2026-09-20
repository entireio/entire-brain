# =============================================================================
# VENDORED COPY with a local undefined-value handling repair.
#
#   Source repo   : <local graphmark checkout>  (graphmark)
#   Source path   : agentic-swebench/tools/metrics.py
#   Source commit : 641c009cbe10ccac72059de0c92e339943274b67
#   Commit date   : 2026-07-27T09:11:02-04:00
#   SHA256 of the ORIGINAL upstream source bytes (before local repairs):
#                   d84be013f6c87d27e9a795402d0842e4b5add2246a90ea40eca9177cf80cb563
#   Vendored for  : BrainMark, benchmarks/agent-brain/brainmark/vendor/
#
# Why vendored: BrainMark's paired statistics (log-ratios, geometric mean,
# median, 20k-iteration bootstrap CI, McNemar, session gates) must be the SAME
# estimator graphmark already uses, and must not silently change when the
# graphmark checkout moves. Pinning the bytes makes the estimator part of the
# sealed artifact -- report.py records this file's sha256 in every REPORT.
#
# NOTE (memory: "estimator flips the sign"): pooled vs equal-weighted choice is
# an ANALYSIS decision, pre-registered in PREREGISTRATION.md, not a free knob.
#
# Local repair: loo_range excludes undefined deletions from numeric bounds,
# reports their count, and refuses to claim sign stability when any are undefined.
# The upstream identity above is retained; existing seals must be revalidated.
# =============================================================================

#!/usr/bin/env python3
"""metrics.py - one reusable, honest token/cost analysis for agentic-swebench runs.

Replaces ad-hoc per-run arithmetic. Python 3 stdlib only, read-only over results/,
no docker, no network.

WHAT IT COMPUTES (and why)
--------------------------
Gate (identical to the eval's submission gate): an instance counts for an arm only if
  total_cost_usd > 0 AND patch.diff is non-empty. A gated-out instance is UNRESOLVED for
  that arm and REMAINS IN THE DENOMINATOR for every arm. Arms are never intersected
  after gating.

Degenerate sessions (empty patch, is_error, or subtype in {error_max_turns,
  aborted_streaming, error_during_execution}) are EXCLUDED FROM TOKEN STATS but KEPT in
  resolve denominators. Rationale: a session that aborts or burns its turn cap without
  finishing the task is not a measurement of "cost to do the task"; a previously reported
  -52% headline was attributable to one aborted session that "saved" 2.5M tokens by not
  doing the work. NOTE HONESTLY: this exclusion is itself an analysis choice and it is not
  neutral - arms differ in how many sessions hit the turn cap - so the tool always prints
  an ALL-GATED sensitivity row alongside the CLEAN (degenerate-excluded) primary.

Cost units, always side by side:
  usd          = total_cost_usd                                  <- the money claim
  billed_new   = input + cache_creation + output                 <- the work signal
  total_tokens = billed_new + cache_read                         <- outlier-prone
  total_tokens overweights cache_read: cache reads bill at a fraction of input price
  (~0.1x), so a run that re-reads a big cached prefix looks catastrophic in total_tokens
  while barely moving usd. usd is the claim; billed_new is the work; total_tokens is
  reported only for continuity with older numbers.

Paired log-ratios: for each instance present (and clean) in BOTH arms,
  r = ln(treat/ref). Geometric mean = exp(mean(r)) - 1, expressed as % change, so a 2x
  increase and a 2x decrease cancel to 0%. Median, 10%-trimmed mean, and the raw pooled
  ratio (sum/sum) are also shown; the pooled one is outlier-prone and is labelled so.

Bootstrap 95% CI: 20,000 resamples over instances, on the geometric mean and on
  tokens-per-resolved / usd-per-resolved.

Leave-one-instance-out (LOO): min/max of every headline when any single instance is
  dropped, plus whether the sign survives. A previous eg-vs-cmm result flipped sign when
  one instance was removed.

Micro vs macro: micro = geometric mean over instances (dominated by the few big repos).
  macro = per-repo geometric mean, then unweighted mean across repos. Both printed, plus
  the divergence.

Also: per-repo table, per-language table (language derived from gold-patch file
  extensions), tokens/resolved and usd/resolved with numerator AND denominator shown
  explicitly, and McNemar exact two-sided p for the resolved-count comparison.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import random
import shutil
import statistics
import sys
import tempfile
from dataclasses import dataclass, field

DEGENERATE_SUBTYPES = {"error_max_turns", "aborted_streaming", "error_during_execution"}
UNITS = ("usd", "billed_new", "total_tokens")
UNIT_NOTE = {
    "usd": "money claim",
    "billed_new": "work signal",
    "total_tokens": "outlier-prone (overweights cache_read)",
}

EXT_LANG = {
    ".py": "Python", ".pyi": "Python",
    ".go": "Go",
    ".java": "Java",
    ".js": "JavaScript", ".jsx": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
    ".ts": "TypeScript", ".tsx": "TypeScript",
    ".rs": "Rust",
    ".rb": "Ruby",
    ".php": "PHP",
    ".c": "C", ".h": "C",
    ".cc": "C++", ".cpp": "C++", ".cxx": "C++", ".hpp": "C++", ".hh": "C++", ".hxx": "C++",
    ".cs": "C#",
    ".swift": "Swift", ".kt": "Kotlin", ".kts": "Kotlin", ".scala": "Scala",
    ".m": "ObjC", ".mm": "ObjC",
    ".lua": "Lua", ".sh": "Shell", ".bash": "Shell", ".pl": "Perl", ".pm": "Perl",
    ".dart": "Dart", ".ex": "Elixir", ".exs": "Elixir", ".erl": "Erlang",
    ".hs": "Haskell", ".clj": "Clojure", ".zig": "Zig", ".jl": "Julia", ".r": "R",
}


# --------------------------------------------------------------------------- session IO

@dataclass
class Session:
    iid: str
    present: bool = False
    usd: float = 0.0
    input_tokens: int = 0
    cache_creation: int = 0
    cache_read: int = 0
    output_tokens: int = 0
    num_turns: int = 0
    subtype: str = "MISSING"
    is_error: bool = False
    patch_nonempty: bool = False
    reasons: list = field(default_factory=list)

    @property
    def billed_new(self) -> int:
        return self.input_tokens + self.cache_creation + self.output_tokens

    @property
    def total_tokens(self) -> int:
        return self.billed_new + self.cache_read

    def unit(self, name: str) -> float:
        return {"usd": self.usd, "billed_new": float(self.billed_new),
                "total_tokens": float(self.total_tokens)}[name]

    @property
    def gate(self) -> bool:
        """Identical to the eval's submission gate."""
        return self.usd > 0.0 and self.patch_nonempty

    @property
    def degenerate(self) -> bool:
        return bool(self.reasons)


def load_session(root: str, tag_arm: str, iid: str) -> Session:
    d = os.path.join(root, tag_arm, iid)
    s = Session(iid=iid)
    pj = os.path.join(d, "cc_out.json")
    cc = None
    if os.path.isfile(pj) and os.path.getsize(pj) > 0:
        try:
            cc = json.load(open(pj))
        except Exception:
            cc = None
    if cc:
        s.present = True
        s.usd = float(cc.get("total_cost_usd") or 0.0)
        u = cc.get("usage") or {}
        s.input_tokens = int(u.get("input_tokens") or 0)
        s.cache_creation = int(u.get("cache_creation_input_tokens") or 0)
        s.cache_read = int(u.get("cache_read_input_tokens") or 0)
        s.output_tokens = int(u.get("output_tokens") or 0)
        s.num_turns = int(cc.get("num_turns") or 0)
        s.subtype = cc.get("subtype") or cc.get("terminal_reason") or "MISSING"
        s.is_error = bool(cc.get("is_error"))
    pp = os.path.join(d, "patch.diff")
    if os.path.isfile(pp):
        try:
            s.patch_nonempty = bool(open(pp, errors="replace").read().strip())
        except Exception:
            s.patch_nonempty = False
    # degeneracy reasons
    if not s.present:
        s.reasons.append("no_session")
    if not s.patch_nonempty:
        s.reasons.append("empty_patch")
    if s.is_error:
        s.reasons.append("is_error")
    if s.subtype in DEGENERATE_SUBTYPES:
        s.reasons.append(f"subtype:{s.subtype}")
    if s.present and s.usd <= 0:
        s.reasons.append("zero_cost")
    return s


# --------------------------------------------------------------------------- report IO

def load_resolved(spec: str) -> tuple[set, str]:
    """Load a resolved-id set from a report spec.

    Accepted forms:
      /path/report.json                 official swebench report (resolved_ids)
      /path/reports.json#armkey         multi-arm dict: {arm: {iid: {resolved: bool}}}
      dir:/path/logs/run_evaluation/<run_id>/<model>   per-instance report.json dirs
    """
    if spec.startswith("dir:"):
        base = spec[4:]
        ids = set()
        n = 0
        for iid in sorted(os.listdir(base)):
            p = os.path.join(base, iid, "report.json")
            if not os.path.isfile(p):
                continue
            n += 1
            try:
                r = json.load(open(p))[iid]
            except Exception:
                continue
            if r.get("resolved"):
                ids.add(iid)
        return ids, f"dir-scan {base} ({n} graded)"
    armkey = None
    path = spec
    if "#" in spec:
        path, armkey = spec.split("#", 1)
    d = json.load(open(path))
    if armkey is not None:
        d = d[armkey]
        ids = {i for i, v in d.items() if isinstance(v, dict) and v.get("resolved")}
        return ids, f"{os.path.basename(path)}#{armkey} ({len(d)} graded)"
    if "resolved_ids" in d:
        return set(d["resolved_ids"]), (
            f"{os.path.basename(path)} (submitted={d.get('submitted_instances')},"
            f" resolved={d.get('resolved_instances')})")
    # plain {iid: {resolved: ...}}
    if d and all(isinstance(v, dict) for v in d.values()):
        ids = {i for i, v in d.items() if v.get("resolved")}
        return ids, f"{os.path.basename(path)} ({len(d)} graded)"
    raise SystemExit(f"unrecognised report format: {spec}")


# --------------------------------------------------------------------------- task meta

def load_task_meta(tasks_path: str) -> dict:
    """iid -> {repo_org, language} ; language derived from gold patch file extensions."""
    d = json.load(open(tasks_path))
    ins = d["instances"] if isinstance(d, dict) and "instances" in d else d
    out = {}
    for rec in ins:
        iid = rec["instance_id"]
        exts = {}
        for line in (rec.get("patch") or "").splitlines():
            if not line.startswith("diff --git "):
                continue
            for tok in line.split()[2:]:
                p = tok[2:] if tok[:2] in ("a/", "b/") else tok
                e = os.path.splitext(p)[1].lower()
                if e in EXT_LANG:
                    exts[EXT_LANG[e]] = exts.get(EXT_LANG[e], 0) + 1
        lang = max(sorted(exts), key=lambda k: (exts[k], )) if exts else "other"
        if exts:
            best = max(exts.values())
            lang = sorted(k for k, v in exts.items() if v == best)[0]
        out[iid] = {"repo_org": iid.split("__", 1)[0], "language": lang,
                    "declared_language": rec.get("language")}
    return out


# --------------------------------------------------------------------------- statistics

def geo(logs: list) -> float | None:
    """exp(mean(log-ratios)) - 1, as a fraction."""
    if not logs:
        return None
    return math.exp(sum(logs) / len(logs)) - 1.0


def trimmed(logs: list, frac: float = 0.10) -> float | None:
    if not logs:
        return None
    s = sorted(logs)
    k = int(len(s) * frac)
    s = s[k:len(s) - k] if len(s) - 2 * k > 0 else s
    return math.exp(sum(s) / len(s)) - 1.0


def median_ratio(logs: list) -> float | None:
    if not logs:
        return None
    return math.exp(statistics.median(logs)) - 1.0


def bootstrap_ci(fn, n: int, draws: int, seed: int) -> dict:
    """Percentile bootstrap over indices 0..n-1 of a statistic fn(list_of_indices)."""
    point = fn(list(range(n)))
    if n == 0:
        return {"point": None, "lo": None, "hi": None, "valid": 0}
    rng = random.Random(seed)
    vals = []
    for _ in range(draws):
        s = [rng.randrange(n) for _ in range(n)]
        v = fn(s)
        if v is not None and math.isfinite(v):
            vals.append(v)
    if not vals:
        return {"point": point, "lo": None, "hi": None, "valid": 0}
    vals.sort()
    lo = vals[int(0.025 * len(vals))]
    hi = vals[min(len(vals) - 1, int(0.975 * len(vals)))]
    return {"point": point, "lo": lo, "hi": hi, "valid": len(vals)}


def loo_range(values: list, agg) -> dict:
    """Range over defined deletions; undefined deletions cannot prove stability."""
    n = len(values)
    full = agg(values)
    if n < 2:
        return {"full": full, "min": full, "max": full, "sign_holds": full is not None, "n": n,
                "worst_id": None, "valid": 0, "undefined": 0}
    outs = [(agg(values[:i] + values[i + 1:]), i) for i in range(n)]
    valid = [(value, i) for value, i in outs
             if value is not None and math.isfinite(value)]
    undefined = n - len(valid)
    if not valid:
        return {"full": full, "min": None, "max": None, "sign_holds": False, "n": n,
                "worst_idx": None, "valid": 0, "undefined": undefined}
    lo, ilo = min(valid)
    hi, ihi = max(valid)
    sign_holds = full is not None and not undefined and all(
        (v > 0) == (full > 0) or v == full == 0 for v, _ in valid)
    worst = ilo if abs(lo - (full or 0)) > abs(hi - (full or 0)) else ihi
    return {"full": full, "min": lo, "max": hi, "sign_holds": bool(sign_holds), "n": n,
            "worst_idx": worst, "valid": len(valid), "undefined": undefined}


def mcnemar_exact(res_a: dict, res_b: dict, ids: list) -> dict:
    a_only = sum(1 for i in ids if res_a[i] and not res_b[i])
    b_only = sum(1 for i in ids if res_b[i] and not res_a[i])
    n = a_only + b_only
    if n == 0:
        return {"a_only": 0, "b_only": 0, "n_discordant": 0, "p_two_sided": 1.0}
    k = min(a_only, b_only)
    p = min(1.0, 2.0 * sum(math.comb(n, j) for j in range(k + 1)) / (2 ** n))
    return {"a_only": a_only, "b_only": b_only, "n_discordant": n, "p_two_sided": p}


def pct(x, nd=1):
    return "n/a" if x is None else f"{x * 100:+.{nd}f}%"


def money(x):
    return "n/a" if x is None else f"${x:,.2f}"


def num(x):
    if x is None:
        return "n/a"
    return f"{x:,.0f}"


# --------------------------------------------------------------------------- comparison

class Comparison:
    def __init__(self, label, results_root, ref_tag, treat_tag, iids, meta,
                 ref_resolved=None, treat_resolved=None, ref_src="", treat_src="",
                 draws=20000, seed=20260727):
        self.label = label
        self.ref_tag, self.treat_tag = ref_tag, treat_tag
        self.iids = list(iids)
        self.meta = meta
        self.draws, self.seed = draws, seed
        self.ref_src, self.treat_src = ref_src, treat_src
        self.have_resolved = ref_resolved is not None and treat_resolved is not None
        self.ref = {i: load_session(results_root, ref_tag, i) for i in self.iids}
        self.treat = {i: load_session(results_root, treat_tag, i) for i in self.iids}
        rr = ref_resolved or set()
        tr = treat_resolved or set()
        # resolved := gate passed AND harness says resolved. Denominator = full set.
        self.res_ref = {i: bool(self.ref[i].gate and i in rr) for i in self.iids}
        self.res_treat = {i: bool(self.treat[i].gate and i in tr) for i in self.iids}
        # sets
        self.gated_both = [i for i in self.iids if self.ref[i].gate and self.treat[i].gate]
        self.clean = [i for i in self.gated_both
                      if not self.ref[i].degenerate and not self.treat[i].degenerate]
        self.excluded = [i for i in self.gated_both if i not in set(self.clean)]
        self.gate_fail_ref = [i for i in self.iids if not self.ref[i].gate]
        self.gate_fail_treat = [i for i in self.iids if not self.treat[i].gate]

    # ---- per-unit paired stats
    def logs(self, unit, ids):
        out = []
        for i in ids:
            a, b = self.treat[i].unit(unit), self.ref[i].unit(unit)
            if a > 0 and b > 0:
                out.append(math.log(a / b))
        return out

    def pairs(self, unit, ids):
        return [(self.treat[i].unit(unit), self.ref[i].unit(unit)) for i in ids
                if self.treat[i].unit(unit) > 0 and self.ref[i].unit(unit) > 0]

    def paired_block(self, unit, ids):
        lg = self.logs(unit, ids)
        pr = self.pairs(unit, ids)
        pooled = (sum(a for a, _ in pr) / sum(b for _, b in pr) - 1.0) if pr else None
        ci = bootstrap_ci(lambda s: geo([lg[k] for k in s]), len(lg), self.draws, self.seed)
        return {
            "n_pairs": len(lg),
            "geo": geo(lg), "median": median_ratio(lg), "trimmed10": trimmed(lg),
            "pooled": pooled,
            "ci_geo": ci,
            "loo_geo": loo_range(lg, geo),
            "loo_pooled": loo_range(pr, lambda v: (sum(a for a, _ in v) / sum(b for _, b in v) - 1.0) if v else None),
            "sum_treat": sum(a for a, _ in pr), "sum_ref": sum(b for _, b in pr),
        }

    def macro_block(self, unit, ids, keyfn):
        groups = {}
        for i in ids:
            a, b = self.treat[i].unit(unit), self.ref[i].unit(unit)
            if a > 0 and b > 0:
                groups.setdefault(keyfn(i), []).append(math.log(a / b))
        per = {k: sum(v) / len(v) for k, v in groups.items()}
        vals = list(per.values())
        macro = math.exp(sum(vals) / len(vals)) - 1.0 if vals else None
        ci = bootstrap_ci(
            lambda s: (math.exp(sum(vals[k] for k in s) / len(s)) - 1.0) if s else None,
            len(vals), self.draws, self.seed + 1)
        return {"macro": macro, "n_groups": len(vals), "ci": ci,
                "loo": loo_range(vals, lambda v: (math.exp(sum(v) / len(v)) - 1.0) if v else None),
                "per_group_logmean": per,
                "group_n": {k: len(v) for k, v in groups.items()}}

    # ---- spend / resolved
    def spend(self, unit, ids, which):
        arm = self.treat if which == "treat" else self.ref
        return sum(arm[i].unit(unit) for i in ids)

    def all_spend_ids(self, which):
        arm = self.treat if which == "treat" else self.ref
        return [i for i in self.iids if arm[i].usd > 0]

    def per_resolved(self, unit, which, numerator_ids=None):
        arm = self.treat if which == "treat" else self.ref
        res = self.res_treat if which == "treat" else self.res_ref
        ids = numerator_ids if numerator_ids is not None else self.all_spend_ids(which)
        numer = sum(arm[i].unit(unit) for i in ids)
        den = sum(1 for i in self.iids if res[i])
        return {"value": (numer / den) if den else None, "numer": numer, "den": den,
                "n_sessions": len(ids), "n_instances": len(self.iids)}

    def per_resolved_ratio(self, unit):
        """treat/ref ratio of (all-spend / resolved), with bootstrap + LOO over instances."""
        ids = self.iids
        rows = []
        for i in ids:
            rows.append((
                self.treat[i].unit(unit) if self.treat[i].usd > 0 else 0.0,
                1 if self.res_treat[i] else 0,
                self.ref[i].unit(unit) if self.ref[i].usd > 0 else 0.0,
                1 if self.res_ref[i] else 0))

        def stat(v):
            if not v:
                return None
            ta = sum(r[0] for r in v); ra = sum(r[1] for r in v)
            tb = sum(r[2] for r in v); rb = sum(r[3] for r in v)
            if ra == 0 or rb == 0:
                return None
            return (ta / ra) / (tb / rb) - 1.0

        ci = bootstrap_ci(lambda s: stat([rows[k] for k in s]), len(rows), self.draws,
                          self.seed + 2)
        return {"point": stat(rows), "ci": ci, "loo": loo_range(rows, stat)}

    # ---- group tables
    def group_table(self, keyname):
        keyfn = (lambda i: self.meta.get(i, {}).get("repo_org", "?")) if keyname == "repo" \
            else (lambda i: self.meta.get(i, {}).get("language", "other"))
        keys = {}
        for i in self.iids:
            keys.setdefault(keyfn(i), []).append(i)
        rows = []
        for k in sorted(keys, key=lambda k: (-len(keys[k]), k)):
            ids = keys[k]
            cl = [i for i in ids if i in set(self.clean)]
            lg_usd = self.logs("usd", cl)
            lg_new = self.logs("billed_new", cl)
            rres = sum(1 for i in ids if self.res_ref[i])
            tres = sum(1 for i in ids if self.res_treat[i])
            rspend = sum(self.ref[i].usd for i in ids if self.ref[i].usd > 0)
            tspend = sum(self.treat[i].usd for i in ids if self.treat[i].usd > 0)
            loo = loo_range(lg_usd, geo)
            rows.append({
                "key": k, "n": len(ids), "n_clean": len(cl),
                "res_ref": rres, "res_treat": tres,
                "usd_per_res_ref": (rspend / rres) if rres else None,
                "usd_per_res_treat": (tspend / tres) if tres else None,
                "geo_usd": geo(lg_usd), "geo_new": geo(lg_new),
                "stable": ("n/a" if len(lg_usd) < 2 else ("yes" if loo["sign_holds"] else "NO")),
                "loo_min": loo["min"], "loo_max": loo["max"],
            })
        return rows

    # ---- everything
    def compute(self):
        out = {"label": self.label, "ref": self.ref_tag, "treat": self.treat_tag,
               "n_instances": len(self.iids), "draws": self.draws, "seed": self.seed,
               "ref_report": self.ref_src, "treat_report": self.treat_src,
               "have_resolved": self.have_resolved}
        out["gate"] = {
            "ref_gate_pass": len(self.iids) - len(self.gate_fail_ref),
            "treat_gate_pass": len(self.iids) - len(self.gate_fail_treat),
            "ref_gate_fail_ids": self.gate_fail_ref,
            "treat_gate_fail_ids": self.gate_fail_treat,
        }
        out["degenerate"] = {
            "n_gated_both": len(self.gated_both),
            "n_clean_pairs": len(self.clean),
            "n_excluded": len(self.excluded),
            "excluded": [{"iid": i,
                          "ref": self.ref[i].reasons, "treat": self.treat[i].reasons,
                          "ref_total_tokens": self.ref[i].total_tokens,
                          "treat_total_tokens": self.treat[i].total_tokens,
                          "ref_usd": round(self.ref[i].usd, 4),
                          "treat_usd": round(self.treat[i].usd, 4)}
                         for i in self.excluded],
        }
        out["paired"] = {}
        for unit in UNITS:
            out["paired"][unit] = {
                "clean": self.paired_block(unit, self.clean),
                "all_gated": self.paired_block(unit, self.gated_both),
                "macro_repo": self.macro_block(
                    unit, self.clean, lambda i: self.meta.get(i, {}).get("repo_org", "?")),
                "macro_lang": self.macro_block(
                    unit, self.clean, lambda i: self.meta.get(i, {}).get("language", "other")),
            }
        if self.have_resolved:
            out["resolved"] = {
                "ref": sum(1 for i in self.iids if self.res_ref[i]),
                "treat": sum(1 for i in self.iids if self.res_treat[i]),
                "n": len(self.iids),
                "mcnemar_treat_vs_ref": mcnemar_exact(self.res_treat, self.res_ref, self.iids),
                "per_resolved": {u: {"ref": self.per_resolved(u, "ref"),
                                     "treat": self.per_resolved(u, "treat"),
                                     "ratio": self.per_resolved_ratio(u)} for u in UNITS},
                "per_resolved_cleanspend": {
                    u: {"ref": self.per_resolved(u, "ref", self.clean),
                        "treat": self.per_resolved(u, "treat", self.clean)} for u in UNITS},
                "loo_resolved_delta": loo_range(
                    [(1 if self.res_treat[i] else 0) - (1 if self.res_ref[i] else 0)
                     for i in self.iids], lambda v: float(sum(v))),
            }
            out["repo_table"] = self.group_table("repo")
            out["lang_table"] = self.group_table("lang")
        else:
            out["repo_table"] = self.group_table("repo")
            out["lang_table"] = self.group_table("lang")
        return out


# --------------------------------------------------------------------------- printing

HEADER = """\
================================================================================
UNITS   usd          = total_cost_usd                       <- MONEY CLAIM
        billed_new   = input + cache_creation + output       <- WORK SIGNAL
        total_tokens = billed_new + cache_read               <- outlier-prone:
        total_tokens overweights cache_read because cache reads bill at a fraction
        (~1/10) of input price. Quote usd for cost claims, billed_new for work.
GATE    cost_usd>0 AND non-empty patch. Gated-out => UNRESOLVED for that arm and
        KEPT in the denominator for every arm. Arms are never intersected.
CLEAN   token stats exclude degenerate sessions (empty patch / is_error / subtype
        in error_max_turns, aborted_streaming, error_during_execution) in EITHER
        arm; resolve denominators keep them. ALL-GATED row = sensitivity check,
        because the exclusion is itself an analysis choice.
RATIO   geo = exp(mean ln(treat/ref)) - 1 (2x up + 2x down cancels to 0%).
        pooled = sum/sum - 1 (outlier-prone, shown for continuity only).
COLLIDER  paired stats condition on BOTH arms gating + being clean, and the arms
        differ in which instances that is - so paired ratios are a matched-task
        diagnostic, not the unconditioned bottom line. Section [5] (all spend
        actually incurred / resolved count) is the unconditioned money number.
================================================================================"""


def print_comparison(r):
    L = print
    L("")
    L("#" * 80)
    L(f"# {r['label']}")
    L(f"#   treat = {r['treat']}   ref = {r['ref']}   instances = {r['n_instances']}")
    L(f"#   treat report: {r['treat_report'] or 'NONE (token metrics only)'}")
    L(f"#   ref   report: {r['ref_report'] or 'NONE (token metrics only)'}")
    L(f"#   bootstrap draws = {r['draws']:,}  seed = {r['seed']}")
    L("#" * 80)

    g = r["gate"]
    L(f"\n[1] GATE  ref pass {g['ref_gate_pass']}/{r['n_instances']}   "
      f"treat pass {g['treat_gate_pass']}/{r['n_instances']}")
    if g["ref_gate_fail_ids"]:
        L(f"    ref gate-fail  ({len(g['ref_gate_fail_ids'])}): "
          f"{', '.join(g['ref_gate_fail_ids'])}")
    if g["treat_gate_fail_ids"]:
        L(f"    treat gate-fail({len(g['treat_gate_fail_ids'])}): "
          f"{', '.join(g['treat_gate_fail_ids'])}")
    d = r["degenerate"]
    L(f"    gated in BOTH arms: {d['n_gated_both']}   clean pairs for token stats: "
      f"{d['n_clean_pairs']}   excluded as degenerate: {d['n_excluded']}")
    for e in d["excluded"]:
        L(f"      - {e['iid']:34s} ref={','.join(e['ref']) or 'ok':28s} "
          f"treat={','.join(e['treat']) or 'ok':28s} "
          f"tok ref={e['ref_total_tokens']:>9,} treat={e['treat_total_tokens']:>9,}")

    L("\n[2] PAIRED RATIOS  (treat vs ref, % change; negative = treat cheaper)")
    L(f"    {'unit':<13}{'set':<10}{'n':>4}  {'geo':>9} {'median':>9} {'trim10':>9} "
      f"{'pooled*':>9}  {'geo 95% CI':>20}  {'geo LOO range':>20} sign")
    for unit in UNITS:
        for setname in ("clean", "all_gated"):
            b = r["paired"][unit][setname]
            ci = b["ci_geo"]; lo = b["loo_geo"]
            L(f"    {unit:<13}{setname:<10}{b['n_pairs']:>4}  {pct(b['geo']):>9} "
              f"{pct(b['median']):>9} {pct(b['trimmed10']):>9} {pct(b['pooled']):>9}  "
              f"{('[' + pct(ci['lo']) + ', ' + pct(ci['hi']) + ']'):>20}  "
              f"{('[' + pct(lo['min']) + ', ' + pct(lo['max']) + ']'):>20} "
              f"{'holds' if lo['sign_holds'] else 'FLIPS'}")
    L("    * pooled = sum/sum, outlier-prone")

    L("\n[3] MICRO vs MACRO  (clean pairs)")
    L(f"    {'unit':<13}{'micro(geo)':>12} {'macro/repo':>12} {'repos':>6} "
      f"{'macro/lang':>12} {'langs':>6}  divergence(micro-macro,repo)")
    for unit in UNITS:
        p = r["paired"][unit]
        mi = p["clean"]["geo"]; ma = p["macro_repo"]["macro"]; mal = p["macro_lang"]["macro"]
        div = None if (mi is None or ma is None) else (mi - ma)
        L(f"    {unit:<13}{pct(mi):>12} {pct(ma):>12} {p['macro_repo']['n_groups']:>6} "
          f"{pct(mal):>12} {p['macro_lang']['n_groups']:>6}  "
          f"{('n/a' if div is None else f'{div*100:+.1f} pt')}")
    for unit in UNITS:
        mr = r["paired"][unit]["macro_repo"]
        ci = mr["ci"]; lo = mr["loo"]
        L(f"      macro/repo {unit:<13} 95% CI [{pct(ci['lo'])}, {pct(ci['hi'])}]  "
          f"LOO(repo) [{pct(lo['min'])}, {pct(lo['max'])}] "
          f"{'holds' if lo['sign_holds'] else 'FLIPS'}")

    if r.get("have_resolved"):
        rr = r["resolved"]
        L(f"\n[4] RESOLVED  ref {rr['ref']}/{rr['n']}   treat {rr['treat']}/{rr['n']}   "
          f"delta {rr['treat'] - rr['ref']:+d}")
        m = rr["mcnemar_treat_vs_ref"]
        L(f"    McNemar exact: treat-only wins {m['a_only']}, ref-only wins {m['b_only']}, "
          f"discordant {m['n_discordant']}, two-sided p = {m['p_two_sided']:.4f}")
        lo = rr["loo_resolved_delta"]
        L(f"    resolved-delta LOO range [{lo['min']:+.0f}, {lo['max']:+.0f}] "
          f"{'sign holds' if lo['sign_holds'] else 'SIGN FLIPS'}")
        L("\n[5] PER-RESOLVED  (numerator = all spend actually incurred on the instance "
          "set; denominator = resolved count)")
        for u in UNITS:
            pr = rr["per_resolved"][u]
            a, b2 = pr["treat"], pr["ref"]
            fmt = money if u == "usd" else num
            L(f"    {u:<13} ref   = {fmt(b2['value']):>14}  "
              f"( {fmt(b2['numer']):>14} over {b2['n_sessions']:>3} sessions / "
              f"{b2['den']:>3} resolved of {b2['n_instances']} )")
            L(f"    {'':<13} treat = {fmt(a['value']):>14}  "
              f"( {fmt(a['numer']):>14} over {a['n_sessions']:>3} sessions / "
              f"{a['den']:>3} resolved of {a['n_instances']} )")
            rt = pr["ratio"]
            L(f"    {'':<13} ratio = {pct(rt['point'])}  95% CI "
              f"[{pct(rt['ci']['lo'])}, {pct(rt['ci']['hi'])}]  LOO "
              f"[{pct(rt['loo']['min'])}, {pct(rt['loo']['max'])}] "
              f"{'holds' if rt['loo']['sign_holds'] else 'FLIPS'}")
        L("    (denominator sensitivity: at these n, +-1 resolved moves per-resolved by "
          f"~{100.0 / max(1, rr['ref']):.0f}% on the ref arm)")
        L("    clean-spend variant (numerator restricted to clean pairs):")
        for u in UNITS:
            cv = rr["per_resolved_cleanspend"][u]
            fmt = money if u == "usd" else num
            L(f"      {u:<13} ref {fmt(cv['ref']['value']):>14} "
              f"treat {fmt(cv['treat']['value']):>14}")
    else:
        L("\n[4] RESOLVED  NO EVAL REPORT SUPPLIED -> token metrics only, no resolve "
          "rates, no per-resolved, no McNemar.")

    hr = bool(r.get("have_resolved"))
    for tname, title in (("repo_table", "[6] PER-REPO (org prefix before __)"),
                         ("lang_table", "[7] PER-LANGUAGE (from gold patch extensions)")):
        L(f"\n{title}" + ("" if hr else "   (no eval report: resolve columns = '-')"))
        L(f"    {'key':<16}{'n':>4}{'cln':>4}{'res_ref':>8}{'res_tr':>7}"
          f"{'usd/res ref':>13}{'usd/res tr':>12}{'geo usd':>10}{'geo new':>10}"
          f"  {'LOO usd range':>22} stable")
        for row in r[tname]:
            rr_ = f"{row['res_ref']:>8}" if hr else f"{'-':>8}"
            rt_ = f"{row['res_treat']:>7}" if hr else f"{'-':>7}"
            ur_ = f"{money(row['usd_per_res_ref']):>13}" if hr else f"{'-':>13}"
            ut_ = f"{money(row['usd_per_res_treat']):>12}" if hr else f"{'-':>12}"
            L(f"    {row['key']:<16}{row['n']:>4}{row['n_clean']:>4}{rr_}{rt_}{ur_}{ut_}"
              f"{pct(row['geo_usd']):>10}{pct(row['geo_new']):>10}  "
              f"{('[' + pct(row['loo_min']) + ', ' + pct(row['loo_max']) + ']'):>22} "
              f"{row['stable']}")


# --------------------------------------------------------------------------- self-test

def _mk_session(root, tag_arm, iid, usd, inp, cc, cr, out, subtype="success",
                is_error=False, patch="diff --git a/x b/x\n+1\n"):
    d = os.path.join(root, tag_arm, iid)
    os.makedirs(d, exist_ok=True)
    json.dump({"total_cost_usd": usd, "num_turns": 5, "subtype": subtype,
               "is_error": is_error,
               "usage": {"input_tokens": inp, "cache_creation_input_tokens": cc,
                         "cache_read_input_tokens": cr, "output_tokens": out}},
              open(os.path.join(d, "cc_out.json"), "w"))
    open(os.path.join(d, "patch.diff"), "w").write(patch)


def self_test() -> int:
    tmp = tempfile.mkdtemp(prefix="metrics-selftest-")
    fails = []

    def check(name, cond, detail=""):
        print(f"    {'PASS' if cond else 'FAIL'}  {name}" + (f"  [{detail}]" if detail else ""))
        if not cond:
            fails.append(name)

    try:
        res = os.path.join(tmp, "results")
        # ---------- fixture 1: gate + degenerate + log-ratio symmetry
        # A: treat 2x ref ; B: treat 0.5x ref  -> geo == 0%
        _mk_session(res, "R/ref", "orgA__p-1", 1.0, 100, 0, 0, 0)
        _mk_session(res, "T/tr", "orgA__p-1", 2.0, 200, 0, 0, 0)
        _mk_session(res, "R/ref", "orgA__p-2", 1.0, 100, 0, 0, 0)
        _mk_session(res, "T/tr", "orgA__p-2", 0.5, 50, 0, 0, 0)
        # C: gate fail in treat (zero cost) but present in resolved report
        _mk_session(res, "R/ref", "orgA__p-3", 1.0, 100, 0, 0, 0)
        _mk_session(res, "T/tr", "orgA__p-3", 0.0, 100, 0, 0, 0)
        # D: degenerate aborted session in treat with a giant token count
        _mk_session(res, "R/ref", "orgA__p-4", 1.0, 100, 0, 0, 0)
        _mk_session(res, "T/tr", "orgA__p-4", 9.0, 2_500_000, 0, 0, 0,
                    subtype="aborted_streaming", is_error=True)
        ids = ["orgA__p-1", "orgA__p-2", "orgA__p-3", "orgA__p-4"]
        meta = {i: {"repo_org": "orgA", "language": "Python"} for i in ids}
        c = Comparison("selftest-1", res, "R/ref", "T/tr", ids, meta,
                       ref_resolved={"orgA__p-1", "orgA__p-3"},
                       treat_resolved={"orgA__p-1", "orgA__p-3", "orgA__p-4"},
                       draws=2000, seed=1)
        r = c.compute()

        print("  gate")
        check("gate-fail instance is unresolved for that arm despite report saying resolved",
              r["resolved"]["treat"] == 2 and "orgA__p-3" in r["gate"]["treat_gate_fail_ids"],
              f"treat resolved={r['resolved']['treat']} (p-1,p-4 only)")
        check("gated-out instance stays in denominator for BOTH arms",
              r["resolved"]["n"] == 4 and r["n_instances"] == 4)
        check("ref resolved unaffected by treat's gate failure",
              r["resolved"]["ref"] == 2)

        print("  log-ratio symmetry")
        gp = r["paired"]["billed_new"]["clean"]
        check("2x-up + 2x-down pair -> geo == 0.0%", abs(gp["geo"]) < 1e-12,
              f"geo={pct(gp['geo'],4)} n_pairs={gp['n_pairs']}")
        check("pooled ratio on the same pair is NOT 0 (outlier-prone)",
              abs(gp["pooled"]) > 0.2, f"pooled={pct(gp['pooled'])}")

        print("  degenerate exclusion")
        check("aborted 2.5M-token session excluded from token stats",
              gp["n_pairs"] == 2 and r["degenerate"]["n_excluded"] == 1,
              f"clean_pairs={gp['n_pairs']} excluded={r['degenerate']['n_excluded']}")
        check("excluded id + reason reported",
              r["degenerate"]["excluded"][0]["iid"] == "orgA__p-4"
              and "subtype:aborted_streaming" in r["degenerate"]["excluded"][0]["treat"])
        ag = r["paired"]["billed_new"]["all_gated"]
        check("ALL-GATED sensitivity row does include it and is inflated",
              ag["n_pairs"] == 3 and ag["geo"] > 1.0, f"all_gated geo={pct(ag['geo'])}")
        check("degenerate session still counted in resolve denominator",
              r["resolved"]["n"] == 4)

        # ---------- fixture 2: macro vs micro divergence
        res2 = os.path.join(tmp, "results2")
        ids2, meta2 = [], {}
        for k in range(10):                      # big repo, treat 0.5x
            i = f"big__p-{k}"
            _mk_session(res2, "R/ref", i, 1.0, 1000, 0, 0, 0)
            _mk_session(res2, "T/tr", i, 0.5, 500, 0, 0, 0)
            ids2.append(i); meta2[i] = {"repo_org": "big", "language": "Python"}
        i = "small__p-0"                          # small repo, treat 4x
        _mk_session(res2, "R/ref", i, 1.0, 1000, 0, 0, 0)
        _mk_session(res2, "T/tr", i, 4.0, 4000, 0, 0, 0)
        ids2.append(i); meta2[i] = {"repo_org": "small", "language": "Go"}
        c2 = Comparison("selftest-2", res2, "R/ref", "T/tr", ids2, meta2, draws=500, seed=2)
        r2 = c2.compute()
        mi = r2["paired"]["usd"]["clean"]["geo"]
        ma = r2["paired"]["usd"]["macro_repo"]["macro"]
        print("  macro/micro divergence")
        check("micro geo ~ -39.6% (dominated by the 10-instance repo)", abs(mi + 0.3958) < 0.01,
              pct(mi))
        check("macro/repo geo ~ +41.4% (repos weighted equally)", abs(ma - 0.4142) < 0.01,
              pct(ma))
        check("micro and macro disagree in SIGN on this fixture", (mi < 0) and (ma > 0),
              f"micro={pct(mi)} macro={pct(ma)}")

        print("  units")
        # cache_read must not touch billed_new but must dominate total_tokens
        res3 = os.path.join(tmp, "results3")
        _mk_session(res3, "R/ref", "u__1", 1.0, 100, 100, 1_000_000, 100)
        _mk_session(res3, "T/tr", "u__1", 1.0, 100, 100, 10, 100)
        c3 = Comparison("selftest-3", res3, "R/ref", "T/tr", ["u__1"],
                        {"u__1": {"repo_org": "u", "language": "C"}}, draws=100, seed=3)
        r3 = c3.compute()
        check("billed_new ignores cache_read",
              abs(r3["paired"]["billed_new"]["clean"]["geo"]) < 1e-12)
        check("total_tokens swings hugely on cache_read alone",
              r3["paired"]["total_tokens"]["clean"]["geo"] < -0.9,
              pct(r3["paired"]["total_tokens"]["clean"]["geo"]))
        check("usd unchanged", abs(r3["paired"]["usd"]["clean"]["geo"]) < 1e-12)

        print("  mcnemar")
        rm = {"a": 1, "b": 1, "c": 0, "d": 0}
        rn = {"a": 1, "b": 0, "c": 1, "d": 0}
        m = mcnemar_exact(rm, rn, list(rm))
        check("McNemar exact on 1/1 discordant -> p=1.0",
              m["n_discordant"] == 2 and abs(m["p_two_sided"] - 1.0) < 1e-9, str(m))
        m2 = mcnemar_exact({"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1},
                           {"a": 0, "b": 0, "c": 0, "d": 0, "e": 0, "f": 0}, list("abcdef"))
        check("McNemar exact 6-0 discordant -> p=0.03125", abs(m2["p_two_sided"] - 0.03125) < 1e-9,
              str(m2))

        print("  loo")
        lo = loo_range([math.log(0.5)] * 3 + [math.log(8.0)], geo)
        check("LOO detects a sign flip driven by one instance", not lo["sign_holds"],
              f"full={pct(lo['full'])} range=[{pct(lo['min'])},{pct(lo['max'])}]")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print(f"\n  SELF-TEST: {'ALL PASS' if not fails else 'FAILURES: ' + ', '.join(fails)}")
    return 1 if fails else 0


# --------------------------------------------------------------------------- main

def read_ids(path: str) -> list:
    seen, out = set(), []
    for line in open(path):
        s = line.strip()
        if s and s not in seen:
            seen.add(s)
            out.append(s)
    return out


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--results-root", default=None,
                    help="dir holding <tag>/<arm>/<iid>/ (default: ../results next to tools/)")
    ap.add_argument("--tasks", default=None,
                    help="task json with gold patches (default: ../tasks/pilot_tasks.json)")
    ap.add_argument("--instances", help="file with one instance id per line")
    ap.add_argument("--ref", help="reference arm as tag/arm, e.g. scr100base_r1/baseline")
    ap.add_argument("--treat", help="treatment arm as tag/arm")
    ap.add_argument("--ref-report", default=None,
                    help="resolved-set source: report.json | reports.json#arm | dir:<logs>")
    ap.add_argument("--treat-report", default=None)
    ap.add_argument("--ref-resolved-add", default="",
                    help="comma-separated ids to add to ref resolved set (documented corrections)")
    ap.add_argument("--treat-resolved-add", default="")
    ap.add_argument("--label", default=None)
    ap.add_argument("--bootstrap", type=int, default=20000)
    ap.add_argument("--seed", type=int, default=20260727)
    ap.add_argument("--json", default=None, help="write full result dict here")
    ap.add_argument("--lang-check", action="store_true",
                    help="cross-check derived language against a declared language field")
    a = ap.parse_args(argv)

    if a.self_test:
        print(HEADER)
        print("\nSELF-TEST (synthetic fixtures)")
        return self_test()

    here = os.path.dirname(os.path.abspath(__file__))
    root = a.results_root or os.path.join(os.path.dirname(here), "results")
    tasks = a.tasks or os.path.join(os.path.dirname(here), "tasks", "pilot_tasks.json")
    if not (a.ref and a.treat and a.instances):
        ap.error("--ref, --treat and --instances are required (or use --self-test)")

    iids = read_ids(a.instances)
    meta = load_task_meta(tasks)
    missing_meta = [i for i in iids if i not in meta]
    if missing_meta:
        print(f"WARNING: {len(missing_meta)} instances missing from {tasks}: "
              f"{missing_meta[:5]}", file=sys.stderr)

    if a.lang_check:
        bad = [(i, meta[i]["language"], meta[i]["declared_language"]) for i in iids
               if i in meta and meta[i]["declared_language"]
               and meta[i]["declared_language"].lower() != meta[i]["language"].lower()]
        n = sum(1 for i in iids if i in meta and meta[i]["declared_language"])
        print(f"LANG-CHECK: derived-vs-declared disagreements {len(bad)}/{n}")
        for b in bad[:20]:
            print(f"   {b[0]:34s} derived={b[1]:12s} declared={b[2]}")

    ref_res = tref = None
    ref_src = tsrc = ""
    if a.ref_report:
        ref_res, ref_src = load_resolved(a.ref_report)
    if a.treat_report:
        tref, tsrc = load_resolved(a.treat_report)
    for extra, target, tag in ((a.ref_resolved_add, ref_res, "ref"),
                              (a.treat_resolved_add, tref, "treat")):
        if extra.strip():
            add = [x.strip() for x in extra.split(",") if x.strip()]
            if target is None:
                ap.error(f"--{tag}-resolved-add needs --{tag}-report")
            target.update(add)
            if tag == "ref":
                ref_src += f" +corrections{add}"
            else:
                tsrc += f" +corrections{add}"

    label = a.label or f"{a.treat} vs {a.ref}  (n={len(iids)})"
    c = Comparison(label, root, a.ref, a.treat, iids, meta, ref_res, tref,
                   ref_src, tsrc, a.bootstrap, a.seed)
    r = c.compute()
    print(HEADER)
    print_comparison(r)
    if a.json:
        json.dump(r, open(a.json, "w"), indent=1, default=str)
        print(f"\n[json] {a.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
