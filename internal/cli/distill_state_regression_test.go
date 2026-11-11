package cli

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

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
