package cli

import (
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// loadFactStoreFiles scans the facts directory and returns every stored fact
// grouped by store directory — one entry per facts.ndjson file, keyed by the
// on-disk directory name. A FILE is the unit recall reads and writers replace,
// which is not the same thing as the branch recorded on each record: the
// hosted pull mirrors the repo feed into the current branch's store without
// restamping, so one record legitimately appears in many files. Operations
// that rewrite stores must therefore iterate files, never record-branch
// buckets — writing a cross-file bucket into one file is how a store ends up
// holding the same record many times over (COR-2220).
func loadFactStoreFiles(brainDir string) (map[string][]factRecord, error) {
	return loadRepoFactViews(brainDir)
}

func writeFactStoreFile(brainDir, dirName string, records []factRecord) error {
	return writeRepoFactView(brainDir, dirName, records)
}

// loadAllFactBranches returns every stored fact grouped by the branch recorded
// on each record (not the on-disk slug, which is lossy). It is the read-side
// view for the manifest and whole-store reports. Because the hosted pull
// mirrors feed records into other branches' files, a bucket here can span
// files and repeat ids across them — safe to COUNT and read, but never write a
// bucket back into a single file (see loadFactStoreFiles).
func loadAllFactBranches(brainDir string) (map[string][]factRecord, error) {
	byFile, err := loadFactStoreFiles(brainDir)
	if err != nil {
		return nil, err
	}
	byBranch := map[string][]factRecord{}
	for _, records := range byFile {
		for _, record := range records {
			byBranch[record.Branch] = append(byBranch[record.Branch], record)
		}
	}
	return byBranch, nil
}

// countFactProposals totals the pending proposals across the given branches.
func countFactProposals(brainDir string, branches []string) int {
	total := 0
	for _, branch := range branches {
		if proposals, err := loadFactProposals(brainDir, branch); err == nil {
			total += len(proposals)
		}
	}
	return total
}

// updateFactSourceManifest recomputes sources.facts from the whole on-disk fact
// store and writes it back. Commands that mutate facts outside the distill
// pipeline (remember, facts review/promote/gc) call this so `status` and the
// manifest stay accurate. Chunk counts are preserved from the existing source
// (they describe the last distill run, not these edits).
func updateFactSourceManifest(brainDir string, now time.Time) error {
	return brainstore.WithBrainWriteLock(brainDir, func() error {
		return updateFactSourceManifestLocked(brainDir, now)
	})
}

// reclassifyAllFactBranches backfills the deterministic KIND/LOCUS onto every
// fact store (non-destructive: only fills facts missing a valid kind, while
// locus is always reconciled), collapses records that repeat an id within one
// file, and rebuilds the fact source manifest so by_kind stays accurate. No
// agent, no tokens. A no-op when no facts exist. Returns the number of facts
// changed (reclassified plus duplicates folded). Holds the brain write lock
// for the whole pass.
//
// This runs from `refresh` so a kind never goes stale: facts distilled before
// kind-storage, or any fact left without a valid kind, are repaired on the next
// refresh rather than needing a manual `facts reclassify`.
//
// The pass iterates store FILES, never record-branch buckets. An earlier
// version wrote loadAllFactBranches buckets back through writeFacts, and a
// bucket spans files: every main-stamped record the hosted pull had mirrored
// into a feature branch's store was concatenated into main's file, roughly
// doubling it on each refresh (COR-2220). The per-file dedup below is also the
// repair for stores that doubling already damaged — the copies carry the same
// id, so folding them through Upsert's merge rules loses nothing.
func reclassifyAllFactBranches(brainDir string, now time.Time) (int, error) {
	changed := 0
	err := brainstore.WithBrainWriteLock(brainDir, func() error {
		byFile, err := loadFactStoreFiles(brainDir)
		if err != nil {
			return err
		}
		if len(byFile) == 0 {
			return nil
		}
		for dirName, facts := range byFile {
			deduped, folded := dedupFactRecords(facts)
			c := reclassifyFacts(deduped, false)
			if c == 0 && folded == 0 {
				continue
			}
			if err := writeFactStoreFile(brainDir, dirName, deduped); err != nil {
				return err
			}
			changed += c + folded
		}
		// Refresh upgrades even an unchanged legacy store. Ordinary read
		// commands remain read-only, including on read-only installations.
		if err := migrateRepoFacts(brainDir); err != nil {
			return err
		}
		return updateFactSourceManifestLocked(brainDir, now)
	})
	return changed, err
}

func updateFactSourceManifestLocked(brainDir string, now time.Time) error {
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return err
	}
	branches := make([]string, 0, len(byBranch))
	for branch := range byBranch {
		branches = append(branches, branch)
	}
	proposals := countFactProposals(brainDir, branches)

	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	var previous *factSourceManifest
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		previous = manifest.Sources.Facts
	}
	source := summarizeFactSource(now, byBranch, 0, 0, proposals, nil)
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
