package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const brainBriefAgentV1FixedUTF8Runes = 10_000

type brainBriefAgentV1PressureFixture struct {
	name               string
	newReport          func() brainBriefReport
	fixtureFingerprint string
	packetSHA256       string
	packetBytes        int
}

func TestBrainBriefAgentV1PressureFixtures(t *testing.T) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			report := fixture.newReport()
			projection := verifyBrainBriefAgentV1PressureFixture(t, fixture, report)

			secondReport := fixture.newReport()
			second := verifyBrainBriefAgentV1PressureFixture(t, fixture, secondReport)
			if second.packet != projection.packet || second.counts != projection.counts {
				t.Fatal("fresh fixture did not produce deterministic packet bytes and counts")
			}

			assertBrainBriefAgentV1PressureShape(t, fixture.name, report, projection)
		})
	}
}

// BenchmarkBrainBriefAgentV1ProfileEmissionCounts measures the production
// profiled agent_v1 serialization/count path. Its name and fixtures match the
// precursor baseline that rebuilt the packet for counts, while this version
// consumes counts returned by the successful emission. Fixture construction,
// snapshot checks, and byte/count parity checks stay outside the timer.
func BenchmarkBrainBriefAgentV1ProfileEmissionCounts(b *testing.B) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		b.Run(fixture.name, func(b *testing.B) {
			report := fixture.newReport()
			want := verifyBrainBriefAgentV1PressureFixture(b, fixture, report)
			before := brainBriefPacketMeasurementFingerprint(b, report)

			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			var (
				counts        brainBriefProfileCounts
				emittedCounts brainBriefAgentV1Counts
				err           error
			)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				emittedCounts, err = emitBrainBriefAgentV1WithCounts(
					cmd, report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit,
				)
				if err != nil {
					break
				}
				counts, err = brainBriefProfilePacketCounts(report, brainBriefPacketAgentV1, &emittedCounts)
				if err != nil {
					break
				}
			}
			b.StopTimer()

			if err != nil {
				b.Fatalf("profile emission: %v", err)
			}
			if counts != want.counts.profileCounts() {
				b.Fatalf("profile counts = %+v, want emitted %+v", counts, want.counts.profileCounts())
			}
			if after := brainBriefPacketMeasurementFingerprint(b, report); after != before {
				b.Fatalf("profile emission/counts mutated fixture: before=%s after=%s", before, after)
			}
			brainBriefAgentV1ProfileCountsBenchmarkSink = counts
			b.ReportMetric(float64(len(want.packet)), "packet-bytes")
		})
	}
}

// BenchmarkBrainBriefAgentV1Packing isolates the existing shared packet
// builder under ordinary and boundary-pressure shapes. Each timed iteration is
// one production build; validation and deterministic snapshot checks are done
// before and after the timed region.
func BenchmarkBrainBriefAgentV1Packing(b *testing.B) {
	for _, fixture := range brainBriefAgentV1PressureFixtures() {
		fixture := fixture
		b.Run(fixture.name, func(b *testing.B) {
			report := fixture.newReport()
			want := verifyBrainBriefAgentV1PressureFixture(b, fixture, report)
			before := brainBriefPacketMeasurementFingerprint(b, report)

			var (
				got brainBriefAgentV1Projection
				err error
			)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err = buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
				if err != nil {
					break
				}
			}
			b.StopTimer()

			if err != nil {
				b.Fatalf("build: %v", err)
			}
			if got.packet != want.packet || got.counts != want.counts {
				b.Fatal("timed build changed frozen packet bytes or emitted counts")
			}
			if after := brainBriefPacketMeasurementFingerprint(b, report); after != before {
				b.Fatalf("builder mutated fixture: before=%s after=%s", before, after)
			}
			brainBriefAgentV1PackingBenchmarkSink = got
			b.ReportMetric(float64(len(want.packet)), "packet-bytes")
		})
	}
}

var (
	brainBriefAgentV1ProfileCountsBenchmarkSink brainBriefProfileCounts
	brainBriefAgentV1PackingBenchmarkSink       brainBriefAgentV1Projection
)

func brainBriefAgentV1PressureFixtures() []brainBriefAgentV1PressureFixture {
	return []brainBriefAgentV1PressureFixture{
		{
			name:               "rich",
			newReport:          newBrainBriefPacketMeasurementReport,
			fixtureFingerprint: "sha256:435cb828133326d063286fc673fdd81ca8fbf683c7c9460bcb94cc59941072dc",
			packetSHA256:       "sha256:dde300548b0fe5c50a1a089faf78cd659c365d4f2f69f1757a30e09b2a1278f3",
			packetBytes:        2486,
		},
		{
			name:               "near_budget_history",
			newReport:          newBrainBriefAgentV1NearBudgetHistoryReport,
			fixtureFingerprint: "sha256:287efbecfb3902fcf6b3a273e5a710c1d2d403bbcfac32649b1627d37a8903ef",
			packetSHA256:       "sha256:522ca11063138f4f7e04f2e1b35cf1f91c2f8bc7696934eac4fdddfa0f0eed18",
			packetBytes:        32071,
		},
		{
			name:               "section_prefix",
			newReport:          newBrainBriefAgentV1SectionPrefixReport,
			fixtureFingerprint: "sha256:561f157a0afb0f0b07619f3cb509294adbe4ebc802a92e8dc4d30cbd6ec3e9ea",
			packetSHA256:       "sha256:2f369c72c92f10c9f225eff3737e2249f11774adff07a1f5564465cd0dd65947",
			packetBytes:        32573,
		},
		{
			name:               "fixed_utf8",
			newReport:          newBrainBriefAgentV1FixedUTF8Report,
			fixtureFingerprint: "sha256:d3916973e1a19d7c4897e19e293133d3aefadb2a170b97d8195f296bfd820913",
			packetSHA256:       "sha256:46ccbd161ca8b6f0b1099cc7c1ffcc34b6e26e0b8bf5baa037e9eae483c95848",
			packetBytes:        31473,
		},
	}
}

func newBrainBriefAgentV1NearBudgetHistoryReport() brainBriefReport {
	report := newBrainBriefPacketMeasurementReport()
	clearBrainBriefAgentV1OptionalPressureFields(&report)
	report.History.Matches = make([]brainTextMatch, 80)
	for i := range report.History.Matches {
		report.History.Matches[i] = brainTextMatch{
			Path:         fmt.Sprintf("sessions/synthetic-%03d.jsonl", i),
			Line:         i + 1,
			Timestamp:    fmt.Sprintf("synthetic-time-%03d", i),
			Score:        100 - i,
			MatchedTerms: []string{"validation", fmt.Sprintf("term-%03d", i)},
			Excerpt:      fmt.Sprintf("history-%03d %s", i, strings.Repeat("evidence ", 90)),
		}
	}
	return report
}

func newBrainBriefAgentV1SectionPrefixReport() brainBriefReport {
	report := newBrainBriefPacketMeasurementReport()
	clearBrainBriefAgentV1OptionalPressureFields(&report)
	report.LikelyEditFiles = make([]string, 100)
	for i := range report.LikelyEditFiles {
		report.LikelyEditFiles[i] = fmt.Sprintf("internal/%03d-%s.go", i, strings.Repeat("x", 550))
	}
	report.LikelyTestFiles = []string{"internal/a_test.go", "internal/b_test.go"}
	return report
}

func newBrainBriefAgentV1FixedUTF8Report() brainBriefReport {
	report := newBrainBriefPacketMeasurementReport()
	clearBrainBriefAgentV1OptionalPressureFields(&report)
	report.Facts[0].Text = strings.Repeat("界", brainBriefAgentV1FixedUTF8Runes)
	return report
}

func clearBrainBriefAgentV1OptionalPressureFields(report *brainBriefReport) {
	report.Semantic = brainBriefSemantic{}
	report.History.Matches = nil
	report.ActionChecklist = nil
	report.LikelyEditFiles = nil
	report.LikelyTestFiles = nil
	report.LikelyFiles = nil
}

func verifyBrainBriefAgentV1PressureFixture(
	tb testing.TB,
	fixture brainBriefAgentV1PressureFixture,
	report brainBriefReport,
) brainBriefAgentV1Projection {
	tb.Helper()
	fingerprint := brainBriefPacketMeasurementFingerprint(tb, report)
	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
	if err != nil {
		tb.Fatalf("build %s pressure fixture: %v", fixture.name, err)
	}
	metrics := measureBrainBriefPacket(projection.packet)

	if fingerprint != fixture.fixtureFingerprint {
		tb.Fatalf("%s fixture fingerprint = %s, want frozen %s", fixture.name, fingerprint, fixture.fixtureFingerprint)
	}
	if metrics.SHA256 != fixture.packetSHA256 || metrics.Bytes != fixture.packetBytes {
		tb.Fatalf("%s packet = %s/%d bytes, want frozen %s/%d bytes",
			fixture.name, metrics.SHA256, metrics.Bytes, fixture.packetSHA256, fixture.packetBytes)
	}
	if !utf8.ValidString(projection.packet) {
		tb.Fatalf("%s packet is not valid UTF-8", fixture.name)
	}
	if len(projection.packet) > brainBriefAgentV1BudgetBytes {
		tb.Fatalf("%s packet bytes = %d, budget = %d", fixture.name, len(projection.packet), brainBriefAgentV1BudgetBytes)
	}
	if err := validateBrainBriefAgentV1Integrity(projection.packet); err != nil {
		tb.Fatalf("validate %s packet integrity: %v", fixture.name, err)
	}

	var emitted bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&emitted)
	if err := emitBrainBriefPacketWithPolicy(
		cmd, report, brainBriefPacketAgentV1, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit,
	); err != nil {
		tb.Fatalf("emit %s pressure fixture: %v", fixture.name, err)
	}
	if emitted.String() != projection.packet {
		tb.Fatalf("%s emitter bytes differ from direct builder", fixture.name)
	}
	counts, err := brainBriefProfilePacketCounts(report, brainBriefPacketAgentV1, &projection.counts)
	if err != nil {
		tb.Fatalf("profile %s pressure fixture: %v", fixture.name, err)
	}
	if counts != projection.counts.profileCounts() {
		tb.Fatalf("%s profile counts = %+v, want emitted %+v", fixture.name, counts, projection.counts.profileCounts())
	}
	if after := brainBriefPacketMeasurementFingerprint(tb, report); after != fingerprint {
		tb.Fatalf("%s verification mutated fixture: before=%s after=%s", fixture.name, fingerprint, after)
	}
	return projection
}

func assertBrainBriefAgentV1PressureShape(
	t *testing.T,
	name string,
	report brainBriefReport,
	projection brainBriefAgentV1Projection,
) {
	t.Helper()
	records := parseAgentV1Records(t, projection.packet)
	end := agentV1RecordsByTag(records, "end")[0]
	switch name {
	case "rich":
		if got := agentV1RawField(t, end, "truncated"); got != "false" {
			t.Fatalf("rich truncated = %s, want false", got)
		}
	case "near_budget_history":
		history := agentV1RecordsByTag(records, "history")
		if len(history) == 0 || len(history) >= len(report.History.Matches) {
			t.Fatalf("history records = %d, want non-empty strict prefix of %d", len(history), len(report.History.Matches))
		}
		for i, record := range history {
			if got := agentV1IntField(t, record, "rank"); got != i {
				t.Fatalf("history[%d].rank = %d", i, got)
			}
			if got := agentV1StringField(t, record, "excerpt"); got != report.History.Matches[i].Excerpt {
				t.Fatalf("history[%d].excerpt changed", i)
			}
		}
		if got := agentV1RawField(t, end, "truncated"); got != "true" {
			t.Fatalf("near-budget history truncated = %s, want true", got)
		}
	case "section_prefix":
		edits := agentV1RecordsByTag(records, "edit_file")
		if len(edits) == 0 || len(edits) >= len(report.LikelyEditFiles) {
			t.Fatalf("edit records = %d, want non-empty strict prefix of %d", len(edits), len(report.LikelyEditFiles))
		}
		for i, record := range edits {
			if got := agentV1StringField(t, record, "path"); got != report.LikelyEditFiles[i] {
				t.Fatalf("edit_file[%d] leapfrogged prefix: %q", i, got)
			}
		}
		tests := agentV1RecordsByTag(records, "test_file")
		if len(tests) != len(report.LikelyTestFiles) {
			t.Fatalf("test records = %d, want lower-priority residual contribution %d", len(tests), len(report.LikelyTestFiles))
		}
		for i, record := range tests {
			if got := agentV1StringField(t, record, "path"); got != report.LikelyTestFiles[i] {
				t.Fatalf("test_file[%d] changed: %q", i, got)
			}
		}
		if got := agentV1RawField(t, end, "truncated"); got != "true" {
			t.Fatalf("section-prefix truncated = %s, want true", got)
		}
	case "fixed_utf8":
		facts := agentV1RecordsByTag(records, "fact")
		if len(facts) != 1 {
			t.Fatalf("facts = %d, want 1", len(facts))
		}
		text := agentV1StringField(t, facts[0], "text")
		if text != report.Facts[0].Text || utf8.RuneCountInString(text) != brainBriefAgentV1FixedUTF8Runes {
			t.Fatal("fixed UTF-8 fact text changed")
		}
		if len(text) != 3*brainBriefAgentV1FixedUTF8Runes {
			t.Fatalf("fixed UTF-8 bytes = %d, want %d", len(text), 3*brainBriefAgentV1FixedUTF8Runes)
		}
		if len(projection.packet) < brainBriefAgentV1BudgetBytes-2*1024 {
			t.Fatalf("fixed UTF-8 packet bytes = %d, want boundary pressure", len(projection.packet))
		}
		if got := agentV1RawField(t, end, "truncated"); got != "false" {
			t.Fatalf("fixed UTF-8 truncated = %s, want false", got)
		}
	default:
		t.Fatalf("unknown pressure fixture %q", name)
	}
}
