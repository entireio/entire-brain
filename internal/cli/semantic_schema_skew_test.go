package cli

import (
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"strings"
	"testing"
)

// TestValidateSemanticSchemaFollowsTheTolerantReaderContract pins ADR 0001's
// tolerant-reader rules: accept any supported-major minor (including an
// additive patch component), refuse an unknown major, and refuse a version
// whose major/minor components are not well formed.
func TestValidateSemanticSchemaFollowsTheTolerantReaderContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{version: "1.0"},
		{version: "1.1"},
		{version: "1.2"},
		{version: "1.17"},
		{version: "1.2.3"},
		{version: "", wantErr: true},
		{version: "1", wantErr: true},
		{version: "2.0", wantErr: true},
		{version: "0.9", wantErr: true},
		// A minor that is not a number is not an additive minor: the contract
		// says major.minor, and silently accepting anything after the dot means
		// a malformed header rides straight into the store.
		{version: "1.abc", wantErr: true},
		{version: "1.", wantErr: true},
		{version: "x.1", wantErr: true},
	} {
		err := semstore.ValidateSchema(tc.version)
		if tc.wantErr && err == nil {
			t.Fatalf("semstore.ValidateSchema(%q) = nil, want an error", tc.version)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("semstore.ValidateSchema(%q) = %v, want nil", tc.version, err)
		}
	}
}

// TestSemanticSchemaErrorsNameTheSupportedRange keeps the operator-facing text
// useful: a rejection must say what this build DOES support, not only what it
// received.
func TestSemanticSchemaErrorsNameTheSupportedRange(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.0", "", "1.abc"} {
		err := semstore.ValidateSchema(version)
		if err == nil {
			t.Fatalf("semstore.ValidateSchema(%q) = nil, want an error", version)
		}
		if !strings.Contains(err.Error(), semstore.SupportedSchemaRange) {
			t.Fatalf("error for %q does not name the supported range %q: %v", version, semstore.SupportedSchemaRange, err)
		}
	}
}

// TestSemanticSchemaNewerMinorWarns is ADR 0001 rule 3: a newer supported-major
// minor is accepted, but the consumer must WARN, because additive facts it does
// not know about may have been skipped. Silently accepting is not compliance.
func TestSemanticSchemaNewerMinorWarns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version  string
		wantWarn bool
	}{
		{version: "1.0"},
		{version: semstore.SchemaVersion},
		{version: "1.99", wantWarn: true},
	} {
		warning := semstore.SchemaMinorSkewWarning(tc.version)
		if tc.wantWarn && warning == nil {
			t.Fatalf("schema %q produced no newer-minor warning", tc.version)
		}
		if !tc.wantWarn && warning != nil {
			t.Fatalf("schema %q produced an unexpected warning: %+v", tc.version, warning)
		}
		if warning == nil {
			continue
		}
		for _, want := range []string{tc.version, semstore.SchemaVersion} {
			if !strings.Contains(warning.Detail, want) {
				t.Fatalf("newer-minor warning does not name %q: %+v", want, warning)
			}
		}
	}
}

// semanticSchemaNewerMinor is the first minor after what this build reads.
// Derived, not a literal: a hardcoded version stops meaning "newer than
// supported" the moment semstore.SchemaVersion is bumped past it, and silently
// starts testing the wrong thing instead of failing (the COR-2007 bump turned
// the hardcoded 1.4 fixtures stale exactly this way).
func semanticSchemaNewerMinor(t *testing.T) string {
	t.Helper()
	major, minor, err := semstore.ParseSchemaVersion(semstore.SchemaVersion)
	if err != nil {
		t.Fatalf("parse semstore.SchemaVersion %q: %v", semstore.SchemaVersion, err)
	}
	return fmt.Sprintf("%d.%d", major, minor+1)
}

// TestCurrentProviderSchemaDoesNotWarn pins COR-2007 and its 1.4 recurrence.
// entire-graph shipped schema 1.3 on 2026-09-13 while this build advertised
// 1.1, then shipped 1.4 (graph v0.5.0) while this build advertised 1.3 —
// each time EVERY index against a current provider raised
// provider_schema_newer_minor and told the user additive facts "may not have
// been ingested". They were ingested: 1.2's identity_revision is consumed by
// internal/entityindex, 1.3's completeness.health rides through as the
// Completeness raw message, and 1.4's provenance is modeled on semanticRecord
// (and is a pure derivation of type+resolution besides).
//
// The warning is still the right mechanism, so this also pins that a genuinely
// newer minor keeps firing it. Bumping the constant without teaching the reader
// the new fields would turn a true warning into a silent drop.
func TestCurrentProviderSchemaDoesNotWarn(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1.0", "1.1", "1.2", "1.3", "1.4"} {
		if w := semstore.SchemaMinorSkewWarning(version); w != nil {
			t.Fatalf("schema %q warned (%s: %s); this build ingests it in full",
				version, w.Code, w.Effect)
		}
	}
	for _, version := range []string{semanticSchemaNewerMinor(t), "1.17"} {
		w := semstore.SchemaMinorSkewWarning(version)
		if w == nil {
			t.Fatalf("schema %q did not warn; a minor this build has not been taught must warn", version)
		}
		if !strings.Contains(w.Detail, semstore.SchemaVersion) {
			t.Fatalf("warning for %q must name what this build reads (%s), got: %s",
				version, semstore.SchemaVersion, w.Detail)
		}
	}
	// A new major is a refusal, not a warning: that boundary is unchanged.
	if err := semstore.ValidateSchema("2.0"); err == nil {
		t.Fatal("schema 2.0 accepted; a new major must still be refused")
	}
}

// TestPartialFailureLanguageSurvivesDecoding pins the one field schema 1.3
// added that this build was dropping. The provider sets language when a source
// fails BEFORE any file record exists, which is exactly when file_path is
// empty -- so dropping it left the failure attributable to nothing at all.
func TestPartialFailureLanguageSurvivesDecoding(t *testing.T) {
	t.Parallel()
	var w semanticWarning
	raw := `{"code":"parse_failed","severity":"warning","language":"rust",` +
		`"effect_on_semantic_completeness":"rust symbols absent"}`
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.Language != "rust" {
		t.Fatalf("language dropped: got %q, want \"rust\"", w.Language)
	}
	// The case that motivates the field: no file_path to fall back on.
	if w.Path != "" {
		t.Fatalf("path should be empty for a pre-file-record failure, got %q", w.Path)
	}
	if w.Effect != "rust symbols absent" {
		t.Fatalf("provider effect alias regressed: %q", w.Effect)
	}
}

// TestRelationProvenanceSurvivesRemarshal pins the one field schema 1.4 added.
// The provider derives provenance from type and resolution at marshal time, so
// no information was lost — but the streaming filter re-marshals every record
// through semanticRecord, and an unmodeled field is silently stripped from the
// stored snapshot, breaking the constant's "ingest in full" claim literally.
func TestRelationProvenanceSurvivesRemarshal(t *testing.T) {
	t.Parallel()
	raw := `{"record_type":"relation","from_id":"a","to_id":"b","type":"CALLS",` +
		`"resolution":"exact","provenance":"EXTRACTED"}`
	var record semanticRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if record.Provenance != "EXTRACTED" {
		t.Fatalf("provenance dropped on decode: got %q", record.Provenance)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"provenance":"EXTRACTED"`) {
		t.Fatalf("provenance dropped on re-marshal: %s", encoded)
	}
}
