# Candidate Distillation Phase-Gate Evidence

`entire brain facts distill-quality resolve` resolves an imported Phase 0–4
gate panel. It validates local artifacts and digest bindings; it does not call,
authenticate, or impersonate Claude, Copilot, Cursor, or Codex Astra. A valid
import is not quantitative proof, a human label, or approval to change the
default distillation pipeline.

```text
entire brain facts distill-quality resolve \
  --evidence evidence.json \
  --out fresh-result-directory \
  [--astra-verdict astra.json] \
  [--json]
```

`--out` must not exist. The resolver creates it with mode `0700` and writes
`summary.json` and `archive.json` with mode `0600`. Inputs and referenced
artifacts must be regular files of at most 8 MiB. Symlinks, FIFOs, directories,
files that change while being opened, and oversized files are refused. Keep
these files local: they are evidence records, not a source-content queue, and
the resolver never uploads them or creates a human-review queue.

The command writes the summary and archive for every well-formed resolution.
It exits successfully only when the resulting state is `pass`. States `fail`,
`open`, and `escalation_required` return a nonzero exit after the artifacts are
written. Invalid schemas, hashes, files, or bindings return nonzero without a
valid resolution.

## Dependency order

The schema deliberately prevents a digest cycle:

1. Seal the base revision, rubric, evaluation input, and evaluation artifact.
2. Compute `evidence_digest` from those four hashes and the phase identity.
3. Give that sealed digest and the same base evidence to each primary judge.
4. Store and hash each judge's prompt, payload, telemetry, and bounded
   rationale. Construct and hash the primary verdicts.
5. Compute `primaries_digest` from the sorted verdicts and explicit coverage.
6. Only when escalation is eligible, give Astra `evidence_digest`,
   `primaries_digest`, the sealed evidence, the primary verdicts, and their
   bounded rationales. Hash Astra's prompt, payload, telemetry, rationale, and
   judgment.
7. Compute the final aggregate from the evidence, primary panel, resolved
   state, and actual Astra judgment digest.

Later judge artifacts never participate in `evidence_digest`. This lets a
prompt contain the already-sealed digest without making
`evidence_digest -> prompt_digest -> evidence_digest` circular.

## Evidence JSON

The top-level contract is `phase_gate_evidence_v1`, schema version `1`, and a
phase from `0` through `4`. It contains:

- `revision_digest`, `rubric_digest`, `input_digest`, and `artifact_digest`;
- `base_artifacts`, with exactly named `revision`, `rubric`, `input`, and
  `evaluation` records that bind those four hashes to verified bytes;
- `evidence_digest`;
- `judge_artifacts` for completed primary judgments and unavailable/invalid
  status evidence;
- `primary_verdicts` containing only structurally valid completed judgments;
- `primary_coverage`, with exactly one record for each of `claude`, `copilot`,
  and `cursor`.

Every artifact record is `{ "name": ..., "path": ..., "digest": ... }`.
Primary artifacts use these exact names:

```text
judge/<judge>/prompt
judge/<judge>/payload
judge/<judge>/telemetry
judge/<judge>/rationale
judge/<judge>/status        # required for invalid or unavailable coverage
```

Completed coverage has `status: "completed"`, a matching verdict, and no
`detail_digest`. Invalid or unavailable coverage has `status: "invalid"` or
`"unavailable"`, no verdict for that judge, and a `detail_digest` matching the
verified `judge/<judge>/status` artifact. This makes missing coverage explicit;
an importer cannot silently omit a malformed fourth record or a failed judge.

A completed verdict binds its judge, model, sealed evidence, overall verdict,
the exact `performance`, `quality`, and `progress` dimensions, and the four
judge artifact digests. No additional dimension is accepted. Overall `pass`
requires all three dimensions to pass. Overall `fail` requires a failing
dimension. `insufficient_evidence` requires an insufficient dimension.

Fewer than two valid primaries fails closed without Astra. Two matching valid
primaries are decisive when the third is unavailable. A three-judge majority
is decisive unless a primary marks critical dissent. No majority or critical
dissent yields `escalation_required`.

## Astra JSON

Astra is accepted only for an eligible escalation. The contract is
`phase_gate_astra_judgment_v1`, schema version `1`. It must bind the exact
`evidence_digest` and `primaries_digest` and report `pass`, `fail`, or
`insufficient_evidence`. It carries exactly four verified artifacts:

```text
astra/prompt
astra/payload
astra/telemetry
astra/rationale
```

Their hashes appear in the judgment. `judgment_digest` binds the judgment and
artifact names and hashes, with artifact paths blanked. Thus relocating local
files does not alter the judgment identity. Astra `insufficient_evidence`
leaves the gate `open`; Astra cannot replace missing primary coverage below the
two-valid-primary floor.

## Canonical digest rules

All digests are lowercase `sha256:` followed by 64 hexadecimal characters.
Artifact digests hash the file bytes exactly. Structured digests hash compact
UTF-8 JSON as emitted by Go's `encoding/json`: no insignificant whitespace,
struct fields in the order listed below, array order preserved, and HTML-safe
string escaping. Maps are not used in digest inputs.

`evidence_digest` hashes these fields in order:

```text
contract, schema_version, phase, revision_digest, artifact_digest,
rubric_digest, input_digest
```

`verdict_digest` hashes the verdict fields in schema order with
`verdict_digest` present as the empty string. Omit `critical` when false, as its
field uses `omitempty`. Before `primaries_digest`, sort verdicts by `judge` and
coverage records by `judge`. Hash an object with fields `verdicts`, then
`coverage`.

`judgment_digest` hashes the Astra fields in schema order with
`judgment_digest` present as the empty string and every artifact path replaced
by the empty string. `aggregate_digest` hashes, in order,
`evidence_digest`, `primaries_digest`, `state`, and optional
`astra_digest`. `astra_digest` is the actual `judgment_digest`, not Astra's
verdict label.

## Minimal fail-closed generator

This standalone example creates real local artifacts, seals the base before
building judge prompts, records two `insufficient_evidence` judgments, and
records Cursor as unavailable. It intentionally resolves to failure and does
not claim a live provider result or phase acceptance.

```python
import hashlib, json, pathlib

root = pathlib.Path("gate-example")
root.mkdir(mode=0o700)

def compact(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode()

def digest(data):
    if isinstance(data, str):
        data = data.encode()
    return "sha256:" + hashlib.sha256(data).hexdigest()

def artifact(name, content):
    path = root / (name.replace("/", "-") + ".json")
    data = compact(content)
    path.write_bytes(data)
    path.chmod(0o600)
    return {"name": name, "path": str(path), "digest": digest(data)}

base = [
    artifact("revision", {"revision": "EXAMPLE-NOT-A-LIVE-REVISION"}),
    artifact("rubric", {"rubric": "example insufficient-evidence rubric"}),
    artifact("input", {"input": "example sealed evaluation identity"}),
    artifact("evaluation", {"result": "insufficient_evidence"}),
]
by_name = {item["name"]: item for item in base}
core = {
    "contract": "phase_gate_evidence_v1", "schema_version": 1, "phase": 2,
    "revision_digest": by_name["revision"]["digest"],
    "artifact_digest": by_name["evaluation"]["digest"],
    "rubric_digest": by_name["rubric"]["digest"],
    "input_digest": by_name["input"]["digest"],
}
evidence_digest = digest(compact(core))

judge_artifacts, verdicts, coverage = [], [], []
for judge in ("claude", "copilot"):
    parts = {}
    for part in ("prompt", "payload", "telemetry", "rationale"):
        record = artifact(
            f"judge/{judge}/{part}",
            {"example": True, "judge": judge, "part": part,
             "evidence_digest": evidence_digest,
             "result": "insufficient_evidence"},
        )
        judge_artifacts.append(record)
        parts[part] = record["digest"]
    verdict = {
        "judge": judge, "model": "example-not-invoked",
        "evidence_digest": evidence_digest,
        "verdict": "insufficient_evidence",
        "dimensions": [
            {"name": "performance", "verdict": "insufficient_evidence"},
            {"name": "quality", "verdict": "insufficient_evidence"},
            {"name": "progress", "verdict": "insufficient_evidence"},
        ],
        "prompt_digest": parts["prompt"],
        "payload_digest": parts["payload"],
        "telemetry_digest": parts["telemetry"],
        "rationale_digest": parts["rationale"],
        "verdict_digest": "",
    }
    verdict["verdict_digest"] = digest(compact(verdict))
    verdicts.append(verdict)
    coverage.append({"judge": judge, "status": "completed"})

status = artifact("judge/cursor/status", {
    "example": True, "judge": "cursor", "status": "unavailable",
    "evidence_digest": evidence_digest,
})
judge_artifacts.append(status)
coverage.append({"judge": "cursor", "status": "unavailable",
                 "detail_digest": status["digest"]})

evidence = dict(core)
evidence.update({
    "base_artifacts": base,
    "judge_artifacts": judge_artifacts,
    "evidence_digest": evidence_digest,
    "primary_verdicts": sorted(verdicts, key=lambda value: value["judge"]),
    "primary_coverage": sorted(coverage, key=lambda value: value["judge"]),
})
(root / "evidence.json").write_bytes(
    json.dumps(evidence, indent=2, ensure_ascii=False).encode() + b"\n"
)
(root / "evidence.json").chmod(0o600)
print(root / "evidence.json")
```

Run it with a fresh output directory:

```text
entire brain facts distill-quality resolve \
  --evidence gate-example/evidence.json \
  --out gate-example/resolution \
  --json
```

The expected command exit is nonzero and the saved summary state is `fail`,
because the two valid primary judgments report insufficient evidence. This is
the intended fail-closed behavior.
