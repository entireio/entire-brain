package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBrainBriefDeduplicateTestRootsRequiresWholeRecordMatch(t *testing.T) {
	context := comprehensiveBrainBriefSemanticRecord("symbol:context")
	exact := cloneBrainBriefSemanticRecord(context)
	sameIDDifferentPayload := cloneBrainBriefSemanticRecord(context)
	sameIDDifferentPayload.Signature += " changed"
	sameIDDifferentWarnings := cloneBrainBriefSemanticRecord(context)
	sameIDDifferentWarnings.WarningCodes[0] = "different-warning"
	sameIDDifferentEvidence := cloneBrainBriefSemanticRecord(context)
	sameIDDifferentEvidence.Evidence[0].Detail = "different evidence"
	unique := comprehensiveBrainBriefSemanticRecord("symbol:unique")
	suggestion := semanticTestSuggestion{
		Symbol: comprehensiveBrainBriefSemanticRecord("test:suggestion"),
		Reason: "preserve the actionable test reason",
	}
	semantic := brainBriefSemantic{
		Context: semanticContextResult{Symbols: []semanticRecord{context}},
		Tests: semanticTestsResult{
			Roots: []semanticRecord{
				sameIDDifferentPayload,
				exact,
				unique,
				sameIDDifferentWarnings,
				sameIDDifferentEvidence,
				cloneBrainBriefSemanticRecord(context),
			},
			Suggestions: []semanticTestSuggestion{suggestion},
		},
	}

	if got := brainBriefDeduplicateTestRoots(&semantic); got != 2 {
		t.Fatalf("removed roots = %d, want 2", got)
	}
	wantRoots := []semanticRecord{
		sameIDDifferentPayload,
		unique,
		sameIDDifferentWarnings,
		sameIDDifferentEvidence,
	}
	if !reflect.DeepEqual(semantic.Tests.Roots, wantRoots) {
		t.Fatalf("stable retained roots mismatch\n got: %#v\nwant: %#v", semantic.Tests.Roots, wantRoots)
	}
	if !reflect.DeepEqual(semantic.Tests.Suggestions, []semanticTestSuggestion{suggestion}) {
		t.Fatalf("suggestions changed: %#v", semantic.Tests.Suggestions)
	}
}

func TestBrainBriefDeduplicateTestRootsPreservesNonNilEmptyRoots(t *testing.T) {
	record := comprehensiveBrainBriefSemanticRecord("symbol:only")
	semantic := brainBriefSemantic{
		Context: semanticContextResult{Symbols: []semanticRecord{record}},
		Tests:   semanticTestsResult{Roots: []semanticRecord{cloneBrainBriefSemanticRecord(record)}},
	}
	if got := brainBriefDeduplicateTestRoots(&semantic); got != 1 {
		t.Fatalf("removed roots = %d, want 1", got)
	}
	if semantic.Tests.Roots == nil || len(semantic.Tests.Roots) != 0 {
		t.Fatalf("roots = %#v, want non-nil empty slice", semantic.Tests.Roots)
	}
	data, err := json.Marshal(semantic.Tests)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"roots":[]`)) {
		t.Fatalf("empty roots did not encode as []: %s", data)
	}
}

func TestBrainBriefDeduplicateTestRootsNoMatchIsPacketByteIdentical(t *testing.T) {
	report := comprehensiveCompactV1Report()
	before := renderBrainBriefAllPacketFormats(t, report)
	if got := brainBriefDeduplicateTestRoots(&report.Semantic); got != 0 {
		t.Fatalf("removed roots = %d, want 0", got)
	}
	after := renderBrainBriefAllPacketFormats(t, report)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("no-match packet bytes changed\n before: %#v\n after: %#v", before, after)
	}
}

func TestBrainBriefDeduplicateTestRootsDeterministicAcrossPacketFormats(t *testing.T) {
	report := comprehensiveCompactV1Report()
	duplicate := cloneBrainBriefSemanticRecord(report.Semantic.Context.Symbols[0])
	firstUnique := comprehensiveBrainBriefSemanticRecord("test:first")
	secondUnique := comprehensiveBrainBriefSemanticRecord("test:second")
	report.Semantic.Tests.Roots = []semanticRecord{firstUnique, duplicate, secondUnique}
	wantLikelyEdits := slices.Clone(report.LikelyEditFiles)
	wantLikelyTests := slices.Clone(report.LikelyTestFiles)
	wantActions := slices.Clone(report.ActionChecklist)
	wantSuggestions := slices.Clone(report.Semantic.Tests.Suggestions)

	if got := brainBriefDeduplicateTestRoots(&report.Semantic); got != 1 {
		t.Fatalf("removed roots = %d, want 1", got)
	}
	if !reflect.DeepEqual(report.Semantic.Tests.Roots, []semanticRecord{firstUnique, secondUnique}) {
		t.Fatalf("retained roots lost stable order: %#v", report.Semantic.Tests.Roots)
	}
	if !slices.Equal(report.LikelyEditFiles, wantLikelyEdits) || !slices.Equal(report.LikelyTestFiles, wantLikelyTests) || !reflect.DeepEqual(report.ActionChecklist, wantActions) {
		t.Fatalf("packet-boundary dedupe changed synthesized guidance: %+v", report)
	}
	if !reflect.DeepEqual(report.Semantic.Tests.Suggestions, wantSuggestions) {
		t.Fatal("packet-boundary dedupe changed test suggestions")
	}

	first := renderBrainBriefAllPacketFormats(t, report)
	second := renderBrainBriefAllPacketFormats(t, report)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("deduped packets are nondeterministic\n first: %#v\nsecond: %#v", first, second)
	}
	for format, packet := range first {
		if !strings.Contains(packet, firstUnique.ID) || !strings.Contains(packet, secondUnique.ID) {
			t.Fatalf("%s lost a unique root:\n%s", format, packet)
		}
	}
}

func TestBrainBriefLivePacketDeduplicatesSemanticTestRootsAndProfilesOutput(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	task := "ValidateToken PRIVATE_TASK_PAYLOAD"
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--limit", "3", "--profile-json", profilePath)
	if err != nil {
		t.Fatalf("brain brief: %v\n%s", err, packet)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatalf("parse packet: %v\n%s", err, packet)
	}
	if len(report.Semantic.Context.Symbols) == 0 {
		t.Fatal("live fixture produced no context symbols")
	}
	if report.Semantic.Tests.Roots == nil || len(report.Semantic.Tests.Roots) != 0 {
		t.Fatalf("live packet roots = %#v, want non-nil empty slice after exact dedupe", report.Semantic.Tests.Roots)
	}
	if len(report.Semantic.Tests.Suggestions) == 0 {
		t.Fatal("live packet produced no test suggestions")
	}
	for i := range report.Semantic.Tests.Roots {
		for j := range report.Semantic.Context.Symbols {
			if reflect.DeepEqual(report.Semantic.Tests.Roots[i], report.Semantic.Context.Symbols[j]) {
				t.Fatalf("live packet retained duplicate test root %q", report.Semantic.Tests.Roots[i].ID)
			}
		}
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		if suggestion.Reason == "" {
			t.Fatalf("live packet lost suggestion reason: %#v", suggestion)
		}
	}

	profileData, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile brainBriefProfile
	if err := json.Unmarshal(profileData, &profile); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if profile.Packet.Counts.TestRoots != len(report.Semantic.Tests.Roots) {
		t.Fatalf("profile test roots = %d, emitted %d", profile.Packet.Counts.TestRoots, len(report.Semantic.Tests.Roots))
	}
}

func BenchmarkBrainBriefDeduplicateTestRoots(b *testing.B) {
	context := make([]semanticRecord, brainBriefDefaultLimit)
	for i := range context {
		context[i] = comprehensiveBrainBriefSemanticRecord("symbol:" + string(rune('a'+i)))
	}
	matchingRoots := make([]semanticRecord, len(context))
	for i := range context {
		matchingRoots[i] = cloneBrainBriefSemanticRecord(context[len(context)-1-i])
	}
	noMatchRoots := make([]semanticRecord, len(context))
	for i := range noMatchRoots {
		noMatchRoots[i] = comprehensiveBrainBriefSemanticRecord("unique:" + string(rune('a'+i)))
	}

	for _, benchmark := range []struct {
		name  string
		roots []semanticRecord
	}{
		{name: "eight_exact_duplicates", roots: matchingRoots},
		{name: "eight_no_matches", roots: noMatchRoots},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			semantic := brainBriefSemantic{Context: semanticContextResult{Symbols: context}}
			b.ReportAllocs()
			b.ResetTimer()
			removed := 0
			for range b.N {
				semantic.Tests.Roots = benchmark.roots
				removed += brainBriefDeduplicateTestRoots(&semantic)
			}
			benchmarkBrainBriefSemanticRootsRemoved = removed
		})
	}
}

var benchmarkBrainBriefSemanticRootsRemoved int

func comprehensiveBrainBriefSemanticRecord(id string) semanticRecord {
	return semanticRecord{
		RecordType: "symbol", ID: id, Kind: "function", Name: "ValidateToken", QualifiedName: "auth.ValidateToken",
		FilePath: "internal/auth/token.go", StartLine: 10, EndLine: 24, Path: "auth/token", Signature: "func ValidateToken(string) error",
		Language: "Go", FromID: "from", ToID: "to", Type: "CALLS", WarningCodes: []string{"partial", "fallback"}, Confidence: 0.875,
		Reason: "task match", StableIDVersion: "v1", Blob: "blob", Score: 9, Bytes: 144, ContainerID: "container", BodyHash: "body-hash",
		RelationScope: "file", Resolution: "exact", TargetKind: "symbol",
		Evidence: []semanticEvidence{{Kind: "call", FilePath: "internal/auth/token.go", StartLine: 18, EndLine: 18, Detail: "direct call"}},
	}
}

func cloneBrainBriefSemanticRecord(record semanticRecord) semanticRecord {
	record.WarningCodes = slices.Clone(record.WarningCodes)
	record.Evidence = slices.Clone(record.Evidence)
	return record
}

func renderBrainBriefAllPacketFormats(t *testing.T, report brainBriefReport) map[string]string {
	t.Helper()
	return map[string]string{
		"json":       renderBrainBriefLegacyJSONForTest(t, report),
		"compact_v1": renderBrainBriefCompactV1ForTest(t, report),
		"compact_v2": renderBrainBriefCompactV2ForTest(t, report),
		"compact_v3": renderBrainBriefCompactV3ForTest(t, report),
	}
}

func renderBrainBriefLegacyJSONForTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := emitBrainBriefReport(cmd, report, true); err != nil {
		t.Fatalf("render legacy JSON: %v", err)
	}
	return out.String()
}
