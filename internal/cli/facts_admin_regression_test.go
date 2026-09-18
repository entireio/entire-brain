package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFactsReviewRootCommandListsAndSettlesExactProposal(t *testing.T) {
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	target := factRecord{ID: factRecordID("deploy uses canary rollout", paths), Paths: paths, Text: "deploy uses canary rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now}
	candidate := factRecord{ID: factRecordID("deploy uses blue green rollout", paths), Paths: paths, Text: "deploy uses blue green rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now}
	f.writeFacts(t, "main", []factRecord{target, candidate})
	proposal := factProposal{Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID, Confidence: 0.63, Branch: "main"}
	unrelated := factProposal{Action: factActionMerge, CandidateID: "fact:unrelated", TargetID: target.ID, Confidence: 0.52, Branch: "main"}
	if err := writeFactProposals(f.brainDir, "main", []factProposal{proposal, unrelated}); err != nil {
		t.Fatal(err)
	}

	listed, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--json")
	if err != nil {
		t.Fatalf("review list: %v\n%s", err, listed)
	}
	var payload struct {
		Branch    string         `json:"branch"`
		Proposals []factProposal `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(listed), &payload); err != nil {
		t.Fatalf("decode review list: %v\n%s", err, listed)
	}
	if payload.Branch != "main" || len(payload.Proposals) != 2 {
		t.Fatalf("review list lost proposal identity: %+v", payload)
	}
	var listedCandidate bool
	for _, listedProposal := range payload.Proposals {
		if listedProposal.CandidateID == candidate.ID && listedProposal.TargetID == target.ID {
			listedCandidate = true
		}
	}
	if !listedCandidate {
		t.Fatalf("review list lost target proposal identity: %+v", payload.Proposals)
	}

	resolved, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", candidate.ID)
	if err != nil || !strings.Contains(resolved, "resolved 1 proposal(s) on main; 1 remaining") {
		t.Fatalf("apply review: err=%v output=%q", err, resolved)
	}
	remaining, err := loadFactProposals(f.brainDir, "main")
	if err != nil || len(remaining) != 1 || remaining[0].CandidateID != unrelated.CandidateID {
		t.Fatalf("proposal queue after apply: %+v err=%v", remaining, err)
	}
	facts, err := loadFacts(f.brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var gotCandidate, gotTarget *factRecord
	for i := range facts {
		if facts[i].ID == candidate.ID {
			gotCandidate = &facts[i]
		}
		if facts[i].ID == target.ID {
			gotTarget = &facts[i]
		}
	}
	if gotCandidate == nil || gotCandidate.Status != factStatusActive || gotTarget == nil || gotTarget.Status != factStatusSuperseded {
		t.Fatalf("apply changed wrong facts: candidate=%+v target=%+v all=%+v", gotCandidate, gotTarget, facts)
	}
	beforeUnknown, err := loadFacts(f.brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", "missing-candidate"); err == nil {
		t.Fatal("review accepted an unknown proposal")
	}
	if after, err := loadFacts(f.brainDir, "main"); err != nil || !reflect.DeepEqual(after, beforeUnknown) {
		t.Fatalf("unknown review mutated facts: %v before=%+v after=%+v", err, beforeUnknown, after)
	}
}

func TestFactsReviewRootCommandRejectsExactProposalWithoutFactMutation(t *testing.T) {
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	fact := factRecord{ID: factRecordID("deploy uses rolling restart", paths), Paths: paths, Text: "deploy uses rolling restart", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now}
	f.writeFacts(t, "main", []factRecord{fact})
	proposal := factProposal{Action: factActionMerge, CandidateID: fact.ID, TargetID: "fact:absent", Confidence: 0.51, Branch: "main"}
	unrelated := factProposal{Action: factActionSupersede, CandidateID: "fact:unrelated", TargetID: fact.ID, Confidence: 0.55, Branch: "main"}
	if err := writeFactProposals(f.brainDir, "main", []factProposal{proposal, unrelated}); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--reject", fact.ID)
	if err != nil || !strings.Contains(out, "resolved 1 proposal(s)") {
		t.Fatalf("reject review: err=%v output=%q", err, out)
	}
	if queued, err := loadFactProposals(f.brainDir, "main"); err != nil || len(queued) != 1 || queued[0].CandidateID != unrelated.CandidateID {
		t.Fatalf("rejected proposal remained: %+v err=%v", queued, err)
	}
	got, err := loadFacts(f.brainDir, "main")
	if err != nil || len(got) != 1 || got[0].ID != fact.ID || got[0].Status != factStatusActive {
		t.Fatalf("reject mutated fact store: %+v err=%v", got, err)
	}
}

func TestSampleAcrossStrataRoundRobinsStableRepresentatives(t *testing.T) {
	var tasks []evalTask
	for _, stratum := range []string{queryTypeCode, queryTypeConcept, queryTypeHowto, queryTypeConvention} {
		for i := 0; i < 2; i++ {
			tasks = append(tasks, evalTask{ID: stratum + "-" + string(rune('a'+i)), Task: "task " + stratum, QueryType: stratum})
		}
	}
	one := sampleAcrossStrata(tasks, 5)
	two := sampleAcrossStrata(tasks, 5)
	if len(one) != 5 || len(two) != 5 {
		t.Fatalf("sample lengths = %d, %d; want 5", len(one), len(two))
	}
	seen := map[string]bool{}
	strata := map[string]int{}
	for i, task := range one {
		if task.ID == "" || seen[task.ID] {
			t.Fatalf("sample contains duplicate or empty task at %d: %+v", i, one)
		}
		seen[task.ID] = true
		strata[task.QueryType]++
		if i > 0 && one[i-1].ID >= task.ID {
			t.Fatalf("sample IDs are not stable sorted: %+v", one)
		}
		if task.Task == "" {
			t.Fatalf("sample lost task label: %+v", task)
		}
	}
	if len(strata) != 4 || strata[queryTypeCode] != 2 || strata[queryTypeConcept] != 1 || strata[queryTypeHowto] != 1 || strata[queryTypeConvention] != 1 {
		t.Fatalf("sample did not cover strata round-robin: %+v", strata)
	}
	for i := range one {
		if one[i].ID != two[i].ID {
			t.Fatalf("sampling is not deterministic: first=%+v second=%+v", one, two)
		}
	}
}
