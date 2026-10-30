package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
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

func TestSemanticResetOnlyRemovesSemanticState(t *testing.T) {
	repo, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
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
	if _, err := os.Stat(filepath.Join(brainDir, semstore.DirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic artifacts remain: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep me" {
		t.Fatalf("unrelated state changed: %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "reset code index") {
		t.Fatalf("reset output = %q", out.String())
	}
}

func TestSemanticResetRequiresForceBeforeTouchingState(t *testing.T) {
	repo, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	_ = mustSemanticSource(t, env)
	err := runSemanticReset(context.Background(), &cobra.Command{Use: "reset"}, opts, semanticResetOptions{semanticOnly: true}, repo)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("reset without force error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, bmanifest.FileName)); err != nil {
		t.Fatalf("state changed before force validation: %v", err)
	}
}

func TestSemanticBundleImportRejectsMalformedArchiveBeforeBrainMutation(t *testing.T) {
	_, env, _, opts := buildSemanticMaintenanceFixture(t)
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	_ = mustSemanticSource(t, env)
	before, err := os.ReadFile(filepath.Join(brainDir, bmanifest.FileName))
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
	if got, readErr := os.ReadFile(filepath.Join(brainDir, bmanifest.FileName)); readErr != nil || !bytes.Equal(got, before) {
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
