package cli

import (
	"context"
	"time"

	"github.com/entireio/entire-brain/factmerge"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// updateFactSourceManifest recomputes sources.facts from the whole on-disk fact
// store and writes it back. Commands that mutate facts outside the distill
// pipeline (remember, facts review/promote/gc) call this when they changed
// something, so `status` and the manifest stay accurate; no-op runs skip it,
// since this full recompute is the expensive part of holding the write lock.
// Chunk counts are preserved from the existing source (they describe the last
// distill run, not these edits).
func updateFactSourceManifest(brainDir string, now time.Time) error {
	return brainstore.WithBrainWriteLock(brainDir, func() error {
		return updateFactSourceManifestLocked(brainDir, now)
	})
}

// reclassifyAllFacts backfills the deterministic KIND/LOCUS onto the fact
// store (non-destructive: only fills facts missing a valid kind, while locus
// is always reconciled), collapses records that repeat an id, and rebuilds the
// fact source manifest so by_kind stays accurate. No agent, no tokens. A no-op
// when no facts exist. Returns the number of facts changed (reclassified plus
// duplicates folded). Holds the brain write lock for the whole pass.
//
// This runs from `refresh` so a kind never goes stale: facts distilled before
// kind-storage, or any fact left without a valid kind, are repaired on the next
// refresh rather than needing a manual `facts reclassify`.
//
// Migration runs FIRST: refresh is a write entry point, so it upgrades legacy
// branch stores into the flat store (and folds post-migration stragglers an
// older binary wrote) before the reclassify reads it. Ordinary read commands
// remain read-only, including on read-only installations.
func reclassifyAllFacts(brainDir string, now time.Time) (int, error) {
	return reclassifyAllFactsContext(context.Background(), brainDir, now)
}

func reclassifyAllFactsContext(ctx context.Context, brainDir string, now time.Time) (int, error) {
	changed := 0
	err := brainstore.WithBrainWriteLockContext(ctx, brainDir, func() error {
		if _, err := migrateFactStoreLocked(brainDir); err != nil {
			return err
		}
		facts, err := factstore.Load(brainDir)
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			return updateFactSourceManifestLocked(brainDir, now)
		}
		deduped, folded := dedupFactRecords(facts)
		c := factstore.Reclassify(deduped, false)
		changed = c + folded
		if changed > 0 {
			if err := factstore.Write(brainDir, deduped); err != nil {
				return err
			}
		}
		return updateFactSourceManifestLocked(brainDir, now)
	})
	return changed, err
}

// migrateFactStoreLocked is the shared write-entry migration step: every
// command that is about to mutate the fact store runs it first (under the
// brain write lock) so a brain upgrades on first write, and legacy branch
// files a released binary wrote after migration are folded back in rather
// than ignored. Read surfaces never call it; Load's read-only fold serves
// them. The straggler warning is returned for the caller to surface.
func migrateFactStoreLocked(brainDir string) (string, error) {
	summary, err := factstore.MigrateRepository(brainDir)
	if err != nil {
		return "", err
	}
	if summary.StragglerFiles > 0 {
		return "an older entire-brain wrote memories after migration; they were upgraded — update that machine", nil
	}
	return "", nil
}

func updateFactSourceManifestLocked(brainDir string, now time.Time) error {
	records, err := factstore.Load(brainDir)
	if err != nil {
		return err
	}

	return updateFactSourceManifestRecordsLocked(brainDir, now, records)
}

// The records are the exact replacement just committed under the writer lock.
func updateFactSourceManifestRecordsLocked(brainDir string, now time.Time, records []factmerge.Record) error {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return err
	}
	var previous *factSourceManifest
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		previous = manifest.Sources.Facts
	}
	source := factstore.SummarizeSource(now, records, 0, 0, nil)
	preserveFactDistillEvidence(source, previous)
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Facts = source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = now
	}
	return writeBrainManifestAndReadme(brainDir, *manifest)
}

func preserveFactDistillEvidence(source, previous *factSourceManifest) {
	if source == nil || previous == nil {
		return
	}
	source.LastDistilledAt = previous.LastDistilledAt
	if previous.TokenUsage != nil {
		usage := *previous.TokenUsage
		source.TokenUsage = &usage
	}
	source.ChunksScanned = previous.ChunksScanned
	source.ChunksDistilled = previous.ChunksDistilled
	source.CacheHits = previous.CacheHits
	source.FailedChunks = previous.FailedChunks
	source.PreprocessedBytes = previous.PreprocessedBytes
	source.ExtractionWaitSeconds = previous.ExtractionWaitSeconds
	source.ReconcileSeconds = previous.ReconcileSeconds
	source.WriteSeconds = previous.WriteSeconds
	// The run-configuration and call-count evidence must survive too: facts
	// admin commands (review/promote/gc) rebuild this summary, and zeroing
	// these blanks the fields the release audit reads from the manifest.
	source.Agent = previous.Agent
	source.Model = previous.Model
	source.Effort = previous.Effort
	source.Branch = previous.Branch
	source.Force = previous.Force
	source.Jobs = previous.Jobs
	source.ExtractionJobsCap = previous.ExtractionJobsCap
	source.MaxChunkBytes = previous.MaxChunkBytes
	source.Confidence = previous.Confidence
	source.ExtractionCalls = previous.ExtractionCalls
	source.ReconcileCalls = previous.ReconcileCalls
	source.TotalAgentCalls = previous.TotalAgentCalls
	source.TotalSeconds = previous.TotalSeconds
	source.Warnings = append([]string(nil), previous.Warnings...)
}
