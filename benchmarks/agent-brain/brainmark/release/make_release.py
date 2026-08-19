#!/usr/bin/env python3
"""Build an anonymized, publication-safe copy of the brainmark tree (plan 0.10).

Two independent pieces, deliberately kept separate:

  1. **The substitution pass** (`anonymize_text` / `build_release`) rewrites
     identifying strings to neutral equivalents: `full_brain`/`no_brain` (the
     product's own arms) -> `system_x`/`baseline`; `entire-brain`/`entirehq`/
     `entireio`/`entire.io` -> generic org/product placeholders; personal
     names -> a generic author placeholder; absolute `/Users/<name>/...`
     paths -> portable placeholders. Competitor names (`mem0`, `graphify`,
     `cmm`) are left as-is -- they are third-party products, not Entire's IP,
     and the paper's own residual-risk list (plan PART 2) already accepts
     "5 named competitors + system_x" as a de-anonymization limitation, not
     something this pass can or should fix.

  2. **The grep-gate** (`scan_forbidden` / `scan_tree`) is an INDEPENDENT
     check over a directory tree for a fixed forbidden-string vocabulary,
     case-insensitive substring match, with an explicit allowlist for words
     that legitimately contain a banned substring (`brainmark` contains
     `brain`; `entirely`/`entirety` contain `entire`). It does not trust the
     substitution pass -- the whole point of a grep-gate, same "don't trust
     the tool, verify the output" doctrine as the parent directory's
     `test_publication_safety.py` (`../PUBLICATION-SANITIZATION.md`), applied
     here to a mechanically-generated release instead of a hand-edited one.

The gate is deliberately strict enough to flag this repo's own un-anonymized
working tree heavily -- that is it doing its job, not a bug; see
`tests/test_release_sanitization.py`'s `CurrentTreeGateTest` for the count,
and `make_release.py check --root .` to reproduce it locally.

What is copied: `.py`/`.md`/`.json`/`.txt` files are read, anonymized, and
written; everything else in the include set is copied byte-for-byte. Default
excludes: `results/`, `candidates/`, `REVIEW.json`, `sheets/`,
`UNBLIND-MAP.json`, `__pycache__/`, `.git/` -- raw paid-run artifacts,
unreviewed candidates, and rater-identifying material are not release
material by default; only sealed tasks, code, and documentation are.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import shutil
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from .. import _harness

TEXT_EXTENSIONS = frozenset({".py", ".md", ".json", ".txt"})
DEFAULT_EXCLUDE_DIRS = frozenset({
    "results", "candidates", "sheets", "__pycache__", ".git", ".pytest_cache",
})
DEFAULT_EXCLUDE_FILES = frozenset({"REVIEW.json", "UNBLIND-MAP.json"})

# --------------------------------------------------------------------------
# substitution pass
# --------------------------------------------------------------------------

_SUBSTITUTIONS: tuple[tuple[re.Pattern[str], str], ...] = (
    # arm names -- the product's own identifying vocabulary
    (re.compile(r"\bFULL_BRAIN\b"), "SYSTEM_X"),
    (re.compile(r"\bfull_brain\b"), "system_x"),
    (re.compile(r"\bfull-brain\b", re.IGNORECASE), "system-x"),
    (re.compile(r"\bNO_BRAIN\b"), "BASELINE"),
    (re.compile(r"\bno_brain\b"), "baseline"),
    (re.compile(r"\bno-brain\b", re.IGNORECASE), "no-memory-baseline"),
    # brand / org
    (re.compile(r"\bentire[-_]?brain\b", re.IGNORECASE), "system_x"),
    (re.compile(r"\bentirehq\b", re.IGNORECASE), "org-anon"),
    (re.compile(r"\bentire\.io\b", re.IGNORECASE), "org-anon.example"),
    (re.compile(r"\bentireio\b", re.IGNORECASE), "org-anon"),
    # personal identity
    (re.compile(r"\bsuhaan\s+thayyil\b", re.IGNORECASE), "the benchmark author"),
    (re.compile(r"\bsuhaan\b", re.IGNORECASE), "author-anon"),
    (re.compile(r"\bthayyil\b", re.IGNORECASE), "anon"),
    # machine-local paths (order matters: most specific first)
    (re.compile(r"/Users/[^/\s\"]+/devenv"), "<REPO_ROOT>"),
    (re.compile(r"/Users/[^/\s\"]+"), "<HOME>"),
    (re.compile(r"\bdevenv\b", re.IGNORECASE), "workspace"),
)


def anonymize_text(text: str) -> tuple[str, int]:
    """-> (anonymized_text, number_of_substitutions_applied)."""
    total = 0
    for pattern, repl in _SUBSTITUTIONS:
        text, n = pattern.subn(repl, text)
        total += n
    return text, total


def build_release(src_root: pathlib.Path, dst_root: pathlib.Path,
                  exclude_dirs: frozenset[str] = DEFAULT_EXCLUDE_DIRS,
                  exclude_files: frozenset[str] = DEFAULT_EXCLUDE_FILES) -> dict:
    """Copy `src_root` into `dst_root`, anonymizing text files in place.

    Returns a manifest: {"files": [...], "substitutions_total": int,
    "excluded_dirs": [...], "excluded_files": [...]}. The manifest itself is
    NOT written to disk here -- callers (CLI below) decide where it goes, so
    tests can call this on a throwaway tree without touching the real one.
    """
    if dst_root.exists() and any(dst_root.iterdir()):
        raise SystemExit(f"{dst_root} exists and is non-empty; refusing to write into it")
    dst_root.mkdir(parents=True, exist_ok=True)

    files: list[dict] = []
    substitutions_total = 0
    excluded_dirs_hit: set[str] = set()
    excluded_files_hit: set[str] = set()

    for src_path in sorted(src_root.rglob("*")):
        if src_path.is_dir():
            continue
        rel = src_path.relative_to(src_root)
        if any(part in exclude_dirs for part in rel.parts[:-1]):
            excluded_dirs_hit.update(p for p in rel.parts[:-1] if p in exclude_dirs)
            continue
        if src_path.name in exclude_files:
            excluded_files_hit.add(src_path.name)
            continue

        dst_path = dst_root / rel
        dst_path.parent.mkdir(parents=True, exist_ok=True)

        if src_path.suffix in TEXT_EXTENSIONS:
            original = src_path.read_text(encoding="utf-8", errors="replace")
            anonymized, n = anonymize_text(original)
            dst_path.write_text(anonymized, encoding="utf-8")
            substitutions_total += n
            files.append({"path": str(rel), "anonymized": True, "substitutions": n})
        else:
            shutil.copyfile(src_path, dst_path)
            files.append({"path": str(rel), "anonymized": False, "substitutions": 0})

    return {
        "src_root": str(src_root),
        "dst_root": str(dst_root),
        "files": sorted(files, key=lambda f: f["path"]),
        "file_count": len(files),
        "substitutions_total": substitutions_total,
        "excluded_dirs": sorted(excluded_dirs_hit),
        "excluded_files": sorted(excluded_files_hit),
    }


# --------------------------------------------------------------------------
# grep-gate: independent of the substitution pass above
# --------------------------------------------------------------------------

FORBIDDEN_WORDS: tuple[str, ...] = ("entire", "entirehq", "entireio", "suhaan", "devenv", "brain")

# "Necessary words" (plan 0.10): legitimate strings that CONTAIN a forbidden
# substring but are not themselves an identifying leak. An allowlist entry
# suppresses a match only when it is EXACTLY this word (case-insensitive,
# whole matched token) -- it never suppresses a match merely adjacent to it,
# so "brainmark-internal" still trips the gate on the "-internal" tail... no:
# see `scan_forbidden` -- the token itself must equal an allowlist entry.
ALLOWLIST_WORDS: frozenset[str] = frozenset({
    "brainmark",                                  # the benchmark's public name
    "brainstorm", "brainstorms", "brainstorming", "brainstormed",  # ordinary English
    "entirely", "entirety",                       # ordinary English containing "entire"
})

_WORD_RE = re.compile(r"[A-Za-z][A-Za-z0-9_.\-]*")


def scan_forbidden(text: str, forbidden: tuple[str, ...] = FORBIDDEN_WORDS,
                   allowlist: frozenset[str] = ALLOWLIST_WORDS) -> list[dict]:
    """-> [{"word": banned_substring, "context_word": full_token, "index": int}, ...]

    Substring match, not whole-word-equals: "entirehq.example.com" and a path
    fragment like "suhaan" glued into a longer filename must both be caught,
    not only an exact standalone "entirehq"/"suhaan" token. `allowlist` is
    checked against the FULL matched token, lowercased.

    A token can match more than one banned word (e.g. "entirehq" contains
    both "entire" and "entirehq") -- every match is recorded, not just the
    first, so the gate's hit count and the more specific word are both
    auditable rather than one match silently shadowing another.
    """
    findings: list[dict] = []
    lowered_forbidden = [f.lower() for f in forbidden]
    for match in _WORD_RE.finditer(text):
        token = match.group(0)
        lowered_token = token.lower()
        if lowered_token in allowlist:
            continue
        for banned in lowered_forbidden:
            if banned in lowered_token:
                findings.append({"word": banned, "context_word": token, "index": match.start()})
    return findings


def scan_tree(root: pathlib.Path, extensions: frozenset[str] = TEXT_EXTENSIONS,
             exclude_dirs: frozenset[str] = DEFAULT_EXCLUDE_DIRS) -> dict[str, list[dict]]:
    """-> {relative_path: [findings, ...]} for every text file with >=1 hit."""
    hits: dict[str, list[dict]] = {}
    for path in sorted(root.rglob("*")):
        if path.is_dir() or path.suffix not in extensions:
            continue
        rel = path.relative_to(root)
        if any(part in exclude_dirs for part in rel.parts[:-1]):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        findings = scan_forbidden(text)
        if findings:
            hits[str(rel)] = findings
    return hits


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def cmd_build(args) -> int:
    manifest = build_release(pathlib.Path(args.src), pathlib.Path(args.dst))
    manifest_path = pathlib.Path(args.dst) / "RELEASE-MANIFEST.json"
    manifest_path.write_text(_harness.pretty_json(manifest), encoding="utf-8")
    print(f"wrote {manifest['file_count']} files ({manifest['substitutions_total']} "
         f"substitutions) to {args.dst}; manifest at {manifest_path}")

    gate_hits = scan_tree(pathlib.Path(args.dst))
    if gate_hits:
        total = sum(len(v) for v in gate_hits.values())
        print(f"WARNING: grep-gate found {total} forbidden-string hit(s) in "
             f"{len(gate_hits)} released file(s) -- release is NOT publication-safe:",
             file=sys.stderr)
        for path, findings in sorted(gate_hits.items()):
            print(f"  {path}: {[f['context_word'] for f in findings]}", file=sys.stderr)
        return 1
    print("grep-gate: clean (0 forbidden-string hits in the released tree)")
    return 0


def cmd_check(args) -> int:
    hits = scan_tree(pathlib.Path(args.root))
    total = sum(len(v) for v in hits.values())
    print(_harness.pretty_json({"root": args.root, "files_with_hits": len(hits),
                                "total_hits": total, "hits": hits}))
    return 1 if hits else 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Build/check an anonymized BrainMark release.")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_build = sub.add_parser("build", help="anonymize a copy of the tree, then run the gate")
    p_build.add_argument("--src", required=True)
    p_build.add_argument("--dst", required=True)
    p_build.set_defaults(func=cmd_build)

    p_check = sub.add_parser("check", help="run the grep-gate against an existing directory")
    p_check.add_argument("--root", required=True)
    p_check.set_defaults(func=cmd_check)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
