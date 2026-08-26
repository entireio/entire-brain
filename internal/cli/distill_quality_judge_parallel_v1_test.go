package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunDistillQualityJudgeConcurrentBatchesPersistOnceAndResumeV1(t *testing.T) {
	root, packets := writeDistillQualityJudgeParallelFixtureV1(t, 65)
	byID := distillQualityPacketsByIDForTestV1(packets)
	var active, maximum, calls atomic.Int32
	var workDirsMu sync.Mutex
	workDirs := map[string]bool{}
	invoker := func(_ context.Context, workDir string, judge distillQualityJudgeV1, model string, prompt []byte, _ time.Duration) (distillQualityJudgeProcessResultV1, error) {
		if _, err := os.Stat(workDir); err != nil {
			return distillQualityJudgeProcessResultV1{}, err
		}
		workDirsMu.Lock()
		if workDirs[workDir] {
			workDirsMu.Unlock()
			return distillQualityJudgeProcessResultV1{}, fmt.Errorf("reused provider workdir")
		}
		workDirs[workDir] = true
		workDirsMu.Unlock()
		current := active.Add(1)
		for {
			prior := maximum.Load()
			if current <= prior || maximum.CompareAndSwap(prior, current) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		calls.Add(1)
		return distillQualityJudgeSuccessForPromptV1(prompt, byID, judge, model), nil
	}

	result, err := runDistillQualityJudgeWithInvokerConcurrencyV1(context.Background(), root, distillQualityJudgeCursorV1, "cursor-test", time.Minute, time.Now, invoker, false, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != len(packets) || result.Invalid != 0 || result.ProviderCalls != 3 || result.Concurrency != 3 {
		t.Fatalf("result=%+v", result)
	}
	if maximum.Load() < 2 || maximum.Load() > 3 || calls.Load() != 3 {
		t.Fatalf("provider parallelism max=%d calls=%d", maximum.Load(), calls.Load())
	}
	workDirsMu.Lock()
	if len(workDirs) != 3 {
		t.Fatalf("workdirs=%d, want 3", len(workDirs))
	}
	workDirsMu.Unlock()
	packetByID := distillQualityPacketsByIDForTestV1(packets)
	verdicts, err := loadDistillQualityJudgeVerdictsV1(root, distillQualityJudgeCursorV1, packetByID)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != len(packets) {
		t.Fatalf("persisted verdicts=%d, want %d", len(verdicts), len(packets))
	}
	seen := make(map[string]bool, len(verdicts))
	for _, verdict := range verdicts {
		if seen[verdict.PacketID] {
			t.Fatalf("duplicate persisted verdict %q", verdict.PacketID)
		}
		seen[verdict.PacketID] = true
	}

	result, err = runDistillQualityJudgeWithInvokerConcurrencyV1(context.Background(), root, distillQualityJudgeCursorV1, "cursor-test", time.Minute, time.Now, func(context.Context, string, distillQualityJudgeV1, string, []byte, time.Duration) (distillQualityJudgeProcessResultV1, error) {
		t.Fatal("resume invoked provider")
		return distillQualityJudgeProcessResultV1{}, nil
	}, false, 3)
	if err != nil || result.ProviderCalls != 3 || result.Completed != len(packets) {
		t.Fatalf("resume result=%+v err=%v", result, err)
	}
}

func TestRunDistillQualityJudgeConcurrentCancellationJoinsWorkersV1(t *testing.T) {
	root, _ := writeDistillQualityJudgeParallelFixtureV1(t, 65)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 3)
	var active atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := runDistillQualityJudgeWithInvokerConcurrencyV1(ctx, root, distillQualityJudgeCopilotV1, "gpt-test", time.Minute, time.Now, func(ctx context.Context, _ string, _ distillQualityJudgeV1, _ string, _ []byte, _ time.Duration) (distillQualityJudgeProcessResultV1, error) {
			active.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			active.Add(-1)
			return distillQualityJudgeProcessResultV1{}, ctx.Err()
		}, false, 3)
		done <- err
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not join cancelled workers")
	}
	if active.Load() != 0 {
		t.Fatalf("workers still active after return: %d", active.Load())
	}
	if _, err := os.Stat(filepath.Join(root, distillQualityPanelDirV1, "copilot.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("cancelled execution persisted verdicts: %v", err)
	}
}

func TestRunDistillQualityJudgeConcurrencyBoundsV1(t *testing.T) {
	root, _ := reportFixtureRunV1(t)
	for _, concurrency := range []int{0, 5} {
		_, err := runDistillQualityJudgeWithInvokerConcurrencyV1(context.Background(), root, distillQualityJudgeClaudeV1, "claude-test", time.Minute, time.Now, nil, false, concurrency)
		if err == nil || !strings.Contains(err.Error(), "concurrency") {
			t.Fatalf("concurrency=%d err=%v", concurrency, err)
		}
	}
}

func distillQualityJudgeSuccessForPromptV1(prompt []byte, byID map[string]distillQualityPanelPacketV1, judge distillQualityJudgeV1, model string) distillQualityJudgeProcessResultV1 {
	var request distillQualityJudgeBatchRequestV1
	if err := json.Unmarshal(prompt, &request); err != nil {
		return distillQualityJudgeProcessResultV1{Content: "{"}
	}
	verdicts := make([]distillQualityPanelVerdictV1, len(request.Packets))
	for index, wire := range request.Packets {
		packet := byID[wire.PacketID]
		var metadata struct {
			CandidateAdmitted bool `json:"candidate_admitted"`
		}
		_ = json.Unmarshal([]byte(packet.Item.Text), &metadata)
		should := "no"
		if metadata.CandidateAdmitted {
			should = "yes"
		}
		verdicts[index] = distillQualityAdmissionVerdictForJudgeTestV1(packet, judge, model, should, metadata.CandidateAdmitted)
	}
	data, _ := json.Marshal(distillQualityJudgeBatchResponseV1{Contract: distillQualityJudgeResponseContractV1, Verdicts: verdicts})
	return distillQualityJudgeProcessResultV1{Content: string(data), InputTokens: 1, OutputTokens: 1, UsageKnown: true}
}

func distillQualityPacketsByIDForTestV1(packets []distillQualityPanelPacketV1) map[string]distillQualityPanelPacketV1 {
	byID := make(map[string]distillQualityPanelPacketV1, len(packets))
	for _, packet := range packets {
		byID[packet.PacketID] = packet
	}
	return byID
}

func writeDistillQualityJudgeParallelFixtureV1(t *testing.T, admittedCount int) (string, []distillQualityPanelPacketV1) {
	t.Helper()
	root := t.TempDir()
	admission := make([]distillQualityAdmissionItemV1, admittedCount)
	sources := make([]distillQualityPrivateSourceRecordV1, 0, admittedCount+1)
	for index := range admittedCount {
		id := fmt.Sprintf("public-%03d", index)
		admission[index] = distillQualityAdmissionItemV1{ID: id, Digest: fmt.Sprintf("%064x", index+1), Stratum: "admitted:direct_user:rule", Roles: []string{"user"}, Authorities: []string{"direct_user"}, Cues: []string{"rule"}, Evidence: "user: Keep write operations explicit.", EvidenceBytes: len("user: Keep write operations explicit."), Admitted: true}
		sources = append(sources, distillQualityPrivateSourceRecordV1{PublicID: id, SessionID: "local-session-" + id, Branch: "main", TranscriptPath: "sessions/main/" + id + ".jsonl", TurnIDs: []string{"turn-1"}, StartLine: 1, EndLine: 1})
	}
	filtered := distillQualityAdmissionItemV1{ID: "filtered-001", Digest: fmt.Sprintf("%064x", admittedCount+1), Stratum: "user:context", Roles: []string{"user"}, Authorities: []string{"context_only"}, Evidence: "user: What is the status?", EvidenceBytes: len("user: What is the status?"), Admitted: false}
	sources = append(sources, distillQualityPrivateSourceRecordV1{PublicID: filtered.ID, SessionID: "local-session-filtered", Branch: "main", TranscriptPath: "sessions/main/filtered.jsonl", TurnIDs: []string{"turn-2"}, StartLine: 1, EndLine: 1})
	bundle := distillQualityBundleV1{SchemaVersion: distillQualityBundleSchemaVersionV1, Admission: admission, Filtered: []distillQualityAdmissionItemV1{filtered}}
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
	sourceBytes, err := marshalDistillQualityJSONLV1(sources)
	if err != nil {
		t.Fatal(err)
	}
	manifest := distillQualityRunManifestV1{Contract: distillQualityRunContractV1, SchemaVersion: 1, RunID: bundle.RunID, GeneratedAt: "2026-08-25T12:00:00Z", EvaluatorRevision: "test-revision", SourceRevision: "source-revision", BrainManifestSHA256: reportTestDigestV1("brain"), CorpusSHA256: reportTestDigestV1("corpus"), BundleSHA256: distillQualitySHA256V1(bundleBytes), PacketsSHA256: distillQualitySHA256V1(packetBytes), PrivateSourceMapSHA256: distillQualitySHA256V1(sourceBytes), PromptSHA256: packets[0].Prompt.Digest, CandidateSchemaVersion: distillCandidateSchemaVersion, RedactionVersion: distillCandidateRedactionVersionV2, SessionViews: 1, AdmittedItems: len(admission), FilteredItems: 1, FilteredRequested: 1, PrivateSourceRecords: len(sources), AdvisoryOnly: true}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{distillQualityRunManifestFileV1: manifestBytes, distillQualityBundleFileV1: bundleBytes, distillQualityPacketsFileV1: packetBytes, distillQualitySourceMapFileV1: sourceBytes} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(packets, func(i, j int) bool { return packets[i].PacketID < packets[j].PacketID })
	return root, packets
}
