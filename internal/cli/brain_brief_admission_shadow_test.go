package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBrainBriefAdmissionShadowDeliversAlwaysPacketBytes(t *testing.T) {
	report := comprehensiveCompactV1Report()
	always, err := prepareBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("prepare always: %v", err)
	}
	shadow, err := prepareBrainBriefAgentV1(report, brainBriefDeliveryShadow, 8)
	if err != nil {
		t.Fatalf("prepare shadow: %v", err)
	}
	if always.projection.packet != shadow.projection.packet {
		t.Fatal("shadow changed the always-bound agent_v1 projection")
	}
	if always.shadow != nil {
		t.Fatal("always mode created a shadow artifact")
	}
	if shadow.shadow == nil {
		t.Fatal("shadow mode did not create its pure evaluator artifact")
	}
	if err := validateBrainBriefAdmissionShadowArtifact(*shadow.shadow); err != nil {
		t.Fatalf("validate shadow artifact: %v", err)
	}
	if strings.Contains(shadow.projection.packet, "admission_shadow") ||
		!strings.Contains(shadow.projection.packet, `delivery_policy="always"`) {
		t.Fatalf("shadow control state leaked into packet:\n%s", shadow.projection.packet)
	}

	var alwaysOut, shadowOut bytes.Buffer
	alwaysCmd := &cobra.Command{}
	alwaysCmd.SetOut(&alwaysOut)
	shadowCmd := &cobra.Command{}
	shadowCmd.SetOut(&shadowOut)
	if err := emitBrainBriefAgentV1(alwaysCmd, report, brainBriefDeliveryAlways, 8); err != nil {
		t.Fatalf("emit always: %v", err)
	}
	if err := emitBrainBriefAgentV1(shadowCmd, report, brainBriefDeliveryShadow, 8); err != nil {
		t.Fatalf("emit shadow: %v", err)
	}
	if !bytes.Equal(alwaysOut.Bytes(), shadowOut.Bytes()) {
		t.Fatal("shadow changed bytes delivered to the agent")
	}
}

func TestBrainBriefAdmissionShadowEmptyAndNullRetrieval(t *testing.T) {
	empty := emptyBrainBriefAdmissionReport()
	emptyArtifact := evaluateBrainBriefAdmissionShadow(empty, 8)
	assertBrainBriefAdmissionServe(t, emptyArtifact, brainBriefAdmissionShadowStatusDiagnostic)
	if emptyArtifact.Features.FactsState != brainBriefAdmissionSignalEmpty ||
		emptyArtifact.Features.HistoryState != brainBriefAdmissionSignalEmpty ||
		emptyArtifact.Features.SemanticState != brainBriefAdmissionSignalEmpty ||
		emptyArtifact.Features.EvidenceSourceCount != 0 ||
		!brainBriefAdmissionHasReason(emptyArtifact, "evidence_empty") {
		t.Fatalf("empty retrieval artifact = %+v", emptyArtifact)
	}

	nullReport := brainBriefReport{Task: "null retrieval"}
	nullArtifact := evaluateBrainBriefAdmissionShadow(nullReport, 8)
	assertBrainBriefAdmissionServe(t, nullArtifact, brainBriefAdmissionShadowStatusUnknown)
	for _, reason := range []string{"facts_unavailable", "history_unavailable", "semantic_unavailable", "fail_safe_serve"} {
		if !brainBriefAdmissionHasReason(nullArtifact, reason) {
			t.Errorf("null retrieval missing reason %q: %+v", reason, nullArtifact.ReasonCodes)
		}
	}
}

func TestBrainBriefAdmissionShadowSuccessfulNilNoHitRetrievalIsEmpty(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	// These nil results match successful no-hit production shapes from fused
	// facts, merged history, and semantic test suggestions. Explicit stage
	// success—not slice nilness—distinguishes them from unavailable retrieval.
	report.Facts = nil
	report.History.Matches = nil
	report.Semantic.Context.Symbols = nil
	report.Semantic.Tests.Suggestions = nil
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusDiagnostic)
	if artifact.Features.FactsState != brainBriefAdmissionSignalEmpty ||
		artifact.Features.HistoryState != brainBriefAdmissionSignalEmpty ||
		artifact.Features.SemanticState != brainBriefAdmissionSignalEmpty ||
		!brainBriefAdmissionHasReason(artifact, "evidence_empty") {
		t.Fatalf("successful nil/no-hit retrieval was not empty: %+v", artifact)
	}
	for _, unavailable := range []string{"facts_unavailable", "history_unavailable", "semantic_unavailable", "fail_safe_serve"} {
		if brainBriefAdmissionHasReason(artifact, unavailable) {
			t.Errorf("successful nil/no-hit retrieval got unavailable reason %q: %v", unavailable, artifact.ReasonCodes)
		}
	}
}

func TestBrainBriefAdmissionShadowWeakAndStrongEvidenceStayDiagnostic(t *testing.T) {
	weak := emptyBrainBriefAdmissionReport()
	weak.Facts = []factRecord{{ID: "fact:weak", Text: "one selected fact", Status: factStatusActive}}
	weakArtifact := evaluateBrainBriefAdmissionShadow(weak, 8)
	assertBrainBriefAdmissionServe(t, weakArtifact, brainBriefAdmissionShadowStatusDiagnostic)
	if weakArtifact.Features.EvidenceSourceCount != 1 || !brainBriefAdmissionHasReason(weakArtifact, "facts_present") {
		t.Fatalf("weak evidence features = %+v reasons=%v", weakArtifact.Features, weakArtifact.ReasonCodes)
	}

	strong := weak
	strong.History.Matches = []brainTextMatch{{Excerpt: "selected history", Score: 4}}
	strong.Semantic.Context.Symbols = []semanticRecord{{ID: "sym:1", Name: "Validate", FilePath: "internal/a.go", Confidence: 0.9}}
	strong.Semantic.Tests.Suggestions = []semanticTestSuggestion{}
	strong.ActionChecklist = []brainBriefAction{{File: "internal/a.go", Action: "inspect"}}
	strong.LikelyEditFiles = []string{"internal/a.go"}
	strong.LikelyTestFiles = []string{"internal/a_test.go"}
	strong.LikelyFiles = []string{"internal/a.go", "internal/a_test.go"}
	strongArtifact := evaluateBrainBriefAdmissionShadow(strong, 8)
	assertBrainBriefAdmissionServe(t, strongArtifact, brainBriefAdmissionShadowStatusDiagnostic)
	if strongArtifact.Features.EvidenceSourceCount != 4 {
		t.Fatalf("strong evidence source count = %d, want 4: %+v", strongArtifact.Features.EvidenceSourceCount, strongArtifact.Features)
	}
	for _, reason := range []string{"facts_present", "history_present", "semantic_present", "synthesis_present"} {
		if !brainBriefAdmissionHasReason(strongArtifact, reason) {
			t.Errorf("strong evidence missing reason %q: %v", reason, strongArtifact.ReasonCodes)
		}
	}
}

func TestBrainBriefAdmissionShadowTrustSignalsFailSafeToUnknownServe(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	report.Facts = []factRecord{{ID: "fact:1", Text: "selected", Status: factStatusActive}}
	report.FactsPendingReview = map[string]factReviewNotice{
		"fact:1": {ReviewID: "review:1", Confidence: 0.8, Message: "verify"},
	}
	report.FactsLocusDrift = map[string][]string{"fact:1": {"pkg.Old"}}
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
	if artifact.Features.TrustSignalCount != 2 {
		t.Fatalf("trust signal count = %d, want 2", artifact.Features.TrustSignalCount)
	}
	for _, reason := range []string{"pending_fact_review", "fact_locus_drift", "fail_safe_serve"} {
		if !brainBriefAdmissionHasReason(artifact, reason) {
			t.Errorf("trust artifact missing %q: %v", reason, artifact.ReasonCodes)
		}
	}
}

func TestBrainBriefAdmissionShadowUnavailableProposalStateFailsSafe(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	report.Facts = []factRecord{{ID: "fact:1", Text: "selected", Status: factStatusActive}}
	report.Warnings = []string{factReviewQueueUnavailableWarning}
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
	if !artifact.Features.ProposalStateUnavailable || artifact.Features.TrustSignalCount != 1 ||
		!brainBriefAdmissionHasReason(artifact, "proposal_state_unavailable") {
		t.Fatalf("proposal-state artifact = %+v", artifact)
	}
}

func TestBrainBriefAdmissionShadowNonFiniteValuesFailSafe(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*brainBriefReport)
	}{
		{
			name: "review_nan",
			mutate: func(report *brainBriefReport) {
				report.FactsPendingReview = map[string]factReviewNotice{
					"fact:1": {ReviewID: "review:1", Confidence: math.NaN(), Message: "verify"},
				}
			},
		},
		{
			name: "review_positive_infinity",
			mutate: func(report *brainBriefReport) {
				report.FactsPendingReview = map[string]factReviewNotice{
					"fact:1": {ReviewID: "review:1", Confidence: math.Inf(1), Message: "verify"},
				}
			},
		},
		{
			name: "symbol_negative_infinity",
			mutate: func(report *brainBriefReport) {
				report.Semantic.Context.Symbols = []semanticRecord{{ID: "sym:1", Confidence: math.Inf(-1)}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := emptyBrainBriefAdmissionReport()
			report.Facts = []factRecord{{ID: "fact:1", Text: "selected", Status: factStatusActive}}
			tt.mutate(&report)
			artifact := evaluateBrainBriefAdmissionShadow(report, 8)
			assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
			if artifact.Features.NonFiniteConfidenceCount != 1 || !brainBriefAdmissionHasReason(artifact, "non_finite_signal") {
				t.Fatalf("non-finite artifact = %+v", artifact)
			}

			alwaysEmission, alwaysErr := prepareBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
			shadowEmission, shadowErr := prepareBrainBriefAgentV1(report, brainBriefDeliveryShadow, 8)
			if alwaysErr == nil || shadowErr == nil {
				t.Fatalf("non-finite packet errors: always=%v shadow=%v", alwaysErr, shadowErr)
			}
			if alwaysErr.Error() != shadowErr.Error() {
				t.Fatalf("shadow changed non-finite packet error: always=%q shadow=%q", alwaysErr, shadowErr)
			}
			if alwaysEmission.shadow != nil {
				t.Fatal("always mode created a shadow artifact on packet failure")
			}
			if shadowEmission.shadow == nil {
				t.Fatal("shadow artifact was lost when the always-bound packet failed")
			}
			assertBrainBriefAdmissionServe(t, *shadowEmission.shadow, brainBriefAdmissionShadowStatusUnknown)
			if !brainBriefAdmissionHasReason(*shadowEmission.shadow, "non_finite_signal") {
				t.Fatalf("production shadow artifact lost non-finite reason: %+v", shadowEmission.shadow)
			}

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := emitBrainBriefAgentV1(cmd, report, brainBriefDeliveryShadow, 8); err == nil {
				t.Fatal("shadow emission unexpectedly accepted a non-finite packet")
			}
			if out.Len() != 0 {
				t.Fatalf("shadow emission wrote %d bytes before packet failure", out.Len())
			}
		})
	}
}

func TestBrainBriefAdmissionShadowPacketBuildErrorsRemainOutOfBandAndFailSafe(t *testing.T) {
	tests := []struct {
		name    string
		report  func() brainBriefReport
		errText string
	}{
		{
			name: "effective_fact_limit",
			report: func() brainBriefReport {
				report := emptyBrainBriefAdmissionReport()
				for i := 0; i <= brainBriefFactsLimit; i++ {
					report.Facts = append(report.Facts, factRecord{
						ID: fmt.Sprintf("fact:%d", i), Text: "selected", Status: factStatusActive,
					})
				}
				return report
			},
			errText: "effective fact limit",
		},
		{
			name: "mandatory_packet_overflow",
			report: func() brainBriefReport {
				report := emptyBrainBriefAdmissionReport()
				report.Facts = []factRecord{{
					ID: "fact:overflow", Text: strings.Repeat("x", brainBriefAgentV1BudgetBytes), Status: factStatusActive,
				}}
				return report
			},
			errText: "mandatory packet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := tt.report()
			preBuild := evaluateBrainBriefAdmissionShadow(report, 8)
			assertBrainBriefAdmissionServe(t, preBuild, brainBriefAdmissionShadowStatusDiagnostic)

			_, alwaysErr := prepareBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
			shadowEmission, shadowErr := prepareBrainBriefAgentV1(report, brainBriefDeliveryShadow, 8)
			if alwaysErr == nil || shadowErr == nil || !strings.Contains(alwaysErr.Error(), tt.errText) {
				t.Fatalf("packet errors: always=%v shadow=%v", alwaysErr, shadowErr)
			}
			if alwaysErr.Error() != shadowErr.Error() {
				t.Fatalf("shadow changed packet error: always=%q shadow=%q", alwaysErr, shadowErr)
			}
			if shadowEmission.shadow == nil {
				t.Fatal("shadow artifact was lost on packet build error")
			}
			assertBrainBriefAdmissionServe(t, *shadowEmission.shadow, brainBriefAdmissionShadowStatusUnknown)
			for _, reason := range []string{"packet_build_error", "fail_safe_serve"} {
				if !brainBriefAdmissionHasReason(*shadowEmission.shadow, reason) {
					t.Fatalf("packet-error artifact missing %q: %v", reason, shadowEmission.shadow.ReasonCodes)
				}
			}

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := emitBrainBriefAgentV1(cmd, report, brainBriefDeliveryShadow, 8); err == nil {
				t.Fatal("shadow emission unexpectedly accepted packet build failure")
			}
			if out.Len() != 0 {
				t.Fatalf("shadow emission wrote %d bytes before packet build failure", out.Len())
			}
		})
	}
}

func TestBrainBriefAdmissionShadowUnsafePathsRemainPrivateAndFailSafe(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	report.Task = "prose /Users/private/task remains outside artifact"
	report.Facts = []factRecord{{
		ID: "fact:1", Text: "prose /Users/private/fact remains outside artifact", Status: factStatusActive,
		Paths: []string{"/Users/private/fact-path"}, Locus: []string{`C:\private\Fact`},
	}}
	report.Status.Live.ChangedFiles = []string{"../private/live.go"}
	report.ActionChecklist = []brainBriefAction{{File: `\\server\share\action.go`, Action: "inspect"}}
	report.LikelyEditFiles = []string{"file:///Users/private/edit.go"}
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
	if artifact.Features.UnsafeStructuredValueCount < 5 || !brainBriefAdmissionHasReason(artifact, "unsafe_structured_input") {
		t.Fatalf("unsafe-path artifact = %+v", artifact)
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/Users/private", `C:\private`, `\\server\share`, "../private", "file:///"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("shadow artifact leaked private input %q: %s", private, data)
		}
	}
}

func TestBrainBriefAdmissionShadowDeterministicAcrossMapInsertionOrder(t *testing.T) {
	first := emptyBrainBriefAdmissionReport()
	first.Facts = []factRecord{
		{ID: "fact:1", Text: "first", Status: factStatusActive},
		{ID: "fact:2", Text: "second", Status: factStatusActive},
	}
	first.FactsLocusDrift = map[string][]string{}
	first.FactsLocusDrift["fact:1"] = []string{"OldOne"}
	first.FactsLocusDrift["fact:2"] = []string{"OldTwo"}
	first.FactsPendingReview = map[string]factReviewNotice{}
	first.FactsPendingReview["fact:1"] = factReviewNotice{ReviewID: "review:1", Confidence: 0.8}
	first.FactsPendingReview["fact:2"] = factReviewNotice{ReviewID: "review:2", Confidence: 0.7}

	second := first
	second.FactsLocusDrift = map[string][]string{}
	second.FactsLocusDrift["fact:2"] = []string{"OldTwo"}
	second.FactsLocusDrift["fact:1"] = []string{"OldOne"}
	second.FactsPendingReview = map[string]factReviewNotice{}
	second.FactsPendingReview["fact:2"] = factReviewNotice{ReviewID: "review:2", Confidence: 0.7}
	second.FactsPendingReview["fact:1"] = factReviewNotice{ReviewID: "review:1", Confidence: 0.8}

	firstArtifact := evaluateBrainBriefAdmissionShadow(first, 8)
	secondArtifact := evaluateBrainBriefAdmissionShadow(second, 8)
	if !reflect.DeepEqual(firstArtifact, secondArtifact) {
		t.Fatalf("map insertion order changed artifact\nfirst:  %+v\nsecond: %+v", firstArtifact, secondArtifact)
	}
}

func TestBrainBriefAdmissionShadowIdentityBindsConfiguration(t *testing.T) {
	config := defaultBrainBriefAdmissionShadowConfig()
	packetConfig := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 8, 6)
	const want = "sha256:04bdda0824459e82186cb6d407ffc2cd4be14d96c86b9f87c976bd3047bc9616"
	base := brainBriefAdmissionShadowConfigIdentity(config, packetConfig, 8, 6)
	if base != want {
		t.Fatalf("shadow config identity = %q, want frozen %q", base, want)
	}
	mutations := []struct {
		name      string
		config    brainBriefAdmissionShadowConfig
		packet    string
		requested int
		effective int
	}{
		{name: "schema", config: func() brainBriefAdmissionShadowConfig { c := config; c.SchemaVersion++; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "version", config: func() brainBriefAdmissionShadowConfig { c := config; c.EvaluatorVersion += "-changed"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "rule", config: func() brainBriefAdmissionShadowConfig { c := config; c.Rule += ";changed"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "thresholds", config: func() brainBriefAdmissionShadowConfig { c := config; c.Thresholds += ";changed"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "product_commit", config: func() brainBriefAdmissionShadowConfig {
			c := config
			c.BaseProductCommit = strings.Repeat("0", 40)
			return c
		}(), packet: packetConfig, requested: 8, effective: 6},
		{name: "feature_schema", config: func() brainBriefAdmissionShadowConfig { c := config; c.FeatureSchema += "changed:bool\n"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "feature_semantics", config: func() brainBriefAdmissionShadowConfig { c := config; c.FeatureSemantics += ";changed"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "reason_schema", config: func() brainBriefAdmissionShadowConfig { c := config; c.ReasonSchema += "changed:always\n"; return c }(), packet: packetConfig, requested: 8, effective: 6},
		{name: "packet_config", config: config, packet: packetConfig + "-changed", requested: 8, effective: 6},
		{name: "requested_limit", config: config, packet: packetConfig, requested: 7, effective: 6},
		{name: "effective_limit", config: config, packet: packetConfig, requested: 8, effective: 5},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			got := brainBriefAdmissionShadowConfigIdentity(mutation.config, mutation.packet, mutation.requested, mutation.effective)
			if got == base {
				t.Fatalf("mutation %s did not change identity", mutation.name)
			}
		})
	}
}

func TestBrainBriefAdmissionShadowArtifactHashBindsDecisionFeaturesAndReasons(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	if err := validateBrainBriefAdmissionShadowArtifact(artifact); err != nil {
		t.Fatalf("validate artifact: %v", err)
	}
	mutations := []func(*brainBriefAdmissionShadowArtifact){
		func(value *brainBriefAdmissionShadowArtifact) { value.Decision = "silence" },
		func(value *brainBriefAdmissionShadowArtifact) { value.Features.Facts++ },
		func(value *brainBriefAdmissionShadowArtifact) {
			value.ReasonCodes = append(value.ReasonCodes, "forged")
		},
	}
	for i, mutate := range mutations {
		changed := artifact
		changed.ReasonCodes = append([]string(nil), artifact.ReasonCodes...)
		mutate(&changed)
		if err := validateBrainBriefAdmissionShadowArtifact(changed); err == nil {
			t.Errorf("tamper mutation %d passed validation", i)
		}
	}

	resealedConfigMutations := []struct {
		name   string
		mutate func(*brainBriefAdmissionShadowArtifact)
	}{
		{name: "schema_version", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.SchemaVersion++ }},
		{name: "evaluator_version", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.EvaluatorVersion += "-forged" }},
		{name: "rule", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.Rule += ";candidate_decision=silence" }},
		{name: "thresholds", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.Thresholds += ";threshold=1" }},
		{name: "base_product_commit", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.BaseProductCommit = strings.Repeat("0", 40) }},
		{name: "config_identity", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.ConfigSHA256 = "sha256:forged" }},
		{name: "feature_schema_identity", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.FeatureSchemaSHA256 = "sha256:forged" }},
		{name: "feature_semantics_identity", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.FeatureSemanticsSHA256 = "sha256:forged" }},
		{name: "reason_schema_identity", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.ReasonSchemaSHA256 = "sha256:forged" }},
		{name: "packet_config_identity", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.PacketConfigSHA256 = "sha256:forged" }},
		{name: "requested_limit", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.RequestedLimit-- }},
		{name: "effective_limit", mutate: func(value *brainBriefAdmissionShadowArtifact) { value.EffectiveFactLimit-- }},
	}
	for _, mutation := range resealedConfigMutations {
		t.Run("resealed_"+mutation.name, func(t *testing.T) {
			changed := artifact
			changed.ReasonCodes = append([]string(nil), artifact.ReasonCodes...)
			mutation.mutate(&changed)
			sealBrainBriefAdmissionShadowArtifact(&changed)
			if err := validateBrainBriefAdmissionShadowArtifact(changed); err == nil {
				t.Fatal("resealed configuration tamper passed validation")
			}
		})
	}
}

func TestBrainBriefAdmissionShadowEvaluatorErrorsDoNotBlockOrChangeDelivery(t *testing.T) {
	report := comprehensiveCompactV1Report()
	always, err := prepareBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("prepare always: %v", err)
	}
	mutations := []struct {
		name   string
		mutate func(*brainBriefAdmissionShadowConfig)
	}{
		{name: "rule", mutate: func(config *brainBriefAdmissionShadowConfig) { config.Rule += ";candidate_decision=silence" }},
		{name: "feature_schema", mutate: func(config *brainBriefAdmissionShadowConfig) { config.FeatureSchema += "forged:bool\n" }},
		{name: "feature_semantics", mutate: func(config *brainBriefAdmissionShadowConfig) { config.FeatureSemantics += ";forged=true" }},
		{name: "reason_schema", mutate: func(config *brainBriefAdmissionShadowConfig) { config.ReasonSchema += "forged:always\n" }},
	}
	wantReasons := []string{"diagnostic_rule_unfrozen", "evaluation_error", "fail_safe_serve"}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			invalid := defaultBrainBriefAdmissionShadowConfig()
			mutation.mutate(&invalid)
			shadow, err := prepareBrainBriefAgentV1WithShadowConfig(report, brainBriefDeliveryShadow, 8, invalid)
			if err != nil {
				t.Fatalf("shadow evaluator error blocked delivery: %v", err)
			}
			if shadow.projection.packet != always.projection.packet {
				t.Fatal("shadow evaluator error changed delivered packet")
			}
			if shadow.shadow == nil {
				t.Fatal("missing fail-safe artifact")
			}
			assertBrainBriefAdmissionServe(t, *shadow.shadow, brainBriefAdmissionShadowStatusUnknown)
			if !reflect.DeepEqual(shadow.shadow.ReasonCodes, wantReasons) {
				t.Fatalf("fail-safe reasons = %v, want %v", shadow.shadow.ReasonCodes, wantReasons)
			}
		})
	}
}

func TestBrainBriefAdmissionShadowInvalidLimitsAndPacketIdentityFailSafe(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	config := defaultBrainBriefAdmissionShadowConfig()
	tests := []struct {
		name      string
		requested int
		effective int
		packet    string
		wantLimit int
	}{
		{name: "invalid_requested_limit", requested: 0, effective: 1, packet: "sha256:forged", wantLimit: brainBriefDefaultLimit},
		{name: "invalid_effective_limit", requested: 8, effective: 5, packet: brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 8, 5), wantLimit: 8},
		{name: "invalid_packet_identity", requested: 8, effective: 6, packet: "sha256:forged", wantLimit: 8},
	}
	wantReasons := []string{"diagnostic_rule_unfrozen", "evaluation_error", "fail_safe_serve"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := evaluateBrainBriefAdmissionShadowSafe(
				report, tt.requested, tt.effective, tt.packet, config,
			)
			assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
			if artifact.RequestedLimit != tt.wantLimit || artifact.EffectiveFactLimit != brainBriefFactsCount(tt.wantLimit) {
				t.Fatalf("fail-safe limits = %d/%d, want %d/%d", artifact.RequestedLimit, artifact.EffectiveFactLimit, tt.wantLimit, brainBriefFactsCount(tt.wantLimit))
			}
			if !reflect.DeepEqual(artifact.ReasonCodes, wantReasons) {
				t.Fatalf("fail-safe reasons = %v, want %v", artifact.ReasonCodes, wantReasons)
			}
		})
	}
}

func TestBrainBriefAdmissionShadowUnclassifiedRiskFailsSafe(t *testing.T) {
	report := emptyBrainBriefAdmissionReport()
	report.Warnings = []string{"new warning without a classified contract"}
	artifact := evaluateBrainBriefAdmissionShadow(report, 8)
	assertBrainBriefAdmissionServe(t, artifact, brainBriefAdmissionShadowStatusUnknown)
	if artifact.Features.UnclassifiedWarningCount != 1 || !brainBriefAdmissionHasReason(artifact, "unclassified_high_risk") {
		t.Fatalf("unclassified-risk artifact = %+v", artifact)
	}
}

func BenchmarkBrainBriefAdmissionShadowEvaluate(b *testing.B) {
	report := comprehensiveCompactV1Report()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		shadowBenchmarkArtifact = evaluateBrainBriefAdmissionShadow(report, 8)
	}
}

var shadowBenchmarkArtifact brainBriefAdmissionShadowArtifact

func emptyBrainBriefAdmissionReport() brainBriefReport {
	return brainBriefReport{
		Task:               "admission shadow fixture",
		Facts:              []factRecord{},
		FactsLocusDrift:    map[string][]string{},
		FactsPendingReview: map[string]factReviewNotice{},
		History:            brainBriefHistory{Matches: []brainTextMatch{}},
		Semantic: brainBriefSemantic{
			Context: semanticContextResult{Symbols: []semanticRecord{}, Relations: []semanticRecord{}, Neighbors: []semanticRecord{}},
			Tests:   semanticTestsResult{Roots: []semanticRecord{}, Suggestions: []semanticTestSuggestion{}},
		},
		ActionChecklist: []brainBriefAction{},
		LikelyEditFiles: []string{},
		LikelyTestFiles: []string{},
		LikelyFiles:     []string{},
		Warnings:        []string{},
		admissionSignals: brainBriefAdmissionSourceSignals{
			FactsAvailable:           true,
			HistoryAvailable:         true,
			SemanticContextAvailable: true,
			SemanticTestsAvailable:   true,
		},
	}
}

func assertBrainBriefAdmissionServe(t testing.TB, artifact brainBriefAdmissionShadowArtifact, status string) {
	t.Helper()
	if artifact.Status != status || artifact.Decision != brainBriefAdmissionShadowDecisionServe ||
		artifact.CandidateDecision != brainBriefAdmissionShadowCandidateUnknown || artifact.WouldSilenceAuthorized {
		t.Fatalf("artifact is not %s/serve without silence authorization: %+v", status, artifact)
	}
	if err := validateBrainBriefAdmissionShadowArtifact(artifact); err != nil {
		t.Fatalf("invalid shadow artifact: %v", err)
	}
}

func brainBriefAdmissionHasReason(artifact brainBriefAdmissionShadowArtifact, reason string) bool {
	for _, candidate := range artifact.ReasonCodes {
		if candidate == reason {
			return true
		}
	}
	return false
}
