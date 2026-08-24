package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestCandidateDistillPhase3OperationalGate is the bounded operational exit
// harness for candidate-v2 writes. It deliberately uses the production
// extraction and application path, while the provider is a deterministic
// protocol fake. The fixture has two main-branch candidates that share a
// code-locus (and therefore produce one neutral relationship), plus one
// feature-branch candidate that proves branch/session cleanup cannot cross the
// ownership boundary.
func TestCandidateDistillPhase3OperationalGate(t *testing.T) {
	now := time.Date(2026, time.August, 23, 20, 0, 0, 0, time.UTC)
	incremental := phase3OperationalFixture(t, now)
	clean := phase3OperationalFixture(t, now)

	var calls int
	mode := 0 // 0 = facts for every card; 1 = feature or B NO_FACTS; 2 = B absent, feature NO_FACTS.
	provider := phase3OperationalProvider(t, &calls, &mode)
	base := phase3OperationalOptions(provider, "operational-v1")

	// A normal apply and a force rebuild from the same source must converge to
	// the same facts, receipts, and advisory relationships. The seeded proposal
	// is an executable legacy queue entry and must remain byte-for-byte intact.
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, base, now); err != nil {
		t.Fatalf("initial candidate apply: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateMembers != 3 || source.CandidateRelationships != 1 {
		t.Fatalf("initial operational summary = %+v, want one packed call, three members, one relationship", source)
	}

	cleanForce := base
	cleanForce.force = true
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), clean, cleanForce, now); err != nil {
		t.Fatalf("clean force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateRelationships != 1 {
		t.Fatalf("clean force summary = %+v, want one packed call and one relationship", source)
	}

	incrementalForce := base
	incrementalForce.force = true
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, incrementalForce, now); err != nil {
		t.Fatalf("incremental force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateRelationships != 1 {
		t.Fatalf("incremental force summary = %+v, want one packed call and one relationship", source)
	}
	if got, want := phase3OperationalFacts(t, incremental), phase3OperationalFacts(t, clean); got != want {
		t.Fatalf("force facts differ between incremental and clean rebuilds:\nwant=%s\ngot=%s", want, got)
	}
	if got, want := phase3OperationalRelationships(t, incremental), phase3OperationalRelationships(t, clean); got != want {
		t.Fatalf("force relationships differ between incremental and clean rebuilds:\nwant=%s\ngot=%s", want, got)
	}
	if got, want := phase3OperationalReceipts(t, incremental), phase3OperationalReceipts(t, clean); got != want {
		t.Fatalf("force receipts differ between incremental and clean rebuilds:\nwant=%s\ngot=%s", want, got)
	}
	phase3OperationalAssertNoExecutableMutation(t, incremental)
	initialRelationships := phase3OperationalRelationships(t, incremental)
	if !strings.Contains(initialRelationships, "repositorykey") {
		t.Fatalf("initial relationship snapshot lacks the shared RepositoryKey locus: %s", initialRelationships)
	}

	// An unchanged replay is a true no-op: no provider call and no rewrite of
	// any candidate-owned or executable state. Capture bytes rather than merely
	// parsing them so atomic-write churn is visible.
	factPath := filepath.Join(incremental, filepath.FromSlash(factsFileRelPath("main")))
	receiptPath := filepath.Join(incremental, filepath.FromSlash(distillApplicationReceiptsV2Path))
	relationshipPath := filepath.Join(incremental, filepath.FromSlash(distillRelationshipStoreV2Path))
	proposalPath := filepath.Join(incremental, filepath.FromSlash(factsProposalsRelPath("main")))
	before := phase3OperationalBytes(t, factPath, receiptPath, relationshipPath, proposalPath)
	callsBeforeNoOp := calls
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, base, now.Add(time.Minute)); err != nil {
		t.Fatalf("unchanged candidate replay: %v", err)
	} else if source.ExtractionCalls != 0 || source.CandidateCacheHits != 3 || calls != callsBeforeNoOp {
		t.Fatalf("no-op summary/calls = %+v/calls=%d, want zero provider calls and three cache hits", source, calls)
	}
	after := phase3OperationalBytes(t, factPath, receiptPath, relationshipPath, proposalPath)
	if !phase3OperationalEqualBytes(before, after) {
		t.Fatal("unchanged candidate replay rewrote facts, receipts, relationships, or executable proposals")
	}

	// A branch-scoped result change to NO_FACTS removes only the feature-owned
	// fact. Main's two facts and their relationship remain untouched.
	mode = 1
	feature := base
	feature.branch = "feature"
	feature.model = "operational-v2"
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, feature, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("feature-scoped NO_FACTS apply: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateEmptyResults != 1 {
		t.Fatalf("feature-scoped summary = %+v, want one empty result", source)
	}
	if got := phase3OperationalFactsForBranch(t, incremental, "feature"); got != "[]" {
		t.Fatalf("feature facts after NO_FACTS = %s, want empty", got)
	}
	if got := phase3OperationalRelationships(t, incremental); got != initialRelationships {
		t.Fatalf("feature-scoped cleanup changed main relationship state:\nwant=%s\ngot=%s", initialRelationships, got)
	}
	mainFactsAfterFeature := phase3OperationalFactsForBranch(t, incremental, "main")
	if mainFactsAfterFeature == "[]" || !strings.Contains(phase3OperationalRelationships(t, incremental), "repositorykey") {
		t.Fatalf("feature-scoped cleanup crossed into main: facts=%s relationships=%s", mainFactsAfterFeature, phase3OperationalRelationships(t, incremental))
	}
	phase3OperationalAssertNoExecutableMutation(t, incremental)

	// Removing the main s2 candidate entirely and selecting only that session
	// removes B and its relationship, while s1 remains. This is the removal
	// half of the NO_FACTS/removal lifecycle and proves session scoping.
	phase3OperationalRewriteSession(t, incremental, "sessions/main/s2.jsonl", `{"type":"event_msg","payload":{"type":"user_message","message":"Thanks."}}`)
	sessionOnly := base
	sessionOnly.session = "s2"
	sessionOnly.model = "operational-v2"
	callsBeforeRemoval := calls
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, sessionOnly, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("session-scoped removal: %v", err)
	} else if source.ExtractionCalls != 0 || calls != callsBeforeRemoval {
		t.Fatalf("session removal made provider calls: %+v/calls=%d", source, calls)
	}
	mainFactsAfterRemoval := phase3OperationalFactsForBranch(t, incremental, "main")
	if mainFactsAfterRemoval == "[]" || strings.Contains(mainFactsAfterRemoval, "Never bypass") || phase3OperationalRelationships(t, incremental) != "{\"entries\":{}}" {
		t.Fatalf("session-scoped removal did not remove only s2 state: facts=%s relationships=%s", mainFactsAfterRemoval, phase3OperationalRelationships(t, incremental))
	}
	phase3OperationalAssertNoExecutableMutation(t, incremental)

	// Final force parity at the post-removal source state: a clean rebuild with
	// B absent and feature NO_FACTS must match the incrementally evolved store.
	finalClean := phase3OperationalFixture(t, now)
	phase3OperationalRewriteSession(t, finalClean, "sessions/main/s2.jsonl", `{"type":"event_msg","payload":{"type":"user_message","message":"Thanks."}}`)
	mode = 2
	finalForce := base
	finalForce.force = true
	finalForce.model = "operational-v3"
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), finalClean, finalForce, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("final clean force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateEmptyResults != 1 {
		t.Fatalf("final clean force summary = %+v, want one packed call and one empty result", source)
	}
	// The incremental store still has the post-removal source, but its cached
	// feature result is already empty. A force replay with mode 2 makes the
	// comparison explicit and catches stale relationship/receipt ownership.
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), incremental, finalForce, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("final incremental force rebuild: %v", err)
	} else if source.ExtractionCalls != 1 || source.CandidateEmptyResults != 1 {
		t.Fatalf("final incremental force summary = %+v, want one packed call and one empty result", source)
	}
	if got, want := phase3OperationalFacts(t, incremental), phase3OperationalFacts(t, finalClean); got != want {
		t.Fatalf("final force facts differ:\nwant=%s\ngot=%s", want, got)
	}
	if got, want := phase3OperationalRelationships(t, incremental), phase3OperationalRelationships(t, finalClean); got != want {
		t.Fatalf("final force relationships differ:\nwant=%s\ngot=%s", want, got)
	}
	phase3OperationalAssertNoExecutableMutation(t, incremental)
	t.Logf("Phase 3 operational gate: provider calls=%d; initial force parity=3 facts/1 relationship; no-op=0 calls and byte-stable; scoped NO_FACTS/removal and final force parity passed", calls)
}

func phase3OperationalOptions(provider distillAgentRunner, model string) distillCommandOptions {
	return distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: provider,
		pipeline: distillPipelineCandidates, model: model, effort: "low",
		maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, concurrency: 1,
	}
}

func phase3OperationalProvider(t *testing.T, calls *int, mode *int) distillAgentRunner {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		*calls++
		cards := phase3OperationalCardTexts(t, input)
		lines := make([]string, 0, len(cards))
		for id, text := range cards {
			switch {
			case strings.Contains(text, "Never bypass") && *mode >= 1:
				lines = append(lines, id+"\tNO_FACTS")
			case strings.Contains(text, "FeatureGate") && *mode >= 1:
				lines = append(lines, id+"\tNO_FACTS")
			case strings.Contains(text, "Always validate"):
				lines = append(lines, id+"\tdecision\tarchitecture.data.flow\tAlways validate `RepositoryKey` at input boundaries.")
			case strings.Contains(text, "Never bypass"):
				lines = append(lines, id+"\tdecision\tarchitecture.data.flow\tNever bypass `RepositoryKey` validation.")
			case strings.Contains(text, "FeatureGate"):
				lines = append(lines, id+"\tdecision\tarchitecture.data.flow\tAlways isolate `FeatureGate` changes.")
			default:
				return "", &phase3OperationalUnexpectedCard{text: text}
			}
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n"), nil
	}
}

type phase3OperationalUnexpectedCard struct{ text string }

func (e *phase3OperationalUnexpectedCard) Error() string {
	return "unexpected operational-gate card: " + e.text
}

func phase3OperationalCardTexts(t *testing.T, input []byte) map[string]string {
	t.Helper()
	texts := make(map[string]string)
	current := ""
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
			t.Fatalf("decode packed candidate: %v", err)
		}
		switch record.Type {
		case "distill_candidate_v1":
			current = record.CandidateID
			texts[current] = ""
		case "distill_candidate_turn_v1":
			if current == "" {
				t.Fatal("candidate turn appeared before header")
			}
			texts[current] += record.Text
		}
	}
	if len(texts) == 0 {
		t.Fatal("packed candidate input contains no cards")
	}
	return texts
}

func phase3OperationalFixture(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := t.TempDir()
	sessions := []exportSession{
		{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp-main-1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-3 * time.Hour)},
		{SessionID: "s2", Branch: "main", LatestCheckpoint: "cp-main-2", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
		{SessionID: "f1", Branch: "feature", LatestCheckpoint: "cp-feature-1", TranscriptPath: "sessions/feature/f1.jsonl", CreatedAt: now.Add(-time.Hour)},
	}
	contents := map[string]string{
		"sessions/main/s1.jsonl":    `{"type":"event_msg","payload":{"type":"user_message","message":"Always validate ` + "`RepositoryKey`" + ` at input boundaries."}}`,
		"sessions/main/s2.jsonl":    `{"type":"event_msg","payload":{"type":"user_message","message":"Never bypass ` + "`RepositoryKey`" + ` validation."}}`,
		"sessions/feature/f1.jsonl": `{"type":"event_msg","payload":{"type":"user_message","message":"Always isolate ` + "`FeatureGate`" + ` changes."}}`,
	}
	for rel, content := range contents {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, DefaultBranch: "main", Sources: &brainSources{
		Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions},
	}}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}
	// Preserve an executable legacy proposal as a negative control: candidate
	// mode must not create, settle, rewrite, or supersede it.
	if err := writeFactProposals(brainDir, "main", []factProposal{{Action: factActionSupersede, CandidateID: "fact:legacy-new", TargetID: "fact:legacy-old", Confidence: 0.71, Branch: "main"}}); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func phase3OperationalRewriteSession(t *testing.T, brainDir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func phase3OperationalBytes(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read operational byte snapshot %s: %v", path, err)
		}
		out[path] = data
	}
	return out
}

func phase3OperationalEqualBytes(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for path, want := range left {
		if !bytes.Equal(want, right[path]) {
			return false
		}
	}
	return true
}

func phase3OperationalAssertNoExecutableMutation(t *testing.T, brainDir string) {
	t.Helper()
	proposals, err := loadFactProposals(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].Action != factActionSupersede || proposals[0].CandidateID != "fact:legacy-new" || proposals[0].TargetID != "fact:legacy-old" {
		t.Fatalf("candidate write mutated executable proposal/supersession state: %+v", proposals)
	}
	for _, branch := range []string{"main", "feature"} {
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range facts {
			if fact.Status != factStatusActive {
				t.Fatalf("candidate write superseded fact %s on %s: %+v", fact.ID, branch, fact)
			}
		}
	}
}

func phase3OperationalFacts(t *testing.T, brainDir string) string {
	t.Helper()
	all := make(map[string][]factRecord)
	for _, branch := range []string{"main", "feature"} {
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			t.Fatal(err)
		}
		for i := range facts {
			facts[i].CreatedAt = time.Time{}
			facts[i].UpdatedAt = time.Time{}
			for j := range facts[i].Provenance {
				facts[i].Provenance[j].Line = 0
				facts[i].Provenance[j].EndLine = 0
			}
		}
		sort.Slice(facts, func(i, j int) bool { return facts[i].ID < facts[j].ID })
		all[branch] = facts
	}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func phase3OperationalFactsForBranch(t *testing.T, brainDir, branch string) string {
	t.Helper()
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if facts == nil {
		facts = []factRecord{}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].ID < facts[j].ID })
	data, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func phase3OperationalRelationships(t *testing.T, brainDir string) string {
	t.Helper()
	store, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(store.entries))
	for id := range store.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make(map[string]any, len(ids))
	for _, id := range ids {
		entries[id] = store.entries[id]
	}
	data, err := json.Marshal(struct {
		Entries map[string]any `json:"entries"`
	}{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func phase3OperationalReceipts(t *testing.T, brainDir string) string {
	t.Helper()
	store, err := loadDistillApplicationReceiptStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for id, receipt := range store.entries {
		receipt.ReceiptID = ""
		receipt.Identity.FactStoreGeneration = ""
		store.entries[id] = receipt
	}
	data, err := json.Marshal(store.entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
