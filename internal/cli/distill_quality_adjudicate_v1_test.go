package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/tui"
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
	for _, want := range []string{"SELECTED FOR EXTRACTION", "STATEMENT UNDER REVIEW", "INDEPENDENT MODEL ADVICE", "copilot", "cursor", "claude", "Saved. Progress"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "raw={") {
		t.Fatalf("plain fallback exposed raw score JSON:\n%s", output.String())
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
	if result.Reviewed != 1 || strings.Count(output.String(), "STATEMENT UNDER REVIEW\n") != 3 {
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

func TestMakeDistillQualityReviewV1IsReadableAndKeepsAdvisoriesSeparate(t *testing.T) {
	root, _ := adjudicationFixtureRunV1(t)
	session, err := openDistillQualityAdjudicationSessionV1(root, "reviewer", distillQualityAdjudicateQueueCalV1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.close() }()
	review, byID := makeDistillQualityReviewV1(session, distillQualityAdjudicateQueueCalV1)
	if len(review.Items) != 1 || len(byID) != 1 {
		t.Fatalf("review items=%d lookup=%d", len(review.Items), len(byID))
	}
	item := review.Items[0]
	if item.Selector != "SELECTED" || len(item.Focus) == 0 || !strings.Contains(item.SelectorReason, "rule-like wording") {
		t.Fatalf("selector/focus was not human-readable: %+v", item)
	}
	if len(item.Judges) != len(distillQualityPanelJudgesV1) {
		t.Fatalf("judges=%d want=%d", len(item.Judges), len(distillQualityPanelJudgesV1))
	}
	for _, judge := range item.Judges {
		if judge.Position != "disagrees_with_filter" || judge.Why == "" {
			t.Fatalf("judge %s lacks a compact filter decision: %+v", judge.Name, judge)
		}
	}
	if !strings.Contains(item.PanelSummary, "FILTER WRONG") || item.AnswerKind != tui.QualityReviewPanelAgreement {
		t.Fatalf("panel summary/question = %q / %q", item.PanelSummary, item.AnswerKind)
	}
	if got := review.Progress; got.Reviewer != "reviewer" || got.Queue != distillQualityAdjudicateQueueCalV1 || got.Remaining == 0 {
		t.Fatalf("progress = %+v", got)
	}
}

func TestDistillQualityFilterPanelOmitsUnavailableJudgesAndHandlesNoMajority(t *testing.T) {
	packet := distillQualityPanelPacketV1{PacketID: "packet"}
	completedNo := reportAdmissionVerdictV1(packet, distillQualityJudgeCopilotV1, distillQualityAdvisoryPassV1)
	completedNo.Scores[0].RawScore = []byte(`{"should_admit":"no","candidate_admitted":true,"critical_miss":false}`)
	completedYes := reportAdmissionVerdictV1(packet, distillQualityJudgeCursorV1, distillQualityAdvisoryPassV1)
	completedYes.Scores[0].RawScore = []byte(`{"should_admit":"yes","candidate_admitted":true,"critical_miss":false}`)
	invalid := distillQualityPanelVerdictV1{Judge: distillQualityJudgeClaudeV1, State: distillQualityVerdictInvalidV1}
	item := distillQualityAdjudicateItemV1{
		Packet: packet, CandidateAdmitted: true,
		Aggregate: distillQualityPanelAggregateV1{Verdicts: []distillQualityPanelVerdictV1{completedNo, completedYes, invalid}},
	}
	view := makeDistillQualityReviewItemV1("Item 01", item)
	if len(view.Judges) != 2 {
		t.Fatalf("available judges=%d want=2: %+v", len(view.Judges), view.Judges)
	}
	if view.AnswerKind != tui.QualityReviewFilterCorrect || !strings.Contains(view.PanelSummary, "No majority") {
		t.Fatalf("split panel = kind %q summary %q", view.AnswerKind, view.PanelSummary)
	}
}

func TestDistillQualityFilterAssessmentStoresOnlyTheAdmissionDecision(t *testing.T) {
	root, packets := adjudicationFixtureRunV1(t)
	session, err := openDistillQualityAdjudicationSessionV1(root, "simple-reviewer", distillQualityAdjudicateQueueCalV1, 1)
	if err != nil {
		t.Fatal(err)
	}
	item := session.items[0]
	when := time.Date(2026, 9, 10, 12, 34, 56, 0, time.UTC)
	if err := session.saveFilterAssessment(item, false, "Disagreed with the available judges' majority.", when); err != nil {
		t.Fatal(err)
	}
	path := session.recordPath
	if err := session.close(); err != nil {
		t.Fatal(err)
	}
	_, loadedPackets, _, aggregates, err := loadDistillQualityAdjudicationInputsV1(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadDistillQualityHumanAdjudicationsV1(root, "simple-reviewer", loadedPackets, aggregates)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Decision != distillQualityHumanRejectV1 || records[0].Rationale != "Disagreed with the available judges' majority." {
		t.Fatalf("record = %+v", records)
	}
	if len(records[0].Dimensions) != 1 || records[0].Dimensions[0].Dimension != distillQualityDimensionAdmissionV1 || records[0].Dimensions[0].ProofLabel != distillQualityProofUnsupportedV1 {
		t.Fatalf("filter assessment claimed extra proof: %+v", records[0].Dimensions)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored assessment info=%v err=%v", info, err)
	}
	resumed, err := openDistillQualityAdjudicationSessionV1(root, "simple-reviewer", distillQualityAdjudicateQueueCalV1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.close() }()
	if resumed.result.AlreadyReviewed != 1 || resumed.result.Remaining != len(packets)-1 {
		t.Fatalf("resume state = %+v items=%d", resumed.result, len(resumed.items))
	}
}

func TestDistillQualityTerminalTextV1MakesControlsInert(t *testing.T) {
	got := distillQualityTerminalTextV1("safe\x1b]52;c;clipboard\a\rrewritten\u202etrick")
	for _, forbidden := range []string{"\x1b", "\a", "\r", "\u202e"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("terminal text retained control %q: %q", forbidden, got)
		}
	}
	if got != "safe�]52;c;clipboard��rewritten�trick" {
		t.Fatalf("terminal text = %q", got)
	}
}

func TestDistillQualityReviewFocusV1IsolatesHumanStatementAcrossFragments(t *testing.T) {
	packet := distillQualityPanelPacketV1{Payload: distillQualityPanelPayloadV1{Fragments: []distillQualityPanelFragmentV1{
		{Text: "assistant: a very long prior analysis that is not being judged\nuser: alright now make a concrete sonnet pl"},
		{Text: "an, don't rush\nassistant: I will read the existing plan first"},
	}}}
	metadata := distillQualityAdjudicateMetadataV1{
		CandidateAdmitted: true,
		Roles:             []string{"user"},
		Authorities:       []string{"direct_user"},
		Cues:              []string{"rule"},
	}
	focus, context, err := distillQualityReviewFocusV1(packet, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(focus) != 1 || focus[0].Role != "user" || focus[0].Text != "alright now make a concrete sonnet plan, don't rush" {
		t.Fatalf("focus = %+v", focus)
	}
	if len(context) != 2 || strings.Contains(focus[0].Text, "prior analysis") {
		t.Fatalf("context = %+v focus=%+v", context, focus)
	}
}

func TestDistillQualityReviewFocusV1KeepsAcceptedProposalWithConfirmation(t *testing.T) {
	packet := distillQualityPanelPacketV1{Payload: distillQualityPanelPayloadV1{Fragments: []distillQualityPanelFragmentV1{{
		Text: "assistant: Use PostgreSQL for durable storage.\nuser: Sounds good.\nassistant: I will implement it.",
	}}}}
	metadata := distillQualityAdjudicateMetadataV1{
		CandidateAdmitted: true,
		Roles:             []string{"user"},
		Authorities:       []string{"direct_user"},
		Cues:              []string{string(distillCandidateCueAcceptanceV1)},
	}
	focus, context, err := distillQualityReviewFocusV1(packet, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(focus) != 2 || focus[0].Role != "assistant" || focus[1].Role != "user" || focus[1].Text != "Sounds good." {
		t.Fatalf("focus = %+v", focus)
	}
	if len(context) != 1 || context[0].Text != "I will implement it." {
		t.Fatalf("context = %+v", context)
	}
}

func TestDistillQualityReviewFocusV1RefusesAmbiguousOrIncompleteEvidence(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		metadata distillQualityAdjudicateMetadataV1
	}{
		{
			name:     "duplicate trigger role",
			text:     "user: first\nassistant: context\nuser: second",
			metadata: distillQualityAdjudicateMetadataV1{CandidateAdmitted: true, Roles: []string{"user"}},
		},
		{
			name:     "filtered packet with context",
			text:     "user: focus\nassistant: unexpected context",
			metadata: distillQualityAdjudicateMetadataV1{Roles: []string{"user"}},
		},
		{
			name:     "truncated",
			text:     "user: incomplete",
			metadata: distillQualityAdjudicateMetadataV1{Roles: []string{"user"}, EvidenceTruncated: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := distillQualityPanelPacketV1{Payload: distillQualityPanelPayloadV1{Fragments: []distillQualityPanelFragmentV1{{Text: test.text}}}}
			if _, _, err := distillQualityReviewFocusV1(packet, test.metadata); err == nil {
				t.Fatal("expected ambiguous evidence to be refused")
			}
		})
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
