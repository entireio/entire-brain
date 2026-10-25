package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoutedExportPartialMetadataPreservesPublishedBrain(t *testing.T) {
	for _, failure := range []struct {
		name     string
		response fakeCommandResponse
	}{
		{"read failure", fakeCommandResponse{err: errors.New("temporary metadata unavailable")}},
		{"invalid JSON", fakeCommandResponse{stdout: "{"}},
		{"session error", fakeCommandResponse{stdout: `{"checkpoint_id":"aaa111aaa111","sessions":[{"session_id":"first-session","error":"unavailable"}]}`}},
		{"missing session ID", fakeCommandResponse{stdout: `{"checkpoint_id":"aaa111aaa111","sessions":[{"index":0}]}`}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			env := EntireEnv{RepoRoot: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir(), PluginCacheDir: t.TempDir()}
			listKey := fakeRoutedCheckpointListKey("entire-test", true)
			firstKey := fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111")
			firstDetail := fakeCommandResponse{stdout: `{"checkpoint_id":"aaa111aaa111","branch":"main","sessions":[{"session_id":"first-session","branch":"main"}]}`}
			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
				fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "https://github.com/example/partial-export.git\n"},
				listKey:  {stdout: `[{"checkpoint_id":"aaa111aaa111","is_logs_only":true},{"checkpoint_id":"bbb222bbb222","is_logs_only":true}]`},
				firstKey: firstDetail,
				fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "bbb222bbb222"):                               {stdout: `{"checkpoint_id":"bbb222bbb222","branch":"main","sessions":[{"session_id":"second-session","branch":"main"}]}`},
				fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "aaa111aaa111"): {stdout: "first captured content\n"},
				fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "bbb222bbb222"): {stdout: "second captured content\n"},
			}}
			run := func(extra ...string) (string, error) {
				t.Helper()
				cmd := NewRootCommand(Options{Env: env, Runner: runner, Now: time.Now})
				return execute(t, cmd, append([]string{"refresh", "sessions", "--entire-binary", "entire-test", "--history-index"}, extra...)...)
			}
			storage, err := repoStoragePaths(context.Background(), runner, env, env.RepoRoot)
			if err != nil {
				t.Fatal(err)
			}
			// Seed a legacy materialized brain. Routed-only discovery may
			// still export bundles, but cannot replace durable local sources.
			if out, err := run("--output", storage.BrainDir); err != nil {
				t.Fatalf("initial bundle: %v %s", err, out)
			}
			manifest, err := loadBrainManifest(storage.BrainDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Sessions) != 2 {
				t.Fatalf("initial sessions = %d", len(manifest.Sessions))
			}
			if err := writeExportCursor(storage.HeadPath, *manifest); err != nil {
				t.Fatal(err)
			}
			// Capture every published artifact, including the history index and cursor.
			before := map[string][]byte{}
			for _, root := range []string{storage.BrainDir, storage.HeadPath} {
				err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if entry.IsDir() {
						return nil
					}
					data, readErr := os.ReadFile(path)
					if readErr != nil {
						return readErr
					}
					before[path] = data
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			runner.responses[firstKey] = failure.response
			out, err := run()
			if err == nil || !strings.Contains(err.Error(), checkpointScopeIncompleteCode) {
				t.Errorf("partial persistent export should fail with scope-incomplete error: %v %s", err, out)
			}
			if err != nil {
				for _, want := range []string{"skipped checkpoint aaa111aaa111", "--output"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("partial export diagnostic missing %q: %v", want, err)
					}
				}
			}
			for path, want := range before {
				got, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Errorf("published artifact lost: %s: %v", path, readErr)
				} else if !bytes.Equal(got, want) {
					t.Errorf("published artifact changed after partial discovery: %s", path)
				}
			}
			// A one-shot export can still provide the readable subset with a warning.
			output := filepath.Join(t.TempDir(), "export")
			if out, err := run("--output", output); err != nil {
				t.Fatalf("explicit partial export: %v %s", err, out)
			}
			partial, err := loadBrainManifest(output)
			if err != nil {
				t.Fatal(err)
			}
			if len(partial.Sessions) != 1 || !strings.Contains(strings.Join(partial.Warnings, "\n"), "skipped checkpoint aaa111aaa111") {
				t.Fatalf("explicit subset missing session/warning: %+v", partial)
			}
			// A complete routed catalog still cannot justify deleting local
			// materializations until checkpoint refs are available locally.
			runner.responses[firstKey] = firstDetail
			if out, err := run(); err == nil || !strings.Contains(err.Error(), "local checkpoint refs") {
				t.Fatalf("routed-only retry: %v %s", err, out)
			}
			for path, want := range before {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("routed retry changed %s: %v", path, err)
				}
			}

		})
	}
}
