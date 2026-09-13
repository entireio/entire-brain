package cli

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// bundleTestBrainDir is where semanticTestEnv's fixtures put the brain.
func bundleTestBrainDir(env EntireEnv) string {
	return filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
}

func writeBundleTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedBundleTestLayers gives an indexed brain one file and one manifest source
// per non-semantic layer, so a bundle round-trip has something to lose.
func seedBundleTestLayers(t *testing.T, brainDir string) {
	t.Helper()
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	writeBundleTestFile(t, filepath.Join(brainDir, seedDirName, "repo-overview.md"), "# overview\n")
	writeBundleTestFile(t, filepath.Join(brainDir, docDirName, docIndexFileName), `{"generated_at":"2026-02-03T04:05:06Z","records":[]}`)
	writeBundleTestFile(t, filepath.Join(brainDir, factsDirName, "taxonomy.json"), `{"version":1}`)
	writeBundleTestFile(t, filepath.Join(brainDir, factsDirName, "main-0000", factsFileName), `{"id":"fact:abc","kind":"decision","statement":"authored fact that must survive a round trip"}`+"\n")
	writeBundleTestFile(t, filepath.Join(brainDir, exportSessionsDirectory, "main", "session.jsonl"), `{"role":"user","text":"verbatim conversation content"}`+"\n")
	writeBundleTestFile(t, filepath.Join(brainDir, historyDirName, "generations", "gen1", historyIndexFileName), `{"records":[]}`)
	writeBundleTestFile(t, filepath.Join(brainDir, brainPatternsDirName, "episodes.ndjson"), `{"id":"episode:1"}`+"\n")

	// Privacy policy and operational state: present in the brain, and never in
	// a bundle.
	writeBundleTestFile(t, filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath)), `{"version":1,"excluded":{"session:secret":{"at":"2026-02-03T04:05:06Z"}}}`)
	writeBundleTestFile(t, filepath.Join(brainDir, historyDirName, "privacy", "session%3Asecret.json"), `{"schema_version":1,"session_id":"session:secret"}`)
	writeBundleTestFile(t, filepath.Join(brainDir, historyDirName, "work", "v1", "worker.log"), "local worker log\n")

	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Seed = &seedSourceManifest{GeneratedAt: now, SummaryPath: seedDirName + "/repo-overview.md", WorktreeMode: "head"}
	manifest.Sources.Docs = &docSourceManifest{GeneratedAt: now, IndexPath: docDirName + "/" + docIndexFileName, Records: 0, Files: 1}
	manifest.Sources.Facts = &factSourceManifest{GeneratedAt: now, TaxonomyPath: factsDirName + "/taxonomy.json", Branches: []string{"main"}, Facts: 1, Authored: 1}
	manifest.Sources.History = &historySourceManifest{GeneratedAt: now, IndexPath: historyDirName + "/generations/gen1/" + historyIndexFileName}
	manifest.Sources.Patterns = &patternSourceManifest{GeneratedAt: now, EpisodesPath: brainPatternsDirName + "/episodes.ndjson", Episodes: 1}
	manifest.Sources.Sessions = &sessionSourceManifest{GeneratedAt: now, TranscriptMode: "full", Scope: "repo"}
	manifest.Branches = []exportBranch{{Branch: "main", Directory: exportSessionsDirectory + "/main", SessionCount: 1, Default: true}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func bundleTestEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		names = append(names, header.Name)
	}
	return names
}

func bundleTestManifest(t *testing.T, path string) exportManifest {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		if header.Name != exportManifestFileName {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read bundle manifest: %v", err)
		}
		var manifest exportManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("parse bundle manifest: %v", err)
		}
		return manifest
	}
	t.Fatalf("bundle has no %s", exportManifestFileName)
	return exportManifest{}
}

func bundleTestIndexedBrain(t *testing.T) (EntireEnv, Options, *cobra.Command, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))
	opts := Options{Version: "9.9.9", Env: env, Runner: runner, Now: func() time.Time {
		return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	}}
	cmd := &cobra.Command{Use: "bundle"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := bundleTestBrainDir(env)
	seedBundleTestLayers(t, brainDir)
	return env, opts, cmd, repoDir, brainDir
}

// TestBundleExportCarriesEveryDefaultLayer is the data-loss regression. The
// exporter hardcoded Sources: &brainSources{Semantic: …} and wrote only the
// semantic subtree, so a bundle carried one layer of seven and a round-trip
// into a fresh store destroyed every authored fact without a word.
func TestBundleExportCarriesEveryDefaultLayer(t *testing.T) {
	_, opts, cmd, _, _ := bundleTestIndexedBrain(t)
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	entries := bundleTestEntries(t, output)
	joined := strings.Join(entries, "\n")
	for _, want := range []string{
		"seed/repo-overview.md",
		"docs/" + docIndexFileName,
		"facts/taxonomy.json",
		"facts/main-0000/" + factsFileName,
	} {
		if !slicesContainsString(entries, want) {
			t.Fatalf("default bundle is missing %s; entries:\n%s", want, joined)
		}
	}
	manifest := bundleTestManifest(t, output)
	if manifest.Sources == nil || manifest.Sources.Facts == nil || manifest.Sources.Facts.Authored != 1 {
		t.Fatalf("bundle manifest lost the facts source: %+v", manifest.Sources)
	}
	if manifest.Sources.Seed == nil || manifest.Sources.Docs == nil {
		t.Fatalf("bundle manifest lost the seed/docs sources: %+v", manifest.Sources)
	}
}

// TestBundleExportRecordsProducerProvenance pins the manifest header fields the
// exporter never filled in: every bundle ever written carried
// generated_at 0001-01-01T00:00:00Z and no branches at all.
func TestBundleExportRecordsProducerProvenance(t *testing.T) {
	_, opts, cmd, _, _ := bundleTestIndexedBrain(t)
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	manifest := bundleTestManifest(t, output)
	if manifest.GeneratedAt.IsZero() {
		t.Fatalf("bundle manifest generated_at is the zero time: %q", manifest.GeneratedAt)
	}
	if !manifest.GeneratedAt.Equal(opts.Now().UTC()) {
		t.Fatalf("bundle manifest generated_at = %s, want %s", manifest.GeneratedAt, opts.Now().UTC())
	}
	if len(manifest.Branches) != 1 || manifest.Branches[0].Branch != "main" {
		t.Fatalf("bundle manifest dropped branches: %+v", manifest.Branches)
	}
	// Producer attribution: provider_version is entire-graph's and must stay
	// so; brain_version is this build's.
	if manifest.BrainVersion != "9.9.9" {
		t.Fatalf("bundle manifest brain_version = %q, want the producing build's version", manifest.BrainVersion)
	}
	// The producer's absolute path is deliberately not in a shareable artifact.
	if manifest.RepoRoot != "" {
		t.Fatalf("bundle manifest leaked the producer repo_root: %q", manifest.RepoRoot)
	}
}

// TestBundleExportExcludesSessionFamilyAndPrivacyState pins what must NOT
// travel: verbatim transcripts and their projections without an explicit
// opt-in, and this machine's privacy policy under any circumstances.
func TestBundleExportExcludesSessionFamilyAndPrivacyState(t *testing.T) {
	_, opts, cmd, _, _ := bundleTestIndexedBrain(t)
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	entries := bundleTestEntries(t, output)
	for _, forbidden := range []string{
		exportSessionsDirectory + "/main/session.jsonl",
		historyDirName + "/generations/gen1/" + historyIndexFileName,
		brainPatternsDirName + "/episodes.ndjson",
	} {
		if slicesContainsString(entries, forbidden) {
			t.Fatalf("default bundle carried session-family entry %s", forbidden)
		}
	}
	withSessions := filepath.Join(t.TempDir(), "with-sessions.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, withSessions, true); err != nil {
		t.Fatalf("export --include-sessions: %v", err)
	}
	sessionEntries := bundleTestEntries(t, withSessions)
	for _, want := range []string{
		exportSessionsDirectory + "/main/session.jsonl",
		historyDirName + "/generations/gen1/" + historyIndexFileName,
		brainPatternsDirName + "/episodes.ndjson",
	} {
		if !slicesContainsString(sessionEntries, want) {
			t.Fatalf("--include-sessions bundle is missing %s; entries:\n%s", want, strings.Join(sessionEntries, "\n"))
		}
	}
	// Privacy policy and operational state stay out of BOTH bundles.
	for _, entries := range [][]string{entries, sessionEntries} {
		for _, forbidden := range []string{
			sessionTombstonesPath,
			historyDirName + "/privacy/session%3Asecret.json",
			historyDirName + "/work/v1/worker.log",
			semanticDirName + "/" + semanticAuditLogName,
		} {
			if slicesContainsString(entries, forbidden) {
				t.Fatalf("bundle carried never-bundled entry %s", forbidden)
			}
		}
	}
}

// TestBundleRoundTripPreservesAuthoredFacts is the end-to-end proof: export
// from one store, import into an empty one, and the authored fact is still
// there with its manifest source.
func TestBundleRoundTripPreservesAuthoredFacts(t *testing.T) {
	_, opts, cmd, repoDir, _ := bundleTestIndexedBrain(t)
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	importEnv := semanticTestEnv(t, repoDir)
	importOpts := opts
	importOpts.Env = importEnv
	if err := runSemanticBundleImport(cmd.Context(), cmd, importOpts, output, bundleSHA256(t, output), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	importedBrain := bundleTestBrainDir(importEnv)
	data, err := os.ReadFile(filepath.Join(importedBrain, factsDirName, "main-0000", factsFileName))
	if err != nil {
		t.Fatalf("imported brain lost the fact store: %v", err)
	}
	if !strings.Contains(string(data), "authored fact that must survive a round trip") {
		t.Fatalf("imported fact store does not contain the authored fact: %s", data)
	}
	manifest, err := loadBrainManifest(importedBrain)
	if err != nil {
		t.Fatalf("load imported manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Facts == nil || manifest.Sources.Facts.Authored != 1 {
		t.Fatalf("imported manifest has no facts source, so every reader still reports zero facts: %+v", manifest.Sources)
	}
	if manifest.Sources.Seed == nil || manifest.Sources.Docs == nil {
		t.Fatalf("imported manifest lost seed/docs sources: %+v", manifest.Sources)
	}
	// The destination's own privacy policy is untouched by an import.
	if _, err := os.Stat(filepath.Join(importedBrain, filepath.FromSlash(sessionTombstonesPath))); !os.IsNotExist(err) {
		t.Fatalf("import wrote privacy policy into the destination brain: %v", err)
	}
}

// TestBundleImportRefusesToOverwriteExistingLayers keeps the fix from opening
// the mirror image of the bug it closes: a bundle restored over a live brain
// would replace that brain's facts with the producer's.
func TestBundleImportRefusesToOverwriteExistingLayers(t *testing.T) {
	_, opts, cmd, _, _ := bundleTestIndexedBrain(t)
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Importing back into the SAME brain: every layer already exists.
	err := runSemanticBundleImport(cmd.Context(), cmd, opts, output, bundleSHA256(t, output), false)
	if err == nil {
		t.Fatalf("import over an occupied brain must refuse without --overwrite")
	}
	for _, layer := range []string{"seed", "docs", "facts"} {
		if !strings.Contains(err.Error(), layer) {
			t.Fatalf("refusal does not name the conflicting layer %q: %v", layer, err)
		}
	}
	if err := runSemanticBundleImport(cmd.Context(), cmd, opts, output, bundleSHA256(t, output), true); err != nil {
		t.Fatalf("import --overwrite: %v", err)
	}
}

// TestBundleImportRejectsPrivacyPolicyEntries pins the import-side allowlist: a
// crafted archive must not be able to overwrite the destination's tombstones.
func TestBundleImportRejectsPrivacyPolicyEntries(t *testing.T) {
	for _, name := range []string{
		sessionTombstonesPath,
		historyDirName + "/privacy/session%3Asecret.json",
		historyDirName + "/work/v1/worker.log",
		semanticDirName + "/" + semanticAuditLogName,
	} {
		if err := validateBundleEntry(&tar.Header{Name: name, Typeflag: tar.TypeReg}); err == nil {
			t.Fatalf("bundle entry %q must be refused", name)
		}
	}
	for _, name := range []string{
		exportManifestFileName,
		seedDirName + "/repo-overview.md",
		factsDirName + "/main-0000/" + factsFileName,
		historyDirName + "/generations/gen1/index.json",
	} {
		if err := validateBundleEntry(&tar.Header{Name: name, Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("bundle entry %q must be accepted: %v", name, err)
		}
	}
}

func slicesContainsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
