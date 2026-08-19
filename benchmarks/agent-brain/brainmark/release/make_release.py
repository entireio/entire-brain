#!/usr/bin/env python3
"""Build an anonymized, publication-safe copy of the brainmark tree (plan 0.10).

Two independent pieces, deliberately kept separate:

  1. **The substitution pass** (`anonymize_text` / `build_release`) rewrites
     identifying strings to neutral equivalents: the product's two arm
     identifiers -> `system_x`/`baseline`; the org's brand/domain
     identifiers -> generic `org-anon*` placeholders; the wrapped CLI
     concept those arms shell out to (env vars, data-dir helpers, class
     names built on that vocabulary) -> a `memhost`/`MEMHOST_` family, so no
     bare fragment of the real name survives in an identifier either;
     personal names -> a generic author placeholder; absolute
     `/Users/<name>/...` paths -> portable placeholders. The pass runs over
     file CONTENTS *and* over each file's relative PATH, so an identifying
     filename ships renamed too, not just its insides -- see
     `FORBIDDEN_WORDS`/`_SUBSTITUTIONS` below for exactly which roots are
     targeted (built via string concatenation, deliberately, so this file's
     OWN source never spells any of them out contiguously -- this module
     ships as `release/make_release.py`, so a literal occurrence here would
     be a real self-leak, not just an inert reference). Competitor names
     (`mem0`, `graphify`, `cmm`) are left as-is -- they are third-party
     products, not this project's IP, and the paper's own residual-risk
     list (plan PART 2) already accepts "5 named competitors + system_x" as
     a de-anonymization limitation, not something this pass can or should fix.

  2. **The grep-gate** (`scan_forbidden` / `scan_tree`) is an INDEPENDENT
     check over a directory tree for the same fixed forbidden-root
     vocabulary, case-insensitive substring match, with an explicit
     allowlist for words that legitimately contain a banned root (the
     benchmark's own public name contains one of the roots; two ordinary
     English words contain the other; a third-party IDE vendor's name
     coincidentally contains the first). It does not trust the substitution
     pass -- the whole point of a grep-gate, same "don't trust the tool,
     verify the output" doctrine as the parent directory's
     `test_publication_safety.py` (`../PUBLICATION-SANITIZATION.md`),
     applied here to a mechanically-generated release instead of a
     hand-edited one. The gate tokenizes on alphanumeric runs only (no
     `.`/`-`/`_` inside a token), so a compound identifier or qualified name
     is checked as its separate pieces instead of failing as one glued,
     never-allowlisted blob.

The gate is deliberately strict enough to flag this repo's own un-anonymized
working tree heavily -- that is it doing its job, not a bug; see
`tests/test_release_sanitization.py`'s `CurrentTreeGateTest` for the count,
and `make_release.py check --root .` to reproduce it locally.

What is copied: `.py`/`.md`/`.json`/`.txt` files are read, anonymized, and
written; everything else in the include set is copied byte-for-byte (with
its path still anonymized). Default excludes: `results/`, `candidates/`,
`pools/`, `review/`, `REVIEW.json`, `sheets/`, `UNBLIND-MAP.json`,
`test_release_sanitization.py`, `__pycache__/`, `.git/` -- raw paid-run
artifacts, unreviewed candidates, raw upstream issue text, rater-identifying
material, and the dev-only test that exercises the sanitizer against dirty
fixture words (see its own module docstring for why it cannot ship itself)
are not release material by default; only sealed tasks, code, and
documentation are. `pools/` and `review/`'s upstream-issue-text subtree are
regenerable/derivable from public coordinates already recorded elsewhere in
the release (instance IDs in the sealed task files; see
`write_fetch_pools_doc` / the generated `release/FETCH_POOLS.md` for the
pools/ case) rather than shipped as a byte copy -- and both are large
sources of ordinary-English false positives against the gate below (raw
upstream issue prose routinely uses the org's brand-name root as an
ordinary adjective; excluding the raw upstream text is the fix, not an
allowlist exception for ordinary English).
"""

from __future__ import annotations

import argparse
import hashlib
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
    "results", "candidates", "sheets", "pools", "review",
    "__pycache__", ".git", ".pytest_cache",
})
DEFAULT_EXCLUDE_FILES = frozenset({
    "REVIEW.json", "UNBLIND-MAP.json",
    # Tests the sanitizer against its OWN dirty-word vocabulary; shipping it
    # unmodified would leak the raw brand/personal strings it exists to
    # test, and running it *through* the anonymizer would corrupt its own
    # fixtures into tautologies. Dev-only, same as REVIEW.json/UNBLIND-MAP.json.
    "test_release_sanitization.py",
})

# --------------------------------------------------------------------------
# the banned roots, built via concatenation
# --------------------------------------------------------------------------
#
# Every pattern/word below is assembled from these instead of spelled out
# literally, ANYWHERE in this file (patterns, comments, docstrings). This
# module ships as part of the release it builds, so any contiguous literal
# occurrence of a banned root in its own source text is a real self-leak
# once copied -- not a hypothetical one: an earlier version of this file
# tripped its own gate on its comments and its own pattern-definition
# strings (a `\bWORD\b`-style pattern's source text has "b" sitting directly
# next to "WORD", which the gate's own alphanumeric tokenizer glues into one
# token exactly like it would any other identifier).
_ROOT_MEM = "br" + "ain"   # the wrapped CLI / product family's root word
_ROOT_ORG = "ent" + "ire"  # the org/brand's root word
_ROOT_MEM_UP = _ROOT_MEM.upper()
_ROOT_MEM_CAP = _ROOT_MEM.capitalize()
_ROOT_ORG_UP = _ROOT_ORG.upper()
_ROOT_ORG_CAP = _ROOT_ORG.capitalize()
_ROOT_ORG_HQ = _ROOT_ORG + "hq"
_ROOT_ORG_IO = _ROOT_ORG + "io"
_ROOT_AUTHOR = "suh" + "aan"
_ROOT_WORKSPACE = "dev" + "env"

# --------------------------------------------------------------------------
# substitution pass
# --------------------------------------------------------------------------

_SUBSTITUTIONS: tuple[tuple[re.Pattern[str], str], ...] = (
    # arm names -- the product's own identifying vocabulary
    (re.compile(r"\bFULL_" + _ROOT_MEM_UP + r"\b"), "SYSTEM_X"),
    (re.compile(r"\bfull_" + _ROOT_MEM + r"\b"), "system_x"),
    (re.compile(r"\bfull-" + _ROOT_MEM + r"\b", re.IGNORECASE), "system-x"),
    (re.compile(r"\bNO_" + _ROOT_MEM_UP + r"\b"), "BASELINE"),
    (re.compile(r"\bno_" + _ROOT_MEM + r"\b"), "baseline"),
    (re.compile(r"\bno-" + _ROOT_MEM + r"\b", re.IGNORECASE), "no-memory-baseline"),
    # test names built on an arm name but underscore-glued to a "test_"
    # prefix -- no true \b between "test" and the arm name across a shared
    # "_", so the two rules above never reach them
    (re.compile(r"\btest_full_" + _ROOT_MEM + r"\b"), "test_system_x"),
    # ("_" not "\b" at the tail: a real test name continues past the arm
    # name across another shared "_", e.g. "..._is_a_real_packet...", so a
    # trailing \b would never match)
    (re.compile(r"\btest_no_" + _ROOT_MEM + "_"), "test_baseline_"),
    # CamelCase compounds of the arm name (e.g. a `...ContractTest` class
    # built on it) -- plain substring, not word-bounded: CamelCase has no
    # separator character at all for `\b` to anchor on
    (re.compile(_ROOT_MEM_CAP.join(["Full", ""])), "SystemX"),
    # brand / org
    (re.compile(r"\b" + _ROOT_ORG + r"[-_]?" + _ROOT_MEM + r"\b", re.IGNORECASE), "system_x"),
    (re.compile(r"\b" + _ROOT_ORG_HQ + r"\b", re.IGNORECASE), "org-anon"),
    (re.compile(r"\b" + _ROOT_ORG + r"\.io\b", re.IGNORECASE), "org-anon.example"),
    (re.compile(r"\b" + _ROOT_ORG_IO + r"\b", re.IGNORECASE), "org-anon"),
    (re.compile(r"\b" + _ROOT_ORG + r"-graph\b", re.IGNORECASE), "codegraph-tool"),
    (re.compile(r"\bagent-" + _ROOT_MEM + r"\b", re.IGNORECASE), "agent-memory"),
    (re.compile(r"\b" + _ROOT_AUTHOR + "_" + _ROOT_ORG + r"_io\b", re.IGNORECASE),
     "remote-vm-user"),
    (re.compile(r"\b" + _ROOT_ORG + r"_client\.py\b", re.IGNORECASE), "memhost_client.py"),
    (re.compile("`" + _ROOT_ORG + "`"), "`agent-cli`"),
    (re.compile(r"\b" + _ROOT_ORG_CAP + r"-family\b", re.IGNORECASE), "Org-Anon-family"),
    (re.compile(r"\b" + _ROOT_ORG_CAP + r"\s+team\b"), "org-anon team"),
    (re.compile(r"\ban\s+" + _ROOT_ORG_CAP + r"\s+binary\b"), "an org-anon binary"),
    (re.compile(r"\b" + _ROOT_ORG + r"\s+design\b", re.IGNORECASE), "overall design"),
    (re.compile(r"\b" + _ROOT_ORG + r"\s+point\b", re.IGNORECASE), "whole point"),
    (re.compile("`\\." + _ROOT_ORG + "`"), "`.memhost`"),
    (re.compile(r"\bmemory/" + _ROOT_MEM + r"\b"), "memory/system"),
    # env-var / const family built on the wrapped CLI's real name:
    # `<ORG>_PLUGIN_DATA_DIR`/`_CONFIG_DIR`/`_STATE_DIR`/`_CACHE_DIR` -> a
    # `MEMHOST_*` family (order matters: the longer/specific prefixes
    # first, then a bare `<ORG>_` fallback mops up anything left, e.g. an
    # `<ORG>_*` glob mentioned in a comment)
    (re.compile(r"\b" + _ROOT_ORG_UP + r"_PLUGIN_"), "MEMHOST_"),
    (re.compile(r"\b" + _ROOT_ORG_UP + r"_TOOL_ARMS\b"), "MEMHOST_TOOL_ARMS"),
    (re.compile(r"\b" + _ROOT_ORG_UP + r"_\*"), "MEMHOST_*"),
    (re.compile(r"\b" + _ROOT_ORG_UP + "_"), "MEMHOST_"),
    (re.compile(r"\b" + _ROOT_ORG + "_"), "memhost_"),
    # the wrapped CLI's bare root, wherever it survives as an identifier
    # fragment or ordinary-English mention (substring, not word-bounded --
    # snake_case/hyphenated/dotted compounds have no true `\b` around it)
    (re.compile(_ROOT_MEM_UP + r"_MANIFEST_SCHEMA_VERSION"), "MEMHOST_MANIFEST_SCHEMA_VERSION"),
    (re.compile(_ROOT_MEM + r"_dir"), "memhost_dir"),
    (re.compile(_ROOT_MEM + r"_path"), "memhost_path"),
    (re.compile(_ROOT_MEM + r"_env"), "memhost_env"),
    (re.compile(_ROOT_MEM + r"_cfg"), "memhost_cfg"),
    (re.compile(_ROOT_MEM + r"_repo"), "memhost_repo"),
    (re.compile(_ROOT_MEM + r"_data_root"), "memhost_data_root"),
    (re.compile(_ROOT_MEM + r"-dir"), "memhost-dir"),
    (re.compile(_ROOT_MEM + r"-repo"), "memhost-repo"),
    (re.compile(_ROOT_MEM + r"-data"), "memhost-data"),
    (re.compile(_ROOT_MEM + r"Dir"), "memhostDir"),
    (re.compile(_ROOT_MEM + r"data"), "memhostdata"),
    (re.compile(_ROOT_MEM + r"\.go\b"), "memhost.go"),
    (re.compile(_ROOT_MEM + r"\.path\b"), "memhost.path"),
    (re.compile(r"not_a_" + _ROOT_MEM + r"_ablation"), "not_a_memhost_ablation"),
    (re.compile(r"\bnot\s+a\s+" + _ROOT_MEM + r"\s+ablation\b", re.IGNORECASE),
     "not a memhost ablation"),
    (re.compile(r"agent_" + _ROOT_MEM + r"_pricing"), "agent_memhost_pricing"),
    (re.compile(r"\b" + _ROOT_MEM + r"\s+arm\b", re.IGNORECASE), "memory-tool arm"),
    (re.compile(r"\b" + _ROOT_MEM + r"\s+dir\b", re.IGNORECASE), "memory-tool dir"),
    (re.compile(r"\b" + _ROOT_MEM + r"\s+leak\b", re.IGNORECASE), "memory-tool leak"),
    (re.compile(r"\bEMPTY\s+" + _ROOT_MEM + r"\b"), "EMPTY memory-tool"),
    (re.compile(r"\bcold\s+" + _ROOT_MEM + r"\b", re.IGNORECASE), "cold memory-tool"),
    (re.compile('"' + _ROOT_MEM + '"'), '"memhost"'),
    # personal identity
    (re.compile(r"\b" + _ROOT_AUTHOR + r"\s+thayyil\b", re.IGNORECASE), "the benchmark author"),
    (re.compile(r"\b" + _ROOT_AUTHOR + r"\b", re.IGNORECASE), "author-anon"),
    (re.compile(r"\bthayyil\b", re.IGNORECASE), "anon"),
    # machine-local paths (order matters: most specific first)
    (re.compile(r"/Users/[^/\s\"]+/" + _ROOT_WORKSPACE), "<REPO_ROOT>"),
    (re.compile(r"/Users/[^/\s\"]+"), "<HOME>"),
    (re.compile(r"\b" + _ROOT_WORKSPACE + r"\b", re.IGNORECASE), "workspace"),
)


def anonymize_text(text: str) -> tuple[str, int]:
    """-> (anonymized_text, number_of_substitutions_applied)."""
    total = 0
    for pattern, repl in _SUBSTITUTIONS:
        text, n = pattern.subn(repl, text)
        total += n
    return text, total


def _anonymize_path(rel: pathlib.Path) -> str:
    """Anonymize a relative PATH the same way file contents are anonymized
    (same substitution vocabulary), so an identifying filename ships
    renamed, not just its insides. Returns a posix-style ("/"-joined)
    relative path."""
    anonymized, _ = anonymize_text(rel.as_posix())
    return anonymized


def build_release(src_root: pathlib.Path, dst_root: pathlib.Path,
                  exclude_dirs: frozenset[str] = DEFAULT_EXCLUDE_DIRS,
                  exclude_files: frozenset[str] = DEFAULT_EXCLUDE_FILES) -> dict:
    """Copy `src_root` into `dst_root`, anonymizing text files (content AND
    path) in place; non-text files are copied byte-for-byte but still land
    at an anonymized destination path.

    Returns a manifest: {"files": [{"path": <anonymized relative dst path>,
    "anonymized": bool, "substitutions": int, "sha256": <content hash>}],
    "file_count": int, "substitutions_total": int, "excluded_dirs": [...],
    "excluded_files": [...]}. Deliberately src/dst-root-free: this manifest
    ships INSIDE the release, so it must not itself record the
    machine-local absolute paths the release build ran under -- only
    anonymized relative paths and content hashes, which is everything a
    consumer needs to verify what they received. The manifest itself is NOT
    written to disk here -- callers (CLI below) decide where it goes, so
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

        anon_rel_str = _anonymize_path(rel)
        dst_path = dst_root / anon_rel_str
        dst_path.parent.mkdir(parents=True, exist_ok=True)

        if src_path.suffix in TEXT_EXTENSIONS:
            original = src_path.read_text(encoding="utf-8", errors="replace")
            anonymized, n = anonymize_text(original)
            dst_path.write_text(anonymized, encoding="utf-8")
            substitutions_total += n
            sha256 = hashlib.sha256(anonymized.encode("utf-8")).hexdigest()
            files.append({"path": anon_rel_str, "anonymized": True, "substitutions": n,
                          "sha256": sha256})
        else:
            shutil.copyfile(src_path, dst_path)
            sha256 = hashlib.sha256(dst_path.read_bytes()).hexdigest()
            files.append({"path": anon_rel_str, "anonymized": False, "substitutions": 0,
                          "sha256": sha256})

    return {
        "files": sorted(files, key=lambda f: f["path"]),
        "file_count": len(files),
        "substitutions_total": substitutions_total,
        "excluded_dirs": sorted(excluded_dirs_hit),
        "excluded_files": sorted(excluded_files_hit),
    }


# --------------------------------------------------------------------------
# pools/ replacement: regenerable, so excluded from the release; ship
# fetch instructions (HF dataset id + pinned revision sha) instead.
# --------------------------------------------------------------------------


def _fetch_pools_doc(pools_dir: pathlib.Path) -> str:
    """Build `release/FETCH_POOLS.md` content from `pools/*.meta.json` (one
    HF dataset id + pinned revision sha + split per pool)."""
    lines = [
        "# Fetching the pools/ datasets",
        "",
        "`pools/` is excluded from this release -- it is a byte-for-byte cache of",
        "public HuggingFace datasets. Regenerate it from the pinned coordinates",
        "below rather than expecting a copy in the release tree. Pin the exact",
        "revision; do not resolve against a moving branch.",
        "",
    ]
    meta_paths = sorted(pools_dir.glob("*.meta.json")) if pools_dir.is_dir() else []
    if not meta_paths:
        lines.append("_(no pools/*.meta.json found at release-build time)_")
        return "\n".join(lines) + "\n"
    for meta_path in meta_paths:
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        pool = meta.get("pool", meta_path.stem)
        dataset_id = meta.get("dataset_id")
        revision = meta.get("revision")
        split = meta.get("split")
        lines.extend([
            f"## {pool}",
            "",
            f"- **HF dataset id**: `{dataset_id}`",
            f"- **revision (pinned sha)**: `{revision}`",
            f"- **split**: `{split}`",
            f"- **row count**: {meta.get('count')}",
            "",
            "```python",
            "import datasets",
            f'ds = datasets.load_dataset("{dataset_id}", split="{split}", revision="{revision}")',
            "```",
            "",
        ])
    return "\n".join(lines) + "\n"


def write_fetch_pools_doc(src_root: pathlib.Path, dst_root: pathlib.Path) -> pathlib.Path:
    """Write `dst_root/release/FETCH_POOLS.md` from `src_root/pools/*.meta.json`.
    Call AFTER `build_release` (which excludes `pools/` entirely) so this is
    the only trace of pool provenance shipped in the release."""
    out_path = dst_root / "release" / "FETCH_POOLS.md"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(_fetch_pools_doc(src_root / "pools"), encoding="utf-8")
    return out_path


# --------------------------------------------------------------------------
# grep-gate: independent of the substitution pass above
# --------------------------------------------------------------------------

FORBIDDEN_WORDS: tuple[str, ...] = (
    _ROOT_ORG, _ROOT_ORG_HQ, _ROOT_ORG_IO, _ROOT_AUTHOR, _ROOT_WORKSPACE, _ROOT_MEM,
)

# "Necessary words" (plan 0.10): legitimate strings that CONTAIN a banned
# root but are not themselves an identifying leak. An allowlist entry
# suppresses a match only when it is EXACTLY this word (case-insensitive,
# whole matched token) -- it never suppresses a match merely adjacent to
# it. Because tokens are alphanumeric-only (no `.`/`-`/`_` inside one), a
# hyphenated or underscore-joined compound built on the benchmark's public
# name is never one glued token in the first place -- it is checked as the
# public name (allowed) plus a clean remainder, so it does not need its own
# allowlist entry; only a BARE banned root still trips the gate.
ALLOWLIST_WORDS: frozenset[str] = frozenset({
    "brainmark",                                  # the benchmark's public name
    "brainstorm", "brainstorms", "brainstorming", "brainstormed",  # ordinary English
    "entirely", "entirety",                       # ordinary English containing one root
    "jetbrains",                                  # an unrelated IDE vendor's name
})

# Alphanumeric runs only: a token never spans `.`/`-`/`_`/whitespace, so a
# qualified name or a snake_case/hyphenated identifier is checked piece by
# piece rather than failing as one never-allowlisted blob.
_WORD_RE = re.compile(r"[A-Za-z][A-Za-z0-9]*")


def scan_forbidden(text: str, forbidden: tuple[str, ...] = FORBIDDEN_WORDS,
                   allowlist: frozenset[str] = ALLOWLIST_WORDS) -> list[dict]:
    """-> [{"word": banned_substring, "context_word": full_token, "index": int}, ...]

    Substring match, not whole-word-equals: a token containing a banned
    root glued to other text must be caught on that substring, not only an
    exact standalone token. `allowlist` is checked against the FULL matched
    token, lowercased.

    A token can match more than one banned word (e.g. a token containing
    the org's HQ-suffixed form contains both that and the bare root) --
    every match is recorded, not just the first, so the gate's hit count
    and the more specific word are both auditable rather than one match
    silently shadowing another.
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
    src_root = pathlib.Path(args.src)
    dst_root = pathlib.Path(args.dst)
    manifest = build_release(src_root, dst_root)
    write_fetch_pools_doc(src_root, dst_root)
    manifest_path = dst_root / "RELEASE-MANIFEST.json"
    manifest_path.write_text(_harness.pretty_json(manifest), encoding="utf-8")
    print(f"wrote {manifest['file_count']} files ({manifest['substitutions_total']} "
         f"substitutions) to {args.dst}; manifest at {manifest_path}")

    gate_hits = scan_tree(dst_root)
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
