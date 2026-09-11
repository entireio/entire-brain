#!/usr/bin/env python3
"""Deterministic local distill agent for the large-repo scheduler evidence.

Like the current-repo ``local-command-agent.py`` fixture, this intentionally
does not judge semantic quality. It exercises the distill extraction scheduler,
reconcile path, and deterministic writes without any network or hosted-model
dependency, so retained large-repo artifacts can prove local parallel
extraction mechanics separately from real-model fact quality.

The only behavioral difference from the current-repo fixture is path
diversity: a large session corpus produces hundreds of chunks, so a fixed
9-path taxonomy would pile far more than 50 facts onto each path and trip the
reconcile "more than 50 existing facts at these paths" scaling notice. A real
hosted-model distill spreads facts across many distinct loci, so this fixture
derives a deterministic per-chunk locus (a realistic top-level prefix plus a
digest-keyed leaf) to keep facts-per-path low and the run warning-free, while
remaining fully deterministic so serial and parallel runs produce identical
output.
"""

from __future__ import annotations

import hashlib
import sys
import time


# Two-level (category.subcategory) prefixes drawn from the default distill
# taxonomy. The agent appends a deterministic digest-keyed leaf to form a
# syntactically valid three-level taxonomy path (category.subcategory.type),
# so a large corpus spreads facts across many distinct paths and never trips
# the reconcile ">50 existing facts at these paths" scaling notice.
PREFIXES = [
    "preferences.coding",
    "preferences.workflow",
    "architecture.boundaries",
    "architecture.data",
    "project.tooling",
    "project.ci_cd",
    "workflow.branching",
    "workflow.testing",
    "constraints.invariants",
]

# A leaf vocabulary sized so a large corpus clusters onto ~144 distinct
# three-level paths (9 prefixes x these 16 leaves). That keeps enough facts per
# path to exercise the per-chunk reconcile path while staying well under the
# 50-per-path reconcile scaling notice.
#
# Facts are stored per branch, so what matters is the busiest branch, not the
# corpus total: four leaves left the entireio/cli `main` branch over 50 facts on
# some paths and the runs came back with reconcile scaling warnings, which the
# auditor rejects. Sixteen leaves divides the byte the index is drawn from
# exactly, so the spread stays uniform and deterministic, and leaves headroom
# for a corpus that keeps growing.
LEAVES = [
    "primary",
    "secondary",
    "tertiary",
    "general",
    "quaternary",
    "auxiliary",
    "baseline",
    "extended",
    "supplemental",
    "adjacent",
    "derived",
    "residual",
    "peripheral",
    "ancillary",
    "supporting",
    "collateral",
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
    prefix = PREFIXES[int(digest[8:10], 16) % len(PREFIXES)]
    leaf = LEAVES[int(digest[2:4], 16) % len(LEAVES)]
    path = f"{prefix}.{leaf}"
    print(
        f"{path}\tLocal distill evidence chunk {digest[:16]} "
        "is retained for large-repo performance measurement."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
