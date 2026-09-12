package factsync

import (
	"context"
	"errors"
	"fmt"
)

// InvalidProposalSetError retains the rejected snapshot for explicit repair.
// Normal consumers receive no usable set and must not process these entries.
type InvalidProposalSetError struct {
	Set     ProposalSet
	Invalid []int
}

func (e *InvalidProposalSetError) Error() string {
	return fmt.Sprintf("%s: %d invalid entries; inspect with facts proposals repair", ErrProposalIDMismatch, len(e.Invalid))
}
func (e *InvalidProposalSetError) Unwrap() error { return ErrProposalIDMismatch }

type ProposalRepairResult struct {
	Ref     string         `json:"ref"`
	Invalid []OpenProposal `json:"invalid"`
	Removed int            `json:"removed"`
	Applied bool           `json:"applied"`
}

// RepairProposalIDs previews invalid queue entries. Applying requires the exact
// preview ref and CAS-publishes only the valid entries; it never writes facts.
// A conflict is returned rather than retrying against an unreviewed snapshot.
func (h *HTTPServer) RepairProposalIDs(ctx context.Context, repoID, branch string, apply bool, expectedRef string) (ProposalRepairResult, error) {
	if apply && expectedRef == "" {
		return ProposalRepairResult{}, errors.New("proposal repair requires the preview ref")
	}
	set, err := h.ListProposals(ctx, repoID, branch)
	var invalid *InvalidProposalSetError
	if err != nil {
		if !errors.As(err, &invalid) {
			return ProposalRepairResult{}, err
		}
		set = invalid.Set
	}
	result := ProposalRepairResult{Ref: set.Ref, Invalid: []OpenProposal{}}
	rejected := make(map[int]bool)
	if invalid != nil {
		for _, i := range invalid.Invalid {
			rejected[i] = true
			result.Invalid = append(result.Invalid, set.Proposals[i])
		}
	}
	if !apply {
		return result, nil
	}
	if expectedRef != set.Ref {
		return result, fmt.Errorf("%w: proposal queue changed; preview repair again", ErrConflict)
	}
	if len(rejected) == 0 {
		return result, nil
	}
	kept := make([]OpenProposal, 0, len(set.Proposals)-len(rejected))
	for i, p := range set.Proposals {
		if !rejected[i] {
			kept = append(kept, p)
		}
	}
	ref, err := h.PublishProposals(ctx, repoID, branch, set.Ref, kept)
	if err != nil {
		return result, err
	}
	result.Ref = ref
	result.Removed = len(rejected)
	result.Applied = true
	return result, nil
}
