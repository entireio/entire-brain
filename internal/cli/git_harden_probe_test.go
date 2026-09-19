package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitFilterProbeFailureDoesNotRunOrPoisonCache(t *testing.T) {
	gitHardenSkipUnsupported(t)
	for _, mode := range []string{"buffered", "with-env", "streaming"} {
		for _, failure := range []string{"cancelled", "exit-error", "exit-one-diagnostic"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				repo, sentinel := gitHardenFilterRepo(t, "probe", "clean")
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				fail := filepath.Join(bin, "fail")
				launched := filepath.Join(bin, "launched")
				if err := os.WriteFile(fail, nil, 0600); err != nil {
					t.Fatal(err)
				}
				// Only the enumeration fails; an incorrectly launched diff still runs
				// real git and would execute the repository's harmless marker filter.
				quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
				code := "2"
				if failure == "exit-one-diagnostic" {
					code = "1"
				}
				script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = '--get-regexp' ]; then\n  if [ -f " + quote(fail) + " ]; then echo 'probe failed' >&2; exit " + code + "; fi\n  exec " + quote(realGit) + " \"$@\"\n fi\ndone\ntouch " + quote(launched) + "\nexec " + quote(realGit) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				run := func(ctx context.Context) ([]byte, error) {
					args := []string{"diff", "HEAD", "--", "a.txt"}
					if mode == "streaming" {
						s, err := (ExecRunner{}).Stream(ctx, repo, "git", args...)
						if err != nil {
							return nil, err
						}
						defer s.Close()
						out, readErr := io.ReadAll(s.Stdout())
						_, err = s.Wait()
						if readErr != nil {
							return out, readErr
						}
						return out, err
					}
					if mode == "with-env" {
						out, _, err := (ExecRunner{}).RunWithEnv(ctx, repo, map[string]string{"F23_TEST": "1"}, "git", args...)
						return out, err
					}
					out, _, err := (ExecRunner{}).Run(ctx, repo, "git", args...)
					return out, err
				}
				ctx := context.Background()
				if failure == "cancelled" {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = cancelled
				}
				if _, err := run(ctx); err == nil {
					t.Error("failed filter probe allowed command to run")
				}
				if _, err := os.Stat(launched); !os.IsNotExist(err) {
					t.Errorf("command launched after failed probe: %v", err)
				}
				gitHardenAssertClean(t, sentinel, "filter after failed probe")
				if _, cached := repoFilterCache.Load(repo); cached {
					t.Error("failed filter probe populated cache")
				}
				if err := os.Remove(fail); err != nil {
					t.Fatal(err)
				}
				out, err := run(context.Background())
				if err != nil {
					t.Fatalf("healthy retry: %v", err)
				}
				gitHardenAssertClean(t, sentinel, "filter after probe retry")
				if !strings.Contains(string(out), "HELLO") {
					t.Fatalf("retry lost diff: %s", out)
				}
			})
		}
	}
}

// Enumeration outside a repository still succeeds (or reports no matching
// keys), so Git operations that create a repository remain available.
func TestGitFilterProbeAllowsInitOutsideRepository(t *testing.T) {
	gitHardenSkipUnsupported(t)
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"init", "--bare", "--template=", "."}
			if streaming {
				stream, err := (ExecRunner{}).Stream(context.Background(), dir, "git", args...)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if _, err := io.Copy(io.Discard, stream.Stdout()); err != nil {
					t.Fatal(err)
				}
				if _, err := stream.Wait(); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := (ExecRunner{}).Run(context.Background(), dir, "git", args...); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
				t.Fatalf("repository was not created: %v", err)
			}
		})
	}
}

func TestGitFilterProbeFailureEvictsPreviousSuccess(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, _ := gitHardenFilterRepo(t, "probe", "clean")
	t.Cleanup(func() { repoFilterCache.Delete(repo) })
	if _, err := repoFilterDriverOverrides(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(repo, ".git", "config")
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(original, []byte("\n[invalid\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := repoFilterDriverOverrides(context.Background(), repo); err == nil {
		t.Fatal("invalid config probe should fail")
	}
	if _, cached := repoFilterCache.Load(repo); cached {
		t.Error("failed probe retained the previous successful cache entry")
	}
	if err := os.WriteFile(config, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(config, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repoFilterDriverOverrides(ctx, repo); err == nil {
		t.Fatal("restored stat signature reused stale cache instead of probing")
	}
	if overrides, err := repoFilterDriverOverrides(context.Background(), repo); err != nil || len(overrides) == 0 {
		t.Fatalf("healthy retry: overrides=%v err=%v", overrides, err)
	}
}
