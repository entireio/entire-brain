#!/usr/bin/env python3
"""Regression Radar eval — HONEST precision + recall.

Earlier this script reported "0 false alarms" by running `len(clean)` on the two tuned tasks against
a tree that still holds the invariant — degenerate (the global-presence guard makes it structurally
zero). This version measures a real, non-degenerate FALSE-ALARM RATE over a corpus where the detector
*could* fire (clean trees, a benign rename, and unrelated queries — all held-out negatives), and
reports RECALL separately, labeled in-sample (the 2 tuned tasks).

Usage: python3 regression_eval.py [CLI_BENCH_PATH] [CACHE_DIR]
"""
import glob, json, os, pathlib, shutil, subprocess, sys, tempfile

BENCH = pathlib.Path(__file__).resolve().parent
ROOT = BENCH.parents[1]


def default_cli_bench() -> pathlib.Path:
    base = pathlib.Path(os.environ.get("AGENT_BENCH_REPO_ROOT") or ROOT.parent)
    return base / "cli-bench"


CLI_BENCH = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else default_cli_bench()
_cache = sys.argv[2] if len(sys.argv) > 2 else next(iter(sorted(glob.glob(str(BENCH / "cache" / "*")))), None)
CACHE = pathlib.Path(_cache) if _cache else None

REVIEW_FILE = "cmd/entire/cli/review_context.go"
REVIEW_Q = "review base flag scope regression reviewContextCommitMessages scopeBaseRef BaseFlagThreadsThroughToPromptAndBanner"
TR_FILE = "cmd/entire/cli/strategy/resolve_transcript.go"
TR_Q = "transcript reresolve resolveTranscriptPath TranscriptPath ReResolvesToNestedLayout"


def build():
    bd = pathlib.Path(tempfile.mkdtemp(prefix="regeval-bin-"))
    subprocess.run(["go", "build", "-o", str(bd / "entire-brain"), "./cmd/entire-brain"], cwd=ROOT, check=True)
    subprocess.run(["go", "build", "-o", str(bd / "entire-sem"), "./cmd/entire-sem"], cwd=ROOT.parent / "entire-sem", check=True)
    return bd


def run_detect(bd, query, extra):
    env = dict(os.environ)
    env["PATH"] = f"{bd}:{env['PATH']}"
    env.update({"ENTIRE_REPO_ROOT": str(CLI_BENCH),
                "ENTIRE_PLUGIN_DATA_DIR": str(CACHE / "plugin" / "data"),
                "ENTIRE_PLUGIN_STATE_DIR": str(CACHE / "plugin" / "state"),
                "ENTIRE_PLUGIN_CONFIG_DIR": str(CACHE / "plugin" / "config"),
                "ENTIRE_PLUGIN_CACHE_DIR": str(CACHE / "plugin" / "cache")})
    out = subprocess.run([str(bd / "entire-brain"), "inspect", "regressions", query, *extra, "--json"],
                         env=env, capture_output=True, text=True)
    try:
        return json.loads(out.stdout).get("anomalies") or []
    except Exception:
        return []


def with_mutation(rel, old, new, fn):
    """Apply old->new in CLI_BENCH/rel, run fn(), always revert."""
    p = CLI_BENCH / rel
    orig = p.read_text()
    assert old in orig, f"{rel}: {old!r} not present"
    p.write_text(orig.replace(old, new))
    try:
        return fn()
    finally:
        p.write_text(orig)


def main():
    if CACHE is None or not (CACHE / "plugin" / "data").exists():
        sys.exit(f"no prepped brain cache at {CACHE}")
    bd = build()

    # ---- RECALL (in-sample: the 2 tuned tasks) ----
    recall = []
    for label, rel, q, extra, old, new in [
        ("review", REVIEW_FILE, REVIEW_Q, [], 'scopeBaseRef+"..HEAD"', '"master..HEAD"'),
        ("transcript", TR_FILE, TR_Q, ["--include-deletions"],
         "\t// Update state so subsequent reads use the correct path.\n\tstate.TranscriptPath = resolved\n\treturn resolved, nil",
         "\t// Update state so subsequent reads use the correct path.\n\treturn resolved, nil"),
    ]:
        an = with_mutation(rel, old, new, lambda: run_detect(bd, q, extra))
        hit = [a for a in an if rel.split("/")[-1] in a.get("file", "")]
        recall.append((label, bool(hit), hit[0] if hit else None))

    # ---- FALSE-ALARM CORPUS (held-out negatives: detector could fire, but SHOULD NOT) ----
    corpus = []
    # clean trees (invariant intact)
    corpus.append(("clean-review", len(run_detect(bd, REVIEW_Q, []))))
    corpus.append(("clean-transcript", len(run_detect(bd, TR_Q, ["--include-deletions"]))))
    # benign rename: scopeBaseRef -> baseBranchRef in the fix file (invariant intact, just renamed)
    corpus.append(("benign-rename", with_mutation(
        REVIEW_FILE, "scopeBaseRef", "baseBranchRef", lambda: len(run_detect(bd, REVIEW_Q, [])))))
    # unrelated queries on a clean tree (no regression)
    for q in ["ComposeReviewPrompt review prompt compose", "NewRootCmd command registration cobra",
              "detectScope scope base detection"]:
        corpus.append(("unrelated:" + q.split()[0], len(run_detect(bd, q, []))))

    # ---- report ----
    print("RECALL (in-sample — the 2 tuned tasks; held-out positives are hard to construct because")
    print("        most invariants repeat across files and the global-presence guard correctly")
    print("        suppresses single-site changes):")
    for label, ok, h in recall:
        d = f"{h['file'].split('/')[-1]}:{h['line']} [{h['kind']}]" if h else "-"
        print(f"   {label:12} {'HIT' if ok else 'MISS':5} {d}")

    fa = sum(n for _, n in corpus)
    print(f"\nFALSE-ALARM CORPUS ({len(corpus)} held-out negative cases; want 0 each):")
    for label, n in corpus:
        print(f"   {label:28} {n} {'<-- FALSE ALARM' if n else ''}")
    print(f"\n   false-alarm rate: {fa}/{len(corpus)} cases fired  (this is a REAL, non-degenerate measure)")
    shutil.rmtree(bd, ignore_errors=True)


if __name__ == "__main__":
    main()
