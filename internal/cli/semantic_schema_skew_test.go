package cli

import (
	"path/filepath"
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

// TestSemanticStaleReportDegradesOnNewerMinor pins the second sink for the
// same rule. Recording the warning at index time is not enough on its own: the
// "provider" freshness axis is what `entire brain doctor`/`status` and every
// semantic read command consult, and an index built from a snapshot this build
// only partly understands must read as degraded there rather than a clean "ok".
func TestSemanticStaleReportDegradesOnNewerMinor(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	// The manifest's recorded schema_version and the on-disk snapshot must
	// agree (every freshness check revalidates the snapshot against the
	// manifest), so index a real 1.99 provider snapshot rather than patching
	// the manifest alone.
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.99"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test", Env: env, Runner: runner, Now: time.Now,
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	provider := report.Axes["provider"]
	if provider.State != "degraded" {
		t.Fatalf("provider axis = %+v, want degraded for a newer provider minor", provider)
	}
	for _, want := range []string{"1.99", semanticSchemaVersion} {
		if !strings.Contains(provider.Detail, want) {
			t.Fatalf("provider axis detail does not name %q: %+v", want, provider)
		}
	}
	if report.Severity != "degraded" {
		t.Fatalf("severity = %s, want degraded", report.Severity)
	}
}

// TestSemanticStaleReportSilentAtSupportedMinor is the negative case for that
// axis: at the newest minor this build fully ingests, nothing about schema skew
// may appear in the provider axis.
func TestSemanticStaleReportSilentAtSupportedMinor(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test", Env: env, Runner: runner, Now: time.Now,
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if detail := report.Axes["provider"].Detail; strings.Contains(detail, "minors are additive") {
		t.Fatalf("provider axis reports schema skew at the supported minor: %q", detail)
	}
}

// TestBundleImportRecordsNewerMinorWarning pins the third sink. A bundle
// carries a snapshot this build ingests only partially in exactly the same way
// a live provider does, so the import must succeed (tolerant reader) and leave
// the warning on the semantic source it persists, rather than accepting the
// bundle with no trace.
func TestBundleImportRecordsNewerMinorWarning(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.99"))
	archive := filepath.Join(t.TempDir(), "newer-minor.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.99","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.99"),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{
		Version: "test", Env: env, Runner: runner, Now: time.Now,
	}, archive, bundleSHA256(t, archive), false); err != nil {
		t.Fatalf("a newer supported-major minor must import: %v", err)
	}
	source := mustSemanticSource(t, env)
	found := false
	for _, warning := range source.Warnings {
		if warning.Code == "provider_schema_newer_minor" {
			found = true
			if !strings.Contains(warning.Detail, "1.99") {
				t.Fatalf("imported warning does not name the remote version: %+v", warning)
			}
		}
	}
	if !found {
		t.Fatalf("bundle import did not record a provider_schema_newer_minor warning: %+v", source.Warnings)
	}
}

// TestBundleImportSilentAtSupportedMinor is the negative case for that sink.
func TestBundleImportSilentAtSupportedMinor(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))
	archive := filepath.Join(t.TempDir(), "supported-minor.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"` + semanticSchemaVersion + `","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot(semanticSchemaVersion),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{
		Version: "test", Env: env, Runner: runner, Now: time.Now,
	}, archive, bundleSHA256(t, archive), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, warning := range mustSemanticSource(t, env).Warnings {
		if warning.Code == "provider_schema_newer_minor" {
			t.Fatalf("unexpected schema-skew warning at the supported minor: %+v", warning)
		}
	}
}
