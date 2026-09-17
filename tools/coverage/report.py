#!/usr/bin/env python3
"""Summarize Go statement profiles without double-counting cross-package blocks."""
import argparse
import json
from pathlib import Path


def read_profiles(paths):
    blocks = {}
    for path in paths:
        lines = Path(path).read_text().splitlines()
        if not lines or lines[0] not in ("mode: set", "mode: count", "mode: atomic"):
            raise ValueError(f"invalid coverage header: {path}")
        for line in lines[1:]:
            location, statements, count = line.rsplit(maxsplit=2)
            statements, count = int(statements), int(count)
            if statements < 0 or count < 0:
                raise ValueError(f"negative coverage count: {line}")
            previous = blocks.get(location)
            if previous and previous[0] != statements:
                raise ValueError(f"incompatible statement counts: {location}")
            blocks[location] = (statements, bool(count) or bool(previous and previous[1]))
    if not blocks:
        raise ValueError("empty coverage profile")
    return blocks


def summarize(blocks):
    files = {}
    for location, (statements, hit) in blocks.items():
        # rsplit also handles drive letters in absolute Windows paths.
        name = location.rsplit(":", 1)[0]
        row = files.setdefault(name, {"statements": 0, "covered": 0})
        row["statements"] += statements
        row["covered"] += statements if hit else 0
    totals = {key: sum(row[key] for row in files.values()) for key in ("statements", "covered")}
    for row in [totals, *files.values()]:
        row["percent"] = 100 * row["covered"] / row["statements"] if row["statements"] else 100.0
    return {"metric": "Go statements", "totals": totals, "files": dict(sorted(files.items()))}


def compare(current, baseline):
    """Compare only equivalent configurations; do not compare macOS with Linux."""
    if any(report["environment"].get("tests_exit_code") != 0 for report in (current, baseline)):
        raise ValueError("coverage comparison requires two passing test runs")
    for key in ("go_version", "platform", "tags", "race", "scope"):
        if current["environment"].get(key) != baseline["environment"].get(key):
            raise ValueError(f"baseline configuration differs: {key}")
    return current["totals"]["percent"] - baseline["totals"]["percent"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("profiles", nargs="+")
    parser.add_argument("--metadata", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--merged-profile", required=True)
    parser.add_argument("--baseline", help="optional same-platform/build JSON report; fail on a coverage decrease")
    args = parser.parse_args()
    blocks = read_profiles(args.profiles)
    report = summarize(blocks)
    report["environment"] = json.loads(Path(args.metadata).read_text())
    if args.baseline:
        report["delta_percentage_points"] = compare(report, json.loads(Path(args.baseline).read_text()))
    Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    Path(args.merged_profile).write_text("mode: set\n" + "".join(
        f"{location} {statements} {int(hit)}\n"
        for location, (statements, hit) in sorted(blocks.items())
    ))
    total = report["totals"]
    print(f"Statement coverage: {total['percent']:.2f}% ({total['covered']}/{total['statements']})")
    return 1 if report.get("delta_percentage_points", 0) < -0.01 else 0


if __name__ == "__main__":
    raise SystemExit(main())
