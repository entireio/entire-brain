package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// distillCandidateApplicationV2 joins one branch-independent extraction result
// to one branch/provenance view. Candidate extraction may be shared across
// branch exports; application receipts and fact anchors may not.
type distillCandidateApplicationV2 struct {
	Result       distillCandidateCacheResultV2
	ResultDigest string
	SourceDigest string
	Target       distillCandidateMaterializationTargetV2
}

type distillCandidateApplicationOutcomeV2 struct {
	Application distillCandidateApplicationV2
	FactIDs     []string
}

type distillCandidateApplicationOwnerV2 struct {
	Branch    string
	SessionID string
}

const (
	distillCandidateIDPrefixV1                = "candidate-v1:"
	distillCandidateApplicationOwnedPrefixV2  = "candidate-application-v2-owned:"
	distillCandidateApplicationSharedPrefixV2 = "candidate-application-v2-shared:"
)

// runDistillCandidateApplyV2 is Phase 3A's opt-in write boundary. Extraction
// has already completed through the framed v2 protocol, so this function makes
// no provider calls. It applies only exact content identities and writes no
// relationship proposal, fuzzy merge, or supersession.
func runDistillCandidateApplyV2(
	ctx context.Context,
	brainDir string,
	prompt string,
	args []string,
	taxonomy factTaxonomy,
	sessions []exportSession,
	snapshots map[string]candidateDistillInputSnapshot,
	opts distillCommandOptions,
	resolveBranch func(exportSession) string,
	extraction *factSourceManifest,
	now time.Time,
) (*factSourceManifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if extraction == nil {
		return nil, fmt.Errorf("candidate v2 extraction summary is missing")
	}
	cache, err := loadDistillCandidateResultCacheV2(brainDir)
	if err != nil {
		return nil, err
	}
	baseIdentity := distillCandidateCacheBaseIdentityV2(prompt, taxonomy, args, opts)
	applications := make([]distillCandidateApplicationV2, 0)
	owners := make(map[distillCandidateApplicationOwnerV2]map[string]struct{})
	seenSlots := make(map[string]distillCandidateApplicationV2)
	for _, session := range sessions {
		branch := resolveBranch(session)
		if !distillSessionSelectedForOptionsV2(session, branch, opts) {
			continue
		}
		owner := distillCandidateApplicationOwnerV2{Branch: branch, SessionID: strings.TrimSpace(session.SessionID)}
		if owner.SessionID == "" {
			return nil, fmt.Errorf("candidate application has no source session ID")
		}
		if _, ok := owners[owner]; !ok {
			owners[owner] = make(map[string]struct{})
		}
		snapshot, ok := snapshots[distillCandidateSnapshotKey(session, branch)]
		if !ok {
			return nil, fmt.Errorf("candidate application preflight snapshot missing for %s", filepath.ToSlash(session.TranscriptPath))
		}
		for _, batch := range snapshot.input.CandidateBatches {
			if len(batch.CandidateIDs) != 1 || len(batch.Anchors) != 1 || batch.CandidateIDs[0] != batch.Anchors[0].CandidateID {
				return nil, fmt.Errorf("candidate v2 found an invalid application boundary for %s", filepath.ToSlash(session.TranscriptPath))
			}
			candidateID := batch.CandidateIDs[0]
			anchor := batch.Anchors[0]
			identity := baseIdentity.withCard(batch.Chunk.Text)
			result, ok := cache.LookupSuccess(candidateID, identity)
			if !ok {
				return nil, fmt.Errorf("candidate %q has no protocol-valid v2 result after extraction", candidateID)
			}
			resultDigest, err := distillApplicationResultDigestV2(result)
			if err != nil {
				return nil, err
			}
			application := distillCandidateApplicationV2{
				Result:       result,
				ResultDigest: resultDigest,
				SourceDigest: identity.CardDigest,
				Target: distillCandidateMaterializationTargetV2{
					CandidateID:      candidateID,
					Branch:           branch,
					SourceSessionID:  anchor.SessionID,
					SourceCheckpoint: anchor.CheckpointID,
					SourceTranscript: filepath.ToSlash(anchor.Transcript),
					// V2 owns an explicitly marked application slot, not the older
					// Phase 1 trigger-turn slot or the card ID itself. Keeping those
					// anchor identities distinct makes rollback and crash cleanup
					// non-destructive to legacy materialization.
					TriggerTurnID: distillCandidateApplicationAnchorIDV2(candidateID, false),
					StartLine:     batch.Chunk.StartLine,
					EndLine:       batch.Chunk.EndLine,
				},
			}
			if _, err := materializeDistillCandidateResultV2(result, application.Target, now); err != nil {
				return nil, err
			}
			identityForSlot := application.receiptIdentity(distillCandidateCacheDigestV2("slot-validation"))
			slotID, err := distillApplicationSlotIDV2(identityForSlot)
			if err != nil {
				return nil, err
			}
			if prior, duplicate := seenSlots[slotID]; duplicate {
				if prior.ResultDigest != application.ResultDigest || prior.SourceDigest != application.SourceDigest || prior.Target != application.Target {
					return nil, fmt.Errorf("candidate application slot %q has conflicting views", slotID)
				}
				continue
			}
			seenSlots[slotID] = application
			owners[owner][candidateID] = struct{}{}
			applications = append(applications, application)
		}
	}

	writeStarted := time.Now()
	var source *factSourceManifest
	err = withBrainWriteLock(brainDir, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipts, err := loadDistillApplicationReceiptStoreV2(brainDir)
		if err != nil {
			return err
		}
		branches := make(map[string][]factRecord)
		beforeDigests := make(map[string]string)
		loadBranch := func(branch string) ([]factRecord, error) {
			if facts, ok := branches[branch]; ok {
				return facts, nil
			}
			facts, err := loadFacts(brainDir, branch)
			if err != nil {
				return nil, err
			}
			branches[branch] = facts
			beforeDigests[branch] = distillFactRecordsDigest(facts)
			return facts, nil
		}

		// A normal incremental run reconciles only sessions present in the frozen
		// input snapshot. Force expands that cleanup to every prior v2 receipt in
		// the selected branch scope, but never drops legacy or authored facts.
		if opts.force {
			selectedBranches := make(map[string]struct{})
			if strings.TrimSpace(opts.branch) != "" {
				selectedBranches[strings.TrimSpace(opts.branch)] = struct{}{}
			} else {
				for owner := range owners {
					selectedBranches[owner.Branch] = struct{}{}
				}
				for _, receipt := range receipts.entries {
					selectedBranches[receipt.Identity.Branch] = struct{}{}
				}
				allBranches, err := loadAllFactBranches(brainDir)
				if err != nil {
					return err
				}
				for branch, facts := range allBranches {
					if hasDistillApplicationAnchorsV2(facts) {
						selectedBranches[branch] = struct{}{}
					}
				}
			}
			for slotID, receipt := range receipts.entries {
				if _, selected := selectedBranches[receipt.Identity.Branch]; !selected {
					continue
				}
				facts, err := loadBranch(receipt.Identity.Branch)
				if err != nil {
					return err
				}
				branches[receipt.Identity.Branch], _ = removeDistillApplicationReceiptAnchorsV2(facts, receipt)
				delete(receipts.entries, slotID)
			}
			for branch := range selectedBranches {
				facts, err := loadBranch(branch)
				if err != nil {
					return err
				}
				branches[branch], _ = removeDistillApplicationAnchorsV2(facts, "", nil)
			}
		} else {
			for owner, current := range owners {
				facts, err := loadBranch(owner.Branch)
				if err != nil {
					return err
				}
				branches[owner.Branch], _ = removeDistillApplicationAnchorsV2(facts, owner.SessionID, current)
				removed, err := receipts.PruneUnseenSlotsWithReceipts(owner.Branch, owner.SessionID, current)
				if err != nil {
					return err
				}
				for _, receipt := range removed {
					facts, err := loadBranch(owner.Branch)
					if err != nil {
						return err
					}
					branches[owner.Branch], _ = removeDistillApplicationReceiptAnchorsV2(facts, receipt)
				}
			}
		}

		outcomesByBranch := make(map[string][]distillCandidateApplicationOutcomeV2)
		for _, application := range applications {
			facts, err := loadBranch(application.Target.Branch)
			if err != nil {
				return err
			}
			generation := distillApplicationFactStoreGenerationV2(facts)
			identity := application.receiptIdentity(generation)
			slotID, err := distillApplicationSlotIDV2(identity)
			if err != nil {
				return err
			}
			prior, hasPrior := receipts.LookupSlot(slotID)
			if !opts.force && (!hasPrior || !sameDistillApplicationContentV2(prior.Identity, identity)) {
				facts, _ = removeDistillApplicationSlotAnchorsV2(facts, application.Target.SourceSessionID, application.Target.CandidateID)
				branches[application.Target.Branch] = facts
			}
			if receipt, hit := receipts.LookupSuccess(application.receiptIdentity(distillApplicationFactStoreGenerationV2(facts))); hit && !opts.force {
				outcomesByBranch[application.Target.Branch] = append(outcomesByBranch[application.Target.Branch], distillCandidateApplicationOutcomeV2{
					Application: application,
					FactIDs:     append([]string(nil), receipt.AppliedFactIDs...),
				})
				continue
			}

			materialized, err := materializeDistillCandidateResultV2(application.Result, application.Target, now)
			if err != nil {
				return err
			}
			materialized = assignDistillApplicationAnchorOwnershipV2(materialized, facts, application.Target.SourceSessionID, application.Target.CandidateID)
			materialized.Facts = applyEntityProvenance(materialized.Facts, opts.entityProvenance)
			if len(materialized.Facts) > 0 {
				var proposals []factProposal
				facts, proposals = applyFactActions(facts, newDistilledFactActions(materialized.Facts), defaultFactConfidenceThreshold, now)
				if len(proposals) != 0 {
					return fmt.Errorf("candidate exact-only application produced %d relationship proposals", len(proposals))
				}
				branches[application.Target.Branch] = facts
			}
			outcomesByBranch[application.Target.Branch] = append(outcomesByBranch[application.Target.Branch], distillCandidateApplicationOutcomeV2{
				Application: application,
				FactIDs:     append([]string(nil), materialized.FactIDs...),
			})
		}

		branchNames := make([]string, 0, len(branches))
		for branch := range branches {
			branchNames = append(branchNames, branch)
		}
		sort.Strings(branchNames)
		for _, branch := range branchNames {
			finalDigest := distillFactRecordsDigest(branches[branch])
			finalGeneration := distillCandidateCacheDigestV2(finalDigest)
			for _, outcome := range outcomesByBranch[branch] {
				identity := outcome.Application.receiptIdentity(finalGeneration)
				provenanceDigest, err := distillApplicationProvenanceDigestV2(identity)
				if err != nil {
					return err
				}
				if _, err := receipts.RecordSuccess(identity, outcome.FactIDs, provenanceDigest); err != nil {
					return err
				}
			}
			if finalDigest != beforeDigests[branch] {
				if err := writeFacts(brainDir, branch, branches[branch]); err != nil {
					return err
				}
			}
		}
		if err := saveDistillApplicationReceiptStoreV2(brainDir, receipts); err != nil {
			return err
		}
		if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
			return err
		}

		allBranches, err := loadAllFactBranches(brainDir)
		if err != nil {
			return err
		}
		allBranchNames := make([]string, 0, len(allBranches))
		for branch := range allBranches {
			allBranchNames = append(allBranchNames, branch)
		}
		totalProposals := countFactProposals(brainDir, allBranchNames)
		source = summarizeFactSource(now, allBranches, extraction.ChunksScanned, extraction.ChunksDistilled, totalProposals, extraction.Warnings)
		copyDistillCandidateRunMetricsV2(source, extraction)
		source.WriteSeconds = time.Since(writeStarted).Seconds()
		source.TotalSeconds = extraction.TotalSeconds + source.WriteSeconds
		return updateBrainManifestAndReadme(brainDir, func(currentManifest *exportManifest) error {
			if currentManifest.Sources == nil {
				currentManifest.Sources = &brainSources{}
			}
			persisted := factSourceForManifestV3(source)
			currentManifest.Sources.Facts = &persisted
			if currentManifest.GeneratedAt.IsZero() {
				currentManifest.GeneratedAt = now
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}

func (application distillCandidateApplicationV2) receiptIdentity(generation string) distillApplicationIdentityV2 {
	return distillApplicationIdentityV2{
		CandidateID:         application.Target.CandidateID,
		ResultID:            application.ResultDigest,
		SourceSessionID:     application.Target.SourceSessionID,
		SourceDigest:        application.SourceDigest,
		Branch:              application.Target.Branch,
		TranscriptPath:      application.Target.SourceTranscript,
		CheckpointID:        application.Target.SourceCheckpoint,
		SourceStartLine:     application.Target.StartLine,
		SourceEndLine:       application.Target.EndLine,
		FactStoreGeneration: generation,
	}
}

func sameDistillApplicationContentV2(left, right distillApplicationIdentityV2) bool {
	left.FactStoreGeneration = ""
	right.FactStoreGeneration = ""
	return left == right
}

func distillApplicationFactStoreGenerationV2(records []factRecord) string {
	return distillCandidateCacheDigestV2(distillFactRecordsDigest(records))
}

// assignDistillApplicationAnchorOwnershipV2 makes crash recovery independent
// of the receipt write. A fact first created by v2 receives an owned marker;
// an exact match that predates v2 receives a shared marker. Ownership propagates
// to later exact v2 matches so the last v2 application can still remove a fact
// that no independent source ever owned.
func assignDistillApplicationAnchorOwnershipV2(materialized distillCandidateMaterializationV2, active []factRecord, sessionID, candidateID string) distillCandidateMaterializationV2 {
	for index := range materialized.Facts {
		owned := false
		activeIndex := indexOfFact(active, materialized.Facts[index].ID)
		if activeIndex < 0 {
			owned = true
		} else {
			for _, anchor := range active[activeIndex].Provenance {
				_, anchorOwned, applicationAnchor := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
				if applicationAnchor && anchorOwned {
					owned = true
					break
				}
			}
		}
		anchorID := distillCandidateApplicationAnchorIDV2(candidateID, owned)
		for anchorIndex := range materialized.Facts[index].Provenance {
			anchor := &materialized.Facts[index].Provenance[anchorIndex]
			if anchor.SessionID == sessionID {
				anchor.DistillTurnID = anchorID
			}
		}
	}
	return materialized
}

func removeDistillApplicationReceiptAnchorsV2(records []factRecord, receipt distillApplicationReceiptV2) ([]factRecord, bool) {
	if len(receipt.AppliedFactIDs) == 0 {
		return records, false
	}
	if distillCandidateApplicationAnchorIDV2(receipt.Identity.CandidateID, false) == "" {
		return records, false
	}
	wanted := make(map[string]struct{}, len(receipt.AppliedFactIDs))
	for _, id := range receipt.AppliedFactIDs {
		wanted[id] = struct{}{}
	}
	out := records[:0]
	changed := false
	for index := range records {
		if _, ok := wanted[records[index].ID]; !ok {
			out = append(out, records[index])
			continue
		}
		kept := records[index].Provenance[:0]
		removedOwned := false
		for _, anchor := range records[index].Provenance {
			candidateID, owned, applicationAnchor := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
			if applicationAnchor && candidateID == receipt.Identity.CandidateID && anchor.SessionID == receipt.Identity.SourceSessionID {
				changed = true
				removedOwned = removedOwned || owned
				continue
			}
			kept = append(kept, anchor)
		}
		records[index].Provenance = kept
		if removedOwned && len(kept) == 0 && records[index].Origin == factOriginDistilled {
			continue
		}
		out = append(out, records[index])
	}
	return out, changed
}

// removeDistillApplicationSlotAnchorsV2 removes every anchor owned by one v2
// application slot, including facts that reached disk before their receipt.
// Receipt fact IDs are an audit record and fast path, never the cleanup
// authority across a crash boundary.
func removeDistillApplicationSlotAnchorsV2(records []factRecord, sessionID, candidateID string) ([]factRecord, bool) {
	if sessionID == "" || distillCandidateApplicationAnchorIDV2(candidateID, false) == "" {
		return records, false
	}
	out := records[:0]
	changed := false
	for index := range records {
		kept := records[index].Provenance[:0]
		removedOwned := false
		for _, anchor := range records[index].Provenance {
			anchorCandidateID, owned, applicationAnchor := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
			if applicationAnchor && anchorCandidateID == candidateID && anchor.SessionID == sessionID {
				changed = true
				removedOwned = removedOwned || owned
				continue
			}
			kept = append(kept, anchor)
		}
		records[index].Provenance = kept
		if removedOwned && len(kept) == 0 && records[index].Origin == factOriginDistilled {
			continue
		}
		out = append(out, records[index])
	}
	return out, changed
}

// removeDistillApplicationAnchorsV2 reconciles the complete current v2
// candidate set for a session. An empty sessionID means a force-scoped branch
// reset. Phase 1 anchors use turn-v1 IDs, so they remain outside this namespace.
func removeDistillApplicationAnchorsV2(records []factRecord, sessionID string, current map[string]struct{}) ([]factRecord, bool) {
	out := records[:0]
	changed := false
	for index := range records {
		kept := records[index].Provenance[:0]
		removedOwned := false
		for _, anchor := range records[index].Provenance {
			candidateID, anchorOwned, applicationAnchor := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
			if applicationAnchor && (sessionID == "" || anchor.SessionID == sessionID) {
				_, keep := current[candidateID]
				if !keep {
					changed = true
					removedOwned = removedOwned || anchorOwned
					continue
				}
			}
			kept = append(kept, anchor)
		}
		records[index].Provenance = kept
		if removedOwned && len(kept) == 0 && records[index].Origin == factOriginDistilled {
			continue
		}
		out = append(out, records[index])
	}
	return out, changed
}

func hasDistillApplicationAnchorsV2(records []factRecord) bool {
	for _, record := range records {
		for _, anchor := range record.Provenance {
			if isDistillCandidateApplicationAnchorIDV2(anchor.DistillTurnID) {
				return true
			}
		}
	}
	return false
}

func distillCandidateApplicationAnchorIDV2(candidateID string, owned bool) string {
	if !strings.HasPrefix(candidateID, distillCandidateIDPrefixV1) {
		return ""
	}
	digest := strings.TrimPrefix(candidateID, distillCandidateIDPrefixV1)
	if !validSHA256Identity("sha256:" + digest) {
		return ""
	}
	prefix := distillCandidateApplicationSharedPrefixV2
	if owned {
		prefix = distillCandidateApplicationOwnedPrefixV2
	}
	return prefix + digest
}

func isDistillCandidateApplicationAnchorIDV2(anchorID string) bool {
	_, _, ok := distillCandidateApplicationAnchorDetailsV2(anchorID)
	return ok
}

func distillCandidateIDFromApplicationAnchorV2(anchorID string) string {
	candidateID, _, _ := distillCandidateApplicationAnchorDetailsV2(anchorID)
	return candidateID
}

func distillCandidateApplicationAnchorDetailsV2(anchorID string) (candidateID string, owned bool, ok bool) {
	prefix := ""
	switch {
	case strings.HasPrefix(anchorID, distillCandidateApplicationOwnedPrefixV2):
		prefix, owned = distillCandidateApplicationOwnedPrefixV2, true
	case strings.HasPrefix(anchorID, distillCandidateApplicationSharedPrefixV2):
		prefix = distillCandidateApplicationSharedPrefixV2
	default:
		return "", false, false
	}
	digest := strings.TrimPrefix(anchorID, prefix)
	if !validSHA256Identity("sha256:" + digest) {
		return "", false, false
	}
	return distillCandidateIDPrefixV1 + digest, owned, true
}

func copyDistillCandidateRunMetricsV2(target, source *factSourceManifest) {
	target.CacheHits = source.CacheHits
	target.FailedChunks = source.FailedChunks
	target.CandidateBytes = source.CandidateBytes
	target.CandidateCards = source.CandidateCards
	target.CandidateSessions = source.CandidateSessions
	target.CandidatePacks = source.CandidatePacks
	target.CandidatePacksDone = source.CandidatePacksDone
	target.CandidateMembers = source.CandidateMembers
	target.CandidateCacheHits = source.CandidateCacheHits
	target.CandidateCacheMisses = source.CandidateCacheMisses
	target.CandidateSplitCalls = source.CandidateSplitCalls
	target.CandidateEmptyResults = source.CandidateEmptyResults
	target.Agent = source.Agent
	target.Model = source.Model
	target.Effort = source.Effort
	target.Pipeline = distillPipelineCandidates
	target.Branch = source.Branch
	target.Force = source.Force
	target.Jobs = source.Jobs
	target.ExtractionJobsCap = source.ExtractionJobsCap
	target.MaxChunkBytes = source.MaxChunkBytes
	target.ExtractionCalls = source.ExtractionCalls
	target.TotalAgentCalls = source.TotalAgentCalls
	target.TokenUsage = source.TokenUsage
	target.ExtractionWaitSeconds = source.ExtractionWaitSeconds
	target.Warnings = append([]string(nil), source.Warnings...)
}

func factSourceForManifestV3(source *factSourceManifest) factSourceManifest {
	persisted := *source
	persisted.CandidateBytes = 0
	persisted.CandidateCards = 0
	persisted.CandidateSessions = 0
	persisted.CandidatePacks = 0
	persisted.CandidatePacksDone = 0
	persisted.CandidateMembers = 0
	persisted.CandidateCacheHits = 0
	persisted.CandidateCacheMisses = 0
	persisted.CandidateSplitCalls = 0
	persisted.CandidateEmptyResults = 0
	persisted.Pipeline = ""
	persisted.Shadow = false
	persisted.ShadowFacts = 0
	return persisted
}
