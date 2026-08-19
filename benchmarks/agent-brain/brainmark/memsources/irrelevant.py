"""`irrelevant` -- the PLACEBO CONTROL arm (plan 0.4). EXPLORATORY, not a brain ablation.

WHAT IT ANSWERS. `full_brain > no_brain` is compatible with two stories: memory
about THIS code helped, or any extra 24KB of plausible-looking coding-session
context helped (a longer prompt, a nudge toward being systematic, a change in
how the model budgets its exploration). Only a packet that is real, same-shaped,
same-sized and about the WRONG code separates them. This arm is that packet.

WHAT THE PACKET IS. Another sealed pair's session-A-derived packet -- the exact
bytes some other pair's headline arm was given -- re-addressed to this pair:

  * SAME producer: it was built by the same memory source over a real session A,
    so it is not synthetic filler and not a degraded version of the real thing;
  * SAME envelope: re-serialized through base.build_packet, so it goes through
    the identical bounder, delimiter guard, tags and prompt scaffold;
  * SAME query field: the envelope's `query` is THIS pair's B problem statement,
    because the query is part of the scaffold, not part of the memory. Leaving
    the donor's query in would hand the agent a visible "this packet is about
    something else" tell that no other arm carries;
  * SAME byte budget, and optionally size-MATCHED to this pair's treatment
    packet (`target_bytes`), so packet length cannot be the explanation.

WHY A DERANGEMENT AND NOT A RANDOM DRAW. The donor assignment must be
(1) deterministic -- re-derivable at report time from the sealed list alone,
(2) fixed-point-free -- a pair must never receive its OWN packet, which would
silently turn the placebo arm into a second treatment arm, and
(3) same-repo-free -- a donor from the same repository is not irrelevant; it is
weak but real memory, which would bias the placebo TOWARD the treatment and make
a null look like a win.

THE CONSTRUCTION (no RNG, no retry loop, no seed to leak):
group pairs by repo, order groups by (-size, repo) and pairs within a group by
pair_id, concatenate into L, and send L[i]'s packet to L[(i+m) % n] where m is
the LARGEST group's size. Shifting by m always clears your own group, because
every group is at most m long: forward it lands past the group's end, and on
wraparound it lands before its start (needs len+m <= n, which holds whenever
m <= n/2). If the largest repo holds more than half the pairs no such assignment
exists at all, and this module raises rather than quietly relaxing the rule.
"""

from __future__ import annotations

import json
from typing import Any, Iterable, Sequence

from .base import MemoryPacket, MemorySourceError, Stopwatch, build_packet, normalize_results

ARM = "irrelevant"

#: Which arm's packet is borrowed. The placebo must be a placebo FOR the
#: headline arm, so it borrows what full_brain produced for the donor pair.
DONOR_ARM = "full_brain"


# --------------------------------------------------------------------------
# the derangement
# --------------------------------------------------------------------------


def _normalize_pairs(pairs: Iterable[Any]) -> list[tuple[str, str]]:
    """-> [(pair_id, repo)], from dicts or (id, repo) tuples. Deterministic."""
    out: list[tuple[str, str]] = []
    seen: set[str] = set()
    for item in pairs:
        if isinstance(item, dict):
            pair_id = str(item.get("pair_id") or "")
            repo = str(item.get("repo") or "")
        elif isinstance(item, (tuple, list)) and len(item) >= 2:
            pair_id, repo = str(item[0]), str(item[1])
        else:
            raise MemorySourceError(f"cannot read a pair_id/repo from {item!r}")
        if not pair_id:
            raise MemorySourceError(f"pair without a pair_id: {item!r}")
        if pair_id in seen:
            raise MemorySourceError(f"duplicate pair_id in the sealed list: {pair_id}")
        seen.add(pair_id)
        out.append((pair_id, repo))
    return out


def donor_order(pairs: Sequence[Any]) -> tuple[list[str], dict[str, str], int]:
    """(ordered pair_ids, pair_id -> repo, shift m). Pure function of the input set."""
    normalized = _normalize_pairs(pairs)
    repo_of = {pair_id: repo for pair_id, repo in normalized}
    groups: dict[str, list[str]] = {}
    for pair_id, repo in normalized:
        groups.setdefault(repo, []).append(pair_id)
    ordered_groups = sorted(groups.items(), key=lambda kv: (-len(kv[1]), kv[0]))
    order: list[str] = []
    for _repo, members in ordered_groups:
        order.extend(sorted(members))
    shift = max((len(members) for _repo, members in ordered_groups), default=0)
    return order, repo_of, shift


def derangement(pairs: Sequence[Any]) -> dict[str, str]:
    """recipient pair_id -> DONOR pair_id. No fixed points, no same-repo donors.

    MUTATION TARGET (b): weaken either guarantee -- shift 0, a same-repo donor,
    a non-deterministic order -- and tests/test_irrelevant_placebo.py fails.
    """
    order, repo_of, shift = donor_order(pairs)
    n = len(order)
    if n < 2:
        raise MemorySourceError(
            f"the placebo arm needs at least 2 sealed pairs to deal from, got {n}"
        )
    if shift * 2 > n:
        biggest = max(set(repo_of.values()), key=lambda r: (
            sum(1 for v in repo_of.values() if v == r), r))
        raise MemorySourceError(
            f"no same-repo-free derangement exists: repo {biggest!r} holds "
            f"{shift}/{n} pairs (> half). Seal more repos or drop the placebo arm; "
            "do NOT relax the same-repo rule -- a same-repo donor is weak real "
            "memory, not a placebo."
        )
    mapping: dict[str, str] = {}
    for index, recipient in enumerate(order):
        donor = order[(index + shift) % n]
        if donor == recipient:  # unreachable given shift in [1, n/2]; assert loudly
            raise MemorySourceError(f"derangement produced a fixed point at {recipient}")
        if repo_of[donor] == repo_of[recipient]:
            raise MemorySourceError(
                f"derangement produced a same-repo donor for {recipient}: {donor}"
            )
        mapping[recipient] = donor
    return mapping


def donor_for(pair_id: str, pairs: Sequence[Any]) -> str:
    mapping = derangement(pairs)
    if pair_id not in mapping:
        raise MemorySourceError(
            f"{pair_id} is not in the sealed pair list the derangement was built from"
        )
    return mapping[pair_id]


# --------------------------------------------------------------------------
# the packet
# --------------------------------------------------------------------------


def donor_results(donor_packet_text: str, top_k: int) -> list[dict]:
    """Lift the `results` array out of a donor packet, dropping its envelope."""
    try:
        payload = json.loads(donor_packet_text)
    except json.JSONDecodeError as exc:
        raise MemorySourceError(
            f"arm {ARM}: donor packet is not valid JSON ({exc}); a placebo built "
            "from a truncated donor is a different-sized packet, not a placebo"
        ) from exc
    if not isinstance(payload, dict) or not isinstance(payload.get("results"), list):
        raise MemorySourceError(f"arm {ARM}: donor packet has no `results` array")
    return normalize_results(payload["results"], top_k)


def build(
    query: str,
    max_bytes: int,
    top_k: int,
    donor_packet_text: str,
    donor_pair_id: str,
    recipient_pair_id: str | None = None,
    donor_arm: str = DONOR_ARM,
    target_bytes: int | None = None,
    **_ignored: Any,
) -> MemoryPacket:
    """The placebo packet: donor RESULTS, this pair's QUERY, one shared envelope.

    `target_bytes` size-matches the placebo to this pair's treatment packet. It
    can only ever TIGHTEN the budget (never exceed max_bytes), and the bounder
    drops the donor's worst-ranked results first, so a size-matched placebo is
    still the donor's best content.
    """
    if recipient_pair_id is not None and donor_pair_id == recipient_pair_id:
        raise MemorySourceError(
            f"arm {ARM}: donor == recipient ({donor_pair_id}); a pair may never "
            "receive its own session-A packet -- that is the treatment, not a placebo"
        )
    if not (donor_packet_text or "").strip():
        raise MemorySourceError(
            f"arm {ARM}: donor packet for {donor_pair_id} is empty; run the donor's "
            f"{donor_arm} packet first rather than delivering an accidental sentinel"
        )

    with Stopwatch() as watch:
        results = donor_results(donor_packet_text, top_k)
    if not results:
        raise MemorySourceError(
            f"arm {ARM}: donor {donor_pair_id} contributed no results; an empty "
            "placebo is indistinguishable from no_brain and must not be measured"
        )

    budget = max_bytes if target_bytes is None else max(1, min(int(max_bytes), int(target_bytes)))
    return build_packet(
        arm=ARM,
        query=query,
        results=results,
        max_bytes=budget,
        prep={
            "seconds": watch.elapsed_s,
            "llm_calls": 0,
            "usd": 0.0,
            "donor_pair_id": donor_pair_id,
            "donor_arm": donor_arm,
            "donor_result_count": len(results),
            "size_matched_to_bytes": target_bytes,
            "effective_max_bytes": budget,
            "control_type": "placebo_exploratory_not_a_brain_ablation",
        },
    )
