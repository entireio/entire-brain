package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func regressionPrivacyFixture(t *testing.T) retrievalEmissionPrivacyFixture {
	t.Helper()
	f := writeRetrievalEmissionPrivacyFixture(t)
	manifest, err := loadBrainManifest(f.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.BrainDir, manifest.Sources.Sessions.Sessions[0].TranscriptPath)
	if err := os.WriteFile(path, []byte(`{"text":"pkg/review.go scopeBaseRef+\"..PRIVATE\""}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.Options.Env.RepoRoot, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Options.Env.RepoRoot, "pkg/review.go"), []byte("package x\nfunc f() string { return \"master..PRIVATE\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func runRegressionPrivacySurface(cmd *cobra.Command, f retrievalEmissionPrivacyFixture, surface string, json bool) error {
	ro := regressionDetectorOptions{limit: 10, json: json}
	query := "fix scopeBaseRef"
	manifest := workspaceManifest{Name: "privacy", Repos: []workspaceRepo{{RepoKey: f.RepoKey, LocalPathHint: f.Options.Env.RepoRoot}}}
	switch surface {
	case "regressions":
		return runRegressionDetect(context.Background(), cmd, f.Options, ro, query)
	case "review":
		return runBrainReview(context.Background(), cmd, f.Options, ro, query)
	case "workspace regressions":
		return runWorkspaceRegressionsManifest(cmd, f.Options, ro, manifest, query, nil)
	default:
		return runWorkspaceReviewManifest(cmd, f.Options, ro, manifest, query, nil)
	}
}

func TestRegressionPrivacySurfaces(t *testing.T) {
	for _, surface := range []string{"regressions", "review", "workspace regressions", "workspace review"} {
		for _, json := range []bool{false, true} {
			name := surface + "/text"
			if json {
				name = surface + "/json"
			}
			t.Run(name, func(t *testing.T) {
				f := regressionPrivacyFixture(t)
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				var out bytes.Buffer
				cmd.SetOut(&out)
				if err := runRegressionPrivacySurface(cmd, f, surface, json); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "..PRIVATE") {
					t.Fatalf("positive control missing assertion: %s", out.String())
				}
				if err := withBrainWriteLock(f.BrainDir, func() error {
					plan, err := buildSessionPurgePlan(f.BrainDir, f.SessionID)
					if err != nil {
						return err
					}
					return executeSessionCleanup(f.BrainDir, f.SessionID, plan, time.Now(), "test exclusion", true)
				}); err != nil {
					t.Fatal(err)
				}
				m, err := loadBrainManifest(f.BrainDir)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(f.BrainDir, m.Sources.Sessions.Sessions[0].TranscriptPath)); err != nil {
					t.Fatalf("excluded transcript should remain: %v", err)
				}
				out.Reset()
				if err := runRegressionPrivacySurface(cmd, f, surface, json); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out.String(), "..PRIVATE") || strings.Contains(out.String(), "privacy-race.jsonl") {
					t.Fatalf("excluded assertion leaked: %s", out.String())
				}
			})
		}
	}
}

func TestRegressionPrivacyLateExclusion(t *testing.T) {
	for _, surface := range []string{"regressions", "review", "workspace regressions", "workspace review"} {
		t.Run(surface, func(t *testing.T) {
			f := regressionPrivacyFixture(t)
			called := installLateRetrievalTombstone(t, f)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			err := runRegressionPrivacySurface(cmd, f, surface, true)
			requireLatePrivacyEmissionFailure(t, err, &out, called)
		})
	}
}

func TestRegressionPrivacyOutputLock(t *testing.T) {
	for _, surface := range []string{"regressions", "review", "workspace regressions", "workspace review"} {
		t.Run(surface, func(t *testing.T) {
			f := regressionPrivacyFixture(t)
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			writes := 0
			cmd.SetOut(evidenceInspectWriter(func(b []byte) (int, error) {
				writes++
				unlock, err := acquireBrainWriteLockTimeout(f.BrainDir, 10*time.Millisecond)
				if err == nil {
					unlock()
					t.Error("output emitted without privacy lock")
				}
				return 0, io.ErrClosedPipe
			}))
			err := runRegressionPrivacySurface(cmd, f, surface, true)
			if !errors.Is(err, io.ErrClosedPipe) || writes != 1 {
				t.Fatalf("write result %v, writes %d", err, writes)
			}
		})
	}
}

func TestRegressionPrivacySkipsDerivedAndFailsClosed(t *testing.T) {
	for _, state := range []string{"derived", "manifest", "tombstones"} {
		t.Run(state, func(t *testing.T) {
			f := regressionPrivacyFixture(t)
			m, err := loadBrainManifest(f.BrainDir)
			if err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(f.BrainDir, m.Sources.Sessions.Sessions[0].TranscriptPath)
			if state == "derived" {
				body, err := os.ReadFile(transcript)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(transcript); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.BrainDir, "facts", "stale.txt")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if state == "manifest" {
				if err := os.WriteFile(filepath.Join(f.BrainDir, exportManifestFileName), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				stones := loadSessionTombstones(f.BrainDir)
				stones.Excluded[f.SessionID] = sessionTombstone{At: time.Now()}
				if err := saveSessionTombstones(f.BrainDir, stones); err != nil {
					t.Fatal(err)
				}
				// A malformed policy must never turn exclusion into an unguarded scan.
				if err := os.WriteFile(filepath.Join(f.BrainDir, sessionTombstonesPath), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			anomalies, scanned, warnings, _ := detectRegressionAnomaliesCapped(f.BrainDir, f.Options.Env.RepoRoot, nil, "fix scopeBaseRef", 10, false)
			if len(anomalies) != 0 || scanned != 0 {
				t.Fatalf("unsafe scan: %+v scanned=%d warnings=%v", anomalies, scanned, warnings)
			}
			if state != "derived" && len(warnings) == 0 {
				t.Fatal("unreadable privacy state must report the failed scan")
			}
		})
	}
}
