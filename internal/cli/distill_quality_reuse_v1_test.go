package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReuseDistillQualityBundleV1CopiesOnlyExactVerdictsAndHumanRecord(t *testing.T) {
	base, packets := adjudicationFixtureRunV1(t)
	target, targetPackets := adjudicationFixtureRunV1(t)
	targetPackets[1].Payload.Fragments[0].Text += " changed"
	targetPackets[1].Payload.Fragments[0].Digest = distillQualityPanelFragmentDigestV1(targetPackets[1].Payload.Fragments[0])
	targetPackets[1].Payload.Digest = distillQualityPanelPayloadDigestV1(targetPackets[1].Payload)
	targetPackets[1].PacketDigest = distillQualityPanelPacketDigestV1(targetPackets[1])
	writeReuseFixturePacketsV1(t, target, targetPackets)
	if err := os.RemoveAll(filepath.Join(target, distillQualityPanelDirV1)); err != nil {
		t.Fatal(err)
	}
	_, _, sources, aggregates, err := loadDistillQualityAdjudicationInputsV1(base)
	if err != nil {
		t.Fatal(err)
	}
	items, err := buildDistillQualityAdjudicateItemsV1(packets, sources, aggregates, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := makeDistillQualityHumanAdjudicationV1("base-run", "reviewer", items[0], "yes", "yes", "yes", "retained", testReuseTimeV1())
	humanDir := filepath.Join(base, distillQualityHumanDirV1)
	if err := ensureDistillQualityPanelDirV1(humanDir); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillQualityHumanAdjudicationsV1(filepath.Join(humanDir, "reviewer.jsonl"), []distillQualityHumanAdjudicationV1{record}); err != nil {
		t.Fatal(err)
	}

	result, err := reuseDistillQualityBundleV1(base, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedVerdicts != len(distillQualityPanelJudgesV1) || result.DeltaPackets != 1 || result.MigratedHumanRecords != 1 {
		t.Fatalf("reuse result = %+v", result)
	}
	if len(result.Metadata.ReusedPacketIDs) != 1 || len(result.Metadata.DeltaPacketIDs) != 1 {
		t.Fatalf("reuse metadata = %+v", result.Metadata)
	}
	for _, judge := range distillQualityPanelJudgesV1 {
		packetByID := map[string]distillQualityPanelPacketV1{}
		for _, packet := range packets {
			packetByID[packet.PacketID] = packet
		}
		verdicts, err := loadDistillQualityJudgeVerdictsV1(target, judge, packetByID)
		if err != nil {
			t.Fatal(err)
		}
		if len(verdicts) != 1 || verdicts[0].PacketID != packets[0].PacketID {
			t.Fatalf("%s copied verdicts = %+v", judge, verdicts)
		}
		if _, err := os.Stat(filepath.Join(target, distillQualityPanelDirV1, string(judge)+"-calls.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("reuse copied call telemetry for %s: err=%v", judge, err)
		}
	}
	manifest, targetPackets, _, aggregates, err := loadDistillQualityAdjudicationInputsV1(target)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RunID != result.Metadata.TargetRunID {
		t.Fatalf("target run = %q metadata = %q", manifest.RunID, result.Metadata.TargetRunID)
	}
	records, err := loadDistillQualityHumanAdjudicationsV1(target, "reviewer", targetPackets, aggregates)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RecordID == record.RecordID || records[0].Rationale != record.Rationale || records[0].AdjudicatedAt != record.AdjudicatedAt {
		t.Fatalf("migrated records = %+v (base=%+v)", records, record)
	}
}

func TestReuseDistillQualityBundleV1DoesNotReuseSameIDWithDifferentDigest(t *testing.T) {
	base, _ := adjudicationFixtureRunV1(t)
	target, targetPackets := adjudicationFixtureRunV1(t)
	if err := os.RemoveAll(filepath.Join(target, distillQualityPanelDirV1)); err != nil {
		t.Fatal(err)
	}
	changed := &targetPackets[0]
	changed.Item.Text = `{"candidate_admitted":true,"stratum":"admitted:direct_user:rule","roles":["user"],"authorities":["direct_user"],"cues":["rule"],"evidence_bytes":12,"evidence_truncated":false}`
	changed.Item.Digest = distillQualityPanelItemDigestV1(changed.Item)
	changed.PacketDigest = distillQualityPanelPacketDigestV1(*changed)
	writeReuseFixturePacketsV1(t, target, targetPackets)

	result, err := reuseDistillQualityBundleV1(base, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedVerdicts != 0 || result.DeltaPackets != 1 || len(result.Metadata.DeltaPacketIDs) != 1 {
		t.Fatalf("changed reuse result = %+v", result)
	}
	for _, judge := range distillQualityPanelJudgesV1 {
		if _, err := os.Stat(filepath.Join(target, distillQualityPanelDirV1, string(judge)+".jsonl")); !os.IsNotExist(err) {
			t.Fatalf("changed packet unexpectedly got %s verdict file: err=%v", judge, err)
		}
	}
}

func TestDistillQualityRunManifestKeepsSealedOlderCandidateSchemaReadable(t *testing.T) {
	root, _ := adjudicationFixtureRunV1(t)
	data, err := os.ReadFile(filepath.Join(root, distillQualityRunManifestFileV1))
	if err != nil {
		t.Fatal(err)
	}
	var manifest distillQualityRunManifestV1
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.CandidateSchemaVersion = distillCandidateSchemaVersion - 1
	if err := validateDistillQualityRunManifestV1(manifest); err != nil {
		t.Fatalf("sealed previous candidate schema became unreadable: %v", err)
	}
	manifest.CandidateSchemaVersion = distillCandidateSchemaVersion + 1
	if err := validateDistillQualityRunManifestV1(manifest); err == nil {
		t.Fatal("future candidate schema was accepted")
	}
}

func TestDistillQualityDeltaQueueSelectsOnlyUnmatchedPackets(t *testing.T) {
	base, _ := adjudicationFixtureRunV1(t)
	target, targetPackets := adjudicationFixtureRunV1(t)
	if err := os.RemoveAll(filepath.Join(target, distillQualityPanelDirV1)); err != nil {
		t.Fatal(err)
	}
	targetPackets[0].Item.Text = `{"candidate_admitted":true,"stratum":"admitted:direct_user:rule","roles":["user"],"authorities":["direct_user"],"cues":["rule"],"evidence_bytes":12,"evidence_truncated":false}`
	targetPackets[0].Item.Digest = distillQualityPanelItemDigestV1(targetPackets[0].Item)
	targetPackets[0].PacketDigest = distillQualityPanelPacketDigestV1(targetPackets[0])
	writeReuseFixturePacketsV1(t, target, targetPackets)
	if _, err := reuseDistillQualityBundleV1(base, target); err != nil {
		t.Fatal(err)
	}
	session, err := openDistillQualityAdjudicationSessionV1(target, "reviewer", distillQualityAdjudicateQueueDeltaV1, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.close() }()
	if len(session.items) != 1 { // only the same-ID packet whose digest changed
		t.Fatalf("delta items = %d, want 1", len(session.items))
	}
	metadata, err := loadDistillQualityReuseMetadataV1(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range session.items {
		found := false
		for _, id := range metadata.DeltaPacketIDs {
			found = found || id == item.Packet.PacketID
		}
		if !found {
			t.Fatalf("delta queue returned reused packet %q", item.Packet.PacketID)
		}
	}
}

func TestReusedDistillQualityJudgeInvokesOnlyChangedPacket(t *testing.T) {
	base, _ := adjudicationFixtureRunV1(t)
	target, targetPackets := adjudicationFixtureRunV1(t)
	if err := os.RemoveAll(filepath.Join(target, distillQualityPanelDirV1)); err != nil {
		t.Fatal(err)
	}
	changed := &targetPackets[1]
	changed.Payload.Fragments[0].Text += " changed"
	changed.Payload.Fragments[0].Digest = distillQualityPanelFragmentDigestV1(changed.Payload.Fragments[0])
	changed.Payload.Digest = distillQualityPanelPayloadDigestV1(changed.Payload)
	changed.PacketDigest = distillQualityPanelPacketDigestV1(*changed)
	writeReuseFixturePacketsV1(t, target, targetPackets)
	if _, err := reuseDistillQualityBundleV1(base, target); err != nil {
		t.Fatal(err)
	}

	providerCalls := 0
	result, err := runDistillQualityJudgeWithInvokerV1(context.Background(), target, distillQualityJudgeClaudeV1, "claude-test", time.Minute, testReuseTimeV1, func(_ context.Context, _ string, judge distillQualityJudgeV1, model string, prompt []byte, _ time.Duration) (distillQualityJudgeProcessResultV1, error) {
		providerCalls++
		var request distillQualityJudgeBatchRequestV1
		if err := json.Unmarshal(prompt, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Packets) != 1 || request.Packets[0].PacketID != changed.PacketID {
			t.Fatalf("provider packets = %+v, want only %s", request.Packets, changed.PacketID)
		}
		var metadata struct {
			CandidateAdmitted bool `json:"candidate_admitted"`
		}
		if err := json.Unmarshal([]byte(changed.Item.Text), &metadata); err != nil {
			t.Fatal(err)
		}
		should := "no"
		if metadata.CandidateAdmitted {
			should = "yes"
		}
		verdict := distillQualityAdmissionVerdictForJudgeTestV1(*changed, judge, model, should, metadata.CandidateAdmitted)
		data, err := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
		if err != nil {
			t.Fatal(err)
		}
		return distillQualityJudgeProcessResultV1{Content: string(data)}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 1 || result.Completed != len(targetPackets) || result.ProviderCalls != 1 {
		t.Fatalf("provider calls=%d result=%+v", providerCalls, result)
	}
}

func writeReuseFixturePacketsV1(t *testing.T, root string, packets []distillQualityPanelPacketV1) {
	t.Helper()
	data, err := marshalDistillQualityJSONLV1(packets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, distillQualityPacketsFileV1), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, distillQualityRunManifestFileV1))
	if err != nil {
		t.Fatal(err)
	}
	var manifest distillQualityRunManifestV1
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.PacketsSHA256 = distillQualitySHA256V1(data)
	manifestBytes, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, distillQualityRunManifestFileV1), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testReuseTimeV1() (atTime time.Time) {
	return time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC)
}
