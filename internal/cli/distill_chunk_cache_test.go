package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// chunkCacheFixture lays down a single-session brain whose transcript can be
// grown between runs, plus a fake agent that records every chunk it receives.
func chunkCacheFixture(t *testing.T, now time.Time, transcript string) (string, *[]string, distillCommandOptions) {
	t.Helper()
	brainDir := writeSingleSessionFixture(t, now, transcript)
	var inputs []string
	fakeRun := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		inputs = append(inputs, string(input))
		// Distinct fact text per call so content-id dedupe never collapses calls.
		return fmt.Sprintf("project.testing.chunks\tFact number %d about chunk input.\n", len(inputs)), nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: 30, timeout: time.Minute, concurrency: 1}
	return brainDir, &inputs, opts
}

func growTranscript(t *testing.T, brainDir, addition string) {
	t.Helper()
	p := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(addition); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// chunkCallsOnly filters the fake agent's recorded inputs down to transcript
// chunks: the reconcile step shares the run func, and its CANDIDATES/EXISTING
// prompt must not count as a chunk dispatch.
func chunkCallsOnly(inputs []string) []string {
	var chunks []string
	for _, in := range inputs {
		if !strings.HasPrefix(in, "CANDIDATES") {
			chunks = append(chunks, in)
		}
	}
	return chunks
}

func TestDistillReusesCachedChunksForGrownTranscript(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	brainDir, inputs, opts := chunkCacheFixture(t, now, "alpha one\nalpha two\nalpha three\nalpha four\n")

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("first distill: %v", err)
	}
	firstCalls := len(chunkCallsOnly(*inputs))
	if firstCalls < 2 {
		t.Fatalf("fixture must produce at least 2 chunks, got %d", firstCalls)
	}

	growTranscript(t, brainDir, "alpha five\nalpha six\n")
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute)); err != nil {
		t.Fatalf("second distill: %v", err)
	}
	secondCalls := chunkCallsOnly((*inputs)[firstCalls:])
	if len(secondCalls) != 1 {
		t.Fatalf("grown transcript must re-dispatch only the new tail chunk, got %d calls: %q", len(secondCalls), secondCalls)
	}
	if !strings.Contains(secondCalls[0], "5\talpha five") {
		t.Fatalf("tail chunk must carry the appended lines, got %q", secondCalls[0])
	}
	for _, earlier := range (*inputs)[:firstCalls] {
		if secondCalls[0] == earlier {
			t.Fatal("second run re-dispatched an already-distilled chunk")
		}
	}

	// Facts from skipped chunks must survive the incremental run.
	facts, err := factstore.Load(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != firstCalls+1 {
		t.Fatalf("expected %d facts (one per chunk call), got %d", firstCalls+1, len(facts))
	}
}

func TestDistillForceBypassesChunkReuse(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	brainDir, inputs, opts := chunkCacheFixture(t, now, "alpha one\nalpha two\nalpha three\nalpha four\n")

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("first distill: %v", err)
	}
	firstCalls := len(chunkCallsOnly(*inputs))

	forceOpts := opts
	forceOpts.force = true
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, forceOpts, now.Add(time.Minute)); err != nil {
		t.Fatalf("force distill: %v", err)
	}
	if got := len(chunkCallsOnly(*inputs)) - firstCalls; got != firstCalls {
		t.Fatalf("force must re-dispatch every chunk (%d), got %d", firstCalls, got)
	}
}
