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
	updated, proposals, stats, warnings := reconcileStagedCandidates(context.Background(), run, nil, facts, nil, ".", time.Minute, 2, now)
	if calls != 0 {
		t.Fatalf("an unflagged candidate must not cost an agent call, got %d", calls)
	}
	if stats.Candidates != 1 || stats.Promoted != 1 || stats.Clusters != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if len(proposals) != 0 || len(warnings) != 0 {
		t.Fatalf("unexpected proposals %v or warnings %v", proposals, warnings)
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
	updated, proposals, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{first, second}, nil, ".", time.Minute, 2, now)
	if stats.Clusters != 1 || stats.AgentCalls != 1 || stats.Promoted != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if !strings.Contains(sawExisting, first.Text) {
		t.Fatalf("the elected head must be in EXISTING so the agent can merge into it:\n%s", sawExisting)
	}
	if len(proposals) != 1 {
		t.Fatalf("a confident merge must queue for review, got proposals %v", proposals)
	}
	var texts []string
	for _, f := range updated {
		if f.Status == factStatusActive {
			texts = append(texts, f.Text)
		}
	}
	if len(texts) != 1 || texts[0] != first.Text {
		t.Fatalf("only the elected head may publish, got %v", texts)
	}
}

// A candidate close to a stored active fact goes to the agent, and a confident
// supersede ruling must wait for explicit review.
func TestStagedReconcileQueuesSupersession(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	stored := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	candidate := stagedTestFact("indent all source files with spaces", []string{"preferences.coding.style"}, factStatusCandidate, now)
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "1 supersede 1 0.95", nil
	}
	updated, _, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, nil, ".", time.Minute, 2, now)
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
	if byID[candidate.ID].Status != factStatusCandidate {
		t.Fatalf("candidate must remain unpublished, got %q", byID[candidate.ID].Status)
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
	updated, _, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, nil, ".", time.Minute, 2, now)
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
// supersede ruling queues for review instead of applying.
func TestStagedReconcileNewestFirstSupersedeQueued(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	stored := stagedTestFact("indent all source files with tabs", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	candidate := stagedTestFact("indent all source files with spaces", []string{"preferences.coding.style"}, factStatusCandidate, now)
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "1 supersede 1 0.95", nil
	}
	updated, proposals, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{stored, candidate}, nil, ".", time.Minute, 2, now)
	if len(proposals) != 1 {
		t.Fatalf("newest-first supersede must queue for review, got %v", proposals)
	}
	active := 0
	for _, f := range updated {
		if f.Status == factStatusActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("only the original may stay active pending review, got %d", active)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "supersede requires review") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the review queueing must be disclosed: %v", warnings)
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
	updated, _, stats, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{leftover}, nil, ".", time.Minute, 2, now)
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
	first, _, _, _ := reconcileStagedCandidates(context.Background(), run, nil, append([]factRecord(nil), facts...), nil, ".", time.Minute, 4, now)
	second, _, _, _ := reconcileStagedCandidates(context.Background(), run, nil, append([]factRecord(nil), facts...), nil, ".", time.Minute, 4, now)
	sortFactRecords(first)
	sortFactRecords(second)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same input, different stores: concurrent clusters leaked completion order")
	}
}

// A high-confidence merge whose two texts share almost no vocabulary is a
// hallucinated pairing (measured: every such ruling on the calibration corpus
// was wrong). The guard queues it for review instead of destroying a fact.
func TestStagedReconcileGuardsLexicallyUnrelatedRulings(t *testing.T) {
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
			updated, proposals, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{similar, unrelated, candidate}, nil, ".", time.Minute, 2, now)
			if len(proposals) != 1 {
				t.Fatalf("an unrelated merge must queue for review, got proposals %v", proposals)
			}
			active := 0
			for _, f := range updated {
				if f.Status == factStatusActive {
					active++
				}
			}
			if active != 2 {
				t.Fatalf("nothing may be destroyed by a guarded ruling; want 2 originals active, got %d", active)
			}
			guarded := false
			for _, w := range warnings {
				if strings.Contains(w, "requires review") {
					guarded = true
				}
			}
			if !guarded {
				t.Fatalf("the guard must be disclosed: %v", warnings)
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
	if err := factstore.Write(brainDir, "main", []factRecord{target, consumed, foreign}); err != nil {
		t.Fatal(err)
	}
	// Baseline knew target+consumed (this run staged and flushed them); memory
	// holds only target: the pass merged the candidate away.
	baseline := []factRecord{target, consumed}
	inMemory := []factRecord{target}
	rebased, err := rebaseFactsOntoDisk(brainDir, "main", inMemory, baseline, false)
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
	if err := factstore.Write(brainDir, "main", []factRecord{target, leftover}); err != nil {
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
		stored, err := factstore.Load(brainDir, "main")
		if err != nil || len(stored) != 2 || stored[indexOfFact(stored, leftover.ID)].Status != factStatusCandidate {
			t.Errorf("resumed candidate must remain staged durably before extraction: %+v, %v", stored, err)
		}
		return "workflow.testing.rules\tRun tests before committing.\n", nil
	}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: run, timeout: time.Minute, flushEvery: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if source.PendingReconcile != 1 || source.CandidatesReconciled != 2 || source.ReconcileCalls != 1 {
		t.Fatalf("resume and new work must both be counted: %+v", source)
	}
	stored, err := factstore.Load(brainDir, "main")
	if err != nil || len(stored) != 3 {
		t.Fatalf("pending candidate was lost after final flush: %+v, %v", stored, err)
	}
}

func TestStagedReconcileForceResumePreservesDeferredBranch(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	stored := stagedTestFact("keep this existing memory", []string{"preferences.coding.style"}, factStatusActive, now.Add(-time.Hour))
	leftover := stagedTestFact("exports use batch checkpoint reads", []string{"architecture.data.flow"}, factStatusCandidate, now.Add(-time.Minute))
	stored.Branch, leftover.Branch = "feature", "feature"
	if err := factstore.Write(brainDir, "feature", []factRecord{stored, leftover}); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		return "workflow.testing.rules\tRun tests before committing.\n", nil
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: run, timeout: time.Minute, force: true, maxSessions: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factstore.Load(brainDir, "feature")
	if err != nil || len(facts) != 2 {
		t.Fatalf("resuming a deferred branch must not force-rebuild it: %+v, %v", facts, err)
	}
	for _, f := range facts {
		if f.Status != factStatusActive {
			t.Fatalf("leftover was not resumed: %+v", f)
		}
	}
}
