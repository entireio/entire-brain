#!/usr/bin/env python3
"""Run the full Go suite and instrumented binary contracts, retaining failures."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]


def capture(*args):
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def binary_contracts(binary, output, cgo, counters):
    with tempfile.TemporaryDirectory(prefix="brain-binary-contract-") as temporary:
        directory = Path(temporary)
        env = {k: v for k, v in os.environ.items() if not k.startswith(("ENTIRE_", "GIT_", "XDG_"))}
        env.update({key: str(directory / key) for key in (
            "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
            "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"
        )})
        env["GOCOVERDIR"] = str(counters)
        cases = [("help", ["--help"], 0), ("version", ["version"], 0),
                 ("capabilities", ["capabilities", "--json"], 0),
                 ("invalid-command", ["not-a-brain-command"], 1)]
        for name, args, expected in cases:
            result = subprocess.run([str(binary), *args], cwd=directory, env=env,
                                    capture_output=True, text=True, timeout=30)
            (output / f"binary-{name}.json").write_text(json.dumps({
                "args": args, "exit_code": result.returncode,
                "stdout": result.stdout, "stderr": result.stderr,
            }, indent=2))
            if result.returncode != expected:
                raise RuntimeError(f"{name}: exit {result.returncode}, expected {expected}: {result.stderr}")
            if name == "help" and "Usage:" not in result.stdout:
                raise RuntimeError("missing CLI usage")
            if name == "version" and result.stdout.strip() != "dev":
                raise RuntimeError("unexpected unstamped version")
            if name == "invalid-command" and "unknown command" not in result.stderr:
                raise RuntimeError("missing invalid-command diagnostic")
            if name == "capabilities":
                capabilities = json.loads(result.stdout)
                if capabilities["schema_version"] != 1 or capabilities["build"]["brain_cgo"] != cgo:
                    raise RuntimeError("binary capabilities disagree with build")
        # These metadata commands must not initialize repository or user state.
        if list(directory.iterdir()):
            raise RuntimeError("read-only binary contracts created persistent state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    parser.add_argument("--tags", default="")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--baseline")
    parser.add_argument("--binary-only", action="store_true",
                        help="Measure CLI contracts without rerunning the test suite")
    parser.add_argument("--test-timeout", default="20m",
                        help="Go test deadline per package (default: 20m)")
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty to prevent stale coverage")
    flags = (["-race"] if args.race else []) + (["-tags", args.tags] if args.tags else [])
    metadata = {
        "commit": capture("git", "rev-parse", "HEAD"),
        "worktree_status": capture("git", "status", "--porcelain"),
        "go_version": capture("go", "version"),
        "platform": f"{platform.system()}/{platform.machine()}",
        "tags": args.tags, "race": args.race, "test_timeout": args.test_timeout,
        "scope": "instrumented binary contracts" if args.binary_only else "./... plus instrumented binary contracts",
    }
    metadata_path = output / "environment.json"
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
    profiles = []
    test_exit_code = 0
    if not args.binary_only:
        command = ["go", "test", *flags, "-count=1", "-timeout", args.test_timeout, "-covermode=atomic",
                   "-coverpkg=./...", f"-coverprofile={output / 'tests.out'}", "-json", "./..."]
        print("Running full Go suite with cross-package coverage", flush=True)
        with (output / "tests.jsonl").open("w", encoding="utf-8") as log:
            result = subprocess.run(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
        test_exit_code = result.returncode
        metadata["tests_exit_code"] = test_exit_code
        metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
        events = []
        for line in (output / "tests.jsonl").read_text(encoding="utf-8").splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get("Action") in ("pass", "skip", "fail"):
                events.append({key: event[key] for key in ("Action", "Package", "Test", "Elapsed") if key in event})
        (output / "test-results.json").write_text(json.dumps(events, indent=2) + "\n")
        for event in events:
            if event["Action"] == "fail":
                print("FAIL:", event.get("Package"), event.get("Test", ""), flush=True)
        profiles.append(str(output / "tests.out"))
    # Keep executables/counters out of uploaded shard artifacts. Only the
    # validated profile and contract results are needed by the merger.
    with tempfile.TemporaryDirectory(prefix="brain-instrumented-cli-") as temporary:
        build_dir = Path(temporary)
        binary = build_dir / ("entire-brain.exe" if os.name == "nt" else "entire-brain")
        subprocess.run(["go", "build", *flags, "-cover", "-covermode=atomic", "-coverpkg=./...",
                        "-o", str(binary), "./cmd/entire-brain"], cwd=ROOT, check=True)
        counters = build_dir / "counters"
        counters.mkdir()
        binary_contracts(binary, output, "brain_cgo" in args.tags.split(), counters)
        subprocess.run(["go", "tool", "covdata", "textfmt", "-i=" + str(counters),
                        "-o=" + str(output / "binary.out")], cwd=ROOT, check=True)
    metadata["tests_exit_code"] = test_exit_code
    metadata["profileSha256"] = hashlib.sha256((output / "binary.out").read_bytes()).hexdigest()
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
    profiles.append(str(output / "binary.out"))
    report_command = [sys.executable, "-B", str(Path(__file__).with_name("report.py")),
                      *profiles,
                      "--metadata", str(metadata_path), "--output", str(output / "summary.json"),
                      "--merged-profile", str(output / "combined.out")]
    if args.baseline:
        report_command += ["--baseline", args.baseline]
    report_result = subprocess.run(report_command, cwd=ROOT)
    if (output / "combined.out").exists():
        subprocess.run(["go", "tool", "cover", "-html=" + str(output / "combined.out"),
                        "-o", str(output / "coverage.html")], cwd=ROOT, check=True)
    return test_exit_code or report_result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
