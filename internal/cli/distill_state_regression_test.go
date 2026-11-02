package cli

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestDistillRejectsNonfiniteConfidenceBeforeAgent(t *testing.T) {
	for _, confidence := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		now := time.Now()
		d := writeSameBranchFixture(t, now)
		calls := 0
		run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			calls++
			return "workflow.testing.rules\tUse alpha tests.\n", nil
		}
		_, err := runDistillForBrain(context.Background(), t.TempDir(), d, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, confidenceThreshold: confidence, timeout: time.Minute}, now)
		if err == nil || calls != 0 {
			t.Errorf("confidence %v: error=%v, agent calls=%d", confidence, err, calls)
		}
		facts, err := factstore.Load(d, "main")
		if err != nil || len(facts) != 0 {
			t.Fatalf("invalid confidence wrote facts: %v, %v", facts, err)
		}
	}
}

func TestDistillPreservesConcurrentSessionManifest(t *testing.T) {
	now := time.Now()
	d := writeSameBranchFixture(t, now)
	fired := false
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		if !fired {
			fired = true
			if err := brainstore.WithBrainWriteLock(d, func() error {
				m, err := bmanifest.Load(d)
				if err != nil {
					return err
				}
				m.Sources.Sessions.Sessions = append(m.Sources.Sessions.Sessions, exportSession{SessionID: "arrived-during-distill", Branch: "main", TranscriptPath: "sessions/main/new.jsonl", CreatedAt: now})
				return writeBrainManifestAndReadme(d, *m)
			}); err != nil {
				t.Fatal(err)
			}
		}
		return "", nil
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), d, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, timeout: time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	m, err := bmanifest.Load(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Sources.Sessions.Sessions {
		if s.SessionID == "arrived-during-distill" {
			return
		}
	}
	t.Fatal("distillation discarded concurrently exported session")
}
