package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestCandidateApplyV2Phase3BParityAndBadReconcile exercises the complete
// exact-write seam around the Phase 3B boundary.  The two candidate facts are
// deliberately unrelated despite sharing one taxonomy path: a --no-network
// constraint and a repository-key/symbol-ID identity fact must remain two
// facts, with no executable merge/supersede proposal.
func TestCandidateApplyV2Phase3BParityAndBadReconcile(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	transcript := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always use the graph snapshot with --no-network."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep repository-key and symbol IDs distinct."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
	}, "\n")

	incremental := phase3BParityFixture(t, now, transcript)
	clean := phase3BParityFixture(t, now, transcript)

	incrementalRuns := 0
	firstOpts := phase3BParityCandidateOptions(t, &incrementalRuns, false)
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, firstOpts, now); err != nil {
		t.Fatalf("initial incremental candidate apply: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateMembers != 2 {
		t.Fatalf("initial incremental summary = %+v, want one packed call and two candidates", source)
	}

	initialFacts := phase3BFactSnapshot(t, incremental, "main")
	initialReceipts := phase3BReceiptSnapshot(t, incremental)
	if len(initialFacts) != 4 {
		t.Fatalf("initial fact count = %d, want legacy/authored plus two v2 facts: %+v", len(initialFacts), initialFacts)
	}
	phase3BAssertExactCandidateFacts(t, initialFacts, true)
	phase3BAssertNoExecutableProposals(t, incremental, "main")
	phase3BAssertNoNeutralRelationships(t, incremental)

	// A replay against the unchanged source must be a true no-op: cache/receipt
	// hits do not rewrite facts or manufacture another proposal.
	factsBytesBefore, err := os.ReadFile(filepath.Join(incremental, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	receiptsBytesBefore, err := os.ReadFile(filepath.Join(incremental, filepath.FromSlash(distillApplicationReceiptsV2Path)))
	if err != nil {
		t.Fatal(err)
	}
	secondOpts := phase3BParityCandidateOptions(t, &incrementalRuns, false)
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, secondOpts, now.Add(time.Minute)); err != nil {
		t.Fatalf("candidate no-op replay: %v", err)
	} else if source.ExtractionCalls != 0 || source.CandidateCacheHits != 2 {
		t.Fatalf("no-op replay summary = %+v, want zero provider calls and two cache hits", source)
	}
	factsBytesAfter, err := os.ReadFile(filepath.Join(incremental, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	receiptsBytesAfter, err := os.ReadFile(filepath.Join(incremental, filepath.FromSlash(distillApplicationReceiptsV2Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(factsBytesAfter, factsBytesBefore) || !bytes.Equal(receiptsBytesAfter, receiptsBytesBefore) {
		t.Fatalf("no-op replay rewrote exact facts or application receipts:\nfacts before=%q\nfacts after=%q\nreceipts before=%q\nreceipts after=%q", factsBytesBefore, factsBytesAfter, receiptsBytesBefore, receiptsBytesAfter)
	}
	if got := phase3BFactSnapshot(t, incremental, "main"); !reflect.DeepEqual(got, initialFacts) {
		t.Fatalf("no-op replay changed normalized facts:\nwant=%+v\ngot=%+v", initialFacts, got)
	}
	if got := phase3BReceiptSnapshot(t, incremental); !reflect.DeepEqual(got, initialReceipts) {
		t.Fatalf("no-op replay changed normalized receipts:\nwant=%+v\ngot=%+v", initialReceipts, got)
	}

	// Force rebuild with a clean negative result for the second candidate.  The
	// first candidate remains, while the old second fact and its v2 anchor must
	// disappear.  The separate clean run is the parity oracle for the same
	// final candidate set.
	forceRuns := 0
	forceOpts := phase3BParityCandidateOptions(t, &forceRuns, true)
	forceOpts.force = true
	forceOpts.model = "phase3b-force-model"
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, forceOpts, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("incremental force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateEmptyResults != 1 {
		t.Fatalf("incremental force summary = %+v, want one packed call and one empty candidate", source)
	}
	gotForce := phase3BFactSnapshot(t, incremental, "main")
	phase3BAssertExactCandidateFacts(t, gotForce, false)
	phase3BAssertNoExecutableProposals(t, incremental, "main")
	phase3BAssertNoNeutralRelationships(t, incremental)
	phase3BAssertNoStaleV2Candidate(t, gotForce, "repository-key and symbol IDs remain distinct")

	cleanRuns := 0
	cleanOpts := phase3BParityCandidateOptions(t, &cleanRuns, true)
	cleanOpts.force = true
	cleanOpts.model = "phase3b-force-model"
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), clean, cleanOpts, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("clean force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateEmptyResults != 1 {
		t.Fatalf("clean force summary = %+v, want one packed call and one empty candidate", source)
	}
	wantForce := phase3BFactSnapshot(t, clean, "main")
	if !reflect.DeepEqual(gotForce, wantForce) {
		t.Fatalf("incremental force and clean rebuild diverged:\nwant=%+v\ngot=%+v", wantForce, gotForce)
	}
	if got, want := phase3BReceiptSnapshot(t, incremental), phase3BReceiptSnapshot(t, clean); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental force and clean receipts diverged after generation normalization:\nwant=%+v\ngot=%+v", want, got)
	}
	phase3BAssertNoExecutableProposals(t, clean, "main")
	phase3BAssertNoNeutralRelationships(t, clean)
}

func phase3BParityFixture(t *testing.T, now time.Time, transcript string) string {
	t.Helper()
	brainDir := writeSingleSessionFixture(t, now, transcript)
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}
	legacyText := "Legacy distillation remains available."
	authoredText := "Authored facts survive candidate rebuilds."
	path := []string{"architecture.data.flow"}
	facts := []factRecord{
		{
			ID: factRecordID(legacyText, path), Paths: path, Kind: factKindConvention, Text: legacyText,
			Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
			Provenance: []factAnchor{{SessionID: "legacy-session", DistillTurnID: "turn-v1:legacy", Transcript: "sessions/main/legacy.jsonl", Line: 1}},
			CreatedAt:  now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		},
		{
			ID: factRecordID(authoredText, path), Paths: path, Kind: factKindPreference, Text: authoredText,
			Branch: "main", Origin: factOriginAuthored, Status: factStatusActive,
			Provenance: []factAnchor{{SessionID: "authored-session", Transcript: "manual", Line: 1}},
			CreatedAt:  now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		},
	}
	if err := writeFacts(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func phase3BParityCandidateOptions(t *testing.T, calls *int, emptySecond bool) distillCommandOptions {
	t.Helper()
	return distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates,
		model: "phase3b-model", effort: "low", maxChunkBytes: defaultDistillChunkSize,
		timeout: time.Minute, concurrency: 1,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			(*calls)++
			ids := phase3BIDsByCardText(t, input)
			lines := make([]string, 0, len(ids))
			for id, cardText := range ids {
				switch {
				case strings.Contains(cardText, "--no-network"):
					lines = append(lines, id+"\tinvariant\tarchitecture.data.flow\tThe graph snapshot uses --no-network.")
				case strings.Contains(cardText, "repository-key"):
					if emptySecond {
						lines = append(lines, id+"\tNO_FACTS")
					} else {
						lines = append(lines, id+"\tdecision\tarchitecture.data.flow\tRepository-key and symbol IDs remain distinct.")
					}
				default:
					return "", &phase3BUnexpectedCandidateError{cardText: cardText}
				}
			}
			sort.Strings(lines)
			return strings.Join(lines, "\n"), nil
		},
	}
}

type phase3BUnexpectedCandidateError struct{ cardText string }

func (e *phase3BUnexpectedCandidateError) Error() string {
	return "unexpected candidate card in Phase 3B parity fixture: " + e.cardText
}

func phase3BIDsByCardText(t *testing.T, input []byte) map[string]string {
	t.Helper()
	ids := make(map[string]string)
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
			t.Fatalf("parse packed candidate card: %v", err)
		}
		if record.Type == "distill_candidate_v1" {
			if record.CandidateID == "" {
				t.Fatal("candidate header has no ID")
			}
			ids[record.CandidateID] = ""
		} else if record.Type == "distill_candidate_turn_v1" {
			ids[record.CandidateID] += record.Text
		}
	}
	if len(ids) == 0 {
		t.Fatal("packed candidate input contains no cards")
	}
	return ids
}

type phase3BFactView struct {
	ID, Text, Kind, Branch, Origin, Status string
	Paths                                  []string
	Provenance                             []factAnchor
}

func phase3BFactSnapshot(t *testing.T, brainDir, branch string) []phase3BFactView {
	t.Helper()
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]phase3BFactView, 0, len(facts))
	for _, fact := range facts {
		view := phase3BFactView{ID: fact.ID, Text: fact.Text, Kind: fact.Kind, Branch: fact.Branch, Origin: fact.Origin, Status: fact.Status, Paths: append([]string(nil), fact.Paths...), Provenance: append([]factAnchor(nil), fact.Provenance...)}
		view.Provenance = phase3BSortedAnchors(view.Provenance)
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func phase3BSortedAnchors(anchors []factAnchor) []factAnchor {
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i].SessionID != anchors[j].SessionID {
			return anchors[i].SessionID < anchors[j].SessionID
		}
		return anchors[i].DistillTurnID < anchors[j].DistillTurnID
	})
	return anchors
}

func phase3BReceiptSnapshot(t *testing.T, brainDir string) map[string]distillApplicationReceiptV2 {
	t.Helper()
	store, err := loadDistillApplicationReceiptStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]distillApplicationReceiptV2, len(store.entries))
	for slotID, receipt := range store.entries {
		receipt.ReceiptID = ""
		receipt.Identity.FactStoreGeneration = ""
		out[slotID] = receipt
	}
	return out
}

func phase3BAssertExactCandidateFacts(t *testing.T, facts []phase3BFactView, wantSecond bool) {
	t.Helper()
	noNetwork, second := 0, 0
	var noNetworkID, secondID string
	for _, fact := range facts {
		if fact.Text == "The graph snapshot uses --no-network." {
			noNetwork++
			noNetworkID = fact.ID
			if fact.Origin != factOriginDistilled || fact.Status != factStatusActive || fact.Kind != factKindInvariant || !reflect.DeepEqual(fact.Paths, []string{"architecture.data.flow"}) || len(fact.Provenance) == 0 {
				t.Fatalf("--no-network fact lost exact state/provenance: %+v", fact)
			}
		}
		if fact.Text == "Repository-key and symbol IDs remain distinct." {
			second++
			secondID = fact.ID
		}
	}
	if noNetwork != 1 || (wantSecond && second != 1) || (!wantSecond && second != 0) {
		t.Fatalf("candidate fact counts no-network=%d second=%d wantSecond=%v: %+v", noNetwork, second, wantSecond, facts)
	}
	if wantSecond && noNetworkID == secondID {
		t.Fatalf("unrelated candidate facts unexpectedly shared one ID: %q", noNetworkID)
	}
}

func phase3BAssertNoStaleV2Candidate(t *testing.T, facts []phase3BFactView, text string) {
	t.Helper()
	retained := make(map[string]struct{})
	for _, fact := range facts {
		if fact.Text != "The graph snapshot uses --no-network." {
			continue
		}
		for _, anchor := range fact.Provenance {
			if candidateID, _, ok := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID); ok {
				retained[candidateID] = struct{}{}
			}
		}
	}
	for _, fact := range facts {
		if fact.Text == text {
			t.Fatalf("stale candidate fact survived force rebuild: %+v", fact)
		}
		for _, anchor := range fact.Provenance {
			if candidateID, _, ok := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID); ok {
				if _, keep := retained[candidateID]; keep {
					continue
				}
				t.Fatalf("stale v2 anchor survived force rebuild: %+v", anchor)
			}
		}
	}
}

func phase3BAssertNoExecutableProposals(t *testing.T, brainDir, branch string) {
	t.Helper()
	proposals, err := loadFactProposals(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Fatalf("exact candidate write emitted executable proposals: %+v", proposals)
	}
}

func phase3BAssertNoNeutralRelationships(t *testing.T, brainDir string) {
	t.Helper()
	store, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("bad-reconcile fixture emitted neutral relationships: %+v", store.entries)
	}
}
