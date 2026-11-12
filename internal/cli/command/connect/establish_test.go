package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

// establishFixtureConfig wires a Config whose host CLI resolves placement
// successfully and whose sync outcome the test controls.
func establishFixtureConfig(t *testing.T, repoDir, brainDir string, sync func() (bool, bool, error)) Config {
	t.Helper()
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse"):
			return []byte(repoDir + "\n"), nil, nil
		case strings.Contains(joined, "repo view"):
			return []byte(`{"repoId":"repo-ulid","apiUrl":"https://cell.api.example.com"}`), nil, nil
		case strings.Contains(joined, "version"):
			return []byte("9.9.9\n"), nil, nil
		}
		return nil, nil, errors.New("unexpected command: " + joined)
	})
	return Config{
		Options: base.Options{Runner: runner},
		Deps: Deps{
			RepoStoragePaths: func(context.Context, base.CommandRunner, base.EntireEnv, string) (base.RepoStorage, error) {
				return base.RepoStorage{BrainDir: brainDir, Key: "gh/acme/repo"}, nil
			},
			AgentSurfaceTarget: func(base.Options, []string) string { return repoDir },
			InstallPushHook: func(context.Context, base.Options, string) (bool, string, error) {
				return false, "", nil
			},
			HostedFactsSyncAndPull: func(context.Context, io.Writer, base.Options, string, base.RepoStorage) (bool, bool, error) {
				return sync()
			},
		},
	}
}

func TestEstablishReportsBoundOnPostBindSyncFailure(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := filepath.Join(t.TempDir(), "brain")
	cfg := establishFixtureConfig(t, repoDir, brainDir, func() (bool, bool, error) { return true, false, nil })

	var stderr bytes.Buffer
	result, err := Establish(context.Background(), cfg, &stderr, "", "")

	if err == nil {
		t.Fatal("a failed first sync must surface as an error")
	}
	if !result.Bound {
		t.Fatalf("the binding precedes the sync, so a sync failure is still bound: %+v", result)
	}
	if _, present, readErr := brainstore.ReadHostedRepoBinding(brainDir); readErr != nil || !present {
		t.Fatalf("hosted binding must exist after a post-bind sync failure (present=%v err=%v)", present, readErr)
	}
}

func TestEstablishPreBindFailureWritesNothing(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := filepath.Join(t.TempDir(), "brain")
	cfg := establishFixtureConfig(t, repoDir, brainDir, func() (bool, bool, error) { return true, true, nil })
	failing := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if strings.Contains(name+" "+strings.Join(args, " "), "rev-parse") {
			return []byte(repoDir + "\n"), nil, nil
		}
		return nil, []byte("error: not onboarded\n"), errors.New("exit status 1")
	})
	cfg.Options.Runner = failing

	result, err := Establish(context.Background(), cfg, io.Discard, "", "")

	if err == nil || result.Bound {
		t.Fatalf("placement failure must be a pre-bind error, got bound=%v err=%v", result.Bound, err)
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(brainDir); present {
		t.Fatal("no binding may be written when placement resolution fails")
	}
}

func TestEstablishSucceedsAndReportsStorageKey(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := filepath.Join(t.TempDir(), "brain")
	cfg := establishFixtureConfig(t, repoDir, brainDir, func() (bool, bool, error) { return true, true, nil })

	result, err := Establish(context.Background(), cfg, io.Discard, "", "")

	if err != nil {
		t.Fatal(err)
	}
	if !result.Bound || result.StorageKey != "gh/acme/repo" {
		t.Fatalf("expected a bound result carrying the storage key, got %+v", result)
	}
}
