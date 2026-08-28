package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderDistillQualityHumanReviewV1(t *testing.T) {
	root, packets := reportFixtureRunV1(t)
	if err := os.Mkdir(filepath.Join(root, distillQualityPanelDirV1), 0o700); err != nil {
		t.Fatal(err)
	}
	verdict := reportAdmissionVerdictV1(packets[0], distillQualityJudgeClaudeV1, distillQualityAdvisoryCriticalV1)
	judgeData, _ := marshalDistillQualityJSONLV1([]distillQualityPanelVerdictV1{verdict})
	if err := os.WriteFile(filepath.Join(root, distillQualityPanelDirV1, "claude.jsonl"), judgeData, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := renderDistillQualityHumanReviewV1(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Critical != 1 || result.MissingJudges != 2 || result.ProofLabels {
		t.Fatalf("result = %+v", result)
	}
	review, err := os.ReadFile(result.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Human Review", "do **not** need to read", "distill-quality adjudicate", "20-item calibration", "full-screen terminal workspace", "--plain", distillQualityAuditArchiveFileV1} {
		if !strings.Contains(string(review), want) {
			t.Fatalf("review missing %q:\n%s", want, review)
		}
	}
	if strings.Contains(string(review), "sessions/main/local-1.jsonl") || strings.Count(string(review), "\n") > 40 {
		t.Fatalf("human start page is not concise:\n%s", review)
	}
	audit, err := os.ReadFile(result.AuditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Audit Archive", "Attention first", "claude", "critical", "Keep write operations explicit.", "sessions/main/local-1.jsonl", "local-session-1"} {
		if !strings.Contains(string(audit), want) {
			t.Fatalf("audit missing %q", want)
		}
	}
	template, err := os.ReadFile(result.TemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(template), `"decision":null`) || !strings.Contains(string(template), `"admission":null`) {
		t.Fatalf("template must be blank: %s", template)
	}
	for _, path := range []string{result.ReviewPath, result.AuditPath, result.AggregatePath, result.TemplatePath} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact mode %s = %v, %v", path, info, statErr)
		}
	}
}

func TestRenderDistillQualityHumanReviewRejectsCorruptJudgeFileV1(t *testing.T) {
	root, _ := reportFixtureRunV1(t)
	if err := os.Mkdir(filepath.Join(root, distillQualityPanelDirV1), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, distillQualityPanelDirV1, "cursor.jsonl"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renderDistillQualityHumanReviewV1(root); err == nil {
		t.Fatal("accepted corrupt existing judge file")
	}
}

func reportFixtureRunV1(t *testing.T) (string, []distillQualityPanelPacketV1) {
	t.Helper()
	root := t.TempDir()
	admitted := distillQualityAdmissionItemV1{ID: "public-1", Digest: strings.Repeat("1", 64), Stratum: "admitted:direct_user:rule", Roles: []string{"user"}, Authorities: []string{"direct_user"}, Cues: []string{"rule"}, Evidence: "user: Keep write operations explicit.", EvidenceBytes: len("user: Keep write operations explicit."), Admitted: true}
	filtered := distillQualityAdmissionItemV1{ID: "public-2", Digest: strings.Repeat("2", 64), Stratum: "user:context", Roles: []string{"user"}, Authorities: []string{"context_only"}, Cues: []string{}, Evidence: "user: What is the status?", EvidenceBytes: len("user: What is the status?"), Admitted: false}
	bundle := distillQualityBundleV1{SchemaVersion: distillQualityBundleSchemaVersionV1, Admission: []distillQualityAdmissionItemV1{admitted}, Filtered: []distillQualityAdmissionItemV1{filtered}}
	bundle.RunID = distillQualityBundleRunIDV1(bundle.Admission, bundle.Filtered)
	packets, err := distillQualityPacketsForBundleV1(bundle)
	if err != nil {
		t.Fatal(err)
	}
	packetBytes, err := marshalDistillQualityJSONLV1(packets)
	if err != nil {
		t.Fatal(err)
	}
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sources := []distillQualityPrivateSourceRecordV1{
		{PublicID: "public-1", SessionID: "local-session-1", Branch: "main", TranscriptPath: "sessions/main/local-1.jsonl", TurnIDs: []string{"turn-1"}, StartLine: 4, EndLine: 7},
		{PublicID: "public-2", SessionID: "local-session-2", Branch: "main", TranscriptPath: "sessions/main/local-2.jsonl", TurnIDs: []string{"turn-2"}, StartLine: 8, EndLine: 8},
	}
	sourceBytes, err := marshalDistillQualityJSONLV1(sources)
	if err != nil {
		t.Fatal(err)
	}
	manifest := distillQualityRunManifestV1{Contract: distillQualityRunContractV1, SchemaVersion: 1, RunID: bundle.RunID, GeneratedAt: "2026-08-25T12:00:00Z", EvaluatorRevision: "test-revision", SourceRevision: "source-revision", BrainManifestSHA256: reportTestDigestV1("brain"), CorpusSHA256: reportTestDigestV1("corpus"), BundleSHA256: distillQualitySHA256V1(bundleBytes), PacketsSHA256: distillQualitySHA256V1(packetBytes), PrivateSourceMapSHA256: distillQualitySHA256V1(sourceBytes), PromptSHA256: packets[0].Prompt.Digest, CandidateSchemaVersion: distillCandidateSchemaVersion, RedactionVersion: distillCandidateRedactionVersionV2, SessionViews: 1, AdmittedItems: 1, FilteredItems: 1, FilteredRequested: 1, PrivateSourceRecords: 2, AdvisoryOnly: true}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{distillQualityRunManifestFileV1: manifestBytes, distillQualityBundleFileV1: bundleBytes, distillQualityPacketsFileV1: packetBytes, distillQualitySourceMapFileV1: sourceBytes} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, packets
}

func reportAdmissionVerdictV1(packet distillQualityPanelPacketV1, judge distillQualityJudgeV1, admission distillQualityAdvisoryLabelV1) distillQualityPanelVerdictV1 {
	return distillQualityPanelVerdictV1{SchemaVersion: 1, PacketID: packet.PacketID, PacketDigest: packet.PacketDigest, ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest, Judge: judge, Model: "claude-test", State: distillQualityVerdictCompletedV1, Scores: []distillQualityPanelScoreV1{
		{Dimension: distillQualityDimensionAdmissionV1, Label: admission, RawScore: json.RawMessage(`{"should_admit":"no","candidate_admitted":true,"critical_miss":false}`), Rationale: "span-001 says this is not durable."},
		{Dimension: distillQualityDimensionAuthorityV1, Label: distillQualityAdvisoryPassV1, RawScore: json.RawMessage(`{"authority":"adequate"}`), Rationale: "span-001 is direct user context."},
		{Dimension: distillQualityDimensionSafetyV1, Label: distillQualityAdvisoryPassV1, RawScore: json.RawMessage(`{"unsafe_material":"no"}`), Rationale: "span-001 contains no unsafe material."},
	}}
}

func reportTestDigestV1(value string) string { return distillQualityPanelDigestV1(value) }
