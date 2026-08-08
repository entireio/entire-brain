package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// live_e2e_test.go is the R0.4 live capture-loop journey: a REAL harness
// session captured by the REAL Entire CLI in an isolated scratch repository,
// then recalled through the brain pipeline: capture -> checkpoint fold ->
// export -> delta -> query -> get -> consolidation -> receipts.
//
// The journeys are opt-in (ENTIRE_BRAIN_LIVE_E2E=1): they spend real agent
// tokens and need `entire` plus the harness on PATH and its credentials in
// the environment. Everything lands in t.TempDir (auto-removed); test output
// carries counts and ids, never transcript bodies, so no retained artifact
// holds session content or credentials.

type liveE2EHarness struct {
	repoDir  string
	brainBin string
	env      []string
	t        *testing.T
}

func newLiveE2EHarness(t *testing.T) *liveE2EHarness {
	t.Helper()
	if os.Getenv("ENTIRE_BRAIN_LIVE_E2E") != "1" {
		t.Skip("live capture-loop journey is opt-in: set ENTIRE_BRAIN_LIVE_E2E=1")
	}
	for _, bin := range []string{"entire", "git", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	root := t.TempDir()
	// macOS TempDir is behind a /var -> /private/var symlink; canonicalize so
	// every pipeline stage resolves ONE repository identity.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &liveE2EHarness{repoDir: repoDir, brainBin: filepath.Join(root, "entire-brain"), t: t}
	h.env = append(os.Environ(),
		"ENTIRE_REPO_ROOT="+repoDir,
		"ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"),
		"ENTIRE_PLUGIN_DATA_DIR="+filepath.Join(root, "data"),
		"ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"),
		"ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"),
		"ACCESSIBLE=1",
	)
	// The brain binary under test is THIS tree's binary.
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", h.brainBin, "./cmd/entire-brain")
	build.Dir = filepath.Join(repoRoot, "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build brain binary: %v: %s", err, out)
	}
	h.run(2*time.Minute, "git", "init", "-q", "-b", "main")
	h.run(time.Minute, "git", "config", "user.email", "live-e2e@example.invalid")
	h.run(time.Minute, "git", "config", "user.name", "Live E2E")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# live e2e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(time.Minute, "git", "add", "-A")
	h.run(time.Minute, "git", "commit", "-qm", "seed")
	h.run(2*time.Minute, "entire", "enable")
	return h
}

func (h *liveE2EHarness) run(timeout time.Duration, name string, args ...string) string {
	h.t.Helper()
	command := exec.Command(name, args...)
	command.Dir = h.repoDir
	command.Env = h.env
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = command.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = command.Process.Kill()
		h.t.Fatalf("%s %s timed out after %s", name, strings.Join(args, " "), timeout)
	}
	if err != nil {
		// Bound the failure output; never dump transcript bodies wholesale.
		snippet := string(out)
		if len(snippet) > 2000 {
			snippet = snippet[:2000]
		}
		h.t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, snippet)
	}
	return string(out)
}

func (h *liveE2EHarness) brain(args ...string) string {
	return h.run(5*time.Minute, h.brainBin, args...)
}

func (h *liveE2EHarness) commitAll(message string) {
	h.run(time.Minute, "git", "add", "-A")
	h.run(time.Minute, "git", "commit", "-qm", message)
}

func (h *liveE2EHarness) searchConversation(query string) []map[string]any {
	raw := h.brain("search", query, "--source", "conversation", "--json")
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		h.t.Fatalf("search payload: %v", err)
	}
	return payload.Results
}

func (h *liveE2EHarness) hookLogContains(needle string) bool {
	data, err := os.ReadFile(filepath.Join(h.repoDir, ".entire", "logs", "entire.log"))
	return err == nil && strings.Contains(strings.ToLower(string(data)), needle)
}

// assertRecallJourney drives export -> query -> get -> outline -> receipts
// for one captured token and reports the conversation hit.
func (h *liveE2EHarness) assertRecallJourney(token string) map[string]any {
	h.t.Helper()
	results := h.searchConversation(token)
	var hit map[string]any
	for _, result := range results {
		if text, _ := result["text"].(string); strings.Contains(text, token) {
			hit = result
		}
	}
	if hit == nil {
		h.diagnose()
		h.t.Fatalf("token not recalled through conversation search (%d results)", len(results))
	}
	id, _ := hit["id"].(string)
	sessionRef, _ := hit["session_ref"].(string)
	if !strings.HasPrefix(id, "conversation:") || !strings.HasPrefix(sessionRef, "conversation-session:") {
		h.t.Fatalf("hit identity malformed: id=%q ref=%q", id, sessionRef)
	}
	caveats, _ := json.Marshal(hit["caveats"])
	if !strings.Contains(string(caveats), "historical_conversation") {
		h.t.Fatal("hit missing the historical-evidence caveat")
	}
	// Bounded expansion carries the token and the caveat.
	getRaw := h.brain("get", id, "--json")
	if !strings.Contains(getRaw, token) || !strings.Contains(getRaw, "historical_conversation") {
		h.t.Fatal("bounded expansion missing token or caveat")
	}
	// The session outline resolves through the virtual identity.
	outlineRaw := h.brain("get", sessionRef, "--json")
	if !strings.Contains(outlineRaw, "session_outline") {
		h.t.Fatal("session outline not resolvable")
	}
	return hit
}

// diagnose logs bounded, content-free pipeline state when recall fails:
// checkpoint fold commits, exported session count, and stats. Never
// transcript bodies.
func (h *liveE2EHarness) diagnose() {
	fold := exec.Command("git", "log", "--oneline", "refs/heads/entire/checkpoints/v1")
	fold.Dir = h.repoDir
	if out, err := fold.CombinedOutput(); err == nil {
		h.t.Logf("checkpoint fold commits:\n%s", string(out))
	}
	trailer := exec.Command("git", "log", "-3", "--format=%s|%(trailers:key=Entire-Checkpoint,valueonly)")
	trailer.Dir = h.repoDir
	if out, err := trailer.CombinedOutput(); err == nil {
		h.t.Logf("recent commits and trailers:\n%s", string(out))
	}
	if log, err := os.ReadFile(filepath.Join(h.repoDir, ".entire", "logs", "entire.log")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(log)), "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		h.t.Logf("entire.log tail:\n%s", strings.Join(lines, "\n"))
	}
	var entireFiles []string
	_ = filepath.Walk(filepath.Join(h.repoDir, ".entire"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			rel, _ := filepath.Rel(h.repoDir, path)
			entireFiles = append(entireFiles, rel)
		}
		return nil
	})
	if len(entireFiles) > 25 {
		entireFiles = entireFiles[:25]
	}
	h.t.Logf(".entire files (names only): %v", entireFiles)
	stats := exec.Command(h.brainBin, "stats", "--json")
	stats.Dir = h.repoDir
	stats.Env = h.env
	if out, err := stats.CombinedOutput(); err == nil {
		snippet := string(out)
		if len(snippet) > 1500 {
			snippet = snippet[:1500]
		}
		h.t.Logf("brain stats: %s", snippet)
	}
}

// TestLiveCaptureLoopClaudeCode is the sanctioned live Claude Code journey.
func TestLiveCaptureLoopClaudeCode(t *testing.T) {
	h := newLiveE2EHarness(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}
	h.run(2*time.Minute, "entire", "agent", "add", "claude-code")
	h.commitAll("entire setup")
	// Baseline the incremental export cursor before any session, exactly as
	// a continuously running watch would.
	h.brain("refresh", "delta")

	// Turn 1: a real agent edit; the user commit folds the checkpoint.
	h.run(10*time.Minute, "claude", "-p", "--model", "haiku", "--permission-mode", "acceptEdits",
		"Create a file named NOTES.md whose entire content is exactly this one line: PINEAPPLE_E2E_7 is the probe token.")
	h.commitAll("first probe turn")

	// Export runs through the delta step (incremental checkpoint export),
	// then the first consolidation publishes the receipts; recall proves the
	// whole chain.
	deltaOut := h.brain("refresh", "delta")
	h.t.Logf("post-turn delta output:\n%s", strings.TrimSpace(deltaOut))
	h.brain("refresh", "--semantic=false")
	h.assertRecallJourney("PINEAPPLE_E2E_7")
	statusRaw := h.brain("memory", "status", "--json")
	if !strings.Contains(statusRaw, `"receipts_current": true`) {
		t.Fatalf("projection receipts not current after consolidation")
	}

	// Turn 2: recalled through the short-term delta BEFORE any further
	// consolidation (near-real-time recall).
	h.run(10*time.Minute, "claude", "-p", "--model", "haiku", "--permission-mode", "acceptEdits",
		"Append one line to NOTES.md that says exactly: MANGO_E2E_9 arrived in the second turn.")
	h.commitAll("second probe turn")
	h.brain("refresh", "delta")
	found := false
	for _, result := range h.searchConversation("MANGO_E2E_9 second turn") {
		if text, _ := result["text"].(string); strings.Contains(text, "MANGO_E2E_9") {
			found = true
		}
	}
	if !found {
		t.Fatal("second turn not recallable through the delta overlay before consolidation")
	}

	// Final consolidation absorbs the overlay and recall still holds.
	h.brain("refresh", "--semantic=false")
	h.assertRecallJourney("MANGO_E2E_9")
}

// TestLiveCaptureLoopCodex is the Codex journey. As of entire
// 0.6.3-nightly's codex integration the hooks it installs are not loaded by
// codex 0.139 (no codex lifecycle events reach the entire log), so the test
// skips with the exact host-integration gap named; it activates automatically
// once the host pairing captures codex sessions.
func TestLiveCaptureLoopCodex(t *testing.T) {
	h := newLiveE2EHarness(t)
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}
	h.run(2*time.Minute, "entire", "agent", "add", "codex")
	h.commitAll("entire setup")

	h.run(10*time.Minute, "codex", "exec", "--sandbox", "workspace-write",
		"Create a file named CODEX_NOTES.md whose entire content is exactly this one line: KIWI_E2E_5 came from the codex probe.")
	if !h.hookLogContains("codex") {
		t.Skip("host integration gap: entire's codex hooks are not loaded by this codex CLI (no codex lifecycle events); journey activates when the host pairing captures codex sessions")
	}
	h.commitAll("codex probe turn")
	h.brain("refresh", "--semantic=false")
	h.assertRecallJourney("KIWI_E2E_5")
}
