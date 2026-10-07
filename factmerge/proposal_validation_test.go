package factmerge

import (
	"reflect"
	"testing"
	"time"
)

func TestApplyProposalRejectsInvalidWithoutMutation(t *testing.T) {
	for _, action := range []string{ActionMerge, ActionSupersede} {
		for _, status := range []string{StatusRetracted, StatusSuperseded} {
			for _, inactive := range []int{0, 1} {
				t.Run(action+"/"+status+"/"+[]string{"candidate", "target"}[inactive], func(t *testing.T) {
					facts := []Record{{ID: "candidate", Status: StatusActive, RelatedIDs: []string{"target"}}, {ID: "target", Status: StatusActive, RelatedIDs: []string{"candidate"}}}
					facts[inactive].Status = status
					assertInvalidProposalUnchanged(t, facts, Proposal{Action: action, CandidateID: "candidate", TargetID: "target"})
				})
			}
		}
		t.Run(action+"/self", func(t *testing.T) {
			assertInvalidProposalUnchanged(t, []Record{{ID: "same", Status: StatusActive}}, Proposal{Action: action, CandidateID: "same", TargetID: "same"})
		})
	}
	t.Run("unsupported action", func(t *testing.T) {
		assertInvalidProposalUnchanged(t, []Record{{ID: "a", Status: StatusActive, RelatedIDs: []string{"b"}}, {ID: "b", Status: StatusActive}}, Proposal{Action: "unexpected", CandidateID: "a", TargetID: "b"})
	})
}

func assertInvalidProposalUnchanged(t *testing.T, facts []Record, proposal Proposal) {
	t.Helper()
	before := append([]Record(nil), facts...)
	for i := range before {
		before[i].RelatedIDs = append([]string(nil), before[i].RelatedIDs...)
	}
	got, err := ApplyProposal(facts, proposal, time.Now())
	if err == nil {
		t.Error("expected invalid proposal error")
	}
	if !reflect.DeepEqual(got, before) || !reflect.DeepEqual(facts, before) {
		t.Errorf("invalid proposal mutated facts: got %+v, want %+v", got, before)
	}
}
