package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCanonicalTranscriptReadEnforcesBoundAndReturnsNoBytes(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "0123456789")

	oldMax := maxDocumentTranscriptBytes
	maxDocumentTranscriptBytes = 8
	t.Cleanup(func() { maxDocumentTranscriptBytes = oldMax })

	data, err := readCanonicalHistoryTranscript(context.Background(), brainDir, "sessions/main/s1.jsonl")
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("bounded read error = %v", err)
	}
	if data != nil {
		t.Fatalf("bounded read returned %d bytes on failure", len(data))
	}
}

func TestUnsafeTranscriptNeverInvokesDistillProvider(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "0123456789")
	oldMax := maxDocumentTranscriptBytes
	maxDocumentTranscriptBytes = 8
	t.Cleanup(func() { maxDocumentTranscriptBytes = oldMax })

	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, maxChunkBytes: defaultDistillChunkSize,
		run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			calls++
			return "", errors.New("provider must not run")
		},
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err == nil || !strings.Contains(err.Error(), memoryErrInputTooLarge) {
		t.Fatalf("over-limit distill error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider invoked %d times for an over-limit canonical transcript", calls)
	}
	assertNoDistillDerivedState(t, brainDir)
}

func assertNoDistillDerivedState(t *testing.T, brainDir string) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest after refused distill: %v", err)
	}
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		t.Fatalf("refused distill published facts manifest: %+v", manifest.Sources.Facts)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(distillCachePath))); !os.IsNotExist(err) {
		t.Fatalf("refused distill cache stat error = %v, want not exist", err)
	}
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		t.Fatalf("load facts after refused distill: %v", err)
	}
	if len(byBranch) != 0 {
		t.Fatalf("refused distill published fact branches: %+v", byBranch)
	}
}

func TestEpisodeUnsafeInputPublishesNoPartialPatternArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, brainDir string)
	}{
		{name: "over limit", mutate: func(t *testing.T, brainDir string) {
			oldMax := maxDocumentTranscriptBytes
			maxDocumentTranscriptBytes = 8
			t.Cleanup(func() { maxDocumentTranscriptBytes = oldMax })
		}},
		{name: "inventory ceiling", mutate: func(t *testing.T, brainDir string) {
			oldLimit := historySessionInventoryMaxEntries
			historySessionInventoryMaxEntries = 1
			t.Cleanup(func() { historySessionInventoryMaxEntries = oldLimit })
		}},
		{name: "irrelevant symlink", mutate: func(t *testing.T, brainDir string) {
			if runtime.GOOS == "windows" {
				t.Skip("creating symlinks requires privileges on many Windows builders")
			}
			external := filepath.Join(t.TempDir(), "external.bin")
			if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, filepath.Join(brainDir, exportSessionsDirectory, "irrelevant.bin")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			brainDir := writeEpisodeFixture(t, now, "gh/acme/canonical", []sessionFixture{{
				id: "s1", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: claudeReviewTranscript,
			}})
			if _, err := writeBrainEpisodesAndSource(brainDir, now); err != nil {
				t.Fatal(err)
			}
			paths := []string{patternsEpisodesPath, patternsTasksPath, patternsProceduresPath, patternsPracticesPath, exportManifestFileName}
			before := make(map[string][]byte, len(paths))
			for _, rel := range paths {
				data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				before[rel] = data
			}

			test.mutate(t, brainDir)
			if _, err := writeBrainEpisodesAndSource(brainDir, now.Add(time.Minute)); err == nil {
				t.Fatal("unsafe episode rebuild unexpectedly succeeded")
			}
			for _, rel := range paths {
				after, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, before[rel]) {
					t.Fatalf("unsafe episode rebuild changed %s", rel)
				}
			}
		})
	}
}

func TestHistoryProjectionDoesNotReadExcludedTranscriptContent(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	secretRel := ""
	for _, session := range manifest.Sources.Sessions.Sessions {
		if session.SessionID == "secret-sess" {
			secretRel = filepath.ToSlash(session.TranscriptPath)
			break
		}
	}
	if secretRel == "" {
		t.Fatal("privacy fixture has no secret session")
	}
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "test"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	var digested []string
	originalHook := beforeHistoryProjectionContentDigest
	beforeHistoryProjectionContentDigest = func(rel string) { digested = append(digested, rel) }
	t.Cleanup(func() { beforeHistoryProjectionContentDigest = originalHook })
	prepared, err := prepareBrainHistoryProjection(brainDir, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = discardPreparedHistoryProjection(brainDir, prepared) })
	for _, rel := range digested {
		if rel == secretRel {
			t.Fatalf("excluded transcript content was hashed during projection: %s", rel)
		}
	}
}
