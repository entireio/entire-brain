package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestCappedHistoryReviewSurfacesDoNotClaimCleanResults(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	var history, current strings.Builder
	current.WriteString("package x\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&history, `{"text":"in pkg/a.go the range must stay scopeBaseRef+\"operand%04d\""}`+"\n", i)
		fmt.Fprintf(&history, `{"text":"keep state.scopeBaseRef = rhsval%04d after the update"}`+"\n", i)
		fmt.Fprintf(&current, "var x%d = scopeBaseRef+\"operand%04d\"\n", i, i)
	}
	repo, key := writeLocalWorkspaceBrainRepo(t, env, history.String(), "pkg/a.go", current.String())
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	opts.Env.RepoRoot = repo
	brainDir, err := brainDirForKey(env, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "z_tail.jsonl"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Confirm the fixture really compares a file without finding an anomaly.
	anomalies, scanned, _, caps := detectRegressionAnomaliesCapped(brainDir, repo, nil, "fix scopeBaseRef", 20, false)
	if !caps.Truncated() || scanned == 0 || len(anomalies) != 0 {
		t.Fatalf("fixture: caps=%+v scanned=%d anomalies=%d", caps, scanned, len(anomalies))
	}
	// The fake status resolver uses a fixed repo key. Mirror the brain there.
	statusKey := "gh/example/repo"
	statusBrain, err := brainDirForKey(env, statusKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(statusBrain, "sessions"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusBrain, "sessions", "s.jsonl"), []byte(history.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusBrain, "sessions", "z_tail.jsonl"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runBrainReview(context.Background(), cmd, opts, regressionDetectorOptions{limit: 20, json: true}, "fix scopeBaseRef"); err != nil {
		t.Fatal(err)
	}
	var report reviewReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Summary, "PARTIAL") {
		t.Errorf("brain review claims a complete result: %s", report.Summary)
	}
	manifest := workspaceManifest{Name: "capped", Repos: []workspaceRepo{{RepoKey: key, LocalPathHint: repo}}}
	opts.Runner = &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	for _, review := range []bool{false, true} {
		out.Reset()
		ro := regressionDetectorOptions{limit: 20}
		var err error
		if review {
			err = runWorkspaceReviewManifest(cmd, opts, ro, manifest, "fix scopeBaseRef", nil)
		} else {
			err = runWorkspaceRegressionsManifest(cmd, opts, ro, manifest, "fix scopeBaseRef", nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "PARTIAL") {
			t.Errorf("workspace surface (review=%t) claims a complete result: %s", review, out.String())
		}
	}
}
