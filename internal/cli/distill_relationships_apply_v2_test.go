package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCandidateApplyV2MaintainsNeutralRelationships(t *testing.T) {
	now := time.Date(2026, time.August, 23, 19, 0, 0, 0, time.UTC)
	transcript := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always validate GraphNodeID before storage."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"GraphNodeID must remain stable across compaction."}}`,
	}, "\n")
	brainDir := writeSingleSessionFixture(t, now, transcript)
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}

	calls := 0
	emptySecond := false
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "relationship-model", effort: "low", maxChunkBytes: defaultDistillChunkSize,
		timeout: time.Minute, concurrency: 1,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			ids := phase3BIDsByCardText(t, input)
			lines := make([]string, 0, len(ids))
			for id, cardText := range ids {
				switch {
				case strings.Contains(cardText, "before storage"):
					lines = append(lines, id+"\tinvariant\tarchitecture.data.flow\tThe graph writer validates GraphNodeID before storage.")
				case strings.Contains(cardText, "across compaction"):
					if emptySecond {
						lines = append(lines, id+"\tNO_FACTS")
					} else {
						lines = append(lines, id+"\tinvariant\tarchitecture.data.flow\tGraphNodeID remains stable across graph compaction.")
					}
				default:
					return "", &phase3BUnexpectedCandidateError{cardText: cardText}
				}
			}
			sort.Strings(lines)
			return strings.Join(lines, "\n"), nil
		},
	}

	first, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.ExtractionCalls != 1 || first.CandidateRelationships != 1 {
		t.Fatalf("first relationship apply = calls:%d source:%+v", calls, first)
	}
	assertNeutralRelationshipStateV2(t, brainDir, 1, 2)
	relPath := filepath.Join(brainDir, filepath.FromSlash(distillRelationshipStoreV2Path))
	firstBytes, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}

	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || second.ExtractionCalls != 0 || second.CandidateRelationships != 1 || !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("no-op relationship replay changed state: calls=%d source=%+v\nbefore=%q\nafter=%q", calls, second, firstBytes, secondBytes)
	}

	// Facts and application receipts are deliberately committed before the
	// advisory relationship store. Simulate a crash in that final window: an
	// unchanged retry must rebuild the missing local observation entirely from
	// live facts/anchors without another provider call.
	if err := saveDistillRelationshipStoreV2(brainDir, newDistillRelationshipStoreV2()); err != nil {
		t.Fatal(err)
	}
	recovered, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	recoveredBytes, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || recovered.ExtractionCalls != 0 || recovered.CandidateRelationships != 1 || !bytes.Equal(firstBytes, recoveredBytes) {
		t.Fatalf("relationship crash-window retry did not converge: calls=%d source=%+v\nwant=%q\ngot=%q", calls, recovered, firstBytes, recoveredBytes)
	}

	if err := os.WriteFile(relPath, []byte("not relationship json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(105*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	rebuiltBytes, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || rebuilt.ExtractionCalls != 0 || rebuilt.CandidateRelationships != 1 || !bytes.Equal(firstBytes, rebuiltBytes) {
		t.Fatalf("corrupt relationship store did not rebuild without provider work: calls=%d source=%+v\nwant=%q\ngot=%q", calls, rebuilt, firstBytes, rebuiltBytes)
	}
	if !strings.Contains(strings.Join(rebuilt.Warnings, "\n"), "neutral relationship store was unreadable and rebuilt") {
		t.Fatalf("relationship rebuild warning = %q", rebuilt.Warnings)
	}

	force := opts
	force.force = true
	force.model = "relationship-force-model"
	third, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, force, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	forceBytes, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || third.CandidateRelationships != 1 || !bytes.Equal(firstBytes, forceBytes) {
		t.Fatalf("force relationship rebuild diverged: calls=%d source=%+v\nwant=%q\ngot=%q", calls, third, firstBytes, forceBytes)
	}

	emptySecond = true
	opts.model = "relationship-empty-model"
	fourth, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || fourth.CandidateRelationships != 0 || fourth.CandidateEmptyResults != 1 {
		t.Fatalf("NO_FACTS relationship cleanup = calls:%d source:%+v", calls, fourth)
	}
	assertNeutralRelationshipStateV2(t, brainDir, 0, 0)
}

func TestCandidateApplyV2SkipsDenseRelationshipBlockWithoutFailingFacts(t *testing.T) {
	now := time.Date(2026, time.August, 23, 20, 0, 0, 0, time.UTC)
	const candidateCount = 92
	lines := make([]string, 0, candidateCount*2)
	for index := 0; index < candidateCount; index++ {
		lines = append(lines,
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":"DenseSubject rule %03d must remain active."}}`, index),
			`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		)
	}
	brainDir := writeSingleSessionFixture(t, now, strings.Join(lines, "\n"))
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}

	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "dense-relationship-model", effort: "low", maxChunkBytes: defaultDistillChunkSize,
		timeout: time.Minute, concurrency: 1,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			ids := phase3BIDsByCardText(t, input)
			out := make([]string, 0, len(ids))
			for id, cardText := range ids {
				marker := strings.Index(cardText, "DenseSubject rule ")
				if marker < 0 || marker+len("DenseSubject rule ")+3 > len(cardText) {
					return "", &phase3BUnexpectedCandidateError{cardText: cardText}
				}
				ordinal := cardText[marker+len("DenseSubject rule ") : marker+len("DenseSubject rule ")+3]
				out = append(out, id+"\tinvariant\tarchitecture.data.flow\tDenseSubject rule "+ordinal+" remains active.")
			}
			sort.Strings(out)
			return strings.Join(out, "\n"), nil
		},
	}

	first, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	if firstCalls < 2 || first.ExtractionCalls != firstCalls || first.CandidateMembers != candidateCount || first.CandidateRelationships != 0 || first.CandidateRelationshipBlocksSkipped != 1 {
		t.Fatalf("dense relationship apply = calls:%d source:%+v", calls, first)
	}
	if len(first.Warnings) == 0 || !strings.Contains(strings.Join(first.Warnings, "\n"), "neutral relationship discovery skipped 1 bounded subject block") {
		t.Fatalf("dense relationship warning = %q", first.Warnings)
	}
	wantFacts := phase3BFactSnapshot(t, brainDir, "main")
	if len(wantFacts) != candidateCount {
		t.Fatalf("dense candidate fact count = %d, want %d", len(wantFacts), candidateCount)
	}
	wantStore, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	wantRelationships, err := marshalDistillRelationshipStoreV2(wantStore)
	if err != nil {
		t.Fatal(err)
	}

	force := opts
	force.force = true
	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, force, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	gotStore, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	gotRelationships, err := marshalDistillRelationshipStoreV2(gotStore)
	if err != nil {
		t.Fatal(err)
	}
	if calls != firstCalls*2 || second.ExtractionCalls != firstCalls || second.CandidateRelationships != 0 || second.CandidateRelationshipBlocksSkipped != 1 {
		t.Fatalf("dense force replay = calls:%d source:%+v", calls, second)
	}
	if gotFacts := phase3BFactSnapshot(t, brainDir, "main"); !reflect.DeepEqual(gotFacts, wantFacts) {
		t.Fatalf("dense force replay changed facts:\nwant=%+v\ngot=%+v", wantFacts, gotFacts)
	}
	if !bytes.Equal(gotRelationships, wantRelationships) {
		t.Fatalf("dense force replay changed bounded relationship state:\nwant=%q\ngot=%q", wantRelationships, gotRelationships)
	}
}

func assertNeutralRelationshipStateV2(t *testing.T, brainDir string, wantRelationships, wantOwners int) {
	t.Helper()
	store, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != wantRelationships {
		t.Fatalf("relationship entries = %+v, want %d", store.entries, wantRelationships)
	}
	for _, relationship := range store.entries {
		if len(relationship.Owners) != wantOwners || relationship.Subject.StrongLocus != "graphnodeid" {
			t.Fatalf("relationship evidence = %+v", relationship)
		}
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.Status != factStatusActive || len(fact.RelatedIDs) != 0 || fact.SupersededBy != "" {
			t.Fatalf("neutral relationship mutated fact state: %+v", fact)
		}
	}
	proposals, err := loadFactProposals(brainDir, "main")
	if err != nil || len(proposals) != 0 {
		t.Fatalf("neutral relationship reached executable proposals: %+v err=%v", proposals, err)
	}
}
