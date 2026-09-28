package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// live_e2e_test.go is the live capture-loop journey: a REAL harness
// session captured by the REAL Entire CLI in an isolated scratch repository,
// then recalled through the brain pipeline: capture -> checkpoint fold ->
// export -> delta -> query -> get -> consolidation -> receipts.
//
// The journeys are opt-in (ENTIRE_BRAIN_LIVE_E2E=1): they spend real agent
// tokens and need `entire` plus the harness on PATH and its credentials in
// the environment. Everything lands in t.TempDir (auto-removed); test output
// normally carries counts and ids. Failure diagnostics may include raw agent
// output and log tails; do not treat them as redacted artifacts.

type liveE2EHarness struct {
	repoDir  string
	brainBin string
	env      []string
	t        *testing.T
}

type liveE2EEnvValue struct {
	key   string
	value string
}

func liveE2EEnvironment(parent []string, replacements ...liveE2EEnvValue) []string {
	out := make([]string, 0, len(parent)+len(replacements))
	for _, entry := range parent {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		replace := false
		for _, candidate := range replacements {
			if key == candidate.key || (runtime.GOOS == "windows" && strings.EqualFold(key, candidate.key)) {
				replace = true
				break
			}
		}
		if !replace {
			out = append(out, entry)
		}
	}
	for _, replacement := range replacements {
		out = append(out, replacement.key+"="+replacement.value)
	}
	return out
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
	pluginRoot := filepath.Join(root, "plugins")
	pluginBinDir := filepath.Join(pluginRoot, "bin")
	if err := os.MkdirAll(pluginBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brainName := "entire-brain"
	if runtime.GOOS == "windows" {
		brainName += ".exe"
	}
	h := &liveE2EHarness{repoDir: repoDir, brainBin: filepath.Join(pluginBinDir, brainName), t: t}
	h.env = liveE2EEnvironment(os.Environ(),
		liveE2EEnvValue{key: "ENTIRE_REPO_ROOT", value: repoDir},
		liveE2EEnvValue{key: "ENTIRE_PLUGIN_DIR", value: pluginRoot},
		liveE2EEnvValue{key: "ENTIRE_PLUGIN_CONFIG_DIR", value: filepath.Join(root, "config")},
		// Match Entire's managed per-plugin data directory so direct harness
		// calls and lifecycle-adapter `entire brain ...` calls share one Brain.
		liveE2EEnvValue{key: "ENTIRE_PLUGIN_DATA_DIR", value: filepath.Join(pluginRoot, "data", "brain")},
		liveE2EEnvValue{key: "ENTIRE_PLUGIN_STATE_DIR", value: filepath.Join(root, "state")},
		liveE2EEnvValue{key: "ENTIRE_PLUGIN_CACHE_DIR", value: filepath.Join(root, "cache")},
		liveE2EEnvValue{key: "ACCESSIBLE", value: "1"},
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

func (h *liveE2EHarness) headHasCheckpointTrailer() bool {
	h.t.Helper()
	command := exec.Command("git", "log", "-1", "--format=%B")
	command.Dir = h.repoDir
	command.Env = h.env
	out, err := command.Output()
	return err == nil && strings.Contains(string(out), "Entire-Checkpoint:")
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
	refs := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/entire/checkpoints/")
	refs.Dir = h.repoDir
	if out, err := refs.CombinedOutput(); err == nil {
		h.t.Logf("checkpoint refs (names only):\n%s", string(out))
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

// TestLiveCaptureLoopCodex is the sanctioned Codex journey. Missing binaries
// still mean the harness is unavailable; once explicitly enabled with both
// binaries present, missing lifecycle capture is a product failure rather than
// a skip that could make the acceptance gate appear green.
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
		t.Fatal("Codex completed but no Codex lifecycle event reached Entire")
	}
	h.commitAll("codex probe turn")
	if !h.headHasCheckpointTrailer() {
		t.Fatal("Codex work committed without an Entire-Checkpoint trailer")
	}
	h.brain("refresh", "--semantic=false")
	h.assertRecallJourney("KIWI_E2E_5")
}
