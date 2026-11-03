package factmerge

import (
	"bytes"
	"testing"
	"time"
)

func TestExtractorFieldsAreOutsideIdentityAndSurviveNDJSON(t *testing.T) {
	paths := []string{"project.testing"}
	r := Record{ID: RecordID("fact text", paths), Paths: paths, Text: "fact text",
		Branch: "main", Origin: "distilled", Status: "active",
		Provenance: []Anchor{{SessionID: "s"}}, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
	stamped := r
	stamped.Agent = "claude"
	stamped.Model = "claude-sonnet-5"
	stamped.Effort = "high"

	if RecordID(stamped.Text, stamped.Paths) != r.ID {
		t.Fatal("extractor fields must not affect RecordID")
	}

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, []Record{stamped}); err != nil {
		t.Fatal(err)
	}
	round, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(round) != 1 || round[0].Agent != "claude" || round[0].Model != "claude-sonnet-5" || round[0].Effort != "high" {
		t.Fatalf("extractor fields must survive an NDJSON round trip: %+v", round)
	}

	var plain bytes.Buffer
	if err := WriteNDJSON(&plain, []Record{r}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain.Bytes(), []byte(`"model"`)) {
		t.Fatal("empty extractor fields must be omitted from NDJSON")
	}
}

func TestMergeKeepsTargetExtractorFields(t *testing.T) {
	now := time.Unix(2, 0).UTC()
	target := Record{Paths: []string{"project.testing"}, Text: "target fact",
		Branch: "main", Origin: "distilled", Status: StatusActive, Model: "model-a",
		Provenance: []Anchor{{SessionID: "s1"}}, CreatedAt: now, UpdatedAt: now}
	target.ID = RecordID(target.Text, target.Paths)
	candidate := Record{Paths: []string{"project.testing"}, Text: "candidate fact",
		Branch: "main", Origin: "distilled", Status: StatusActive, Model: "model-b",
		Provenance: []Anchor{{SessionID: "s2"}}, CreatedAt: now, UpdatedAt: now}
	candidate.ID = RecordID(candidate.Text, candidate.Paths)

	merged := ApplyActions([]Record{target},
		[]Action{{Kind: ActionMerge, TargetID: target.ID, Confidence: 0.99, Candidate: candidate}},
		DefaultConfidenceThreshold, now)
	if len(merged) != 1 || merged[0].Model != "model-a" {
		t.Fatalf("merge must keep the surviving record's extractor model: %+v", merged)
	}
}
