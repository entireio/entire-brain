#!/usr/bin/env python3
"""Regression Radar eval — measures the `inspect regressions` detector honestly:
RECALL (does it flag the true seeded regression line?) and FALSE-ALARM (does a clean tree stay
quiet?). Builds entire-brain + entire-sem, reuses a prepped cli-bench brain cache, and toggles the
seeded regression in a temp checkout of cli-bench.

Usage: python3 regression_eval.py [CLI_BENCH_PATH] [CACHE_DIR]
"""
import glob, json, os, pathlib, shutil, subprocess, sys, tempfile

BENCH = pathlib.Path(__file__).resolve().parent
ROOT = BENCH.parents[1]
CLI_BENCH = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path("/Users/suhaan/Documents/Coding/cli-bench")
_cache = sys.argv[2] if len(sys.argv) > 2 else next(iter(sorted(glob.glob(str(BENCH / "cache" / "*")))), None)
CACHE = pathlib.Path(_cache) if _cache else None

TASKS = {
    "review": ("entireio-cli-review-base-flag-scope", "cmd/entire/cli/review_context.go",
               "review base flag scope regression reviewContextCommitMessages scopeBaseRef BaseFlagThreadsThroughToPromptAndBanner", []),
    "transcript": ("entireio-cli-transcript-reresolve", "cmd/entire/cli/strategy/resolve_transcript.go",
                   "transcript reresolve resolveTranscriptPath TranscriptPath ReResolvesToNestedLayout", ["--include-deletions"]),
}


def build():
    bindir = pathlib.Path(tempfile.mkdtemp(prefix="regeval-bin-"))
    subprocess.run(["go", "build", "-o", str(bindir / "entire-brain"), "./cmd/entire-brain"], cwd=ROOT, check=True)
    subprocess.run(["go", "build", "-o", str(bindir / "entire-sem"), "./cmd/entire-sem"], cwd=ROOT.parent / "entire-sem", check=True)
    return bindir


def run_detect(bindir, repo, query, extra):
    env = dict(os.environ)
    env["PATH"] = f"{bindir}:{env['PATH']}"
    env.update({
        "ENTIRE_REPO_ROOT": str(repo),
        "ENTIRE_PLUGIN_DATA_DIR": str(CACHE / "plugin" / "data"),
        "ENTIRE_PLUGIN_STATE_DIR": str(CACHE / "plugin" / "state"),
        "ENTIRE_PLUGIN_CONFIG_DIR": str(CACHE / "plugin" / "config"),
        "ENTIRE_PLUGIN_CACHE_DIR": str(CACHE / "plugin" / "cache"),
    })
    out = subprocess.run([str(bindir / "entire-brain"), "inspect", "regressions", query, *extra, "--json"],
                         env=env, capture_output=True, text=True)
    try:
        return json.loads(out.stdout).get("anomalies") or []
    except Exception:
        return []


def main():
    if CACHE is None or not (CACHE / "plugin" / "data").exists():
        sys.exit(f"no prepped brain cache found at {CACHE}")
    bindir = build()
    rows = []
    for label, (task_id, fix_file, query, extra) in TASKS.items():
        task = json.loads((BENCH / "tasks" / f"{task_id}.json").read_text())
        rep = task["setup_replacements"][0]
        target = CLI_BENCH / fix_file
        original = target.read_text()

        # clean → false alarm count
        clean = run_detect(bindir, CLI_BENCH, query, extra)
        false_alarms = len(clean)

        # regressed → recall (flag on the true fix file?)
        assert rep["old"] in original, f"{label}: seeded OLD not present"
        target.write_text(original.replace(rep["old"], rep["new"], 1))
        try:
            regressed = run_detect(bindir, CLI_BENCH, query, extra)
        finally:
            target.write_text(original)
        hit = [a for a in regressed if fix_file.split("/")[-1] in a.get("file", "")]
        rows.append((label, extra, bool(hit), false_alarms, hit[0] if hit else None))

    print(f"{'task':12}{'mode':18}{'recall':8}{'false_alarms(clean)':22}detail")
    print("-" * 78)
    for label, extra, recalled, fa, h in rows:
        mode = " ".join(extra) or "default(changed)"
        detail = f"{h['file'].split('/')[-1]}:{h['line']} [{h['kind']}]" if h else "-"
        print(f"{label:12}{mode:18}{'YES' if recalled else 'NO':8}{fa:<22}{detail}")
    shutil.rmtree(bindir, ignore_errors=True)


if __name__ == "__main__":
    main()
