package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitFilterProbeObservesIncludedConfigChanges(t *testing.T) {
	gitHardenSkipUnsupported(t)
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "included", true: "linked-worktree-shared"}[linked], func(t *testing.T) {
			repo := gitHardenRepo(t)
			target := repo
			configFile := filepath.Join(t.TempDir(), "included.conf")
			if linked {
				target = filepath.Join(t.TempDir(), "linked")
				gitHardenRun(t, repo, "worktree", "add", "--detach", target, "HEAD")
				configFile = filepath.Join(repo, ".git", "config")
				if err := os.WriteFile(filepath.Join(target, "a.txt"), []byte("HELLO\nWORLD\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(configFile, nil, 0600); err != nil {
					t.Fatal(err)
				}
				gitHardenRun(t, repo, "config", "include.path", configFile)
			}
			if err := os.WriteFile(filepath.Join(target, ".gitattributes"), []byte("a.txt filter=later\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runner := ExecRunner{}
			if _, _, err := runner.Run(context.Background(), target, "git", "diff", "HEAD", "--", "a.txt"); err != nil {
				t.Fatal(err)
			}
			payload, sentinel := gitHardenPayload(t)
			gitHardenRun(t, repo, "config", "--file", configFile, "filter.later.clean", payload)
			diff, _, err := runner.Run(context.Background(), target, "git", "diff", "HEAD", "--", "a.txt")
			if err != nil {
				t.Fatal(err)
			}
			gitHardenAssertClean(t, sentinel, "filter added after successful probe")
			if !strings.Contains(string(diff), "HELLO") {
				t.Fatalf("diff lost content: %s", diff)
			}
		})
	}
}

func TestGitFilterProbePreservesTrustedGlobalFilter(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitHardenRun(t, repo, "config", "--global", "filter.trusted.clean", payload)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("a.txt filter=trusted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (ExecRunner{}).Run(context.Background(), repo, "git", "diff", "HEAD", "--", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("trusted global filter was disabled: %v", err)
	}
}
