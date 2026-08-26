package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func qualityBundleTestInputV1(id, branch string) distillQualityBundleInputV1 {
	session := candidateTestSession()
	session.SessionID = id
	session.Branch = branch
	session.TranscriptPath = "sessions/" + branch + "/" + id + ".jsonl"
	session.LatestCheckpoint = "checkpoint-" + id
	return distillQualityBundleInputV1{Session: session, Branch: branch, Content: strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible ghp_12345678901234567890123456789012"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"The migration rollback test passed."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Can you summarize the result?"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"The summary is ready."}}`,
	}, "\n")}
}

func TestBuildDistillQualityBundleV1RedactsAndOmitsProvenance(t *testing.T) {
	bundle, err := buildDistillQualityBundleV1([]distillQualityBundleInputV1{qualityBundleTestInputV1("session-a", "main")}, distillQualityBundleConfigV1{MaxFilteredItems: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Admission) != 1 || !bundle.Admission[0].Admitted {
		t.Fatalf("admission = %+v", bundle.Admission)
	}
	if len(bundle.Filtered) == 0 || bundle.Filtered[0].Admitted {
		t.Fatalf("filtered = %+v", bundle.Filtered)
	}
	if strings.Contains(bundle.Admission[0].Evidence, "ghp_123") {
		t.Fatal("secret survived redaction")
	}
	public, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"session-a", "sessions/main/session-a.jsonl", "checkpoint-session-a", `"branch"`, `"session_id"`} {
		if strings.Contains(string(public), forbidden) {
			t.Fatalf("public payload contains %q: %s", forbidden, public)
		}
	}
	if len(bundle.Sources) == 0 || bundle.Sources[0].SessionID == "" {
		t.Fatal("private source map did not retain provenance")
	}
}

func TestBuildDistillQualityBundleV1StableIDsAndDeduplicatesCards(t *testing.T) {
	inputs := []distillQualityBundleInputV1{qualityBundleTestInputV1("session-a", "main"), qualityBundleTestInputV1("session-b", "feature")}
	one, err := buildDistillQualityBundleV1(inputs, distillQualityBundleConfigV1{MaxFilteredItems: 50})
	if err != nil {
		t.Fatal(err)
	}
	two, err := buildDistillQualityBundleV1([]distillQualityBundleInputV1{inputs[1], inputs[0]}, distillQualityBundleConfigV1{MaxFilteredItems: 50})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(two)
	if string(a) != string(b) {
		t.Fatalf("bundle changed with input order\n%s\n%s", a, b)
	}
	if len(one.Admission) != 1 {
		t.Fatalf("identical admitted cards were not collapsed: %d", len(one.Admission))
	}
	if len(one.Sources) < 2 {
		t.Fatalf("deduplication lost source anchors: %d", len(one.Sources))
	}
}

func TestBuildDistillQualityBundleV1DeduplicatesFilteredViewsAndRetainsSources(t *testing.T) {
	main := qualityBundleTestInputV1("session-a", "main")
	feature := qualityBundleTestInputV1("session-a", "feature")
	bundle, err := buildDistillQualityBundleV1([]distillQualityBundleInputV1{main, feature}, distillQualityBundleConfigV1{MaxFilteredItems: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range bundle.Filtered {
		if seen[item.ID] {
			t.Fatalf("duplicate filtered item %q", item.ID)
		}
		seen[item.ID] = true
		matches := 0
		for _, source := range bundle.Sources {
			if source.PublicID == item.ID {
				matches++
			}
		}
		if matches != 2 {
			t.Fatalf("filtered item %q retained %d sources, want 2", item.ID, matches)
		}
	}
}

func TestBuildDistillQualityBundleV1StratifiesFilteredSample(t *testing.T) {
	bundle, err := buildDistillQualityBundleV1([]distillQualityBundleInputV1{qualityBundleTestInputV1("session-a", "main")}, distillQualityBundleConfigV1{MaxFilteredItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Filtered) != 2 {
		t.Fatalf("filtered sample = %d, want 2", len(bundle.Filtered))
	}
	roles := map[string]bool{}
	for _, item := range bundle.Filtered {
		roles[item.Roles[0]] = true
	}
	if !roles["assistant"] || !roles["user"] {
		t.Fatalf("sample was not stratified across visible roles: %+v", roles)
	}
}
