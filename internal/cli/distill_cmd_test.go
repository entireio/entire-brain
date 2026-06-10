package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// writeSingleSessionFixture lays down a brain dir with a session manifest and
// one main-branch session whose transcript holds the given content, returning
// the brain dir.
func writeSingleSessionFixture(t *testing.T, now time.Time, transcript string) string {
	t.Helper()
	brainDir := t.TempDir()
	tp := "sessions/main/s1.jsonl"
	p := filepath.Join(brainDir, filepath.FromSlash(tp))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: tp, CreatedAt: now.Add(-time.Hour)},
		}}},
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
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"make it cohesive"}],"timestamp":1}}`,                        // pi user -> keep
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"adjusted"}]}}`,    // pi assistant: keep text, drop thinking
		`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"ls output blob"}]}}`,                // pi tool output -> drop
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
		"make it cohesive",   // pi user kept
		"adjusted",           // pi assistant text kept, thinking stripped
		"",                   // pi toolResult dropped
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
		calls++
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

func TestPreprocessTranscriptForDistillOpencodeDocument(t *testing.T) {
	// opencode exports one pretty-printed JSON document (not JSONL). Only
	// parts of type "text" are conversation; tool/patch/reasoning/step parts
	// are mechanics and must be stripped (a real 22.6 MB opencode session was
	// 1.3% conversation text).
	doc := `{
  "info": {
    "id": "ses_x",
    "title": "Remove header borders"
  },
  "messages": [
    {
      "info": {"role": "user"},
      "parts": [
        {"type": "text", "text": "Remove the header bottom border"},
        {"type": "file", "url": "frontend/Page.tsx"}
      ]
    },
    {
      "info": {"role": "assistant"},
      "parts": [
        {"type": "step-start"},
        {"type": "tool", "tool": "edit", "state": {"output": "huge tool output"}},
        {"type": "patch", "hash": "abc", "files": ["frontend/Page.tsx"]},
        {"type": "step-finish", "tokens": {"total": 19815}}
      ]
    },
    {
      "info": {"role": "assistant"},
      "parts": [
        {"type": "reasoning", "text": "thinking about borders"},
        {"type": "text", "text": "Done: removed both borders."}
      ]
    }
  ]
}`
	out, ok := distillDocumentConversation(doc)
	if !ok {
		t.Fatal("document-form transcript not detected")
	}
	got := strings.Split(out, "\n")
	// The output mirrors the document line-for-line (the JSONL provenance
	// contract): each message's text sits on the line where its object opens
	// in the original document, so fact anchors point at real file lines.
	if len(got) != strings.Count(doc, "\n")+1 {
		t.Fatalf("line count = %d, want %d (one output line per document line): %q", len(got), strings.Count(doc, "\n")+1, out)
	}
	want := map[int]string{
		7:  "Remove the header bottom border", // user message opens on line 7; file part dropped
		23: "Done: removed both borders.",     // assistant message opens on line 23; reasoning dropped
	}
	for i, line := range got {
		if line != want[i+1] { // tool-only message (line 14) and structure stay blank
			t.Errorf("line %d = %q, want %q", i+1, line, want[i+1])
		}
	}
	if strings.Contains(out, "huge tool output") || strings.Contains(out, "thinking about") {
		t.Errorf("tool/reasoning content leaked into distill input: %q", out)
	}

	// The preprocessor must route document transcripts through this path.
	if pre := preprocessTranscriptForDistill(doc); !strings.Contains(pre, "Remove the header bottom border") || strings.Contains(pre, "step-finish") {
		t.Errorf("preprocess did not strip document transcript: %q", truncateString(pre, 200))
	}

	// JSONL and plain-text transcripts must NOT match the document path.
	if _, ok := distillDocumentConversation(`{"type":"user","message":{"content":"hi"}}` + "\n"); ok {
		t.Error("JSONL transcript misdetected as document")
	}
	if _, ok := distillDocumentConversation("turn one\nturn two\n"); ok {
		t.Error("plain text misdetected as document")
	}

	// A document with a messages array in some OTHER chat shape (role/content,
	// no opencode-style text parts) yields no conversation text and must NOT
	// be claimed: claiming it would blank the entire session and cache it as
	// distilled with zero facts. It falls through to the JSONL path, which
	// passes the unparseable pretty-printed lines through unstripped so the
	// agent can still mine them.
	foreign := `{
  "messages": [
    {"role": "user", "content": "always use tabs"},
    {"role": "assistant", "content": "noted"}
  ]
}`
	if _, ok := distillDocumentConversation(foreign); ok {
		t.Error("foreign chat document with no text parts must not be swallowed by the document path")
	}
	if pre := preprocessTranscriptForDistill(foreign); !strings.Contains(pre, "always use tabs") {
		t.Errorf("foreign chat document content must pass through for the agent to mine: %q", pre)
	}
}

func TestRunDistillForBrainConcurrentChunks(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	// One session with many one-line chunks (maxChunkBytes=1 forces each line
	// into its own chunk).
	brainDir := writeSingleSessionFixture(t, now, strings.Repeat("a conversational turn\n", 12))

	var mu sync.Mutex
	inFlight, maxInFlight, calls := 0, 0, 0
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		mu.Lock()
		calls++
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // hold the slot so overlap is observable
		mu.Lock()
		inFlight--
		mu.Unlock()
		return "", nil // no facts: keeps reconcile out of the in-flight accounting
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: 1, timeout: time.Minute, concurrency: 3}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if calls != 12 {
		t.Fatalf("expected one agent call per chunk (12), got %d", calls)
	}
	if maxInFlight < 2 {
		t.Errorf("agent calls never overlapped (max in flight %d) with concurrency 3", maxInFlight)
	}
	if maxInFlight > 3 {
		t.Errorf("max in flight %d exceeds concurrency 3", maxInFlight)
	}
}

// pipelineFixture lays down a transcript with `lines` one-chunk-able lines
// (maxChunkBytes=1 puts each on its own chunk) and returns the brain dir plus
// the session slice for driving startSessionPrefetch directly.
func pipelineFixture(t *testing.T, lines int) (string, []exportSession) {
	t.Helper()
	brainDir := t.TempDir()
	tp := "sessions/main/s1.jsonl"
	p := filepath.Join(brainDir, filepath.FromSlash(tp))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("a conversational turn\n", lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	return brainDir, []exportSession{{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: tp, CreatedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}}
}

func mainBranch(exportSession) string { return "main" }

func TestSessionPrefetchLookaheadBoundedByConsumption(t *testing.T) {
	// A pool slot is held until the consumer reads the result, so dispatch can
	// run at most `concurrency` calls ahead of consumption. With a consumer
	// that aborts after 5 results, total launched calls must stay within
	// consumed+concurrency — a completion-released slot would instead let the
	// dispatcher launch (and bill) all 40 chunks while the consumer lagged.
	brainDir, sessions := pipelineFixture(t, 40)
	var mu sync.Mutex
	calls := 0
	failingRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "", errors.New("agent down")
	}
	const concurrency = 3
	opts := distillCommandOptions{run: failingRun, maxChunkBytes: 1, timeout: time.Minute, concurrency: concurrency}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared := startSessionPrefetch(ctx, brainDir, t.TempDir(), nil, sessions, distillCache{}, opts, mainBranch)
	ps := <-prepared
	if len(ps.chunks) != 40 {
		t.Fatalf("fixture should chunk to 40, got %d", len(ps.chunks))
	}
	const consumed = 5
	for i := 0; i < consumed; i++ {
		if _, err := ps.result(i); err == nil {
			t.Fatal("expected agent error")
		}
	}
	cancel()
	mu.Lock()
	launched := calls
	mu.Unlock()
	if launched > consumed+concurrency {
		t.Errorf("dispatcher launched %d calls after %d were consumed (concurrency %d); lookahead is not consumption-bounded", launched, consumed, concurrency)
	}
}

func TestSessionPrefetchConcurrencyOneIsLazyAndSequential(t *testing.T) {
	// concurrency<=1 must reproduce the original behavior exactly: one agent
	// process at a time, calls made lazily on consumption — a chunk the
	// consumer never asks about (e.g. after an abort) costs nothing.
	brainDir, sessions := pipelineFixture(t, 8)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "", errors.New("agent down")
	}
	opts := distillCommandOptions{run: run, maxChunkBytes: 1, timeout: time.Minute, concurrency: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared := startSessionPrefetch(ctx, brainDir, t.TempDir(), nil, sessions, distillCache{}, opts, mainBranch)
	ps := <-prepared
	for i := 0; i < 3; i++ {
		if _, err := ps.result(i); err == nil {
			t.Fatal("expected agent error")
		}
		if calls != i+1 {
			t.Fatalf("sequential mode made %d calls after consuming %d results; calls must be lazy", calls, i+1)
		}
	}
	if calls != 3 {
		t.Errorf("sequential mode launched %d calls for 3 consumed chunks; chunks 4-8 must never run", calls)
	}
}

// TestRunDistillForBrainCrossSessionConcurrency locks in the pipeline's whole
// point: the call pool spans sessions, so two 1-chunk sessions — the common
// shape after stripping — overlap their agent calls. The per-session pool
// this replaced could never overlap them, serializing a corpus of small
// sessions on agent latency.
func TestRunDistillForBrainCrossSessionConcurrency(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 + s2, one chunk each at default size

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // hold the slot so overlap is observable
		mu.Lock()
		inFlight--
		mu.Unlock()
		return "", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, concurrency: 2}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if maxInFlight < 2 {
		t.Errorf("agent calls for distinct sessions never overlapped (max in flight %d) with concurrency 2", maxInFlight)
	}
	if maxInFlight > 2 {
		t.Errorf("max in flight %d exceeds the shared pool size 2", maxInFlight)
	}
}

func TestRunDistillForBrainFlushesIncrementally(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	// flushEvery=1 flushes after every agent call; by the time s2's chunk is
	// distilled, s1's fact must already be durable on disk — the property that
	// makes a killed run resumable instead of losing everything.
	var calls int
	sawFlushedFact := false
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 2 {
			if facts, err := loadFacts(brainDir, "main"); err == nil && len(facts) == 1 {
				sawFlushedFact = true
			}
		}
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, flushEvery: 1}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if !sawFlushedFact {
		t.Error("s1's fact was not on disk while s2 was distilling — mid-run flush missing")
	}
	// The final state must be identical to a non-flushing run.
	if source.Facts != 2 || source.Distilled != 2 {
		t.Fatalf("expected 2 distilled facts after flushing run, got %+v", source)
	}
	for _, branch := range []string{"main", "feature"} {
		facts, err := loadFacts(brainDir, branch)
		if err != nil || len(facts) != 1 {
			t.Fatalf("expected 1 fact on %s, got %d (%v)", branch, len(facts), err)
		}
	}
	cache := loadDistillCache(brainDir)
	if len(cache.Sessions) != 2 {
		t.Fatalf("final cache should hold both sessions, got %v", cache.Sessions)
	}
}

func TestRunDistillForBrainFlushesDuringFailureStreak(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	// maxChunkBytes=1 puts each line in its own chunk: one success, then a
	// failure streak long enough to cross the flush interval.
	brainDir := writeSingleSessionFixture(t, now, strings.Repeat("a conversational turn\n", 6))

	// Chunk 1 distills a fact (2 counted calls: distill + reconcile), then the
	// agent starts failing. The flush interval (3) is crossed on chunk 2's
	// failure, so by chunk 3's call the fact must already be durable — failure
	// streaks are exactly when runs get killed, and the failure path skipping
	// maybeFlush left everything since the last flush at risk for the whole
	// streak.
	var calls int
	sawFlushedFact := false
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 1 {
			return "preferences.coding.style\tThe user prefers concise commits.\n", nil
		}
		if calls == 3 {
			if facts, err := loadFacts(brainDir, "main"); err == nil && len(facts) == 1 {
				sawFlushedFact = true
			}
		}
		return "", errors.New("rate limited")
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: 1, timeout: time.Minute, flushEvery: 3}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if !sawFlushedFact {
		t.Error("fact was not flushed during the failure streak; the error path must call maybeFlush too")
	}
}

func TestRunDistillForBrainForceFlushDoesNotResurrectCache(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	fact := "preferences.coding.style\tThe user prefers concise commits.\n"
	seedRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return fact, nil
	}
	seedOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: seedRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, seedOpts, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if cache := loadDistillCache(brainDir); len(cache.Sessions) != 2 {
		t.Fatalf("seed run should cache both sessions, got %v", cache.Sessions)
	}

	// A --force run drops every previously distilled fact from the in-memory
	// store before rebuilding. If a mid-run flush persisted prevCache
	// fingerprints for sessions not yet re-visited, killing the run there and
	// rerunning WITHOUT --force would skip those sessions as "unchanged" even
	// though the same flush already deleted their facts — permanent silent
	// loss. So while s2 is being distilled (a flush already ran during s1),
	// the on-disk cache must NOT hold s2's still-valid old fingerprint. (It
	// does not hold s1 yet either: a session enters newCache only once it
	// completes, and the next flush after that persists it.)
	var calls int
	sawResurrectedEntry := false
	forceRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 2 { // s2's distill call: at least one flush has happened
			if _, ok := loadDistillCache(brainDir).Sessions["s2"]; ok {
				sawResurrectedEntry = true
			}
		}
		return fact, nil
	}
	forceOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: forceRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, force: true, flushEvery: 1}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, forceOpts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}
	if sawResurrectedEntry {
		t.Error("mid-run flush under --force resurrected the unvisited s2 cache entry; a killed force run would lose s2's facts forever")
	}
	if cache := loadDistillCache(brainDir); len(cache.Sessions) != 2 {
		t.Fatalf("completed force run should cache both sessions, got %v", cache.Sessions)
	}
}

func TestRunDistillForBrainFlushSkipsCleanBranches(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	seedRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	seedOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: seedRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, seedOpts, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Invalidate only s2 so the second run re-distills feature while main is
	// loaded but untouched. Backdate main's facts file: if any flush (mid-run
	// or final) rewrote the clean branch, the mtime would advance.
	s2 := filepath.Join(brainDir, filepath.FromSlash("sessions/branches/feature/s2.jsonl"))
	if err := os.WriteFile(s2, []byte("turn one\nturn two\na new turn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainFacts := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main")))
	backdated := now.Add(-24 * time.Hour)
	if err := os.Chtimes(mainFacts, backdated, backdated); err != nil {
		t.Fatal(err)
	}

	// A different taxonomy path keeps reconcile off the agent (no existing
	// facts at the candidate's paths).
	secondRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "workflow.testing.rules\tRun go test before pushing.\n", nil
	}
	secondOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: secondRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, flushEvery: 1}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, secondOpts, now); err != nil {
		t.Fatalf("second run: %v", err)
	}

	info, err := os.Stat(mainFacts)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(backdated) {
		t.Error("clean branch main was rewritten by a flush; flushes must only write dirty branches")
	}
	if facts, err := loadFacts(brainDir, "feature"); err != nil || len(facts) != 2 {
		t.Errorf("feature should hold its seeded and new fact, got %d (%v)", len(facts), err)
	}
}

func TestRunDistillForBrainSurvivesMidRunFlushFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The simulated disk failure is a read-only directory, which Windows
		// permission bits do not enforce — the flush never fails there.
		t.Skip("directory permission bits do not restrict writes on Windows")
	}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	// Two chunks (maxChunkBytes=1): the first produces a fact whose flush
	// fails, the second gives the retry a chance after the disk "recovers".
	brainDir := writeSingleSessionFixture(t, now, "turn one\nturn two\n")

	factsDir := filepath.Join(brainDir, factsDirName)
	if err := os.MkdirAll(factsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(factsDir, 0o700) }
	t.Cleanup(restore)

	var calls int
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		switch calls {
		case 1: // chunk 1: the flush after this fact will hit a read-only dir
			if err := os.Chmod(factsDir, 0o500); err != nil {
				t.Fatal(err)
			}
			return "preferences.coding.style\tThe user prefers concise commits.\n", nil
		default: // chunk 2: disk recovers; the retry flush must succeed
			restore()
			return "", nil
		}
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: 1, timeout: time.Minute, flushEvery: 1}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("a transient mid-run flush failure must not abort the run: %v", err)
	}
	flushWarned := false
	for _, w := range source.Warnings {
		if strings.Contains(w, "mid-run flush failed") {
			flushWarned = true
		}
	}
	if !flushWarned {
		t.Errorf("expected a mid-run flush warning, got %v", source.Warnings)
	}
	// The branch stayed dirty through the failure, so the retry persisted it.
	if facts, err := loadFacts(brainDir, "main"); err != nil || len(facts) != 1 {
		t.Errorf("fact lost across the flush failure: got %d (%v)", len(facts), err)
	}
}

func TestRunDistillForBrainBranchFilterKeepsOtherBranchCacheEntries(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	fact := "preferences.coding.style\tThe user prefers concise commits.\n"
	seedRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return fact, nil
	}
	seedOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: seedRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, seedOpts, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// A --branch feature run must not evict main's cache entries: the final
	// flush persists newCache only, so a filtered session that is never copied
	// over forces the next unfiltered run to re-distill it from scratch.
	filteredRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		t.Error("no agent call expected: s2 is cached and s1 is branch-filtered")
		return "", nil
	}
	filteredOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: filteredRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, branch: "feature"}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, filteredOpts, now); err != nil {
		t.Fatalf("branch-filtered run: %v", err)
	}
	cache := loadDistillCache(brainDir)
	if _, ok := cache.Sessions["s1"]; !ok {
		t.Errorf("branch-filtered run evicted s1's cache entry; the next unfiltered run would re-distill it: %v", cache.Sessions)
	}
	if _, ok := cache.Sessions["s2"]; !ok {
		t.Errorf("cached unchanged session s2 missing from cache: %v", cache.Sessions)
	}
}

func TestRunDistillForBrainForceKeepsProposalBacklogUntilFinalFlush(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	// A queued review backlog must survive a killed --force run: mid-run
	// flushes keep priors, and only the final flush of a COMPLETED rebuild
	// drops them in favor of the rebuilt proposals.
	backlog := []factProposal{{Action: "merge", CandidateID: "c1", TargetID: "t1", Confidence: 0.5, Branch: "main"}}
	if err := writeFactProposals(brainDir, "main", backlog); err != nil {
		t.Fatal(err)
	}

	var calls int
	backlogWipedMidRun := false
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 2 { // s2's distill call: main was already flushed during s1
			if prior, err := loadFactProposals(brainDir, "main"); err != nil || len(prior) == 0 {
				backlogWipedMidRun = true
			}
		}
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, force: true, flushEvery: 1}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if backlogWipedMidRun {
		t.Error("mid-run --force flush wiped the proposal backlog; a killed force run would lose the review queue")
	}
	// The completed rebuild replaces the backlog (this run queued nothing).
	if final, err := loadFactProposals(brainDir, "main"); err != nil || len(final) != 0 {
		t.Errorf("completed force run should drop prior proposals, got %d (%v)", len(final), err)
	}
}

// TestRunDistillForBrainSessionFilter locks in the --session fast path
// (Phase 2 item 5): only the named session is distilled, and every other
// session's cache entry is carried through so the next unfiltered run does
// not re-distill the world.
func TestRunDistillForBrainSessionFilter(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now) // s1 (main, older) then s2 (feature)

	fact := "preferences.coding.style\tThe user prefers concise commits.\n"
	seedRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return fact, nil
	}
	seedOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: seedRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, seedOpts, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Invalidate BOTH transcripts, then run with --session s2: only s2 may
	// cost agent calls, and s1's (now stale) cache entry must survive.
	for _, p := range []string{"sessions/main/s1.jsonl", "sessions/branches/feature/s2.jsonl"} {
		if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(p)), []byte("turn one\nturn two\na new turn\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	countRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "", nil
	}
	filtered := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: countRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, session: "s2"}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, filtered, now)
	if err != nil {
		t.Fatalf("session-filtered run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one agent call (s2's single chunk), got %d", calls)
	}
	if source.ChunksScanned != 1 {
		t.Fatalf("only s2 should be scanned, got %d chunks", source.ChunksScanned)
	}
	cache := loadDistillCache(brainDir)
	if _, ok := cache.Sessions["s1"]; !ok {
		t.Errorf("session-filtered run evicted s1's cache entry: %v", cache.Sessions)
	}
	if _, ok := cache.Sessions["s2"]; !ok {
		t.Errorf("s2 missing from cache after its own distill: %v", cache.Sessions)
	}
}

// TestRunDistillForBrainSessionWithForceRejected: a forced rebuild drops every
// distilled fact on the loaded branch but a session filter re-derives only one
// session's — silent loss for the rest, so the combination must refuse to run.
func TestRunDistillForBrainSessionWithForceRejected(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"},
		run: func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
			t.Error("no agent call expected; the option combination must be rejected up front")
			return "", nil
		},
		maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, session: "s1", force: true}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected the --session/--force combination to be rejected, got %v", err)
	}
}
