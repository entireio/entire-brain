package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const distillQualityJudgeMaxConcurrencyV1 = 4

// distillQualityJudgeBatchV1 is built before provider execution so concurrent
// scheduling cannot change packet membership or prompt construction.
type distillQualityJudgeBatchV1 struct {
	Packets []distillQualityPanelPacketV1
	Prompt  []byte
}

// distillQualityJudgeBatchOutcomeV1 crosses from provider workers to the sole
// persistence owner. It contains only validated verdicts and content-free call
// telemetry; raw provider output and errors never leave the worker.
type distillQualityJudgeBatchOutcomeV1 struct {
	Verdicts []distillQualityPanelVerdictV1
	Calls    []distillQualityJudgeCallV1
	Err      error
}

// runDistillQualityJudgeBatchesV1 allows concurrent provider work but invokes
// persist only from its caller goroutine. This keeps JSONL replacement atomic
// and makes resuming equivalent to the sequential runner.
func runDistillQualityJudgeBatchesV1(ctx context.Context, concurrency int, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, invoke distillQualityJudgeInvokerV1, recovery bool, batches []distillQualityJudgeBatchV1, persist func(distillQualityJudgeBatchOutcomeV1) error) error {
	if concurrency < 1 || concurrency > distillQualityJudgeMaxConcurrencyV1 {
		return fmt.Errorf("quality judge: concurrency must be 1..%d", distillQualityJudgeMaxConcurrencyV1)
	}
	if len(batches) == 0 {
		return ctx.Err()
	}
	if persist == nil {
		return fmt.Errorf("quality judge: persistence callback is required")
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan distillQualityJudgeBatchV1)
	outcomes := make(chan distillQualityJudgeBatchOutcomeV1, concurrency)

	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case batch, ok := <-jobs:
					if !ok {
						return
					}
					outcome := executeDistillQualityJudgeBatchV1(workCtx, judge, model, timeout, now, invoke, recovery, batch)
					select {
					case outcomes <- outcome:
					case <-workCtx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, batch := range batches {
			select {
			case jobs <- batch:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(outcomes)
	}()

	var firstErr error
	for outcome := range outcomes {
		if firstErr != nil {
			continue
		}
		if outcome.Err != nil {
			firstErr = outcome.Err
			cancel()
			continue
		}
		if err := persist(outcome); err != nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func executeDistillQualityJudgeBatchV1(ctx context.Context, judge distillQualityJudgeV1, model string, timeout time.Duration, now func() time.Time, invoke distillQualityJudgeInvokerV1, recovery bool, batch distillQualityJudgeBatchV1) distillQualityJudgeBatchOutcomeV1 {
	var accepted []distillQualityPanelVerdictV1
	calls := make([]distillQualityJudgeCallV1, 0, 2)
	for retry := 0; retry < 2; retry++ {
		if err := ctx.Err(); err != nil {
			return distillQualityJudgeBatchOutcomeV1{Err: err}
		}
		workDir, err := os.MkdirTemp("", "entire-phase2-judge-"+string(judge)+"-")
		if err != nil {
			return distillQualityJudgeBatchOutcomeV1{Err: err}
		}
		startedAt := now().UTC()
		wallStart := time.Now()
		processResult, invokeErr := invoke(ctx, workDir, judge, model, batch.Prompt, timeout)
		removeErr := os.RemoveAll(workDir)
		if err := ctx.Err(); err != nil {
			return distillQualityJudgeBatchOutcomeV1{Err: err}
		}
		if removeErr != nil {
			return distillQualityJudgeBatchOutcomeV1{Err: removeErr}
		}
		call := distillQualityJudgeCallV1{
			Judge: judge, Model: model, StartedAt: startedAt.Format(time.RFC3339Nano),
			DurationMS: time.Since(wallStart).Milliseconds(), InputBytes: len(batch.Prompt), Retry: retry, Recovery: recovery,
			PacketIDs: distillQualityPacketIDsV1(batch.Packets),
		}
		if invokeErr != nil {
			call.Status = "transport_error"
		} else {
			call.OutputBytes = len(processResult.Content)
			call.InputTokens = processResult.InputTokens
			call.OutputTokens = processResult.OutputTokens
			call.UsageReported = processResult.UsageKnown
			call.ResponseSHA256 = distillQualitySHA256V1([]byte(processResult.Content))
			accepted, err = decodeDistillQualityJudgeResponseV1([]byte(processResult.Content), judge, model, batch.Packets)
			if err != nil {
				call.Status = "schema_error"
				call.ValidationError = boundedDistillQualityValidationErrorV1(err)
			} else {
				call.Status = "complete"
				call.TransportNormalized = !json.Valid([]byte(processResult.Content))
			}
		}
		calls = append(calls, call)
		if call.Status == "complete" {
			break
		}
	}
	if len(accepted) == 0 {
		accepted = make([]distillQualityPanelVerdictV1, len(batch.Packets))
		for index, packet := range batch.Packets {
			accepted[index] = invalidDistillQualityJudgeVerdictV1(packet, judge, model)
		}
	}
	return distillQualityJudgeBatchOutcomeV1{Verdicts: accepted, Calls: calls}
}
