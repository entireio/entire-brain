package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
