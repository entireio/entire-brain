# Brain Artifact Wire Contract

Milestone **P1.M1.1**. This document specifies the versioned, transport-agnostic
wire schema for the composite Entire Brain artifact — the thing a server-side
brain store (M1.2) serializes and transports. It lives in the Go package
`internal/brainwire`.

The wire contract does not replace the on-disk model. It is a **new, parallel**
type that mirrors the load-bearing subset of the on-disk manifests in
`internal/cli` and promotes it into an explicit contract that crosses a
transport boundary. The on-disk types (`exportManifest`, `brainSources`,
`semanticSourceManifest`, and the per-source manifests) remain the source of
truth for local generation and are unchanged by this milestone.

## Why a separate wire type

The on-disk manifests carry generator-local detail (absolute `repo_root` paths,
per-run scan counters, provider metrics, warnings) that is either
machine-specific or too fine-grained to be a stable transport contract. The wire
type keeps only what a consumer on the other side of a transport needs:
repository identity, freshness, versioning, provider identity, and
content-addressed pointers to the heavy payloads. Keeping it separate lets the
on-disk shape evolve for local needs without perturbing the transport contract,
and vice versa.

## Type layout

All types are in `internal/brainwire/contract.go`. Every field has an explicit,
stable JSON tag. **The JSON tags are the contract**; Go field names may be
refactored freely, but a tag may only change with a schema version bump.

```
BrainArtifact
├── manifest        BrainManifest      identity + versioning header
├── snapshots       []SnapshotRef      commit-addressed semantic snapshots
├── overlays        []OverlayRef       (base,head)-addressed branch overlays
└── facts           []FactsRef         branch-scoped durable-fact streams
```

### `BrainManifest`

Mirrors the header of `internal/cli.exportManifest` plus the entire-graph provider
identity from `internal/cli.semanticSourceManifest`.

| JSON field                | Type      | Mirrors                                   | Notes |
|---------------------------|-----------|-------------------------------------------|-------|
| `repo_key`                | string    | `exportManifest.RepoKey`                  | deterministic repo identity, e.g. `gh/org/repo` |
| `default_branch`          | string    | `exportManifest.DefaultBranch`            | omitempty |
| `generated_at`            | RFC3339   | `exportManifest.GeneratedAt`              | when the brain state was produced |
| `brain_schema_version`    | string    | *(new)*                                   | this wire contract's version, `major.minor` |
| `provider`                | string    | `semanticSourceManifest.Provider`         | e.g. `entire-graph`; omitempty |
| `provider_version`        | string    | `semanticSourceManifest.ProviderVersion`  | omitempty |
| `provider_schema_version` | string    | `semanticSourceManifest.SchemaVersion`    | the provider's own `major.minor`; independent of `brain_schema_version`; omitempty |

`brain_schema_version` (this contract) and `provider_schema_version` (the
entire-graph output contract) are versioned independently and must not be
conflated.

### Content-addressed references

The heavy payloads are **not** embedded in the artifact. Each is referenced by
its key plus a `ContentRef`. The store transports the referenced bytes out of
band; a reader that already holds a blob with the same digest need not re-fetch
it.

`ContentRef`:

| JSON field   | Type   | Notes |
|--------------|--------|-------|
| `digest`     | string | algorithm-prefixed content hash, e.g. `sha256:9f86d0…`; the canonical address |
| `size`       | int64  | blob length in bytes, when known; omitempty |
| `media_type` | string | e.g. `application/json`, `application/x-ndjson`; omitempty |
| `path`       | string | advisory artifact-relative source path; **not** identity; omitempty |

The three reference kinds and their keys:

| Wire type     | Key                          | On-disk origin (see `docs/semantic_brain_plan.md`, "Brain Layout") |
|---------------|------------------------------|--------------------------------------------------------------------|
| `SnapshotRef` | `commit` (+ optional `tree`) | `semantic/snapshots/<commit>/`                                     |
| `OverlayRef`  | (`base_commit`, `head_commit`) | `semantic/overlays/<base>..<head>.json`                          |
| `FactsRef`    | `branch`                     | `facts/<branch>/facts.ndjson`                                     |

## Content-addressing scheme

Brain state is commit-addressed and branch-aware, per the Semantic Brain Plan's
key design decisions:

- **Snapshots are addressed by git commit.** A semantic snapshot is immutable
  for the commit it indexes, so the commit SHA is its natural key. The optional
  `tree` records the exact indexed tree object.
- **Overlays are addressed by the `(base_commit, head_commit)` pair**, not by
  branch name. A rebase or force-push changes the pair, so the overlay's meaning
  cannot silently drift under a stable branch name. The `branch` field is
  advisory provenance only.
- **Facts are addressed by branch.** Durable facts accrete per branch as an
  NDJSON stream (`facts/<branch>/facts.ndjson`).
- **Blobs are content-addressed by digest.** `ContentRef.Digest` is an
  algorithm-prefixed hash (`sha256:<hex>`) of the referenced bytes. Identity is
  the digest; `path` is only a hint about where the bytes lived on disk. This
  lets the store deduplicate and lets a reader skip fetching blobs it already
  has.

Reference slices are stored in caller-controlled order and contain no maps, so
re-marshaling a decoded `BrainArtifact` with this package's `encoding/json`
round-trip is byte-stable (the round-trip test asserts `Marshal(Unmarshal(b)) ==
b` for canonically-produced bytes). This is a Go-`encoding/json` guarantee, not a
cross-implementation one: it relies on Go's HTML escaping, struct field order,
`omitempty`, and RFC3339Nano time formatting, and it does not hold for bytes a
non-Go producer emitted or for a non-canonical original (e.g. explicit
`"size":0` or reordered fields). **Do not** sign or content-hash the received
artifact bytes directly across implementations, and do not compare them for
equality. Integrity/signing hashes each referenced blob by its `sha256:` digest
(already content-addressed); when the artifact itself must be hashed, signed, or
byte-compared across implementations, run it through the canonical encoding
below instead of relying on `encoding/json` defaults.

## Canonical encoding

Milestone **P1.M1.3**, implemented in `internal/brainwire/canonical.go`. It is a
profile of [RFC 8785][jcs] (JSON Canonicalization Scheme) restricted to this
schema, and it closes exactly the four Go-specific hazards named above.

[jcs]: https://www.rfc-editor.org/rfc/rfc8785

```go
CanonicalMarshal(a BrainArtifact) ([]byte, error)     // typed value -> canonical bytes
Canonicalize(raw []byte) ([]byte, error)              // foreign bytes -> canonical bytes
CanonicalUnmarshal(raw []byte, out *BrainArtifact) error
```

`CanonicalMarshal` routes through the same normalizer as `Canonicalize`, so
`CanonicalMarshal(a)` equals `Canonicalize(anyValidEncodingOf(a))` by
construction. `Canonicalize` is idempotent: `Canonicalize(Canonicalize(b)) ==
Canonicalize(b)`. Neither function changes a single JSON tag — **the tags are
the contract**; this milestone only constrains how those tags are serialized, so
it is not a schema version bump.

### The rules

1. **No HTML escaping.** `<`, `>`, `&` and every other non-control code point
   are emitted as raw UTF-8, never `\u003c`. Affects `repo_key`,
   `default_branch`, `branch`, `path`, `media_type`, `provider`.
2. **Keys sorted lexicographically** by UTF-16 code unit, *not* in Go struct
   declaration order. So a manifest starts with `brain_schema_version` and ends
   with `repo_key`, and the artifact's top-level order is `facts`, `manifest`,
   `overlays`, `snapshots`. Sorting by UTF-16 code unit (not UTF-8 bytes)
   matters only for non-BMP keys a future minor might add.
3. **`omitempty` is normative and exhaustive.** Every optional field has one
   declared omitted default; a producer that emits it explicitly is
   non-canonical, and the canonicalizer removes it. A JSON `null` and an empty
   array both count as absent. Required fields (no `omitempty`) are *always*
   emitted, zero-filled when the input omits them.

   | Type | Field | Required? | Omitted when |
   |---|---|---|---|
   | `BrainArtifact` | `manifest` | required | — |
   | `BrainArtifact` | `snapshots`, `overlays`, `facts` | optional | `null` or `len == 0` |
   | `BrainManifest` | `repo_key`, `generated_at`, `brain_schema_version` | required | — |
   | `BrainManifest` | `default_branch`, `provider`, `provider_version`, `provider_schema_version` | optional | `""` |
   | `ContentRef` | `digest` | required | — |
   | `ContentRef` | `size` | optional | `0` |
   | `ContentRef` | `media_type`, `path` | optional | `""` |
   | `SnapshotRef` | `commit`, `content` | required | — |
   | `SnapshotRef` | `tree` | optional | `""` |
   | `OverlayRef` | `base_commit`, `head_commit`, `content` | required | — |
   | `OverlayRef` | `branch` | optional | `""` |
   | `FactsRef` | `branch`, `content` | required | — |

   The table lives in code as `canonicalArtifact` and friends;
   `TestCanonicalFieldTableMatchesStructTags` fails the build if it ever drifts
   from the struct tags.
4. **Time.** `generated_at` is converted to UTC and **truncated to whole
   seconds**, then written with a literal `Z` — never `+00:00`, never a
   fractional part. Sub-second input **truncates, it does not error**, so
   `12:00:00Z` and `12:00:00.123456789Z` canonicalize to identical bytes. A
   caller needing sub-second fidelity must not use this contract. Input is
   parsed as RFC 3339, so any offset form is accepted and converted; an
   unparseable timestamp is an error, and an absent one becomes the zero time
   `0001-01-01T00:00:00Z`.
5. **Numbers: decimal integers only.** No exponent, no fraction, no leading
   zeros, no leading `+`. `-0` normalizes to `0`. `1e2` and `1.0` are rejected
   rather than rounded, as is anything outside `int64`.
6. **Strings: valid UTF-8, minimal escaping.** Only `"`, `\` and C0 controls are
   escaped; C0 uses the short forms `\b \f \n \r \t` where they exist and
   lowercase `\u00xx` otherwise. `U+007F`, `U+2028`, `U+2029` and all multi-byte
   sequences are emitted raw.

Unknown fields (those a newer additive minor introduced, per the tolerant-reader
rule below) are **preserved** and canonically re-emitted. Their omitted defaults
are unknowable to an older reader, so their values pass through unchanged apart
from key ordering, string escaping, and number syntax — which is what lets an
older verifier still check a signature over a newer producer's artifact.

`CanonicalUnmarshal` canonicalizes and then tolerantly decodes, so the resulting
value's `GeneratedAt` is already UTC-truncated and `CanonicalMarshal` of it
reproduces the same bytes — except when the input carried unknown fields, which
the typed decode necessarily drops. Verify signatures against `Canonicalize`
output, not against a re-marshaled struct.

## Versioning rules

`brain_schema_version` is a `major.minor` string, governed by the ADR-0001
schema-compatibility policy (mirrored in `docs/semantic_brain_plan.md`, "Schema
compatibility policy"). The local version is
`brainwire.BrainSchemaVersion` (currently `"1.0"`).

- **Major is the compatibility boundary.** A reader supports exactly one major
  and refuses any other.
- **Minors are additive-only.** A higher minor within the same major adds
  fields but removes or renames nothing.
- **Breaking changes require a major bump.** Removing/renaming a field, changing
  a field's type or meaning, or tightening a key is a major change.

`CheckCompatibility(remote string) (ok bool, warn string, err error)`
(`internal/brainwire/version.go`) implements the rules. Call it on
`Manifest.BrainSchemaVersion`:

| Remote vs local                         | Result                                    |
|-----------------------------------------|-------------------------------------------|
| Same major, remote minor ≤ local        | `ok=true`, no warning                     |
| Same major, remote minor > local        | `ok=true`, warning: newer additive fields will be ignored |
| Different major (greater **or** unknown)| hard error, `ok=false`                    |
| Unparseable (`""`, `"1"`, `"1.2.3"`, `"1.x"`, `"-1.0"`) | hard error, `ok=false`     |

Callers must hard-fail on a non-nil `err`, surface a non-empty `warn` to the
operator, and otherwise proceed.

## Tolerant-reader requirement

The additive-minor guarantee only holds if decoders are **tolerant**: they must
ignore fields they do not recognize rather than reject them. Concretely:

> **Decoders MUST NOT enable `json.Decoder.DisallowUnknownFields`.**

A plain `json.Unmarshal` (or a `json.Decoder` left at its default) drops unknown
fields, so an older reader can consume an artifact produced by a newer minor of
the same major, keeping the fields it understands and ignoring the rest. This is
what makes a minor bump non-breaking. `CheckCompatibility` pairs with this: it
returns a warning (not an error) for a newer minor precisely because the reader
will silently skip the newer fields, and the operator should know that some data
may not have been consumed.

The behavior is proven by `TestTolerantReaderIgnoresUnknownFields`, which decodes
a payload carrying unknown fields at the manifest, artifact, and reference levels
on a `1.0` reader.

## Constructing an artifact

The store composes a `BrainArtifact` from the on-disk manifests:

```go
art := brainwire.NewBrainArtifact(repoKey, defaultBranch, generatedAt).
    SetProvider(sem.Provider, sem.ProviderVersion, sem.SchemaVersion).
    AddSnapshot(brainwire.SnapshotRef{Commit: c, Content: ref}).
    AddOverlay(brainwire.OverlayRef{BaseCommit: b, HeadCommit: h, Content: ref}).
    AddFacts(brainwire.FactsRef{Branch: br, Content: ref})
```

`NewBrainArtifact` stamps the local `BrainSchemaVersion` onto the manifest. The
identity fields map directly from `exportManifest` (`repo_key`,
`default_branch`, `generated_at`) and the provider fields from
`semanticSourceManifest` (`Provider`, `ProviderVersion`, `SchemaVersion`). The
content digests are computed by the store when it serializes each payload; this
milestone defines the contract, not the store.

## Tests

`internal/brainwire/contract_test.go`:

- **Round-trip** — a representative artifact exercising every type survives
  `json.Marshal → Unmarshal` with `reflect.DeepEqual` and byte-stable
  re-marshaling.
- **`CheckCompatibility`** — table covering same-major ok, unknown/greater-major
  error, higher-minor ok+warn, and malformed versions.
- **Tolerant reader** — a payload with unknown additive fields decodes without
  error and preserves known fields.
- **Local version invariant** — `BrainSchemaVersion` is itself valid
  `major.minor` and self-compatible.

`internal/brainwire/canonical_test.go`, one test per canonical-encoding rule:

- **Golden fixture** — the exact canonical bytes of the representative
  artifact, written by hand rather than captured from the encoder.
- **Key order** — lexicographic, and explicitly *not* Go declaration order.
- **Idempotence** — `Canonicalize(Canonicalize(x)) == Canonicalize(x)` for
  canonical, Go-default, non-canonical and empty input.
- **Order independence** — two field orders of the same artifact produce
  identical bytes.
- **Omitted defaults** — explicit `"size":0` / `"tree":""` / `null` / `[]` are
  normalized away; required fields are zero-filled; the field table is checked
  against the struct tags by reflection.
- **Escaping** — HTML characters stay raw, `"` and `\` and C0 controls escape
  minimally.
- **Time** — sub-second, `+00:00` and non-zero offsets all collapse to the same
  whole-second `Z` form.
- **Numbers** — exponents, fractions and overflow rejected; `-0` normalized.
- **Unknown fields** — preserved and canonically re-emitted.
- **Errors** — malformed JSON, trailing data, wrong JSON types, unparseable
  timestamps.
