package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func factsReportFixture(t *testing.T) (verifyFixture, []factRecord) {
	t.Helper()
	f := newVerifyFixture(t)
	now := f.now
	fact := func(text, path, locus, kind string) factRecord {
		return factRecord{
			ID: factRecordID(text, []string{path}), Paths: []string{path}, Locus: []string{locus},
			Text: text, Branch: "main", Kind: kind, Origin: factOriginAuthored, Status: factStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	facts := []factRecord{
		fact("facts commands preserve durable identities", "constraints.invariants.general", "internal/cli/facts.go", factKindInvariant),
		fact("fact reports keep counts deterministic", "constraints.invariants.general", "internal/cli/facts.go", factKindDecision),
		fact("operator preferences use explicit taxonomy paths", "preferences.coding.style", "internal/cli/report.go", factKindPreference),
		fact("workflow warnings name their source", "workflow.testing.rules", "internal/agentsetup/report.go", factKindGotcha),
	}
	f.writeFacts(t, "main", facts)
	return f, facts
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFactsReadReportsOutlineMapTreeExactAndReadOnlyFacts(t *testing.T) {
	f, facts := factsReportFixture(t)
	factsPath := filepath.Join(f.brainDir, filepath.FromSlash(factsBranchRelDir("main")), factsFileName)
	beforeFacts := fileDigest(t, factsPath)

	outlineOut, err := execute(t, NewRootCommand(f.opts), "facts", "outline", "--agent", "none", "--min-facts", "2", "--json")
	if err != nil {
		t.Fatalf("outline: %v\n%s", err, outlineOut)
	}
	var outline struct {
		Branch      string `json:"branch"`
		Nodes       int    `json:"nodes"`
		Regenerated int    `json:"regenerated"`
	}
	if err := json.Unmarshal([]byte(outlineOut), &outline); err != nil {
		t.Fatalf("decode outline report: %v\n%s", err, outlineOut)
	}
	if outline.Branch != "main" || outline.Nodes != 3 || outline.Regenerated != 0 {
		t.Fatalf("outline report = %+v, want main/3/0", outline)
	}

	mapOut, err := execute(t, NewRootCommand(f.opts), "facts", "map", "--path", "internal/cli", "--depth", "2", "--json")
	if err != nil {
		t.Fatalf("map: %v\n%s", err, mapOut)
	}
	var mapped factOutline
	if err := json.Unmarshal([]byte(mapOut), &mapped); err != nil {
		t.Fatalf("decode map: %v\n%s", err, mapOut)
	}
	cliNode, ok := mapped.Nodes["internal/cli"]
	wantCLIIDs := []string{facts[0].ID, facts[1].ID}
	slices.Sort(wantCLIIDs)
	gotCLIIDs := slices.Clone(cliNode.LeafFactIDs)
	slices.Sort(gotCLIIDs)
	if !ok || cliNode.Label != "cli" || !slices.Equal(gotCLIIDs, wantCLIIDs) {
		t.Fatalf("map internal/cli node = %+v, want IDs %v", cliNode, wantCLIIDs)
	}
	wantRootIDs := []string{facts[2].ID, facts[3].ID}
	slices.Sort(wantRootIDs)
	gotRootIDs := slices.Clone(mapped.Nodes[""].LeafFactIDs)
	slices.Sort(gotRootIDs)
	if !slices.Equal(gotRootIDs, wantRootIDs) {
		t.Fatalf("map root fact IDs = %v, want %v", gotRootIDs, wantRootIDs)
	}

	treeOut, err := execute(t, NewRootCommand(f.opts), "facts", "tree", "--path", "constraints", "--kind", "invariant", "--depth", "3", "--leaves", "1")
	if err != nil {
		t.Fatalf("tree: %v\n%s", err, treeOut)
	}
	for _, want := range []string{"1 facts on main under constraints", "constraints (1)", "constraints.invariants.general (1)", "facts commands preserve durable identities"} {
		if !strings.Contains(treeOut, want) {
			t.Fatalf("tree missing %q:\n%s", want, treeOut)
		}
	}
	if strings.Contains(treeOut, "operator preferences") || strings.Contains(treeOut, "workflow warnings") {
		t.Fatalf("tree filter leaked unrelated facts:\n%s", treeOut)
	}
	if after := fileDigest(t, factsPath); after != beforeFacts {
		t.Fatalf("read reports changed facts store: before=%s after=%s", beforeFacts, after)
	}
}

func TestFactsRetractRootCommandChangesOnlyExplicitFact(t *testing.T) {
	f, facts := factsReportFixture(t)
	target := facts[0]
	other := facts[1]
	out, err := execute(t, NewRootCommand(f.opts), "facts", "retract", target.ID, "--json")
	if err != nil {
		t.Fatalf("retract: %v\n%s", err, out)
	}
	var result struct {
		ID      string `json:"id"`
		Branch  string `json:"branch"`
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode retract: %v\n%s", err, out)
	}
	if result.ID != target.ID || result.Branch != "main" || result.Status != factStatusRetracted || !result.Changed {
		t.Fatalf("retract result = %+v", result)
	}
	stored, err := loadFacts(f.brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(facts) {
		t.Fatalf("retract changed record count: got %d want %d", len(stored), len(facts))
	}
	byID := make(map[string]factRecord, len(stored))
	for _, fact := range stored {
		byID[fact.ID] = fact
	}
	if len(byID) != len(facts) {
		t.Fatalf("retract changed record identities: %+v", byID)
	}
	for _, fact := range facts {
		got, ok := byID[fact.ID]
		if !ok {
			t.Fatalf("retract removed record %s", fact.ID)
		}
		want := fact
		if fact.ID == target.ID {
			want.Status = factStatusRetracted
		}
		if fact.ID != target.ID && !reflect.DeepEqual(got, want) {
			t.Fatalf("retract changed unselected fact %s: got=%+v want=%+v", fact.ID, got, want)
		}
	}
	for _, fact := range stored {
		switch fact.ID {
		case target.ID:
			if fact.Status != factStatusRetracted {
				t.Fatalf("target status = %q", fact.Status)
			}
		case other.ID:
			if fact.Status != factStatusActive {
				t.Fatalf("unselected fact status = %q", fact.Status)
			}
		}
	}
}
