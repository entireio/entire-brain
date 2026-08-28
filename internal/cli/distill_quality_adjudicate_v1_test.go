package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDistillQualityAdjudicationV1SavesAndResumes(t *testing.T) {
	root, packets := adjudicationFixtureRunV1(t)
	now := func() time.Time { return time.Date(2026, 8, 28, 19, 20, 21, 0, time.UTC) }
	var output bytes.Buffer
	result, err := runDistillQualityAdjudicationV1(root, "thomi", distillQualityAdjudicateQueueCalV1, 1, false, strings.NewReader("y\ny\ny\nClear standing rule.\n"), &output, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || result.Presented != 1 || result.AlreadyReviewed != 0 || result.Remaining != len(packets)-1 {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{"Candidate: ADMITTED", "Advisory scores", "copilot", "cursor", "claude", "Saved. Progress"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, output.String())
		}
	}
	info, err := os.Stat(result.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("adjudication mode = %o", info.Mode().Perm())
	}
	_, loadedPackets, _, aggregates, err := loadDistillQualityAdjudicationInputsV1(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadDistillQualityHumanAdjudicationsV1(root, "thomi", loadedPackets, aggregates)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Decision != distillQualityHumanAcceptV1 || records[0].Rationale != "Clear standing rule." {
		t.Fatalf("records = %+v", records)
	}
	wantProof := map[distillQualityDimensionV1]distillQualityProofLabelV1{
		distillQualityDimensionAdmissionV1:    distillQualityProofSupportedV1,
		distillQualityDimensionFaithfulnessV1: distillQualityProofNotApplicableV1,
		distillQualityDimensionAuthorityV1:    distillQualityProofSupportedV1,
		distillQualityDimensionTaxonomyV1:     distillQualityProofNotApplicableV1,
		distillQualityDimensionLocusV1:        distillQualityProofNotApplicableV1,
		distillQualityDimensionSafetyV1:       distillQualityProofSupportedV1,
	}
	for _, dimension := range records[0].Dimensions {
		if wantProof[dimension.Dimension] != dimension.ProofLabel {
			t.Fatalf("dimension %s = %s", dimension.Dimension, dimension.ProofLabel)
		}
		delete(wantProof, dimension.Dimension)
	}
	if len(wantProof) != 0 {
		t.Fatalf("missing dimensions: %v", wantProof)
	}

	before, err := os.ReadFile(result.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	resumed, err := runDistillQualityAdjudicationV1(root, "thomi", distillQualityAdjudicateQueueCritV1, 1, false, strings.NewReader(""), &output, now)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(result.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.AlreadyReviewed != 1 || resumed.Reviewed != 0 || !bytes.Equal(before, after) {
		t.Fatalf("resume result=%+v changed=%v", resumed, !bytes.Equal(before, after))
	}
}

func TestRunDistillQualityAdjudicationV1CanViewEvidenceRepeatedly(t *testing.T) {
	root, _ := adjudicationFixtureRunV1(t)
	var output bytes.Buffer
	result, err := runDistillQualityAdjudicationV1(root, "reviewer", distillQualityAdjudicateQueueCritV1, 1, false, strings.NewReader("v\nv\nn\nu\nn\n\n"), &output, func() time.Time { return time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || strings.Count(output.String(), "Evidence:\n") != 3 {
		t.Fatalf("result=%+v output=\n%s", result, output.String())
	}
}

func TestLoadDistillQualityHumanAdjudicationsV1RejectsStaleAdvisory(t *testing.T) {
	root, packets := adjudicationFixtureRunV1(t)
	_, _, sources, aggregates, err := loadDistillQualityAdjudicationInputsV1(root)
	if err != nil {
		t.Fatal(err)
	}
	items, err := buildDistillQualityAdjudicateItemsV1(packets, sources, aggregates, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := makeDistillQualityHumanAdjudicationV1("run", "reviewer", items[0], "yes", "yes", "yes", "", time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC))
	record.AdvisoryDigest = reportTestDigestV1("stale")
	humanDir := filepath.Join(root, distillQualityHumanDirV1)
	if err := ensureDistillQualityPanelDirV1(humanDir); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillQualityHumanAdjudicationsV1(filepath.Join(humanDir, "reviewer.jsonl"), []distillQualityHumanAdjudicationV1{record}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDistillQualityHumanAdjudicationsV1(root, "reviewer", packets, aggregates); err == nil || !strings.Contains(err.Error(), "advisory_digest mismatch") {
		t.Fatalf("stale advisory error = %v", err)
	}
}

func TestSelectDistillQualityAdjudicateQueueV1IsBalancedAndDeterministic(t *testing.T) {
	item := func(id string, admitted, critical, invalid, disagreement bool) distillQualityAdjudicateItemV1 {
		return distillQualityAdjudicateItemV1{Packet: distillQualityPanelPacketV1{PacketID: id}, CandidateAdmitted: admitted, Aggregate: distillQualityPanelAggregateV1{Critical: critical, Invalid: invalid, Disagreement: disagreement}}
	}
	items := []distillQualityAdjudicateItemV1{
		item("invalid", false, false, true, false),
		item("admitted-critical", true, true, false, false),
		item("filtered-critical", false, true, false, false),
		item("disagreement", false, false, false, true),
		item("clean", false, false, false, false),
		item("invalid-2", false, false, true, false),
	}
	selected := selectDistillQualityAdjudicateQueueV1(items, distillQualityAdjudicateQueueCalV1, 6)
	var ids []string
	for _, got := range selected {
		ids = append(ids, got.Packet.PacketID)
	}
	if got, want := strings.Join(ids, ","), "invalid,admitted-critical,filtered-critical,disagreement,clean,invalid-2"; got != want {
		t.Fatalf("selection=%s want=%s", got, want)
	}
}

func adjudicationFixtureRunV1(t *testing.T) (string, []distillQualityPanelPacketV1) {
	t.Helper()
	root, packets := reportFixtureRunV1(t)
	if err := os.Mkdir(filepath.Join(root, distillQualityPanelDirV1), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, judge := range distillQualityPanelJudgesV1 {
		verdict := reportAdmissionVerdictV1(packets[0], judge, distillQualityAdvisoryCriticalV1)
		data, err := marshalDistillQualityJSONLV1([]distillQualityPanelVerdictV1{verdict})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, distillQualityPanelDirV1, string(judge)+".jsonl"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, packets
}
