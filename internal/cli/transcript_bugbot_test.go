package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

// Bundle discovery uses the semantic fixture, while checkpoint object reads
// use its real Git repository and the production no-lazy-fetch environment.
type bundleCheckpointRunner struct {
	ExecRunner
	discovery CommandRunner
}

func (r bundleCheckpointRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return r.discovery.Run(ctx, dir, name, args...)
}

func TestFilelessBundleOmitsExcludedSessions(t *testing.T) {
	for _, keepIncluded := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-excluded", true: "mixed"}[keepIncluded], func(t *testing.T) {
			realOpts, realBrain, _ := filelessCheckpointFixture(t)
			runFilelessExport(t, realOpts, "", false)
			realManifest, err := bmanifest.Load(realBrain)
			if err != nil {
				t.Fatal(err)
			}
			_, opts, cmd, _, brain := bundleTestIndexedBrain(t)
			if err := os.RemoveAll(filepath.Join(brain, brainstore.HistoryDirName, "privacy")); err != nil {
				t.Fatal(err)
			}
			manifest, err := bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			manifest.RepoRoot = realManifest.RepoRoot
			manifest.CheckpointGitDir = realManifest.CheckpointGitDir
			manifest.Sources.Sessions = realManifest.Sources.Sessions
			included := manifest.Sources.Sessions.Sessions[0]
			excluded := included
			excluded.SessionID = "session:secret"
			excluded.TranscriptPath = "sessions/main/secret.jsonl"
			manifest.Sources.Sessions.Sessions = []exportSession{excluded}
			if keepIncluded {
				manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, included)
			}
			applySessionSourceAliases(manifest)
			if err := writeBrainManifestAndReadme(brain, *manifest); err != nil {
				t.Fatal(err)
			}
			opts.Runner = bundleCheckpointRunner{discovery: opts.Runner}
			output := filepath.Join(t.TempDir(), "excluded.tar")
			if err := runSemanticBundleExport(context.Background(), cmd, opts, output, true); err != nil {
				t.Fatal(err)
			}
			exported := bundleTestManifest(t, output)
			entries := bundleTestEntries(t, output)
			source := exported.Sources.Sessions
			want := 0
			if keepIncluded {
				want = 1
			}
			if len(source.Sessions) != want || source.TranscriptStorage != "" {
				t.Fatalf("bundle sessions = %+v", source)
			}
			if slicesContainsString(entries, excluded.TranscriptPath) {
				t.Fatal("excluded transcript exported")
			}
			if keepIncluded && (!slicesContainsString(entries, included.TranscriptPath) || source.Sessions[0].SessionID != included.SessionID) {
				t.Fatal("included transcript missing")
			}
			for _, branches := range [][]exportBranch{source.Branches, exported.Branches} {
				if len(branches) != 1 || branches[0].SessionCount != want {
					t.Fatalf("bundle branches = %+v", branches)
				}
			}
			current, err := bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			if len(current.Sources.Sessions.Sessions) != want+1 {
				t.Fatal("bundle export changed the local catalog")
			}
		})
	}
}

func TestFilelessMigrationPreservesUnrecoverableExcludedCache(t *testing.T) {
	for _, variant := range []string{"different-checkpoint-bytes", "old-checkpoint-missing"} {
		t.Run(variant, func(t *testing.T) {
			opts, brain, _ := filelessCheckpointFixture(t)
			runFilelessExport(t, opts, brain, false)
			manifest, err := bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			session := manifest.Sources.Sessions.Sessions[0]
			preserved := "unique cached transcript that is not in the available checkpoint\n"
			cachedPath := filepath.Join(brain, session.TranscriptPath)
			if err := os.WriteFile(cachedPath, []byte(preserved), 0600); err != nil {
				t.Fatal(err)
			}
			if variant == "old-checkpoint-missing" {
				manifest.Sources.Sessions.Sessions[0].LatestCheckpoint = "bbb222bbb222"
				applySessionSourceAliases(manifest)
				if err := writeBrainManifestAndReadme(brain, *manifest); err != nil {
					t.Fatal(err)
				}
			}
			stones := brainstore.EmptySessionTombstones()
			stones.Excluded[session.SessionID] = sessionTombstone{At: opts.Now(), Reason: "excluded"}
			if err := brainstore.SaveSessionTombstones(brain, stones); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
			if err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			err = runExport(context.Background(), cmd, opts, exportCommandOptions{scope: exportScopeAll, entireBinary: "entire", rawTranscript: true})
			// The two variants diverge here on purpose, and the difference is
			// the fix: an UNREADABLE checkpoint still refuses the migration,
			// because nothing can prove what the cached file is; DIVERGENT
			// bytes do not, because one stray file must not abort the whole
			// export. Blocking on divergence froze a real brain's manifest for
			// thirteen days and left the memory worker relaunching forever.
			// Either way the cached copy below must survive.
			if variant == "different-checkpoint-bytes" {
				if err != nil {
					t.Fatalf("divergent bytes must retain the file, not abort the export: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "retain transcript cache") {
				t.Fatalf("migration error = %v", err)
			}
			got, err := os.ReadFile(cachedPath)
			if err != nil || string(got) != preserved {
				t.Fatalf("cache lost: %q %v", got, err)
			}
			after, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			// A refused migration must leave the manifest exactly as it was. A
			// retained one is allowed to rewrite it -- that is the export
			// completing, which is the entire point -- so asserting it
			// unchanged there would be asserting the bug.
			if variant != "different-checkpoint-bytes" && string(before) != string(after) {
				t.Fatal("refused migration changed the manifest")
			}
		})
	}
}

func TestFilelessRefreshPreparesHistoryOutsideWriteLock(t *testing.T) {
	for _, changePrivacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "lock-available", true: "reject-stale-privacy"}[changePrivacy], func(t *testing.T) {
			opts, brain, _ := filelessCheckpointFixture(t)
			originalWrite := historyProjectionWriteFile
			checked := false
			historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
				if !checked && strings.HasPrefix(rel, historystore.StagingDir+"/") {
					checked = true
					unlock, err := brainstore.AcquireBrainWriteLockTimeout(brain, 100*time.Millisecond)
					if err != nil {
						t.Fatalf("history preparation holds the brain write lock: %v", err)
					}
					if changePrivacy {
						stones := brainstore.EmptySessionTombstones()
						stones.Excluded["session-one"] = sessionTombstone{At: opts.Now(), Reason: "excluded"}
						err = brainstore.SaveSessionTombstones(brain, stones)
					}
					unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				return originalWrite(dir, rel, data, mode)
			}
			t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			err := runExport(context.Background(), cmd, opts, exportCommandOptions{scope: exportScopeAll, entireBinary: "entire", rawTranscript: true, historyIndex: true})
			if !checked {
				t.Fatal("history was not prepared")
			}
			if changePrivacy {
				if err == nil || !strings.Contains(err.Error(), "memory_source_stale") {
					t.Fatalf("stale publication error = %v", err)
				}
				manifest, loadErr := bmanifest.Load(brain)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if manifest.Sources.History != nil {
					t.Fatal("stale history was published")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFilelessMigrationRecoversExcludedSessionAfterInclude(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, brain, false)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	stones := brainstore.EmptySessionTombstones()
	stones.Excluded[session.SessionID] = sessionTombstone{At: opts.Now(), Reason: "excluded"}
	if err := brainstore.SaveSessionTombstones(brain, stones); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(filepath.Join(brain, session.TranscriptPath)); !os.IsNotExist(err) {
		t.Fatalf("recoverable cache was not swept: %v", err)
	}
	if _, err := readCanonicalHistoryTranscript(context.Background(), brain, session.TranscriptPath); err == nil {
		t.Fatal("migration bypassed exclusion")
	}
	if err := brainstore.WithBrainWriteLock(brain, func() error { return includeSessionLocked(brain, session.SessionID, opts.Now()) }); err != nil {
		t.Fatal(err)
	}
	got, err := readCanonicalHistoryTranscript(context.Background(), brain, session.TranscriptPath)
	if err != nil || string(got) != conversationClaudeFixture {
		t.Fatalf("include did not recover transcript: %q %v", got, err)
	}
}
