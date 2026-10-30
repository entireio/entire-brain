package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestStatusTextDistinguishesFreshnessFromIntegrity(t *testing.T) {
	opts, _, _ := statusTruthFixture(t)
	for _, flags := range [][]string{nil, {"--verbose"}, {"--details"}} {
		out, err := execute(t, NewRootCommand(opts), append([]string{"status"}, flags...)...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Integrity not checked") || !strings.Contains(out, "doctor`") {
			t.Fatalf("status must name the audit boundary and doctor: %s", out)
		}
	}
}

// Status must not read dirty snapshot contents, even when a stored fingerprint
// could match. Strict reports must still detect an untracked content-only edit.
func TestStatusSkipsDirtySnapshotFingerprints(t *testing.T) {
	for _, layer := range []string{"seed"} {
		t.Run(layer, func(t *testing.T) {
			opts, repoDir, brainDir := statusTruthFixture(t)
			ctx := context.Background()
			if err := runSemanticIndex(ctx, &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
				t.Fatal(err)
			}
			runner := opts.Runner.(*fakeCommandRunner)
			runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? new.go\n"}
			runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: "?? new.go\n"}
			runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
			runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
			untracked := filepath.Join(repoDir, "new.go")
			if err := os.WriteFile(untracked, []byte("package before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			hash, err := worktreeFingerprint(ctx, runner, repoDir)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := bmanifest.Load(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			if layer == "semantic" {
				manifest.Sources.Semantic.WorktreeMode = "worktree"
				manifest.Sources.Semantic.WorktreeHash = hash
			} else {
				manifest.Sources.Seed = &seedSourceManifest{GeneratedAt: opts.Now(), Commit: "aaa111", WorktreeMode: "worktree", WorktreeHash: hash}
				manifest.Sources.Docs = &docSourceManifest{GeneratedAt: opts.Now(), IndexPath: docstore.IndexPath}
				if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, docstore.IndexPath, []byte(`{"records":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
				t.Fatal(err)
			}
			axis := func(report brainStatusReport) staleAxis {
				if layer == "semantic" {
					return report.Semantic.Freshness.Axes["worktree"]
				}
				return report.Retrieval.Freshness.Axes["seed"]
			}
			for _, changed := range []bool{false, true} {
				if changed {
					if err := os.WriteFile(untracked, []byte("package after\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for _, flags := range [][]string{{"--json"}, {"--json", "--verbose"}, {"--json", "--details"}} {
					runner.calls = nil
					out, err := execute(t, NewRootCommand(opts), append([]string{"status"}, flags...)...)
					if err != nil {
						t.Fatalf("status: %v\n%s", err, out)
					}
					var report brainStatusReport
					if err := json.Unmarshal([]byte(out), &report); err != nil {
						t.Fatal(err)
					}
					if got := axis(report); got.State != "degraded" || !strings.Contains(got.Detail, "not verified") {
						t.Errorf("dirty snapshot must be unverified, changed=%v: %+v", changed, got)
					}
					freshness := report.Semantic.Freshness
					if layer == "seed" {
						freshness = report.Retrieval.Freshness
					}
					if freshness.Severity != "degraded" {
						t.Errorf("unverified snapshot severity = %q, want degraded", freshness.Severity)
					}
					for _, call := range runner.calls {
						if call.name == "git" && strings.Contains(strings.Join(call.args, " "), "--binary") {
							t.Errorf("status read worktree contents: %v", call.args)
						}
					}
				}
				out, err := execute(t, NewRootCommand(opts), "status", "--json", "--fail-on", "release")
				if !errors.Is(err, errSemanticAuditGate) {
					t.Errorf("release gate accepted unverified content: %v\n%s", err, out)
				}
				runner.calls = nil
				strict, err := buildBrainStatusReport(ctx, opts, repoDir)
				if err != nil {
					t.Fatal(err)
				}
				want := "dirty-indexed"
				if changed {
					want = "dirty-stale"
				}
				if got := axis(strict).State; got != want {
					t.Errorf("strict snapshot state = %s, want %s", got, want)
				}
				if !fakeRunnerCalled(runner, "git", "diff", "--binary", "HEAD") || !fakeRunnerCalled(runner, "git", "diff", "--cached", "--binary", "HEAD") {
					t.Error("strict report stopped fingerprinting dirty contents")
				}
			}
			for _, tc := range []struct {
				name, mode, hash, head, status, want string
			}{
				{"missing fingerprint", "worktree", "", "aaa111", "?? new.go\n", "unsafe"},
				{"committed index", "tracked", hash, "aaa111", "?? new.go\n", "dirty-unindexed"},
				{"cleaned overlay", "worktree", hash, "aaa111", "", "worktree-overlay-stale"},
				{"head mismatch", "worktree", hash, "newhead", "?? new.go\n", "stale"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if layer == "semantic" {
						manifest.Sources.Semantic.WorktreeMode = tc.mode
						manifest.Sources.Semantic.WorktreeHash = tc.hash
					} else {
						manifest.Sources.Seed.WorktreeMode = tc.mode
						manifest.Sources.Seed.WorktreeHash = tc.hash
					}
					if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
						t.Fatal(err)
					}
					runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: tc.head + "\n"}
					runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: tc.status}
					runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: tc.status}
					runner.calls = nil
					report, err := buildAvailableBrainStatusReport(ctx, opts, repoDir)
					if err != nil {
						t.Fatal(err)
					}
					got := axis(report).State
					if layer == "semantic" && tc.name == "head mismatch" {
						got = report.Semantic.Freshness.Axes["head"].State
					}
					if got != tc.want {
						t.Errorf("state = %q, want %q", got, tc.want)
					}
					for _, call := range runner.calls {
						if call.name == "git" && strings.Contains(strings.Join(call.args, " "), "--binary") {
							t.Errorf("status read worktree contents: %v", call.args)
						}
					}
				})
			}
		})
	}
}
