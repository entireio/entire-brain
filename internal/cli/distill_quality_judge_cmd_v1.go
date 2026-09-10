package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type distillQualityJudgeInvokerV1 func(context.Context, string, distillQualityJudgeV1, string, []byte, time.Duration) (distillQualityJudgeProcessResultV1, error)

func runDistillQualityJudgeV1(ctx context.Context, bundleDir string, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, recoverInvalid bool) (distillQualityJudgeResultV1, error) {
	return runDistillQualityJudgeWithConcurrencyV1(ctx, bundleDir, judge, model, timeout, now, recoverInvalid, 1)
}

func runDistillQualityJudgeWithInvokerV1(ctx context.Context, bundleDir string, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, invoke distillQualityJudgeInvokerV1, recoverInvalid bool) (distillQualityJudgeResultV1, error) {
	return runDistillQualityJudgeWithInvokerConcurrencyV1(ctx, bundleDir, judge, model, timeout, now, invoke, recoverInvalid, 1)
}

func runDistillQualityJudgeWithConcurrencyV1(ctx context.Context, bundleDir string, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, recoverInvalid bool, concurrency int) (distillQualityJudgeResultV1, error) {
	return runDistillQualityJudgeWithInvokerConcurrencyV1(ctx, bundleDir, judge, model, timeout, now, runDistillQualityJudgeProcessV1, recoverInvalid, concurrency)
}

func runDistillQualityJudgeWithInvokerConcurrencyV1(ctx context.Context, bundleDir string, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, invoke distillQualityJudgeInvokerV1, recoverInvalid bool, concurrency int) (distillQualityJudgeResultV1, error) {
	if !validDistillQualityJudgeV1(judge) {
		return distillQualityJudgeResultV1{}, fmt.Errorf("quality judge: unsupported judge %q", judge)
	}
	if concurrency < 1 || concurrency > distillQualityJudgeMaxConcurrencyV1 {
		return distillQualityJudgeResultV1{}, fmt.Errorf("quality judge: concurrency must be 1..%d", distillQualityJudgeMaxConcurrencyV1)
	}
	if strings.TrimSpace(model) == "" {
		model = defaultDistillQualityJudgeModelV1(judge)
	}
	if !validPanelWordV1(model) {
		return distillQualityJudgeResultV1{}, errors.New("quality judge: invalid model")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if now == nil {
		now = time.Now
	}
	if invoke == nil {
		return distillQualityJudgeResultV1{}, errors.New("quality judge: runner is required")
	}
	manifest, packets, _, err := loadDistillQualityRunV1(bundleDir)
	if err != nil {
		return distillQualityJudgeResultV1{}, err
	}
	bundleDir, err = validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return distillQualityJudgeResultV1{}, err
	}
	panelDir := filepath.Join(bundleDir, distillQualityPanelDirV1)
	if err := ensureDistillQualityPanelDirV1(panelDir); err != nil {
		return distillQualityJudgeResultV1{}, err
	}
	// Exact reused invalid/abstain verdicts retain terminal recovery lineage.
	// Calls are deliberately not copied, so this content-free metadata is the
	// only way to avoid granting a fresh retry budget after each rebase.
	terminalReuse := map[string]bool{}
	if metadata, metadataErr := loadDistillQualityReuseMetadataV1(bundleDir); metadataErr == nil {
		prefix := string(judge) + ":"
		for _, key := range metadata.RecoveryTerminalKeys {
			if strings.HasPrefix(key, prefix) {
				terminalReuse[strings.TrimPrefix(key, prefix)] = true
			}
		}
	}
	packetByID := make(map[string]distillQualityPanelPacketV1, len(packets))
	for _, packet := range packets {
		packetByID[packet.PacketID] = packet
	}
	verdicts, err := loadDistillQualityJudgeVerdictsV1(bundleDir, judge, packetByID)
	if err != nil {
		return distillQualityJudgeResultV1{}, err
	}
	for _, verdict := range verdicts {
		if verdict.Model != model {
			return distillQualityJudgeResultV1{}, fmt.Errorf("quality judge %s already has retained model %q, not %q", judge, verdict.Model, model)
		}
	}
	calls, err := loadDistillQualityJudgeCallsV1(bundleDir, judge, model, packetByID)
	if err != nil {
		return distillQualityJudgeResultV1{}, err
	}
	recoveryCallsByPacket := make(map[string]int)
	recoveryTerminal := make(map[string]bool)
	for _, verdict := range verdicts {
		if terminalReuse[verdict.PacketID] {
			recoveryTerminal[verdict.PacketID] = true
		}
	}
	for _, call := range calls {
		if !call.Recovery {
			continue
		}
		for _, id := range call.PacketIDs {
			recoveryCallsByPacket[id]++
			if call.Status == "complete" {
				recoveryTerminal[id] = true
			}
		}
	}
	for id, count := range recoveryCallsByPacket {
		if count >= 2 {
			recoveryTerminal[id] = true
		}
	}
	verdictByID := make(map[string]distillQualityPanelVerdictV1, len(verdicts))
	recoveryCandidateIDs := make(map[string]struct{})
	for _, verdict := range verdicts {
		if recoverInvalid && verdict.State == distillQualityVerdictInvalidV1 && !recoveryTerminal[verdict.PacketID] {
			recoveryCandidateIDs[verdict.PacketID] = struct{}{}
			continue
		}
		verdictByID[verdict.PacketID] = verdict
	}
	pending := make([]distillQualityPanelPacketV1, 0, len(packets)-len(verdicts))
	for _, packet := range packets {
		if _, done := verdictByID[packet.PacketID]; !done {
			pending = append(pending, packet)
		}
	}
	batches := make([]distillQualityJudgeBatchV1, 0, (len(pending)+distillQualityJudgeRunBatchLimitV1(judge, recoverInvalid)-1)/distillQualityJudgeRunBatchLimitV1(judge, recoverInvalid))
	for len(pending) > 0 {
		batchLimit := distillQualityJudgeRunBatchLimitV1(judge, recoverInvalid)
		batch, prompt, err := nextDistillQualityJudgeBatchWithLimitV1(judge, model, pending, batchLimit)
		if err != nil {
			return distillQualityJudgeResultV1{}, err
		}
		batches = append(batches, distillQualityJudgeBatchV1{Packets: batch, Prompt: prompt})
		pending = pending[len(batch):]
	}
	if err := runDistillQualityJudgeBatchesV1(ctx, concurrency, judge, model, timeout, now, invoke, recoverInvalid, batches, func(outcome distillQualityJudgeBatchOutcomeV1) error {
		calls = append(calls, outcome.Calls...)
		if err := saveDistillQualityJudgeCallsV1(bundleDir, judge, calls); err != nil {
			return err
		}
		for _, verdict := range outcome.Verdicts {
			verdictByID[verdict.PacketID] = verdict
		}
		verdicts = verdicts[:0]
		for _, verdict := range verdictByID {
			verdicts = append(verdicts, verdict)
		}
		return saveDistillQualityJudgeVerdictsV1(bundleDir, judge, verdicts)
	}); err != nil {
		return distillQualityJudgeResultV1{}, err
	}

	result := distillQualityJudgeResultV1{RunID: manifest.RunID, Judge: judge, Model: model, Packets: len(packets), ProviderCalls: len(calls), Recovery: recoverInvalid, Concurrency: concurrency}
	for _, verdict := range verdictByID {
		switch verdict.State {
		case distillQualityVerdictCompletedV1:
			result.Completed++
		case distillQualityVerdictAbstainV1:
			result.Abstained++
		case distillQualityVerdictInvalidV1:
			result.Invalid++
		}
	}
	if recoverInvalid {
		for id := range recoveryCandidateIDs {
			if verdict, ok := verdictByID[id]; ok && verdict.State != distillQualityVerdictInvalidV1 {
				result.Recovered++
			}
		}
	}
	for _, call := range calls {
		if call.Retry > 0 {
			result.Retries++
		}
		result.InputTokens += call.InputTokens
		result.OutputTokens += call.OutputTokens
	}
	return result, nil
}

func distillQualityJudgeRunBatchLimitV1(judge distillQualityJudgeV1, recoverInvalid bool) int {
	if recoverInvalid || judge == distillQualityJudgeClaudeV1 {
		// Claude completed large structured responses within the deadline but
		// repeatedly omitted or corrupted a member identity. Eight-member
		// batches passed the same contract and keep failures attributable.
		return 8
	}
	return distillQualityJudgeMaxBatchItemsV1
}

func nextDistillQualityJudgeBatchV1(judge distillQualityJudgeV1, model string, pending []distillQualityPanelPacketV1) ([]distillQualityPanelPacketV1, []byte, error) {
	return nextDistillQualityJudgeBatchWithLimitV1(judge, model, pending, distillQualityJudgeMaxBatchItemsV1)
}

func nextDistillQualityJudgeBatchWithLimitV1(judge distillQualityJudgeV1, model string, pending []distillQualityPanelPacketV1, maxItems int) ([]distillQualityPanelPacketV1, []byte, error) {
	if len(pending) == 0 {
		return nil, nil, errors.New("quality judge: no pending packets")
	}
	if maxItems <= 0 || maxItems > distillQualityJudgeMaxBatchItemsV1 {
		return nil, nil, errors.New("quality judge: invalid batch limit")
	}
	limit := len(pending)
	if limit > maxItems {
		limit = maxItems
	}
	var chosen []distillQualityPanelPacketV1
	var prompt []byte
	for count := 1; count <= limit; count++ {
		candidate, err := buildDistillQualityJudgePromptV1(judge, model, pending[:count])
		if err != nil {
			if count == 1 {
				return nil, nil, err
			}
			break
		}
		chosen = pending[:count]
		prompt = candidate
	}
	return chosen, prompt, nil
}

func invalidDistillQualityJudgeVerdictV1(packet distillQualityPanelPacketV1, judge distillQualityJudgeV1, model string) distillQualityPanelVerdictV1 {
	return distillQualityPanelVerdictV1{
		SchemaVersion: distillQualityPanelSchemaVersion,
		PacketID:      packet.PacketID, PacketDigest: packet.PacketDigest,
		ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest,
		Judge: judge, Model: model, State: distillQualityVerdictInvalidV1,
		Rationale: "transport_or_schema_failure_after_one_retry",
	}
}

func defaultDistillQualityJudgeModelV1(judge distillQualityJudgeV1) string {
	switch judge {
	case distillQualityJudgeClaudeV1:
		return "claude-haiku-4-5"
	case distillQualityJudgeCursorV1:
		return "composer-2.5"
	case distillQualityJudgeCopilotV1:
		return "gpt-5.4"
	default:
		return ""
	}
}

func distillQualityPacketIDsV1(packets []distillQualityPanelPacketV1) []string {
	ids := make([]string, len(packets))
	for index, packet := range packets {
		ids[index] = packet.PacketID
	}
	return ids
}

func ensureDistillQualityPanelDirV1(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("quality panel path must be a real directory")
	}
	return os.Chmod(path, 0o700)
}

func loadDistillQualityJudgeCallsV1(bundleDir string, judge distillQualityJudgeV1, model string, packets map[string]distillQualityPanelPacketV1) ([]distillQualityJudgeCallV1, error) {
	data, err := readDistillQualityFileV1(filepath.Join(bundleDir, distillQualityPanelDirV1), string(judge)+"-calls.jsonl", distillQualityCallsMaxBytesV1, false)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), distillQualityPanelMaxJSONBytes)
	var calls []distillQualityJudgeCallV1
	for line := 1; scanner.Scan(); line++ {
		var call distillQualityJudgeCallV1
		if err := decodeDistillQualityStrictJSONV1(scanner.Bytes(), &call); err != nil {
			return nil, fmt.Errorf("quality judge call line %d: %w", line, err)
		}
		if call.Judge != judge || call.Model != model || call.StartedAt == "" || call.InputBytes <= 0 || len(call.PacketIDs) == 0 || len(call.PacketIDs) > distillQualityJudgeMaxBatchItemsV1 || (call.Status != "complete" && call.Status != "schema_error" && call.Status != "transport_error") || call.Retry < 0 || call.Retry > 1 || len(call.ValidationError) > 1024 || strings.ContainsRune(call.ValidationError, 0) {
			return nil, fmt.Errorf("quality judge call line %d has invalid metadata", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, call.StartedAt); err != nil {
			return nil, fmt.Errorf("quality judge call line %d has invalid time", line)
		}
		for _, id := range call.PacketIDs {
			if _, ok := packets[id]; !ok {
				return nil, fmt.Errorf("quality judge call line %d references unknown packet", line)
			}
		}
		if call.ResponseSHA256 != "" && !validSHA256Identity(call.ResponseSHA256) {
			return nil, fmt.Errorf("quality judge call line %d has invalid response digest", line)
		}
		calls = append(calls, call)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return calls, nil
}

func boundedDistillQualityValidationErrorV1(err error) string {
	if err == nil {
		return ""
	}
	value := strings.Join(strings.Fields(err.Error()), " ")
	if len(value) > 1024 {
		value = value[:1024]
	}
	return value
}

func saveDistillQualityJudgeCallsV1(bundleDir string, judge distillQualityJudgeV1, calls []distillQualityJudgeCallV1) error {
	data, err := marshalDistillQualityJSONLV1(calls)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(bundleDir, distillQualityPanelDirV1, string(judge)+"-calls.jsonl"), data, 0o600)
}
