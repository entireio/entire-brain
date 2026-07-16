package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	brainBriefPacketMeasurementLimit         = 8
	brainBriefPacketMeasurementLexemeVersion = "unicode_word_or_scalar_v1"
	brainBriefPacketMeasurementFixtureSHA256 = "sha256:435cb828133326d063286fc673fdd81ca8fbf683c7c9460bcb94cc59941072dc"
)

type brainBriefPacketMeasurementArm struct {
	name   string
	format brainBriefPacketFormat
	policy brainBriefDeliveryPolicy
}

type brainBriefPacketMeasurementMetrics struct {
	SHA256       string
	Bytes        int
	UnicodeRunes int
	Lexemes      int
	LexemeMetric string
}

type brainBriefPacketUtilityDimension struct {
	Name        string
	WeightBP    int
	Numerator   int
	Denominator int
}

type brainBriefPacketUtility struct {
	ScoreBP    int
	Dimensions []brainBriefPacketUtilityDimension
}

type brainBriefPacketUtilityDefinition struct {
	name     string
	weightBP int
	needles  []string
}

func TestBrainBriefPacketMeasurementMatrix(t *testing.T) {
	if got := (brainBriefOptions{}).resolvedPacketFormat(); got != brainBriefPacketText {
		t.Fatalf("CLI default packet format = %q, want text", got)
	}
	if got := (brainBriefOptions{json: true}).resolvedPacketFormat(); got != brainBriefPacketLegacyJSON {
		t.Fatalf("CLI --json packet format = %q, want legacy JSON", got)
	}
	packets := make(map[string]string, len(brainBriefPacketMeasurementArms()))
	metrics := make(map[string]brainBriefPacketMeasurementMetrics, len(brainBriefPacketMeasurementArms()))
	utilities := make(map[string]brainBriefPacketUtility, len(brainBriefPacketMeasurementArms()))
	var shadowArtifact *brainBriefAdmissionShadowArtifact

	wantFixtureFingerprint := brainBriefPacketMeasurementFingerprint(t, newBrainBriefPacketMeasurementReport())
	if wantFixtureFingerprint != brainBriefPacketMeasurementFixtureSHA256 {
		t.Fatalf("fixture fingerprint = %s, want frozen %s", wantFixtureFingerprint, brainBriefPacketMeasurementFixtureSHA256)
	}
	for _, arm := range brainBriefPacketMeasurementArms() {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			report := newBrainBriefPacketMeasurementReport()
			before := brainBriefPacketMeasurementFingerprint(t, report)
			if before != wantFixtureFingerprint {
				t.Fatalf("fresh fixture fingerprint = %s, want %s", before, wantFixtureFingerprint)
			}

			packet, artifact := renderBrainBriefPacketMeasurement(t, arm, report)
			after := brainBriefPacketMeasurementFingerprint(t, report)
			if after != before {
				t.Fatalf("serializer mutated fixture: before=%s after=%s", before, after)
			}
			if !utf8.ValidString(packet) {
				t.Fatal("packet is not valid UTF-8")
			}

			validateBrainBriefPacketMeasurement(t, arm, report, packet)
			gotMetrics := measureBrainBriefPacket(packet)
			gotUtility := measureBrainBriefPacketUtility(packet)
			packets[arm.name] = packet
			metrics[arm.name] = gotMetrics
			utilities[arm.name] = gotUtility
			if artifact != nil {
				copy := *artifact
				shadowArtifact = &copy
			}
			t.Logf("metrics sha256=%s bytes=%d runes=%d lexemes=%d lexeme_metric=%s utility=%s",
				gotMetrics.SHA256, gotMetrics.Bytes, gotMetrics.UnicodeRunes, gotMetrics.Lexemes,
				gotMetrics.LexemeMetric, formatBrainBriefPacketUtility(gotUtility))
		})
	}

	assertCompactV2ProjectionParity(t, packets["compact_v1"], packets["compact_v2"])
	if len(packets["compact_v2"]) >= len(packets["compact_v1"]) {
		t.Fatalf("compact_v2 bytes = %d, want less than compact_v1 %d", len(packets["compact_v2"]), len(packets["compact_v1"]))
	}
	if len(packets["agent_v1_always"]) >= len(packets["legacy_json"]) {
		t.Fatalf("agent_v1 bytes = %d, want less than legacy JSON %d", len(packets["agent_v1_always"]), len(packets["legacy_json"]))
	}
	if len(packets["agent_v1_always"]) > brainBriefAgentV1BudgetBytes {
		t.Fatalf("agent_v1 bytes = %d, budget = %d", len(packets["agent_v1_always"]), brainBriefAgentV1BudgetBytes)
	}
	if packets["agent_v1_always"] != packets["agent_v1_shadow"] {
		t.Fatal("agent_v1 shadow changed always-bound packet bytes")
	}
	if shadowArtifact == nil {
		t.Fatal("agent_v1 shadow did not produce an admission artifact")
	}
	assertBrainBriefPacketMeasurementShadow(t, *shadowArtifact)
	assertBrainBriefPacketMeasurementPrivacy(t, packets["agent_v1_always"])
	assertBrainBriefPacketMeasurementPrivacy(t, packets["agent_v1_shadow"])

	// These maps deliberately freeze the exact development fixture wire outputs
	// and the transparent utility-rubric result. Update them only after reviewing
	// the packet contract change that caused the delta.
	wantMetrics := map[string]brainBriefPacketMeasurementMetrics{
		"cli_text_default": {
			SHA256:       "sha256:7fc91c5a784e7466f15ff73a5d34afa856ad5353319f326039208ba6f3400729",
			Bytes:        1573,
			UnicodeRunes: 1567,
			Lexemes:      309,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
		"legacy_json": {
			SHA256:       "sha256:68b9661b1f0c1b407d6c4c4ffd956c7dc9e966188c28960dad204925105ef8e8",
			Bytes:        12220,
			UnicodeRunes: 12220,
			Lexemes:      2995,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
		"compact_v1": {
			SHA256:       "sha256:c0f8aafbf6baff14bae1dbdb5217f4be9a7c771f0c1af2399dff944c41d080b8",
			Bytes:        5662,
			UnicodeRunes: 5662,
			Lexemes:      1468,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
		"compact_v2": {
			SHA256:       "sha256:15bdb6d4810cacc9e4b10cd3fc04e87c99a601d31f07dd89497149e707a4c4c0",
			Bytes:        5356,
			UnicodeRunes: 5356,
			Lexemes:      1385,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
		"agent_v1_always": {
			SHA256:       "sha256:dde300548b0fe5c50a1a089faf78cd659c365d4f2f69f1757a30e09b2a1278f3",
			Bytes:        2486,
			UnicodeRunes: 2486,
			Lexemes:      508,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
		"agent_v1_shadow": {
			SHA256:       "sha256:dde300548b0fe5c50a1a089faf78cd659c365d4f2f69f1757a30e09b2a1278f3",
			Bytes:        2486,
			UnicodeRunes: 2486,
			Lexemes:      508,
			LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
		},
	}
	wantUtilities := map[string]string{
		"cli_text_default": "evidence_fidelity=4/10@3500bp;trust_signal_retention=5/7@2500bp;navigation_actionability=8/9@2500bp;task_current_state=1/3@1500bp;packet_utility_score_bp=5907",
		"legacy_json":      "evidence_fidelity=10/10@3500bp;trust_signal_retention=7/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=10000",
		"compact_v1":       "evidence_fidelity=10/10@3500bp;trust_signal_retention=5/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=9285",
		"compact_v2":       "evidence_fidelity=10/10@3500bp;trust_signal_retention=5/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=9285",
		"agent_v1_always":  "evidence_fidelity=8/10@3500bp;trust_signal_retention=4/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=8228",
		"agent_v1_shadow":  "evidence_fidelity=8/10@3500bp;trust_signal_retention=4/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=8228",
	}
	for _, arm := range brainBriefPacketMeasurementArms() {
		wantMetric, ok := wantMetrics[arm.name]
		if !ok {
			t.Errorf("%s has no frozen packet metrics", arm.name)
			continue
		}
		if metrics[arm.name] != wantMetric {
			t.Errorf("%s metrics = %+v, want %+v", arm.name, metrics[arm.name], wantMetric)
		}
		wantUtility, ok := wantUtilities[arm.name]
		if !ok {
			t.Errorf("%s has no frozen packet utility result", arm.name)
			continue
		}
		if got := formatBrainBriefPacketUtility(utilities[arm.name]); got != wantUtility {
			t.Errorf("%s utility = %s, want %s", arm.name, got, wantUtility)
		}
	}
}

func TestBrainBriefPacketMeasurementFixtureFingerprintAndLexemes(t *testing.T) {
	first := newBrainBriefPacketMeasurementReport()
	second := newBrainBriefPacketMeasurementReport()
	want := brainBriefPacketMeasurementFingerprint(t, first)
	if want != brainBriefPacketMeasurementFixtureSHA256 {
		t.Fatalf("fixture fingerprint = %s, want frozen %s", want, brainBriefPacketMeasurementFixtureSHA256)
	}
	if got := brainBriefPacketMeasurementFingerprint(t, second); got != want {
		t.Fatalf("fresh fixture fingerprint = %s, want %s", got, want)
	}

	second.Task += " mutated"
	if got := brainBriefPacketMeasurementFingerprint(t, second); got == want {
		t.Fatal("fixture fingerprint ignored serialized report mutation")
	}
	second = newBrainBriefPacketMeasurementReport()
	second.admissionSignals.HistoryAvailable = false
	if got := brainBriefPacketMeasurementFingerprint(t, second); got == want {
		t.Fatal("fixture fingerprint ignored admission signal mutation")
	}

	const sample = "alpha beta_2!\n界 + e\u0301"
	metrics := measureBrainBriefPacket(sample)
	if metrics.Bytes != len(sample) || metrics.UnicodeRunes != 20 || metrics.Lexemes != 7 ||
		metrics.LexemeMetric != brainBriefPacketMeasurementLexemeVersion {
		t.Fatalf("lexeme metric = %+v", metrics)
	}
}

func BenchmarkBrainBriefPacketFormats(b *testing.B) {
	for _, arm := range brainBriefPacketMeasurementArms() {
		arm := arm
		b.Run(arm.name, func(b *testing.B) {
			report := newBrainBriefPacketMeasurementReport()
			before := brainBriefPacketMeasurementFingerprint(b, report)
			packet, _ := renderBrainBriefPacketMeasurement(b, arm, report)
			metrics := measureBrainBriefPacket(packet)
			brainBriefPacketMeasurementSink = packet

			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := emitBrainBriefPacketWithPolicy(cmd, report, arm.format, arm.policy, brainBriefPacketMeasurementLimit); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(metrics.Bytes), "packet-bytes")
			b.ReportMetric(float64(metrics.UnicodeRunes), "packet-runes")
			b.ReportMetric(float64(metrics.Lexemes), "packet-lexemes-v1")
			if after := brainBriefPacketMeasurementFingerprint(b, report); after != before {
				b.Fatalf("serializer mutated fixture: before=%s after=%s", before, after)
			}
		})
	}
}

var brainBriefPacketMeasurementSink string

func brainBriefPacketMeasurementArms() []brainBriefPacketMeasurementArm {
	return []brainBriefPacketMeasurementArm{
		{name: "cli_text_default", format: brainBriefPacketText, policy: brainBriefDeliveryAlways},
		{name: "legacy_json", format: brainBriefPacketLegacyJSON, policy: brainBriefDeliveryAlways},
		{name: "compact_v1", format: brainBriefPacketCompactV1, policy: brainBriefDeliveryAlways},
		{name: "compact_v2", format: brainBriefPacketCompactV2, policy: brainBriefDeliveryAlways},
		{name: "agent_v1_always", format: brainBriefPacketAgentV1, policy: brainBriefDeliveryAlways},
		{name: "agent_v1_shadow", format: brainBriefPacketAgentV1, policy: brainBriefDeliveryShadow},
	}
}

func newBrainBriefPacketMeasurementReport() brainBriefReport {
	report := comprehensiveCompactV1Report()
	report.Task = "MEASURE_TASK_PACKET_MATRIX repair validation ordering"
	report.Status.Live.Branch = "measure/packet-matrix"
	report.Status.Live.Head = "measure-head-abc123"
	report.Status.Live.Staged = []string{"internal/staged_only.go"}
	report.Status.Live.Unstaged = []string{"internal/unstaged_only.go"}
	report.Status.Live.Untracked = []string{"internal/untracked_only.go"}
	report.Status.Live.ChangedFiles = []string{
		"internal/live_only.go",
		"/synthetic/host/live.go",
		"../synthetic/escape.go",
	}

	report.Semantic.Context.Symbols[0].QualifiedName = "measurepkg.MeasureValidate"
	report.Semantic.Context.Symbols[0].FilePath = "internal/symbol_only.go"
	report.Semantic.Context.Symbols[0].Reason = "MEASURE_SYMBOL_REASON"
	report.Semantic.Context.Relations[0].Resolution = "MEASURE_RELATION_RESOLUTION"
	report.Semantic.RuntimeTraces[0].Reason = "MEASURE_RUNTIME_REASON"
	report.Semantic.Tests.Suggestions[0].Symbol.Name = "MeasureSuggestedTest"
	report.Semantic.Tests.Suggestions[0].Symbol.QualifiedName = "measurepkg.MeasureSuggestedTest"
	report.Semantic.Tests.Suggestions[0].Symbol.FilePath = "internal/suggestion_only_test.go"
	report.Semantic.Tests.Suggestions[0].Reason = "MEASURE_TEST_SUGGESTION_REASON"

	report.History.Matches[0].Excerpt = "MEASURE_HISTORY_EXCERPT preserve validation order"
	report.History.Matches[0].MatchedTerms = []string{"MEASURE_HISTORY_TERM", "order"}
	report.Facts[0].Paths = []string{"architecture.measure.safe", "/synthetic/host/fact-path"}
	report.Facts[0].Locus = []string{"measurepkg.MeasureFactLocus", "../synthetic.FactLocus"}
	report.Facts[0].Text = "MEASURE_FACT_TEXT exact prose /synthetic/host/prose must remain verbatim."
	report.Facts[0].Status = "measure_active"
	report.Facts[0].Confidence = "measure_high"
	report.FactsLocusDrift = map[string][]string{
		report.Facts[0].ID: {"OldValidate", "/synthetic/host/stale.go"},
		"fact:orphan":      {"GoneSymbol"},
	}
	report.FactsPendingReview = map[string]factReviewNotice{
		report.Facts[0].ID: {
			ReviewID:   "measure-review-1",
			Action:     "merge",
			Confidence: 0.75,
			Message:    "MEASURE_REVIEW_MESSAGE verify before trusting",
			RelatedIDs: []string{"fact:related"},
		},
	}

	report.LikelyEditFiles = []string{"internal/edit_only.go", "/synthetic/host/edit.go"}
	report.LikelyTestFiles = []string{"internal/test_only_test.go", "../synthetic/test.go"}
	report.LikelyFiles = []string{
		"internal/edit_only.go",
		"internal/test_only_test.go",
		"docs/likely_only.md",
		"/synthetic/host/likely.md",
	}
	report.ActionChecklist = []brainBriefAction{
		{File: "internal/action_only.go", Symbol: "measurepkg.MeasureAction", Action: "MEASURE_ACTION_INSPECT_ORDER", Evidence: "MEASURE_ACTION_EVIDENCE"},
		{File: "/synthetic/host/action.go", Symbol: "../synthetic.Action", Action: "MEASURE_UNSAFE_ACTION_PROSE /synthetic/host/action-prose", Evidence: "synthetic privacy exercise"},
	}
	report.Warnings = append(report.Warnings, factReviewQueueUnavailableWarning)
	report.admissionSignals = brainBriefAdmissionSourceSignals{
		FactsAvailable:           true,
		HistoryAvailable:         true,
		SemanticContextAvailable: true,
		SemanticTestsAvailable:   true,
	}
	return report
}

func renderBrainBriefPacketMeasurement(tb testing.TB, arm brainBriefPacketMeasurementArm, report brainBriefReport) (string, *brainBriefAdmissionShadowArtifact) {
	tb.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := emitBrainBriefPacketWithPolicy(cmd, report, arm.format, arm.policy, brainBriefPacketMeasurementLimit); err != nil {
		tb.Fatalf("render %s: %v", arm.name, err)
	}
	if arm.policy != brainBriefDeliveryShadow {
		return out.String(), nil
	}
	emission, err := prepareBrainBriefAgentV1(report, brainBriefDeliveryShadow, brainBriefPacketMeasurementLimit)
	if err != nil {
		tb.Fatalf("prepare %s artifact: %v", arm.name, err)
	}
	if emission.shadow == nil {
		tb.Fatalf("prepare %s returned no shadow artifact", arm.name)
	}
	if emission.projection.packet != out.String() {
		tb.Fatalf("prepare %s packet differs from emitted bytes", arm.name)
	}
	return out.String(), emission.shadow
}

func validateBrainBriefPacketMeasurement(t *testing.T, arm brainBriefPacketMeasurementArm, report brainBriefReport, packet string) {
	t.Helper()
	switch arm.format {
	case brainBriefPacketText:
		if !strings.HasSuffix(packet, "\n") || !strings.Contains(packet, "task: "+report.Task+"\n") {
			t.Fatal("default text packet failed its stable surface checks")
		}
	case brainBriefPacketLegacyJSON:
		var decoded brainBriefReport
		if err := json.Unmarshal([]byte(packet), &decoded); err != nil {
			t.Fatalf("decode legacy JSON: %v", err)
		}
		if decoded.Task != report.Task || !reflect.DeepEqual(decoded.Facts, report.Facts) {
			t.Fatal("legacy JSON changed task or fact projection")
		}
	case brainBriefPacketCompactV1:
		parseCompactV1Records(t, packet)
	case brainBriefPacketCompactV2:
		parseCompactV2Records(t, packet)
	case brainBriefPacketAgentV1:
		if err := validateBrainBriefAgentV1Integrity(packet); err != nil {
			t.Fatalf("validate agent_v1: %v", err)
		}
		parseAgentV1Records(t, packet)
	default:
		t.Fatalf("unknown measurement format %q", arm.format)
	}
}

func measureBrainBriefPacket(packet string) brainBriefPacketMeasurementMetrics {
	sum := sha256.Sum256([]byte(packet))
	return brainBriefPacketMeasurementMetrics{
		SHA256:       fmt.Sprintf("sha256:%x", sum),
		Bytes:        len(packet),
		UnicodeRunes: utf8.RuneCountInString(packet),
		Lexemes:      brainBriefPacketLexemeCountV1(packet),
		LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
	}
}

// brainBriefPacketLexemeCountV1 is a tokenizer-independent lexical proxy, not
// a model-token count. It ignores Unicode whitespace, counts each maximal run
// of Unicode letters/digits/underscore once, and every other scalar once.
func brainBriefPacketLexemeCountV1(packet string) int {
	lexemes := 0
	inWord := false
	for _, r := range packet {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			if !inWord {
				lexemes++
			}
			inWord = true
		default:
			lexemes++
			inWord = false
		}
	}
	return lexemes
}

func measureBrainBriefPacketUtility(packet string) brainBriefPacketUtility {
	definitions := []brainBriefPacketUtilityDefinition{
		{
			name:     "evidence_fidelity",
			weightBP: 3500,
			needles: []string{
				"MEASURE_FACT_TEXT exact prose /synthetic/host/prose must remain verbatim.",
				"measure_active",
				"measure_high",
				"MEASURE_HISTORY_EXCERPT preserve validation order",
				"MEASURE_HISTORY_TERM",
				"MEASURE_SYMBOL_REASON",
				"MEASURE_RELATION_RESOLUTION",
				"MEASURE_RUNTIME_REASON",
				"architecture.measure.safe",
				"measurepkg.MeasureFactLocus",
			},
		},
		{
			name:     "trust_signal_retention",
			weightBP: 2500,
			needles: []string{
				"measure-review-1",
				"MEASURE_REVIEW_MESSAGE verify before trusting",
				"OldValidate",
				factReviewQueueUnavailableWarning,
				"status warning",
				"live warning",
				"brief warning",
			},
		},
		{
			name:     "navigation_actionability",
			weightBP: 2500,
			needles: []string{
				"internal/live_only.go",
				"internal/edit_only.go",
				"internal/test_only_test.go",
				"docs/likely_only.md",
				"internal/action_only.go",
				"MEASURE_ACTION_INSPECT_ORDER",
				"measurepkg.MeasureValidate",
				"measurepkg.MeasureSuggestedTest",
				"MEASURE_TEST_SUGGESTION_REASON",
			},
		},
		{
			name:     "task_current_state",
			weightBP: 1500,
			needles: []string{
				"MEASURE_TASK_PACKET_MATRIX repair validation ordering",
				"measure/packet-matrix",
				"measure-head-abc123",
			},
		},
	}

	result := brainBriefPacketUtility{Dimensions: make([]brainBriefPacketUtilityDimension, 0, len(definitions))}
	for _, definition := range definitions {
		numerator := 0
		for _, needle := range definition.needles {
			if strings.Contains(packet, needle) {
				numerator++
			}
		}
		dimension := brainBriefPacketUtilityDimension{
			Name:        definition.name,
			WeightBP:    definition.weightBP,
			Numerator:   numerator,
			Denominator: len(definition.needles),
		}
		result.Dimensions = append(result.Dimensions, dimension)
		result.ScoreBP += definition.weightBP * numerator / len(definition.needles)
	}
	return result
}

func formatBrainBriefPacketUtility(utility brainBriefPacketUtility) string {
	parts := make([]string, 0, len(utility.Dimensions)+1)
	for _, dimension := range utility.Dimensions {
		parts = append(parts, fmt.Sprintf("%s=%d/%d@%dbp", dimension.Name, dimension.Numerator, dimension.Denominator, dimension.WeightBP))
	}
	parts = append(parts, fmt.Sprintf("packet_utility_score_bp=%d", utility.ScoreBP))
	return strings.Join(parts, ";")
}

func brainBriefPacketMeasurementFingerprint(tb testing.TB, report brainBriefReport) string {
	tb.Helper()
	payload := struct {
		Report           brainBriefReport                 `json:"report"`
		AdmissionSignals brainBriefAdmissionSourceSignals `json:"admission_signals"`
	}{
		Report:           report,
		AdmissionSignals: report.admissionSignals,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		tb.Fatalf("fingerprint measurement fixture: %v", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}

func assertBrainBriefPacketMeasurementPrivacy(t *testing.T, packet string) {
	t.Helper()
	for _, unsafe := range []string{
		"/synthetic/host/live.go",
		"../synthetic/escape.go",
		"/synthetic/host/fact-path",
		"../synthetic.FactLocus",
		"/synthetic/host/stale.go",
		"/synthetic/host/edit.go",
		"../synthetic/test.go",
		"/synthetic/host/likely.md",
		"/synthetic/host/action.go",
		"../synthetic.Action",
	} {
		if strings.Contains(packet, unsafe) {
			t.Errorf("agent_v1 leaked unsafe structured value %q", unsafe)
		}
	}
	for _, exactProse := range []string{
		"MEASURE_FACT_TEXT exact prose /synthetic/host/prose must remain verbatim.",
		"MEASURE_UNSAFE_ACTION_PROSE /synthetic/host/action-prose",
	} {
		if !strings.Contains(packet, exactProse) {
			t.Errorf("agent_v1 changed natural-language evidence %q", exactProse)
		}
	}
	if !strings.Contains(packet, "locus_drift=true") || !strings.Contains(packet, `stale_locus=["OldValidate"]`) {
		t.Error("agent_v1 did not retain safe locus-drift trust state")
	}
}

func assertBrainBriefPacketMeasurementShadow(t *testing.T, artifact brainBriefAdmissionShadowArtifact) {
	t.Helper()
	if err := validateBrainBriefAdmissionShadowArtifact(artifact); err != nil {
		t.Fatalf("validate shadow artifact: %v", err)
	}
	if artifact.Status != brainBriefAdmissionShadowStatusUnknown ||
		artifact.Decision != brainBriefAdmissionShadowDecisionServe ||
		artifact.CandidateDecision != brainBriefAdmissionShadowCandidateUnknown ||
		artifact.WouldSilenceAuthorized {
		t.Fatalf("shadow artifact is not fail-safe unknown/serve: %+v", artifact)
	}
	for _, reason := range []string{
		"pending_fact_review",
		"fact_locus_drift",
		"proposal_state_unavailable",
		"unsafe_structured_input",
		"unclassified_high_risk",
		"fail_safe_serve",
	} {
		if !brainBriefAdmissionHasReason(artifact, reason) {
			t.Errorf("shadow artifact missing reason %q: %v", reason, artifact.ReasonCodes)
		}
	}
}
