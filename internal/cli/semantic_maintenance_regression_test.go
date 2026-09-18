package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func buildSemanticMaintenanceFixture(t *testing.T) (string, EntireEnv, *fakeCommandRunner, Options) {
	t.Helper()
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	opts := Options{Version: "maintenance-test", Env: env, Runner: runner, Now: now}
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatalf("seed semantic index: %v", err)
	}
	return repo, env, runner, opts
}

func TestSemanticRepairRebuildsCorruptGenerationAndKeepsSnapshotEvidence(t *testing.T) {
	repo, env, runner, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	source := mustSemanticSource(t, env)
	snapshotBefore, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath)))
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(brainDir, filepath.FromSlash(source.StorePath))
	if err := os.WriteFile(store, []byte("corrupt sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	var repairOut bytes.Buffer
	repairCmd := &cobra.Command{Use: "repair"}
	repairCmd.SetOut(&repairOut)
	if err := runSemanticRepair(context.Background(), repairCmd, opts, repo); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !strings.Contains(repairOut.String(), "repaired semantic brain") {
		t.Fatalf("repair output = %q", repairOut.String())
	}
	repaired := mustSemanticSource(t, env)
	if repaired.SnapshotPath != source.SnapshotPath || repaired.Symbols != source.Symbols || repaired.Relations != source.Relations {
		t.Fatalf("repair changed snapshot evidence: before=%+v after=%+v", source, repaired)
	}
	if got, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(repaired.SnapshotPath))); err != nil || !bytes.Equal(got, snapshotBefore) {
		t.Fatalf("snapshot changed during repair: %v", err)
	}
	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&queryOut)
	if err := runSemanticQuery(context.Background(), queryCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query after repair: %v", err)
	}
	if !strings.Contains(queryOut.String(), "ValidateToken") || len(runner.calls) == 0 {
		t.Fatalf("query lost repaired evidence: %q", queryOut.String())
	}
}

func TestSemanticResetOnlyRemovesSemanticState(t *testing.T) {
	repo, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	sentinel := filepath.Join(brainDir, "unrelated-state.json")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	resetCmd := &cobra.Command{Use: "reset"}
	resetCmd.SetOut(&out)
	if err := runSemanticReset(context.Background(), resetCmd, opts, semanticResetOptions{force: true, semanticOnly: true}, repo); err != nil {
		t.Fatalf("semantic-only reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, semanticDirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic artifacts remain: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep me" {
		t.Fatalf("unrelated state changed: %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "reset semantic brain") {
		t.Fatalf("reset output = %q", out.String())
	}
}

func TestSemanticResetRequiresForceBeforeTouchingState(t *testing.T) {
	repo, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	source := mustSemanticSource(t, env)
	err := runSemanticReset(context.Background(), &cobra.Command{Use: "reset"}, opts, semanticResetOptions{semanticOnly: true}, repo)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("reset without force error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath))); err != nil {
		t.Fatalf("state changed before force validation: %v", err)
	}
}

func TestSemanticBundleExportFailureLeavesExistingOutputAndGeneration(t *testing.T) {
	_, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	source := mustSemanticSource(t, env)
	snapshot := filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath))
	if err := os.WriteFile(snapshot, []byte("broken snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(output, []byte("existing output"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runSemanticBundleExport(context.Background(), &cobra.Command{Use: "bundle export"}, opts, output, false)
	if err == nil || (!strings.Contains(err.Error(), "snapshot") && !strings.Contains(err.Error(), "schema")) {
		t.Fatalf("export corruption diagnostic = %v", err)
	}
	if got, readErr := os.ReadFile(output); readErr != nil || string(got) != "existing output" {
		t.Fatalf("failed export replaced output: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(snapshot); readErr != nil || string(got) != "broken snapshot" {
		t.Fatalf("failed export changed snapshot: %q, %v", got, readErr)
	}
}

func TestSemanticBundleImportRejectsMalformedArchiveBeforeBrainMutation(t *testing.T) {
	_, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	source := mustSemanticSource(t, env)
	before, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath)))
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "missing.tar")
	if err := os.WriteFile(archive, []byte("not a tar bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("not a tar bundle"))
	expected := hex.EncodeToString(hash[:])
	err = runSemanticBundleImport(context.Background(), &cobra.Command{Use: "bundle import"}, opts, archive, expected, true)
	if err == nil || strings.Contains(err.Error(), "checksum mismatch") ||
		(!strings.Contains(err.Error(), "invalid tar") && !strings.Contains(err.Error(), "unexpected EOF") && !strings.Contains(err.Error(), "archive/tar")) {
		t.Fatalf("malformed archive diagnostic = %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath))); readErr != nil || !bytes.Equal(got, before) {
		t.Fatalf("failed import changed active snapshot: %v", readErr)
	}
}

func TestSemanticSnapshotFileCountCountsDistinctValidPathsOnly(t *testing.T) {
	raw := "{" + `"schema_version":"1.0"` + "}\n" +
		`{"record_type":"file","path":"internal/a.go"}` + "\n" +
		`{"record_type":"symbol","file_path":"internal/a.go"}` + "\n" +
		`{"record_type":"symbol","file_path":"internal/b.go"}` + "\n" +
		"not json\n" +
		`{"record_type":"relation","from_id":"x"}` + "\n"
	if got := semanticSnapshotFileCount([]byte(raw)); got != 2 {
		t.Fatalf("semanticSnapshotFileCount = %d, want 2", got)
	}
	if got := semanticSnapshotFileCount([]byte("not a snapshot\n")); got != 0 {
		t.Fatalf("malformed snapshot file count = %d, want 0", got)
	}
}
