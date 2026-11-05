package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestDistillCancellationPreservesCompletedWrites(t *testing.T) {
	for _, mode := range []string{"foreground final wait", "background final wait", "background interval flush"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			brainDir := writeSingleSessionFixture(t, now, "Keep completed extraction results.\n")
			repoDir := t.TempDir()
			ctx := context.Background()
			if strings.HasPrefix(mode, "background") {
				ctx = brainstore.BackgroundWrites(ctx)
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var calls atomic.Int32
			opts := distillCommandOptions{
				agent: "command", agentCommand: []string{"fake"}, timeout: time.Minute,
				run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
					calls.Add(1)
					if mode == "background interval flush" {
						cancel()
					}
					return "project.testing.rules\tPreserve completed extraction results.\n", nil
				},
			}
			if mode == "background interval flush" {
				opts.flushEvery = 1
			} else {
				ready := make(chan *brainstore.FileLock, 1)
				opts.progress = func(distillProgress) {
					lock, err := brainstore.AcquireFileLock(brainstore.BrainWriteLockPath(brainDir), "brain_locked", 0)
					if err != nil {
						t.Error(err)
					}
					if mode == "background final wait" {
						cancel()
					}
					ready <- lock
				}
				type result struct {
					source *factSourceManifest
					err    error
				}
				done := make(chan result, 1)
				go func() {
					source, err := runDistillForBrain(ctx, repoDir, brainDir, opts, now)
					done <- result{source, err}
				}()
				var held *brainstore.FileLock
				select {
				case held = <-ready:
					if held == nil {
						t.Fatal("could not hold the writer lock")
					}
				case result := <-done:
					t.Fatalf("distill finished before progress: %v", result.err)
				case <-time.After(10 * time.Second):
					t.Fatal("distill did not finish extraction")
				}
				defer held.Close()
				if mode == "foreground final wait" {
					deadline := time.Now().Add(10 * time.Second)
					for {
						intents, err := filepath.Glob(filepath.Join(brainDir, brainstore.BrainLockDirName, "write-foreground-*.lock"))
						if err != nil {
							t.Fatal(err)
						}
						if len(intents) > 0 {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("distill never waited for the writer lock")
						}
						time.Sleep(time.Millisecond)
					}
					cancel()
				}
				select {
				case result := <-done:
					t.Fatalf("cancel discarded completed work while locked: %v", result.err)
				case <-time.After(25 * time.Millisecond):
				}
				_ = held.Close()
				select {
				case result := <-done:
					if result.err != nil {
						t.Fatal(result.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("settlement did not finish")
				}
			}
			if mode == "background interval flush" {
				source, err := runDistillForBrain(ctx, repoDir, brainDir, opts, now)
				if err != nil {
					t.Fatal(err)
				}
				for _, warning := range source.Warnings {
					if strings.Contains(warning, "mid-run flush failed") {
						t.Fatalf("completed interval flush was discarded: %s", warning)
					}
				}
			}
			facts, err := factstore.Load(brainDir)
			if err != nil || len(facts) != 1 || facts[0].Status != factStatusActive {
				t.Fatalf("completed fact not published: %+v %v", facts, err)
			}
			cache := loadDistillCache(brainDir)
			if len(cache.Sessions) != 1 || len(cache.Chunks) != 1 {
				t.Fatalf("completed cache not saved: %+v", cache)
			}
			manifest, err := bmanifest.Load(brainDir)
			if err != nil || manifest.Sources == nil || manifest.Sources.Facts == nil || manifest.Sources.Facts.Facts != 1 {
				t.Fatalf("completed manifest not saved: %+v %v", manifest, err)
			}
			before := calls.Load()
			opts.progress = nil
			source, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts, now.Add(time.Minute))
			if err != nil || source.CacheHits != 1 || calls.Load() != before {
				t.Fatalf("completed extraction ran again: source=%+v calls=%d err=%v", source, calls.Load(), err)
			}
		})
	}
}
