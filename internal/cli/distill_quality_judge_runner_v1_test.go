package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDistillQualityJudgeBatchPromptAndResponse(t *testing.T) {
	packet := qualityPanelPacketForTest()
	prompt, err := buildDistillQualityJudgePromptV1(distillQualityJudgeClaudeV1, "claude-sonnet-5", []distillQualityPanelPacketV1{packet})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "session_id") || strings.Contains(string(prompt), "transcript") {
		t.Fatalf("prompt leaked source identity: %s", prompt)
	}

	verdict := qualityPanelVerdictForTest(packet, distillQualityJudgeClaudeV1, distillQualityAdvisoryPassV1)
	verdict.Model = "claude-sonnet-5"
	data, err := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeDistillQualityJudgeResponseV1(data, distillQualityJudgeClaudeV1, "claude-sonnet-5", []distillQualityPanelPacketV1{packet})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PacketID != packet.PacketID {
		t.Fatalf("verdicts = %+v", got)
	}

	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	response["unknown"] = true
	bad, _ := json.Marshal(response)
	if _, err := decodeDistillQualityJudgeResponseV1(bad, distillQualityJudgeClaudeV1, "claude-sonnet-5", []distillQualityPanelPacketV1{packet}); err == nil {
		t.Fatal("accepted unknown response field")
	}
}

func TestDecodeDistillQualityJudgeResponseNormalizesDerivedCriticalMiss(t *testing.T) {
	_, packets := reportFixtureRunV1(t)
	packet := packets[0]
	verdict := distillQualityAdmissionVerdictForJudgeTestV1(packet, distillQualityJudgeClaudeV1, "claude-haiku-4-5", "yes", true)
	verdict.PacketDigest = "sha256:" + strings.Repeat("0", 64)
	verdict.ItemDigest = "sha256:" + strings.Repeat("1", 64)
	verdict.PromptDigest = "sha256:" + strings.Repeat("2", 64)
	verdict.PayloadDigest = "sha256:" + strings.Repeat("3", 64)
	for index := range verdict.Scores {
		if verdict.Scores[index].Dimension == distillQualityDimensionAdmissionV1 {
			verdict.Scores[index].RawScore = json.RawMessage(`{"should_admit":"yes","candidate_admitted":true,"critical_miss":true}`)
		}
	}
	data, err := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeDistillQualityJudgeResponseV1(data, distillQualityJudgeClaudeV1, verdict.Model, []distillQualityPanelPacketV1{packet})
	if err != nil {
		t.Fatal(err)
	}
	var admission json.RawMessage
	for _, score := range got[0].Scores {
		if score.Dimension == distillQualityDimensionAdmissionV1 {
			admission = score.RawScore
			break
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(admission, &raw); err != nil {
		t.Fatal(err)
	}
	if critical, ok := raw["critical_miss"].(bool); !ok || critical {
		t.Fatalf("critical_miss = %#v", raw["critical_miss"])
	}
	if got[0].PacketDigest != packet.PacketDigest || got[0].ItemDigest != packet.Item.Digest || got[0].PromptDigest != packet.Prompt.Digest || got[0].PayloadDigest != packet.Payload.Digest {
		t.Fatalf("binding digests were not canonicalized: %+v", got[0])
	}
}

func TestDistillQualityJudgeResponseRejectsMissingDuplicateAndWrongIdentity(t *testing.T) {
	packet := qualityPanelPacketForTest()
	verdict := qualityPanelVerdictForTest(packet, distillQualityJudgeCursorV1, distillQualityAdvisoryPassV1)
	verdict.Model = "composer-2.5"
	for name, response := range map[string]distillQualityJudgeBatchResponseV1{
		"missing":   {Contract: distillQualityJudgeResponseContractV1},
		"duplicate": {Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict, verdict}},
		"contract":  {Contract: "wrong", Verdicts: []distillQualityPanelVerdictV1{verdict}},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(response)
			if _, err := decodeDistillQualityJudgeResponseV1(data, distillQualityJudgeCursorV1, "composer-2.5", []distillQualityPanelPacketV1{packet}); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	verdict.Judge = distillQualityJudgeCopilotV1
	data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
	if _, err := decodeDistillQualityJudgeResponseV1(data, distillQualityJudgeCursorV1, "composer-2.5", []distillQualityPanelPacketV1{packet}); err == nil {
		t.Fatal("accepted wrong judge identity")
	}
}

func TestDecodeDistillQualityJudgeProcessOutput(t *testing.T) {
	claudeRaw := `{"is_error":false,"result":"{\"ok\":true}","usage":{"input_tokens":10,"output_tokens":3},"extra":"ignored"}`
	claude, err := decodeDistillQualityJudgeProcessOutputV1(distillQualityJudgeClaudeV1, "claude-sonnet-5", claudeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Content != `{"ok":true}` || !claude.UsageKnown || claude.InputTokens != 10 || claude.OutputTokens != 3 {
		t.Fatalf("claude = %+v", claude)
	}

	cursorRaw := `{"type":"result","is_error":false,"result":"{\"ok\":true}","usage":{"inputTokens":12,"outputTokens":4}}`
	cursor, err := decodeDistillQualityJudgeProcessOutputV1(distillQualityJudgeCursorV1, "composer-2.5", cursorRaw)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Content != `{"ok":true}` || !cursor.UsageKnown || cursor.InputTokens != 12 || cursor.OutputTokens != 4 {
		t.Fatalf("cursor = %+v", cursor)
	}

	copilotRaw := "{\"type\":\"assistant.message_delta\",\"data\":{\"phase\":\"final_answer\",\"deltaContent\":\"ignored\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"phase\":\"final_answer\",\"content\":\"{\\\"ok\\\":true}\"}}\n" +
		"{\"type\":\"result\",\"data\":null}\n"
	copilot, err := decodeDistillQualityJudgeProcessOutputV1(distillQualityJudgeCopilotV1, "gpt-5.4", copilotRaw)
	if err != nil {
		t.Fatal(err)
	}
	if copilot.Content != `{"ok":true}` || copilot.UsageKnown {
		t.Fatalf("copilot = %+v", copilot)
	}
}

func TestDistillQualityJudgeJSONSchemaV1IsValid(t *testing.T) {
	if !json.Valid([]byte(distillQualityJudgeJSONSchemaV1())) {
		t.Fatal("Claude judge schema is not valid JSON")
	}
}

func TestDecodeDistillQualityJudgeResponseNormalizesRawStringControlsOnly(t *testing.T) {
	_, packets := reportFixtureRunV1(t)
	packet := packets[0]
	verdict := distillQualityAdmissionVerdictForJudgeTestV1(packet, distillQualityJudgeCopilotV1, "gpt-test", "yes", true)
	data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
	data = []byte(strings.Replace(string(data), "supports this", "supports\nthis", 1))
	if json.Valid(data) {
		t.Fatal("fixture unexpectedly remained valid JSON")
	}
	got, err := decodeDistillQualityJudgeResponseV1(data, verdict.Judge, verdict.Model, []distillQualityPanelPacketV1{packet})
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Scores[0].Rationale, "supports\nthis") {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	broken := []byte(strings.Replace(string(data), `"state":"completed"`, `"state":"completed`, 1))
	if _, err := decodeDistillQualityJudgeResponseV1(broken, verdict.Judge, verdict.Model, []distillQualityPanelPacketV1{packet}); err == nil {
		t.Fatal("normalizer repaired structural JSON damage")
	}
}
