package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func stagedTestFact(text string, paths []string, status string, at time.Time) factRecord {
	return factRecord{
		ID:        factRecordID(text, paths),
		Text:      text,
		Paths:     paths,
		Status:    status,
		Branch:    "main",
		Origin:    factstore.OriginDistilled,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

// A candidate resembling nothing stored or staged is new by construction, so
// the pass must promote it without spending an agent call.
func TestStagedReconcilePromotesUnflaggedWithoutAgent(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{
		stagedTestFact("the daemon label derives from the daemon name", []string{"architecture.data.flow"}, factStatusActive, now.Add(-time.Hour)),
		stagedTestFact("tests run with count one to defeat the build cache", []string{"workflow.testing.rules"}, factStatusCandidate, now),
	}
	calls := 0
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		calls++
		return "", nil
	}
	updated, stats, warnings := reconcileStagedCandidates(context.Background(), run, nil, facts, ".", time.Minute, 2, now)
	if calls != 0 {
		t.Fatalf("an unflagged candidate must not cost an agent call, got %d", calls)
	}
	if stats.Candidates != 1 || stats.Promoted != 1 || stats.Clusters != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings %v", warnings)
	}
	active := 0
	for _, f := range updated {
		if f.Status == factStatusCandidate {
			t.Fatalf("candidate left staged after the pass: %q", f.Text)
		}
		if f.Status == factStatusActive {
			active++
		}
	}
	if active != 2 {
		t.Fatalf("want 2 active facts, got %d", active)
	}
}

// Two same-run near-duplicates with nothing stored must still be adjudicated:
// the earliest is new by election and serves as EXISTING for the rest, so the
// agent can consolidate them instead of both being silently stored.
func TestStagedReconcileClustersSameRunNearDuplicates(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	first := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusCandidate, now.Add(-time.Minute))
	second := stagedTestFact("use tabs to indent source files", []string{"preferences.coding.style"}, factStatusCandidate, now)
	var sawExisting string
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		sawExisting = string(input)
		return "1 merge 1 0.95", nil
	}
	updated, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{first, second}, ".", time.Minute, 2, now)
	if stats.Clusters != 1 || stats.AgentCalls != 1 || stats.Promoted != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if !strings.Contains(sawExisting, first.Text) {
		t.Fatalf("the elected head must be in EXISTING so the agent can merge into it:\n%s", sawExisting)
	}
	// Keep-both: the merge ruling publishes the duplicate too, cross-linked to
	// the elected head for the compaction loop to settle (COR-2273).
	byID := map[string]factRecord{}
	for _, f := range updated {
		if f.Status != factStatusActive {
			t.Fatalf("every adjudicated candidate must publish: %+v", f)
		}
		byID[f.ID] = f
	}
	if !contains(byID[second.ID].RelatedIDs, first.ID) || !contains(byID[first.ID].RelatedIDs, second.ID) {
		t.Fatalf("merged pair must be cross-linked: %+v", updated)
	}
}

// A candidate close to a stored active fact goes to the agent; a confident
// supersede ruling keeps both facts active and cross-links them — it never
// retires the stored fact on its own (the compaction loop settles the pair).
func TestStagedReconcilePublishesSupersedeAsKeepBoth(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	stored := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	candidate := stagedTestFact("indent all source files with spaces", []string{"preferences.coding.style"}, factStatusCandidate, now)
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "1 supersede 1 0.95", nil
	}
	updated, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, ".", time.Minute, 2, now)
	if stats.AgentCalls != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	byID := map[string]factRecord{}
	for _, f := range updated {
		byID[f.ID] = f
	}
	if byID[stored.ID].Status != factStatusActive {
		t.Fatalf("stored fact must stay active, got %q", byID[stored.ID].Status)
	}
	if byID[candidate.ID].Status != factStatusActive {
		t.Fatalf("candidate must publish, got %q", byID[candidate.ID].Status)
	}
	if !contains(byID[candidate.ID].RelatedIDs, stored.ID) || !contains(byID[stored.ID].RelatedIDs, candidate.ID) {
		t.Fatalf("conflicting pair must be cross-linked: %+v", updated)
	}
}

// An agent failure preserves both texts but leaves the candidate unpublished.
func TestStagedReconcileAgentFailureKeepsBoth(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	stored := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	candidate := stagedTestFact("indent all source files with spaces", []string{"preferences.coding.style"}, factStatusCandidate, now)
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "", errors.New("agent down")
	}
	updated, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, ".", time.Minute, 2, now)
	active := 0
	for _, f := range updated {
		if f.Status == factStatusActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("agent failure must keep only the original active, got %d", active)
	}
	if len(warnings) == 0 {
		t.Fatal("the degradation must be disclosed")
	}
}

// In newest-first order, extraction order is not evidence of recency: a
// supersede ruling never retires the stored fact — both stay active,
// cross-linked for the compaction loop.
func TestStagedReconcileNewestFirstSupersedeKeepsBoth(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	stored := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	candidate := stagedTestFact("indent all source files with spaces", []string{"preferences.coding.style"}, factStatusCandidate, now)
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "1 supersede 1 0.95", nil
	}
	updated, _, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, ".", time.Minute, 2, now)
	for _, f := range updated {
		if f.Status != factStatusActive {
			t.Fatalf("supersede ruling must not retire or stage anything: %+v", f)
		}
	}
}

// Candidates left staged by a crashed run are reconciled by the next pass even
// when it extracted nothing new — the resume rule that makes staging safe.
func TestStagedReconcileResumesLeftovers(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	leftover := stagedTestFact("exports read checkpoint blobs through one batch child", []string{"architecture.data.flow"}, factStatusCandidate, now.Add(-24*time.Hour))
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		t.Fatal("nothing resembles the leftover; no agent call expected")
		return "", nil
	}
	updated, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{leftover}, ".", time.Minute, 2, now)
	if stats.Candidates != 1 || stats.Promoted != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if updated[0].Status != factStatusActive {
		t.Fatalf("leftover must promote, got %q", updated[0].Status)
	}
}

// Concurrent cluster calls must not make the outcome depend on completion
// order: identical inputs produce identical stores, run after run.
func TestStagedReconcileIsDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	var facts []factRecord
	for i := 0; i < 6; i++ {
		facts = append(facts, stagedTestFact(
			fmt.Sprintf("cluster alpha duplicate statement number %d about tabs", i),
			[]string{"preferences.coding.style"}, factStatusCandidate, now.Add(time.Duration(i)*time.Second)))
		facts = append(facts, stagedTestFact(
			fmt.Sprintf("cluster beta duplicate statement number %d about exports", i),
			[]string{"architecture.data.flow"}, factStatusCandidate, now.Add(time.Duration(i)*time.Second)))
	}
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		// Deterministic on input: merge every candidate into existing 1.
		n := 0
		for _, line := range strings.Split(string(input), "\n") {
			if line == "" || line == "CANDIDATES" {
				continue
			}
			if line == "EXISTING" {
				break
			}
			n++
		}
		var out strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&out, "%d merge 1 0.9\n", i)
		}
		return out.String(), nil
	}
	first, _, _ := reconcileStagedCandidates(context.Background(), run, nil, append([]factRecord(nil), facts...), ".", time.Minute, 4, now)
	second, _, _ := reconcileStagedCandidates(context.Background(), run, nil, append([]factRecord(nil), facts...), ".", time.Minute, 4, now)
	sortFactRecords(first)
	sortFactRecords(second)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same input, different stores: concurrent clusters leaked completion order")
	}
}

// A high-confidence merge whose two texts share almost no vocabulary is a
// hallucinated pairing (measured: every such ruling on the calibration corpus
// was wrong). Keep-both means even a hallucinated ruling never destroys or
// retires a fact; the wrong cross-link is settled by compaction (COR-2273).
func TestStagedReconcileLexicallyUnrelatedRulingsDestroyNothing(t *testing.T) {
	for _, kind := range []string{factActionMerge, factActionSupersede} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			similar := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-2*time.Hour))
			unrelated := stagedTestFact("the export pipeline reads checkpoint blobs in batches", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
			candidate := stagedTestFact("use tabs to indent source files", []string{"preferences.coding.style"}, factStatusCandidate, now)
			run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
				// EXISTING is most-recent-first: 1 = unrelated, 2 = similar. Rule a
				// confident merge into the UNRELATED fact — the hallucination shape.
				if !strings.Contains(string(input), "1 ") {
					t.Fatalf("unexpected input:\n%s", input)
				}
				return "1 " + kind + " 1 0.95", nil
			}
			updated, _, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{similar, unrelated, candidate}, ".", time.Minute, 2, now)
			for _, f := range updated {
				if f.Status != factStatusActive {
					t.Fatalf("nothing may be destroyed or retired by a ruling: %+v", f)
				}
			}
			if len(updated) != 3 {
				t.Fatalf("every original and the candidate must survive, got %d", len(updated))
			}
		})
	}
}

// A staged candidate that was flushed to disk and then consumed by the
// reconcile pass (merged away) must not be resurrected by the rebase-onto-disk
// at the final write; one staged by ANOTHER run (absent from our baseline) must
// survive for that run's resume pass.
func TestRebaseDropsConsumedCandidatesOnly(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	consumed := stagedTestFact("use tabs to indent source files", []string{"preferences.coding.style"}, factStatusCandidate, now)
	foreign := stagedTestFact("another run staged this statement", []string{"architecture.data.flow"}, factStatusCandidate, now)
	target := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	if err := factstore.Write(brainDir, []factRecord{target, consumed, foreign}); err != nil {
		t.Fatal(err)
	}
	// Baseline knew target+consumed (this run staged and flushed them); memory
	// holds only target: the pass merged the candidate away.
	baseline := []factRecord{target, consumed}
	inMemory := []factRecord{target}
	rebased, err := rebaseFactsOntoDisk(brainDir, inMemory, baseline)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, f := range rebased {
		ids[f.ID] = f.Status
	}
	if _, resurrected := ids[consumed.ID]; resurrected {
		t.Fatal("a consumed candidate was resurrected by the rebase")
	}
	if status, ok := ids[foreign.ID]; !ok || status != factStatusCandidate {
		t.Fatalf("a foreign run's staged candidate must survive for its resume pass, got %v", ids)
	}
	if _, kept := ids[target.ID]; !kept {
		t.Fatal("the merge target must survive")
	}
}

func TestStagedReconcilePersistsResumeBeforeExtraction(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "new transcript content\n")
	target := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	leftover := stagedTestFact("use tabs to indent source files", target.Paths, factStatusCandidate, now.Add(-time.Minute))
	if err := factstore.Write(brainDir, []factRecord{target, leftover}); err != nil {
		t.Fatal(err)
	}
	reconciled := false
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		if strings.HasPrefix(string(input), "CANDIDATES\n") {
			reconciled = true
			return "1 merge 1 0.95", nil
		}
		if !reconciled {
			t.Error("extraction began before leftover reconciliation")
		}
		stored, err := factstore.Load(brainDir)
		if err != nil || len(stored) != 2 || stored[indexOfFact(stored, leftover.ID)].Status != factStatusActive {
			t.Errorf("resumed candidate must be published durably before extraction: %+v, %v", stored, err)
		}
		return "workflow.testing.rules\tRun tests before committing.\n", nil
	}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: run, timeout: time.Minute, flushEvery: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if source.PendingReconcile != 0 || source.CandidatesReconciled != 2 || source.ReconcileCalls != 1 {
		t.Fatalf("resume and new work must both be counted: %+v", source)
	}
	stored, err := factstore.Load(brainDir)
	if err != nil || len(stored) != 3 {
		t.Fatalf("a reconciled fact was lost after final flush: %+v, %v", stored, err)
	}
}

// forceSweepFact seeds an active distilled fact anchored to one session, the
// shape the force sweep adjudicates.
func forceSweepFact(text, branch, sessionID string, at time.Time) factRecord {
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	return factRecord{
		ID: factRecordID(text, paths), Text: text, Paths: paths, Branch: branch,
		Origin: factstore.OriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: sessionID}},
		CreatedAt:  at, UpdatedAt: at,
	}
}

func forceSweepIDs(t *testing.T, brainDir string) map[string]factRecord {
	t.Helper()
	facts, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]factRecord, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	return byID
}

var forceSweepRun = func(context.Context, string, []string, []byte, time.Duration) (string, error) {
	return "workflow.testing.rules\tRun tests before committing.\n", nil
}

// A capped force run (--max-sessions) must not touch facts from the sessions
// it deferred; the deferred session re-derives on the next run because its
// cache entry was dropped. (Bugbot on PR #590: the pre-drop lost everything
// outside the cap's scope.)
func TestForceWithSessionCapKeepsDeferredSessionsFacts(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	staleS1 := forceSweepFact("old derivation from session one", "main", "s1", now.Add(-time.Hour))
	keepS2 := forceSweepFact("derivation from the deferred session", "feature", "s2", now.Add(-time.Hour))
	if err := factstore.Write(brainDir, []factRecord{staleS1, keepS2}); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: forceSweepRun, timeout: time.Minute, force: true, maxSessions: 1}
	if _, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	rebuilt := factRecordID("Run tests before committing.", normalizeFactPaths([]string{"workflow.testing.rules"}))
	byID := forceSweepIDs(t, brainDir)
	if _, ok := byID[staleS1.ID]; ok {
		t.Fatalf("the rebuilt session's stale fact must be swept: %+v", byID)
	}
	if _, ok := byID[keepS2.ID]; !ok {
		t.Fatalf("the deferred session's fact must survive the capped force run: %+v", byID)
	}
	if _, ok := byID[rebuilt]; !ok {
		t.Fatalf("the visited session's fact must be re-derived: %+v", byID)
	}
	// The deferred session's cache entry was dropped, so a plain follow-up run
	// re-distills it (s1 stays cached from the force run).
	calls := 0
	counting := func(ctx context.Context, dir string, args []string, input []byte, d time.Duration) (string, error) {
		calls++
		return forceSweepRun(ctx, dir, args, input, d)
	}
	opts2 := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: counting, timeout: time.Minute}
	source, err := runDistillForBrain(context.Background(), repoDir, brainDir, opts2, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || source.Distilled != 2 {
		t.Fatalf("the deferred session must re-derive on the next run: calls=%d distilled=%d", calls, source.Distilled)
	}
	if byID = forceSweepIDs(t, brainDir); len(byID) != 2 {
		t.Fatalf("store = %+v, want deferred-session fact + rebuilt fact", byID)
	}
}

// A full-scope force run drops exactly the stale facts it proved stale:
// distilled, active, not regenerated, and citing only rebuilt sessions.
// Authored and retired records survive, and so does a fact citing a session
// that left the catalog — dropping those is the mirror sweep's job now.
func TestForceSweepDropsOnlyProvenStaleFacts(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	stale := forceSweepFact("exports use batch checkpoint reads", "main", "s1", now.Add(-time.Hour))
	authored := forceSweepFact("keep this authored memory", "main", "s1", now.Add(-time.Hour))
	authored.Origin = factstore.OriginAuthored
	retired := forceSweepFact("retired statement stays a tombstone", "main", "s1", now.Add(-time.Hour))
	retired.Status = factStatusSuperseded
	orphan := forceSweepFact("derived from a session the catalog lost", "main", "ghost", now.Add(-time.Hour))
	if err := factstore.Write(brainDir, []factRecord{stale, authored, retired, orphan}); err != nil {
		t.Fatal(err)
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: forceSweepRun, timeout: time.Minute, force: true}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	byID := forceSweepIDs(t, brainDir)
	if _, ok := byID[stale.ID]; ok {
		t.Fatalf("stale fact must be dropped: %+v", byID)
	}
	for _, keep := range []factRecord{authored, retired, orphan} {
		if _, ok := byID[keep.ID]; !ok {
			t.Fatalf("%q must survive the force sweep: %+v", keep.Text, byID)
		}
	}
	rebuilt := factRecordID("Run tests before committing.", normalizeFactPaths([]string{"workflow.testing.rules"}))
	if got, ok := byID[rebuilt]; !ok || got.Status != factStatusActive {
		t.Fatalf("force run must re-derive the sessions' fact: %+v", byID)
	}
}

// A branch-scoped force run sweeps stale facts only from the sessions the
// branch filter let it rebuild.
func TestForceWithBranchFilterSweepsOnlyThatBranchsSessions(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	keepMain := forceSweepFact("old derivation from session one", "main", "s1", now.Add(-time.Hour))
	staleFeature := forceSweepFact("old derivation from session two", "feature", "s2", now.Add(-time.Hour))
	if err := factstore.Write(brainDir, []factRecord{keepMain, staleFeature}); err != nil {
		t.Fatal(err)
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: forceSweepRun, timeout: time.Minute, force: true, branch: "feature"}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	byID := forceSweepIDs(t, brainDir)
	if _, ok := byID[staleFeature.ID]; ok {
		t.Fatalf("the filtered branch's stale fact must be swept: %+v", byID)
	}
	if _, ok := byID[keepMain.ID]; !ok {
		t.Fatalf("the other branch's fact must survive a scoped force run: %+v", byID)
	}
	rebuilt := factRecordID("Run tests before committing.", normalizeFactPaths([]string{"workflow.testing.rules"}))
	if got, ok := byID[rebuilt]; !ok || got.Branch != "feature" {
		t.Fatalf("the scoped session's fact must be re-derived on its branch: %+v", byID)
	}
}

// A ruling may target a cluster member that publishes in a LATER wave: that
// member sat in the agent's EXISTING list as comparison context, so the ruling
// is legitimate, but at apply time the target is not yet in the result set.
// The cross-link must land regardless of apply order (Bugbot on #587: staged
// merge targets lost their cross-links, hiding the pair from the compaction
// loop).
func TestStagedReconcileCrossWaveRulingStillCrossLinks(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	paths := []string{"preferences.coding.style"}
	stored := stagedTestFact("indent all source files with tabs", paths, factStatusActive, now.Add(-time.Hour))
	candidates := make([]factRecord, 0, maxReconcileExisting+1)
	for i := 0; i <= maxReconcileExisting; i++ {
		candidates = append(candidates, stagedTestFact(
			fmt.Sprintf("indent all source files with tabs variant %02d", i),
			paths, factStatusCandidate, now.Add(time.Duration(i)*time.Second)))
	}
	sectionLines := func(input, header string) []string {
		_, section, _ := strings.Cut(input, header+"\n")
		if i := strings.Index(section, "\n\n"); i >= 0 {
			section = section[:i]
		}
		var lines []string
		for _, line := range strings.Split(section, "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
	lineText := func(line string) string {
		_, text, _ := strings.Cut(line, "] ")
		return text
	}
	// The cluster's member order is not chronological and EXISTING is ranked
	// newest-first (activeFactsAtPaths), so neither the ruled candidate nor the
	// cross-wave target can be assumed by position. Locate the wave-2 member in
	// wave 1's EXISTING list explicitly: it is the one cluster member absent
	// from wave 1's CANDIDATES, present as comparison context.
	var ruledText, crossWaveTargetText string
	var wave2RuledText, wave2TargetText string
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		cands := sectionLines(string(input), "CANDIDATES")
		existing := sectionLines(string(input), "EXISTING")
		if len(cands) == maxReconcileExisting {
			// Wave 1: rule the wave's first candidate against the member a
			// later wave publishes.
			inWave := map[string]bool{}
			for _, line := range cands {
				inWave[lineText(line)] = true
			}
			targetNumber := 0
			for i, line := range existing {
				text := lineText(line)
				if text != stored.Text && strings.HasPrefix(text, "indent all source files with tabs variant") && !inWave[text] {
					targetNumber = i + 1
					crossWaveTargetText = text
					break
				}
			}
			if targetNumber == 0 {
				return "", fmt.Errorf("wave-2 member not found in wave 1's EXISTING list:\n%s", input)
			}
			ruledText = lineText(cands[0])
			var b strings.Builder
			fmt.Fprintf(&b, "1 merge %d 0.95\n", targetNumber)
			for i := 2; i <= maxReconcileExisting; i++ {
				fmt.Fprintf(&b, "%d new - 1.0\n", i)
			}
			return b.String(), nil
		}
		// Wave 2: rule the lone remaining member against EXISTING #1 (the
		// prompt caps EXISTING at the 50 most recently updated, so which fact
		// that is is the prompt's choice, not this test's).
		wave2RuledText = lineText(cands[0])
		wave2TargetText = lineText(existing[0])
		return "1 merge 1 0.95", nil
	}

	updated, stats, _ := reconcileStagedCandidates(context.Background(),
		run, nil, append([]factRecord{stored}, candidates...), ".", time.Minute, 2, now)
	if stats.Candidates != maxReconcileExisting+1 {
		t.Fatalf("expected one cluster over every candidate, stats: %+v", stats)
	}
	if ruledText == "" || crossWaveTargetText == "" {
		t.Fatal("wave 1 never observed; the cluster did not split into two waves")
	}
	byText := map[string]factRecord{}
	for _, f := range updated {
		byText[f.Text] = f
	}
	ruled, target := byText[ruledText], byText[crossWaveTargetText]
	if ruled.Status != factStatusActive || target.Status != factStatusActive {
		t.Fatalf("both sides of the ruling must publish: ruled=%q target=%q", ruled.Status, target.Status)
	}
	if !contains(ruled.RelatedIDs, target.ID) || !contains(target.RelatedIDs, ruled.ID) {
		t.Fatalf("cross-wave ruling must cross-link once both sides land:\nruled=%+v\ntarget=%+v",
			ruled.RelatedIDs, target.RelatedIDs)
	}
	w2Ruled, w2Target := byText[wave2RuledText], byText[wave2TargetText]
	if !contains(w2Ruled.RelatedIDs, w2Target.ID) || !contains(w2Target.RelatedIDs, w2Ruled.ID) {
		t.Fatalf("wave-2 ruling must cross-link too:\nruled=%+v\ntarget=%+v",
			w2Ruled.RelatedIDs, w2Target.RelatedIDs)
	}
}
