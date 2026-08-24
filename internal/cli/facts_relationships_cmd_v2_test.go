package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/spf13/cobra"
)

func TestFactsRelationshipsListV2TextAndJSON(t *testing.T) {
	f, relation := newFactsRelationshipsFixtureV2(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
	if err != nil {
		t.Fatalf("facts relationships list: %v\n%s", err, out)
	}
	for _, want := range []string{
		"1 local relationship observation(s) on main", relation.ID,
		relation.FactIDs[0], relation.FactIDs[1], "kind=invariant",
		"top_level=architecture", "strong_locus=internal/cli/graph.go",
		relation.Owners[0].CandidateID + "/" + relation.Owners[0].SourceSessionID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"first private fact text", "second private fact text"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("text output leaked fact text %q:\n%s", forbidden, out)
		}
	}

	out, err = execute(t, NewRootCommand(f.opts), "facts", "relationships", "list", "--json")
	if err != nil {
		t.Fatalf("facts relationships list --json: %v\n%s", err, out)
	}
	var report factsRelationshipsListReportV2
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if report.Branch != "main" || len(report.Relationships) != 1 {
		t.Fatalf("JSON report = %+v", report)
	}
	got := report.Relationships[0]
	if got.ID != relation.ID || got.Kind != relation.Kind || !reflect.DeepEqual(got.FactIDs, relation.FactIDs) || got.Subject != relation.Subject || !reflect.DeepEqual(got.Owners, relation.Owners) {
		t.Fatalf("JSON relationship = %+v, want %+v", got, relation)
	}
	if strings.Contains(out, "first private fact text") || strings.Contains(out, "second private fact text") {
		t.Fatalf("JSON output leaked fact text:\n%s", out)
	}
}

func TestFactsRelationshipsListV2BranchFilter(t *testing.T) {
	f, mainRelation := newFactsRelationshipsFixtureV2(t)
	featureRelation := writeFactsRelationshipV2(t, f, "feature/relationships", "feature candidate session", "feature first private fact text", "feature second private fact text")
	store, err := loadDistillRelationshipStoreV2(f.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 2 || store.entries[mainRelation.ID].Branch != "main" || store.entries[featureRelation.ID].Branch != "feature/relationships" {
		t.Fatalf("fixture store = %+v", store.entries)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list", "--branch", "feature/relationships")
	if err != nil {
		t.Fatalf("feature relationships list: %v\n%s", err, out)
	}
	if !strings.Contains(out, featureRelation.ID) || strings.Contains(out, mainRelation.ID) {
		t.Fatalf("branch-filtered output = %q", out)
	}
}

func TestFactsRelationshipsListV2ReportsSkippedDenseBlocks(t *testing.T) {
	f := newVerifyFixture(t)
	if err := writeFacts(f.brainDir, "main", relationshipBlockFactsV2("DenseSubject", 92)); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list", "--json")
	if err != nil {
		t.Fatalf("dense relationships list --json: %v\n%s", err, out)
	}
	var report factsRelationshipsListReportV2
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse dense JSON: %v\n%s", err, out)
	}
	if report.Branch != "main" || len(report.Relationships) != 0 || report.SkippedBlocks != 1 {
		t.Fatalf("dense relationship report = %+v", report)
	}

	out, err = execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
	if err != nil {
		t.Fatalf("dense relationships list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no local relationship observations on main") || !strings.Contains(out, "1 bounded subject block(s) skipped") {
		t.Fatalf("dense relationship text report = %q", out)
	}
}

func TestFactsRelationshipsListV2FailsClosedWhenTombstoneLandsBeforeEmission(t *testing.T) {
	f, relation := newFactsRelationshipsFixtureV2(t)
	original := beforeRetrievalResponsePrivacyEmissionCheck
	beforeRetrievalResponsePrivacyEmissionCheck = func() {
		stones, _, err := loadSessionTombstonesChecked(f.brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if stones.Excluded == nil {
			stones.Excluded = make(map[string]sessionTombstone)
		}
		stones.Excluded[relation.Owners[0].SourceSessionID] = sessionTombstone{At: time.Date(2026, time.August, 23, 21, 0, 0, 0, time.UTC)}
		if err := saveSessionTombstones(f.brainDir, stones); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = original })

	out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("relationship list privacy race error = %v\n%s", err, out)
	}
	if strings.Contains(out, relation.ID) || strings.Contains(out, relation.FactIDs[0]) || strings.Contains(out, relation.Owners[0].SourceSessionID) {
		t.Fatalf("relationship list emitted tombstoned evidence:\n%s", out)
	}
}

func TestFactsRelationshipsV2HasOnlyListAndReadOnlyFlags(t *testing.T) {
	cmd := newFactsRelationshipsCommand(Options{})
	commands := cmd.Commands()
	if len(commands) != 1 || commands[0].Name() != "list" {
		t.Fatalf("relationships subcommands = %v; want only list", commandNamesV2(commands))
	}
	for _, name := range []string{"apply", "reject", "sync", "publish", "review", "force", "delete"} {
		if cmd.Flags().Lookup(name) != nil || commands[0].Flags().Lookup(name) != nil {
			t.Fatalf("relationships command exposed mutation flag %q", name)
		}
	}
	for _, name := range []string{"branch", "json"} {
		if commands[0].Flags().Lookup(name) == nil {
			t.Fatalf("list missing read-only flag %q", name)
		}
	}
}

func TestFactsRelationshipsListV2RejectsStaleLiveEvidence(t *testing.T) {
	t.Run("missing fact", func(t *testing.T) {
		f, relation := newFactsRelationshipsFixtureV2(t)
		facts, err := loadFacts(f.brainDir, "main")
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFacts(f.brainDir, "main", facts[:1]); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
		if err == nil || !strings.Contains(err.Error(), relation.ID) || !strings.Contains(err.Error(), "referenced facts") {
			t.Fatalf("stale fact list error = %v\n%s", err, out)
		}
	})

	t.Run("changed subject", func(t *testing.T) {
		f, relation := newFactsRelationshipsFixtureV2(t)
		facts, err := loadFacts(f.brainDir, "main")
		if err != nil {
			t.Fatal(err)
		}
		facts[1].Locus = []string{"internal/cli/other.go"}
		if err := writeFacts(f.brainDir, "main", facts); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
		if err == nil || !strings.Contains(err.Error(), relation.ID) || !strings.Contains(err.Error(), "subject block") {
			t.Fatalf("stale subject list error = %v\n%s", err, out)
		}
	})

	t.Run("missing owner anchor", func(t *testing.T) {
		f, relation := newFactsRelationshipsFixtureV2(t)
		facts, err := loadFacts(f.brainDir, "main")
		if err != nil {
			t.Fatal(err)
		}
		facts[0].Provenance = nil
		facts[1].Provenance = nil
		if err := writeFacts(f.brainDir, "main", facts); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
		if err == nil || !strings.Contains(err.Error(), relation.ID) || !strings.Contains(err.Error(), "no live v2 application anchor") {
			t.Fatalf("stale owner list error = %v\n%s", err, out)
		}
	})
}

func TestFactsRelationshipsListV2DoesNotUseExecutableProposals(t *testing.T) {
	f, relation := newFactsRelationshipsFixtureV2(t)
	if err := writeFactProposals(f.brainDir, "main", []factProposal{{
		Action: factActionSupersede, CandidateID: relation.FactIDs[0], TargetID: relation.FactIDs[1], Confidence: 0.5, Branch: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	factsPath := filepath.Join(f.brainDir, filepath.FromSlash(factsFileRelPath("main")))
	storePath := filepath.Join(f.brainDir, filepath.FromSlash(distillRelationshipStoreV2Path))
	proposalsPath := filepath.Join(f.brainDir, filepath.FromSlash(factsProposalsRelPath("main")))
	beforeFacts := readRelationshipFileV2(t, factsPath)
	beforeStore := readRelationshipFileV2(t, storePath)
	beforeProposals := readRelationshipFileV2(t, proposalsPath)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "relationships", "list")
	if err != nil {
		t.Fatalf("relationships list with executable proposal: %v\n%s", err, out)
	}
	if !strings.Contains(out, relation.ID) {
		t.Fatalf("relationship not listed:\n%s", out)
	}
	if got := readRelationshipFileV2(t, factsPath); !reflect.DeepEqual(got, beforeFacts) {
		t.Fatalf("list mutated facts")
	}
	if got := readRelationshipFileV2(t, storePath); !reflect.DeepEqual(got, beforeStore) {
		t.Fatalf("list mutated relationship store")
	}
	if got := readRelationshipFileV2(t, proposalsPath); !reflect.DeepEqual(got, beforeProposals) {
		t.Fatalf("list mutated executable proposal queue")
	}
}

func newFactsRelationshipsFixtureV2(t *testing.T) (verifyFixture, factmerge.RelationshipProposal) {
	t.Helper()
	f := newVerifyFixture(t)
	relation := writeFactsRelationshipV2(t, f, "main", "candidate session", "first private fact text", "second private fact text")
	return f, relation
}

func writeFactsRelationshipV2(t *testing.T, f verifyFixture, branch, sessionID, leftText, rightText string) factmerge.RelationshipProposal {
	t.Helper()
	candidateID := "candidate-v1:" + strings.Repeat("a", 64)
	owner := factmerge.RelationshipOwner{CandidateID: candidateID, SourceSessionID: sessionID}
	anchor := factAnchor{SessionID: sessionID, DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, true), Transcript: "sessions/local.jsonl", Line: 1}
	now := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	paths := []string{"architecture.data.flow"}
	facts := []factRecord{
		{ID: factRecordID(leftText, paths), Paths: paths, Kind: factKindInvariant, Locus: []string{"internal/cli/graph.go"}, Text: leftText, Branch: branch, Origin: factOriginDistilled, Status: factStatusActive, Provenance: []factAnchor{anchor}, CreatedAt: now, UpdatedAt: now},
		{ID: factRecordID(rightText, paths), Paths: paths, Kind: factKindInvariant, Locus: []string{"INTERNAL/CLI/GRAPH.GO"}, Text: rightText, Branch: branch, Origin: factOriginDistilled, Status: factStatusActive, Provenance: []factAnchor{anchor}, CreatedAt: now, UpdatedAt: now},
	}
	if err := writeFacts(f.brainDir, branch, facts); err != nil {
		t.Fatal(err)
	}
	relation, ok, err := factmerge.BuildPossibleSameSubject(facts[0], facts[1], []factmerge.RelationshipOwner{owner})
	if err != nil || !ok {
		t.Fatalf("build fixture relationship: relation=%+v ok=%v err=%v", relation, ok, err)
	}
	store, err := loadDistillRelationshipStoreV2(f.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(relation); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillRelationshipStoreV2(f.brainDir, store); err != nil {
		t.Fatal(err)
	}
	return relation
}

func commandNamesV2(commands []*cobra.Command) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name())
	}
	return names
}

func readRelationshipFileV2(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
