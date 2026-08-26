package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDistillQualityJudgeV1PersistsAndResumes(t *testing.T) {
	root, packets := reportFixtureRunV1(t)
	calls := 0
	invoker := func(_ context.Context, _ string, judge distillQualityJudgeV1, model string, prompt []byte, _ time.Duration) (distillQualityJudgeProcessResultV1, error) {
		calls++
		var request distillQualityJudgeBatchRequestV1
		if err := json.Unmarshal(prompt, &request); err != nil {
			t.Fatal(err)
		}
		verdicts := make([]distillQualityPanelVerdictV1, len(request.Packets))
		byID := map[string]distillQualityPanelPacketV1{}
		for _, packet := range packets {
			byID[packet.PacketID] = packet
		}
		for index, wire := range request.Packets {
			packet := byID[wire.PacketID]
			var metadata struct {
				CandidateAdmitted bool `json:"candidate_admitted"`
			}
			if err := json.Unmarshal([]byte(packet.Item.Text), &metadata); err != nil {
				t.Fatal(err)
			}
			should := "no"
			if metadata.CandidateAdmitted {
				should = "yes"
			}
			verdicts[index] = distillQualityAdmissionVerdictForJudgeTestV1(packet, judge, model, should, metadata.CandidateAdmitted)
		}
		data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: verdicts})
		return distillQualityJudgeProcessResultV1{Content: string(data), InputTokens: 20, OutputTokens: 10, UsageKnown: true}, nil
	}
	now := func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }
	result, err := runDistillQualityJudgeWithInvokerV1(context.Background(), root, distillQualityJudgeClaudeV1, "claude-test", time.Minute, now, invoker, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Completed != len(packets) || result.Invalid != 0 || result.ProviderCalls != 1 {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	result, err = runDistillQualityJudgeWithInvokerV1(context.Background(), root, distillQualityJudgeClaudeV1, "claude-test", time.Minute, now, func(context.Context, string, distillQualityJudgeV1, string, []byte, time.Duration) (distillQualityJudgeProcessResultV1, error) {
		t.Fatal("resume invoked provider")
		return distillQualityJudgeProcessResultV1{}, nil
	}, false)
	if err != nil || result.Completed != len(packets) || result.ProviderCalls != 1 {
		t.Fatalf("resume result=%+v err=%v", result, err)
	}
}

func TestDistillQualityJudgeRunBatchLimitV1(t *testing.T) {
	if got := distillQualityJudgeRunBatchLimitV1(distillQualityJudgeClaudeV1, false); got != 8 {
		t.Fatalf("Claude primary limit = %d", got)
	}
	if got := distillQualityJudgeRunBatchLimitV1(distillQualityJudgeCopilotV1, true); got != 8 {
		t.Fatalf("recovery limit = %d", got)
	}
	if got := distillQualityJudgeRunBatchLimitV1(distillQualityJudgeCursorV1, false); got != distillQualityJudgeMaxBatchItemsV1 {
		t.Fatalf("Cursor primary limit = %d", got)
	}
}

func TestRunDistillQualityJudgeV1RetriesThenStoresContentFreeInvalid(t *testing.T) {
	root, packets := reportFixtureRunV1(t)
	calls := 0
	secret := "provider echoed SECRET-CANARY"
	result, err := runDistillQualityJudgeWithInvokerV1(context.Background(), root, distillQualityJudgeCursorV1, "cursor-test", time.Minute, time.Now, func(context.Context, string, distillQualityJudgeV1, string, []byte, time.Duration) (distillQualityJudgeProcessResultV1, error) {
		calls++
		return distillQualityJudgeProcessResultV1{}, errors.New(secret)
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Invalid != len(packets) || result.Retries != 1 {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	for _, name := range []string{"cursor.jsonl", "cursor-calls.jsonl"} {
		data, readErr := os.ReadFile(filepath.Join(root, distillQualityPanelDirV1, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s retained provider error text", name)
		}
	}
}

func TestRunDistillQualityJudgeV1RecoversInvalidOnce(t *testing.T) {
	root, packets := reportFixtureRunV1(t)
	failedCalls := 0
	_, err := runDistillQualityJudgeWithInvokerV1(context.Background(), root, distillQualityJudgeCopilotV1, "gpt-test", time.Minute, time.Now, func(context.Context, string, distillQualityJudgeV1, string, []byte, time.Duration) (distillQualityJudgeProcessResultV1, error) {
		failedCalls++
		return distillQualityJudgeProcessResultV1{}, errors.New("unavailable")
	}, false)
	if err != nil || failedCalls != 2 {
		t.Fatalf("initial calls=%d err=%v", failedCalls, err)
	}
	recoveryCalls := 0
	result, err := runDistillQualityJudgeWithInvokerV1(context.Background(), root, distillQualityJudgeCopilotV1, "gpt-test", time.Minute, time.Now, func(_ context.Context, _ string, judge distillQualityJudgeV1, model string, prompt []byte, _ time.Duration) (distillQualityJudgeProcessResultV1, error) {
		recoveryCalls++
		var request distillQualityJudgeBatchRequestV1
		if err := json.Unmarshal(prompt, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Packets) > 8 {
			t.Fatalf("recovery batch has %d packets", len(request.Packets))
		}
		byID := make(map[string]distillQualityPanelPacketV1, len(packets))
		for _, packet := range packets {
			byID[packet.PacketID] = packet
		}
		verdicts := make([]distillQualityPanelVerdictV1, len(request.Packets))
		for index, wire := range request.Packets {
			packet := byID[wire.PacketID]
			var metadata struct {
				CandidateAdmitted bool `json:"candidate_admitted"`
			}
			if err := json.Unmarshal([]byte(packet.Item.Text), &metadata); err != nil {
				t.Fatal(err)
			}
			should := "no"
			if metadata.CandidateAdmitted {
				should = "yes"
			}
			verdicts[index] = distillQualityAdmissionVerdictForJudgeTestV1(packet, judge, model, should, metadata.CandidateAdmitted)
		}
		data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: verdicts})
		return distillQualityJudgeProcessResultV1{Content: string(data)}, nil
	}, true)
	if err != nil || result.Invalid != 0 || result.Recovered != len(packets) || recoveryCalls != 1 {
		t.Fatalf("recovery calls=%d result=%+v err=%v", recoveryCalls, result, err)
	}
	callBytes, err := os.ReadFile(filepath.Join(root, distillQualityPanelDirV1, "copilot-calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(callBytes), `"recovery":true`) {
		t.Fatal("recovery call was not retained")
	}
}

func TestValidateDistillQualityAdmissionVerdictV1RequiresExactDimensions(t *testing.T) {
	_, packets := reportFixtureRunV1(t)
	packet := packets[0]
	verdict := distillQualityAdmissionVerdictForJudgeTestV1(packet, distillQualityJudgeCopilotV1, "gpt-test", "yes", true)
	verdict.Scores = verdict.Scores[:2]
	data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: []distillQualityPanelVerdictV1{verdict}})
	if _, err := decodeDistillQualityJudgeResponseV1(data, verdict.Judge, verdict.Model, []distillQualityPanelPacketV1{packet}); err == nil {
		t.Fatal("accepted incomplete admission dimensions")
	}
}

func distillQualityAdmissionVerdictForJudgeTestV1(packet distillQualityPanelPacketV1, judge distillQualityJudgeV1, model, should string, candidate bool) distillQualityPanelVerdictV1 {
	admissionRaw, _ := json.Marshal(map[string]any{"should_admit": should, "candidate_admitted": candidate, "critical_miss": should == "yes" && !candidate})
	return distillQualityPanelVerdictV1{
		SchemaVersion: 1, PacketID: packet.PacketID, PacketDigest: packet.PacketDigest,
		ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest,
		Judge: judge, Model: model, State: distillQualityVerdictCompletedV1,
		Scores: []distillQualityPanelScoreV1{
			{Dimension: distillQualityDimensionAdmissionV1, Label: distillQualityAdvisoryPassV1, RawScore: admissionRaw, Rationale: "span-001 supports this admission decision."},
			{Dimension: distillQualityDimensionAuthorityV1, Label: distillQualityAdvisoryPassV1, RawScore: json.RawMessage(`{"authority":"adequate"}`), Rationale: "span-001 has the expected authority."},
			{Dimension: distillQualityDimensionSafetyV1, Label: distillQualityAdvisoryPassV1, RawScore: json.RawMessage(`{"unsafe_material":"no"}`), Rationale: "span-001 has no unsafe material."},
		},
	}
}
