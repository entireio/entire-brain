// Package brainwire defines the versioned, transport-agnostic wire contract for
// the composite Entire Brain artifact.
//
// It is the schema a server-side brain store (milestone M1.2) serializes and
// transports. It is deliberately a NEW, parallel type that mirrors — but does
// not replace — the on-disk model in package internal/cli (exportManifest,
// brainSources, semanticSourceManifest, and the per-source manifests). The
// on-disk types stay the source of truth for local generation; brainwire
// promotes the subset that crosses a transport boundary into an explicit,
// stable contract.
//
// Design invariants:
//
//   - Every field carries an explicit, stable JSON tag. Tags are the contract;
//     Go field names may be refactored, JSON tags may not (except by a schema
//     version bump per BrainSchemaVersion / ADR-0001).
//   - Content is addressed, not embedded. Large payloads (semantic snapshots,
//     branch overlays, durable-fact streams) are referenced by a
//     content-addressed digest plus their key. The store transports the bytes
//     out of band; this artifact carries only the manifest and the references.
//   - Readers MUST be tolerant. Decoders MUST NOT set DisallowUnknownFields, so
//     a newer minor schema that adds fields still decodes on an older reader.
//     See CheckCompatibility and version.go.
package brainwire

import "time"

// BrainArtifact is the wire representation of one repository's composite brain:
// an identity/version manifest plus content-addressed references to the heavy,
// separately-transported payloads.
//
// The content addressing mirrors the on-disk brain layout documented in
// docs/semantic_brain_plan.md ("Brain Layout"):
//
//	semantic/snapshots/<commit>/      -> Snapshots, keyed by Commit
//	semantic/overlays/<base>..<head>  -> Overlays, keyed by (BaseCommit, HeadCommit)
//	facts/<branch>/facts.ndjson       -> Facts, keyed by Branch
//
// The reference slices are kept in a caller-controlled, stable order so that
// re-marshaling a decoded BrainArtifact is byte-identical to the original.
type BrainArtifact struct {
	Manifest  BrainManifest `json:"manifest"`
	Snapshots []SnapshotRef `json:"snapshots,omitempty"`
	Overlays  []OverlayRef  `json:"overlays,omitempty"`
	Facts     []FactsRef    `json:"facts,omitempty"`
}

// BrainManifest is the identity and versioning header for a BrainArtifact.
//
// It mirrors the load-bearing header fields of internal/cli.exportManifest
// (RepoKey, DefaultBranch, GeneratedAt) and the entire-sem provider identity
// from internal/cli.semanticSourceManifest (Provider, ProviderVersion,
// ProviderSchemaVersion). BrainSchemaVersion is the version of THIS wire
// contract, distinct from the provider's own schema version.
type BrainManifest struct {
	// RepoKey is the deterministic repository identity (e.g. "gh/org/repo").
	RepoKey string `json:"repo_key"`
	// DefaultBranch is the repository's default branch name, when known.
	DefaultBranch string `json:"default_branch,omitempty"`
	// GeneratedAt is when the underlying brain state was produced.
	GeneratedAt time.Time `json:"generated_at"`
	// BrainSchemaVersion is the wire schema version, "major.minor" per
	// ADR-0001. Compare it against the local BrainSchemaVersion with
	// CheckCompatibility before trusting the artifact.
	BrainSchemaVersion string `json:"brain_schema_version"`

	// Provider is the semantic provider name that produced the semantic
	// snapshots (e.g. "entire-sem").
	Provider string `json:"provider,omitempty"`
	// ProviderVersion is the semantic provider's release version.
	ProviderVersion string `json:"provider_version,omitempty"`
	// ProviderSchemaVersion is the provider's own "major.minor" contract
	// version (semanticSourceManifest.SchemaVersion). It is independent of
	// BrainSchemaVersion.
	ProviderSchemaVersion string `json:"provider_schema_version,omitempty"`
}

// ContentRef is a content-addressed pointer to an opaque blob that the brain
// store serializes and transports separately from this artifact.
//
// Digest is the content address and is authoritative: a reader that already
// holds a blob with the same digest need not re-fetch it. Path is an advisory,
// artifact-relative hint about where the bytes lived on disk and MUST NOT be
// used as identity.
type ContentRef struct {
	// Digest is an algorithm-prefixed content hash of the blob, e.g.
	// "sha256:9f86d0...". It is the canonical address of the bytes.
	Digest string `json:"digest"`
	// Size is the blob length in bytes, when known.
	Size int64 `json:"size,omitempty"`
	// MediaType is the blob's media type, e.g. "application/json" for a
	// semantic snapshot or "application/x-ndjson" for a facts stream.
	MediaType string `json:"media_type,omitempty"`
	// Path is the artifact-relative source path the bytes came from
	// (advisory only; not identity).
	Path string `json:"path,omitempty"`
}

// SnapshotRef is a commit-addressed semantic snapshot. Commit is the git commit
// the snapshot indexes; it is the key (on disk: semantic/snapshots/<commit>/).
type SnapshotRef struct {
	// Commit is the git commit the snapshot indexes.
	Commit string `json:"commit"`
	// Tree is the git tree object the snapshot indexes, when recorded.
	Tree string `json:"tree,omitempty"`
	// Content addresses the snapshot payload.
	Content ContentRef `json:"content"`
}

// OverlayRef is a branch overlay addressed by the (BaseCommit, HeadCommit) pair
// rather than by branch name, so rebases and force-pushes do not silently
// change its meaning (on disk: semantic/overlays/<base>..<head>.json).
type OverlayRef struct {
	// BaseCommit is the overlay's base git commit.
	BaseCommit string `json:"base_commit"`
	// HeadCommit is the overlay's head git commit.
	HeadCommit string `json:"head_commit"`
	// Branch is the branch this overlay was generated for, advisory only;
	// the (BaseCommit, HeadCommit) pair is the key.
	Branch string `json:"branch,omitempty"`
	// Content addresses the overlay payload.
	Content ContentRef `json:"content"`
}

// FactsRef is a branch-scoped durable-facts stream, keyed by Branch (on disk:
// facts/<branch>/facts.ndjson).
type FactsRef struct {
	// Branch is the branch whose facts stream this reference addresses.
	Branch string `json:"branch"`
	// Content addresses the NDJSON facts payload.
	Content ContentRef `json:"content"`
}

// NewBrainArtifact returns a BrainArtifact stamped with the local
// BrainSchemaVersion and the given identity fields. The store (M1.2) composes
// the full artifact by reading the on-disk exportManifest header (repo_key,
// default_branch, generated_at) and the semantic source manifest, then adding
// references with SetProvider / AddSnapshot / AddOverlay / AddFacts.
func NewBrainArtifact(repoKey, defaultBranch string, generatedAt time.Time) *BrainArtifact {
	return &BrainArtifact{
		Manifest: BrainManifest{
			RepoKey:            repoKey,
			DefaultBranch:      defaultBranch,
			GeneratedAt:        generatedAt,
			BrainSchemaVersion: BrainSchemaVersion,
		},
	}
}

// SetProvider records the entire-sem provider identity on the manifest, mirroring
// internal/cli.semanticSourceManifest's Provider, ProviderVersion, and
// SchemaVersion. It returns the receiver for chaining.
func (a *BrainArtifact) SetProvider(name, version, schemaVersion string) *BrainArtifact {
	a.Manifest.Provider = name
	a.Manifest.ProviderVersion = version
	a.Manifest.ProviderSchemaVersion = schemaVersion
	return a
}

// AddSnapshot appends a commit-addressed snapshot reference and returns the
// receiver for chaining.
func (a *BrainArtifact) AddSnapshot(ref SnapshotRef) *BrainArtifact {
	a.Snapshots = append(a.Snapshots, ref)
	return a
}

// AddOverlay appends a (base, head)-addressed overlay reference and returns the
// receiver for chaining.
func (a *BrainArtifact) AddOverlay(ref OverlayRef) *BrainArtifact {
	a.Overlays = append(a.Overlays, ref)
	return a
}

// AddFacts appends a branch-scoped facts reference and returns the receiver for
// chaining.
func (a *BrainArtifact) AddFacts(ref FactsRef) *BrainArtifact {
	a.Facts = append(a.Facts, ref)
	return a
}
