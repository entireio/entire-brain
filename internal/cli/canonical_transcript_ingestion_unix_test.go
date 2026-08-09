//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSymlinkTranscriptNeverInvokesDistillProvider(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "private transcript bytes")
	target := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	external := filepath.Join(t.TempDir(), "external.jsonl")
	if err := os.WriteFile(external, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, target); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, maxChunkBytes: defaultDistillChunkSize,
		run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			calls++
			return "", errors.New("provider must not run")
		},
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err == nil || !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("symlinked distill error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider invoked %d times for a symlinked canonical transcript", calls)
	}
	assertNoDistillDerivedState(t, brainDir)
}

func TestDistillCanonicalTranscriptFinishRejectsRootSwapWithoutPublication(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "private transcript bytes")
	detached := filepath.Join(filepath.Dir(brainDir), filepath.Base(brainDir)+"-detached")

	originalHook := beforeCanonicalHistoryTranscriptFinish
	swapped := false
	beforeCanonicalHistoryTranscriptFinish = func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.Rename(brainDir, detached); err != nil {
			t.Fatalf("detach brain root: %v", err)
		}
		if err := os.MkdirAll(brainDir, 0o700); err != nil {
			t.Fatalf("replace brain root: %v", err)
		}
	}
	t.Cleanup(func() { beforeCanonicalHistoryTranscriptFinish = originalHook })

	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, maxChunkBytes: defaultDistillChunkSize,
		run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			calls++
			return "", errors.New("provider must not run")
		},
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err == nil || !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("root-swap distill error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider invoked %d times after a canonical root swap", calls)
	}
	assertNoDistillDerivedState(t, detached)
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(distillCachePath))); !os.IsNotExist(err) {
		t.Fatalf("replacement-root distill cache stat error = %v, want not exist", err)
	}
}

func TestCanonicalTranscriptFinishRejectsLeafSwapAndReturnsNoBytes(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "private transcript bytes")
	rel := "sessions/main/s1.jsonl"
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	originalHook := beforeCanonicalHistoryTranscriptFinish
	beforeCanonicalHistoryTranscriptFinish = func() {
		detached := filepath.Join(t.TempDir(), "detached.jsonl")
		if err := os.Rename(target, detached); err != nil {
			t.Fatalf("detach transcript: %v", err)
		}
		if err := os.WriteFile(target, []byte("replacement transcript!"), 0o600); err != nil {
			t.Fatalf("replace transcript: %v", err)
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			t.Fatalf("restore replacement mtime: %v", err)
		}
	}
	t.Cleanup(func() { beforeCanonicalHistoryTranscriptFinish = originalHook })

	data, err := readCanonicalHistoryTranscript(context.Background(), brainDir, rel)
	if err == nil || !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("leaf-swap error = %v", err)
	}
	if data != nil {
		t.Fatalf("leaf-swap read returned %d bytes", len(data))
	}
}
