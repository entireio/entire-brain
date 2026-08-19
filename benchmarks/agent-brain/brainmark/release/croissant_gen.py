#!/usr/bin/env python3
"""Emit a Croissant JSON-LD manifest + Responsible-AI metadata (plan 0.10).

2026+ artifact-compliance tracks (NeurIPS D&B included) desk-reject a dataset
submission without a valid Croissant (https://mlcommons.org/croissant/)
manifest and Responsible-AI metadata fields at submission time -- this module
exists so that requirement is met mechanically from data BrainMark already
produces, not hand-authored and left to drift:

  - `SEAL-MANIFEST.json` (`seal.py`) supplies the dataset's identity: sealed/
    dev task file sha256s (-> Croissant `distribution` file objects with
    content hashes), the seal's dual-review kappa (-> a Responsible-AI
    "human-review reliability" field), and provenance (miner/prompts/
    mechmetrics/estimator hashes).
  - `DATASHEET.md` (Gebru et al. datasheet, already written by hand) supplies
    the prose: motivation, composition, uses/non-uses, limitations,
    licensing. This module does not re-derive that prose -- it extracts it by
    section heading, so a hand-edit to DATASHEET.md propagates into the next
    generated manifest instead of needing a second hand-edit here.

Validation is import-guarded: if the real `mlcroissant` package is
importable, its `Dataset(jsonld=...)` constructor is used as the ground-truth
validator; if it is not (the common case -- `mlcroissant` is a heavy,
non-stdlib dependency this repo does not otherwise need), a STRUCTURAL
self-validator checks the required-field shape instead. Both paths return the
same `{"validator": ..., "ok": bool, "errors": [...]}` shape so callers never
need to know which one ran.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from .. import _harness

CROISSANT_CONTEXT: dict = {
    "@language": "en",
    "@vocab": "https://schema.org/",
    "citeAs": "cr:citeAs",
    "column": "cr:column",
    "conformsTo": "dct:conformsTo",
    "cr": "http://mlcommons.org/croissant/",
    "rai": "http://mlcommons.org/croissant/RAI/",
    "data": {"@id": "cr:data", "@type": "@json"},
    "dataType": {"@id": "cr:dataType", "@type": "@vocab"},
    "dct": "http://purl.org/dc/terms/",
    "extract": "cr:extract",
    "field": "cr:field",
    "fileObject": "cr:fileObject",
    "fileSet": "cr:fileSet",
    "recordSet": "cr:recordSet",
    "sc": "https://schema.org/",
    "source": "cr:source",
    "subField": "cr:subField",
}

REQUIRED_TOP_LEVEL: tuple[str, ...] = (
    "@context", "@type", "name", "description", "license", "distribution", "recordSet",
)


# --------------------------------------------------------------------------
# DATASHEET.md section extraction
# --------------------------------------------------------------------------


def parse_markdown_sections(text: str) -> dict[str, str]:
    """`## Heading` -> body text, up to the next `## Heading` or `# Heading`.

    Generic on purpose (no hardcoded DATASHEET.md heading list): if a heading
    is renamed upstream, this just returns a dict under the new name and
    `build_responsible_ai`'s `.get(..., "")` lookups degrade to an empty
    field rather than raising -- a stale manifest is a smaller failure than a
    crashing release build.
    """
    sections: dict[str, str] = {}
    current: str | None = None
    buf: list[str] = []
    for line in text.splitlines():
        match = re.match(r"^#{1,6}\s+(.*)$", line)
        if match:
            if current is not None:
                sections[current] = "\n".join(buf).strip()
            current = match.group(1).strip()
            buf = []
        elif current is not None:
            buf.append(line)
    if current is not None:
        sections[current] = "\n".join(buf).strip()
    return sections


# --------------------------------------------------------------------------
# Croissant document construction
# --------------------------------------------------------------------------


def _file_objects(manifest: dict) -> list[dict]:
    """SEAL-MANIFEST.json `tasks` -> one Croissant `cr:FileObject` per sealed/
    dev task file, content-addressed by the SAME sha256 the seal already
    pinned -- the release's file identity and the seal's file identity are
    provably the same number, not two numbers that happen to agree today."""
    objects = []
    for rel_path, entry in sorted(manifest.get("tasks", {}).items()):
        objects.append({
            "@type": "cr:FileObject",
            "@id": f"file-{rel_path}",
            "name": rel_path,
            "contentUrl": rel_path,
            "encodingFormat": "application/json",
            "sha256": entry["sha256"],
            "contentSize": entry.get("bytes"),
            "cr:extraProperty": {"split": entry.get("split")},
        })
    return objects


def _record_set(manifest: dict) -> dict:
    return {
        "@type": "cr:RecordSet",
        "@id": "pairs",
        "name": "pairs",
        "description": "One record per mined (A, B) SWE-bench instance pair.",
        "field": [
            {"@type": "cr:Field", "@id": "pairs/pair_id", "name": "pair_id", "dataType": "sc:Text"},
            {"@type": "cr:Field", "@id": "pairs/split", "name": "split", "dataType": "sc:Text"},
            {"@type": "cr:Field", "@id": "pairs/sha256", "name": "sha256", "dataType": "sc:Text"},
        ],
    }


def build_responsible_ai(manifest: dict, datasheet_sections: dict[str, str]) -> dict:
    """Responsible-AI metadata fields (rai: namespace), populated from the
    seal manifest (quantitative: kappa, provenance) and DATASHEET.md
    (qualitative: uses, limitations, licensing) rather than re-authored here.
    """
    seal_v2 = manifest.get("seal_v2")
    human_review: dict = {
        "reviewProcess": "dual independent review; see SEAL_PROTOCOL.md",
        "reviewerCount": 2 if seal_v2 else 1,
    }
    if seal_v2:
        human_review["interRaterReliability"] = {
            "statistic": "Cohen's kappa",
            "value": seal_v2.get("cohens_kappa"),
            "nDuallyReviewedPairs": seal_v2.get("n_dually_reviewed_pairs"),
            "disagreementsAdjudicated": seal_v2.get("disagreements_adjudicated"),
        }
        human_review["raterDisclosure"] = (
            "Raters are the benchmark author and one project-affiliated teammate; "
            "see SEAL_PROTOCOL.md's disclosure template."
        )

    return {
        "rai:dataCollection": datasheet_sections.get("Collection process", ""),
        "rai:dataCollectionType": "deterministic offline mining over an existing public benchmark "
                                  "(SWE-bench); no new human-subject data collected",
        "rai:dataAnnotationProtocol": datasheet_sections.get("Preprocessing / labeling", ""),
        "rai:dataAnnotationPlatform": human_review,
        "rai:dataUseCases": datasheet_sections.get("Uses", ""),
        "rai:dataLimitations": datasheet_sections.get("Known limitations (stated up front)", ""),
        "rai:dataBiasAndFairness": (
            "Two repositories represented at current n; no per-repo sub-group is reported "
            "below the ~30-pair power floor (PREREGISTRATION.md §7). See DATASHEET.md "
            "§Composition."
        ),
        "rai:personalSensitiveInformation": (
            "None. Records reference public SWE-bench instance IDs and derived sha256/score "
            "metadata only; no upstream problem statement, patch, or test text is copied "
            "into the record (DATASHEET.md §Composition)."
        ),
    }


def build_croissant(manifest: dict, datasheet_text: str, config: dict | None = None,
                    dataset_name: str = "brainmark-pairs") -> dict:
    sections = parse_markdown_sections(datasheet_text)
    motivation = sections.get("Motivation", "")
    licensing = sections.get("Distribution and licensing", "")

    doc: dict = {
        "@context": dict(CROISSANT_CONTEXT),
        "@type": "sc:Dataset",
        "name": dataset_name,
        "description": motivation or "BrainMark paired-task dataset for cross-session "
                                     "memory-utility evaluation of coding agents.",
        "conformsTo": "http://mlcommons.org/croissant/1.0",
        "license": "MIT (harness); consumers must obtain SWE-bench itself from upstream "
                   "(see DATASHEET.md §Distribution and licensing)",
        "version": f"seal-schema-{manifest.get('schema_version', '?')}",
        "sealed_count": manifest.get("sealed_count"),
        "dev_count": manifest.get("dev_count"),
        "distribution": _file_objects(manifest),
        "recordSet": [_record_set(manifest)],
        "rai": build_responsible_ai(manifest, sections),
        "_provenance": {
            "miner_sha256": manifest.get("miner_sha256"),
            "config_sha256": manifest.get("config_sha256"),
            "prereg_sha256": manifest.get("prereg_sha256"),
            "mechmetrics_sha256": manifest.get("mechmetrics_sha256"),
            "vendored_metrics_sha256": manifest.get("vendored_metrics_sha256"),
        },
    }
    if licensing:
        doc["_licensing_detail"] = licensing
    return doc


# --------------------------------------------------------------------------
# validation: mlcroissant if importable, else structural self-validation
# --------------------------------------------------------------------------


def validate(doc: dict) -> dict:
    try:
        import mlcroissant as mlc  # noqa: PLC0415 - optional, import-guarded on purpose
    except ImportError:
        return _structural_validate(doc)

    try:
        mlc.Dataset(jsonld=doc)
    except Exception as exc:  # noqa: BLE001 - surfaced as a validation error, not raised
        return {"validator": "mlcroissant", "ok": False, "errors": [str(exc)]}
    return {"validator": "mlcroissant", "ok": True, "errors": []}


def _structural_validate(doc: dict) -> dict:
    """No `mlcroissant` available: check the shape ourselves.

    Not a substitute for real Croissant-spec validation -- it is a floor that
    catches "the generator is obviously broken" (missing required fields, a
    file object with no content hash, an empty record set) without taking on
    a heavy dependency this repo does not otherwise need. `validate()`'s
    return shape is identical either way so callers (tests, CLI) never branch
    on which validator ran.
    """
    errors: list[str] = []
    for key in REQUIRED_TOP_LEVEL:
        if not doc.get(key):
            errors.append(f"missing or empty required field: {key}")

    if doc.get("@type") not in ("sc:Dataset", "Dataset", "https://schema.org/Dataset"):
        errors.append(f"@type should identify a Dataset, got {doc.get('@type')!r}")

    for i, dist in enumerate(doc.get("distribution", [])):
        if not dist.get("sha256"):
            errors.append(f"distribution[{i}] ({dist.get('name')}) has no sha256 content hash")
        if not dist.get("contentUrl"):
            errors.append(f"distribution[{i}] ({dist.get('name')}) has no contentUrl")

    record_sets = doc.get("recordSet") or []
    if not record_sets:
        errors.append("recordSet is empty")
    for i, rs in enumerate(record_sets):
        if not rs.get("field"):
            errors.append(f"recordSet[{i}] ({rs.get('name')}) has no fields")

    if not isinstance(doc.get("rai"), dict) or not doc["rai"]:
        errors.append("rai (Responsible-AI metadata) is missing or empty")

    return {"validator": "structural", "ok": not errors, "errors": errors}


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate + validate a Croissant manifest for BrainMark.")
    parser.add_argument("--manifest", default=str(_harness.BRAINMARK_DIR / "SEAL-MANIFEST.json"))
    parser.add_argument("--datasheet", default=str(_harness.BRAINMARK_DIR / "DATASHEET.md"))
    parser.add_argument("--name", default="brainmark-pairs")
    parser.add_argument("--out", default=None)
    args = parser.parse_args(argv)

    manifest_path = pathlib.Path(args.manifest)
    if not manifest_path.is_file():
        raise SystemExit(f"{manifest_path} not found -- run seal.py promote first")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    datasheet_text = pathlib.Path(args.datasheet).read_text(encoding="utf-8") \
        if pathlib.Path(args.datasheet).is_file() else ""

    doc = build_croissant(manifest, datasheet_text, dataset_name=args.name)
    result = validate(doc)

    rendered = _harness.pretty_json(doc)
    if args.out:
        pathlib.Path(args.out).write_text(rendered, encoding="utf-8")
    print(rendered, end="")
    print(f"\nvalidation ({result['validator']}): {'OK' if result['ok'] else 'FAILED'}", file=sys.stderr)
    for error in result["errors"]:
        print(f"  - {error}", file=sys.stderr)
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
