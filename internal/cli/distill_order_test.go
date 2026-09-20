package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeOrderedDistillFixture lays down N same-branch sessions whose transcripts
// are individually identifiable, so a distill run's visit order is observable
// from the agent calls it makes.
func writeOrderedDistillFixture(t *testing.T, now time.Time, ids ...string) string {
	t.Helper()
	brainDir := t.TempDir()
	sessions := make([]exportSession, 0, len(ids))
	for i, id := range ids {
		path := filepath.Join("sessions", "main", id+".jsonl")
		full := filepath.Join(brainDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("marker "+id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{
			SessionID:        id,
			Branch:           "main",
			LatestCheckpoint: "cp-" + id,
			TranscriptPath:   filepath.ToSlash(path),
			// ids are given oldest-first, so each later id is more recent.
			CreatedAt: now.Add(time.Duration(i-len(ids)) * time.Hour),
		})
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

// recordingDistillRunner returns a fake agent whose calls append the marker of
// the session being distilled, in visit order.
func recordingDistillRunner(visited *[]string) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		for _, line := range strings.Split(string(input), "\n") {
			if _, marker, ok := strings.Cut(line, "marker "); ok {
				*visited = append(*visited, strings.TrimSpace(marker))
			}
		}
		return "conventions.testing\tA convention.\n", nil
	}
}

func TestSortDistillSessionsOrdersOldestOrNewestFirst(t *testing.T) {
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	build := func() []exportSession {
		return []exportSession{
			{SessionID: "b", CreatedAt: base.Add(2 * time.Hour)},
			{SessionID: "a", CreatedAt: base},
			{SessionID: "c", CreatedAt: base.Add(4 * time.Hour)},
		}
	}
	ids := func(sessions []exportSession) []string {
		out := make([]string, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, s.SessionID)
		}
		return out
	}

	oldest := build()
	sortDistillSessions(oldest, false)
	if got := strings.Join(ids(oldest), ","); got != "a,b,c" {
		t.Fatalf("default order must stay oldest-first, got %s", got)
	}

	newest := build()
	sortDistillSessions(newest, true)
	if got := strings.Join(ids(newest), ","); got != "c,b,a" {
		t.Fatalf("--newest-first must visit the most recent session first, got %s", got)
	}
}

func TestSortDistillSessionsBreaksTiesDeterministically(t *testing.T) {
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessions := []exportSession{{SessionID: "z", CreatedAt: base}, {SessionID: "a", CreatedAt: base}}
	sortDistillSessions(sessions, true)
	if sessions[0].SessionID != "z" {
		t.Fatalf("newest-first tie break must be reverse id, got %s", sessions[0].SessionID)
	}
	sortDistillSessions(sessions, false)
	if sessions[0].SessionID != "a" {
		t.Fatalf("oldest-first tie break must be ascending id, got %s", sessions[0].SessionID)
	}
}

// TestRunDistillNewestFirstVisitsRecentSessionsFirst is the end-to-end guard on
// the property the background backfill depends on: the most useful (most
// recent) facts must land first, because a long backfill's early output is the
// only output the user sees soon.
func TestRunDistillNewestFirstVisitsRecentSessionsFirst(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")

	var visited []string
	opts := distillCommandOptions{
		agent:         "command",
		agentCommand:  []string{"fake"},
		run:           recordingDistillRunner(&visited),
		maxChunkBytes: defaultDistillChunkSize,
		timeout:       time.Minute,
		newestFirst:   true,
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if got := strings.Join(visited, ","); got != "new,mid,old" {
		t.Fatalf("--newest-first must distill newest sessions first, visited %s", got)
	}
}

func TestRunDistillDefaultsToOldestFirst(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")

	var visited []string
	opts := distillCommandOptions{
		agent:         "command",
		agentCommand:  []string{"fake"},
		run:           recordingDistillRunner(&visited),
		maxChunkBytes: defaultDistillChunkSize,
		timeout:       time.Minute,
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if got := strings.Join(visited, ","); got != "old,mid,new" {
		t.Fatalf("the default order must remain oldest-first, visited %s", got)
	}
}

// TestRunDistillMaxSessionsDefersRatherThanSkips proves the backfill budget is
// a deferral, not a loss: the capped run spends on exactly N sessions, and the
// sessions it did not reach stay uncached so the NEXT run picks them up.
func TestRunDistillMaxSessionsDefersRatherThanSkips(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")

	var first []string
	var progress []distillProgress
	deferred := -1
	opts := distillCommandOptions{
		agent:              "command",
		agentCommand:       []string{"fake"},
		run:                recordingDistillRunner(&first),
		maxChunkBytes:      defaultDistillChunkSize,
		timeout:            time.Minute,
		newestFirst:        true,
		maxSessions:        2,
		progress:           func(p distillProgress) { progress = append(progress, p) },
		onDeferredSessions: func(n int) { deferred = n },
	}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if deferred != 1 {
		t.Fatalf("deferred=%d, want 1", deferred)
	}
	if got := strings.Join(first, ","); got != "new,mid" {
		t.Fatalf("--max-sessions 2 must spend on exactly the two newest sessions, visited %s", got)
	}
	if !strings.Contains(strings.Join(source.Warnings, "\n"), "--max-sessions 2 reached") {
		t.Fatalf("a budget-limited run must say so: %v", source.Warnings)
	}
	if len(progress) != 3 || progress[len(progress)-1].SessionsDone != 3 || progress[len(progress)-1].SessionsTotal != 3 {
		t.Fatalf("deferred sessions must still complete progress: %+v", progress)
	}

	// The deferred session must be undistilled, not silently cached.
	status := factsBackfillStatusForBrain(brainDir)
	if status.Sessions != 3 || status.Distilled != 2 {
		t.Fatalf("expected 2/3 sessions distilled after a capped run, got %d/%d", status.Distilled, status.Sessions)
	}

	var second []string
	opts.run = recordingDistillRunner(&second)
	opts.maxSessions = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("second runDistillForBrain: %v", err)
	}
	if deferred != 0 {
		t.Fatalf("deferred=%d, want 0", deferred)
	}
	if got := strings.Join(second, ","); got != "old" {
		t.Fatalf("the follow-up run must distill only the deferred session, visited %s", got)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 3 {
		t.Fatalf("all three sessions should be distilled after the follow-up, got %d", status.Distilled)
	}
}

// TestForcedCappedDistillDoesNotStrandDeferredSessions: `--force --max-sessions N`
// silently deleted the deferred sessions' facts and marked them done forever.
// A forced pass drops every distilled fact on each branch it VISITS
// (ensureBranch), and a session deferred by the budget `continue`s before that —
// so a sibling session on the same branch had already dropped its facts while
// its cache fingerprint was carried through unchanged. It is then skipped by
// every later run, and `status` reports it as distilled because it counts cache
// keys. The analogous pair, --session + --force, is rejected outright; this one
// had no guard at all.
func TestForcedCappedDistillDoesNotStrandDeferredSessions(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")
	repoDir := t.TempDir()

	var first []string
	opts := distillCommandOptions{
		agent:         "command",
		agentCommand:  []string{"fake"},
		run:           recordingDistillRunner(&first),
		maxChunkBytes: defaultDistillChunkSize,
		timeout:       time.Minute,
	}
	if _, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts, now); err != nil {
		t.Fatalf("initial runDistillForBrain: %v", err)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 3 {
		t.Fatalf("all three sessions should be distilled first, got %d", status.Distilled)
	}

	// The forced rebuild: capped at 2, so "old" is deferred — but "new" and
	// "mid" are on the same branch, and visiting either already dropped "old"'s
	// facts.
	var second []string
	opts.run = recordingDistillRunner(&second)
	opts.force = true
	opts.newestFirst = true
	opts.maxSessions = 2
	if _, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts, now); err != nil {
		t.Fatalf("forced runDistillForBrain: %v", err)
	}
	if got := strings.Join(second, ","); got != "new,mid" {
		t.Fatalf("the forced pass must spend on exactly the two newest sessions, visited %s", got)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 2 {
		t.Fatalf("a forced pass dropped the deferred session's facts but still marked it distilled: %d/%d\nit is skipped by every later run and status lies about it",
			status.Distilled, status.Sessions)
	}

	// And the follow-up must actually re-derive it.
	var third []string
	opts.run = recordingDistillRunner(&third)
	opts.force = false
	opts.newestFirst = false
	opts.maxSessions = 0
	if _, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts, now); err != nil {
		t.Fatalf("follow-up runDistillForBrain: %v", err)
	}
	if got := strings.Join(third, ","); got != "old" {
		t.Fatalf("the follow-up run must re-distill the stranded session, visited %s", got)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 3 {
		t.Fatalf("all three sessions should be distilled after the follow-up, got %d", status.Distilled)
	}
}

func TestBuildDistillPlanHonorsOrderAndBudget(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	plan, err := buildDistillPlan(brainDir, manifest, distillCommandOptions{newestFirst: true, maxSessions: 1}, "salt")
	if err != nil {
		t.Fatalf("buildDistillPlan: %v", err)
	}
	if len(plan.Sessions) != 3 {
		t.Fatalf("every session belongs in the plan, got %d", len(plan.Sessions))
	}
	if plan.Sessions[0].Session.SessionID != "new" {
		t.Fatalf("dry-run plan must respect --newest-first, first session was %s", plan.Sessions[0].Session.SessionID)
	}
	if plan.SessionsToDistill != 1 || plan.BudgetDeferredSessions != 2 {
		t.Fatalf("expected 1 planned + 2 deferred, got %d + %d", plan.SessionsToDistill, plan.BudgetDeferredSessions)
	}
}

// TestInProcessDistillOptionsPassTheConcurrencyGuard is a regression guard on a
// bug a live run found: runDistill rejects concurrency <= 0, but the two
// in-process callers (the watcher's gated distill step and `hook session-end`)
// build their options in Go, where the cobra flag defaults never apply. Both
// therefore failed before making a single agent call — the watcher's entire
// token-spending path and the session-end write loop were dead.
func TestInProcessDistillOptionsPassTheConcurrencyGuard(t *testing.T) {
	t.Parallel()
	w := defaultWatchOptions()
	if got := watchDistillOptions(w); got.concurrency <= 0 {
		t.Fatalf("watch's distill options must set concurrency, got %d", got.concurrency)
	}
	// A caller that raises --jobs must raise the pool the pipeline actually reads.
	w.distillJobs = 4
	if got := watchDistillOptions(w); got.concurrency != 4 {
		t.Fatalf("--jobs must drive the distill pool, got concurrency %d", got.concurrency)
	}
}

func TestNewestFirstDoesNotAutomaticallySupersedeNewerFacts(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "new")
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		if strings.HasPrefix(string(input), "CANDIDATES") {
			return "1 supersede 1 1.0\n", nil
		}
		if strings.Contains(string(input), "marker new") {
			return "project.tooling.stack\tUse Go 1.26.\n", nil
		}
		return "project.tooling.stack\tUse Go 1.20.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, newestFirst: true, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Status == "superseded" {
			t.Fatalf("newest-first silently superseded a fact: %+v", f)
		}
	}
	proposals, err := loadFactProposals(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("want one conflict awaiting review, got %+v", proposals)
	}
}
