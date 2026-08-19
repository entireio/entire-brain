#!/usr/bin/env python3
"""Render a B session's pre-edit LOCATE calls into a blind human-labeling sheet.

validity/ (plan 0.8): the construct-validity check for the pre-registered
primary metric `locate_calls_pre_edit`. See LABEL_PROTOCOL.md for the full
protocol; this module is the "render" half -- turning one session's
`stream.jsonl` into a Markdown sheet a human can label offline, WITHOUT being
able to tell which arm produced it.

BLINDING (mechanical, not just a rater instruction -- see LABEL_PROTOCOL.md
"Blind to arm"):
    - the sheet is written under an opaque id, sha256(seed:pair_id:arm)[:12],
      never the arm name or the results-directory path;
    - `packet.txt` and the prompt's `--- MEMORY ---` section are NEVER read;
    - only the `--- ISSUE ---` text is extracted from `prompt.txt`;
    - `meta.json`'s `arm` field and `packet_provenance` are never rendered.
The id -> (pair_id, arm) mapping goes to a SEPARATE `UNBLIND-MAP.json` that a
labeler should not open before every rater has submitted labels for a batch.

The labeled UNIT is exactly `mechmetrics.session_metrics()`'s pre-edit LOCATE
slice -- `extract_tool_calls` is IMPORTED from mechmetrics, not reimplemented,
so the labeled calls and the scored calls can never silently drift apart.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import random
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
    from brainmark.mechmetrics import extract_tool_calls  # type: ignore[no-redef]
    from brainmark.mine_pairs import patch_files  # type: ignore[no-redef]
    from brainmark.session_a import load_problem_statement  # type: ignore[no-redef]
else:
    from .. import _harness
    from ..mechmetrics import extract_tool_calls
    from ..mine_pairs import patch_files
    from ..session_a import load_problem_statement

ISSUE_HEADER = "--- ISSUE ---"
MEMORY_HEADER = "--- MEMORY ---"
RESULT_SNIPPET_CHARS = 400


# --------------------------------------------------------------------------
# blinding primitives
# --------------------------------------------------------------------------


def opaque_session_id(seed: int, pair_id: str, arm: str) -> str:
    """One-way id. A labeling sheet never carries the arm name or a results
    path, so nothing in the sheet itself can leak which arm produced it."""
    return _harness.sha256_text(f"{seed}:{pair_id}:{arm}")[:12]


def extract_issue_text(prompt_text: str) -> str:
    """The `--- ISSUE ---` section only. Never reads `--- MEMORY ---` content
    -- that section differs by arm, and its presence/shape is itself a signal
    this function must not even look at."""
    start = prompt_text.find(ISSUE_HEADER)
    if start == -1:
        return ""
    start += len(ISSUE_HEADER)
    end = prompt_text.find(MEMORY_HEADER, start)
    body = prompt_text[start:end] if end != -1 else prompt_text[start:]
    return body.strip()


# --------------------------------------------------------------------------
# labeling unit: mechmetrics' pre-edit LOCATE slice, plus display metadata
# --------------------------------------------------------------------------


def _tool_input_index(stream_path: pathlib.Path) -> dict[str, dict]:
    """tool_use id -> raw input dict. mechmetrics.extract_tool_calls keeps
    only a Bash `command`, not Read/Grep/Glob targets, so this is a second,
    display-only pass over the same events -- the CLASSIFICATION (locate/edit/
    order) still comes solely from the imported mechmetrics function."""
    index: dict[str, dict] = {}
    if not stream_path.is_file():
        return index
    for line in stream_path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict) or event.get("type") != "assistant":
            continue
        message = event.get("message")
        content = message.get("content") if isinstance(message, dict) else None
        if not isinstance(content, list):
            continue
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("id"):
                index[block["id"]] = block.get("input") or {}
    return index


def _tool_result_text(content) -> str | None:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [b.get("text") for b in content if isinstance(b, dict) and b.get("type") == "text"]
        parts = [p for p in parts if isinstance(p, str)]
        return "\n".join(parts) if parts else None
    return None


def tool_result_index(stream_path: pathlib.Path) -> dict[str, str]:
    """tool_use id -> short result snippet, when the transcript carries
    `user`/`tool_result` echoes (real Claude Code transcripts do; this
    harness's own synthetic test fixtures often don't -- that degrades to {},
    which callers treat as 'no snippet available', never as an error)."""
    index: dict[str, str] = {}
    if not stream_path.is_file():
        return index
    for line in stream_path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict) or event.get("type") != "user":
            continue
        message = event.get("message")
        content = message.get("content") if isinstance(message, dict) else None
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_result":
                continue
            use_id = block.get("tool_use_id")
            text = _tool_result_text(block.get("content"))
            if use_id and text:
                index[use_id] = text[:RESULT_SNIPPET_CHARS]
    return index


def _render_target(name: str, tool_input: dict) -> str:
    """Short, arm-neutral description of what a call targeted."""
    if name == "Bash":
        return str(tool_input.get("command") or tool_input.get("cmd") or tool_input.get("script") or "")
    path = tool_input.get("file_path") or tool_input.get("path")
    pattern = tool_input.get("pattern")
    if pattern and path:
        return f"{pattern!r} in {path}"
    if pattern:
        return str(pattern)
    if path:
        return str(path)
    return json.dumps(tool_input, sort_keys=True)[:200]


def pre_edit_locate_calls(stream_path: pathlib.Path) -> list[dict]:
    """The labeling unit: exactly mechmetrics' pre-edit LOCATE slice, each
    call enriched (display-only) with its target and a result snippet."""
    calls, _result = extract_tool_calls(stream_path)
    inputs = _tool_input_index(stream_path)
    results = tool_result_index(stream_path)
    first_edit = next((i for i, c in enumerate(calls) if c["kind"] == "edit"), None)
    horizon = len(calls) if first_edit is None else first_edit

    out: list[dict] = []
    for index, call in enumerate(calls[:horizon]):
        if call["kind"] != "locate":
            continue
        call_id = call.get("id") or ""
        out.append({
            "call_index": index,
            "name": call["name"],
            "target": _render_target(call["name"], inputs.get(call_id, {})),
            "result_snippet": results.get(call_id, ""),
        })
    return out


# --------------------------------------------------------------------------
# session A reference panel (public task content -- not an arm signal: every
# arm's B session was measured against the SAME A)
# --------------------------------------------------------------------------


def a_reference_panel(a_dir: pathlib.Path, config: dict | None = None) -> dict:
    meta_path = a_dir / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.is_file() else {}
    patch_path = a_dir / "patch.diff"
    patch_text = patch_path.read_text(encoding="utf-8", errors="replace") if patch_path.is_file() else ""

    problem = meta.get("problem_statement")
    instance_id = meta.get("instance_id")
    if not problem and config is not None and instance_id:
        try:
            problem = load_problem_statement(config, instance_id)
        except (KeyError, OSError):
            problem = None

    return {
        "instance_id": instance_id or "",
        "problem_statement": (problem or "").strip(),
        "files_changed": sorted(patch_files(patch_text)) if patch_text else [],
        "patch_present": bool(patch_text),
    }


# --------------------------------------------------------------------------
# rendering
# --------------------------------------------------------------------------


def render_sheet(session_id: str, issue_text: str, a_ref: dict, calls: list[dict],
                 shared_files: list[str] | None = None) -> str:
    lines = [f"# Labeling sheet -- session `{session_id}`", ""]
    lines.append(
        "Blind to arm -- do not try to guess which memory source (if any) produced "
        "this session. Read `validity/LABEL_PROTOCOL.md` before labeling."
    )
    lines.append("")
    lines.append("## Session A reference (ground truth; safe to read)")
    lines.append("")
    lines.append(f"- A instance: `{a_ref.get('instance_id') or '?'}`")
    if a_ref.get("files_changed"):
        rendered = ", ".join(f"`{f}`" for f in a_ref["files_changed"])
        lines.append(f"- files A's session changed: {rendered}")
    else:
        lines.append("- files A's session changed: _(patch not available)_")
    if shared_files:
        lines.append(f"- pair-level shared files (from the miner): {', '.join(f'`{f}`' for f in shared_files)}")
    lines.append("")
    lines.append("**A's problem statement:**")
    lines.append("")
    lines.append("> " + (a_ref.get("problem_statement") or "_(not available)_").replace("\n", "\n> "))
    lines.append("")
    lines.append("## Session B's issue (identical text across every arm of this pair)")
    lines.append("")
    lines.append("> " + (issue_text or "_(not available)_").replace("\n", "\n> "))
    lines.append("")
    lines.append("## Pre-edit LOCATE calls to label")
    lines.append("")
    lines.append(
        "For each call below: does it re-derive knowledge session A's transcript "
        "already established (same file/function/decision A touched)? "
        "Answer `yes` / `no` / `unclear` -- see LABEL_PROTOCOL.md for the definitions."
    )
    lines.append("")
    if not calls:
        lines.append("_(no pre-edit locate calls in this session)_")
    for call in calls:
        lines.append(f"### call {call['call_index']} -- `{call['name']}`")
        lines.append("")
        lines.append(f"- target: `{call['target']}`")
        if call.get("result_snippet"):
            snippet = call["result_snippet"].replace("\n", "\n  ")
            lines.append(f"- result snippet:\n  ```\n  {snippet}\n  ```")
        else:
            lines.append("- result snippet: _(not available)_")
        lines.append("- **verdict:** `[ yes | no | unclear ]`  <!-- fill in -->")
        lines.append("- notes:")
        lines.append("")
    return "\n".join(lines) + "\n"


def render_session(pair: dict, a_dir: pathlib.Path, b_cell_dir: pathlib.Path,
                   session_id: str, config: dict | None = None) -> str:
    prompt_path = b_cell_dir / "prompt.txt"
    issue_text = extract_issue_text(
        prompt_path.read_text(encoding="utf-8")) if prompt_path.is_file() else ""
    a_ref = a_reference_panel(a_dir, config)
    calls = pre_edit_locate_calls(b_cell_dir / "stream.jsonl")
    shared_files = pair.get("shared_files") if isinstance(pair, dict) else None
    return render_sheet(session_id, issue_text, a_ref, calls, shared_files=shared_files)


# --------------------------------------------------------------------------
# stratified, deterministic sampling
# --------------------------------------------------------------------------


def discover_b_sessions(results_root: pathlib.Path, arms: list[str]
                        ) -> list[tuple[str, str, pathlib.Path]]:
    """-> [(pair_id, arm, cell_dir), ...] for every arm cell with a stream.jsonl."""
    out: list[tuple[str, str, pathlib.Path]] = []
    if not results_root.is_dir():
        return out
    for pair_dir in sorted(results_root.iterdir()):
        if not pair_dir.is_dir():
            continue
        for arm in arms:
            cell = pair_dir / arm
            if (cell / "stream.jsonl").is_file():
                out.append((pair_dir.name, arm, cell))
    return out


def stratified_sample(sessions: list[tuple[str, str, pathlib.Path]], n: int, seed: int
                      ) -> list[tuple[str, str, pathlib.Path]]:
    """Deterministic, arm-balanced, pair-diverse sample.

    Each arm's pool is sorted (removes filesystem-iteration-order nondeterminism)
    then shuffled with a seeded RNG. Draw proceeds in rounds, one pick per arm
    per round, preferring a pair_id not already chosen this batch; when every
    remaining item in an arm repeats an already-chosen pair, that arm just
    contributes its next item rather than stalling the whole draw. This is
    deterministic given (sessions, n, seed) -- reproducible and auditable, never
    redrawn after a bad-looking batch.
    """
    by_arm: dict[str, list[tuple[str, str, pathlib.Path]]] = {}
    for item in sessions:
        by_arm.setdefault(item[1], []).append(item)
    arms_order = sorted(by_arm)
    rng = random.Random(seed)
    for arm in arms_order:
        pool = sorted(by_arm[arm], key=lambda it: it[0])
        rng.shuffle(pool)
        by_arm[arm] = pool

    chosen: list[tuple[str, str, pathlib.Path]] = []
    seen_pairs: set[str] = set()
    while len(chosen) < n and any(by_arm[a] for a in arms_order):
        progressed = False
        for arm in arms_order:
            if len(chosen) >= n:
                break
            pool = by_arm[arm]
            if not pool:
                continue
            pick_at = next((i for i, item in enumerate(pool) if item[0] not in seen_pairs), 0)
            item = pool.pop(pick_at)
            chosen.append(item)
            seen_pairs.add(item[0])
            progressed = True
        if not progressed:
            break
    return chosen


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _find_pair_json(pairs_dirs: list[pathlib.Path], pair_id: str) -> pathlib.Path | None:
    for d in pairs_dirs:
        candidate = d / f"{pair_id}.json"
        if candidate.is_file():
            return candidate
    return None


def cmd_single(args) -> int:
    pair = json.loads(pathlib.Path(args.pair).read_text(encoding="utf-8"))
    config = _harness.load_config(args.config) if args.config else None
    session_id = args.session_id or opaque_session_id(args.seed, pair["pair_id"], "unknown")
    sheet = render_session(pair, pathlib.Path(args.a_dir), pathlib.Path(args.b_dir),
                           session_id, config)
    out = pathlib.Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(sheet, encoding="utf-8")
    print(f"wrote {out}")
    return 0


def cmd_batch(args) -> int:
    results_root = pathlib.Path(args.results)
    arms = [a.strip() for a in args.arms.split(",") if a.strip()]
    sessions = discover_b_sessions(results_root, arms)
    if not sessions:
        raise SystemExit(f"no B sessions with stream.jsonl found under {results_root} for arms {arms}")
    sample = stratified_sample(sessions, args.n, args.seed)

    pairs_dirs = [pathlib.Path(p) for p in args.pairs_dir]
    a_root = pathlib.Path(args.a_root)
    config = _harness.load_config(args.config) if args.config else None

    out_dir = pathlib.Path(args.out_dir)
    sheets_dir = out_dir / "sheets"
    sheets_dir.mkdir(parents=True, exist_ok=True)

    unblind_map: dict[str, dict] = {}
    skipped: list[str] = []
    for pair_id, arm, cell in sample:
        pair_path = _find_pair_json(pairs_dirs, pair_id)
        if pair_path is None:
            skipped.append(pair_id)
            continue
        pair = json.loads(pair_path.read_text(encoding="utf-8"))
        session_id = opaque_session_id(args.seed, pair_id, arm)
        sheet = render_session(pair, a_root / pair_id, cell, session_id, config)
        (sheets_dir / f"{session_id}.md").write_text(sheet, encoding="utf-8")
        unblind_map[session_id] = {"pair_id": pair_id, "arm": arm}

    unblind_path = out_dir / "UNBLIND-MAP.json"
    unblind_path.write_text(_harness.pretty_json({
        "_warning": "DO NOT open before every rater has submitted labels for this batch.",
        "seed": args.seed,
        "sessions": unblind_map,
    }), encoding="utf-8")
    print(f"wrote {len(unblind_map)} sheets to {sheets_dir}; unblind map at {unblind_path}")
    if skipped:
        print(f"WARNING: skipped {len(skipped)} sampled session(s) with no pair JSON: {skipped}",
              file=sys.stderr)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Render BrainMark B sessions into blind labeling sheets.")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_single = sub.add_parser("single", help="render one session")
    p_single.add_argument("--pair", required=True, help="path to the pair JSON")
    p_single.add_argument("--a-dir", required=True, help="session A's results directory for this pair")
    p_single.add_argument("--b-dir", required=True, help="the B arm cell directory (has stream.jsonl)")
    p_single.add_argument("--session-id", default=None, help="opaque id; auto-derived if omitted")
    p_single.add_argument("--seed", type=int, default=20260815)
    p_single.add_argument("--config", default=None)
    p_single.add_argument("--out", required=True)
    p_single.set_defaults(func=cmd_single)

    p_batch = sub.add_parser("batch", help="stratified sample -> a batch of blind sheets")
    p_batch.add_argument("--results", required=True, help="B results root (contains <pair_id>/<arm>/)")
    p_batch.add_argument("--a-root", required=True, help="A results root (contains <pair_id>/)")
    p_batch.add_argument("--pairs-dir", action="append", required=True,
                         help="directory of <pair_id>.json (repeatable, e.g. tasks/dev tasks/sealed)")
    p_batch.add_argument("--arms", default="no_brain,full_brain,mem0,graphify,cmm")
    p_batch.add_argument("--n", type=int, default=30)
    p_batch.add_argument("--seed", type=int, default=20260815)
    p_batch.add_argument("--config", default=None)
    p_batch.add_argument("--out-dir", required=True)
    p_batch.set_defaults(func=cmd_batch)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
