package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCandidateApplyV2UsesPackedCacheAndExactOnlyReceipts(t *testing.T) {
	now := time.Date(2026, time.August, 23, 14, 0, 0, 0, time.UTC)
	transcript := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Never skip race tests."}}`,
	}, "\n")
	brainDir := writeSingleSessionFixture(t, now, transcript)
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}
	paths := []string{"workflow.testing.rules"}
	legacy := factRecord{
		ID: factRecordID("Always keep migrations reversible.", paths), Paths: paths,
		Kind: factKindConvention, Text: "Always keep migrations reversible.", Branch: "main",
		Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "legacy-session", DistillTurnID: "turn-v1:legacy", Transcript: "sessions/main/legacy.jsonl", Line: 1}},
		CreatedAt:  now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := writeFacts(brainDir, "main", []factRecord{legacy}); err != nil {
		t.Fatal(err)
	}
	legacyCache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{"main/legacy": "sha256:legacy"}}
	if err := saveDistillCache(brainDir, legacyCache); err != nil {
		t.Fatal(err)
	}
	legacyCacheBytes, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(distillCachePath)))
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	var firstIDs []string
	returnNoFacts := false
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "fixed-model", effort: "low", maxChunkBytes: 1, // ignored by fixed candidate-v2 packing
		timeout: time.Minute, concurrency: 2,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls.Add(1)
			ids := candidateIDsFromPackedInputV2(t, input)
			if len(firstIDs) == 0 {
				firstIDs = append([]string(nil), ids...)
			}
			lines := make([]string, 0, len(ids))
			for index, id := range ids {
				if returnNoFacts && index == len(ids)-1 {
					lines = append(lines, id+"\tNO_FACTS")
					continue
				}
				text := "Always keep migrations reversible."
				if index == len(ids)-1 {
					text = "Never skip race tests."
				}
				lines = append(lines, id+"\tconvention\tworkflow.testing.rules\t"+text)
			}
			return strings.Join(lines, "\n"), nil
		},
	}
	plan, err := buildDistillDryRunReport(brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Shadow || plan.CandidateMembers != 2 || plan.CandidatePacks != 1 || plan.ExtractionAgentCalls != 1 || plan.ReconcileAgentCallsUpperBound != 0 {
		t.Fatalf("write-mode dry-run did not use the framed v2 plan: %+v", plan)
	}

	first, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first.ChunksScanned != 1 || first.ChunksDistilled != 1 || first.CandidatePacks != 1 || first.CandidateMembers != 2 || first.ExtractionCalls != 1 || first.ReconcileCalls != 0 || first.Shadow {
		t.Fatalf("first candidate apply summary/calls = calls:%d source:%+v", calls.Load(), first)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts after exact-only apply = %+v", facts)
	}
	exact := facts[indexOfFact(facts, legacy.ID)]
	if len(exact.Provenance) != 2 || exact.Provenance[0].DistillTurnID != "turn-v1:legacy" || exact.Provenance[1].DistillTurnID != distillCandidateApplicationAnchorIDV2(firstIDs[0], false) {
		t.Fatalf("exact identity did not preserve legacy and v2 ownership: %+v", exact.Provenance)
	}
	if proposals, err := loadFactProposals(brainDir, "main"); err != nil || len(proposals) != 0 {
		t.Fatalf("exact-only candidate apply queued proposals: %+v err=%v", proposals, err)
	}
	receipts, err := loadDistillApplicationReceiptStoreV2(brainDir)
	if err != nil || len(receipts.entries) != 2 {
		t.Fatalf("application receipts = %d err=%v", len(receipts.entries), err)
	}
	if got, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(distillCachePath))); err != nil || !reflect.DeepEqual(got, legacyCacheBytes) {
		t.Fatalf("candidate v2 changed legacy cache: err=%v\nwant=%q\ngot=%q", err, legacyCacheBytes, got)
	}
	plan, err = buildDistillDryRunReport(brainDir, opts, now.Add(time.Minute))
	if err != nil || plan.CandidateCacheHits != 2 || plan.CandidatePacks != 0 || plan.ExtractionAgentCalls != 0 {
		t.Fatalf("cached write-mode dry-run = %+v err=%v", plan, err)
	}

	factsBytes, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || second.ExtractionCalls != 0 || second.CandidateCacheHits != 2 {
		t.Fatalf("cached application was not zero-call: calls=%d source=%+v", calls.Load(), second)
	}
	gotFactsBytes, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil || !reflect.DeepEqual(gotFactsBytes, factsBytes) {
		t.Fatalf("no-op application rewrote facts: err=%v\nwant=%q\ngot=%q", err, factsBytes, gotFactsBytes)
	}

	// A model change invalidates extraction but not the application slot. A
	// protocol-valid empty result retracts only that candidate's v2-owned fact.
	returnNoFacts = true
	opts.model = "fixed-model-v2"
	third, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || third.ExtractionCalls != 1 || third.CandidateEmptyResults != 1 {
		t.Fatalf("changed result extraction = calls:%d source:%+v", calls.Load(), third)
	}
	facts, err = loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].ID != legacy.ID || len(facts[0].Provenance) != 2 {
		t.Fatalf("empty result removed legacy/exact materialization incorrectly: %+v", facts)
	}
}

func TestCandidateApplyV2RelocationUsesCacheAndUpdatesOnlyV2Anchor(t *testing.T) {
	now := time.Date(2026, time.August, 23, 15, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`
	brainDir := writeSingleSessionFixture(t, now, transcript)
	var calls int
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			id := candidateIDsFromPackedInputV2(t, input)[0]
			return id + "\tconvention\tworkflow.testing.rules\tAlways keep migrations reversible.", nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || facts[0].Provenance[0].Line != 1 {
		t.Fatalf("initial facts = %+v err=%v", facts, err)
	}
	rel := "sessions/main/s1.jsonl"
	relocated := `{"type":"session_meta","payload":{"id":"s1"}}` + "\n" + transcript
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(rel)), []byte(relocated), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("mechanics-only relocation made %d provider calls, want one total", calls)
	}
	facts, err = loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || len(facts[0].Provenance) != 1 || facts[0].Provenance[0].Line != 2 {
		t.Fatalf("relocated v2 provenance = %+v err=%v", facts, err)
	}
}

func TestCandidateApplyV2CrashBeforeReceiptDoesNotRetainChangedResult(t *testing.T) {
	now := time.Date(2026, time.August, 23, 16, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`
	brainDir := writeSingleSessionFixture(t, now, transcript)
	outputText := "Always keep migrations reversible."
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "model-a", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			id := candidateIDsFromPackedInputV2(t, input)[0]
			return id + "\tconvention\tworkflow.testing.rules\t" + outputText, nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 {
		t.Fatalf("initial facts = %+v err=%v", facts, err)
	}
	// Model a crash after result B replaced the facts file but before its
	// application receipt replaced result A's receipt.
	unreceipted := facts[0]
	unreceipted.Text = "Unreceipted result B."
	unreceipted.ID = factRecordID(unreceipted.Text, unreceipted.Paths)
	unreceipted.Locus = factLocus(unreceipted.Text)
	unreceipted.UpdatedAt = now.Add(time.Minute)
	if err := writeFacts(brainDir, "main", []factRecord{unreceipted}); err != nil {
		t.Fatal(err)
	}

	outputText = "Current result C."
	opts.model = "model-c"
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	facts, err = loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || facts[0].Text != outputText {
		t.Fatalf("crash retry retained an unreceipted result: %+v err=%v", facts, err)
	}
}

func TestCandidateApplyV2PrunesReceiptlessResultWhenCandidateDisappears(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`
	brainDir := writeSingleSessionFixture(t, now, transcript)
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			id := candidateIDsFromPackedInputV2(t, input)[0]
			return id + "\tconvention\tworkflow.testing.rules\tAlways keep migrations reversible.", nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	// Model a first application whose facts write succeeded but receipt write
	// did not. The next canonical view no longer contains the candidate.
	if err := saveDistillApplicationReceiptStoreV2(brainDir, newDistillApplicationReceiptStoreV2()); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	neutral := `{"type":"event_msg","payload":{"type":"user_message","message":"Thanks."}}`
	if err := os.WriteFile(rel, []byte(neutral), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil || len(facts) != 0 {
		t.Fatalf("receiptless removed candidate retained facts: %+v err=%v", facts, err)
	}
}

func TestCandidateApplyV2PreservesAnchorlessLegacyExactFact(t *testing.T) {
	now := time.Date(2026, time.August, 23, 18, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`
	brainDir := writeSingleSessionFixture(t, now, transcript)
	paths := []string{"workflow.testing.rules"}
	legacy := factRecord{
		ID: factRecordID("Always keep migrations reversible.", paths), Paths: paths,
		Kind: factKindConvention, Text: "Always keep migrations reversible.", Branch: "main",
		Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := writeFacts(brainDir, "main", []factRecord{legacy}); err != nil {
		t.Fatal(err)
	}
	noFacts := false
	var candidateID string
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "model-a", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			candidateID = candidateIDsFromPackedInputV2(t, input)[0]
			if noFacts {
				return candidateID + "\tNO_FACTS", nil
			}
			return candidateID + "\tconvention\tworkflow.testing.rules\tAlways keep migrations reversible.", nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || len(facts[0].Provenance) != 1 || facts[0].Provenance[0].DistillTurnID != distillCandidateApplicationAnchorIDV2(candidateID, false) {
		t.Fatalf("exact legacy fact did not receive shared v2 ownership: %+v err=%v", facts, err)
	}

	noFacts = true
	opts.model = "model-b"
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	facts, err = loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || facts[0].ID != legacy.ID || len(facts[0].Provenance) != 0 {
		t.Fatalf("NO_FACTS removed an anchorless legacy fact: %+v err=%v", facts, err)
	}
}

func TestCandidateApplicationOwnershipPropagatesAcrossExactV2Matches(t *testing.T) {
	now := time.Date(2026, time.August, 23, 19, 0, 0, 0, time.UTC)
	result := distillCandidateCacheResultV2{Facts: []distillCandidateCachedFactV2{{
		Kind: factKindConvention, Paths: []string{"workflow.testing.rules"}, Text: "Always keep migrations reversible.",
	}}}
	makeTarget := func(label string) distillCandidateMaterializationTargetV2 {
		candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, label)
		return distillCandidateMaterializationTargetV2{
			CandidateID: candidateID, Branch: "main", SourceSessionID: "s-" + label,
			SourceTranscript: "sessions/main/" + label + ".jsonl",
			TriggerTurnID:    distillCandidateApplicationAnchorIDV2(candidateID, false), StartLine: 1, EndLine: 1,
		}
	}
	var active []factRecord
	for _, label := range []string{"a", "b"} {
		target := makeTarget(label)
		materialized, err := materializeDistillCandidateResultV2(result, target, now)
		if err != nil {
			t.Fatal(err)
		}
		materialized = assignDistillApplicationAnchorOwnershipV2(materialized, active, target.SourceSessionID, target.CandidateID)
		active, _ = applyFactActions(active, newDistilledFactActions(materialized.Facts), defaultFactConfidenceThreshold, now)
	}
	if len(active) != 1 || len(active[0].Provenance) != 2 {
		t.Fatalf("exact v2 matches did not converge: %+v", active)
	}
	for _, anchor := range active[0].Provenance {
		_, owned, ok := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
		if !ok || !owned {
			t.Fatalf("v2-created ownership did not propagate: %+v", active[0].Provenance)
		}
	}
	for _, label := range []string{"a", "b"} {
		target := makeTarget(label)
		active, _ = removeDistillApplicationSlotAnchorsV2(active, target.SourceSessionID, target.CandidateID)
		if label == "a" && len(active) != 1 {
			t.Fatalf("first owner removal deleted corroborated fact: %+v", active)
		}
	}
	if len(active) != 0 {
		t.Fatalf("last v2 owner removal retained an unowned fact: %+v", active)
	}
}
