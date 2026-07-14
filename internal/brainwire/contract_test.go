package brainwire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// representativeArtifact builds a BrainArtifact that exercises every wire type
// and both provider and reference fields. GeneratedAt is parsed (not
// constructed with time.Date) so it uses the exact internal representation that
// json decoding produces, keeping reflect.DeepEqual reliable after a round trip.
func representativeArtifact(t *testing.T) *BrainArtifact {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-06-01T12:00:00Z")
	if err != nil {
		t.Fatalf("parse timestamp: %v", err)
	}
	return NewBrainArtifact("gh/org/repo", "main", ts).
		SetProvider("entire-graph", "0.1.0", "1.1").
		AddSnapshot(SnapshotRef{
			Commit: "abc123",
			Tree:   "tree789",
			Content: ContentRef{
				Digest:    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Size:      2048,
				MediaType: "application/json",
				Path:      "semantic/snapshots/abc123/snapshot.json",
			},
		}).
		AddOverlay(OverlayRef{
			BaseCommit: "abc123",
			HeadCommit: "def456",
			Branch:     "feature-x",
			Content: ContentRef{
				Digest:    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				Size:      512,
				MediaType: "application/json",
				Path:      "semantic/overlays/abc123..def456.json",
			},
		}).
		AddFacts(FactsRef{
			Branch: "main",
			Content: ContentRef{
				Digest:    "sha256:3333333333333333333333333333333333333333333333333333333333333333",
				Size:      4096,
				MediaType: "application/x-ndjson",
				Path:      "facts/main/facts.ndjson",
			},
		})
}

func TestBrainArtifactRoundTrip(t *testing.T) {
	original := representativeArtifact(t)

	b1, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}

	var decoded BrainArtifact
	if err := json.Unmarshal(b1, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// (a) deep-equality: the decoded artifact matches the original exactly.
	if !reflect.DeepEqual(*original, decoded) {
		t.Fatalf("round-trip changed the artifact:\n original=%#v\n decoded=%#v", *original, decoded)
	}

	// (a) byte-stability: re-marshaling the decoded artifact reproduces the
	// exact original bytes (declaration-ordered fields, ordered slices, no maps).
	b2, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal decoded: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("marshal is not byte-stable:\n first=%s\nsecond=%s", b1, b2)
	}

	// The stamped version must be the local wire schema version.
	if decoded.Manifest.BrainSchemaVersion != BrainSchemaVersion {
		t.Fatalf("brain_schema_version = %q, want %q", decoded.Manifest.BrainSchemaVersion, BrainSchemaVersion)
	}
}

func TestCheckCompatibility(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		wantOK   bool
		wantWarn bool // whether warn should be non-empty
		wantErr  bool
	}{
		{name: "same version", remote: "1.0", wantOK: true},
		{name: "same major lower-or-equal minor", remote: "1.0", wantOK: true},
		{name: "same major higher minor warns", remote: "1.7", wantOK: true, wantWarn: true},
		{name: "unknown greater major errors", remote: "2.0", wantErr: true},
		{name: "unknown lesser major errors", remote: "0.9", wantErr: true},
		{name: "missing minor errors", remote: "1", wantErr: true},
		{name: "too many components errors", remote: "1.0.0", wantErr: true},
		{name: "non-numeric errors", remote: "1.x", wantErr: true},
		{name: "empty errors", remote: "", wantErr: true},
		{name: "negative errors", remote: "-1.0", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, warn, err := CheckCompatibility(tc.remote)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CheckCompatibility(%q) = (ok=%v, warn=%q, err=nil), want error", tc.remote, ok, warn)
				}
				if ok {
					t.Fatalf("CheckCompatibility(%q): ok must be false on error", tc.remote)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckCompatibility(%q) unexpected error: %v", tc.remote, err)
			}
			if ok != tc.wantOK {
				t.Fatalf("CheckCompatibility(%q) ok = %v, want %v", tc.remote, ok, tc.wantOK)
			}
			if gotWarn := warn != ""; gotWarn != tc.wantWarn {
				t.Fatalf("CheckCompatibility(%q) warn=%q; hasWarn=%v, want %v", tc.remote, warn, gotWarn, tc.wantWarn)
			}
		})
	}
}

// TestTolerantReaderIgnoresUnknownFields proves a reader on schema 1.0 decodes an
// artifact produced by a hypothetical newer minor that added fields at every
// level. This is the tolerant-reader guarantee: a plain json.Unmarshal (no
// DisallowUnknownFields) drops unknown fields instead of failing.
func TestTolerantReaderIgnoresUnknownFields(t *testing.T) {
	// A superset payload: valid 1.0 fields plus unknown additive fields a
	// future 1.x minor might introduce, at the manifest, artifact, and
	// reference levels.
	payload := `{
		"manifest": {
			"repo_key": "gh/org/repo",
			"default_branch": "main",
			"generated_at": "2026-06-01T12:00:00Z",
			"brain_schema_version": "1.4",
			"provider": "entire-graph",
			"provider_version": "0.2.0",
			"provider_schema_version": "1.2",
			"future_manifest_field": {"nested": ["anything", 1, true]}
		},
		"snapshots": [
			{
				"commit": "abc123",
				"tree": "tree789",
				"content": {
					"digest": "sha256:1111",
					"size": 10,
					"media_type": "application/json",
					"future_content_field": "ignored"
				},
				"future_snapshot_field": 42
			}
		],
		"facts": [
			{"branch": "main", "content": {"digest": "sha256:3333"}}
		],
		"future_top_level_field": ["ignored"]
	}`

	var decoded BrainArtifact
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("tolerant decode failed: %v", err)
	}

	// Known fields survived.
	if decoded.Manifest.RepoKey != "gh/org/repo" {
		t.Fatalf("repo_key = %q, want gh/org/repo", decoded.Manifest.RepoKey)
	}
	if decoded.Manifest.BrainSchemaVersion != "1.4" {
		t.Fatalf("brain_schema_version = %q, want 1.4", decoded.Manifest.BrainSchemaVersion)
	}
	if decoded.Manifest.ProviderSchemaVersion != "1.2" {
		t.Fatalf("provider_schema_version = %q, want 1.2", decoded.Manifest.ProviderSchemaVersion)
	}
	if len(decoded.Snapshots) != 1 || decoded.Snapshots[0].Commit != "abc123" {
		t.Fatalf("snapshots not decoded: %#v", decoded.Snapshots)
	}
	if decoded.Snapshots[0].Content.Digest != "sha256:1111" {
		t.Fatalf("snapshot content digest = %q", decoded.Snapshots[0].Content.Digest)
	}
	if len(decoded.Facts) != 1 || decoded.Facts[0].Branch != "main" {
		t.Fatalf("facts not decoded: %#v", decoded.Facts)
	}

	// And such a newer-minor artifact is compatible with a warning.
	ok, warn, err := CheckCompatibility(decoded.Manifest.BrainSchemaVersion)
	if err != nil || !ok {
		t.Fatalf("CheckCompatibility(1.4) = (ok=%v, err=%v), want ok, no error", ok, err)
	}
	if !strings.Contains(warn, "tolerant reader") {
		t.Fatalf("expected tolerant-reader warning, got %q", warn)
	}
}

// TestLocalSchemaVersionParses guards the package invariant that the local
// BrainSchemaVersion constant is itself a valid "major.minor" version.
func TestLocalSchemaVersionParses(t *testing.T) {
	if _, _, err := parseSchemaVersion(BrainSchemaVersion); err != nil {
		t.Fatalf("BrainSchemaVersion %q is not valid major.minor: %v", BrainSchemaVersion, err)
	}
	ok, warn, err := CheckCompatibility(BrainSchemaVersion)
	if err != nil || !ok || warn != "" {
		t.Fatalf("CheckCompatibility(local) = (ok=%v, warn=%q, err=%v), want (true, \"\", nil)", ok, warn, err)
	}
}
