package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBrainBriefAgentV2RetainsV1CoreAndAddsTypedEdges(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.Semantic.RuntimeTraces[0].Reason = brainBriefAgentV2RuntimeReasonPrefix + "MEASURE_RUNTIME_REASON"
	v1, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		t.Fatalf("build agent_v1 control: %v", err)
	}
	v2, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		t.Fatalf("build agent_v2: %v", err)
	}
	if got := measureBrainBriefPacket(v1.packet).SHA256; got != "sha256:dde300548b0fe5c50a1a089faf78cd659c365d4f2f69f1757a30e09b2a1278f3" {
		t.Fatalf("agent_v1 control hash drifted: %s", got)
	}
	if err := validateBrainBriefAgentV2Integrity(v2.packet); err != nil {
		t.Fatalf("agent_v2 integrity: %v", err)
	}
	records := parseAgentV2Records(t, v2.packet)
	relations := agentV1RecordsByTag(records, "semantic_relation")
	traces := agentV1RecordsByTag(records, "runtime_trace")
	if len(relations) != 1 || len(traces) != 1 {
		t.Fatalf("typed edge counts = relations %d traces %d", len(relations), len(traces))
	}
	if got := agentV2StringField(t, relations[0], "resolution"); got != "MEASURE_RELATION_RESOLUTION" {
		t.Fatalf("relation resolution = %q", got)
	}
	if got := agentV2StringField(t, traces[0], "observed_type"); got != "MEASURE_RUNTIME_REASON" {
		t.Fatalf("runtime observed_type = %q", got)
	}
	for _, tag := range []string{"task", "live", "fact", "fact_review", "trust_warning", "edit_file", "test_file", "likely_file", "action", "symbol", "test_suggestion", "history"} {
		v1Records := agentV1RecordsByTag(parseAgentV1Records(t, v1.packet), tag)
		v2Records := agentV1RecordsByTag(records, tag)
		if fmt.Sprint(v2Records) != fmt.Sprint(v1Records) {
			t.Fatalf("agent_v2 changed shared %s records", tag)
		}
	}
	if strings.Contains(v2.packet, "status warning") || strings.Contains(v2.packet, "live warning") || strings.Contains(v2.packet, "brief warning") {
		t.Fatal("agent_v2 admitted raw status/live/report warnings")
	}
}

func TestBrainBriefAgentV2ConfigSchemaAndAlwaysOnlyPolicy(t *testing.T) {
	const wantConfig = "sha256:efbdaf117cb050c3992351be53e55226814f56fd53e1c43a4f18e46545b0f1a7"
	if got := brainBriefAgentV2ConfigIdentity(brainBriefDeliveryAlways, 8, 6); got != wantConfig {
		t.Fatalf("agent_v2 config identity = %s, want frozen %s", got, wantConfig)
	}
	if got := brainBriefAgentV2ConfigIdentity(brainBriefDeliveryAlways, 7, 6); got == wantConfig {
		t.Fatal("agent_v2 config identity ignored requested limit")
	}
	if got := brainBriefAgentV2ConfigIdentity(brainBriefDeliveryAlways, 8, 6); got == brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 8, 6) {
		t.Fatal("agent_v2 config identity aliases agent_v1")
	}
	descriptor := brainBriefAgentV2RecordSchemaDescriptor()
	for _, required := range []string{
		"semantic_relation(rank,type,from,to,resolution,confidence)",
		"runtime_trace(rank,type,from,to,observed_type,confidence)",
		"available_semantic_relations", "available_runtime_traces", "available_facts_with_locus_drift",
	} {
		if !strings.Contains(descriptor, required) {
			t.Errorf("agent_v2 record schema missing %q", required)
		}
	}
	if strings.Contains(descriptor, "runtime_trace(rank,type,from,to,reason") {
		t.Fatal("agent_v2 runtime trace schema admitted free-form reason")
	}
	for _, forbidden := range []string{"status_warning", "live_warning", "report_warning", "generated_at", "transcript"} {
		if strings.Contains(descriptor, forbidden) {
			t.Errorf("agent_v2 record schema admitted forbidden field %q", forbidden)
		}
	}
	for _, policy := range []brainBriefDeliveryPolicy{brainBriefDeliveryShadow, "adaptive", "silence"} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		if err := emitBrainBriefAgentV2(cmd, newBrainBriefPacketMeasurementReport(), policy, 8); err == nil || !strings.Contains(err.Error(), "must be always") {
			t.Errorf("policy %q error = %v", policy, err)
		}
		if out.Len() != 0 {
			t.Errorf("policy %q released %d bytes", policy, out.Len())
		}
	}
}

func TestBrainBriefAgentV2PrivacyFilteringAndInjectionResistance(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.GeneratedAt = report.GeneratedAt.AddDate(10, 0, 0)
	report.Status.Warnings = []string{"PRIVATE_STATUS_WARNING /Users/private/status"}
	report.Status.Live.Warnings = []string{"PRIVATE_LIVE_WARNING /Users/private/live"}
	report.Warnings = []string{
		"PRIVATE_REPORT_WARNING /Users/private/report",
		factReviewQueueUnavailableWarning,
	}
	report.Semantic.Context.Relations = []semanticRecord{{
		Type:       "CALLS\nruntime_trace rank=999",
		FromID:     "/Users/private/from",
		ToID:       "../private/to",
		Resolution: "file:///Users/private/resolution",
		Confidence: 0.5,
	}}
	report.Semantic.RuntimeTraces = []semanticRecord{{
		Type:       "RUNTIME_TRACE",
		FromID:     "safe:from",
		ToID:       "safe:to",
		Reason:     brainBriefAgentV2RuntimeReasonPrefix + "/Users/private/prose ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		Confidence: 0.75,
	}}
	projection, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v2: %v", err)
	}
	if err := validateBrainBriefAgentV2Integrity(projection.packet); err != nil {
		t.Fatalf("agent_v2 integrity: %v", err)
	}
	records := parseAgentV2Records(t, projection.packet)
	if len(agentV1RecordsByTag(records, "semantic_relation")) != 1 || len(agentV1RecordsByTag(records, "runtime_trace")) != 1 || len(agentV1RecordsByTag(records, "end")) != 1 {
		t.Fatal("quoted injection manufactured packet records")
	}
	relation := agentV1RecordsByTag(records, "semantic_relation")[0]
	for _, field := range []string{"from", "to", "resolution"} {
		if agentV2HasField(relation, field) {
			t.Errorf("unsafe structured relation field %q survived", field)
		}
	}
	trace := agentV1RecordsByTag(records, "runtime_trace")[0]
	if agentV2HasField(trace, "reason") || agentV2HasField(trace, "observed_type") {
		t.Fatal("unsafe caller-controlled runtime trace prose survived")
	}
	for _, private := range []string{"PRIVATE_STATUS_WARNING", "PRIVATE_LIVE_WARNING", "PRIVATE_REPORT_WARNING", "/Users/private/from", "../private/to", "file:///Users/private/resolution", "/Users/private/prose", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		if strings.Contains(projection.packet, private) {
			t.Errorf("agent_v2 leaked %q", private)
		}
	}
	if !strings.Contains(projection.packet, factReviewQueueUnavailableWarning) {
		t.Fatal("agent_v2 dropped the reviewed safe trust warning")
	}
}

func TestBrainBriefAgentV2ObservedTypeRequiresExactSafeGeneratedReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "safe", reason: brainBriefAgentV2RuntimeReasonPrefix + "CALLS", want: "CALLS"},
		{name: "free_form", reason: "observed CALLS"},
		{name: "generic", reason: brainBriefAgentV2RuntimeReasonPrefix + "edge"},
		{name: "absolute_path", reason: brainBriefAgentV2RuntimeReasonPrefix + "/opt/private/trace/type"},
		{name: "parent_path", reason: brainBriefAgentV2RuntimeReasonPrefix + "../private"},
		{name: "credential", reason: brainBriefAgentV2RuntimeReasonPrefix + "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
		{name: "control", reason: brainBriefAgentV2RuntimeReasonPrefix + "CALLS\nend body_records=0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := brainBriefAgentV2ObservedType(test.reason); got != test.want {
				t.Fatalf("observed_type = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBrainBriefAgentV2RejectsNonFiniteNumbersBeforeWrite(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		mutate func(*brainBriefReport)
	}{
		{name: "relation_nan", path: "semantic.context.relations[0].confidence", mutate: func(report *brainBriefReport) { report.Semantic.Context.Relations[0].Confidence = math.NaN() }},
		{name: "relation_inf", path: "semantic.context.relations[0].confidence", mutate: func(report *brainBriefReport) { report.Semantic.Context.Relations[0].Confidence = math.Inf(1) }},
		{name: "runtime_nan", path: "semantic.runtime_traces[0].confidence", mutate: func(report *brainBriefReport) { report.Semantic.RuntimeTraces[0].Confidence = math.NaN() }},
		{name: "symbol_inf", path: "semantic.context.symbols[0].confidence", mutate: func(report *brainBriefReport) { report.Semantic.Context.Symbols[0].Confidence = math.Inf(-1) }},
		{name: "review_nan", path: `facts_pending_review["fact:1"].confidence`, mutate: func(report *brainBriefReport) {
			notice := report.FactsPendingReview[report.Facts[0].ID]
			notice.Confidence = math.NaN()
			report.FactsPendingReview[report.Facts[0].ID] = notice
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := newBrainBriefPacketMeasurementReport()
			test.mutate(&report)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err := emitBrainBriefAgentV2(cmd, report, brainBriefDeliveryAlways, 8)
			if err == nil || !strings.Contains(err.Error(), "agent_v2 cannot encode non-finite "+test.path) {
				t.Fatalf("error = %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("failed emission released %d bytes", out.Len())
			}
		})
	}
}

func TestBrainBriefAgentV2OptionalPriorityUsesPrefixesAndResidualCapacity(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	clearBrainBriefAgentV1OptionalPressureFields(&report)
	report.Semantic.Context.Relations = []semanticRecord{
		{Type: "CALLS", FromID: "sym:a", ToID: "sym:b", Resolution: "exact", Confidence: 0.9},
		{Type: "CALLS", FromID: "sym:b", ToID: "sym:c", Resolution: strings.Repeat("x", brainBriefAgentV2BudgetBytes), Confidence: 0.8},
		{Type: "CALLS", FromID: "sym:c", ToID: "sym:d", Resolution: "exact", Confidence: 0.7},
	}
	report.Semantic.RuntimeTraces = []semanticRecord{{Type: "RUNTIME_TRACE", FromID: "sym:a", ToID: "sym:b", Reason: "observed", Confidence: 0.6}}
	report.History.Matches = []brainTextMatch{{Score: 3, Excerpt: "lower priority residual history"}}
	projection, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v2: %v", err)
	}
	if len(projection.packet) > brainBriefAgentV2BudgetBytes {
		t.Fatalf("packet bytes = %d", len(projection.packet))
	}
	records := parseAgentV2Records(t, projection.packet)
	relations := agentV1RecordsByTag(records, "semantic_relation")
	if len(relations) != 1 || agentV2IntField(t, relations[0], "rank") != 0 {
		t.Fatalf("relation prefix = %+v", relations)
	}
	if len(agentV1RecordsByTag(records, "runtime_trace")) != 1 || len(agentV1RecordsByTag(records, "history")) != 1 {
		t.Fatal("lower-priority short records did not use residual capacity")
	}
	end := agentV1RecordsByTag(records, "end")[0]
	if got := agentV2IntField(t, end, "available_semantic_relations"); got != 3 {
		t.Fatalf("available semantic relations = %d", got)
	}
	if got := agentV2RawField(t, end, "truncated"); got != "true" {
		t.Fatalf("truncated = %s", got)
	}
}

func TestBrainBriefAgentV2EdgePressureCannotDisplaceV1TestSuggestions(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	clearBrainBriefAgentV1OptionalPressureFields(&report)
	report.Semantic.Tests.Suggestions = []semanticTestSuggestion{{
		Symbol: semanticRecord{
			ID: "test:priority", Kind: "function", Name: "TestPriority", FilePath: "internal/priority_test.go",
			StartLine: 10, EndLine: 20,
		},
		Reason: "existing V1 actionable test suggestion",
	}}
	report.Semantic.Context.Relations = make([]semanticRecord, 500)
	for index := range report.Semantic.Context.Relations {
		report.Semantic.Context.Relations[index] = semanticRecord{
			Type:       "CALLS",
			FromID:     fmt.Sprintf("sym:source:%03d", index),
			ToID:       fmt.Sprintf("sym:target:%03d", index),
			Resolution: strings.Repeat("resolved", 8),
			Confidence: 0.5,
		}
	}
	projection, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build edge-pressure packet: %v", err)
	}
	records := parseAgentV2Records(t, projection.packet)
	suggestions := agentV1RecordsByTag(records, "test_suggestion")
	if len(suggestions) != 1 || agentV2StringField(t, suggestions[0], "reason") != report.Semantic.Tests.Suggestions[0].Reason {
		t.Fatal("edge pressure displaced the existing V1 test-suggestion section")
	}
	relations := agentV1RecordsByTag(records, "semantic_relation")
	if len(relations) == 0 || len(relations) >= len(report.Semantic.Context.Relations) {
		t.Fatalf("edge pressure relation count = %d, want non-empty strict prefix of %d", len(relations), len(report.Semantic.Context.Relations))
	}
	if got := agentV2RawField(t, agentV1RecordsByTag(records, "end")[0], "truncated"); got != "true" {
		t.Fatalf("edge pressure truncated = %s", got)
	}
}

func TestBrainBriefAgentV2MandatoryOverflowFailsBeforeWrite(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.Facts[0].Text = strings.Repeat("x", brainBriefAgentV2BudgetBytes)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := emitBrainBriefAgentV2(cmd, report, brainBriefDeliveryAlways, 8)
	if err == nil || !strings.Contains(err.Error(), "agent_v2 mandatory packet") {
		t.Fatalf("overflow error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("overflow released %d bytes", out.Len())
	}
}

func TestBrainBriefAgentV2IntegrityRejectsTamperingAndRecomputedInvalidConfig(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.Semantic.RuntimeTraces[0].Reason = brainBriefAgentV2RuntimeReasonPrefix + "MEASURE_RUNTIME_REASON"
	projection, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v2: %v", err)
	}
	packet := projection.packet
	if err := validateBrainBriefAgentV2Integrity(packet); err != nil {
		t.Fatalf("control integrity: %v", err)
	}
	tests := []struct {
		name   string
		packet string
	}{
		{name: "truncated", packet: packet[:len(packet)-1]},
		{name: "body", packet: strings.Replace(packet, "MEASURE_RUNTIME_REASON", "MUTATED_RUNTIME_REASON", 1)},
		{name: "unknown_field", packet: strings.Replace(packet, "semantic_relation rank=0", "semantic_relation unknown=1 rank=0", 1)},
		{name: "footer_count", packet: strings.Replace(packet, " semantic_relations=1", " semantic_relations=0", 1)},
		{name: "config_hash", packet: strings.Replace(packet, "config_sha256=\"sha256:", "config_sha256=\"sha256:f", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateBrainBriefAgentV2Integrity(test.packet); err == nil {
				t.Fatal("tampered packet passed integrity")
			}
		})
	}

	invalidRequested := rewriteAgentV2ConfigForTest(t, packet, projection.counts, -1, 1)
	if err := validateBrainBriefAgentV2Integrity(invalidRequested); err == nil || !strings.Contains(err.Error(), "requested_limit must be positive") {
		t.Fatalf("recomputed invalid requested_limit error = %v", err)
	}
	invalidEffective := rewriteAgentV2ConfigForTest(t, packet, projection.counts, 8, 5)
	if err := validateBrainBriefAgentV2Integrity(invalidEffective); err == nil || !strings.Contains(err.Error(), "effective_fact_limit mismatch") {
		t.Fatalf("recomputed invalid effective_fact_limit error = %v", err)
	}

	badReview := rewriteAgentV2PacketForTest(t, packet, projection.counts, projection.counts, func(lines []string) {
		for index, line := range lines {
			if strings.HasPrefix(line, "fact_review ") {
				lines[index] = strings.Replace(line, `fact_id="fact:1"`, `fact_id="fact:other"`, 1)
				return
			}
		}
		t.Fatal("fact_review record missing")
	})
	if err := validateBrainBriefAgentV2Integrity(badReview); err == nil || !strings.Contains(err.Error(), "does not immediately follow its fact") {
		t.Fatalf("reassociated fact_review error = %v", err)
	}

	badWarning := rewriteAgentV2PacketForTest(t, packet, projection.counts, projection.counts, func(lines []string) {
		for index, line := range lines {
			if strings.HasPrefix(line, "trust_warning ") {
				lines[index] = strings.TrimSuffix(agentV1RecordLine("trust_warning",
					compactV1StringAlways("code", "raw_report_warning"),
					compactV1StringAlways("message", "PRIVATE raw warning"),
				), "\n")
				return
			}
		}
		t.Fatal("trust_warning record missing")
	})
	if err := validateBrainBriefAgentV2Integrity(badWarning); err == nil || !strings.Contains(err.Error(), "approved safe warning") {
		t.Fatalf("recomputed unsafe trust warning error = %v", err)
	}

	badObservedType := rewriteAgentV2PacketForTest(t, packet, projection.counts, projection.counts, func(lines []string) {
		for index, line := range lines {
			if strings.HasPrefix(line, "runtime_trace ") {
				lines[index] = strings.Replace(line, `observed_type="MEASURE_RUNTIME_REASON"`, `observed_type="/Users/private/type"`, 1)
				return
			}
		}
		t.Fatal("runtime_trace record missing")
	})
	if err := validateBrainBriefAgentV2Integrity(badObservedType); err == nil || !strings.Contains(err.Error(), "observed_type is unsafe") {
		t.Fatalf("recomputed unsafe observed_type error = %v", err)
	}
}

func TestBrainBriefAgentV2DeterministicSliceOrder(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.Semantic.Context.Relations = append(report.Semantic.Context.Relations,
		semanticRecord{Type: "IMPLEMENTS", FromID: "sym:second", ToID: "sym:target", Resolution: "name_only", Confidence: 0.4})
	report.Semantic.RuntimeTraces = append(report.Semantic.RuntimeTraces,
		semanticRecord{Type: "RUNTIME_TRACE", FromID: "sym:second", ToID: "sym:target", Reason: brainBriefAgentV2RuntimeReasonPrefix + "IMPLEMENTS", Confidence: 0.3})
	first, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("fresh agent_v2 builds are not deterministic")
	}
	records := parseAgentV2Records(t, first.packet)
	for _, tag := range []string{"semantic_relation", "runtime_trace"} {
		for rank, record := range agentV1RecordsByTag(records, tag) {
			if got := agentV2IntField(t, record, "rank"); got != rank {
				t.Fatalf("%s[%d].rank = %d", tag, rank, got)
			}
		}
	}
	report.Semantic.Context.Relations[0], report.Semantic.Context.Relations[1] = report.Semantic.Context.Relations[1], report.Semantic.Context.Relations[0]
	reordered, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.packet == first.packet {
		t.Fatal("agent_v2 ignored source relation order")
	}
}

func TestBrainBriefAgentV2RichMeasurementAgainstFrozenSixArmBaseline(t *testing.T) {
	report := newBrainBriefPacketMeasurementReport()
	report.Semantic.RuntimeTraces[0].Reason = brainBriefAgentV2RuntimeReasonPrefix + "MEASURE_RUNTIME_REASON"
	v2, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		t.Fatalf("build agent_v2: %v", err)
	}
	v1, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		t.Fatalf("build agent_v1 control: %v", err)
	}
	v2Metrics := measureBrainBriefPacket(v2.packet)
	v1Metrics := measureBrainBriefPacket(v1.packet)
	v2Utility := measureBrainBriefPacketUtility(v2.packet)
	const wantUtility = "evidence_fidelity=10/10@3500bp;trust_signal_retention=4/7@2500bp;navigation_actionability=9/9@2500bp;task_current_state=3/3@1500bp;packet_utility_score_bp=8928"
	wantMetrics := brainBriefPacketMeasurementMetrics{
		SHA256: "sha256:9bb1de87f60a493b0cb6abdb653f11becb49a6547b1f7fd98413cae31e404802", Bytes: 2866, UnicodeRunes: 2866, Lexemes: 584,
		LexemeMetric: brainBriefPacketMeasurementLexemeVersion,
	}
	if v2Metrics != wantMetrics {
		t.Errorf("agent_v2 rich metrics = %+v, want frozen %+v", v2Metrics, wantMetrics)
	}
	if got := formatBrainBriefPacketUtility(v2Utility); got != wantUtility {
		t.Errorf("agent_v2 utility = %s, want %s", got, wantUtility)
	}
	if v1Metrics.SHA256 != "sha256:dde300548b0fe5c50a1a089faf78cd659c365d4f2f69f1757a30e09b2a1278f3" || v1Metrics.Bytes != 2486 || v1Metrics.Lexemes != 508 {
		t.Fatalf("frozen six-arm agent_v1 control drifted: %+v", v1Metrics)
	}
	if v2Metrics.Bytes >= 12220 || v2Metrics.Lexemes >= 2995 {
		t.Fatalf("agent_v2 lost compactness vs frozen legacy JSON: %+v", v2Metrics)
	}
	t.Logf("agent_v2 rich: bytes=%d (%+d vs agent_v1), lexemes=%d (%+d, tokenizer-free non-token proxy), utility=%d (%+d bp, transparent packet-content rubric, not code quality)",
		v2Metrics.Bytes, v2Metrics.Bytes-v1Metrics.Bytes, v2Metrics.Lexemes, v2Metrics.Lexemes-v1Metrics.Lexemes,
		v2Utility.ScoreBP, v2Utility.ScoreBP-measureBrainBriefPacketUtility(v1.packet).ScoreBP)
}

func rewriteAgentV2ConfigForTest(
	t *testing.T,
	packet string,
	counts brainBriefAgentV2Counts,
	requestedLimit, effectiveFactLimit int,
) string {
	t.Helper()
	return rewriteAgentV2PacketForTest(t, packet, counts, counts, func(lines []string) {
		lines[1] = strings.TrimSuffix(agentV1RecordLine("config",
			compactV1IntAlways("schema", brainBriefAgentV2Schema),
			compactV1StringAlways("packet_format", "agent_v2"),
			compactV1StringAlways("delivery_policy", string(brainBriefDeliveryAlways)),
			compactV1IntAlways("byte_budget", brainBriefAgentV2BudgetBytes),
			compactV1IntAlways("requested_limit", requestedLimit),
			compactV1IntAlways("effective_fact_limit", effectiveFactLimit),
			compactV1StringAlways("config_sha256", brainBriefAgentV2ConfigIdentity(brainBriefDeliveryAlways, requestedLimit, effectiveFactLimit)),
		), "\n")
	})
}

func rewriteAgentV2PacketForTest(
	t *testing.T,
	packet string,
	emitted, available brainBriefAgentV2Counts,
	mutate func([]string),
) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	mutate(lines)
	body := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	bodyHash := sha256.Sum256([]byte(body))
	return body + brainBriefAgentV2EndRecordLine(
		emitted, available, len(lines)-2, len(body), fmt.Sprintf("sha256:%x", bodyHash),
	)
}

func parseAgentV2Records(t *testing.T, packet string) []compactV1RawRecord {
	t.Helper()
	if !strings.HasSuffix(packet, "\n") {
		t.Fatal("agent_v2 packet is missing final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 5 || lines[0] != brainBriefAgentV2Marker {
		t.Fatalf("agent_v2 marker = %q", lines[0])
	}
	records := make([]compactV1RawRecord, 0, len(lines)-1)
	for _, line := range lines[1:] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			t.Fatalf("parse agent_v2 record %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func agentV2HasField(record compactV1RawRecord, key string) bool {
	for _, field := range record.fields {
		if field.key == key {
			return true
		}
	}
	return false
}

func agentV2RawField(t *testing.T, record compactV1RawRecord, key string) string {
	t.Helper()
	raw, err := agentV2RecordField(record, key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func agentV2StringField(t *testing.T, record compactV1RawRecord, key string) string {
	t.Helper()
	value, err := agentV2RecordString(record, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func agentV2IntField(t *testing.T, record compactV1RawRecord, key string) int {
	t.Helper()
	value, err := agentV2RecordInt(record, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestBrainBriefAgentV2SizingSHAIsFixedWidth(t *testing.T) {
	if len(brainBriefAgentV2SizingSHA256) != len("sha256:")+sha256.Size*2 ||
		strconv.Quote(brainBriefAgentV2SizingSHA256) != `"`+brainBriefAgentV2SizingSHA256+`"` {
		t.Fatalf("sizing SHA is not fixed-width unescaped SHA-256: %q", brainBriefAgentV2SizingSHA256)
	}
}

func TestBrainBriefProfilePreservesAgentV2PacketAndUsesEmittedCounts(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	task := "PRIVATE_AGENT_V2_PROFILE_TASK PRIVATE_FACT_PAYLOAD ValidateToken"
	briefOpts := brainBriefOptions{
		limit:          brainBriefPacketMeasurementLimit,
		packetFormat:   brainBriefPacketAgentV2,
		deliveryPolicy: brainBriefDeliveryAlways,
	}
	withoutProfile := runBrainBriefProfileAgentV1Test(t, fixture, briefOpts, task)
	profilePath := filepath.Join(t.TempDir(), "agent-v2-profile.json")
	briefOpts.profileJSON = profilePath
	withProfile := runBrainBriefProfileAgentV1Test(t, fixture, briefOpts, task)
	if withProfile != withoutProfile {
		t.Fatalf("profiling changed agent_v2 packet bytes\nwithout (%d):\n%s\nwith (%d):\n%s",
			len(withoutProfile), withoutProfile, len(withProfile), withProfile)
	}
	if err := validateBrainBriefAgentV2Integrity(withProfile); err != nil {
		t.Fatalf("profiled agent_v2 integrity: %v", err)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	for _, private := range []string{task, "PRIVATE_AGENT_V2_PROFILE_TASK", "PRIVATE_FACT_PAYLOAD", fixture.repoDir, fixture.brainDir} {
		if strings.Contains(string(data), private) {
			t.Fatalf("agent_v2 profile leaked private value %q:\n%s", private, data)
		}
	}
	var profile brainBriefProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if profile.Packet.Format != string(brainBriefPacketAgentV2) || profile.Packet.ByteCount != len(withProfile) {
		t.Fatalf("agent_v2 packet metrics = %+v, output bytes = %d", profile.Packet, len(withProfile))
	}
	records := parseAgentV2Records(t, withProfile)
	actual, err := brainBriefAgentV2CountBodyRecords(records[:len(records)-1])
	if err != nil {
		t.Fatalf("count profiled agent_v2 body: %v", err)
	}
	if profile.Packet.Counts != actual.profileCounts() {
		t.Fatalf("profile counts = %+v, emitted body = %+v", profile.Packet.Counts, actual.profileCounts())
	}

	if counts, err := brainBriefProfilePacketCountsWithV2(
		newBrainBriefPacketMeasurementReport(), brainBriefPacketAgentV2, nil, nil,
	); err == nil || !strings.Contains(err.Error(), "counts unavailable") || counts != (brainBriefProfileCounts{}) {
		t.Fatalf("agent_v2 missing-emission profile counts = %+v, %v", counts, err)
	}
}
