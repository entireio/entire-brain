package cli

import (
	"io"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

type brainBriefAgentV2PressureSnapshot struct {
	fixtureSHA256 string
	packetSHA256  string
	packetBytes   int
}

var brainBriefAgentV2PressureSnapshots = map[string]brainBriefAgentV2PressureSnapshot{
	"rich":                {fixtureSHA256: "sha256:d35d2cd5942f86c3849d0fa48a6db8bea65c289c2c3d225096dd1ca141d7489e", packetSHA256: "sha256:79b9646aefe2cf98734c3f13bae4bae571054999117505890be07074f8d002be", packetBytes: 2866},
	"near_budget_history": {fixtureSHA256: "sha256:287efbecfb3902fcf6b3a273e5a710c1d2d403bbcfac32649b1627d37a8903ef", packetSHA256: "sha256:cb45a44a90cb231b02904de55d491df0b91579c05f59598853839ada729a6c81", packetBytes: 32202},
	"section_prefix":      {fixtureSHA256: "sha256:561f157a0afb0f0b07619f3cb509294adbe4ebc802a92e8dc4d30cbd6ec3e9ea", packetSHA256: "sha256:f3c7fb242f8c5ba2e6af2781fc4b9a488ed9f9affae24377c790e69ce4a75734", packetBytes: 32704},
	"fixed_utf8":          {fixtureSHA256: "sha256:d3916973e1a19d7c4897e19e293133d3aefadb2a170b97d8195f296bfd820913", packetSHA256: "sha256:b3c20ff3903d4f1344c5a3af94bcf696863d43ec6387b63adc5982ad5592b3b4", packetBytes: 31604},
}

func TestBrainBriefAgentV2PressureFixtures(t *testing.T) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			report := newBrainBriefAgentV2PressureReport(fixture)
			projection := verifyBrainBriefAgentV2PressureFixture(t, fixture, report)
			second := verifyBrainBriefAgentV2PressureFixture(t, fixture, newBrainBriefAgentV2PressureReport(fixture))
			if second != projection {
				t.Fatal("fresh fixture did not produce deterministic agent_v2 packet bytes and counts")
			}
			assertBrainBriefAgentV2PressureShape(t, fixture.name, report, projection)
		})
	}
}

func BenchmarkBrainBriefAgentV2Packing(b *testing.B) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		b.Run(fixture.name, func(b *testing.B) {
			report := newBrainBriefAgentV2PressureReport(fixture)
			want := verifyBrainBriefAgentV2PressureFixture(b, fixture, report)
			before := brainBriefPacketMeasurementFingerprint(b, report)
			var (
				got brainBriefAgentV2Projection
				err error
			)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err = buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
				if err != nil {
					break
				}
			}
			b.StopTimer()
			if err != nil {
				b.Fatalf("build: %v", err)
			}
			if got != want {
				b.Fatal("timed build changed frozen packet bytes or emitted counts")
			}
			if after := brainBriefPacketMeasurementFingerprint(b, report); after != before {
				b.Fatalf("builder mutated fixture: before=%s after=%s", before, after)
			}
			brainBriefAgentV2PackingBenchmarkSink = got
			b.ReportMetric(float64(len(want.packet)), "packet-bytes")
		})
	}
}

func BenchmarkBrainBriefAgentV2ProfileEmissionCounts(b *testing.B) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		b.Run(fixture.name, func(b *testing.B) {
			report := newBrainBriefAgentV2PressureReport(fixture)
			want := verifyBrainBriefAgentV2PressureFixture(b, fixture, report)
			before := brainBriefPacketMeasurementFingerprint(b, report)
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			var (
				emitted brainBriefAgentV2Counts
				profile brainBriefProfileCounts
				err     error
			)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				emitted, err = emitBrainBriefAgentV2WithCounts(
					cmd, report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit,
				)
				if err != nil {
					break
				}
				profile, err = brainBriefProfilePacketCountsWithV2(report, brainBriefPacketAgentV2, nil, &emitted)
				if err != nil {
					break
				}
			}
			b.StopTimer()
			if err != nil {
				b.Fatalf("profile emission: %v", err)
			}
			if emitted != want.counts || profile != want.counts.profileCounts() {
				b.Fatal("profile emission counts differ from frozen packet")
			}
			if after := brainBriefPacketMeasurementFingerprint(b, report); after != before {
				b.Fatalf("profile emission mutated fixture: before=%s after=%s", before, after)
			}
			brainBriefAgentV2ProfileCountsBenchmarkSink = profile
			b.ReportMetric(float64(len(want.packet)), "packet-bytes")
		})
	}
}

var (
	brainBriefAgentV2PackingBenchmarkSink       brainBriefAgentV2Projection
	brainBriefAgentV2ProfileCountsBenchmarkSink brainBriefProfileCounts
)

func newBrainBriefAgentV2PressureReport(fixture brainBriefAgentV1PressureFixture) brainBriefReport {
	report := fixture.newReport()
	if fixture.name == "rich" && len(report.Semantic.RuntimeTraces) > 0 {
		report.Semantic.RuntimeTraces = append([]semanticRecord(nil), report.Semantic.RuntimeTraces...)
		report.Semantic.RuntimeTraces[0].Reason = brainBriefAgentV2RuntimeReasonPrefix + "MEASURE_RUNTIME_REASON"
	}
	return report
}

func verifyBrainBriefAgentV2PressureFixture(
	tb testing.TB,
	fixture brainBriefAgentV1PressureFixture,
	report brainBriefReport,
) brainBriefAgentV2Projection {
	tb.Helper()
	fingerprint := brainBriefPacketMeasurementFingerprint(tb, report)
	projection, err := buildBrainBriefAgentV2(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		tb.Fatalf("build %s agent_v2 fixture: %v", fixture.name, err)
	}
	if err := validateBrainBriefAgentV2Integrity(projection.packet); err != nil {
		tb.Fatalf("validate %s agent_v2 packet: %v", fixture.name, err)
	}
	metrics := measureBrainBriefPacket(projection.packet)
	want, ok := brainBriefAgentV2PressureSnapshots[fixture.name]
	if !ok {
		tb.Fatalf("missing %s agent_v2 pressure snapshot", fixture.name)
	}
	if fingerprint != want.fixtureSHA256 {
		tb.Fatalf("%s agent_v2 fixture fingerprint = %s, want frozen %s", fixture.name, fingerprint, want.fixtureSHA256)
	}
	if metrics.SHA256 != want.packetSHA256 || metrics.Bytes != want.packetBytes {
		tb.Fatalf("%s agent_v2 packet = %s/%d bytes, want frozen %s/%d bytes",
			fixture.name, metrics.SHA256, metrics.Bytes, want.packetSHA256, want.packetBytes)
	}
	if !utf8.ValidString(projection.packet) {
		tb.Fatalf("%s agent_v2 packet is not valid UTF-8", fixture.name)
	}
	if len(projection.packet) > brainBriefAgentV2BudgetBytes {
		tb.Fatalf("%s agent_v2 packet bytes = %d, budget = %d", fixture.name, len(projection.packet), brainBriefAgentV2BudgetBytes)
	}
	if after := brainBriefPacketMeasurementFingerprint(tb, report); after != fingerprint {
		tb.Fatalf("%s agent_v2 verification mutated fixture: before=%s after=%s", fixture.name, fingerprint, after)
	}
	return projection
}

func assertBrainBriefAgentV2PressureShape(
	t *testing.T,
	name string,
	report brainBriefReport,
	projection brainBriefAgentV2Projection,
) {
	t.Helper()
	records := parseAgentV2Records(t, projection.packet)
	end := agentV1RecordsByTag(records, "end")[0]
	switch name {
	case "rich":
		if len(agentV1RecordsByTag(records, "semantic_relation")) != 1 || len(agentV1RecordsByTag(records, "runtime_trace")) != 1 {
			t.Fatal("rich agent_v2 fixture dropped typed edges")
		}
		if got := agentV2RawField(t, end, "truncated"); got != "false" {
			t.Fatalf("rich truncated = %s", got)
		}
	case "near_budget_history":
		history := agentV1RecordsByTag(records, "history")
		if len(history) == 0 || len(history) >= len(report.History.Matches) {
			t.Fatalf("history records = %d, want non-empty strict prefix of %d", len(history), len(report.History.Matches))
		}
		for index, record := range history {
			if agentV2IntField(t, record, "rank") != index || agentV2StringField(t, record, "excerpt") != report.History.Matches[index].Excerpt {
				t.Fatalf("history[%d] changed or leapfrogged", index)
			}
		}
		if got := agentV2RawField(t, end, "truncated"); got != "true" {
			t.Fatalf("near-budget history truncated = %s", got)
		}
	case "section_prefix":
		edits := agentV1RecordsByTag(records, "edit_file")
		if len(edits) == 0 || len(edits) >= len(report.LikelyEditFiles) {
			t.Fatalf("edit records = %d, want non-empty strict prefix of %d", len(edits), len(report.LikelyEditFiles))
		}
		for index, record := range edits {
			if got := agentV2StringField(t, record, "path"); got != report.LikelyEditFiles[index] {
				t.Fatalf("edit_file[%d] leapfrogged prefix: %q", index, got)
			}
		}
		if len(agentV1RecordsByTag(records, "test_file")) != len(report.LikelyTestFiles) {
			t.Fatal("lower-priority test files did not use residual capacity")
		}
		if got := agentV2RawField(t, end, "truncated"); got != "true" {
			t.Fatalf("section-prefix truncated = %s", got)
		}
	case "fixed_utf8":
		facts := agentV1RecordsByTag(records, "fact")
		if len(facts) != 1 {
			t.Fatalf("facts = %d, want 1", len(facts))
		}
		text := agentV2StringField(t, facts[0], "text")
		if text != report.Facts[0].Text || utf8.RuneCountInString(text) != brainBriefAgentV1FixedUTF8Runes {
			t.Fatal("fixed UTF-8 fact text changed")
		}
		if len(projection.packet) < brainBriefAgentV2BudgetBytes-2*1024 {
			t.Fatalf("fixed UTF-8 packet bytes = %d, want boundary pressure", len(projection.packet))
		}
		if got := agentV2RawField(t, end, "truncated"); got != "false" {
			t.Fatalf("fixed UTF-8 truncated = %s", got)
		}
	default:
		t.Fatalf("unknown pressure fixture %q", name)
	}
}
