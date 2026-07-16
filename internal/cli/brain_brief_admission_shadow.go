package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const (
	brainBriefAdmissionShadowSchemaVersion = 1
	brainBriefAdmissionShadowVersion       = "agent_v1_admission_shadow_v1"
	// This is the product commit whose packet contract the shadow evaluator is
	// observing. A commit cannot bind its own hash, so the evaluator binds the
	// already-landed agent_v1 product base explicitly.
	brainBriefAdmissionShadowBaseProductCommit = "856bcff33349c7c76f36b1eb60a23cf8ebec318b"
	brainBriefAdmissionShadowRule              = "actual_delivery=serve;candidate_decision=unknown;" +
		"would_silence_authorized=false;high_risk=unknown_serve"
	brainBriefAdmissionShadowThresholds = "none;development_admission_thresholds=unfrozen"

	brainBriefAdmissionShadowStatusDiagnostic = "diagnostic_unfrozen"
	brainBriefAdmissionShadowStatusUnknown    = "unknown"
	brainBriefAdmissionShadowDecisionServe    = "serve"
	brainBriefAdmissionShadowCandidateUnknown = "unknown"

	brainBriefAdmissionSignalUnavailable = "unavailable"
	brainBriefAdmissionSignalPartial     = "partial"
	brainBriefAdmissionSignalEmpty       = "empty"
	brainBriefAdmissionSignalPresent     = "present"

	// Field order and types are part of the evaluator identity. The artifact is
	// deliberately count/state-only: it must not retain task text, record IDs,
	// paths, excerpts, host values, or timestamps.
	brainBriefAdmissionShadowFeatureSchema = "facts_state:string\n" +
		"facts:int\n" +
		"fact_reviews:int\n" +
		"facts_with_locus_drift:int\n" +
		"history_state:string\n" +
		"history_matches:int\n" +
		"semantic_state:string\n" +
		"semantic_symbols:int\n" +
		"test_suggestions:int\n" +
		"actions:int\n" +
		"likely_edit_files:int\n" +
		"likely_test_files:int\n" +
		"evidence_source_count:int\n" +
		"proposal_state_unavailable:bool\n" +
		"trust_signal_count:int\n" +
		"unclassified_warning_count:int\n" +
		"unsafe_structured_value_count:int\n" +
		"non_finite_confidence_count:int\n"
	brainBriefAdmissionShadowFeatureSemantics = "counts=selected_report_pre_serialization;" +
		"source_presence=facts,history,semantic,synthesis;" +
		"availability=explicit_stage_success;semantic_availability=context_and_tests;" +
		"trust=selected_fact_review+unfiltered_locus_drift+proposal_unavailable;" +
		"unsafe=agent_v1_structured_candidate_rejection_count;" +
		"nonfinite=selected_review_and_context_symbol_confidence;" +
		"warnings=status+live+report_nonproposal+semantic_coverage"
	brainBriefAdmissionShadowReasonSchema = "diagnostic_rule_unfrozen:always\n" +
		"evidence_empty:evidence_source_count=0\n" +
		"facts_present:facts>0\n" +
		"history_present:history_matches>0\n" +
		"semantic_present:semantic_symbols+test_suggestions>0\n" +
		"synthesis_present:actions+likely_edit_files+likely_test_files>0\n" +
		"facts_unavailable:facts_state=unavailable\n" +
		"history_unavailable:history_state=unavailable\n" +
		"semantic_unavailable:semantic_state=unavailable|partial\n" +
		"pending_fact_review:fact_reviews>0\n" +
		"fact_locus_drift:facts_with_locus_drift>0\n" +
		"proposal_state_unavailable:proposal_state_unavailable=true\n" +
		"unsafe_structured_input:unsafe_structured_value_count>0\n" +
		"non_finite_signal:non_finite_confidence_count>0\n" +
		"unclassified_high_risk:unclassified_warning_count>0\n" +
		"evaluation_error:evaluator_error|panic\n" +
		"packet_build_error:always_bound_packet_build_error\n" +
		"fail_safe_serve:any_high_risk|evaluation_error|packet_build_error\n"
)

type brainBriefAdmissionShadowConfig struct {
	SchemaVersion     int
	EvaluatorVersion  string
	Rule              string
	Thresholds        string
	BaseProductCommit string
	FeatureSchema     string
	FeatureSemantics  string
	ReasonSchema      string
}

func defaultBrainBriefAdmissionShadowConfig() brainBriefAdmissionShadowConfig {
	return brainBriefAdmissionShadowConfig{
		SchemaVersion:     brainBriefAdmissionShadowSchemaVersion,
		EvaluatorVersion:  brainBriefAdmissionShadowVersion,
		Rule:              brainBriefAdmissionShadowRule,
		Thresholds:        brainBriefAdmissionShadowThresholds,
		BaseProductCommit: brainBriefAdmissionShadowBaseProductCommit,
		FeatureSchema:     brainBriefAdmissionShadowFeatureSchema,
		FeatureSemantics:  brainBriefAdmissionShadowFeatureSemantics,
		ReasonSchema:      brainBriefAdmissionShadowReasonSchema,
	}
}

type brainBriefAdmissionSourceSignals struct {
	FactsAvailable           bool
	HistoryAvailable         bool
	SemanticContextAvailable bool
	SemanticTestsAvailable   bool
}

type brainBriefAdmissionShadowFeatures struct {
	FactsState                 string `json:"facts_state"`
	Facts                      int    `json:"facts"`
	FactReviews                int    `json:"fact_reviews"`
	FactsWithLocusDrift        int    `json:"facts_with_locus_drift"`
	HistoryState               string `json:"history_state"`
	HistoryMatches             int    `json:"history_matches"`
	SemanticState              string `json:"semantic_state"`
	SemanticSymbols            int    `json:"semantic_symbols"`
	TestSuggestions            int    `json:"test_suggestions"`
	Actions                    int    `json:"actions"`
	LikelyEditFiles            int    `json:"likely_edit_files"`
	LikelyTestFiles            int    `json:"likely_test_files"`
	EvidenceSourceCount        int    `json:"evidence_source_count"`
	ProposalStateUnavailable   bool   `json:"proposal_state_unavailable"`
	TrustSignalCount           int    `json:"trust_signal_count"`
	UnclassifiedWarningCount   int    `json:"unclassified_warning_count"`
	UnsafeStructuredValueCount int    `json:"unsafe_structured_value_count"`
	NonFiniteConfidenceCount   int    `json:"non_finite_confidence_count"`
}

type brainBriefAdmissionShadowArtifact struct {
	SchemaVersion          int                               `json:"schema_version"`
	EvaluatorVersion       string                            `json:"evaluator_version"`
	ConfigSHA256           string                            `json:"config_sha256"`
	ArtifactSHA256         string                            `json:"artifact_sha256"`
	Status                 string                            `json:"status"`
	Decision               string                            `json:"decision"`
	CandidateDecision      string                            `json:"candidate_decision"`
	WouldSilenceAuthorized bool                              `json:"would_silence_authorized"`
	Rule                   string                            `json:"rule"`
	Thresholds             string                            `json:"thresholds"`
	BaseProductCommit      string                            `json:"base_product_commit"`
	FeatureSchemaSHA256    string                            `json:"feature_schema_sha256"`
	FeatureSemanticsSHA256 string                            `json:"feature_semantics_sha256"`
	ReasonSchemaSHA256     string                            `json:"reason_schema_sha256"`
	PacketConfigSHA256     string                            `json:"packet_config_sha256"`
	RequestedLimit         int                               `json:"requested_limit"`
	EffectiveFactLimit     int                               `json:"effective_fact_limit"`
	Features               brainBriefAdmissionShadowFeatures `json:"features"`
	ReasonCodes            []string                          `json:"reason_codes"`
}

func brainBriefAdmissionShadowConfigIdentity(config brainBriefAdmissionShadowConfig, packetConfig string, requestedLimit, effectiveFactLimit int) string {
	canonical := fmt.Sprintf(
		"schema=%d\nevaluator_version=%s\nstatus=%s\nrule=%s\nthresholds=%s\nbase_product_commit=%s\npacket_config=%s\nrequested_limit=%d\neffective_fact_limit=%d\nfeature_schema_bytes=%d\n%sfeature_semantics_bytes=%d\n%s\nreason_schema_bytes=%d\n%s",
		config.SchemaVersion,
		config.EvaluatorVersion,
		brainBriefAdmissionShadowStatusDiagnostic,
		config.Rule,
		config.Thresholds,
		config.BaseProductCommit,
		packetConfig,
		requestedLimit,
		effectiveFactLimit,
		len(config.FeatureSchema),
		config.FeatureSchema,
		len(config.FeatureSemantics),
		config.FeatureSemantics,
		len(config.ReasonSchema),
		config.ReasonSchema,
	)
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("sha256:%x", sum)
}

func brainBriefAdmissionShadowFeatureSchemaIdentity(schema string) string {
	sum := sha256.Sum256([]byte(schema))
	return fmt.Sprintf("sha256:%x", sum)
}

func newBrainBriefAdmissionShadowArtifact(config brainBriefAdmissionShadowConfig, packetConfig string, requestedLimit, effectiveFactLimit int) brainBriefAdmissionShadowArtifact {
	return brainBriefAdmissionShadowArtifact{
		SchemaVersion:          config.SchemaVersion,
		EvaluatorVersion:       config.EvaluatorVersion,
		ConfigSHA256:           brainBriefAdmissionShadowConfigIdentity(config, packetConfig, requestedLimit, effectiveFactLimit),
		Status:                 brainBriefAdmissionShadowStatusDiagnostic,
		Decision:               brainBriefAdmissionShadowDecisionServe,
		CandidateDecision:      brainBriefAdmissionShadowCandidateUnknown,
		WouldSilenceAuthorized: false,
		Rule:                   config.Rule,
		Thresholds:             config.Thresholds,
		BaseProductCommit:      config.BaseProductCommit,
		FeatureSchemaSHA256:    brainBriefAdmissionShadowFeatureSchemaIdentity(config.FeatureSchema),
		FeatureSemanticsSHA256: brainBriefAdmissionShadowFeatureSchemaIdentity(config.FeatureSemantics),
		ReasonSchemaSHA256:     brainBriefAdmissionShadowFeatureSchemaIdentity(config.ReasonSchema),
		PacketConfigSHA256:     packetConfig,
		RequestedLimit:         requestedLimit,
		EffectiveFactLimit:     effectiveFactLimit,
		ReasonCodes:            []string{"diagnostic_rule_unfrozen"},
	}
}

func evaluateBrainBriefAdmissionShadow(report brainBriefReport, requestedLimit int) brainBriefAdmissionShadowArtifact {
	config := defaultBrainBriefAdmissionShadowConfig()
	effectiveFactLimit := brainBriefFactsCount(requestedLimit)
	packetConfig := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, requestedLimit, effectiveFactLimit)
	return evaluateBrainBriefAdmissionShadowSafe(report, requestedLimit, effectiveFactLimit, packetConfig, config)
}

// evaluateBrainBriefAdmissionShadowSafe is deliberately unable to block
// delivery. Invalid configuration, evaluator errors, and panics all collapse to
// a sealed unknown/serve artifact with no silence authorization.
func evaluateBrainBriefAdmissionShadowSafe(
	report brainBriefReport,
	requestedLimit int,
	effectiveFactLimit int,
	packetConfig string,
	config brainBriefAdmissionShadowConfig,
) (artifact brainBriefAdmissionShadowArtifact) {
	compiledConfig := defaultBrainBriefAdmissionShadowConfig()
	fallbackRequestedLimit := requestedLimit
	if fallbackRequestedLimit <= 0 {
		fallbackRequestedLimit = brainBriefDefaultLimit
	}
	fallbackEffectiveFactLimit := brainBriefFactsCount(fallbackRequestedLimit)
	failSafe := func() brainBriefAdmissionShadowArtifact {
		expectedPacketConfig := brainBriefAgentV1ConfigIdentity(
			brainBriefDeliveryAlways, fallbackRequestedLimit, fallbackEffectiveFactLimit,
		)
		failed := newBrainBriefAdmissionShadowArtifact(
			compiledConfig, expectedPacketConfig, fallbackRequestedLimit, fallbackEffectiveFactLimit,
		)
		failed.Features.FactsState = brainBriefAdmissionSignalUnavailable
		failed.Features.HistoryState = brainBriefAdmissionSignalUnavailable
		failed.Features.SemanticState = brainBriefAdmissionSignalUnavailable
		failed.Status = brainBriefAdmissionShadowStatusUnknown
		failed.ReasonCodes = []string{"diagnostic_rule_unfrozen", "evaluation_error", "fail_safe_serve"}
		sealBrainBriefAdmissionShadowArtifact(&failed)
		return failed
	}
	defer func() {
		if recover() != nil {
			artifact = failSafe()
		}
	}()

	evaluated, err := evaluateBrainBriefAdmissionShadowCore(
		report, requestedLimit, effectiveFactLimit, packetConfig, config,
	)
	if err != nil {
		return failSafe()
	}
	return evaluated
}

func evaluateBrainBriefAdmissionShadowCore(
	report brainBriefReport,
	requestedLimit int,
	effectiveFactLimit int,
	packetConfig string,
	config brainBriefAdmissionShadowConfig,
) (brainBriefAdmissionShadowArtifact, error) {
	if config != defaultBrainBriefAdmissionShadowConfig() {
		return brainBriefAdmissionShadowArtifact{}, fmt.Errorf("invalid admission shadow configuration")
	}
	if requestedLimit <= 0 || effectiveFactLimit != brainBriefFactsCount(requestedLimit) {
		return brainBriefAdmissionShadowArtifact{}, fmt.Errorf("invalid admission shadow limits")
	}
	expectedPacketConfig := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, requestedLimit, effectiveFactLimit)
	if packetConfig != expectedPacketConfig {
		return brainBriefAdmissionShadowArtifact{}, fmt.Errorf("admission shadow packet config mismatch")
	}

	artifact := newBrainBriefAdmissionShadowArtifact(config, packetConfig, requestedLimit, effectiveFactLimit)
	features := brainBriefAdmissionShadowFeaturesFor(report)
	artifact.Features = features
	highRisk := false

	if features.EvidenceSourceCount == 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "evidence_empty")
	}
	if features.Facts > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "facts_present")
	}
	if features.HistoryMatches > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "history_present")
	}
	if features.SemanticSymbols+features.TestSuggestions > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "semantic_present")
	}
	if features.Actions+features.LikelyEditFiles+features.LikelyTestFiles > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "synthesis_present")
	}
	if features.FactsState == brainBriefAdmissionSignalUnavailable {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "facts_unavailable")
		highRisk = true
	}
	if features.HistoryState == brainBriefAdmissionSignalUnavailable {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "history_unavailable")
		highRisk = true
	}
	if features.SemanticState == brainBriefAdmissionSignalUnavailable || features.SemanticState == brainBriefAdmissionSignalPartial {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "semantic_unavailable")
		highRisk = true
	}
	if features.FactReviews > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "pending_fact_review")
		highRisk = true
	}
	if features.FactsWithLocusDrift > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "fact_locus_drift")
		highRisk = true
	}
	if features.ProposalStateUnavailable {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "proposal_state_unavailable")
		highRisk = true
	}
	if features.UnsafeStructuredValueCount > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "unsafe_structured_input")
		highRisk = true
	}
	if features.NonFiniteConfidenceCount > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "non_finite_signal")
		highRisk = true
	}
	if features.UnclassifiedWarningCount > 0 {
		artifact.ReasonCodes = append(artifact.ReasonCodes, "unclassified_high_risk")
		highRisk = true
	}
	if highRisk {
		artifact.Status = brainBriefAdmissionShadowStatusUnknown
		artifact.ReasonCodes = append(artifact.ReasonCodes, "fail_safe_serve")
	}
	sealBrainBriefAdmissionShadowArtifact(&artifact)
	return artifact, nil
}

func brainBriefAdmissionShadowFeaturesFor(report brainBriefReport) brainBriefAdmissionShadowFeatures {
	factReviews := 0
	factsWithLocusDrift := 0
	nonFinite := 0
	for _, fact := range report.Facts {
		if len(report.FactsLocusDrift[fact.ID]) > 0 {
			factsWithLocusDrift++
		}
		if notice, ok := report.FactsPendingReview[fact.ID]; ok {
			factReviews++
			if math.IsNaN(notice.Confidence) || math.IsInf(notice.Confidence, 0) {
				nonFinite++
			}
		}
	}
	for _, symbol := range report.Semantic.Context.Symbols {
		if math.IsNaN(symbol.Confidence) || math.IsInf(symbol.Confidence, 0) {
			nonFinite++
		}
	}

	proposalUnavailable := false
	unclassifiedWarnings := len(report.Status.Warnings) + len(report.Status.Live.Warnings)
	for _, warning := range report.Warnings {
		switch {
		case warning == factReviewQueueUnavailableWarning:
			proposalUnavailable = true
		case strings.TrimSpace(warning) != "":
			unclassifiedWarnings++
		}
	}
	if report.Status.Semantic != nil && report.Status.Semantic.Coverage != nil {
		unclassifiedWarnings += report.Status.Semantic.Coverage.Warnings
		unclassifiedWarnings += report.Status.Semantic.Coverage.PartialFailures
	}

	features := brainBriefAdmissionShadowFeatures{
		FactsState:                 brainBriefAdmissionSourceState(report.admissionSignals.FactsAvailable, len(report.Facts)),
		Facts:                      len(report.Facts),
		FactReviews:                factReviews,
		FactsWithLocusDrift:        factsWithLocusDrift,
		HistoryState:               brainBriefAdmissionSourceState(report.admissionSignals.HistoryAvailable, len(report.History.Matches)),
		HistoryMatches:             len(report.History.Matches),
		SemanticState:              brainBriefAdmissionSemanticState(report),
		SemanticSymbols:            len(report.Semantic.Context.Symbols),
		TestSuggestions:            len(report.Semantic.Tests.Suggestions),
		Actions:                    len(report.ActionChecklist),
		LikelyEditFiles:            len(report.LikelyEditFiles),
		LikelyTestFiles:            len(report.LikelyTestFiles),
		ProposalStateUnavailable:   proposalUnavailable,
		UnclassifiedWarningCount:   unclassifiedWarnings,
		UnsafeStructuredValueCount: brainBriefAdmissionUnsafeStructuredValueCount(report),
		NonFiniteConfidenceCount:   nonFinite,
	}
	if features.Facts > 0 {
		features.EvidenceSourceCount++
	}
	if features.HistoryMatches > 0 {
		features.EvidenceSourceCount++
	}
	if features.SemanticSymbols+features.TestSuggestions > 0 {
		features.EvidenceSourceCount++
	}
	if features.Actions+features.LikelyEditFiles+features.LikelyTestFiles > 0 {
		features.EvidenceSourceCount++
	}
	features.TrustSignalCount = factReviews + factsWithLocusDrift
	if proposalUnavailable {
		features.TrustSignalCount++
	}
	return features
}

func brainBriefAdmissionSourceState(available bool, count int) string {
	if !available {
		return brainBriefAdmissionSignalUnavailable
	}
	if count == 0 {
		return brainBriefAdmissionSignalEmpty
	}
	return brainBriefAdmissionSignalPresent
}

func brainBriefAdmissionSemanticState(report brainBriefReport) string {
	contextAvailable := report.admissionSignals.SemanticContextAvailable
	testsAvailable := report.admissionSignals.SemanticTestsAvailable
	if !contextAvailable && !testsAvailable {
		return brainBriefAdmissionSignalUnavailable
	}
	if !contextAvailable || !testsAvailable {
		return brainBriefAdmissionSignalPartial
	}
	if len(report.Semantic.Context.Symbols) == 0 && len(report.Semantic.Tests.Suggestions) == 0 {
		return brainBriefAdmissionSignalEmpty
	}
	return brainBriefAdmissionSignalPresent
}

func brainBriefAdmissionUnsafeStructuredValueCount(report brainBriefReport) int {
	count := 0
	add := func(value string) {
		if value != "" && !agentV1SafeStructuredValue(value) {
			count++
		}
	}
	addAll := func(values []string) {
		for _, value := range values {
			add(value)
		}
	}
	addAll(report.Status.Live.ChangedFiles)
	addAll(report.LikelyEditFiles)
	addAll(report.LikelyTestFiles)
	addAll(report.LikelyFiles)
	for _, fact := range report.Facts {
		addAll(fact.Paths)
		addAll(fact.Locus)
		addAll(report.FactsLocusDrift[fact.ID])
	}
	for _, action := range report.ActionChecklist {
		add(action.File)
		add(action.Symbol)
	}
	for _, symbol := range report.Semantic.Context.Symbols {
		add(symbol.FilePath)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		add(suggestion.Symbol.FilePath)
	}
	return count
}

func sealBrainBriefAdmissionShadowArtifact(artifact *brainBriefAdmissionShadowArtifact) {
	artifact.ArtifactSHA256 = ""
	data, _ := json.Marshal(*artifact)
	sum := sha256.Sum256(data)
	artifact.ArtifactSHA256 = fmt.Sprintf("sha256:%x", sum)
}

func markBrainBriefAdmissionShadowPacketBuildError(artifact *brainBriefAdmissionShadowArtifact) {
	artifact.Status = brainBriefAdmissionShadowStatusUnknown
	if last := len(artifact.ReasonCodes) - 1; last >= 0 && artifact.ReasonCodes[last] == "fail_safe_serve" {
		artifact.ReasonCodes = artifact.ReasonCodes[:last]
	}
	artifact.ReasonCodes = append(artifact.ReasonCodes, "packet_build_error", "fail_safe_serve")
	sealBrainBriefAdmissionShadowArtifact(artifact)
}

func validateBrainBriefAdmissionShadowArtifact(artifact brainBriefAdmissionShadowArtifact) error {
	want := artifact.ArtifactSHA256
	copy := artifact
	sealBrainBriefAdmissionShadowArtifact(&copy)
	if want == "" || copy.ArtifactSHA256 != want {
		return fmt.Errorf("admission shadow artifact hash mismatch")
	}
	config := defaultBrainBriefAdmissionShadowConfig()
	if artifact.RequestedLimit <= 0 || artifact.EffectiveFactLimit != brainBriefFactsCount(artifact.RequestedLimit) {
		return fmt.Errorf("admission shadow artifact limit mismatch")
	}
	expectedPacketConfig := brainBriefAgentV1ConfigIdentity(
		brainBriefDeliveryAlways, artifact.RequestedLimit, artifact.EffectiveFactLimit,
	)
	if artifact.PacketConfigSHA256 != expectedPacketConfig {
		return fmt.Errorf("admission shadow artifact packet config mismatch")
	}
	if artifact.SchemaVersion != config.SchemaVersion ||
		artifact.EvaluatorVersion != config.EvaluatorVersion ||
		artifact.Rule != config.Rule ||
		artifact.Thresholds != config.Thresholds ||
		artifact.BaseProductCommit != config.BaseProductCommit ||
		artifact.FeatureSchemaSHA256 != brainBriefAdmissionShadowFeatureSchemaIdentity(config.FeatureSchema) ||
		artifact.FeatureSemanticsSHA256 != brainBriefAdmissionShadowFeatureSchemaIdentity(config.FeatureSemantics) ||
		artifact.ReasonSchemaSHA256 != brainBriefAdmissionShadowFeatureSchemaIdentity(config.ReasonSchema) ||
		artifact.ConfigSHA256 != brainBriefAdmissionShadowConfigIdentity(
			config, expectedPacketConfig, artifact.RequestedLimit, artifact.EffectiveFactLimit,
		) {
		return fmt.Errorf("admission shadow artifact configuration mismatch")
	}
	if artifact.Decision != brainBriefAdmissionShadowDecisionServe ||
		artifact.CandidateDecision != brainBriefAdmissionShadowCandidateUnknown ||
		artifact.WouldSilenceAuthorized ||
		(artifact.Status != brainBriefAdmissionShadowStatusDiagnostic && artifact.Status != brainBriefAdmissionShadowStatusUnknown) {
		return fmt.Errorf("admission shadow artifact is not fail-safe serve")
	}
	features := artifact.Features
	if !brainBriefAdmissionSignalStateValid(features.FactsState) ||
		!brainBriefAdmissionSignalStateValid(features.HistoryState) ||
		!brainBriefAdmissionSignalStateValid(features.SemanticState) ||
		features.Facts < 0 || features.FactReviews < 0 || features.FactsWithLocusDrift < 0 ||
		features.HistoryMatches < 0 || features.SemanticSymbols < 0 || features.TestSuggestions < 0 ||
		features.Actions < 0 || features.LikelyEditFiles < 0 || features.LikelyTestFiles < 0 ||
		features.EvidenceSourceCount < 0 || features.EvidenceSourceCount > 4 ||
		features.TrustSignalCount < 0 || features.UnclassifiedWarningCount < 0 ||
		features.UnsafeStructuredValueCount < 0 || features.NonFiniteConfidenceCount < 0 {
		return fmt.Errorf("admission shadow artifact feature contract mismatch")
	}
	if len(artifact.ReasonCodes) == 0 || artifact.ReasonCodes[0] != "diagnostic_rule_unfrozen" {
		return fmt.Errorf("admission shadow artifact reason contract mismatch")
	}
	lastReason := -1
	for _, reason := range artifact.ReasonCodes {
		ordinal := brainBriefAdmissionShadowReasonOrdinal(reason)
		if ordinal < 0 || ordinal <= lastReason {
			return fmt.Errorf("admission shadow artifact reason contract mismatch")
		}
		lastReason = ordinal
	}
	return nil
}

func brainBriefAdmissionSignalStateValid(state string) bool {
	switch state {
	case brainBriefAdmissionSignalUnavailable, brainBriefAdmissionSignalPartial,
		brainBriefAdmissionSignalEmpty, brainBriefAdmissionSignalPresent:
		return true
	default:
		return false
	}
}

func brainBriefAdmissionShadowReasonOrdinal(reason string) int {
	switch reason {
	case "diagnostic_rule_unfrozen":
		return 0
	case "evidence_empty":
		return 1
	case "facts_present":
		return 2
	case "history_present":
		return 3
	case "semantic_present":
		return 4
	case "synthesis_present":
		return 5
	case "facts_unavailable":
		return 6
	case "history_unavailable":
		return 7
	case "semantic_unavailable":
		return 8
	case "pending_fact_review":
		return 9
	case "fact_locus_drift":
		return 10
	case "proposal_state_unavailable":
		return 11
	case "unsafe_structured_input":
		return 12
	case "non_finite_signal":
		return 13
	case "unclassified_high_risk":
		return 14
	case "evaluation_error":
		return 15
	case "packet_build_error":
		return 16
	case "fail_safe_serve":
		return 17
	default:
		return -1
	}
}
