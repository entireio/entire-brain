package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestUnsafeSkillEvidenceStopsBeforeProviderOutputAndPersistence(t *testing.T) {
	brainDir := t.TempDir()
	externalDir := t.TempDir()
	externalPath := filepath.Join(externalDir, "external.jsonl")
	externalData := []byte("external private evidence\n")
	if err := os.WriteFile(externalPath, externalData, 0o600); err != nil {
		t.Fatal(err)
	}
	anchorPath := filepath.Join(brainDir, "sessions", "main", "unsafe.jsonl")
	if err := os.MkdirAll(filepath.Dir(anchorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalPath, anchorPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	candidate := taskCandidate{
		ID: "task:unsafe", RepoKey: "gh/acme/repo", IntentSignature: "deploy:release",
		Support: 3, Examples: []episodeAnchor{{Path: "sessions/main/unsafe.jsonl", Line: 1}},
	}
	providerCalled := false
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		providerCalled = true
		return "---\nname: unsafe\ndescription: should not run\n---\n", nil
	}
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := synthesizeAndForm(context.Background(), cmd, candidate, nil, nil, brainDir, t.TempDir(), "codex", run, skillFormOptions{draftOnly: true}, time.Now())
	if err == nil || !strings.Contains(err.Error(), memoryErrSessionInventory) {
		t.Fatalf("unsafe evidence error = %v, want %s", err, memoryErrSessionInventory)
	}
	if providerCalled || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("unsafe evidence escaped preflight: provider=%v stdout=%q stderr=%q", providerCalled, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(patternsSkillMemoryPath))); !os.IsNotExist(err) {
		t.Fatalf("unsafe evidence persisted skill memory: %v", err)
	}
	got, err := os.ReadFile(externalPath)
	if err != nil || !bytes.Equal(got, externalData) {
		t.Fatalf("external evidence target changed: err=%v got=%q", err, got)
	}
}
