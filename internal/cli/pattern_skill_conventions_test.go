package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedCapabilityFactsFile writes durable facts to brainDir/facts/main/facts.ndjson.
func seedCapabilityFactsFile(t *testing.T, brainDir string, facts []factRecord) {
	t.Helper()
	dir := filepath.Join(brainDir, factsDirName, "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, factsFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range facts {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}

const conventionProposalJSON = `{"conventions":[{
  "title":"Regenerate radar-tool evidence when pinned sources change",
  "trigger":"editing mcp.go, retrieve.go, or workspace.go in radar-tool",
  "not_when":"editing unrelated, non-pinned files",
  "knowledge":["radar-tool source files are hash-pinned in the release manifest","the audit diffs committed report files too, so regenerate manifest AND reports"],
  "member_fact_ids":["fact:pinned","fact:audit"],
  "verdict":"accepted"
}]}`

// A capability skill is sourced from durable gotcha/convention/invariant facts,
// stored as an accepted `convention:` deep dossier (archetype "capability"), and
// surfaces as a formable proposal carrying the real fact texts.
func TestProposeSkillConventionsRoundTrip(t *testing.T) {
	now := time.Now()
	brainDir := t.TempDir()
	newCorpusAtDir(t, brainDir)
	seedCapabilityFactsFile(t, brainDir, []factRecord{
		{ID: "fact:pinned", Kind: "convention", Branch: "main", Status: "active",
			Text: "radar-tool source files are hash-pinned in the release evidence manifest", Locus: []string{"mcp.go"}},
		{ID: "fact:audit", Kind: "gotcha", Branch: "main", Status: "active",
			Text: "the release-evidence audit also diffs the committed report files", Locus: []string{"reports/"}},
		{ID: "fact:obvious", Kind: "preference", Branch: "main", Status: "active",
			Text: "the user likes concise commits"}, // not a capability kind → filtered out
	})

	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := proposeSkillConventions(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", stubRunner(conventionProposalJSON), now)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 {
		t.Fatalf("expected 1 accepted convention, got %+v", stats)
	}

	props := loadKnowledgeSkillProposals(brainDir)
	var conv *taskCandidate
	for i := range props {
		if strings.HasPrefix(props[i].ID, "convention:") {
			conv = &props[i]
		}
	}
	if conv == nil {
		t.Fatalf("expected a convention proposal, got %+v", props)
	}
	in, ok := loadAcceptedDeepDossier(brainDir, conv.ID)
	if !ok {
		t.Fatal("accepted convention dossier should load via loadAcceptedDeepDossier")
	}
	if in.rec.Archetype != "capability" {
		t.Errorf("want capability archetype, got %q", in.rec.Archetype)
	}
	if in.rec.NotWhen == "" || len(in.rec.Knowledge) == 0 || len(in.rec.Facts) == 0 {
		t.Errorf("capability dossier missing not_when/knowledge/facts: %+v", in.rec)
	}

	ev := buildDeepSkillEvidence(in)
	for _, want := range []string{
		"Archetype: capability",
		"Do NOT use when:",
		"Non-obvious knowledge (the spine",
		"hash-pinned",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("capability skill evidence missing %q:\n%s", want, ev)
		}
	}
}

// Only non-obvious fact kinds (gotcha/convention/invariant) are sampled;
// preferences/decisions are excluded from the capability channel.
func TestSampleCapabilityFactsFiltersKind(t *testing.T) {
	brainDir := t.TempDir()
	newCorpusAtDir(t, brainDir)
	seedCapabilityFactsFile(t, brainDir, []factRecord{
		{ID: "fact:inv", Kind: "invariant", Branch: "main", Status: "active", Text: "x must hold"},
		{ID: "fact:pref", Kind: "preference", Branch: "main", Status: "active", Text: "user likes y"},
		{ID: "fact:retired", Kind: "convention", Branch: "main", Status: "retracted", Text: "old rule"},
	})
	db, _ := openPatternCorpusDB(brainDir)
	defer db.Close()
	sample, _, ids := sampleCapabilityFacts(db, brainDir)
	if len(sample) != 1 || len(ids) != 1 || ids[0] != "fact:inv" {
		t.Errorf("expected only the active invariant fact, got ids %v", ids)
	}
}
