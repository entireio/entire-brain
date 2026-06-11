package cli

import (
	"os"
	"path/filepath"
	"time"
)

// loadAllFactBranches scans the facts directory and returns every stored fact
// grouped by the branch recorded on each record (not the on-disk slug, which is
// lossy). It is the basis for rebuilding the manifest after a single-branch
// mutation and for whole-store operations like gc.
func loadAllFactBranches(brainDir string) (map[string][]factRecord, error) {
	root := filepath.Join(brainDir, factsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]factRecord{}, nil
		}
		return nil, err
	}
	byBranch := map[string][]factRecord{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		records, err := parseFactsFile(filepath.Join(root, entry.Name(), factsFileName))
		if err != nil {
			return nil, err
		}
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
	return withBrainWriteLock(brainDir, func() error {
		return updateFactSourceManifestLocked(brainDir, now)
	})
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
