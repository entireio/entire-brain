package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func connectTestEnv(t *testing.T, repoDir string) EntireEnv {
	t.Helper()
	return EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
}

func runConnectCommand(t *testing.T, opts Options, args ...string) (string, string, error) {
	t.Helper()
	cmd := newConnectCommand(opts)
	cmd.SetArgs(args)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestConnectWithExplicitFlagsWritesBindingWithoutShellOut(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	hostCalls := 0
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name != "git" {
			hostCalls++
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts,
		"--repo-id", "01M2Q9WATHXVM46CD8Y4D3AX8W",
		"--api-url", "https://aws-us-east-2.api.partial.to/api/v1",
		"--jurisdiction", "us")
	if err != nil {
		t.Fatal(err)
	}
	if hostCalls != 0 {
		t.Fatalf("explicit flags must bypass the host CLI entirely, got %d calls", hostCalls)
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := readHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("binding must be written: present=%v err=%v", present, err)
	}
	if binding.RepoID != "01M2Q9WATHXVM46CD8Y4D3AX8W" || binding.Jurisdiction != "us" ||
		binding.BaseURL != "https://aws-us-east-2.api.partial.to/api/v1" {
		t.Fatalf("binding mismatch: %+v", binding)
	}
}

func TestConnectResolvesViaWhereami(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "whereami" {
			return []byte(`{"repo_id":"01AAAAAAAAAAAAAAAAAAAAAAAA","api_url":"https://eu.api.example.com/api/v1","jurisdiction":"eu"}` + "\nUpdate available! 1 -> 2\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	if _, _, err := runConnectCommand(t, opts); err != nil {
		t.Fatal(err)
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := readHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("binding must be written: present=%v err=%v", present, err)
	}
	if binding.RepoID != "01AAAAAAAAAAAAAAAAAAAAAAAA" || binding.Jurisdiction != "eu" ||
		binding.BaseURL != "https://eu.api.example.com/api/v1" {
		t.Fatalf("binding mismatch: %+v", binding)
	}
}

func TestConnectWhereamiUnavailableIsCleanError(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("unknown command")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts)
	if err == nil {
		t.Fatal("whereami failure without flags must error")
	}
	if !strings.Contains(err.Error(), "--repo-id") {
		t.Fatalf("error must point at the explicit flags, got: %v", err)
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, _ := readHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("no binding may be written on failure")
	}
}

func TestConnectRejectsInvalidAPIURL(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts,
		"--repo-id", "01M2Q9WATHXVM46CD8Y4D3AX8W",
		"--api-url", "http://evil.example/api/v1")
	if err == nil {
		t.Fatal("plaintext non-loopback URL must be rejected")
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, _ := readHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("no binding may be written for a rejected URL")
	}
}
