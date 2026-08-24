package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// distillCandidateAdmissionGoldFixtureV1 is deliberately a compact, sanitized
// contract for candidate admission. It complements, but does not replace, the
// broader Phase 2 human-labeled corpus quality exit gate.
type distillCandidateAdmissionGoldFixtureV1 struct {
	FixtureContract string                                       `json:"fixture_contract"`
	SchemaVersion   int                                          `json:"schema_version"`
	Cases           []distillCandidateAdmissionGoldCaseFixtureV1 `json:"cases"`
}

type distillCandidateAdmissionGoldCaseFixtureV1 struct {
	Name              string   `json:"name"`
	Dialect           string   `json:"dialect"`
	Content           string   `json:"content"`
	CheckpointOutcome string   `json:"checkpoint_outcome"`
	FilesTouched      []string `json:"files_touched"`
	Kind              string   `json:"kind"`
	IsTask            bool     `json:"is_task"`
	WantCards         int      `json:"want_cards"`
	TriggerLine       string   `json:"trigger_line"`
	ForbiddenTrigger  string   `json:"forbidden_trigger"`
}

func TestDistillCandidateAdmissionGoldFixtureContractV1(t *testing.T) {
	fixturePath := filepath.Join("testdata", "distill_candidate_admission_gold_v1.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture distillCandidateAdmissionGoldFixtureV1
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode %s: %v", fixturePath, err)
	}
	if fixture.FixtureContract != "distill_candidate_admission_gold_v1" || fixture.SchemaVersion != 1 {
		t.Fatalf("unexpected fixture contract: %+v", fixture)
	}
	if len(fixture.Cases) < 16 || len(fixture.Cases) > 24 {
		t.Fatalf("gold case count = %d, want 16..24", len(fixture.Cases))
	}

	seenNames := make(map[string]struct{}, len(fixture.Cases))
	for index, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if _, exists := seenNames[tc.Name]; exists || strings.TrimSpace(tc.Name) == "" {
				t.Fatalf("duplicate or empty case name at index %d: %q", index, tc.Name)
			}
			seenNames[tc.Name] = struct{}{}
			if strings.TrimSpace(tc.Dialect) == "" || strings.TrimSpace(tc.Content) == "" {
				t.Fatalf("fixture case lacks dialect/content: %+v", tc)
			}
			if tc.WantCards > 0 && strings.TrimSpace(tc.TriggerLine) == "" {
				t.Fatal("positive fixture case lacks trigger_line")
			}
			if tc.WantCards == 0 && strings.TrimSpace(tc.ForbiddenTrigger) == "" {
				t.Fatal("negative fixture case lacks forbidden_trigger")
			}

			session := candidateTestSession()
			session.SessionID = "gold-session-" + tc.Name
			session.TranscriptPath = "testdata/gold/" + tc.Name + ".jsonl"
			session.Kind = tc.Kind
			session.IsTask = tc.IsTask
			session.FilesTouched = append([]string(nil), tc.FilesTouched...)
			if tc.CheckpointOutcome != "" {
				session.Summary = &checkpointSummary{Outcome: tc.CheckpointOutcome}
			}

			first := normalizeDistillTranscriptV1(session, "main", tc.Content)
			second := normalizeDistillTranscriptV1(session, "main", tc.Content)
			if first.Unsupported || first.Overflow || !first.Recognized {
				t.Fatalf("sanitized %s fixture did not normalize safely: %+v", tc.Dialect, first)
			}
			firstCards := selectDistillCandidateCardsV1(first)
			secondCards := selectDistillCandidateCardsV1(second)
			if len(firstCards) != tc.WantCards {
				t.Fatalf("cards = %d, want %d: %+v", len(firstCards), tc.WantCards, firstCards)
			}
			if len(secondCards) != len(firstCards) {
				t.Fatalf("repeat cards = %d, want %d", len(secondCards), len(firstCards))
			}

			for cardIndex, card := range firstCards {
				if card.ID != secondCards[cardIndex].ID {
					t.Errorf("card %d ID changed across identical admission: %q != %q", cardIndex, card.ID, secondCards[cardIndex].ID)
				}
				if rendered, repeated := renderDistillCandidateCardV1(card), renderDistillCandidateCardV1(secondCards[cardIndex]); rendered != repeated {
					t.Errorf("card %d rendering changed across identical admission", cardIndex)
				}
			}

			if tc.WantCards > 0 {
				matched := false
				for _, card := range firstCards {
					for _, trigger := range card.Triggers {
						if turn := distillCandidateTurnByIDV1(card.Turns, trigger.TurnID); turn != nil && strings.Contains(turn.Text, tc.TriggerLine) {
							matched = true
						}
					}
				}
				if !matched {
					t.Fatalf("positive trigger_line %q was not an admitted trigger: %+v", tc.TriggerLine, firstCards)
				}
			}
			if tc.ForbiddenTrigger != "" {
				for _, card := range firstCards {
					for _, trigger := range card.Triggers {
						if turn := distillCandidateTurnByIDV1(card.Turns, trigger.TurnID); turn != nil && strings.Contains(turn.Text, tc.ForbiddenTrigger) {
							t.Fatalf("forbidden trigger %q overlapped admitted card: %+v", tc.ForbiddenTrigger, card)
						}
					}
				}
			}
		})
	}
}

func distillCandidateTurnByIDV1(turns []distillNormalizedTurnV1, id string) *distillNormalizedTurnV1 {
	for index := range turns {
		if turns[index].ID == id {
			return &turns[index]
		}
	}
	return nil
}
