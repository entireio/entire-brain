package cli

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func findDeployCand(t *testing.T, cands []taskCandidate) *taskCandidate {
	t.Helper()
	for i := range cands {
		if cands[i].IntentSignature == "deploy:release" {
			return &cands[i]
		}
	}
	t.Fatal("deploy:release candidate not found")
	return nil
}

func TestCorpusTaskCandidatesAdapter(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	cands, ok := loadCorpusTaskCandidates(brainDir)
	if !ok {
		t.Fatal("expected corpus-backed task candidates")
	}
	c := findDeployCand(t, cands)
	if c.ID == "" || c.ID[:8] != "pattern:" {
		t.Errorf("candidate id should be the corpus pattern id, got %q", c.ID)
	}
	if len(c.Commands) == 0 {
		t.Error("candidate should carry the workflow commands")
	}
	if len(c.MatchingFacts) == 0 {
		t.Error("candidate should carry dossier facts (fact:deploy)")
	}
}

func TestSkillFormUnderCorpusIDAndLifecycle(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	repoDir := t.TempDir()
	cands, _ := loadCorpusTaskCandidates(brainDir)
	cand := findDeployCand(t, cands)

	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "---\nname: deploy-release\ndescription: Deploy a release. Use when shipping.\n---\n\n# Workflow\n1. mise build\n2. mise deploy\n", nil
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	s := skillFormOptions{taskID: cand.ID, target: "claude-code", scope: "repo", yes: true, name: "deploy-release"}
	if err := synthesizeAndForm(context.Background(), cmd, *cand, nil, brainDir, repoDir, "codex", run, s, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Recorded under the corpus pattern id.
	mem := skillMemoryByPatternID(mustLoadSkillMemory(brainDir))
	rec, ok := mem[cand.ID]
	if !ok {
		t.Fatalf("skill-memory not recorded under corpus id %s", cand.ID)
	}
	if rec.Status != skillStatusActive || len(rec.Installs) == 0 {
		t.Errorf("unexpected record: %+v", rec)
	}

	// Lifecycle agrees with the V2 listing: the formed skill is current (suppressed).
	views, _, _ := loadCorpusPatternViews(brainDir)
	var view *patternView
	for i := range views {
		if views[i].ID == cand.ID {
			view = &views[i]
		}
	}
	if view == nil {
		t.Fatal("formed pattern missing from corpus listing")
	}
	if !evaluateSkillMemory(rec, view).suppressInListing() {
		t.Error("a freshly-formed, unchanged skill should be suppressed (current) in the listing")
	}

	// Dedupe: re-forming without --force refuses to overwrite the existing file.
	if err := synthesizeAndForm(context.Background(), cmd, *cand, nil, brainDir, repoDir, "codex", run, s, time.Now()); err == nil {
		t.Error("expected refusal to overwrite existing skill file without --force")
	}

	// Hand-edit detection: mutate the installed file → edited.
	editPath := expandHomePath(rec.Installs[0].Path)
	if err := os.WriteFile(editPath, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sub := evaluateSkillMemory(rec, view).Sub; sub != skillSubEdited {
		t.Errorf("hand-edited skill should report edited, got %q", sub)
	}
}

// SR2: `patterns skills` proposals require an ACCEPTED deep dossier. A promotable
// task with no deep dossier (or a rejected/needs_split/low_confidence one) is NOT
// a proposal; only an accepted deep dossier makes it formable.
func TestSkillProposalsRequireAcceptedDeepDossier(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pid string
	if err := db.QueryRow(`SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	isProposal := func() bool {
		cands, ok := loadSkillProposals(brainDir)
		if !ok {
			t.Fatal("expected corpus-backed proposals query")
		}
		for _, c := range cands {
			if c.ID == pid {
				return true
			}
		}
		return false
	}
	setDeep := func(v string) {
		db.Exec(`DELETE FROM deep_dossiers WHERE pattern_id=?`, pid)
		if v != "" {
			if _, err := db.Exec(`INSERT INTO deep_dossiers (pattern_id, fingerprint, json_redacted, verdict, status, created_at, updated_at) VALUES (?,?,?,?, 'current','t','t')`,
				pid, "sha256:x", "{}", v); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Promotable but no deep dossier → NOT a proposal (must verify --deep first).
	if isProposal() {
		t.Error("a promotable task with no accepted deep dossier must not be a skill proposal")
	}
	// But it IS still resolvable by the form (for the 'needs deep verification' message).
	if c, _ := corpusTaskCandidateByID(brainDir, pid); c == nil {
		t.Error("form resolver must still find the promotable task")
	}

	// Unresolved/negative deep verdicts → not a proposal.
	for _, v := range []string{"rejected", "needs_split", "low_confidence"} {
		setDeep(v)
		if isProposal() {
			t.Errorf("deep verdict %q must not be a skill proposal", v)
		}
	}
	// Accepted deep dossier → proposal.
	setDeep("accepted")
	if !isProposal() {
		t.Error("an accepted deep dossier must make the task a skill proposal")
	}
}

func TestSkillFilterDeclinedAndReconsider(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	cands, _ := loadCorpusTaskCandidates(brainDir)
	cand := findDeployCand(t, cands)

	// Declined + unchanged evidence → suppressed from the listing.
	matchingFP := patternEvidenceFingerprint(taskView(*cand))
	if err := recordSkillDecision(brainDir, skillMemoryRecord{
		PatternID: cand.ID, Status: skillStatusDeclined, Fingerprint: matchingFP,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	filtered := filterSkillCandidatesByMemory(brainDir, cands)
	for _, c := range filtered {
		if c.ID == cand.ID {
			t.Error("declined-unchanged candidate must be suppressed")
		}
	}

	// Declined but evidence materially changed → reconsider (stays visible).
	if err := recordSkillDecision(brainDir, skillMemoryRecord{
		PatternID: cand.ID, Status: skillStatusDeclined, Fingerprint: "sha256:stale-different",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	filtered = filterSkillCandidatesByMemory(brainDir, cands)
	found := false
	for _, c := range filtered {
		if c.ID == cand.ID {
			found = true
		}
	}
	if !found {
		t.Error("a declined candidate with changed evidence (reconsider) must stay visible")
	}
}
