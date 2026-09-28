#!/usr/bin/env python3
"""Measure the existing benchmark runner, preserving its quarantine/failure policy."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty")
    config = output / "coverage.ini"
    config.write_text(
        "[run]\nbranch = True\nparallel = True\npatch = subprocess\n"
        f"data_file = {output / '.coverage'}\n"
        f"source = {ROOT / 'benchmarks/agent-brain'}\n"
        "omit = */test_*.py,*/tests/*,*_test.py,*/vendor/*\n"
        "[report]\nskip_empty = True\n"
    )
    metadata = {
        "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "python": sys.version,
        "coverage": subprocess.check_output([sys.executable, "-m", "coverage", "--version"], text=True).strip(),
        "scope": "first-party benchmark Python; includes attempted quarantined/failed modules",
    }
    (output / "environment.json").write_text(json.dumps(metadata, indent=2) + "\n")
    base = [sys.executable, "-B", "-m", "coverage"]
    with (output / "tests.log").open("w") as log:
        result = subprocess.run([*base, "run", f"--rcfile={config}",
                                 "benchmarks/agent-brain/ci_tests.py"], cwd=ROOT,
                                stdout=log, stderr=subprocess.STDOUT)
    # Report even when a real test fails. Keep the original failure as the exit status.
    # New coverage versions combine automatically; explicitly consume any
    # remaining shards so report generation does not depend on that behavior.
    combine_exit = 0
    if list(output.glob(".coverage.*")):
        combine_exit = subprocess.run(
            [*base, "combine", f"--rcfile={config}", "--keep"], cwd=ROOT
        ).returncode
    report_result = subprocess.run([*base, "json", f"--rcfile={config}",
                                    "-o", str(output / "coverage.json")], cwd=ROOT)
    html_result = subprocess.run([*base, "html", f"--rcfile={config}", "-d", str(output / "html")], cwd=ROOT)
    lines = (output / "tests.log").read_text().splitlines()
    summary = [line for line in lines if line.startswith(("ok ", "FAIL ", "quarantined ", "skipped ", "UNEXPECTED PASS", "failing test module:", "stale quarantine"))]
    (output / "test-results.json").write_text(json.dumps({"exit_code": result.returncode, "modules": summary}, indent=2) + "\n")
    print("\n".join(summary))
    return result.returncode or combine_exit or report_result.returncode or html_result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
