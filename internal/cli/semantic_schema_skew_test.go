package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestValidateSemanticSchemaFollowsTheTolerantReaderContract pins ADR 0001's
// tolerant-reader rules: accept any supported-major minor, refuse an unknown
// major, and refuse a version that is not a well-formed major.minor at all.
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
		err := validateSemanticSchema(tc.version)
		if tc.wantErr && err == nil {
			t.Fatalf("validateSemanticSchema(%q) = nil, want an error", tc.version)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("validateSemanticSchema(%q) = %v, want nil", tc.version, err)
		}
	}
}

// TestSemanticSchemaErrorsNameTheSupportedRange keeps the operator-facing text
// useful: a rejection must say what this build DOES support, not only what it
// received.
func TestSemanticSchemaErrorsNameTheSupportedRange(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.0", "", "1.abc"} {
		err := validateSemanticSchema(version)
		if err == nil {
			t.Fatalf("validateSemanticSchema(%q) = nil, want an error", version)
		}
		if !strings.Contains(err.Error(), semanticSupportedSchemaRange) {
			t.Fatalf("error for %q does not name the supported range %q: %v", version, semanticSupportedSchemaRange, err)
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
		{version: semanticSchemaVersion},
		{version: "1.99", wantWarn: true},
	} {
		warning := semanticSchemaMinorSkewWarning(tc.version)
		if tc.wantWarn && warning == nil {
			t.Fatalf("schema %q produced no newer-minor warning", tc.version)
		}
		if !tc.wantWarn && warning != nil {
			t.Fatalf("schema %q produced an unexpected warning: %+v", tc.version, warning)
		}
		if warning == nil {
			continue
		}
		for _, want := range []string{tc.version, semanticSchemaVersion} {
			if !strings.Contains(warning.Detail, want) {
				t.Fatalf("newer-minor warning does not name %q: %+v", want, warning)
			}
		}
	}
}

// TestSemanticIndexRecordsNewerMinorWarning proves the warning actually reaches
// the recorded index rather than being computed and dropped.
func TestSemanticIndexRecordsNewerMinorWarning(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.99"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test", Env: env, Runner: runner, Now: time.Now,
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("a newer supported-major minor must be accepted: %v", err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		t.Fatal("manifest has no semantic source")
	}
	found := false
	for _, warning := range manifest.Sources.Semantic.Warnings {
		if warning.Code == "provider_schema_newer_minor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("index did not record a provider_schema_newer_minor warning: %+v", manifest.Sources.Semantic.Warnings)
	}
}
