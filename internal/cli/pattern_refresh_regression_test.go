package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCommandPatternsRefreshTextContract(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	out, stderr, err := executeSplit(t, NewRootCommand(opts), "patterns", "refresh", repoDir)
	if err != nil {
		t.Fatalf("refresh: %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Patterns == nil || manifest.Sources.Patterns.Episodes == 0 {
		t.Fatal("refresh did not publish a nonempty episode source")
	}
	source := manifest.Sources.Patterns
	r := source.Reinforcement
	want := fmt.Sprintf("patterns: rebuilt %d episode(s) (success %d, corrected %d, neutral %d)\n", source.Episodes, r.Success, r.Corrected, r.Neutral)
	if out != want || stderr != "" {
		t.Fatalf("refresh output: stdout=%q stderr=%q; want stdout=%q and empty stderr", out, stderr, want)
	}
	if info, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("successful refresh did not publish corpus: %v", err)
	}
}

func TestRootCommandPatternsRefreshFailureDoesNotReportSuccess(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			opts, repoDir, brainDir := commandRegressionFixture(t)
			// A directory at the database path is a portable, real publication
			// failure, unlike permission bits which differ across OSes and users.
			corpus := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
			if err := os.MkdirAll(corpus, 0o700); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(corpus, "keep")
			if err := os.WriteFile(canary, []byte("existing data"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"patterns", "refresh", repoDir}
			if asJSON {
				args = append(args, "--json")
			}
			out, _, err := executeSplit(t, NewRootCommand(opts), args...)
			if err == nil || !strings.Contains(err.Error(), "pattern corpus:") {
				t.Fatalf("refresh should fail at corpus build: err=%v stdout=%q", err, out)
			}
			if out != "" {
				t.Fatalf("failed refresh published success output: %q", out)
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "existing data" {
				t.Fatalf("failed refresh changed existing data: %q, %v", data, err)
			}
		})
	}
}

func TestRootCommandPatternsRefreshMissingTranscriptPreservesState(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	if _, _, err := executeSplit(t, NewRootCommand(opts), "patterns", "refresh", repoDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(brainDir, "sessions", "main", "session-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	// Lock-owner metadata changes on acquisition; compare the published
	// pattern artifacts and source manifest instead.
	before := treeContentHash(t, filepath.Join(brainDir, "patterns"))
	manifestBefore, err := os.ReadFile(filepath.Join(brainDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeSplit(t, NewRootCommand(opts), "patterns", "refresh", repoDir, "--json")
	if err == nil || !strings.Contains(err.Error(), "session-1.jsonl") {
		t.Fatalf("refresh should reject missing transcript: err=%v stdout=%q", err, out)
	}
	if out != "" {
		t.Fatalf("failed refresh published success output: %q", out)
	}
	if after := treeContentHash(t, filepath.Join(brainDir, "patterns")); after != before {
		t.Fatal("failed source rebuild changed published pattern artifacts")
	}
	manifestAfter, err := os.ReadFile(filepath.Join(brainDir, "manifest.json"))
	if err != nil || string(manifestAfter) != string(manifestBefore) {
		t.Fatalf("failed source rebuild changed source manifest: %v", err)
	}
}
