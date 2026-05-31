package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSeedWritesDeterministicBrain(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	runner := seedFixtureRunner(repoDir)

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "seed", repoDir)
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	brainDir := filepath.Join(dataDir, brainDirName, "gh", "example", "repo")
	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil {
		t.Fatalf("seed source missing: %+v", manifest)
	}
	if len(manifest.Sources.Seed.Commands) == 0 {
		t.Fatalf("commands missing from package.json: %+v", manifest.Sources.Seed)
	}
	if !hasSeedCommand(manifest.Sources.Seed.Commands, "mise run fmt") || !hasSeedCommand(manifest.Sources.Seed.Commands, "mise run test:ci") {
		t.Fatalf("mise commands missing: %+v", manifest.Sources.Seed.Commands)
	}
	if !hasSeedDocument(manifest.Sources.Seed.Documents, "README.md") || !hasSeedDocument(manifest.Sources.Seed.Documents, "CLAUDE.md") || !hasSeedDocument(manifest.Sources.Seed.Documents, ".github/copilot-instructions.md") {
		t.Fatalf("expected docs missing: %+v", manifest.Sources.Seed.Documents)
	}
	if _, err := os.Stat(filepath.Join(brainDir, seedDirName, seedDocsDirName, "README.md")); err != nil {
		t.Fatalf("copied readme missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, brainDirName, "gh", "example", "repo", seedCursorFileName)); err != nil {
		t.Fatalf("seed cursor missing: %v", err)
	}
	indexData, err := os.ReadFile(filepath.Join(brainDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatalf("read file index: %v", err)
	}
	if !strings.Contains(string(indexData), "denied path") {
		t.Fatalf("file index did not record denied files:\n%s", indexData)
	}
}

func TestSeedAgentCommandArgsCodexUsesStructuredReadOnlyExec(t *testing.T) {
	args, err := seedAgentCommandArgs("/repo", "quick", seedCommandOptions{agent: "codex"})
	if err != nil {
		t.Fatalf("codex args: %v", err)
	}
	joined := strings.Join(args, "\x00")
	for _, want := range []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("codex args missing %q: %#v", want, args)
		}
	}
	if strings.Contains(joined, "--output-schema") {
		t.Fatalf("codex args should rely on prompt plus local validation, not --output-schema: %#v", args)
	}
	if !strings.Contains(args[len(args)-1], "return only raw JSON") {
		t.Fatalf("codex prompt missing structured-output instruction:\n%s", args[len(args)-1])
	}
}

func TestSeedAgentCommandArgsClaudeCodeDisablesToolsAndSessions(t *testing.T) {
	args, err := seedAgentCommandArgs("/repo", "deep", seedCommandOptions{agent: "claude-code"})
	if err != nil {
		t.Fatalf("claude-code args: %v", err)
	}
	joined := strings.Join(args, "\x00")
	for _, want := range []string{"claude", "--print", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "\x00\x00", "--system-prompt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claude-code args missing %q: %#v", want, args)
		}
	}
	if strings.Contains(joined, "--bare") {
		t.Fatalf("claude-code args should not use --bare because it bypasses logged-in Claude auth: %#v", args)
	}
	if !strings.Contains(args[len(args)-1], "return only raw JSON") {
		t.Fatalf("claude-code prompt missing structured-output instruction:\n%s", args[len(args)-1])
	}
}

func TestValidateSeedAgentOutputRequiresSuccessSchema(t *testing.T) {
	tests := []struct {
		name string
		out  seedAgentOutput
	}{
		{name: "failed status", out: seedAgentOutput{SchemaVersion: 1, Status: "failed", Artifacts: map[string]string{"quick-overview.md": "x"}}},
		{name: "missing schema", out: seedAgentOutput{Status: "success", Artifacts: map[string]string{"quick-overview.md": "x"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSeedAgentOutput(test.out, "quick"); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
	if err := validateSeedAgentOutput(seedAgentOutput{SchemaVersion: 1, Status: "success"}, "quick"); err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
}

func TestValidateSeedAgentArtifactSetRejectsExtras(t *testing.T) {
	artifacts := map[string]string{
		"quick-overview.md": "ok",
		"extra.md":          "unexpected",
	}
	err := validateSeedAgentArtifactSet(artifacts, []string{"quick-overview.md"}, "quick")
	if err == nil || !strings.Contains(err.Error(), "unexpected artifact") {
		t.Fatalf("artifact validation err = %v", err)
	}
}

func TestSeedRejectsNonEmptyOutputUnlessForced(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginConfigDir: filepath.Join(t.TempDir(), "config")},
		Runner:  seedFixtureRunner(repoDir),
	})
	_, err := execute(t, cmd, "seed", "--output", outputDir, repoDir)
	if err == nil || !strings.Contains(err.Error(), "output directory is not empty") {
		t.Fatalf("seed should reject non-empty output, got %v", err)
	}

	cmd = NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginConfigDir: filepath.Join(t.TempDir(), "config")},
		Runner:  seedFixtureRunner(repoDir),
	})
	if _, err := execute(t, cmd, "seed", "--force", "--output", outputDir, repoDir); err != nil {
		t.Fatalf("seed --force: %v", err)
	}
}

func TestSeedWorktreeIncludesSelectedUntrackedDocs(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "AGENTS.md"), []byte("# Agents\n"), 0o600); err != nil {
		t.Fatalf("write agents: %v", err)
	}
	outputDir := filepath.Join(t.TempDir(), "seed")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "ls-files", "--others", "--exclude-standard")] = fakeCommandResponse{stdout: "AGENTS.md\n.env.local\n"}
	cmd := NewRootCommand(Options{Version: "test-version", Runner: runner})
	if _, err := execute(t, cmd, "seed", "--worktree", "--output", outputDir, repoDir); err != nil {
		t.Fatalf("seed --worktree: %v", err)
	}
	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(outputDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Sources.Seed.WorktreeMode != "worktree" {
		t.Fatalf("worktree mode = %q", manifest.Sources.Seed.WorktreeMode)
	}
	if !hasSeedDocument(manifest.Sources.Seed.Documents, "AGENTS.md") {
		t.Fatalf("AGENTS.md was not included: %+v", manifest.Sources.Seed.Documents)
	}
}

func TestSeedAgentCommandQuickAndDeep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses sh")
	}
	repoDir := seedFixtureRepo(t)
	outputDir := filepath.Join(t.TempDir(), "seed")
	agent := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/sh
python3 -c 'import json,sys; p=json.load(sys.stdin); phase=p["phase"]; arts={"quick-overview.md":"quick"} if phase=="quick" else {"overview.md":"overview","architecture.md":"arch","risks.md":"risks","maintenance-guide.md":"maint","open-questions.md":"questions"}; print(json.dumps({"schema_version":1,"status":"success","model":"fake","artifacts":arts}))'
`
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Runner: seedFixtureRunner(repoDir)})
	if _, err := execute(t, cmd, "seed", "--output", outputDir, "--agent", "command", "--agent-command", agent, repoDir); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, seedDirName, seedAgentDirName, "quick-overview.md")); err != nil {
		t.Fatalf("quick artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, seedDirName, seedAgentDirName, "maintenance-guide.md")); err != nil {
		t.Fatalf("deep artifact missing: %v", err)
	}
}

func TestSeedAgentRejectsPathTraversalArtifact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses sh")
	}
	repoDir := seedFixtureRepo(t)
	outputDir := filepath.Join(t.TempDir(), "seed")
	agent := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/sh
printf '{"schema_version":1,"status":"success","artifacts":{"../bad.md":"bad","quick-overview.md":"quick"}}'
`
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatalf("write agent: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Runner: seedFixtureRunner(repoDir)})
	out, err := execute(t, cmd, "seed", "--output", outputDir, "--agent", "command", "--agent-command", agent, "--require-agent", repoDir)
	if err == nil {
		t.Fatalf("seed should reject unsafe artifact:\n%s", out)
	}
}

func TestSeedHistoryBaselineUsesRootCommit(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	outputDir := t.TempDir()
	oldestSession := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := writeBrainSessionSource(outputDir, "gh/example/repo", exportManifest{
		GeneratedAt:        oldestSession,
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		CheckpointLimit:    10,
		CheckpointsScanned: 1,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111aaa111",
			CreatedAt:        oldestSession,
			TranscriptPath:   "sessions/main/session.jsonl",
		}},
	}); err != nil {
		t.Fatalf("write session source: %v", err)
	}
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "rev-list", "--max-parents=0", "HEAD")] = fakeCommandResponse{stdout: "root-newer\nroot-older\n"}
	runner.responses[fakeCommandKey("git", "show", "-s", "--format=%aI", "root-newer")] = fakeCommandResponse{stdout: "2025-01-01T00:00:00Z\n"}
	runner.responses[fakeCommandKey("git", "show", "-s", "--format=%aI", "root-older")] = fakeCommandResponse{stdout: "2024-01-01T00:00:00Z\n"}

	baseline := buildSeedHistoryBaseline(context.Background(), runner, repoDir, outputDir)
	if baseline.OldestCommitAt == nil || !baseline.OldestCommitAt.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("oldest commit = %+v, want 2024 root", baseline.OldestCommitAt)
	}
	if !baseline.SeedRequired {
		t.Fatalf("seed should be required when root commit predates session: %+v", baseline)
	}
}

func TestSeedHistoryCoverageFindsLaterMissingSessionCommits(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	outputDir := t.TempDir()
	oldestSession := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := writeBrainSessionSource(outputDir, "gh/example/repo", exportManifest{
		GeneratedAt:        oldestSession,
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		CheckpointLimit:    10,
		CheckpointsScanned: 2,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111aaa111",
			CreatedAt:        oldestSession,
			TranscriptPath:   "sessions/main/session.jsonl",
		}},
	}); err != nil {
		t.Fatalf("write session source: %v", err)
	}
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "log", "--reverse", "--format=%H%x00%P%x00%aI%x00%an%x00%ae%x00%B%x1e")] = fakeCommandResponse{stdout: strings.Join([]string{
		"precommit\x00\x002026-01-01T00:00:00Z\x00Alice\x00alice@example.com\x00Initial import",
		"coveredcommit\x00precommit\x002026-01-02T01:00:00Z\x00Alice\x00alice@example.com\x00Covered work\n\nEntire-Checkpoint: aaa111aaa111",
		"unexportedcommit\x00coveredcommit\x002026-01-02T02:00:00Z\x00Bob\x00bob@example.com\x00Intermediate checkpoint\n\nEntire-Checkpoint: bbb222bbb222",
		"missingcommit\x00unexportedcommit\x002026-01-02T03:00:00Z\x00Bob\x00bob@example.com\x00Manual follow-up",
		"mergecommit\x00missingcommit otherparent\x002026-01-02T04:00:00Z\x00Bob\x00bob@example.com\x00Merge branch feature",
	}, gitLogRecordSeparator) + gitLogRecordSeparator}

	coverage := buildSeedHistoryCoverage(context.Background(), runner, repoDir, outputDir)
	if coverage.TotalCommits != 5 {
		t.Fatalf("total commits = %d, want 5: %+v", coverage.TotalCommits, coverage)
	}
	if coverage.PreSessionCommits != 1 || coverage.CoveredCommits != 1 || coverage.CheckpointedUnexportedCommits != 1 || coverage.MissingSessionCommits != 2 {
		t.Fatalf("unexpected coverage counts: %+v", coverage)
	}
	if coverage.MergeCommits != 1 {
		t.Fatalf("merge commits = %d, want 1", coverage.MergeCommits)
	}
	if len(coverage.UncoveredCommits) != 3 {
		t.Fatalf("uncovered len = %d, want 3: %+v", len(coverage.UncoveredCommits), coverage.UncoveredCommits)
	}
	if coverage.UncoveredCommits[0].Coverage != "checkpointed_unexported" || coverage.UncoveredCommits[1].Coverage != "missing_session" || coverage.UncoveredCommits[2].Coverage != "missing_session" {
		t.Fatalf("unexpected uncovered classes: %+v", coverage.UncoveredCommits)
	}
}

func TestCombinedReadmeDeduplicatesWarnings(t *testing.T) {
	warning := "same warning"
	readme := renderCombinedBrainReadme(exportManifest{
		Warnings: []string{warning},
		Sources: &brainSources{
			Seed: &seedSourceManifest{GeneratedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SummaryPath: "seed/repo-overview.md"},
			Sessions: &sessionSourceManifest{
				Warnings: []string{warning},
			},
		},
	})
	if strings.Count(readme, warning) != 1 {
		t.Fatalf("warning was not deduplicated:\n%s", readme)
	}
}

func seedFixtureRepo(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	files := map[string]string{
		"README.md":                       "# Test Repo\n",
		"CLAUDE.md":                       "# Rules\n",
		".github/copilot-instructions.md": "# Copilot\n",
		"package.json":                    `{"scripts":{"dev":"vite","test":"vitest"}}`,
		"mise.toml":                       "[tasks.fmt]\nrun = \"gofmt -w .\"\n\n[tasks.\"test:ci\"]\nrun = '''\ngo test -race ./...\n'''\n",
		"src/main.jsx":                    "export function main() {}\n",
		"src/index.ts":                    "export {}\n",
		"src/__tests__/main.test.js":      "test('x', () => {})\n",
		".env.local":                      "TOKEN=secret\n",
		"dist/generated.js":               "generated\n",
		"package-lock.json":               "{}\n",
	}
	for path, content := range files {
		abs := filepath.Join(repoDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return repoDir
}

func seedFixtureRunner(repoDir string) *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {
			stdout: repoDir + "\n",
		},
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "https://github.com/example/repo.git\n",
		},
		fakeCommandKey("git", "rev-parse", "HEAD"): {
			stdout: "abc123\n",
		},
		fakeCommandKey("git", "log", "--reverse", "--format=%aI", "--max-count=1"): {
			stdout: "2025-01-01T00:00:00Z\n",
		},
		fakeCommandKey("git", "rev-list", "--max-parents=0", "HEAD"): {
			stdout: "root-one\n",
		},
		fakeCommandKey("git", "show", "-s", "--format=%aI", "root-one"): {
			stdout: "2025-01-01T00:00:00Z\n",
		},
		fakeCommandKey("git", "ls-files"): {
			stdout: strings.Join([]string{
				"README.md",
				"CLAUDE.md",
				".github/copilot-instructions.md",
				"package.json",
				"mise.toml",
				"src/main.jsx",
				"src/index.ts",
				"src/__tests__/main.test.js",
				".env.local",
				"dist/generated.js",
				"package-lock.json",
			}, "\n") + "\n",
		},
	}}
}

func hasSeedDocument(docs []seedDocument, path string) bool {
	for _, doc := range docs {
		if doc.Path == path {
			return true
		}
	}
	return false
}

func hasSeedCommand(commands []seedCommand, name string) bool {
	for _, command := range commands {
		if command.Name == name {
			return true
		}
	}
	return false
}
