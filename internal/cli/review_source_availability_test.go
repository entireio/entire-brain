package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactAvailabilityRejectsEmptyCorruptAndSymlinkStores(t *testing.T) {
	for _, mode := range []string{"empty", "corrupt", "symlink", "directory"} {
		t.Run(mode, func(t *testing.T) {
			opts, repo, brain := factsAvailabilityFixture(t)
			path := filepath.Join(brain, filepath.FromSlash(factsFileRelPath("feature")))
			switch mode {
			case "empty":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(t.TempDir(), "outside")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			status, err := buildAvailableBrainStatusReport(context.Background(), opts, repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := statusFactsMissingBranches(t, status); len(got) != 1 || got[0] != "feature" {
				t.Fatalf("%s reported healthy: %v", mode, got)
			}
			if mode == "empty" {
				for _, args := range [][]string{{"recall", "postgres", "--no-semantic"}, {"recall", "postgres", "--no-semantic", "--json"}} {
					out, err := execute(t, NewRootCommand(opts), args...)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(out, factsFileName) || strings.Contains(out, "the answer may genuinely not be") {
						t.Fatalf("empty store hidden by recall: %s", out)
					}
				}
			}
		})
	}
}

func TestBriefFactWarningOnlyNamesTheBranchRead(t *testing.T) {
	opts, _, brain := factsAvailabilityFixture(t)
	facts, err := loadFacts(brain, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(brain, "other", facts); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(brain, opts.Now()); err != nil {
		t.Fatal(err)
	}
	removeFactStore(t, brain, "other")
	out, err := execute(t, NewRootCommand(opts), "brief", "postgres", "--json", "--no-semantic")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, factsFileName) {
			t.Fatalf("healthy branch falsely degraded: %v", report.Warnings)
		}
	}
}

func TestOverviewReportsUnreadableSemanticBoundaries(t *testing.T) {
	opts, brain := overviewHistoryFixture(t)
	manifest, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Semantic = &semanticSourceManifest{StorePath: "semantic/broken.sqlite"}
	if err := writeBrainRelativeFileAtomic(brain, "semantic/broken.sqlite", []byte("invalid sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(brain, *manifest); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"overview"}, {"overview", "--json"}} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"route", "tool", "workflow"} {
			if !strings.Contains(out, kind+" boundaries unavailable") {
				t.Fatalf("silent boundary failure: %s", out)
			}
		}
	}
}
