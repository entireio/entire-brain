package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// distillRelationshipBlockKeyV2 is only an index key. The relationship core
// revalidates both records before a proposal can reach durable state.
type distillRelationshipBlockKeyV2 struct {
	Branch      string
	Kind        string
	TopLevel    string
	StrongLocus string
}

// distillRelationshipBuildStatsV2 describes bounded advisory discovery. A
// skipped block is intentionally absent in its entirety: partial block output
// would make a relationship set depend on input order and would be misleading
// to an operator inspecting the advisory store.
type distillRelationshipBuildStatsV2 struct {
	BlocksConsidered      int
	BlocksBuilt           int
	SkippedBlocks         int
	SkippedDenseBlocks    int
	SkippedCapacityBlocks int
	SkippedByteBlocks     int
	SkippedOwnerBlocks    int
}

func (stats *distillRelationshipBuildStatsV2) add(other distillRelationshipBuildStatsV2) {
	if stats == nil {
		return
	}
	stats.BlocksConsidered += other.BlocksConsidered
	stats.BlocksBuilt += other.BlocksBuilt
	stats.SkippedBlocks += other.SkippedBlocks
	stats.SkippedDenseBlocks += other.SkippedDenseBlocks
	stats.SkippedCapacityBlocks += other.SkippedCapacityBlocks
	stats.SkippedByteBlocks += other.SkippedByteBlocks
	stats.SkippedOwnerBlocks += other.SkippedOwnerBlocks
}

// distillRelationshipBuildBudgetV2 is the remaining capacity available for
// the branch being rebuilt. RemainingBytes includes only relationship record
// lines; the caller reserves the canonical header and every other branch.
type distillRelationshipBuildBudgetV2 struct {
	RemainingEntries int
	RemainingBytes   int
}

// distillRelationshipBlockV2MaxPairs is kept at the durable-store capacity.
// Since a block can produce at most one relationship per unordered pair, this
// lets the builder reject a dense block before any quadratic work begins.
const distillRelationshipBlockV2MaxPairs = distillRelationshipStoreV2MaxEntries

// buildDistillRelationshipsForBranchV2 derives the complete neutral advisory
// state for one fact branch. It never mutates facts. At least one side of a
// pair must carry a current candidate-v2 application anchor, which makes every
// relationship attributable and lets result replacement, NO_FACTS, force, and
// privacy cleanup converge by rebuilding from current truth.
func buildDistillRelationshipsForBranchV2(records []factRecord) ([]factmerge.RelationshipProposal, error) {
	proposals, _, err := buildDistillRelationshipsForBranchWithStatsV2(records)
	return proposals, err
}

// buildDistillRelationshipsForBranchWithStatsV2 derives bounded neutral
// advisory state for one fact branch. The relationship store has a hard entry
// limit, so dense blocks and blocks that cannot fit in the remaining global
// budget are skipped as whole units. This keeps advisory discovery from
// failing primary fact distillation merely because it encountered O(n^2)
// candidate pairs.
func buildDistillRelationshipsForBranchWithStatsV2(records []factRecord) ([]factmerge.RelationshipProposal, distillRelationshipBuildStatsV2, error) {
	return buildDistillRelationshipsForBranchWithBudgetV2(records, defaultDistillRelationshipBuildBudgetV2())
}

// buildDistillRelationshipsForBranchWithBudgetV2 is the capacity-aware form
// used when other branches already occupy relationship-store space. It admits
// a block only when all of its resulting canonical relationship lines fit.
func buildDistillRelationshipsForBranchWithBudgetV2(records []factRecord, budget distillRelationshipBuildBudgetV2) ([]factmerge.RelationshipProposal, distillRelationshipBuildStatsV2, error) {
	type indexedFact struct {
		record factRecord
		owners []factmerge.RelationshipOwner
	}
	stats := distillRelationshipBuildStatsV2{}
	if budget.RemainingEntries < 0 || budget.RemainingBytes < 0 {
		return nil, stats, fmt.Errorf("relationship build budget is invalid")
	}

	ordered := make([]indexedFact, 0, len(records))
	for _, record := range records {
		if record.Status != factStatusActive || strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Branch) == "" {
			continue
		}
		record.Kind = factKindOrInferred(record)
		record.Locus = append([]string(nil), factLocusOf(record)...)
		ordered = append(ordered, indexedFact{record: record, owners: distillRelationshipOwnersForFactV2(record)})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].record.ID < ordered[j].record.ID })

	blocks := make(map[distillRelationshipBlockKeyV2][]int)
	blockKeys := make([]distillRelationshipBlockKeyV2, 0)
	for index, candidate := range ordered {
		for _, key := range distillRelationshipBlocksForFactV2(candidate.record) {
			if _, exists := blocks[key]; !exists {
				blockKeys = append(blockKeys, key)
			}
			blocks[key] = append(blocks[key], index)
		}
	}
	sort.Slice(blockKeys, func(i, j int) bool {
		left, right := blockKeys[i], blockKeys[j]
		if left.Branch != right.Branch {
			return left.Branch < right.Branch
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.TopLevel != right.TopLevel {
			return left.TopLevel < right.TopLevel
		}
		return left.StrongLocus < right.StrongLocus
	})

	byID := make(map[string]factmerge.RelationshipProposal)
	usedBytes := 0
	for _, key := range blockKeys {
		block := blocks[key]
		stats.BlocksConsidered++
		if len(block) < 2 {
			stats.BlocksBuilt++
			continue
		}
		if distillRelationshipPairCountExceedsV2(len(block), distillRelationshipBlockV2MaxPairs) {
			stats.SkippedBlocks++
			stats.SkippedDenseBlocks++
			continue
		}
		if distillRelationshipPairCountExceedsV2(len(block), budget.RemainingEntries-len(byID)) {
			stats.SkippedBlocks++
			stats.SkippedCapacityBlocks++
			continue
		}
		blockByID := make(map[string]factmerge.RelationshipProposal)
		ownerOverflow := false
		for candidateOffset := 1; candidateOffset < len(block); candidateOffset++ {
			candidate := ordered[block[candidateOffset]]
			for _, priorIndex := range block[:candidateOffset] {
				prior := ordered[priorIndex]
				owners := factmerge.UnionRelationshipOwners(prior.owners, candidate.owners)
				if len(owners) == 0 {
					continue
				}
				if len(owners) > factmerge.RelationshipMaxOwners {
					ownerOverflow = true
					break
				}
				proposal, ok, err := factmerge.BuildPossibleSameSubject(prior.record, candidate.record, owners)
				if err != nil {
					return nil, stats, err
				}
				if !ok {
					continue
				}
				// A pair can be indexed under more than one shared key, while
				// BuildPossibleSameSubject chooses its canonical (lexically first)
				// subject. Admit it only from that canonical block so skipping a
				// block can never leak a partial relationship through another one.
				if proposal.Subject != (factmerge.RelationshipSubject{Kind: key.Kind, TopLevel: key.TopLevel, StrongLocus: key.StrongLocus}) {
					continue
				}
				if existing, duplicate := blockByID[proposal.ID]; duplicate {
					proposal.Owners = factmerge.UnionRelationshipOwners(existing.Owners, proposal.Owners)
				}
				blockByID[proposal.ID] = proposal
			}
			if ownerOverflow {
				break
			}
		}
		if ownerOverflow {
			stats.SkippedBlocks++
			stats.SkippedOwnerBlocks++
			continue
		}
		updates := make(map[string]factmerge.RelationshipProposal, len(blockByID))
		byteDelta := 0
		tooLarge := false
		for id, proposal := range blockByID {
			oldLineBytes := 0
			if existing, duplicate := byID[id]; duplicate {
				existingLineBytes, lineErr := distillRelationshipProposalLineBytesV2(existing)
				if lineErr != nil {
					return nil, stats, lineErr
				}
				oldLineBytes = existingLineBytes
				proposal.Owners = factmerge.UnionRelationshipOwners(existing.Owners, proposal.Owners)
			}
			lineBytes, err := distillRelationshipProposalLineBytesV2(proposal)
			if err != nil {
				return nil, stats, err
			}
			if lineBytes-1 > distillRelationshipStoreV2MaxLine {
				tooLarge = true
				break
			}
			byteDelta += lineBytes - oldLineBytes
			updates[id] = proposal
		}
		if tooLarge || usedBytes+byteDelta > budget.RemainingBytes {
			stats.SkippedBlocks++
			stats.SkippedByteBlocks++
			continue
		}
		for id, proposal := range updates {
			byID[id] = proposal
		}
		usedBytes += byteDelta
		stats.BlocksBuilt++
	}

	out := make([]factmerge.RelationshipProposal, 0, len(byID))
	for _, proposal := range byID {
		if err := factmerge.ValidateRelationshipProposal(proposal); err != nil {
			return nil, stats, err
		}
		out = append(out, proposal)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, stats, nil
}

func defaultDistillRelationshipBuildBudgetV2() distillRelationshipBuildBudgetV2 {
	headerBytes, err := distillRelationshipStoreHeaderBytesV2()
	if err != nil {
		// json.Marshal of the fixed header cannot fail. Retain a conservative
		// fallback should that ever stop being true.
		headerBytes = distillRelationshipStoreV2MaxBytes
	}
	return distillRelationshipBuildBudgetV2{
		RemainingEntries: distillRelationshipStoreV2MaxEntries,
		RemainingBytes:   distillRelationshipStoreV2MaxBytes - headerBytes,
	}
}

// distillRelationshipBuildBudgetForReplacementV2 returns exact remaining
// store capacity after omitting the branch that will be transactionally
// replaced. It gives callers a deterministic global budget across branches.
func distillRelationshipBuildBudgetForReplacementV2(store distillRelationshipStoreV2, branch string) (distillRelationshipBuildBudgetV2, error) {
	headerBytes, err := distillRelationshipStoreHeaderBytesV2()
	if err != nil {
		return distillRelationshipBuildBudgetV2{}, err
	}
	usedEntries, usedBytes := 0, headerBytes
	for _, proposal := range store.entries {
		if proposal.Branch == branch {
			continue
		}
		lineBytes, err := distillRelationshipProposalLineBytesV2(proposal)
		if err != nil {
			return distillRelationshipBuildBudgetV2{}, err
		}
		usedEntries++
		usedBytes += lineBytes
	}
	return distillRelationshipBuildBudgetV2{
		RemainingEntries: distillRelationshipStoreV2MaxEntries - usedEntries,
		RemainingBytes:   distillRelationshipStoreV2MaxBytes - usedBytes,
	}, nil
}

func distillRelationshipStoreHeaderBytesV2() (int, error) {
	data, err := json.Marshal(distillRelationshipStoreLineV2{Type: "header", Version: distillRelationshipStoreV2Version})
	if err != nil {
		return 0, err
	}
	return len(data) + 1, nil
}

func distillRelationshipProposalLineBytesV2(proposal factmerge.RelationshipProposal) (int, error) {
	data, err := json.Marshal(distillRelationshipStoreLineV2{Type: "relationship", Relation: &proposal})
	if err != nil {
		return 0, err
	}
	return len(data) + 1, nil
}

// distillRelationshipPairCountExceedsV2 compares n choose 2 without integer
// multiplication, which keeps an adversarially large in-memory block from
// overflowing before it can be safely skipped.
func distillRelationshipPairCountExceedsV2(count, limit int) bool {
	if count < 2 {
		return false
	}
	if limit < 1 {
		return true
	}
	left, right := count, count-1
	if left%2 == 0 {
		left /= 2
	} else {
		right /= 2
	}
	return left > limit/right
}

func distillRelationshipOwnersForFactV2(record factRecord) []factmerge.RelationshipOwner {
	owners := make([]factmerge.RelationshipOwner, 0, len(record.Provenance))
	for _, anchor := range record.Provenance {
		candidateID, _, ok := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
		if !ok || strings.TrimSpace(anchor.SessionID) == "" {
			continue
		}
		owners = append(owners, factmerge.RelationshipOwner{
			CandidateID: candidateID, SourceSessionID: strings.TrimSpace(anchor.SessionID),
		})
	}
	return factmerge.UnionRelationshipOwners(owners)
}

func distillRelationshipBlocksForFactV2(record factRecord) []distillRelationshipBlockKeyV2 {
	topLevels := make(map[string]struct{})
	for _, path := range record.Paths {
		path = strings.TrimSpace(path)
		if !validFactPath(path) {
			continue
		}
		topLevels[factTopLevel(path)] = struct{}{}
	}
	loci := make(map[string]struct{})
	for _, locus := range record.Locus {
		if strong, ok := factmerge.StrongRelationshipLocusForRecord(record, locus); ok {
			loci[strong] = struct{}{}
		}
	}
	keys := make([]distillRelationshipBlockKeyV2, 0, len(topLevels)*len(loci))
	for topLevel := range topLevels {
		for locus := range loci {
			keys = append(keys, distillRelationshipBlockKeyV2{
				Branch: record.Branch, Kind: record.Kind, TopLevel: topLevel, StrongLocus: locus,
			})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].TopLevel != keys[j].TopLevel {
			return keys[i].TopLevel < keys[j].TopLevel
		}
		return keys[i].StrongLocus < keys[j].StrongLocus
	})
	return keys
}

func replaceDistillRelationshipBranchV2(store *distillRelationshipStoreV2, branch string, proposals []factmerge.RelationshipProposal) error {
	if store == nil || strings.TrimSpace(branch) == "" {
		return fmt.Errorf("relationship branch replacement is invalid")
	}
	// Build and validate the complete replacement before changing store.entries.
	// This is important when a later proposal is malformed or the global entry
	// capacity is exceeded: the old branch must remain available intact. Runtime
	// callers separately pass the exact durable byte budget to the builder.
	next := make(map[string]factmerge.RelationshipProposal, len(store.entries)+len(proposals))
	for id, proposal := range store.entries {
		if proposal.Branch != branch {
			next[id] = cloneDistillRelationshipProposalV2(proposal)
		}
	}
	for _, proposal := range proposals {
		if proposal.Branch != branch {
			return fmt.Errorf("relationship %q belongs to branch %q, not %q", proposal.ID, proposal.Branch, branch)
		}
		if err := validateDistillRelationshipProposalV2(proposal); err != nil {
			return err
		}
		if existing, duplicate := next[proposal.ID]; duplicate {
			proposal.Owners = factmerge.UnionRelationshipOwners(existing.Owners, proposal.Owners)
			if err := validateDistillRelationshipProposalV2(proposal); err != nil {
				return err
			}
		}
		next[proposal.ID] = cloneDistillRelationshipProposalV2(proposal)
	}
	if len(next) > distillRelationshipStoreV2MaxEntries {
		return fmt.Errorf("relationship store exceeds %d entries", distillRelationshipStoreV2MaxEntries)
	}
	store.entries = next
	return nil
}

func distillRelationshipsForBranchV2(store distillRelationshipStoreV2, branch string) []factmerge.RelationshipProposal {
	out := make([]factmerge.RelationshipProposal, 0)
	for _, proposal := range store.entries {
		if proposal.Branch == branch {
			out = append(out, cloneDistillRelationshipProposalV2(proposal))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
