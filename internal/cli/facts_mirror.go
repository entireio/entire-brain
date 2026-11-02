package cli

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// dropFactsForMissingSessions applies the mirror rule to the fact layer: the
// brain serves only what the checkpoint refs can back, so on refresh every
// provenance anchor citing a session that is gone from the current checkpoint
// catalog is dropped, and a distilled fact left with no provenance is deleted.
// Proposals referencing a deleted fact are pruned with it.
//
// Two classes of facts are never touched. Authored facts (`remember`) have no
// session source to mirror. Facts without any transcript anchor were not
// distilled by this brain — SanitizeForEgress strips transcript anchors on
// push, so a pulled teammate copy never carries one (see hasLocalProvenance) —
// and their anchor sessions legitimately live outside this catalog.
//
// Returns (facts deleted, anchors stripped from surviving facts).
func dropFactsForMissingSessions(brainDir string, now time.Time) (int, int, error) {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return 0, 0, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return 0, 0, nil
	}
	presentIDs := map[string]bool{}
	presentRels := map[string]bool{}
	for _, session := range manifest.Sources.Sessions.Sessions {
		if id := strings.TrimSpace(session.SessionID); id != "" {
			presentIDs[id] = true
		}
		if rel := brainstore.NormalizeTranscriptPath(session.TranscriptPath); rel != "" {
			presentRels[rel] = true
		}
	}
	anchorPresent := func(anchor factAnchor) bool {
		id := strings.TrimSpace(anchor.SessionID)
		rel := brainstore.NormalizeTranscriptPath(anchor.Transcript)
		if id == "" && rel == "" {
			return true // unattributable; nothing to mirror against
		}
		return presentIDs[id] || presentRels[rel]
	}
	deletedTotal, strippedTotal := 0, 0
	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		byFile, err := factstore.LoadStoreFiles(brainDir)
		if err != nil {
			return err
		}
		changedAny := false
		for dir, facts := range byFile {
			changed := false
			deletedIDs := map[string]bool{}
			kept := make([]factRecord, 0, len(facts))
			for _, fact := range facts {
				if fact.Origin == factstore.OriginAuthored || !hasLocalProvenance(fact) {
					kept = append(kept, fact)
					continue
				}
				remaining := fact.Provenance[:0:0]
				stripped := 0
				for _, anchor := range fact.Provenance {
					if anchorPresent(anchor) {
						remaining = append(remaining, anchor)
						continue
					}
					stripped++
				}
				if stripped > 0 && len(remaining) == 0 {
					deletedIDs[fact.ID] = true
					deletedTotal++
					changed = true
					continue
				}
				if stripped > 0 {
					fact.Provenance = remaining
					strippedTotal += stripped
					changed = true
				}
				kept = append(kept, fact)
			}
			if !changed {
				continue
			}
			changedAny = true
			if err := factstore.WriteStoreFile(brainDir, dir, kept); err != nil {
				return err
			}
			proposalRel := filepath.ToSlash(filepath.Join(factstore.DirName, dir, factsProposalsFileName))
			proposals, err := loadFactProposalsAt(brainDir, proposalRel)
			if err != nil {
				return err
			}
			if len(proposals) > 0 {
				keptProposals := proposals[:0:0]
				for _, proposal := range proposals {
					if deletedIDs[proposal.CandidateID] || deletedIDs[proposal.TargetID] {
						continue
					}
					keptProposals = append(keptProposals, proposal)
				}
				if len(keptProposals) != len(proposals) {
					if err := writeFactProposalsAt(brainDir, proposalRel, keptProposals); err != nil {
						return err
					}
				}
			}
		}
		if !changedAny {
			return nil
		}
		if err := factstore.MigrateRepository(brainDir); err != nil {
			return err
		}
		return updateFactSourceManifestLocked(brainDir, now)
	})
	return deletedTotal, strippedTotal, err
}
