#!/usr/bin/env python3
"""Translate a codex event stream into the canonical agent JSONL every arm reads.

WHY THIS EXISTS -- BOTH MEMORY ARMS ARE OTHERWISE DEAD, SILENTLY
----------------------------------------------------------------
Session A is the one session all arms share, and its transcript is the ONLY
input any memory source is built from. On the codex backend that transcript is
codex's own ThreadEvent JSONL (`thread.started` / `turn.started` / `item.*` /
`turn.completed`), and NEITHER consumer understands it:

  * entire-brain's distill preprocessor switches on the top-level `"type"` and
    recognizes only agent_message / event_msg / response_item / assistant /
    user / message (`internal/cli/distill_cmd.go:1502-1543`). Every codex line
    falls to `default: jsonString(obj["message"])` -- a key the codex schema
    does not have -- so every line blanks to `""`. A distill over a raw codex
    stream produces ZERO facts and does NOT error.
  * BrainMark's own competitor ingest (`memsources/_competitor.py
    transcript_to_messages`) keeps lines whose top-level `"type"` is `"user"` or
    `"assistant"` AND that carry a `"message"` dict. A codex stream yields zero
    messages, and the mem0 arm raises "session-A transcript produced no
    messages".

So one arm would have scored 0 for an infrastructure reason and the other would
have crashed -- and the first of those is indistinguishable, in a results table,
from "memory did not help". That is the easiest way to fake this benchmark's
headline, so it is fixed at the source rather than per arm.

WHY THIS IS FAIR
----------------
The translation is an INPUT NORMALIZATION applied ONCE, before any arm sees
anything, and the SAME translated bytes are handed to every arm. It is symmetric
by construction: there is no arm-specific branch in this file, and no arm can
receive different bytes than another. The raw codex stream is kept beside the
canonical one and both are sha256-pinned, so the translation is auditable and
reversible -- a reviewer can re-run it and compare.

WHAT IS ALSO SANITIZED HERE
---------------------------
Absolute harness paths. codex reports `file_change` paths and shell output
containing session A's worktree path, which sits inside the harness tree.
Leaving them in would write harness locations into every memory packet -- and,
worse, would tell a downstream session something about the layout it is being
measured in. The worktree prefix is rewritten to `.` before translation.

Usage (post-hoc, idempotent, over already-harvested A dirs):

    python3 -m brainmark.transcript_canon <session-A dir> [<session-A dir> ...]
"""

from __future__ import annotations

import json
import pathlib
import sys
from typing import Any

if __package__ in (None, ""):  # pragma: no cover
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
    from brainmark.mechmetrics import effective_shell_command  # type: ignore[no-redef]
else:
    from . import _harness
    from .mechmetrics import effective_shell_command

#: Caps on what one translated line may carry. Tool RESULTS are capped harder
#: than prose: a 200KB `cat` dump is not session learning, and letting it
#: through would let one noisy command dominate what every arm ingests.
MAX_RESULT = 4000
MAX_TEXT = 20000

CANONICAL_NAME = "session_transcript.canonical.jsonl"
RAW_CODEX_NAME = "session_transcript.codex.jsonl"

TRANSLATED_SOURCE = "codex_event_stream_translated_to_agent_jsonl"

#: codex item kind -> how it is rendered. Kept beside mechmetrics' vocabulary
#: rather than imported from it, because these two answer different questions:
#: mechmetrics CLASSIFIES a call, this RE-EXPRESSES it.
_SHELL_ITEMS = ("command_execution", "local_shell_call", "shell_call")
_EDIT_ITEMS = ("file_change", "patch_apply", "apply_patch")


def scrub(text: Any, worktree: pathlib.Path | str | None) -> Any:
    """Rewrite the session-A worktree prefix to `.`; leave non-strings alone."""
    if not isinstance(text, str) or not worktree:
        return text
    wt = str(worktree)
    return text.replace(wt + "/", "").replace(wt, ".")


def _assistant(blocks: list[dict]) -> dict:
    return {"type": "assistant", "message": {"role": "assistant", "content": blocks}}


def _user(blocks: list[dict]) -> dict:
    return {"type": "user", "message": {"role": "user", "content": blocks}}


def translate(events: list[dict], worktree: pathlib.Path | str | None = None,
              first_user_text: str | None = None) -> tuple[list[dict], dict]:
    """codex ThreadEvents -> claude stream-JSONL objects, plus item statistics.

    Only `item.completed` is translated. `item.started` carries the same id and
    a partial payload, so translating both would DOUBLE every tool call in the
    memory corpus; the id-dedup below is what prevents it.
    """
    out: list[dict] = []
    if first_user_text:
        out.append(_user([{"type": "text",
                           "text": scrub(first_user_text, worktree)[:MAX_TEXT]}]))

    stats = {"agent_message": 0, "reasoning": 0, "command_execution": 0,
             "file_change": 0, "other_item": 0, "skipped": 0}
    seen: set = set()

    for event in events:
        if event.get("type") != "item.completed":
            continue
        item = event.get("item") or {}
        if not isinstance(item, dict):
            continue
        item_id = item.get("id")
        if item_id is not None:
            if item_id in seen:
                continue
            seen.add(item_id)
        # Both codex schemas: 0.148 renamed item_type -> type (see mechmetrics).
        kind = item.get("item_type") or item.get("type")

        if kind == "agent_message":
            text = scrub(item.get("text") or "", worktree)
            if text.strip():
                out.append(_assistant([{"type": "text", "text": text[:MAX_TEXT]}]))
                stats["agent_message"] += 1

        elif kind == "reasoning":
            text = item.get("text") or item.get("summary") or ""
            if isinstance(text, list):
                text = "\n".join(str(x) for x in text)
            text = scrub(text, worktree)
            if text.strip():
                # Tagged, not dropped: reasoning is where a session states the
                # conclusion a later session would reuse. Tagging keeps it
                # distinguishable from what the agent actually said.
                out.append(_assistant([{"type": "text",
                                        "text": ("[reasoning] " + text)[:MAX_TEXT]}]))
                stats["reasoning"] += 1

        elif kind in _SHELL_ITEMS:
            raw = item.get("command") if item.get("command") is not None else item.get("action")
            command = scrub(effective_shell_command(raw) or "", worktree)
            out.append(_assistant([{"type": "tool_use", "id": str(item_id),
                                    "name": "Bash", "input": {"command": command}}]))
            output = scrub(str(item.get("aggregated_output") or item.get("output") or ""),
                           worktree)
            out.append(_user([{"type": "tool_result", "tool_use_id": str(item_id),
                               "content": output[:MAX_RESULT]}]))
            stats["command_execution"] += 1

        elif kind in _EDIT_ITEMS:
            changes = [
                {"path": scrub(str(ch.get("path") or ""), worktree), "kind": ch.get("kind")}
                for ch in (item.get("changes") or []) if isinstance(ch, dict)
            ]
            out.append(_assistant([{"type": "tool_use", "id": str(item_id),
                                    "name": "Edit", "input": {"changes": changes}}]))
            stats["file_change"] += 1

        elif kind in ("mcp_tool_call", "web_search"):
            out.append(_assistant([{
                "type": "tool_use", "id": str(item_id),
                "name": "WebSearch" if kind == "web_search" else "MCP",
                "input": {k: v for k, v in item.items()
                          if k not in ("id", "type", "item_type")},
            }]))
            stats["other_item"] += 1

        else:
            stats["skipped"] += 1

    return out, stats


def render(lines: list[dict]) -> str:
    return "".join(json.dumps(obj, ensure_ascii=False) + "\n" for obj in lines)


def parse_stream(raw_bytes: bytes) -> list[dict]:
    """Lenient NDJSON: a truncated tail must not lose the whole session."""
    events: list[dict] = []
    for line in raw_bytes.decode("utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict):
            events.append(obj)
    return events


def canonicalize(out_dir: pathlib.Path, raw_bytes: bytes,
                 canonical_path: pathlib.Path,
                 worktree: pathlib.Path | str | None = None,
                 a_prompt: str | None = None) -> dict[str, Any]:
    """Write the raw stream + its canonical translation and return provenance.

    Refuses to pin an EMPTY translation. A codex stream that yields no lines
    means the schema moved again, and pinning zero bytes would hand every arm
    an empty corpus -- the exact silent failure this module exists to stop.
    """
    out_dir = pathlib.Path(out_dir)
    events = parse_stream(raw_bytes)
    lines, stats = translate(events, worktree, a_prompt)
    if not lines:
        raise RuntimeError(
            f"codex transcript in {out_dir} translated to ZERO lines "
            f"({len(events)} events read); refusing to pin an empty transcript, "
            "which would silently give every memory arm nothing to learn from."
        )

    kept = out_dir / RAW_CODEX_NAME
    kept.write_bytes(raw_bytes)
    body = render(lines)
    canonical_path = pathlib.Path(canonical_path)
    canonical_path.write_text(body, encoding="utf-8")

    return {
        "translator": "brainmark.transcript_canon",
        "translator_sha256": _harness.sha256_file(pathlib.Path(__file__)),
        "raw_codex_path": str(kept),
        "raw_codex_sha256": _harness.sha256_bytes(raw_bytes),
        "raw_codex_bytes": len(raw_bytes),
        "canonical_path": str(canonical_path),
        "canonical_sha256": _harness.sha256_text(body),
        "canonical_bytes": len(body.encode("utf-8")),
        "events_in": len(events),
        "lines_out": len(lines),
        "item_stats": stats,
        "a_prompt_prepended": bool(a_prompt),
        "why": ("codex ThreadEvent JSONL is parsed by NEITHER entire-brain distill "
                "(internal/cli/distill_cmd.go:1502-1543) NOR brainmark's competitor "
                "ingest (memsources/_competitor.py); both would silently produce an "
                "EMPTY memory. Translated ONCE, byte-identical to every arm."),
    }


def canonicalize_dir(a_dir: pathlib.Path) -> dict[str, Any]:
    """Post-hoc, idempotent canonicalization of an already-harvested A dir.

    Verifies the raw transcript against its recorded pin BEFORE translating --
    translating a transcript that does not match its pin would launder a
    tampered or truncated file into the pinned corpus -- then re-pins meta.json
    onto the canonical bytes.
    """
    a_dir = pathlib.Path(a_dir)
    meta_path = a_dir / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    if meta.get("transcript_canonicalized"):
        return {"pair_id": meta.get("pair_id"), "skipped": "already canonicalized"}

    raw_path = pathlib.Path(meta["transcript_path"])
    raw_bytes = raw_path.read_bytes()
    if _harness.sha256_bytes(raw_bytes) != meta.get("transcript_sha256"):
        raise RuntimeError(
            f"{a_dir}: raw transcript pin mismatch; refusing to translate")

    provenance = canonicalize(
        a_dir, raw_bytes, a_dir / CANONICAL_NAME,
        worktree=a_dir / "worktree", a_prompt=meta.get("a_prompt"),
    )
    meta["transcript_canonicalized"] = provenance
    meta["transcript_path"] = provenance["canonical_path"]
    meta["transcript_sha256"] = provenance["canonical_sha256"]
    meta["transcript_source"] = TRANSLATED_SOURCE
    meta_path.write_text(_harness.pretty_json(meta), encoding="utf-8")
    return {"pair_id": meta.get("pair_id"), **provenance}


def main(argv: list[str] | None = None) -> int:
    for target in (argv if argv is not None else sys.argv[1:]):
        print(json.dumps(canonicalize_dir(pathlib.Path(target)), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
