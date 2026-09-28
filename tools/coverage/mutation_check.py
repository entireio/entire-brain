#!/usr/bin/env python3
"""Prove selected regression tests fail for precise, temporary Go-overlay mutations."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def test_events(text):
    events = []
    for line in text.splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            continue
    return events


def detected(result, case):
    events = test_events(result.stdout)
    failed = any(e.get("Action") == "fail" and e.get("Test") == case["test"] for e in events)
    diagnostic = "".join(e.get("Output", "") for e in events if e.get("Test") == case["test"])
    return result.returncode != 0 and failed and case["diagnostic"] in diagnostic


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", default=str(Path(__file__).with_name("mutations.json")))
    parser.add_argument("--tags", default="")
    args = parser.parse_args()
    cases = json.loads(Path(args.cases).read_text())
    flags = ["-tags", args.tags] if args.tags else []
    for case in cases:
        source = (ROOT / case["file"]).resolve()
        source.relative_to(ROOT)
        original = source.read_text()
        if original.count(case["original"]) != 1:
            raise RuntimeError(f"mutation anchor is stale or ambiguous: {case['name']}")
        command = ["go", "test", *flags, "-json", "-count=1", "-timeout=2m",
                   "./internal/cli", "-run", "^" + re.escape(case["test"]) + "$"]
        baseline = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=180)
        if baseline.returncode or not any(e.get("Action") == "pass" and e.get("Test") == case["test"] for e in test_events(baseline.stdout)):
            raise RuntimeError(f"baseline did not pass: {case['name']}\n{baseline.stdout}\n{baseline.stderr}")
        with tempfile.TemporaryDirectory(prefix="brain-mutation-") as temporary:
            directory = Path(temporary)
            replacement = directory / source.name
            replacement.write_text(original.replace(case["original"], case["replacement"], 1))
            overlay = directory / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
            mutant_command = command[:2] + [f"-overlay={overlay}"] + command[2:]
            mutant = subprocess.run(mutant_command, cwd=ROOT, capture_output=True, text=True, timeout=180)
            if not detected(mutant, case):
                raise RuntimeError(f"mutation was not caught by the intended assertion: {case['name']}\n{mutant.stdout}\n{mutant.stderr}")
        print(f"PASS: {case['name']} — clean baseline passes; intended regression fails", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
