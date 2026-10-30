package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

const transcriptStorageCheckpoint = "checkpoint"

func checkpointBackedSessions(manifest *exportManifest) bool {
	return manifest != nil && manifest.Sources != nil && manifest.Sources.Sessions != nil && manifest.Sources.Sessions.TranscriptStorage == transcriptStorageCheckpoint
}

// errRepositoryGone reports a prepass that failed because the repository this
// brain belongs to is no longer there.
//
// Also durable. A brain outlives its repository -- scratch clones, benchmark
// cells, a checkout someone deleted -- and the worker went on relaunching
// against it. Measured on this machine: nine orphaned brains, three of them
// holding roughly 2,650 recorded prepass failures each.
//
// If the repository comes back, nothing is lost: a new lifecycle event starts a
// fresh chain, which is exactly how the existing give-up path already behaves.
func errRepositoryGone(err error) bool {
	if err == nil {
		return false
	}
	// The common orphan case never reaches git at all. When the repository
	// DIRECTORY is gone the subprocess fails at chdir, and runExecCommand only
	// folds stderr into the message when stderr is non-empty -- which it is not
	// here -- so the error reads
	//
	//	entire [checkpoint explain --json --search-all]: chdir /path: no such file or directory
	//
	// and neither string below appears in it. That left the exact scenario this
	// function exists for -- a brain whose repository is gone, relaunching on a
	// 1m..30m chain, ~2,650 recorded failures apiece -- classified as transient.
	//
	// Matched by type rather than by text: a chdir that cannot find its target
	// is precisely "the working directory is not there", and will not drift
	// with a git or Go message change. Narrow on purpose -- any other missing
	// file is NOT a durable failure, and calling it one sends the reader to
	// `entire brain reset`, which deletes the brain.
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "chdir" && errors.Is(pathErr, fs.ErrNotExist) {
		return true
	}
	// A directory that exists but holds no repository DOES reach git, which
	// writes to stderr, which runExecCommand folds into the error.
	msg := err.Error()
	return strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "repository does not exist")
}

// errDurablePrepassFailure reports a failure that retrying cannot change.
//
// The relaunch chain is 1m, 2m, 4m, 8m, 16m, 30m, 30m -- about two hours -- and
// each pending retry is a live sleeping process, not a timer. Spending that on
// a condition fixed only by a human is the defect this classifies away.
func errDurablePrepassFailure(err error) bool {
	return bmanifest.IsRetiredFieldError(err) || errRepositoryGone(err)
}

// durablePrepassMessage states the failure and what to do about it.
// durablePrepassMessage renders a durable failure for the worker log and for
// `entire memory worker --json`. Both are durable and both get pasted into
// issues, so every branch below is redacted at the return rather than at the
// call sites -- a second caller added later would otherwise reintroduce the
// leak this closed.
func durablePrepassMessage(err error) string {
	return base.RedactText(durablePrepassText(err))
}

func durablePrepassText(err error) string {
	if err == nil {
		return ""
	}
	if bmanifest.IsRetiredFieldError(err) {
		return bmanifest.RetiredFieldMessage(err)
	}
	if errRepositoryGone(err) {
		return err.Error() + "; the repository this brain belongs to is gone, so nothing here can be refreshed -- remove the brain with `" + base.BrainInvocation(os.LookupEnv, "reset") + "` if the repository is not coming back"
	}
	return err.Error()
}

// errManifestRoundTrips reports a manifest this build can already rewrite, so
// there is nothing for a forced refresh to discard.
var errManifestRoundTrips = errors.New("manifest round-trips")

// discardManifestThisBuildCannotRewrite removes a manifest whose only defect is
// fields this build cannot name, so an explicit forced refresh can rebuild it.
//
// Every writer refuses such a manifest deliberately: re-encoding from a struct
// that never saw a field erases it. That leaves a brain readable but frozen,
// and until now the only way out was for a person to delete the file by hand.
// An operator asking for a forced refresh is asking for exactly that
// replacement, rebuilt from canonical sources -- but only for this one cause:
//
//   - a newer schema stays. Down-converting a vNext manifest is the loss the
//     version check exists to prevent, and --force does not overrule it.
//   - genuine corruption stays. That is a diagnosis for the operator, not bytes
//     to delete on a guess.
//
// The removal runs under the manifest write lock and through the checked
// remover, so it inherits the same symlink and file-identity guarantees as
// every other cooperative manifest mutation.
func discardManifestThisBuildCannotRewrite(brainDir string) (bool, error) {
	unrewritable := false
	err := brainstore.WithBrainManifestWriteLock(brainDir, func() error {
		return removeCheckedMemoryStateFileValidated(brainDir, bmanifest.FileName, "brain manifest", func(before os.FileInfo) (bool, error) {
			f, present, err := brainstore.OpenMemoryStateFileExpected(brainDir, bmanifest.FileName, "brain manifest", before)
			if err != nil || !present {
				return present, err
			}
			defer f.Close()
			_, _, droppedUnknownFields, err := bmanifest.DecodeFile(f, true)
			if err != nil {
				return true, err
			}
			if !droppedUnknownFields {
				return true, errManifestRoundTrips
			}
			unrewritable = true
			return true, nil
		})
	})
	if errors.Is(err, errManifestRoundTrips) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return unrewritable, nil
}

func writeBrainSessionSource(outputDir, repoKey string, sessionManifest exportManifest) error {
	return brainstore.WithBrainWriteLock(outputDir, func() error {
		return writeBrainSessionSourceLocked(outputDir, repoKey, sessionManifest)
	})
}

func writeBrainSessionSourceLocked(outputDir, repoKey string, sessionManifest exportManifest) error {
	existing, err := bmanifest.Load(outputDir)
	if err != nil {
		return err
	}
	if existing.Sources == nil {
		existing.Sources = &brainSources{}
	}
	sessionManifest.SchemaVersion = bmanifest.SchemaVersion
	sessionManifest.RepoKey = repoKey
	sessionManifest.Sources = existing.Sources
	sessionManifest.Sources.Sessions = bmanifest.SessionSourceFromExport(sessionManifest)
	if existing.Sources.Seed != nil {
		sessionManifest.Sources.Seed = existing.Sources.Seed
	}
	if existing.Sources.Semantic != nil {
		sessionManifest.Sources.Semantic = existing.Sources.Semantic
	}
	return writeBrainManifestAndReadme(outputDir, sessionManifest)
}

func writeBrainSeedSource(outputDir, repoKey string, seed *seedSourceManifest) error {
	return brainstore.WithBrainWriteLock(outputDir, func() error {
		return writeBrainSeedSourceLocked(outputDir, repoKey, seed)
	})
}

func writeBrainSeedSourceLocked(outputDir, repoKey string, seed *seedSourceManifest) error {
	manifest, err := bmanifest.Load(outputDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.SchemaVersion = bmanifest.SchemaVersion
	manifest.RepoKey = repoKey
	manifest.GeneratedAt = seed.GeneratedAt
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = time.Now().UTC()
	}
	manifest.Sources.Seed = seed
	applySessionSourceAliases(manifest)
	return writeBrainManifestAndReadme(outputDir, *manifest)
}

func applySessionSourceAliases(manifest *exportManifest) {
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return
	}
	sessions := manifest.Sources.Sessions
	manifest.EntireCLIVersion = sessions.EntireCLIVersion
	manifest.DefaultBranch = sessions.DefaultBranch
	manifest.TranscriptMode = sessions.TranscriptMode
	manifest.TranscriptStorage = sessions.TranscriptStorage
	manifest.Scope = sessions.Scope
	manifest.CheckpointLimit = sessions.CheckpointLimit
	manifest.CheckpointsScanned = sessions.CheckpointsScanned
	manifest.Branches = append([]exportBranch(nil), sessions.Branches...)
	manifest.Sessions = append([]exportSession(nil), sessions.Sessions...)
	manifest.Warnings = append([]string(nil), sessions.Warnings...)
}

func writeBrainManifestAndReadme(outputDir string, manifest exportManifest) error {
	return brainstore.WithBrainManifestWriteLock(outputDir, func() error {
		// All production manifest replacements share this leaf-specific lock and
		// classify the exact present bytes while holding it. This does not claim
		// to exclude hostile non-cooperating filesystem mutation, but no current
		// writer can down-convert a vNext or erase unrecognized current bytes it
		// observed.
		if _, err := bmanifest.LoadForReplace(outputDir); err != nil {
			return err
		}
		bmanifest.Normalize(&manifest)
		manifest.SchemaVersion = bmanifest.SchemaVersion
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", bmanifest.FileName, err)
		}
		data = append(data, '\n')
		// Canonical manifest readers have no byte ceiling; retain every record
		// and publish the complete document atomically.
		if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, bmanifest.FileName, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", bmanifest.FileName, err)
		}
		if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, exportReadmeFileName, []byte(renderBrainReadme(manifest)), 0o600); err != nil {
			return fmt.Errorf("write brain readme: %w", err)
		}
		return nil
	})
}

func renderBrainReadme(manifest exportManifest) string {
	if manifest.Sources == nil || (manifest.Sources.Seed == nil && manifest.Sources.Semantic == nil && manifest.Sources.History == nil) {
		return renderExportReadme(manifest)
	}
	return renderCombinedBrainReadme(manifest)
}
