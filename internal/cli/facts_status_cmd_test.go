package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFactsStatusEmptyStoreJSON(t *testing.T) {
	f := newVerifyFixture(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "status", "--json")
	if err != nil {
		t.Fatalf("facts status: %v\n%s", err, out)
	}
	var report factsStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("schema = %d", report.SchemaVersion)
	}
	if report.Branch != "main" || report.AllBranches {
		t.Fatalf("unexpected target: branch=%q all=%v", report.Branch, report.AllBranches)
	}
	if report.FactsArmReady {
		t.Fatalf("empty store should not be facts-arm ready: %+v", report)
	}
	if report.Totals.Facts != 0 || report.Totals.Active != 0 || report.Totals.Proposals != 0 {
		t.Fatalf("empty totals = %+v", report.Totals)
	}
	if report.ManifestFacts != nil {
		t.Fatalf("empty store should not invent a manifest facts source: %+v", report.ManifestFacts)
	}
	warnings := strings.Join(report.Warnings, "\n")
	for _, want := range []string{"no active facts", "no facts source"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings missing %q: %+v", want, report.Warnings)
		}
	}
}

func TestFactsStatusCountsBranchJSON(t *testing.T) {
	f := newVerifyFixture(t)
	now := f.now
	records := []factRecord{
		factsStatusTestFact("Authored verified fact.", "feature", factOriginAuthored, factStatusActive, now, []factAnchor{{SessionID: "s1", Verified: true}}),
		factsStatusTestFact("Distilled unsigned fact.", "feature", factOriginDistilled, factStatusActive, now, []factAnchor{{SessionID: "s2"}}),
		factsStatusTestFact("Superseded mixed anchors.", "feature", factOriginDistilled, factStatusSuperseded, now, []factAnchor{{SessionID: "s3", Verified: true}, {SessionID: "s4"}}),
		factsStatusTestFact("Retracted authored fact.", "feature", factOriginAuthored, factStatusRetracted, now, nil),
	}
	if err := writeFacts(f.brainDir, "feature", records); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := writeFactProposals(f.brainDir, "feature", []factProposal{
		{Action: factActionMerge, CandidateID: records[1].ID, TargetID: records[0].ID, Confidence: 0.61, Branch: "feature"},
		{Action: factActionSupersede, CandidateID: records[2].ID, TargetID: records[0].ID, Confidence: 0.62, Branch: "feature"},
	}); err != nil {
		t.Fatalf("write proposals: %v", err)
	}
	if err := updateFactSourceManifest(f.brainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "status", "--branch", "feature", "--json")
	if err != nil {
		t.Fatalf("facts status: %v\n%s", err, out)
	}
	var report factsStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if report.Branch != "feature" || !report.FactsArmReady {
		t.Fatalf("unexpected status target/readiness: %+v", report)
	}
	want := factsStatusCounts{
		Facts:             4,
		Active:            2,
		Superseded:        1,
		Retracted:         1,
		Distilled:         2,
		Authored:          2,
		Proposals:         2,
		ProvenanceAnchors: 4,
		VerifiedAnchors:   2,
		UnsignedAnchors:   2,
	}
	if report.Totals != want {
		t.Fatalf("totals = %+v, want %+v", report.Totals, want)
	}
	if report.ManifestFacts == nil || report.ManifestFacts.Facts != 4 || report.ManifestFacts.Proposals != 2 {
		t.Fatalf("manifest facts source missing counts: %+v", report.ManifestFacts)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", report.Warnings)
	}
}

func TestFactsStatusAllBranchesJSON(t *testing.T) {
	f := newVerifyFixture(t)
	now := f.now
	mainFact := factsStatusTestFact("Main active fact.", "main", factOriginDistilled, factStatusActive, now, []factAnchor{{SessionID: "m1"}})
	featureFact := factsStatusTestFact("Feature superseded fact.", "feature", factOriginAuthored, factStatusSuperseded, now, []factAnchor{{SessionID: "f1", Verified: true}})
	if err := writeFacts(f.brainDir, "main", []factRecord{mainFact}); err != nil {
		t.Fatalf("write main facts: %v", err)
	}
	if err := writeFacts(f.brainDir, "feature", []factRecord{featureFact}); err != nil {
		t.Fatalf("write feature facts: %v", err)
	}
	if err := updateFactSourceManifest(f.brainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "status", "--all-branches", "--json")
	if err != nil {
		t.Fatalf("facts status: %v\n%s", err, out)
	}
	var report factsStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if !report.AllBranches || report.Branch != "" {
		t.Fatalf("unexpected all-branches target: %+v", report)
	}
	if len(report.Branches) != 2 || report.Branches[0].Branch != "feature" || report.Branches[1].Branch != "main" {
		t.Fatalf("branches not sorted/stable: %+v", report.Branches)
	}
	if report.Totals.Facts != 2 || report.Totals.Active != 1 || report.Totals.Superseded != 1 {
		t.Fatalf("totals = %+v", report.Totals)
	}
	if !report.FactsArmReady || report.Branches[0].FactsArmReady || !report.Branches[1].FactsArmReady {
		t.Fatalf("readiness = report:%v branches:%+v", report.FactsArmReady, report.Branches)
	}
}

func factsStatusTestFact(text, branch, origin, status string, now time.Time, anchors []factAnchor) factRecord {
	paths := normalizeFactPaths([]string{"architecture.boundaries.rationale"})
	return factRecord{
		ID:         factRecordID(text, paths),
		Paths:      paths,
		Text:       text,
		Branch:     branch,
		Origin:     origin,
		Status:     status,
		Provenance: append([]factAnchor(nil), anchors...),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}
