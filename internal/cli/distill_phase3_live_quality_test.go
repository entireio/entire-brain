package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDistillPhase3LiveQualityAuthorityFailures is the retained, sanitized
// counterpart to the authority failures manually found in the live corpus.
// These are deliberately transcript-shaped fixtures, rather than calls to a
// provider: candidate admission must suppress them before extraction.
func TestDistillPhase3LiveQualityAuthorityFailures(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		kind      string
		isTask    bool
		wantCards int
		forbidden string
	}{
		{
			name:      "false_reviewer_preference_claude_review_session",
			kind:      "agent_review",
			content:   `{"type":"user","message":{"role":"user","content":"I prefer every review to require a migration diagram."}}`,
			forbidden: "I prefer every review to require a migration diagram.",
		},
		{
			name:      "slash_expanded_reviewer_instruction_is_injected_codex_mechanics",
			content:   `{"type":"custom_message","customType":"review-settle-monitor","content":"/review: Never approve without a migration diagram.","display":true}`,
			forbidden: "Never approve without a migration diagram.",
		},
		{
			name:      "task_local_review_convention_pi",
			isTask:    true,
			content:   `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"For this review only, always require a migration diagram."}]}}`,
			forbidden: "For this review only, always require a migration diagram.",
		},
		{
			name:      "ordinary_user_preference_opencode_control",
			content:   `{"info":{"id":"ses_live_quality"},"messages":[{"info":{"role":"user"},"parts":[{"type":"text","text":"I prefer migration diagrams for schema changes."}]}]}`,
			wantCards: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := candidateTestSession()
			session.SessionID = "live-quality-" + tc.name
			session.TranscriptPath = "testdata/phase3_live_quality/" + tc.name + ".jsonl"
			session.Kind = tc.kind
			session.IsTask = tc.isTask

			transcript := normalizeDistillTranscriptV1(session, "main", tc.content)
			if transcript.Unsupported || transcript.Overflow || !transcript.Recognized {
				t.Fatalf("sanitized fixture did not normalize safely: %+v", transcript)
			}
			cards := selectDistillCandidateCardsV1(transcript)
			if len(cards) != tc.wantCards {
				t.Fatalf("candidate cards = %d, want %d: %+v", len(cards), tc.wantCards, cards)
			}
			if tc.forbidden == "" {
				return
			}
			for _, card := range cards {
				for _, trigger := range card.Triggers {
					turn := distillCandidateTurnByIDV1(card.Turns, trigger.TurnID)
					if turn != nil && strings.Contains(turn.Text, tc.forbidden) {
						t.Fatalf("forbidden live-corpus failure became an extraction trigger: %+v", card)
					}
				}
			}
		})
	}
}

// TestDistillPhase3LiveQualityReconciliationGate locks the documented
// live-corpus bad-reconciliation shapes behind the real candidate, apply, and
// neutral-relationship paths. Different fact IDs never cause executable
// supersession. The one allowed relation demonstrates the narrow positive:
// same branch/kind/top-level plus the exact code-like GraphNodeID locus.
func TestDistillPhase3LiveQualityReconciliationGate(t *testing.T) {
	now := time.Date(2026, time.August, 23, 21, 0, 0, 0, time.UTC)
	transcript := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always run the graph snapshot with --no-network."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always invoke graph snapshots with the --no-network flag."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep repoKey and symbol IDs distinct."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"The entire sem snapshot --no-network contract must remain explicit."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"The entire graph index --force rebuild behavior must remain documented."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"GraphNodeID must be validated before writing."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"GraphNodeID must remain stable during compaction."}}`,
	}, "\n")
	brainDir := writeSingleSessionFixture(t, now, transcript)
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}

	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"phase3-live-quality-fake"}, pipeline: distillPipelineCandidates,
		model: "phase3-live-quality", effort: "low", maxChunkBytes: defaultDistillChunkSize,
		timeout: time.Minute, concurrency: 1,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			ids := phase3LiveQualityCandidateTexts(t, input)
			out := make([]string, 0, len(ids))
			for id, text := range ids {
				switch {
				case strings.Contains(text, "run the graph snapshot with --no-network"):
					out = append(out, id+"\tinvariant\tconstraints.invariants.general\tThe graph snapshot uses --no-network.")
				case strings.Contains(text, "invoke graph snapshots with the --no-network flag"):
					out = append(out, id+"\tinvariant\tconstraints.invariants.general\tGraph snapshots retain the --no-network flag.")
				case strings.Contains(text, "keep repoKey and symbol IDs distinct"):
					out = append(out, id+"\tinvariant\tconstraints.invariants.general\t`repoKey` identifies repositories; symbol IDs are separate.")
				case strings.Contains(text, "entire sem snapshot --no-network contract"):
					out = append(out, id+"\tgotcha\tconstraints.invariants.general\t`entire sem snapshot --no-network` does not enforce the network contract.")
				case strings.Contains(text, "entire graph index --force rebuild behavior"):
					out = append(out, id+"\tgotcha\tconstraints.invariants.general\t`entire graph index --force` always rebuilds the snapshot.")
				case strings.Contains(text, "GraphNodeID must be validated before writing"):
					out = append(out, id+"\tinvariant\tarchitecture.data.flow\tThe graph writer validates GraphNodeID before writing.")
				case strings.Contains(text, "GraphNodeID must remain stable during compaction"):
					out = append(out, id+"\tinvariant\tarchitecture.data.flow\tGraphNodeID remains stable during graph compaction.")
				default:
					t.Fatalf("unexpected candidate card: %q", text)
				}
			}
			sort.Strings(out)
			return strings.Join(out, "\n"), nil
		},
	}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("candidate apply: %v", err)
	}
	if calls != 1 || source.ExtractionCalls != 1 || source.CandidateMembers != 7 || source.CandidateRelationships != 1 {
		t.Fatalf("candidate apply summary = calls:%d source:%+v", calls, source)
	}

	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 7 {
		t.Fatalf("active live-quality facts = %d, want seven distinct facts: %+v", len(facts), facts)
	}
	byText := make(map[string]factRecord, len(facts))
	for _, fact := range facts {
		if fact.Status != factStatusActive || fact.SupersededBy != "" || len(fact.RelatedIDs) != 0 {
			t.Fatalf("different-id candidate mutated durable fact state: %+v", fact)
		}
		byText[fact.Text] = fact
	}
	noNetworkA := phase3LiveQualityFact(t, byText, "The graph snapshot uses --no-network.")
	noNetworkB := phase3LiveQualityFact(t, byText, "Graph snapshots retain the --no-network flag.")
	repoKey := phase3LiveQualityFact(t, byText, "`repoKey` identifies repositories; symbol IDs are separate.")
	semSnapshot := phase3LiveQualityFact(t, byText, "`entire sem snapshot --no-network` does not enforce the network contract.")
	graphIndex := phase3LiveQualityFact(t, byText, "`entire graph index --force` always rebuilds the snapshot.")
	graphBefore := phase3LiveQualityFact(t, byText, "The graph writer validates GraphNodeID before writing.")
	graphStable := phase3LiveQualityFact(t, byText, "GraphNodeID remains stable during graph compaction.")
	for _, pair := range [][2]factRecord{{noNetworkA, noNetworkB}, {noNetworkA, repoKey}} {
		if pair[0].ID == pair[1].ID {
			t.Fatalf("bad-reconciliation pair unexpectedly exact-merged: %+v", pair)
		}
	}

	proposals, err := loadFactProposals(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 || source.Proposals != 0 {
		t.Fatalf("candidate mode emitted executable merge/supersede proposals: source=%+v proposals=%+v", source, proposals)
	}

	store, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 {
		t.Fatalf("neutral relationships = %+v, want only GraphNodeID positive", store.entries)
	}
	for _, relation := range store.entries {
		if relation.Subject.StrongLocus != "graphnodeid" || relation.Subject.Kind != factKindInvariant || relation.Subject.TopLevel != "architecture" || len(relation.FactIDs) != 2 || !phase3LiveQualitySameIDs(relation.FactIDs, graphBefore.ID, graphStable.ID) {
			t.Fatalf("relationship did not retain the narrow pairwise positive: %+v", relation)
		}
		for _, forbidden := range []string{noNetworkA.ID, noNetworkB.ID, repoKey.ID, semSnapshot.ID, graphIndex.ID} {
			for _, id := range relation.FactIDs {
				if id == forbidden {
					t.Fatalf("unrelated live-corpus fact became neutral relationship evidence: %+v", relation)
				}
			}
		}
	}
}

func phase3LiveQualityCandidateTexts(t *testing.T, input []byte) map[string]string {
	t.Helper()
	texts := make(map[string]string)
	for _, line := range bytes.Split(input, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type        string `json:"type"`
			CandidateID string `json:"candidate_id"`
			Text        string `json:"text"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("parse packed candidate input: %v", err)
		}
		switch record.Type {
		case "distill_candidate_v1":
			if record.CandidateID == "" {
				t.Fatal("candidate card is missing an ID")
			}
			texts[record.CandidateID] = ""
		case "distill_candidate_turn_v1":
			texts[record.CandidateID] += record.Text
		}
	}
	if len(texts) != 7 {
		t.Fatalf("packed live-quality candidates = %d, want seven: %+v", len(texts), texts)
	}
	return texts
}

func phase3LiveQualityFact(t *testing.T, facts map[string]factRecord, text string) factRecord {
	t.Helper()
	fact, ok := facts[text]
	if !ok {
		t.Fatalf("missing retained fact %q in %+v", text, facts)
	}
	return fact
}

func phase3LiveQualitySameIDs(ids []string, left, right string) bool {
	if len(ids) != 2 {
		return false
	}
	return (ids[0] == left && ids[1] == right) || (ids[0] == right && ids[1] == left)
}
