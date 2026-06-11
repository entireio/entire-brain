#!/usr/bin/env python3
"""Deterministic local distill agent for retained scheduler evidence.

This intentionally does not judge semantic quality. It exercises the distill
extraction scheduler, reconcile path, and deterministic writes without network
or hosted-model dependency, so retained artifacts can prove local parallel
extraction mechanics separately from real-model fact quality.
"""

from __future__ import annotations

import hashlib
import sys
import time


PATHS = [
    "preferences.coding.style",
    "preferences.workflow.general",
    "architecture.boundaries.rationale",
    "architecture.data.flow",
    "project.tooling.stack",
    "project.ci_cd.pipeline",
    "workflow.branching.rules",
    "workflow.testing.rules",
    "constraints.invariants.general",
]


def candidate_count(reconcile_input: str) -> int:
    count = 0
    in_candidates = False
    for line in reconcile_input.splitlines():
        if line == "CANDIDATES":
            in_candidates = True
            continue
        if line == "EXISTING":
            break
        if in_candidates and line.strip():
            fields = line.split(maxsplit=1)
            if fields and fields[0].isdigit():
                count += 1
    return count


def main() -> int:
    text = sys.stdin.read()
    if text.startswith("CANDIDATES\n"):
        for index in range(1, candidate_count(text) + 1):
            print(f"{index} new - 1.0")
        return 0

    time.sleep(0.04)
    digest = hashlib.sha256(text.encode("utf-8", "ignore")).hexdigest()
    path = PATHS[int(digest[:8], 16) % len(PATHS)]
    print(
        f"{path}\tLocal distill evidence chunk {digest[:16]} "
        "is retained for current-repo performance measurement."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
