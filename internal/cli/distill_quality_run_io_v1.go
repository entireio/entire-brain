package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	distillQualityManifestMaxBytesV1 = 1 << 20
	distillQualityBundleMaxBytesV1   = 40 << 20
	distillQualityPacketsMaxBytesV1  = 40 << 20
	distillQualitySourcesMaxBytesV1  = 64 << 20
	distillQualityVerdictsMaxBytesV1 = 64 << 20
	distillQualityCallsMaxBytesV1    = 16 << 20
)

func loadDistillQualityRunV1(bundleDir string) (distillQualityRunManifestV1, []distillQualityPanelPacketV1, []distillQualityPrivateSourceRecordV1, error) {
	var zero distillQualityRunManifestV1
	dir, err := validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return zero, nil, nil, err
	}
	manifestBytes, err := readDistillQualityFileV1(dir, distillQualityRunManifestFileV1, distillQualityManifestMaxBytesV1, true)
	if err != nil {
		return zero, nil, nil, err
	}
	var manifest distillQualityRunManifestV1
	if err := decodeDistillQualityStrictJSONV1(manifestBytes, &manifest); err != nil {
		return zero, nil, nil, fmt.Errorf("quality run manifest: %w", err)
	}
	if err := validateDistillQualityRunManifestV1(manifest); err != nil {
		return zero, nil, nil, err
	}
	bundleBytes, err := readDistillQualityFileV1(dir, distillQualityBundleFileV1, distillQualityBundleMaxBytesV1, true)
	if err != nil {
		return zero, nil, nil, err
	}
	if distillQualitySHA256V1(bundleBytes) != manifest.BundleSHA256 {
		return zero, nil, nil, errors.New("quality run bundle digest mismatch")
	}
	var bundle distillQualityBundleV1
	if err := decodeDistillQualityStrictJSONV1(bundleBytes, &bundle); err != nil {
		return zero, nil, nil, fmt.Errorf("quality run bundle: %w", err)
	}
	if bundle.SchemaVersion != distillQualityBundleSchemaVersionV1 || bundle.RunID != manifest.RunID || bundle.RunID != distillQualityBundleRunIDV1(bundle.Admission, bundle.Filtered) {
		return zero, nil, nil, errors.New("quality run bundle identity mismatch")
	}
	if err := validateDistillQualityPublicV1(bundle, nil, distillQualityBundleDefaultRunBytesV1, distillQualityBundleDefaultItemBytesV1); err != nil {
		return zero, nil, nil, err
	}
	if len(bundle.Admission) != manifest.AdmittedItems || len(bundle.Filtered) != manifest.FilteredItems {
		return zero, nil, nil, errors.New("quality run bundle counts do not match manifest")
	}
	packetBytes, err := readDistillQualityFileV1(dir, distillQualityPacketsFileV1, distillQualityPacketsMaxBytesV1, true)
	if err != nil {
		return zero, nil, nil, err
	}
	if distillQualitySHA256V1(packetBytes) != manifest.PacketsSHA256 {
		return zero, nil, nil, errors.New("quality run packets digest mismatch")
	}
	packets, err := decodeDistillQualityPacketsJSONLV1(packetBytes)
	if err != nil {
		return zero, nil, nil, err
	}
	if len(packets) != manifest.AdmittedItems+manifest.FilteredItems {
		return zero, nil, nil, errors.New("quality run packet count does not match manifest")
	}
	if len(packets) == 0 || packets[0].Prompt.Digest != manifest.PromptSHA256 {
		return zero, nil, nil, errors.New("quality run prompt digest mismatch")
	}
	for _, packet := range packets {
		if packet.Prompt.Digest != manifest.PromptSHA256 {
			return zero, nil, nil, errors.New("quality run contains mixed prompt digests")
		}
	}
	sourceBytes, err := readDistillQualityFileV1(dir, distillQualitySourceMapFileV1, distillQualitySourcesMaxBytesV1, true)
	if err != nil {
		return zero, nil, nil, err
	}
	if distillQualitySHA256V1(sourceBytes) != manifest.PrivateSourceMapSHA256 {
		return zero, nil, nil, errors.New("quality run private source-map digest mismatch")
	}
	sources, err := decodeDistillQualitySourcesJSONLV1(sourceBytes)
	if err != nil {
		return zero, nil, nil, err
	}
	if len(sources) != manifest.PrivateSourceRecords {
		return zero, nil, nil, errors.New("quality run private source count does not match manifest")
	}
	itemIDs := make(map[string]bool, len(packets))
	for _, packet := range packets {
		itemIDs[packet.Item.ID] = true
	}
	seenSources := make(map[string]bool, len(itemIDs))
	for _, source := range sources {
		if !itemIDs[source.PublicID] {
			return zero, nil, nil, fmt.Errorf("quality source map references unknown public item %q", source.PublicID)
		}
		seenSources[source.PublicID] = true
	}
	for id := range itemIDs {
		if !seenSources[id] {
			return zero, nil, nil, fmt.Errorf("quality item %q has no private source mapping", id)
		}
	}
	return manifest, packets, sources, nil
}

func validateDistillQualityRunManifestV1(manifest distillQualityRunManifestV1) error {
	if manifest.Contract != distillQualityRunContractV1 || manifest.SchemaVersion != 1 {
		return errors.New("quality run manifest has unsupported contract or schema")
	}
	if !validPanelWordV1(manifest.RunID) || len(manifest.RunID) > distillQualityPanelMaxOpaqueIDBytes {
		return errors.New("quality run manifest has invalid run_id")
	}
	if _, err := time.Parse(time.RFC3339, manifest.GeneratedAt); err != nil {
		return fmt.Errorf("quality run manifest has invalid generated_at: %w", err)
	}
	if strings.TrimSpace(manifest.EvaluatorRevision) == "" || len(manifest.EvaluatorRevision) > 128 || strings.TrimSpace(manifest.SourceRevision) == "" || len(manifest.SourceRevision) > 128 {
		return errors.New("quality run manifest lacks bounded evaluator/source revisions")
	}
	for field, value := range map[string]string{
		"brain_manifest_sha256":     manifest.BrainManifestSHA256,
		"corpus_sha256":             manifest.CorpusSHA256,
		"bundle_sha256":             manifest.BundleSHA256,
		"packets_sha256":            manifest.PacketsSHA256,
		"private_source_map_sha256": manifest.PrivateSourceMapSHA256,
		"prompt_sha256":             manifest.PromptSHA256,
	} {
		if !validSHA256Identity(value) {
			return fmt.Errorf("quality run manifest has invalid %s", field)
		}
	}
	if manifest.CandidateSchemaVersion != distillCandidateSchemaVersion || manifest.RedactionVersion != distillCandidateRedactionVersionV2 {
		return errors.New("quality run manifest candidate or redaction version mismatch")
	}
	if manifest.SessionViews <= 0 || manifest.AdmittedItems < 0 || manifest.FilteredItems <= 0 || manifest.FilteredRequested <= 0 || manifest.FilteredItems > manifest.FilteredRequested || manifest.PrivateSourceRecords < manifest.AdmittedItems+manifest.FilteredItems {
		return errors.New("quality run manifest has invalid counts")
	}
	if manifest.HumanLabels || !manifest.AdvisoryOnly {
		return errors.New("quality run manifest must remain advisory and unlabeled")
	}
	return nil
}

func decodeDistillQualityPacketsJSONLV1(data []byte) ([]distillQualityPanelPacketV1, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), distillQualityPanelMaxJSONBytes)
	seen := map[string]bool{}
	var packets []distillQualityPanelPacketV1
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, fmt.Errorf("quality packets line %d is empty", line)
		}
		packet, err := decodeDistillQualityPanelPacketJSONV1(scanner.Bytes())
		if err != nil {
			return nil, fmt.Errorf("quality packets line %d: %w", line, err)
		}
		if seen[packet.PacketID] {
			return nil, fmt.Errorf("quality packets contain duplicate %q", packet.PacketID)
		}
		seen[packet.PacketID] = true
		packets = append(packets, packet)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("quality packets: %w", err)
	}
	if len(packets) == 0 {
		return nil, errors.New("quality packets are empty")
	}
	sort.Slice(packets, func(i, j int) bool { return packets[i].PacketID < packets[j].PacketID })
	return packets, nil
}

func decodeDistillQualitySourcesJSONLV1(data []byte) ([]distillQualityPrivateSourceRecordV1, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), distillQualityPanelMaxJSONBytes)
	var sources []distillQualityPrivateSourceRecordV1
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, fmt.Errorf("quality source-map line %d is empty", line)
		}
		var source distillQualityPrivateSourceRecordV1
		if err := decodeDistillQualityStrictJSONV1(scanner.Bytes(), &source); err != nil {
			return nil, fmt.Errorf("quality source-map line %d: %w", line, err)
		}
		if source.PublicID == "" || source.SessionID == "" || source.Branch == "" || source.TranscriptPath == "" || len(source.TurnIDs) == 0 || source.StartLine <= 0 || source.EndLine < source.StartLine {
			return nil, fmt.Errorf("quality source-map line %d has invalid source identity", line)
		}
		if filepath.IsAbs(source.TranscriptPath) || filepath.Clean(source.TranscriptPath) != source.TranscriptPath || strings.HasPrefix(source.TranscriptPath, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("quality source-map line %d has unsafe transcript path", line)
		}
		sources = append(sources, source)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("quality source-map: %w", err)
	}
	return sources, nil
}

func loadDistillQualityJudgeVerdictsV1(bundleDir string, judge distillQualityJudgeV1, packets map[string]distillQualityPanelPacketV1) ([]distillQualityPanelVerdictV1, error) {
	data, err := readDistillQualityFileV1(filepath.Join(bundleDir, distillQualityPanelDirV1), string(judge)+".jsonl", distillQualityVerdictsMaxBytesV1, false)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), distillQualityPanelMaxJSONBytes)
	seen := map[string]bool{}
	var verdicts []distillQualityPanelVerdictV1
	for line := 1; scanner.Scan(); line++ {
		var verdict distillQualityPanelVerdictV1
		if err := decodeDistillQualityStrictJSONV1(scanner.Bytes(), &verdict); err != nil {
			return nil, fmt.Errorf("quality %s verdict line %d: %w", judge, line, err)
		}
		packet, ok := packets[verdict.PacketID]
		if !ok {
			return nil, fmt.Errorf("quality %s verdict references unknown packet %q", judge, verdict.PacketID)
		}
		if verdict.Judge != judge {
			return nil, fmt.Errorf("quality %s verdict has mismatched judge", judge)
		}
		if err := validateDistillQualityPanelVerdictV1(packet, verdict); err != nil {
			return nil, err
		}
		if err := validateDistillQualityAdmissionVerdictV1(packet, verdict); err != nil {
			return nil, err
		}
		if seen[verdict.PacketID] {
			return nil, fmt.Errorf("quality %s verdicts contain duplicate packet %q", judge, verdict.PacketID)
		}
		seen[verdict.PacketID] = true
		verdicts = append(verdicts, verdict)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].PacketID < verdicts[j].PacketID })
	return verdicts, nil
}

func saveDistillQualityJudgeVerdictsV1(bundleDir string, judge distillQualityJudgeV1, verdicts []distillQualityPanelVerdictV1) error {
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].PacketID < verdicts[j].PacketID })
	data, err := marshalDistillQualityJSONLV1(verdicts)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(bundleDir, distillQualityPanelDirV1, string(judge)+".jsonl"), data, 0o600)
}

func readDistillQualityFileV1(dir, name string, maxBytes int64, required bool) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("quality file %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("quality file %s is unsafe or exceeds %d bytes", name, maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("quality file %s changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("quality file %s exceeds %d bytes", name, maxBytes)
	}
	return data, nil
}

func validateDistillQualityBundleDirV1(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("quality bundle directory is required")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("quality bundle path must be a real directory")
	}
	return abs, nil
}

func decodeDistillQualityStrictJSONV1(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}
