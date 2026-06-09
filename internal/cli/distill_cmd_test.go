package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeDistillFixture lays down a brain dir with a session manifest and two
// transcript files, returning the brain dir.
func writeDistillFixture(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := t.TempDir()
	sessions := []exportSession{
		{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
		{SessionID: "s2", Branch: "feature", LatestCheckpoint: "cp2", TranscriptPath: "sessions/branches/feature/s2.jsonl", CreatedAt: now.Add(-1 * time.Hour)},
	}
	for _, s := range sessions {
		path := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("turn one\nturn two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func TestRunDistillForBrainWritesFactsAndManifest(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var calls int
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		// The agent must receive a line-numbered transcript chunk on stdin.
		if !strings.Contains(string(input), "1\tturn one") {
			t.Errorf("chunk not line-numbered: %q", input)
		}
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one agent call per session (2), got %d", calls)
	}
	if source.Facts != 2 || source.Distilled != 2 {
		t.Fatalf("expected 2 distilled facts, got %+v", source)
	}
	if len(source.Branches) != 2 {
		t.Fatalf("expected facts on 2 branches, got %v", source.Branches)
	}

	mainFacts, err := loadFacts(brainDir, "main")
	if err != nil || len(mainFacts) != 1 {
		t.Fatalf("expected 1 fact on main, got %d (%v)", len(mainFacts), err)
	}
	fact := mainFacts[0]
	if fact.Origin != factOriginDistilled || fact.Status != factStatusActive {
		t.Errorf("wrong origin/status: %+v", fact)
	}
	if len(fact.Provenance) != 1 || fact.Provenance[0].SessionID != "s1" || fact.Provenance[0].CheckpointID != "cp1" {
		t.Errorf("provenance not anchored to session: %+v", fact.Provenance)
	}
	if fact.Provenance[0].Transcript != "sessions/main/s1.jsonl" {
		t.Errorf("provenance transcript wrong: %q", fact.Provenance[0].Transcript)
	}

	// Manifest source and taxonomy snapshot must be written.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Facts == nil {
		t.Fatalf("manifest facts source not written: %v", err)
	}
	if manifest.Sources.Facts.Facts != 2 {
		t.Errorf("manifest facts count = %d, want 2", manifest.Sources.Facts.Facts)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(factsTaxonomyPath))); err != nil {
		t.Errorf("taxonomy snapshot not written: %v", err)
	}
}

func TestRunDistillForBrainDoesNotHoldWriteLockDuringExtraction(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var checked atomic.Bool
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		unlock, err := acquireBrainWriteLockTimeout(brainDir, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("distill extraction held brain write lock: %v", err)
		}
		unlock()
		checked.Store(true)
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if !checked.Load() {
		t.Fatalf("fake extraction agent was not called")
	}
}

func TestRunDistillForBrainAbortsWhenTaxonomyChangesDuringExtraction(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var changed atomic.Bool
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if changed.CompareAndSwap(false, true) {
			if err := withBrainWriteLock(brainDir, func() error {
				taxonomy, err := loadFactTaxonomy(brainDir, now)
				if err != nil {
					return err
				}
				taxonomy.Categories["release"] = "Release readiness facts."
				return writeFactTaxonomy(brainDir, taxonomy)
			}); err != nil {
				t.Fatalf("change taxonomy: %v", err)
			}
		}
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err == nil || !strings.Contains(err.Error(), "taxonomy changed") {
		t.Fatalf("expected taxonomy changed error, got %v", err)
	}
}

func TestPreprocessTranscriptForDistillStripsTools(t *testing.T) {
	// One record per line, mixing both transcript dialects. Tool calls, tool
	// outputs, and meta must be dropped; human + assistant text must survive.
	lines := []string{
		`{"type":"session_meta","payload":{"big":"x"}}`,                                                                                           // meta -> drop
		`{"type":"event_msg","payload":{"type":"user_message","message":"always use tabs"}}`,                                                      // human -> keep
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`,                                             // tool call -> drop
		`{"type":"response_item","payload":{"type":"function_call_output","output":"huge command output here"}}`,                                  // tool output -> drop
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"decided to keep it"}]}}`,        // assistant -> keep
		`{"type":"assistant","message":{"content":[{"type":"text","text":"mixed turn"},{"type":"tool_use","name":"bash","input":{"cmd":"ls"}}]}}`, // keep text, drop tool_use
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"result blob"}]}}`,                                                  // tool output -> drop
		`not json at all`, // non-JSON -> passthrough
	}
	out := preprocessTranscriptForDistill(strings.Join(lines, "\n"))
	got := strings.Split(out, "\n")

	if len(got) != len(lines) {
		t.Fatalf("line count changed: got %d, want %d (provenance would break)", len(got), len(lines))
	}
	want := []string{
		"",                   // session_meta dropped
		"always use tabs",    // user_message kept
		"",                   // function_call dropped
		"",                   // function_call_output dropped
		"decided to keep it", // assistant message kept
		"mixed turn",         // assistant text kept, tool_use stripped
		"",                   // tool_result dropped
		"not json at all",    // passthrough
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i+1, got[i], want[i])
		}
	}

	// No tool noise should remain anywhere in the output.
	for _, needle := range []string{"function_call", "tool_use", "tool_result", "session_meta", "command output", "result blob", "ls"} {
		if strings.Contains(out, needle) {
			t.Errorf("preprocessed output still contains tool/meta token %q", needle)
		}
	}
}

func TestRunDistillForBrainReportsProgress(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	var updates []distillProgress
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: fakeRun,
		maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		progress: func(p distillProgress) { updates = append(updates, p) },
	}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}

	// The fixture has two sessions; progress must fire once per session with a
	// stable total and a monotonically increasing done count that reaches it.
	if len(updates) != 2 {
		t.Fatalf("expected 2 progress updates, got %d (%+v)", len(updates), updates)
	}
	for i, u := range updates {
		if u.SessionsTotal != 2 {
			t.Errorf("update %d: SessionsTotal = %d, want 2", i, u.SessionsTotal)
		}
		if u.SessionsDone != i+1 {
			t.Errorf("update %d: SessionsDone = %d, want %d", i, u.SessionsDone, i+1)
		}
		// Emitted after each session, so the cumulative fact count must include the
		// session just processed (each fixture session yields one fact).
		if u.Facts != i+1 {
			t.Errorf("update %d: Facts = %d, want %d (count must include the session just processed)", i, u.Facts, i+1)
		}
		if u.Branch == "" {
			t.Errorf("update %d: empty branch", i)
		}
	}

	// The label is human-readable and reflects the counts.
	if got := distillProgressLabel(updates[1]); !strings.Contains(got, "2/2 done") {
		t.Errorf("label = %q, want it to contain %q", got, "2/2 done")
	}
}

// The distill label must not match progressCountPattern, or the non-TTY throttle
// would treat same-fact-count sessions as the same status and suppress the
// documented per-session progress line.
func TestDistillProgressLabelNotThrottled(t *testing.T) {
	label := distillProgressLabel(distillProgress{SessionsDone: 12, SessionsTotal: 55, Branch: "main", Facts: 0})
	if progressCountPattern.MatchString(label) {
		t.Errorf("label %q matches progressCountPattern; per-session updates would be throttled", label)
	}
}

// On non-TTY output, two cached/zero-fact sessions in quick succession must each
// emit a progress line rather than collapsing under the same-status throttle.
func TestDistillProgressEmitsPerSessionNonTTY(t *testing.T) {
	var buf bytes.Buffer // not an *os.File -> non-TTY path (no spinner)
	task := newProgress(&buf, "distill").Begin("distill sessions")
	task.Update(distillProgressLabel(distillProgress{SessionsDone: 1, SessionsTotal: 3, Branch: "main", Facts: 0}))
	task.Update(distillProgressLabel(distillProgress{SessionsDone: 2, SessionsTotal: 3, Branch: "main", Facts: 0}))
	out := buf.String()
	if c := strings.Count(out, "1/3 done"); c != 1 {
		t.Errorf("expected exactly one line for session 1, got %d in %q", c, out)
	}
	if c := strings.Count(out, "2/3 done"); c != 1 {
		t.Errorf("expected exactly one line for session 2 despite unchanged fact count, got %d in %q", c, out)
	}
}

func TestRunDistillForBrainIncrementalSkipsUnchanged(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var calls int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.Contains(string(input), "turn one") {
			calls++
		}
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstCalls := calls

	// A second run with no transcript changes must skip every session.
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if calls != firstCalls {
		t.Fatalf("incremental run re-invoked the agent: %d extra calls", calls-firstCalls)
	}

	// --force must reprocess everything.
	forceOpts := opts
	forceOpts.force = true
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, forceOpts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}
	if calls <= firstCalls {
		t.Fatalf("--force did not reprocess sessions")
	}
}

func TestDistillDryRunCountsChunksWithoutAgent(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	report, err := buildDistillDryRunReport(brainDir, distillCommandOptions{maxChunkBytes: defaultDistillChunkSize}, now)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Sessions != 2 || report.SessionsToDistill != 2 || report.Chunks != 2 {
		t.Fatalf("unexpected dry-run counts: %+v", report)
	}
	if report.ExtractionAgentCalls != 2 || report.EstimatedAgentCallsUpperBound != 4 {
		t.Fatalf("unexpected agent-call estimate: %+v", report)
	}
	if report.PreprocessedBytes == 0 || len(report.Branches) != 2 || len(report.LargestSessions) != 2 {
		t.Fatalf("dry-run report missing cost drivers: %+v", report)
	}
}

func TestRunDistillForBrainParallelExtractionMatchesSerial(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	serialBrain := writeDistillFixture(t, now)
	parallelBrain := writeDistillFixture(t, now)

	runSerial := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	serialOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: runSerial, jobs: 1, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), serialBrain, serialOpts, now); err != nil {
		t.Fatalf("serial distill: %v", err)
	}

	var active, maxActive int32
	runParallel := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.Contains(string(input), "\tturn one") {
			current := atomic.AddInt32(&active, 1)
			for {
				seen := atomic.LoadInt32(&maxActive)
				if current <= seen || atomic.CompareAndSwapInt32(&maxActive, seen, current) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
			atomic.AddInt32(&active, -1)
		}
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	parallelOpts := serialOpts
	parallelOpts.run = runParallel
	parallelOpts.jobs = 4
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), parallelBrain, parallelOpts, now); err != nil {
		t.Fatalf("parallel distill: %v", err)
	}
	if atomic.LoadInt32(&maxActive) < 2 {
		t.Fatalf("--jobs did not run extraction concurrently; maxActive=%d", maxActive)
	}
	for _, branch := range []string{"main", "feature"} {
		serialFacts, err := loadFacts(serialBrain, branch)
		if err != nil {
			t.Fatal(err)
		}
		parallelFacts, err := loadFacts(parallelBrain, branch)
		if err != nil {
			t.Fatal(err)
		}
		if len(serialFacts) != len(parallelFacts) || serialFacts[0].ID != parallelFacts[0].ID {
			t.Fatalf("parallel output changed %s facts: serial=%+v parallel=%+v", branch, serialFacts, parallelFacts)
		}
	}
}

func TestRunDistillForBrainParallelDoesNotAbortAfterOrderedFailures(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	var sessions []exportSession
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("s%d", i)
		tp := fmt.Sprintf("sessions/main/%s.jsonl", id)
		path := filepath.Join(brainDir, filepath.FromSlash(tp))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		text := "success chunk"
		if i < distillAgentAbortThreshold {
			text = "fail chunk"
		}
		if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{SessionID: id, Branch: "main", LatestCheckpoint: "cp" + id, TranscriptPath: tp, CreatedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	var calls int32
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.Contains(string(input), "chunk") {
			atomic.AddInt32(&calls, 1)
		}
		if strings.Contains(string(input), "fail chunk") {
			return "", fmt.Errorf("ordered failure")
		}
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, jobs: 4, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("parallel distill should keep later successes instead of aborting: %v", err)
	}
	if calls != int32(len(sessions)) {
		t.Fatalf("expected every parallel chunk to complete, got %d calls", calls)
	}
	if source.FailedChunks != distillAgentAbortThreshold || source.Facts == 0 {
		t.Fatalf("unexpected mixed success summary: %+v", source)
	}
}

func TestRunDistillForBrainPreservesAuthoredOnForce(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	// Seed an authored fact on main that distillation must never clobber.
	paths := normalizeFactPaths([]string{"workflow.testing.rules"})
	authored := factRecord{
		ID:        factRecordID("Always run go test before pushing.", paths),
		Paths:     paths,
		Text:      "Always run go test before pushing.",
		Branch:    "main",
		Origin:    factOriginAuthored,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := writeFacts(brainDir, "main", []factRecord{authored}); err != nil {
		t.Fatal(err)
	}

	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var sawAuthored, sawDistilled bool
	for _, f := range facts {
		switch f.Origin {
		case factOriginAuthored:
			sawAuthored = true
		case factOriginDistilled:
			sawDistilled = true
		}
	}
	if !sawAuthored {
		t.Errorf("--force clobbered the authored fact")
	}
	if !sawDistilled {
		t.Errorf("--force did not produce distilled facts")
	}
}

func TestRunDistillForBrainRetriesFailedSessions(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	// First run: the agent fails on every chunk.
	failRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "", context.DeadlineExceeded
	}
	failOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: failRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, failOpts, now)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if source.Facts != 0 {
		t.Fatalf("expected 0 facts after total failure, got %d", source.Facts)
	}

	// Second run (no --force): failed sessions must be retried, not skipped as
	// cached. This time the agent succeeds.
	var calls int
	okRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "project.tooling.stack\tThe project uses TypeScript.\n", nil
	}
	okOpts := failOpts
	okOpts.run = okRun
	source, err = runDistillForBrain(context.Background(), t.TempDir(), brainDir, okOpts, now)
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if calls == 0 {
		t.Fatalf("failed sessions were cached and skipped instead of retried")
	}
	if source.Facts == 0 {
		t.Fatalf("retry produced no facts despite a working agent")
	}
}

// A session with an empty branch field is distilled under the manifest default
// branch, so a `--branch <default>` run must include it rather than filtering it
// out on the raw (empty) session.Branch before the default is resolved.
func TestRunDistillForBrainBranchFilterResolvesDefault(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	sessions := []exportSession{
		{SessionID: "s1", Branch: "", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
		{SessionID: "s2", Branch: "feature", LatestCheckpoint: "cp2", TranscriptPath: "sessions/branches/feature/s2.jsonl", CreatedAt: now.Add(-1 * time.Hour)},
	}
	for _, s := range sessions {
		path := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("turn one\nturn two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	var inputs []string
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		inputs = append(inputs, string(input))
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, branch: "main", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	// The empty-branch session (resolved to main) must have been distilled; the
	// feature session must not. distillCalls == 1.
	if len(inputs) != 1 {
		t.Fatalf("expected only the default-branch session distilled, got %d agent calls", len(inputs))
	}
	mainFacts, err := loadFacts(brainDir, "main")
	if err != nil || len(mainFacts) != 1 {
		t.Fatalf("empty-branch session not distilled under default branch: %d facts (%v)", len(mainFacts), err)
	}
}

func TestRunDistillForBrainBranchLimitedPreservesManifest(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	// Seed an authored fact on the feature branch. A `--branch main` distill must
	// not drop it (or the feature branch) from the manifest summary.
	paths := normalizeFactPaths([]string{"workflow.testing.rules"})
	authored := factRecord{
		ID:        factRecordID("Feature work needs integration tests.", paths),
		Paths:     paths,
		Text:      "Feature work needs integration tests.",
		Branch:    "feature",
		Origin:    factOriginAuthored,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := writeFacts(brainDir, "feature", []factRecord{authored}); err != nil {
		t.Fatal(err)
	}

	var calls int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, branch: "main", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	// Only the main session should have been distilled.
	if calls != 1 {
		t.Fatalf("expected only the main session distilled, got %d agent calls", calls)
	}

	// The returned summary and the persisted manifest must both reflect the whole
	// store: the feature branch and its authored fact survive a main-only run.
	hasFeature := false
	for _, b := range source.Branches {
		if b == "feature" {
			hasFeature = true
		}
	}
	if !hasFeature {
		t.Fatalf("branch-limited distill dropped feature branch from summary: %v", source.Branches)
	}
	if source.Facts != 2 || source.Authored != 1 || source.Distilled != 1 {
		t.Fatalf("summary lost whole-store counts: %+v", source)
	}

	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Facts == nil {
		t.Fatalf("manifest facts source not written: %v", err)
	}
	if manifest.Sources.Facts.Facts != 2 || manifest.Sources.Facts.Authored != 1 {
		t.Errorf("manifest dropped facts from untouched branch: %+v", manifest.Sources.Facts)
	}
	// The feature branch's authored fact must remain on disk untouched.
	featureFacts, err := loadFacts(brainDir, "feature")
	if err != nil || len(featureFacts) != 1 {
		t.Fatalf("feature facts disturbed by main-only distill: %d (%v)", len(featureFacts), err)
	}
}

func TestRunDistillForBrainBranchLimitedPreservesUntouchedCache(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var calls int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, jobs: 1, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("full distill: %v", err)
	}
	if calls != 2 {
		t.Fatalf("first run should distill both sessions, got %d", calls)
	}

	calls = 0
	mainOnly := opts
	mainOnly.branch = "main"
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, mainOnly, now); err != nil {
		t.Fatalf("main-only distill: %v", err)
	}
	if calls != 0 {
		t.Fatalf("main-only run should use cache, got %d calls", calls)
	}

	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("post-branch all distill: %v", err)
	}
	if calls != 0 {
		t.Fatalf("branch-limited run dropped untouched cache entries; all-branch reran %d chunks", calls)
	}
}

func TestRunDistillForBrainRedistillsOnContentChange(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	// Count only transcript-chunk invocations (those carry the line-numbered
	// transcript on stdin), so the reconcile agent's calls don't skew the tally.
	var transcriptChunks int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.Contains(string(input), "\tturn one") {
			transcriptChunks++
		}
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if transcriptChunks != 2 {
		t.Fatalf("first run should distill both sessions, got %d transcript chunks", transcriptChunks)
	}

	// Rewrite the main transcript bytes WITHOUT touching session id, checkpoint,
	// path, or branch — the scenario a compact↔raw re-export or exporter fix
	// produces. Incremental distill must notice and re-run only that session.
	mainTranscript := filepath.Join(brainDir, filepath.FromSlash("sessions/main/s1.jsonl"))
	if err := os.WriteFile(mainTranscript, []byte("turn one\nturn two\nturn three\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	transcriptChunks = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if transcriptChunks != 1 {
		t.Fatalf("content change should re-distill exactly the changed session, got %d transcript chunks", transcriptChunks)
	}
}

// The cache fingerprint is computed over the preprocessed distill input, so
// rewriting only stripped tool I/O must NOT force a re-distill, while changing
// the surviving conversation text must.
func TestRunDistillForBrainCacheIgnoresStrippedToolChurn(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	session := exportSession{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-time.Hour)}
	transcript := filepath.Join(brainDir, filepath.FromSlash(session.TranscriptPath))
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	withToolOutput := func(blob string) string {
		return strings.Join([]string{
			`{"type":"event_msg","payload":{"type":"user_message","message":"always use tabs"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","output":"` + blob + `"}}`,
		}, "\n") + "\n"
	}
	if err := os.WriteFile(transcript, []byte(withToolOutput("first command output")), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{session}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	var transcriptChunks int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.Contains(string(input), "always use") {
			transcriptChunks++
		}
		return "preferences.coding.style\tThe user prefers tabs.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if transcriptChunks != 1 {
		t.Fatalf("first run should distill the session once, got %d", transcriptChunks)
	}

	// Rewrite only the (stripped) tool output: the distill input is unchanged, so
	// the session must stay cached.
	if err := os.WriteFile(transcript, []byte(withToolOutput("a completely different and much longer command output blob")), 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptChunks = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("tool-churn run: %v", err)
	}
	if transcriptChunks != 0 {
		t.Fatalf("tool-I/O-only churn should not re-distill, got %d transcript chunks", transcriptChunks)
	}

	// Change the surviving conversation text: the distill input changes, so the
	// session must re-distill.
	changed := strings.Replace(withToolOutput("a completely different and much longer command output blob"), "always use tabs", "always use spaces", 1)
	if err := os.WriteFile(transcript, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptChunks = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("content-change run: %v", err)
	}
	if transcriptChunks != 1 {
		t.Fatalf("conversation-text change should re-distill, got %d transcript chunks", transcriptChunks)
	}
}

func TestRunDistillForBrainNoSessions(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{}}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: func(context.Context, string, []string, []byte, time.Duration) (string, error) { return "", nil }}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err == nil {
		t.Fatalf("expected error when no sessions are exported")
	}
}

func TestChunkTranscript(t *testing.T) {
	// Small budget forces multiple chunks; blank lines are skipped but still
	// advance the line counter so anchors stay accurate.
	content := "alpha\n\nbravo\ncharlie\n"
	chunks := chunkTranscript(content, 12) // each "N\tword\n" is ~8 bytes
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if chunks[0].StartLine != 1 {
		t.Errorf("first chunk should start at line 1, got %d", chunks[0].StartLine)
	}
	// "bravo" is line 3 (blank line 2 was skipped but counted).
	joined := chunks[0].Text + strings.Join(func() []string {
		var s []string
		for _, c := range chunks[1:] {
			s = append(s, c.Text)
		}
		return s
	}(), "")
	if !strings.Contains(joined, "3\tbravo") {
		t.Errorf("blank line did not advance line counter: %q", joined)
	}
	if !strings.Contains(joined, "1\talpha") || strings.Contains(joined, "2\t") {
		t.Errorf("blank line should be skipped, not numbered: %q", joined)
	}

	if got := chunkTranscript("", defaultDistillChunkSize); len(got) != 0 {
		t.Errorf("empty content should yield no chunks, got %d", len(got))
	}

	// A single oversized line still becomes its own chunk.
	big := strings.Repeat("x", 100)
	one := chunkTranscript(big+"\n", 10)
	if len(one) != 1 {
		t.Errorf("oversized single line should be one chunk, got %d", len(one))
	}
}

// TestRunDistillForBrainAbortsOnSystematicAgentFailure locks in the fail-fast
// guard: when every agent call errors (e.g. an invalid --model) and nothing has
// distilled, distill aborts within a few calls with the agent's own error rather
// than churning silently through every session reporting "0 facts found".
func TestRunDistillForBrainAbortsOnSystematicAgentFailure(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	var sessions []exportSession
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("s%d", i)
		tp := fmt.Sprintf("sessions/main/%s.jsonl", id)
		p := filepath.Join(brainDir, filepath.FromSlash(tp))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("turn one\nturn two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{SessionID: id, Branch: "main", LatestCheckpoint: "cp" + id, TranscriptPath: tp, CreatedAt: now.Add(-time.Duration(i) * time.Hour)})
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	var calls int
	failRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "", fmt.Errorf("agent failed: exit status 1: model 'bogus-4.6' may not exist")
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: failRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}

	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err == nil {
		t.Fatal("expected distill to abort on systematic agent failure")
	}
	if !strings.Contains(err.Error(), "distill aborted") || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("abort error should be actionable (mention --model): %v", err)
	}
	if !strings.Contains(err.Error(), "may not exist") {
		t.Fatalf("abort should surface the agent's own error: %v", err)
	}
	if calls != distillAgentAbortThreshold {
		t.Fatalf("expected abort at %d calls, not churning all 8 sessions; got %d", distillAgentAbortThreshold, calls)
	}
}

func TestExecOllamaDistillAgentUsesLoopbackGenerateAPI(t *testing.T) {
	var sawModel, sawSystem, sawPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			Model  string `json:"model"`
			System string `json:"system"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawModel, sawSystem, sawPrompt = req.Model, req.System, req.Prompt
		if req.Stream {
			t.Fatal("ollama distill must request non-streaming output")
		}
		fmt.Fprint(w, `{"response":"project.tooling.stack\tThe project uses Go.\n"}`)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	out, err := execOllamaDistillAgent(context.Background(), t.TempDir(), []string{"ollama", "llama3.2", "system prompt"}, []byte("chunk input"), time.Second)
	if err != nil {
		t.Fatalf("ollama runner: %v", err)
	}
	if !strings.Contains(out, "project.tooling.stack") {
		t.Fatalf("unexpected output %q", out)
	}
	if sawModel != "llama3.2" || sawSystem != "system prompt" || sawPrompt != "chunk input" {
		t.Fatalf("unexpected request model/system/prompt: %q %q %q", sawModel, sawSystem, sawPrompt)
	}
}

func TestExecOllamaDistillAgentRejectsNonLoopbackURL(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", "https://example.com/api/generate")
	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(), []string{"ollama", "llama3.2", "system"}, []byte("input"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "loopback-only") {
		t.Fatalf("expected loopback rejection, got %v", err)
	}
}

func TestExecOllamaDistillAgentRejectsNonLoopbackRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/api/generate", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(), []string{"ollama", "llama3.2", "system"}, []byte("input"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "redirect must stay loopback-only") {
		t.Fatalf("expected redirect loopback rejection, got %v", err)
	}
}
