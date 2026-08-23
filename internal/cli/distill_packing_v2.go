package cli

import (
	"fmt"
	"strings"
)

const (
	// distillCandidatePackMaxRenderedBytesV2 is deliberately independent of a
	// caller's model/chunk setting. It bounds one complete provider request.
	distillCandidatePackMaxRenderedBytesV2 = 32 << 10
	distillCandidatePackMaxMembersV2       = 32
)

// distillCandidatePackMemberV2 is one already-rendered, redacted candidate
// card. RenderedCard is opaque to the packer: it is appended byte-for-byte to
// ProviderInput, while Anchor retains the provenance needed to attribute a
// completed member result without reparsing provider input.
//
// CandidateID and Anchor.CandidateID must agree. Keeping both makes the
// ownership relation explicit at this boundary and prevents a result from
// being attached to another card's provenance during future integration.
type distillCandidatePackMemberV2 struct {
	CandidateID  string
	RenderedCard string
	Anchor       distillCandidateAnchorV1
}

// distillCandidatePackV2 is one complete multi-card provider request. Members
// remain in their supplied order and ProviderInput is their exact
// concatenation; rendering is responsible for any inter-card delimiter.
type distillCandidatePackV2 struct {
	Members       []distillCandidatePackMemberV2
	ProviderInput string
}

// packDistillCandidateMembersV2 groups complete rendered cards in supplied
// order. It is a pure, deterministic sequential first-fit packer: a member is
// appended to the one active pack when both fixed limits permit it, otherwise
// that complete pack is emitted and the member starts the next pack. It never splits or
// rewrites cards, so callers can safely rely on candidate IDs and byte-exact
// provider input.
func packDistillCandidateMembersV2(members []distillCandidatePackMemberV2) ([]distillCandidatePackV2, error) {
	if len(members) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(members))
	for index := range members {
		member := members[index]
		if err := validateDistillCandidatePackMemberV2(member, seen); err != nil {
			return nil, fmt.Errorf("candidate pack v2 member %d: %w", index, err)
		}
		seen[member.CandidateID] = struct{}{}
	}

	// At most one pack per member. Preallocating this capacity also ensures no
	// output aliases the caller's slice.
	packs := make([]distillCandidatePackV2, 0, len(members))
	current := distillCandidatePackV2{}
	currentBytes := 0
	emit := func() {
		if len(current.Members) == 0 {
			return
		}
		packs = append(packs, current)
		current = distillCandidatePackV2{}
		currentBytes = 0
	}

	for _, member := range members {
		cardBytes := len(member.RenderedCard)
		if len(current.Members) == distillCandidatePackMaxMembersV2 ||
			currentBytes > distillCandidatePackMaxRenderedBytesV2-cardBytes {
			emit()
		}
		current.Members = append(current.Members, member)
		current.ProviderInput += member.RenderedCard
		currentBytes += cardBytes
	}
	emit()
	return packs, nil
}

func validateDistillCandidatePackMemberV2(member distillCandidatePackMemberV2, seen map[string]struct{}) error {
	id := member.CandidateID
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\t\r\n") {
		return fmt.Errorf("invalid candidate ID %q", id)
	}
	if _, exists := seen[id]; exists {
		return fmt.Errorf("duplicate candidate ID %q", id)
	}
	if strings.TrimSpace(member.RenderedCard) == "" {
		return fmt.Errorf("candidate %q has an empty rendered card", id)
	}
	if len(member.RenderedCard) > distillCandidatePackMaxRenderedBytesV2 {
		return fmt.Errorf("candidate %q rendered card exceeds %d bytes", id, distillCandidatePackMaxRenderedBytesV2)
	}
	if member.Anchor.CandidateID != id {
		return fmt.Errorf("candidate %q anchor candidate ID is %q", id, member.Anchor.CandidateID)
	}
	return nil
}
