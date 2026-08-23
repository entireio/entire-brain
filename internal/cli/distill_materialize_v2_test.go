package cli

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

func distillCandidateMaterializationTestTarget() distillCandidateMaterializationTargetV2 {
	return distillCandidateMaterializationTargetV2{
		CandidateID:      "candidate-v1:test-candidate",
		Branch:           "feature/test",
		SourceSessionID:  "session-test",
		SourceCheckpoint: "checkpoint-test",
		SourceTranscript: "sessions/test.jsonl",
		TriggerTurnID:    "turn-test",
		StartLine:        8,
		EndLine:          12,
	}
}

func distillCandidateMaterializationTestResult(text string) distillCandidateCacheResultV2 {
	return distillCandidateCacheResultV2{Facts: []distillCandidateCachedFactV2{{
		Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: text,
	}}}
}

func TestMaterializeDistillCandidateResultV2ExplicitEmpty(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 11, 12, 0, time.UTC)
	got, err := materializeDistillCandidateResultV2(distillCandidateCacheResultV2{Empty: true}, distillCandidateMaterializationTestTarget(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty || len(got.Facts) != 0 || len(got.FactIDs) != 0 {
		t.Fatalf("empty materialization = %+v", got)
	}
	active := []factRecord{{ID: "fact:existing", Text: "existing", Status: factStatusActive}}
	updated, applied, err := applyDistillCandidateResultV2(active, distillCandidateCacheResultV2{Empty: true}, distillCandidateMaterializationTestTarget(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Empty || !reflect.DeepEqual(updated, active) {
		t.Fatalf("empty apply changed facts: got %+v want %+v", updated, active)
	}
}

func TestApplyDistillCandidateResultV2ExactDuplicatePreservesAuthoredLegacyAnchor(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 11, 12, 0, time.UTC)
	text := "The repository keeps candidate extraction branch independent."
	paths := []string{"architecture.data.flow"}
	id := factRecordID(text, paths)
	legacy := factAnchor{SessionID: "authored-session", Transcript: "sessions/legacy.jsonl", Line: 3}
	old := now.Add(-time.Hour)
	active := []factRecord{{
		ID: id, Paths: paths, Kind: factKindDecision, Text: text,
		Branch: "feature/test", Origin: factOriginAuthored, Status: factStatusActive,
		Provenance: []factAnchor{legacy}, CreatedAt: old, UpdatedAt: old,
	}}
	updated, applied, err := applyDistillCandidateResultV2(active, distillCandidateMaterializationTestResult(text), distillCandidateMaterializationTestTarget(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || len(applied.FactIDs) != 1 || applied.FactIDs[0] != id {
		t.Fatalf("exact apply = facts %+v materialization %+v", updated, applied)
	}
	got := updated[0]
	if got.Origin != factOriginAuthored || got.Status != factStatusActive || !got.CreatedAt.Equal(old) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("existing record state changed: %+v", got)
	}
	if len(got.Provenance) != 2 || got.Provenance[0] != legacy {
		t.Fatalf("legacy anchor was not preserved: %+v", got.Provenance)
	}
	wantNew := factAnchor{SessionID: "session-test", CheckpointID: "checkpoint-test", DistillTurnID: "turn-test", Transcript: "sessions/test.jsonl", Line: 8, EndLine: 12}
	if got.Provenance[1] != wantNew {
		t.Fatalf("new exact anchor = %+v, want %+v", got.Provenance[1], wantNew)
	}
}

func TestMaterializeDistillCandidateResultV2SeparateIDsRemainSeparate(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 11, 12, 0, time.UTC)
	result := distillCandidateCacheResultV2{Facts: []distillCandidateCachedFactV2{
		{Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: "The repository keeps candidate extraction branch independent."},
		{Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: "The repository keeps candidate extraction branch dependent."},
	}}
	updated, applied, err := applyDistillCandidateResultV2(nil, result, distillCandidateMaterializationTestTarget(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 2 || len(applied.FactIDs) != 2 || applied.FactIDs[0] == applied.FactIDs[1] {
		t.Fatalf("different IDs collapsed: facts=%+v IDs=%v", updated, applied.FactIDs)
	}
	for _, fact := range updated {
		if fact.Status != factStatusActive || fact.Origin != factOriginDistilled || len(fact.Provenance) != 1 {
			t.Fatalf("materialized fact state = %+v", fact)
		}
	}
}

func TestMaterializeDistillCandidateResultV2BranchProvenanceAndDeterministicIDs(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 11, 12, 0, time.UTC)
	target := distillCandidateMaterializationTestTarget()
	result := distillCandidateCacheResultV2{Facts: []distillCandidateCachedFactV2{
		{Kind: factKindDecision, Paths: []string{"workflow.testing.rules"}, Text: "Run the focused tests before applying candidate materialization."},
		{Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: "The branch owns its materialized provenance."},
	}}
	got, err := materializeDistillCandidateResultV2(result, target, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != target.Branch || got.CandidateID != target.CandidateID || len(got.FactIDs) != 2 {
		t.Fatalf("materialization identity = %+v", got)
	}
	if !sortStringsAreAscending(got.FactIDs) {
		t.Fatalf("fact IDs are not deterministic/sorted: %v", got.FactIDs)
	}
	for _, fact := range got.Facts {
		if fact.Branch != target.Branch || !fact.CreatedAt.Equal(now) || !fact.UpdatedAt.Equal(now) {
			t.Fatalf("fact target/timestamps = %+v", fact)
		}
		if len(fact.Provenance) != 1 || fact.Provenance[0].SessionID != target.SourceSessionID || fact.Provenance[0].CheckpointID != target.SourceCheckpoint || fact.Provenance[0].DistillTurnID != target.TriggerTurnID || fact.Provenance[0].Line != target.StartLine || fact.Provenance[0].EndLine != target.EndLine {
			t.Fatalf("fact anchor = %+v", fact.Provenance)
		}
	}
	// Repeating pure materialization produces the same receipt-facing IDs.
	repeated, err := materializeDistillCandidateResultV2(result, target, now)
	if err != nil || !reflect.DeepEqual(got.FactIDs, repeated.FactIDs) {
		t.Fatalf("repeated IDs = %v, err=%v", repeated.FactIDs, err)
	}
}

func TestMaterializeDistillCandidateResultV2RejectsInvalidTargetAndResult(t *testing.T) {
	now := time.Date(2026, time.August, 23, 10, 11, 12, 0, time.UTC)
	cases := []struct {
		name   string
		result distillCandidateCacheResultV2
		target distillCandidateMaterializationTargetV2
	}{
		{name: "implicit empty", result: distillCandidateCacheResultV2{}, target: distillCandidateMaterializationTestTarget()},
		{name: "empty with facts", result: distillCandidateCacheResultV2{Empty: true, Facts: []distillCandidateCachedFactV2{{Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: "fact"}}}, target: distillCandidateMaterializationTestTarget()},
		{name: "blank candidate", result: distillCandidateCacheResultV2{Empty: true}, target: func() distillCandidateMaterializationTargetV2 {
			target := distillCandidateMaterializationTestTarget()
			target.CandidateID = " "
			return target
		}()},
		{name: "bad line range", result: distillCandidateCacheResultV2{Empty: true}, target: func() distillCandidateMaterializationTargetV2 {
			target := distillCandidateMaterializationTestTarget()
			target.EndLine = 1
			return target
		}()},
		{name: "absolute transcript", result: distillCandidateCacheResultV2{Empty: true}, target: func() distillCandidateMaterializationTargetV2 {
			target := distillCandidateMaterializationTestTarget()
			target.SourceTranscript = "/tmp/session.jsonl"
			return target
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := materializeDistillCandidateResultV2(tc.result, tc.target, now); err == nil {
				t.Fatal("invalid materialization was accepted")
			}
		})
	}
	if _, err := materializeDistillCandidateResultV2(distillCandidateCacheResultV2{Empty: true}, distillCandidateMaterializationTestTarget(), time.Time{}); err == nil {
		t.Fatal("zero timestamp was accepted")
	}
}

func sortStringsAreAscending(values []string) bool {
	return reflect.DeepEqual(values, sortedStrings(values))
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
