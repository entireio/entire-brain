package cli

// This file implements the bounded, local-only bridge between two sealed
// Phase 2 quality bundles.  It deliberately copies verdicts and human labels
// only when the cryptographic packet identity is unchanged.  Provider call
// telemetry is never part of the bridge: a new run must report only calls
// actually made for that run.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	distillQualityReuseFileV1     = "reuse-delta.json"
	distillQualityReuseContractV1 = "phase2_distill_quality_reuse_v1"
	distillQualityReuseMaxBytesV1 = 1 << 20
)

// distillQualityReuseMetadataV1 is intentionally content-free.  It is an
// audit index, not a copy of the source bundle or its local source map.
type distillQualityReuseMetadataV1 struct {
	Contract             string   `json:"contract"`
	SchemaVersion        int      `json:"schema_version"`
	BaseRunID            string   `json:"base_run_id"`
	BaseManifestSHA256   string   `json:"base_manifest_sha256"`
	BaseBundleSHA256     string   `json:"base_bundle_sha256"`
	BasePacketsSHA256    string   `json:"base_packets_sha256"`
	BasePacketCount      int      `json:"base_packet_count"`
	TargetRunID          string   `json:"target_run_id"`
	TargetPacketCount    int      `json:"target_packet_count"`
	ReusedPacketIDs      []string `json:"reused_packet_ids"`
	DeltaPacketIDs       []string `json:"delta_packet_ids"`
	MigratedHumanRecords int      `json:"migrated_human_records"`
	// RecoveryTerminalKeys records unchanged invalid/abstain verdict lineage as
	// judge:packet-id. It carries no provider telemetry and prevents a rebase
	// from replenishing the bounded recovery allowance.
	RecoveryTerminalKeys []string `json:"recovery_terminal_keys,omitempty"`
}

type distillQualityReuseResultV1 struct {
	Metadata               distillQualityReuseMetadataV1
	ReusedVerdicts         int
	CarriedInvalidVerdicts int
	DeltaPackets           int
	MigratedHumanRecords   int
}

type distillQualityReusePacketIdentityV1 struct {
	PacketID      string
	PacketDigest  string
	ItemDigest    string
	PromptDigest  string
	PayloadDigest string
}

func makeDistillQualityReusePacketIdentityV1(packet distillQualityPanelPacketV1) distillQualityReusePacketIdentityV1 {
	return distillQualityReusePacketIdentityV1{
		PacketID: packet.PacketID, PacketDigest: packet.PacketDigest,
		ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest,
		PayloadDigest: packet.Payload.Digest,
	}
}

func distillQualityReuseVerdictIdentityV1(verdict distillQualityPanelVerdictV1) distillQualityReusePacketIdentityV1 {
	return distillQualityReusePacketIdentityV1{
		PacketID: verdict.PacketID, PacketDigest: verdict.PacketDigest,
		ItemDigest: verdict.ItemDigest, PromptDigest: verdict.PromptDigest,
		PayloadDigest: verdict.PayloadDigest,
	}
}

func distillQualityReuseIdentityEqualV1(left, right distillQualityReusePacketIdentityV1) bool {
	return left == right
}

// reuseDistillQualityBundleV1 validates both sealed runs, then copies only
// exact packet matches.  The returned metadata can be used to route a delta
// adjudication without exposing source paths or transcript content.
func reuseDistillQualityBundleV1(baseDir, targetDir string) (distillQualityReuseResultV1, error) {
	var result distillQualityReuseResultV1
	baseManifest, basePackets, _, err := loadDistillQualityRunV1(baseDir)
	if err != nil {
		return result, fmt.Errorf("quality reuse base bundle: %w", err)
	}
	targetManifest, targetPackets, _, err := loadDistillQualityRunV1(targetDir)
	if err != nil {
		return result, fmt.Errorf("quality reuse target bundle: %w", err)
	}
	baseAbs, err := validateDistillQualityBundleDirV1(baseDir)
	if err != nil {
		return result, err
	}
	targetAbs, err := validateDistillQualityBundleDirV1(targetDir)
	if err != nil {
		return result, err
	}
	if baseAbs == targetAbs {
		return result, errors.New("quality reuse base and target bundles must differ")
	}
	baseManifestBytes, err := readDistillQualityFileV1(baseAbs, distillQualityRunManifestFileV1, distillQualityManifestMaxBytesV1, true)
	if err != nil {
		return result, err
	}

	baseByID := make(map[string]distillQualityPanelPacketV1, len(basePackets))
	targetByID := make(map[string]distillQualityPanelPacketV1, len(targetPackets))
	for _, packet := range basePackets {
		baseByID[packet.PacketID] = packet
	}
	for _, packet := range targetPackets {
		targetByID[packet.PacketID] = packet
	}

	// A packet is reusable only if all five identity fields match.  A shared
	// packet ID with even one changed digest is delta work.
	var reusedIDs, deltaIDs []string
	var recoveryTerminalKeys []string
	for _, packet := range targetPackets {
		base, ok := baseByID[packet.PacketID]
		if ok && distillQualityReuseIdentityEqualV1(makeDistillQualityReusePacketIdentityV1(packet), makeDistillQualityReusePacketIdentityV1(base)) {
			reusedIDs = append(reusedIDs, packet.PacketID)
		} else {
			deltaIDs = append(deltaIDs, packet.PacketID)
		}
	}
	sort.Strings(reusedIDs)
	sort.Strings(deltaIDs)

	if err := ensureDistillQualityPanelDirV1(filepath.Join(targetAbs, distillQualityPanelDirV1)); err != nil {
		return result, err
	}
	for _, judge := range distillQualityPanelJudgesV1 {
		oldVerdicts, err := loadDistillQualityJudgeVerdictsV1(baseAbs, judge, baseByID)
		if err != nil {
			return result, fmt.Errorf("quality reuse %s: %w", judge, err)
		}
		var copied []distillQualityPanelVerdictV1
		for _, verdict := range oldVerdicts {
			basePacket := baseByID[verdict.PacketID]
			targetPacket, ok := targetByID[verdict.PacketID]
			if !ok || !distillQualityReuseIdentityEqualV1(distillQualityReuseVerdictIdentityV1(verdict), makeDistillQualityReusePacketIdentityV1(basePacket)) || !distillQualityReuseIdentityEqualV1(distillQualityReuseVerdictIdentityV1(verdict), makeDistillQualityReusePacketIdentityV1(targetPacket)) {
				continue
			}
			if err := validateDistillQualityPanelVerdictV1(targetPacket, verdict); err != nil {
				return result, fmt.Errorf("quality reuse %s packet %s: %w", judge, verdict.PacketID, err)
			}
			copied = append(copied, verdict)
			if verdict.State == distillQualityVerdictCompletedV1 {
				result.ReusedVerdicts++
			} else {
				result.CarriedInvalidVerdicts++
			}
			if verdict.State == distillQualityVerdictInvalidV1 || verdict.State == distillQualityVerdictAbstainV1 {
				recoveryTerminalKeys = append(recoveryTerminalKeys, string(judge)+":"+verdict.PacketID)
			}
		}
		if len(copied) > 0 {
			if err := saveDistillQualityJudgeVerdictsV1(targetAbs, judge, copied); err != nil {
				return result, err
			}
		}
	}

	// Human records are migrated file-by-file.  Records whose packet changed,
	// or whose recomputed advisory aggregate changed, are intentionally omitted.
	migrated, err := migrateDistillQualityHumanV1(baseAbs, targetAbs, basePackets, targetPackets)
	if err != nil {
		return result, err
	}
	result.MigratedHumanRecords = migrated
	sort.Strings(recoveryTerminalKeys)
	result.Metadata = distillQualityReuseMetadataV1{
		Contract: distillQualityReuseContractV1, SchemaVersion: 1,
		BaseRunID:          baseManifest.RunID,
		BaseManifestSHA256: distillQualitySHA256V1(baseManifestBytes),
		BaseBundleSHA256:   baseManifest.BundleSHA256, BasePacketsSHA256: baseManifest.PacketsSHA256,
		BasePacketCount: len(basePackets), TargetRunID: targetManifest.RunID,
		TargetPacketCount: len(targetPackets), ReusedPacketIDs: reusedIDs, DeltaPacketIDs: deltaIDs,
		MigratedHumanRecords: migrated,
		RecoveryTerminalKeys: recoveryTerminalKeys,
	}
	data, err := json.MarshalIndent(result.Metadata, "", "  ")
	if err != nil {
		return result, err
	}
	if err := writeFileAtomic(filepath.Join(targetAbs, distillQualityReuseFileV1), append(data, '\n'), 0o600); err != nil {
		return result, err
	}
	result.DeltaPackets = len(deltaIDs)
	return result, nil
}

func migrateDistillQualityHumanV1(baseDir, targetDir string, basePackets, targetPackets []distillQualityPanelPacketV1) (int, error) {
	_, _, _, baseAggregates, err := loadDistillQualityAdjudicationInputsV1(baseDir)
	if err != nil {
		return 0, fmt.Errorf("quality reuse human base: %w", err)
	}
	targetManifest, _, _, targetAggregates, err := loadDistillQualityAdjudicationInputsV1(targetDir)
	if err != nil {
		return 0, fmt.Errorf("quality reuse human target: %w", err)
	}
	baseByID := make(map[string]distillQualityPanelPacketV1, len(basePackets))
	targetByID := make(map[string]distillQualityPanelPacketV1, len(targetPackets))
	for _, packet := range basePackets {
		baseByID[packet.PacketID] = packet
	}
	for _, packet := range targetPackets {
		targetByID[packet.PacketID] = packet
	}
	baseHumanDir := filepath.Join(baseDir, distillQualityHumanDirV1)
	entries, err := os.ReadDir(baseHumanDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	targetHumanDir := filepath.Join(targetDir, distillQualityHumanDirV1)
	if err := ensureDistillQualityPanelDirV1(targetHumanDir); err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		adjudicator := strings.TrimSuffix(entry.Name(), ".jsonl")
		if err := validateDistillQualityPanelOpaqueIDV1("adjudicator_id", adjudicator, distillQualityPanelMaxOpaqueIDBytes); err != nil {
			return 0, err
		}
		records, err := loadDistillQualityHumanAdjudicationsV1(baseDir, adjudicator, basePackets, baseAggregates)
		if err != nil {
			return 0, err
		}
		var migrated []distillQualityHumanAdjudicationV1
		for _, record := range records {
			basePacket, ok := baseByID[record.PacketID]
			targetPacket, targetOK := targetByID[record.PacketID]
			if !ok || !targetOK || !distillQualityReuseIdentityEqualV1(makeDistillQualityReusePacketIdentityV1(basePacket), makeDistillQualityReusePacketIdentityV1(targetPacket)) {
				continue
			}
			oldAgg, oldOK := baseAggregates[record.PacketID]
			newAgg, newOK := targetAggregates[record.PacketID]
			if !oldOK || !newOK || !reflect.DeepEqual(oldAgg, newAgg) || record.AdvisoryDigest != distillQualityPanelAggregateDigestV1(newAgg) {
				continue
			}
			record.PacketDigest = targetPacket.PacketDigest
			record.ItemDigest = targetPacket.Item.Digest
			record.PromptDigest = targetPacket.Prompt.Digest
			record.PayloadDigest = targetPacket.Payload.Digest
			record.RecordID = distillQualityHumanRecordIDV1(targetManifest.RunID, record.AdjudicatorID, record.PacketID)
			if err := validateDistillQualityHumanAdjudicationV1(targetPacket, newAgg, record); err != nil {
				return 0, err
			}
			migrated = append(migrated, record)
		}
		if len(migrated) > 0 {
			if err := saveDistillQualityHumanAdjudicationsV1(filepath.Join(targetHumanDir, entry.Name()), migrated); err != nil {
				return 0, err
			}
			count += len(migrated)
		}
	}
	return count, nil
}

func distillQualityHumanRecordIDV1(runID, adjudicatorID, packetID string) string {
	identity := distillQualitySHA256V1([]byte(strings.Join([]string{runID, adjudicatorID, packetID}, "\x00")))
	return "human-v1:" + strings.TrimPrefix(identity, "sha256:")
}

func loadDistillQualityReuseMetadataV1(bundleDir string) (distillQualityReuseMetadataV1, error) {
	var metadata distillQualityReuseMetadataV1
	root, err := validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return metadata, err
	}
	data, err := readDistillQualityFileV1(root, distillQualityReuseFileV1, distillQualityReuseMaxBytesV1, true)
	if err != nil {
		return metadata, err
	}
	if err := decodeDistillQualityStrictJSONV1(data, &metadata); err != nil {
		return metadata, err
	}
	if metadata.Contract != distillQualityReuseContractV1 || metadata.SchemaVersion != 1 || !validPanelWordV1(metadata.BaseRunID) || !validPanelWordV1(metadata.TargetRunID) || !validSHA256Identity(metadata.BaseManifestSHA256) || !validSHA256Identity(metadata.BaseBundleSHA256) || !validSHA256Identity(metadata.BasePacketsSHA256) || metadata.BasePacketCount < 1 || metadata.TargetPacketCount < 1 || metadata.MigratedHumanRecords < 0 {
		return metadata, errors.New("quality reuse metadata is invalid")
	}
	if err := validateDistillQualityReuseIDsV1(metadata.ReusedPacketIDs); err != nil {
		return metadata, err
	}
	if err := validateDistillQualityReuseIDsV1(metadata.DeltaPacketIDs); err != nil {
		return metadata, err
	}
	reused := make(map[string]struct{}, len(metadata.ReusedPacketIDs))
	for _, id := range metadata.ReusedPacketIDs {
		reused[id] = struct{}{}
	}
	for _, id := range metadata.DeltaPacketIDs {
		if _, ok := reused[id]; ok {
			return metadata, errors.New("quality reuse metadata reuses a packet in both sets")
		}
	}
	if len(metadata.ReusedPacketIDs)+len(metadata.DeltaPacketIDs) != metadata.TargetPacketCount {
		return metadata, errors.New("quality reuse metadata packet counts do not match")
	}
	return metadata, nil
}

func validateDistillQualityReuseIDsV1(ids []string) error {
	for i, id := range ids {
		if err := validateDistillQualityPanelOpaqueIDV1("packet_id", id, distillQualityPanelMaxPacketIDBytes); err != nil {
			return err
		}
		if i > 0 && ids[i-1] >= id {
			return errors.New("quality reuse packet IDs must be sorted uniquely")
		}
	}
	return nil
}

// validateDistillQualityReuseTargetV1 binds the post-seal sidecar back to the
// exact target packet set before it is allowed to route human work.
func validateDistillQualityReuseTargetV1(metadata distillQualityReuseMetadataV1, manifest distillQualityRunManifestV1, packets []distillQualityPanelPacketV1) error {
	if metadata.TargetRunID != manifest.RunID || metadata.TargetPacketCount != len(packets) {
		return errors.New("reuse metadata targets another run")
	}
	actual := make(map[string]struct{}, len(packets))
	for _, packet := range packets {
		actual[packet.PacketID] = struct{}{}
	}
	seen := make(map[string]struct{}, metadata.TargetPacketCount)
	for _, ids := range [][]string{metadata.ReusedPacketIDs, metadata.DeltaPacketIDs} {
		for _, id := range ids {
			if _, ok := actual[id]; !ok {
				return fmt.Errorf("reuse metadata references unknown target packet %q", id)
			}
			if _, ok := seen[id]; ok {
				return fmt.Errorf("reuse metadata repeats target packet %q", id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(seen) != len(actual) {
		return errors.New("reuse metadata does not partition the target packet set")
	}
	return nil
}
