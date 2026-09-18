package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// commandRegressionFixture puts session-backed brain state at the same storage
// path production derives from a local repository. It keeps command routing,
// storage resolution, and disk writes in the test rather than calling command
// handlers directly.
func commandRegressionFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:acme/command-regression.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env:     EntireEnv{RepoRoot: repoDir, PluginDataDir: t.TempDir()},
		Runner:  runner,
		Now:     func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := make([]sessionFixture, 0, 3)
	for i := 1; i <= 3; i++ {
		fixtures = append(fixtures, sessionFixture{
			id: "session-" + string(rune('0'+i)), branch: "main", agent: "Codex", checkpoint: "checkpoint-" + string(rune('0'+i)),
			relPath: "sessions/main/session-" + string(rune('0'+i)) + ".jsonl", author: "Ada", transcript: commitFixtureTranscript,
		})
	}
	fixture := writeEpisodeFixture(t, now, "gh/acme/command-regression", fixtures)
	if err := os.MkdirAll(filepath.Dir(storage.BrainDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture, storage.BrainDir); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage.BrainDir
}

// installPromotableCorpus supplies the stable corpus fixture used by the
// routing assertions below. Refresh itself is still exercised first; this
// separates its source-episode contract from the corpus's higher evidence
// threshold for a formable task candidate.
func installPromotableCorpus(t *testing.T, brainDir string) {
	t.Helper()
	donor := promotableCorpusDir(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	data, err := os.ReadFile(filepath.Join(donor, filepath.FromSlash(patternCorpusPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedRegressionDeepDossier(t *testing.T, brainDir, taskID string) {
	t.Helper()
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	seedAcceptedDeepDossier(t, db, taskID, deployDeepDossier(), nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func fakeCodexProvider(t *testing.T, response string) {
	t.Helper()
	binDir := t.TempDir()
	event, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": response}})
	if err != nil {
		t.Fatal(err)
	}
	// Compile a native responder instead of scripting one: Windows does not
	// execute shebang files, and a Go string literal keeps every JSON byte safe
	// if future provider output contains shell-significant characters.
	source := "package main\nimport (\"fmt\"; \"io\"; \"os\")\nfunc main() { _, _ = io.Copy(io.Discard, os.Stdin); fmt.Print(" + strconv.Quote(string(event)+"\n") + ") }\n"
	sourcePath := filepath.Join(binDir, "main.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "codex")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, sourcePath)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build portable fake codex: %v\n%s", err, output)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved, err := exec.LookPath("codex")
	if err != nil || filepath.Clean(resolved) != filepath.Clean(binary) {
		t.Fatalf("fake codex was not selected: got %q err=%v want %q", resolved, err, binary)
	}
}

const regressionSkill = "---\nname: deploy-release\ndescription: Deploy a release. Use when shipping a tagged build. Do NOT use when changing unrelated configuration.\n---\n# Deploy release\nRun mise build, then mise deploy.\n## Verification\ngo test ./...\n"

func TestRootCommandPatternsLifecycleAndPrivacyFailures(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)

	// The read-only default and status route both report a missing derived layer
	// before refresh, and do not manufacture a corpus as a side effect.
	for _, args := range [][]string{{"patterns", "--json"}, {"patterns", "status", "--json"}} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
		if !strings.Contains(out, `"present": false`) && out != "[]\n" {
			t.Fatalf("%s before refresh = %s", strings.Join(args, " "), out)
		}
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); !os.IsNotExist(err) {
		t.Fatalf("read-only routes created corpus: %v", err)
	}

	out, err := execute(t, NewRootCommand(opts), "patterns", "refresh", repoDir, "--json")
	if err != nil {
		t.Fatalf("patterns refresh: %v\n%s", err, out)
	}
	var source patternSourceManifest
	if err := json.Unmarshal([]byte(out), &source); err != nil {
		t.Fatalf("decode refresh JSON: %v\n%s", err, out)
	}
	if source.Episodes == 0 {
		t.Fatalf("refresh did not derive episodes: %+v", source)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); err != nil {
		t.Fatalf("refresh did not publish corpus: %v", err)
	}

	out, err = execute(t, NewRootCommand(opts), "patterns", repoDir, "--json")
	if err != nil {
		t.Fatalf("patterns list: %v\n%s", err, out)
	}
	var refreshedPatterns []patternView
	if err := json.Unmarshal([]byte(out), &refreshedPatterns); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, out)
	}

	out, err = execute(t, NewRootCommand(opts), "patterns", "status", repoDir, "--json")
	if err != nil {
		t.Fatalf("patterns status: %v\n%s", err, out)
	}
	var status patternsStatusReport
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, out)
	}
	if !status.Present || status.Episodes != source.Episodes {
		t.Fatalf("status does not describe refreshed state: %+v source=%+v", status, source)
	}

	installPromotableCorpus(t, brainDir)
	out, err = execute(t, NewRootCommand(opts), "patterns", repoDir, "--json")
	if err != nil {
		t.Fatalf("patterns list from corpus: %v\n%s", err, out)
	}
	var patterns []patternView
	if err := json.Unmarshal([]byte(out), &patterns); err != nil {
		t.Fatalf("decode corpus list JSON: %v\n%s", err, out)
	}
	if len(patterns) == 0 {
		t.Fatalf("promotable corpus produced no listable patterns: %s", out)
	}
	out, err = execute(t, NewRootCommand(opts), "patterns", repoDir, "--type", "task", "--limit", "1")
	if err != nil || !strings.Contains(out, "] deploy:release") || !strings.Contains(out, "id pattern:") {
		t.Fatalf("text task list = %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "patterns", repoDir, "--scope", "workspace")
	if err != nil || !strings.Contains(out, "patterns: none detected yet") {
		t.Fatalf("empty filtered list = %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "patterns", "status", repoDir)
	if err != nil || !strings.Contains(out, "patterns: current") || !strings.Contains(out, "episodes:") {
		t.Fatalf("text status = %v\n%s", err, out)
	}

	// The skills list is a real routed read; shallow corpus candidates are not
	// formable until deep verification. Even --yes must leave no skill state.
	out, err = execute(t, NewRootCommand(opts), "patterns", "skills", repoDir, "--json")
	if err != nil {
		t.Fatalf("patterns skills: %v\n%s", err, out)
	}
	var proposals []taskCandidate
	if err := json.Unmarshal([]byte(out), &proposals); err != nil {
		t.Fatalf("decode initial skills list JSON: %v\n%s", err, out)
	}
	if len(proposals) != 0 {
		t.Fatalf("unverified corpus exposed formable skills: %+v", proposals)
	}
	var taskID string
	for _, p := range patterns {
		if p.Type == "task" {
			taskID = p.ID
			break
		}
	}
	if taskID == "" {
		t.Fatalf("fixture lacks task pattern: %+v", patterns)
	}
	if out, err = execute(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--yes"); err == nil || !strings.Contains(err.Error(), "needs deep verification first") {
		t.Fatalf("form shallow task error = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternsSkillMemoryPath))); !os.IsNotExist(err) {
		t.Fatalf("failed form persisted skill memory: %v", err)
	}

	seedRegressionDeepDossier(t, brainDir, taskID)
	out, err = execute(t, NewRootCommand(opts), "patterns", "skills", repoDir, "--json")
	if err != nil {
		t.Fatalf("accepted skills list: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &proposals); err != nil {
		t.Fatalf("decode accepted skills list JSON: %v\n%s", err, out)
	}
	if len(proposals) != 1 || proposals[0].ID != taskID {
		t.Fatalf("accepted skills list = %+v, want %q", proposals, taskID)
	}
	out, err = execute(t, NewRootCommand(opts), "patterns", "skills", repoDir)
	if err != nil || !strings.Contains(out, "id "+taskID) || !strings.Contains(out, "session(s)") {
		t.Fatalf("text skills list = %v\n%s", err, out)
	}
	fakeCodexProvider(t, regressionSkill)
	var stderr string
	out, stderr, err = executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex", "--draft-only", "--json")
	if err != nil {
		t.Fatalf("draft skill form: %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	if !strings.Contains(stderr, "synthesizing skill from the verified deep dossier") {
		t.Fatalf("draft skill form omitted synthesis diagnostic: %s", stderr)
	}
	var preview struct {
		TaskID  string `json:"task_id"`
		IsSkill bool   `json:"is_skill"`
		Name    string `json:"name"`
		Skill   string `json:"skill"`
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("decode skill preview JSON: %v\n%s", err, out)
	}
	if preview.TaskID != taskID || !preview.IsSkill || preview.Name != "deploy-release" || !strings.Contains(preview.Skill, "## Verification") {
		t.Fatalf("skill preview lost synthesis contract: %+v", preview)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternsSkillMemoryPath))); !os.IsNotExist(err) {
		t.Fatalf("draft-only form persisted skill memory: %v", err)
	}
	out, stderr, err = executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex")
	if err != nil || !strings.Contains(out, "--- proposed SKILL.md ---") || !strings.Contains(out, "would write to:") || !strings.Contains(stderr, "synthesizing") {
		t.Fatalf("text skill preview = %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	installedPath := filepath.Join(repoDir, ".agents", "skills", "deploy-release", "SKILL.md")
	if _, err := os.Stat(installedPath); !os.IsNotExist(err) {
		t.Fatalf("preview wrote skill file: %v", err)
	}
	out, stderr, err = executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex", "--yes", "--json")
	if err != nil {
		t.Fatalf("skill install: %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	var installed struct {
		TaskID   string         `json:"task_id"`
		Name     string         `json:"name"`
		Status   string         `json:"status"`
		Installs []skillInstall `json:"installs"`
	}
	if err := json.Unmarshal([]byte(out), &installed); err != nil || installed.TaskID != taskID || installed.Name != "deploy-release" || installed.Status != "active" || len(installed.Installs) != 1 {
		t.Fatalf("installed skill JSON = %+v err=%v output=%s", installed, err, out)
	}
	data, readErr := os.ReadFile(installedPath)
	if readErr != nil || strings.TrimSpace(string(data)) != strings.TrimSpace(regressionSkill) {
		t.Fatalf("installed skill = %q err=%v", data, readErr)
	}
	if out, _, err = executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex", "--yes"); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("duplicate install error = %v\n%s", err, out)
	}
	if _, _, err = executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex", "--yes", "--force"); err != nil {
		t.Fatalf("forced reinstall: %v", err)
	}

	// Verification is the sole potentially egressing route. Its no-egress
	// rejection happens before corpus mutation or a provider invocation.
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	before := treeContentHash(t, brainDir)
	out, err = execute(t, NewRootCommand(opts), "patterns", "verify", repoDir, "--agent", "codex")
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("verify no-egress error = %v\n%s", err, out)
	}
	if after := treeContentHash(t, brainDir); after != before {
		t.Fatal("no-egress verification changed derived brain state")
	}
}

func TestRootCommandPatternsVerifyAcceptedAndCached(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	installPromotableCorpus(t, brainDir)
	fakeCodexProvider(t, `{"verdict":"accepted","reason":"corroborated"}`)

	out, stderr, err := executeSplit(t, NewRootCommand(opts), "patterns", "verify", repoDir, "--agent", "codex", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("verify JSON = %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
	var stats dossierVerifyStats
	if err := json.Unmarshal([]byte(out), &stats); err != nil || stats.Considered == 0 || stats.Verified != stats.Considered || stats.Failed != 0 {
		t.Fatalf("verify stats = %+v err=%v output=%s", stats, err, out)
	}
	out, stderr, err = executeSplit(t, NewRootCommand(opts), "patterns", "verify", repoDir, "--agent", "codex")
	if err != nil || !strings.Contains(out, "0 verified") || !strings.Contains(out, "cached") || stderr != "" {
		t.Fatalf("cached verify = %v\nstdout=%s\nstderr=%s", err, out, stderr)
	}
}

type cancelOnVizURLWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	cancel    context.CancelFunc
	cancelled bool
}

func (w *cancelOnVizURLWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if !w.cancelled && strings.Contains(w.buf.String(), "http://127.0.0.1:") {
		w.cancelled = true
		w.cancel()
	}
	return n, err
}

func (w *cancelOnVizURLWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestRootCommandVizReportsLoopbackReadOnlyServer(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := NewRootCommand(opts)
	cmd.SetContext(ctx)
	var out cancelOnVizURLWriter
	out.cancel = cancel
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"viz", repoDir, "--no-open", "--port", "0"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("viz: %v\n%s", err, out.String())
	}
	if !out.cancelled || !strings.Contains(out.String(), "http://127.0.0.1:") || !strings.Contains(out.String(), "Read-only, loopback-only, no network") {
		t.Fatalf("viz output lost its local-only contract:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); !os.IsNotExist(err) {
		t.Fatalf("viz wrote a pattern corpus: %v", err)
	}
}
