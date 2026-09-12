package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// entities_live_e2e_test.go is the one END-TO-END proof that the entity index
// works against the REAL semantic provider and REAL git, rather than against
// scripted runner output. It is feature-detected, not opt-in by env var: when
// `entire graph capabilities --json` is unavailable the test skips, so it runs
// wherever the provider happens to be installed (developer machines, the
// release image) and never fails a bare CI box.
//
// It is free: no agent, no network, no tokens. Everything lands in t.TempDir.

func liveGraphProviderAvailable(t *testing.T) bool {
	t.Helper()
	// The package pins the XDG roots (see main_test.go) so no test can resolve a
	// store under the real home. The installed CLI finds its plugins under the
	// data root, so this one test — the only one that drives the REAL provider —
	// gets the machine's own data root back for its duration.
	useHostXDGDataHome(t)
	binary, err := exec.LookPath("entire")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "graph", "capabilities", "--json").Output()
	if err != nil {
		return false
	}
	var capabilities map[string]any
	if err := json.Unmarshal(out, &capabilities); err != nil {
		return false
	}
	// A provider that cannot do a semantic diff cannot build this index.
	optional, _ := capabilities["optional_local_only_features"].(map[string]any)
	if semanticDiff, ok := optional["semantic_diff"].(bool); ok && !semanticDiff {
		return false
	}
	return true
}

func liveGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Entity Index", "GIT_AUTHOR_EMAIL=entity@entire.local",
		"GIT_COMMITTER_NAME=Entity Index", "GIT_COMMITTER_EMAIL=entity@entire.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestLiveEntityIndexBackfillAndHistory builds a real two-commit repository
// whose second commit modifies a known function, backfills the index through
// the real provider, and asserts `entities history` names exactly the commits
// that touched it — with the checkpoint trailer resolved.
func TestLiveEntityIndexBackfillAndHistory(t *testing.T) {
	if !liveGraphProviderAvailable(t) {
		t.Skip("entire graph capabilities unavailable; skipping the live entity-index journey")
	}
	repoDir := t.TempDir()
	liveGit(t, repoDir, "init", "-q", "-b", "main", ".")

	writeLiveFile := func(body string) {
		if err := os.WriteFile(filepath.Join(repoDir, "billing.go"), []byte(body), 0o600); err != nil {
			t.Fatalf("write source: %v", err)
		}
	}
	writeLiveFile("package billing\n\nfunc ChargeCard(id string) error {\n\treturn nil\n}\n")
	liveGit(t, repoDir, "add", "-A")
	liveGit(t, repoDir, "commit", "-q", "-m", "add ChargeCard")
	first := trimLine(liveGit(t, repoDir, "rev-parse", "HEAD"))

	// A commit that does NOT touch ChargeCard, to prove the index discriminates.
	if err := os.WriteFile(filepath.Join(repoDir, "unrelated.go"), []byte("package billing\n\nfunc Unrelated() {}\n"), 0o600); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	liveGit(t, repoDir, "add", "-A")
	liveGit(t, repoDir, "commit", "-q", "-m", "add an unrelated function")
	unrelated := trimLine(liveGit(t, repoDir, "rev-parse", "HEAD"))

	writeLiveFile("package billing\n\nfunc ChargeCard(id string) error {\n\tif id == \"\" {\n\t\treturn nil\n\t}\n\treturn nil\n}\n")
	liveGit(t, repoDir, "add", "-A")
	liveGit(t, repoDir, "commit", "-q", "-m", "harden ChargeCard\n\nEntire-Checkpoint: 0123456789ab\n")
	third := trimLine(liveGit(t, repoDir, "rev-parse", "HEAD"))

	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: ExecRunner{},
		Now:    time.Now,
	}

	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := NewRootCommand(opts)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	var build map[string]any
	if err := json.Unmarshal([]byte(run("entities", "backfill", "--json")), &build); err != nil {
		t.Fatalf("decode backfill: %v", err)
	}
	if indexed, _ := build["indexed"].(float64); indexed != 3 {
		t.Fatalf("live backfill indexed %v commits, want 3 (%v)", build["indexed"], build)
	}

	var result entityHistoryResult
	if err := json.Unmarshal([]byte(run("entities", "history", "ChargeCard", "--json")), &result); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("live history matches = %+v", result.Matches)
	}
	match := result.Matches[0]
	if match.Name != "ChargeCard" || match.Path != "billing.go" {
		t.Fatalf("live match = %+v", match)
	}
	commits := map[string]bool{}
	for _, occ := range match.Occurrences {
		commits[occ.Commit] = true
	}
	if !commits[first] || !commits[third] {
		t.Fatalf("live occurrences %v miss the commits that touched ChargeCard (%s, %s)", match.Occurrences, first, third)
	}
	if commits[unrelated] {
		t.Fatalf("live occurrences include the unrelated commit %s: %+v", unrelated, match.Occurrences)
	}
	for _, occ := range match.Occurrences {
		if occ.Commit != third {
			continue
		}
		if len(occ.CheckpointIDs) != 1 || occ.CheckpointIDs[0] != "0123456789ab" {
			t.Fatalf("the checkpoint trailer was not resolved: %+v", occ)
		}
	}

	// The stored delta document must be the frozen contract, verbatim.
	var delta map[string]any
	if err := json.Unmarshal([]byte(run("entities", "show", third)), &delta); err != nil {
		t.Fatalf("decode live delta: %v", err)
	}
	if delta["schema_version"] != "1.0" || delta["producer"] != "entire-graph" {
		t.Fatalf("live delta header = %v", delta)
	}
	if delta["head"] != third {
		t.Fatalf("live delta head = %v, want %s", delta["head"], third)
	}
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
