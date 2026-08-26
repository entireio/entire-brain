package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	distillQualityJudgeBatchContractV1    = "phase2_judge_batch_v1"
	distillQualityJudgeResponseContractV1 = "phase2_judge_batch_response_v1"
	distillQualityJudgeMaxBatchItemsV1    = 32
	distillQualityJudgeMaxPromptBytesV1   = 256 << 10
	distillQualityJudgeMaxOutputBytesV1   = 8 << 20
	distillQualityJudgeMaxErrorBytesV1    = 64 << 10
)

type distillQualityJudgeBatchRequestV1 struct {
	Contract     string                            `json:"contract"`
	Judge        distillQualityJudgeV1             `json:"judge"`
	Model        string                            `json:"model"`
	Instructions string                            `json:"instructions"`
	Packets      []distillQualityJudgeWirePacketV1 `json:"packets"`
}

type distillQualityJudgeWirePacketV1 struct {
	PacketID     string                       `json:"packet_id"`
	PacketDigest string                       `json:"packet_digest"`
	Item         distillQualityPanelItemV1    `json:"item"`
	PromptDigest string                       `json:"prompt_digest"`
	Payload      distillQualityPanelPayloadV1 `json:"payload"`
}

type distillQualityJudgeBatchResponseV1 struct {
	Contract string                         `json:"contract"`
	Judge    distillQualityJudgeV1          `json:"judge,omitempty"`
	Model    string                         `json:"model,omitempty"`
	Verdicts []distillQualityPanelVerdictV1 `json:"verdicts"`
}

type distillQualityJudgeCallV1 struct {
	Judge               distillQualityJudgeV1 `json:"judge"`
	Model               string                `json:"model"`
	StartedAt           string                `json:"started_at"`
	DurationMS          int64                 `json:"duration_ms"`
	InputBytes          int                   `json:"input_bytes"`
	OutputBytes         int                   `json:"output_bytes"`
	InputTokens         int64                 `json:"input_tokens,omitempty"`
	OutputTokens        int64                 `json:"output_tokens,omitempty"`
	UsageReported       bool                  `json:"usage_reported"`
	Retry               int                   `json:"retry"`
	Recovery            bool                  `json:"recovery,omitempty"`
	Status              string                `json:"status"`
	ResponseSHA256      string                `json:"response_sha256,omitempty"`
	ValidationError     string                `json:"validation_error,omitempty"`
	TransportNormalized bool                  `json:"transport_normalized,omitempty"`
	PacketIDs           []string              `json:"packet_ids"`
}

type distillQualityJudgeProcessResultV1 struct {
	Content      string
	Model        string
	InputTokens  int64
	OutputTokens int64
	UsageKnown   bool
}

func buildDistillQualityJudgePromptV1(judge distillQualityJudgeV1, model string, packets []distillQualityPanelPacketV1) ([]byte, error) {
	if !validDistillQualityJudgeV1(judge) || !validPanelWordV1(model) {
		return nil, errors.New("quality judge: invalid judge or model")
	}
	if len(packets) == 0 || len(packets) > distillQualityJudgeMaxBatchItemsV1 {
		return nil, fmt.Errorf("quality judge: packet count must be 1..%d", distillQualityJudgeMaxBatchItemsV1)
	}
	instructions := packets[0].Prompt.Instructions
	promptDigest := packets[0].Prompt.Digest
	request := distillQualityJudgeBatchRequestV1{
		Contract:     distillQualityJudgeBatchContractV1,
		Judge:        judge,
		Model:        model,
		Instructions: instructions,
		Packets:      make([]distillQualityJudgeWirePacketV1, 0, len(packets)),
	}
	seen := make(map[string]struct{}, len(packets))
	for _, packet := range packets {
		if err := validateDistillQualityPanelPacketV1(packet); err != nil {
			return nil, err
		}
		if packet.Prompt.Digest != promptDigest || packet.Prompt.Instructions != instructions {
			return nil, errors.New("quality judge: a batch must share one prompt contract")
		}
		if _, exists := seen[packet.PacketID]; exists {
			return nil, fmt.Errorf("quality judge: duplicate packet %q", packet.PacketID)
		}
		seen[packet.PacketID] = struct{}{}
		request.Packets = append(request.Packets, distillQualityJudgeWirePacketV1{
			PacketID:     packet.PacketID,
			PacketDigest: packet.PacketDigest,
			Item:         packet.Item,
			PromptDigest: packet.Prompt.Digest,
			Payload:      packet.Payload,
		})
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(data) > distillQualityJudgeMaxPromptBytesV1 {
		return nil, fmt.Errorf("quality judge: batch prompt exceeds %d bytes", distillQualityJudgeMaxPromptBytesV1)
	}
	return data, nil
}

func decodeDistillQualityJudgeResponseV1(data []byte, judge distillQualityJudgeV1, model string, packets []distillQualityPanelPacketV1) ([]distillQualityPanelVerdictV1, error) {
	if len(data) == 0 || len(data) > distillQualityJudgeMaxOutputBytesV1 {
		return nil, errors.New("quality judge: invalid response byte length")
	}
	data, _ = normalizeDistillQualityJudgeJSONControlsV1(data)
	var response distillQualityJudgeBatchResponseV1
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("quality judge: decode response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("quality judge: response has trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("quality judge: response has trailing data")
	}
	if response.Contract != distillQualityJudgeResponseContractV1 {
		return nil, fmt.Errorf("quality judge: unexpected response contract %q", response.Contract)
	}
	if (response.Judge != "" && response.Judge != judge) || (response.Model != "" && response.Model != model) {
		return nil, errors.New("quality judge: top-level judge or model mismatch")
	}
	if len(response.Verdicts) != len(packets) {
		return nil, fmt.Errorf("quality judge: got %d verdicts for %d packets", len(response.Verdicts), len(packets))
	}
	packetByID := make(map[string]distillQualityPanelPacketV1, len(packets))
	for _, packet := range packets {
		packetByID[packet.PacketID] = packet
	}
	seen := make(map[string]struct{}, len(response.Verdicts))
	for index := range response.Verdicts {
		verdict := &response.Verdicts[index]
		if verdict.Judge != judge || verdict.Model != model {
			return nil, fmt.Errorf("quality judge: verdict identity mismatch for packet %q", verdict.PacketID)
		}
		packet, ok := packetByID[verdict.PacketID]
		if !ok {
			return nil, fmt.Errorf("quality judge: verdict for unknown packet %q", verdict.PacketID)
		}
		if _, exists := seen[verdict.PacketID]; exists {
			return nil, fmt.Errorf("quality judge: duplicate verdict for packet %q", verdict.PacketID)
		}
		seen[verdict.PacketID] = struct{}{}
		// PacketID is the immutable cryptographic binding selected by the judge.
		// The remaining identities are redundant transport fields, so bind them
		// locally instead of discarding otherwise valid judgments for a copied
		// digest typo.
		verdict.PacketDigest = packet.PacketDigest
		verdict.ItemDigest = packet.Item.Digest
		verdict.PromptDigest = packet.Prompt.Digest
		verdict.PayloadDigest = packet.Payload.Digest
		if err := normalizeDistillQualityAdmissionDerivedFieldsV1(packet, verdict); err != nil {
			return nil, fmt.Errorf("quality judge: packet %q: %w", verdict.PacketID, err)
		}
		if err := validateDistillQualityPanelVerdictV1(packet, *verdict); err != nil {
			return nil, err
		}
		if err := validateDistillQualityAdmissionVerdictV1(packet, *verdict); err != nil {
			return nil, fmt.Errorf("quality judge: packet %q: %w", verdict.PacketID, err)
		}
	}
	sort.Slice(response.Verdicts, func(i, j int) bool { return response.Verdicts[i].PacketID < response.Verdicts[j].PacketID })
	return response.Verdicts, nil
}

func normalizeDistillQualityAdmissionDerivedFieldsV1(packet distillQualityPanelPacketV1, verdict *distillQualityPanelVerdictV1) error {
	if packet.Prompt.ID != distillQualityAdmissionPromptIDV1 || verdict.State != distillQualityVerdictCompletedV1 {
		return nil
	}
	var itemMetadata struct {
		CandidateAdmitted bool `json:"candidate_admitted"`
	}
	if err := json.Unmarshal([]byte(packet.Item.Text), &itemMetadata); err != nil {
		return fmt.Errorf("packet admission metadata is invalid: %w", err)
	}
	for index := range verdict.Scores {
		score := &verdict.Scores[index]
		if score.Dimension != distillQualityDimensionAdmissionV1 {
			continue
		}
		var raw map[string]any
		decoder := json.NewDecoder(bytes.NewReader(score.RawScore))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("admission raw_score is invalid: %w", err)
		}
		shouldAdmit, shouldOK := raw["should_admit"].(string)
		candidateAdmitted, candidateOK := raw["candidate_admitted"].(bool)
		_, criticalOK := raw["critical_miss"].(bool)
		if !shouldOK || !candidateOK || !criticalOK || candidateAdmitted != itemMetadata.CandidateAdmitted {
			return nil
		}
		raw["critical_miss"] = shouldAdmit == "yes" && !candidateAdmitted
		canonical, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		score.RawScore = canonical
		return nil
	}
	return nil
}

func normalizeDistillQualityJudgeJSONControlsV1(data []byte) ([]byte, bool) {
	var out bytes.Buffer
	out.Grow(len(data))
	inString, escaped, changed := false, false, false
	hex := "0123456789abcdef"
	for _, value := range data {
		if !inString {
			out.WriteByte(value)
			if value == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			out.WriteByte(value)
			escaped = false
			continue
		}
		switch value {
		case '\\':
			out.WriteByte(value)
			escaped = true
		case '"':
			out.WriteByte(value)
			inString = false
		case '\n':
			out.WriteString(`\n`)
			changed = true
		case '\r':
			out.WriteString(`\r`)
			changed = true
		case '\t':
			out.WriteString(`\t`)
			changed = true
		default:
			if value < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[value>>4])
				out.WriteByte(hex[value&0x0f])
				changed = true
			} else {
				out.WriteByte(value)
			}
		}
	}
	if !changed {
		return data, false
	}
	return out.Bytes(), true
}

func validateDistillQualityAdmissionVerdictV1(packet distillQualityPanelPacketV1, verdict distillQualityPanelVerdictV1) error {
	if packet.Prompt.ID != distillQualityAdmissionPromptIDV1 {
		return nil
	}
	if verdict.State != distillQualityVerdictCompletedV1 {
		return nil
	}
	var itemMetadata struct {
		CandidateAdmitted bool `json:"candidate_admitted"`
	}
	if err := json.Unmarshal([]byte(packet.Item.Text), &itemMetadata); err != nil {
		return fmt.Errorf("packet admission metadata is invalid: %w", err)
	}
	want := map[distillQualityDimensionV1]bool{
		distillQualityDimensionAdmissionV1: true,
		distillQualityDimensionAuthorityV1: true,
		distillQualityDimensionSafetyV1:    true,
	}
	if len(verdict.Scores) != len(want) {
		return errors.New("completed admission verdict must contain exactly admission, authority, and safety")
	}
	for _, score := range verdict.Scores {
		if !want[score.Dimension] {
			return fmt.Errorf("unexpected admission dimension %q", score.Dimension)
		}
		if len(score.RawScore) == 0 {
			return fmt.Errorf("dimension %q requires raw_score", score.Dimension)
		}
		if strings.TrimSpace(score.Rationale) == "" || len(score.Rationale) > 240 {
			return fmt.Errorf("dimension %q requires a rationale of at most 240 bytes", score.Dimension)
		}
		citesEvidence := false
		for _, fragment := range packet.Payload.Fragments {
			if strings.Contains(score.Rationale, fragment.ID) {
				citesEvidence = true
				break
			}
		}
		if !citesEvidence {
			return fmt.Errorf("dimension %q rationale must cite an evidence fragment", score.Dimension)
		}
		var raw map[string]any
		decoder := json.NewDecoder(bytes.NewReader(score.RawScore))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("dimension %q has invalid raw_score: %w", score.Dimension, err)
		}
		var required map[string]bool
		switch score.Dimension {
		case distillQualityDimensionAdmissionV1:
			required = map[string]bool{"should_admit": true, "candidate_admitted": true, "critical_miss": true}
			if !qualityRawEnumV1(raw["should_admit"], "yes", "no", "unclear") || !qualityRawBoolV1(raw["candidate_admitted"]) || !qualityRawBoolV1(raw["critical_miss"]) {
				return errors.New("admission raw_score has invalid values")
			}
			if raw["candidate_admitted"] != itemMetadata.CandidateAdmitted {
				return errors.New("admission raw_score changed candidate_admitted")
			}
			shouldAdmit, _ := raw["should_admit"].(string)
			criticalMiss, _ := raw["critical_miss"].(bool)
			if criticalMiss != (shouldAdmit == "yes" && !itemMetadata.CandidateAdmitted) {
				return errors.New("admission raw_score has inconsistent critical_miss")
			}
		case distillQualityDimensionAuthorityV1:
			required = map[string]bool{"authority": true}
			if !qualityRawEnumV1(raw["authority"], "adequate", "inadequate", "unclear") {
				return errors.New("authority raw_score has invalid value")
			}
		case distillQualityDimensionSafetyV1:
			required = map[string]bool{"unsafe_material": true}
			if !qualityRawEnumV1(raw["unsafe_material"], "yes", "no", "unclear") {
				return errors.New("safety raw_score has invalid value")
			}
		}
		if len(raw) != len(required) {
			return fmt.Errorf("dimension %q raw_score has unexpected fields", score.Dimension)
		}
		for key := range required {
			if _, ok := raw[key]; !ok {
				return fmt.Errorf("dimension %q raw_score is missing %q", score.Dimension, key)
			}
		}
	}
	return nil
}

func qualityRawEnumV1(value any, choices ...string) bool {
	got, ok := value.(string)
	if !ok {
		return false
	}
	for _, choice := range choices {
		if got == choice {
			return true
		}
	}
	return false
}

func qualityRawBoolV1(value any) bool {
	_, ok := value.(bool)
	return ok
}

func runDistillQualityJudgeProcessV1(ctx context.Context, workDir string, judge distillQualityJudgeV1, model string, prompt []byte, timeout time.Duration) (distillQualityJudgeProcessResultV1, error) {
	if !validDistillQualityJudgeV1(judge) || !validPanelWordV1(model) {
		return distillQualityJudgeProcessResultV1{}, errors.New("quality judge: invalid judge or model")
	}
	if len(prompt) == 0 || len(prompt) > distillQualityJudgeMaxPromptBytesV1 {
		return distillQualityJudgeProcessResultV1{}, errors.New("quality judge: invalid prompt size")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	executable, args, err := distillQualityJudgeCommandV1(judge, model, string(prompt), workDir)
	if err != nil {
		return distillQualityJudgeProcessResultV1{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, executable, args...)
	command.Dir = workDir
	stdout := newCappedDistillBuffer(distillQualityJudgeMaxOutputBytesV1)
	stderr := newCappedDistillBuffer(distillQualityJudgeMaxErrorBytesV1)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return distillQualityJudgeProcessResultV1{}, fmt.Errorf("quality judge %s timed out after %s", judge, timeout)
		}
		return distillQualityJudgeProcessResultV1{}, fmt.Errorf("quality judge %s process failed: %w", judge, err)
	}
	if stdout.Exceeded() {
		return distillQualityJudgeProcessResultV1{}, fmt.Errorf("quality judge %s output exceeds %d bytes", judge, distillQualityJudgeMaxOutputBytesV1)
	}
	return decodeDistillQualityJudgeProcessOutputV1(judge, model, stdout.String())
}

func distillQualityJudgeCommandV1(judge distillQualityJudgeV1, model, prompt, workDir string) (string, []string, error) {
	var executable string
	var args []string
	switch judge {
	case distillQualityJudgeClaudeV1:
		executable = "claude"
		args = []string{"--no-session-persistence", "--print", "--output-format", "json", "--model", model, "--permission-mode", "dontAsk", "--tools", "", "--disable-slash-commands", "--json-schema", distillQualityJudgeJSONSchemaV1(), "--system-prompt", "Return only the exact JSON response required by the user prompt. Do not use tools.", prompt}
	case distillQualityJudgeCursorV1:
		executable = "agent"
		args = []string{"--print", "--output-format", "json", "--mode", "ask", "--model", model, "--workspace", workDir, "--trust", "--sandbox", "enabled", prompt}
	case distillQualityJudgeCopilotV1:
		executable = "copilot"
		args = []string{"--prompt", prompt, "--output-format", "json", "--silent", "--mode", "interactive", "--model", model, "--no-custom-instructions", "--disable-builtin-mcps", "--no-remote-export", "--no-auto-update", "--no-ask-user", "--log-level", "none", "--stream", "off", "-C", workDir}
	default:
		return "", nil, fmt.Errorf("quality judge: unsupported judge %q", judge)
	}
	path, err := exec.LookPath(executable)
	if err != nil && judge == distillQualityJudgeCopilotV1 {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			candidate := filepath.Join(home, ".local", "share", "gh", "copilot", "copilot")
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				path, err = candidate, nil
			}
		}
	}
	if err != nil {
		return "", nil, fmt.Errorf("quality judge %s executable unavailable: %w", judge, err)
	}
	return path, args, nil
}

func distillQualityJudgeJSONSchemaV1() string {
	return `{"type":"object","additionalProperties":false,"required":["contract","verdicts"],"properties":{"contract":{"const":"phase2_judge_batch_response_v1"},"verdicts":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","additionalProperties":false,"required":["schema_version","packet_id","packet_digest","item_digest","prompt_digest","payload_digest","judge","model","state","scores"],"properties":{"schema_version":{"const":1},"packet_id":{"type":"string"},"packet_digest":{"type":"string"},"item_digest":{"type":"string"},"prompt_digest":{"type":"string"},"payload_digest":{"type":"string"},"judge":{"enum":["copilot","cursor","claude"]},"model":{"type":"string"},"state":{"enum":["completed","abstain"]},"scores":{"type":"array","maxItems":3,"items":{"type":"object","additionalProperties":false,"required":["dimension","label","raw_score","rationale"],"properties":{"dimension":{"enum":["admission","authority","safety"]},"label":{"enum":["pass","concern","critical"]},"raw_score":{"type":"object"},"rationale":{"type":"string","maxLength":240}}}},"rationale":{"type":"string","maxLength":240}}}}}}`
}

func decodeDistillQualityJudgeProcessOutputV1(judge distillQualityJudgeV1, configuredModel, raw string) (distillQualityJudgeProcessResultV1, error) {
	result := distillQualityJudgeProcessResultV1{Model: configuredModel}
	switch judge {
	case distillQualityJudgeCopilotV1:
		content, err := decodeDistillQualityCopilotJSONLV1(raw)
		if err != nil {
			return result, err
		}
		result.Content = strings.TrimSpace(content)
	case distillQualityJudgeCursorV1:
		var envelope struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Usage   struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
			} `json:"usage"`
		}
		if err := decodeStrictDistillQualityJudgeEnvelopeV1(raw, &envelope); err != nil {
			return result, err
		}
		if envelope.IsError {
			return result, errors.New("quality judge cursor reported an error")
		}
		result.Content = strings.TrimSpace(envelope.Result)
		result.InputTokens, result.OutputTokens, result.UsageKnown = envelope.Usage.InputTokens, envelope.Usage.OutputTokens, true
	case distillQualityJudgeClaudeV1:
		var envelope struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Usage   struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := decodeStrictDistillQualityJudgeEnvelopeV1(raw, &envelope); err != nil {
			return result, err
		}
		if envelope.IsError {
			return result, errors.New("quality judge claude reported an error")
		}
		result.Content = strings.TrimSpace(envelope.Result)
		result.InputTokens, result.OutputTokens, result.UsageKnown = envelope.Usage.InputTokens, envelope.Usage.OutputTokens, true
	default:
		return result, fmt.Errorf("quality judge: unsupported judge %q", judge)
	}
	if result.Content == "" {
		return result, fmt.Errorf("quality judge %s returned no content", judge)
	}
	return result, nil
}

func decodeDistillQualityCopilotJSONLV1(raw string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var final string
	for {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Phase   string `json:"phase"`
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return "", fmt.Errorf("quality judge: decode copilot event stream: %w", err)
		}
		if event.Type == "assistant.message" && event.Data.Phase == "final_answer" && event.Data.Content != "" {
			final = event.Data.Content
		}
	}
	if final == "" {
		return "", errors.New("quality judge: copilot event stream has no final answer")
	}
	return final, nil
}

func decodeStrictDistillQualityJudgeEnvelopeV1(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	// Provider envelopes intentionally contain additive telemetry. Decode only
	// the fields above, then still require one complete JSON value.
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("quality judge: decode provider envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("quality judge: provider envelope has trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("quality judge: provider envelope has trailing data")
	}
	return nil
}
