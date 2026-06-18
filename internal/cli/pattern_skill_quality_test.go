package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// SR6: quality gates on what skill synthesis actually receives/produces — not
// just plumbing. The synthesis INPUT is the lever that makes a real agent produce
// a good (or generic) skill, so these assert on the input the agent is handed and
// on the NOT_A_SKILL / gating behaviors.

// (a) An accepted release/check workflow's skill input carries the exact command
// AND a verification step.
func TestSkillQualityReleaseInputHasCommandAndVerification(t *testing.T) {
	in := deepSkillInput{rec: deployDeepDossier()} // workflow: mise build/deploy, verification: go test
	ev := buildDeepSkillEvidence(in)
	if !strings.Contains(ev, "mise deploy") {
		t.Errorf("release skill input must include the exact command:\n%s", ev)
	}
	if !strings.Contains(strings.ToLower(ev), "verification") || !strings.Contains(ev, "go test") {
		t.Errorf("release skill input must include a verification step:\n%s", ev)
	}
}

// (b) A corrected episode becomes a failure mode / gotcha in the dossier-built
// skill input.
func TestSkillQualityCorrectedEpisodeBecomesFailureMode(t *testing.T) {
	db, pid := deepCorpus(t, time.Now()) // 2 corrected episodes with a failed go test
	deep, err := buildDeepDossier(db, t.TempDir(), pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(deep.FailureModes) == 0 {
		t.Fatal("corrected episodes must produce failure modes in the deep dossier")
	}
	ev := buildDeepSkillEvidence(deepSkillInput{rec: deep})
	if !strings.Contains(ev, "Failure modes") || !strings.Contains(ev, "failed:") {
		t.Errorf("skill input must carry the failure mode/gotcha from corrected episodes:\n%s", ev)
	}
}

// (c) A generic git workflow returns NOT_A_SKILL.
func TestSkillQualityGenericGitNotASkill(t *testing.T) {
	in := deepSkillInput{rec: deepDossierRecord{
		Title: "commit:push — git add ▷ git push", Trigger: "recurring intent: commit:push",
		Workflow: []string{"git add", "git push"},
	}}
	run := stubRunner("NOT_A_SKILL: generic git add/push, nothing repo-specific")
	res, err := synthesizeSkillFromDossier(context.Background(), t.TempDir(), in, "codex", "", "", run)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsSkill {
		t.Error("a generic git dossier must return NOT_A_SKILL")
	}
}

// (d) A shallow-only candidate (promotable, no accepted deep dossier) is not
// formable.
func TestSkillQualityShallowOnlyNotFormable(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, _ := openPatternCorpusDB(brainDir)
	var pid string
	db.QueryRow(`SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release'`).Scan(&pid)
	db.Close()
	// It is promotable (resolvable) but has no accepted deep dossier → not formable.
	if _, ok := loadAcceptedDeepDossier(brainDir, pid); ok {
		t.Fatal("setup: expected no accepted deep dossier")
	}
	if props, _ := loadSkillProposals(brainDir); len(props) != 0 {
		t.Errorf("a shallow-only candidate must not be a formable proposal, got %d", len(props))
	}
}

// (e) A theme skill cites the verified latent practice (its description), not an
// intent_sig.
func TestSkillQualityThemeCitesPracticeNotIntentSig(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, _ := openPatternCorpusDB(brainDir)
	// An accepted theme with an agent description (no intent_sig label).
	db.Exec(`INSERT INTO themes (id, scope, title, description, shape, member_keys, support, fingerprint, strength, status, verdict, created_at, updated_at)
		VALUES ('theme:rev','repo','reviewing the current branch','recurring practice of auditing branch changes for regressions','conversation','[]',5,'sha256:t',0.7,'active','accepted','t','t')`)
	db.Close()
	in, ok := loadAcceptedThemeAsDeep(brainDir, "theme:rev")
	if !ok {
		t.Fatal("expected the accepted theme to load")
	}
	ev := buildDeepSkillEvidence(in)
	if !strings.Contains(ev, "auditing branch changes for regressions") {
		t.Errorf("theme skill input must cite the verified latent practice description:\n%s", ev)
	}
	if strings.Contains(ev, "review:current") || strings.Contains(ev, "intent_sig") {
		t.Errorf("theme skill input must NOT cite an intent_sig label:\n%s", ev)
	}
}

// (f) A workspace skill input includes per-repo variation when the evidence
// differs across repos.
func TestSkillQualityWorkspaceIncludesRepoVariation(t *testing.T) {
	fam := workspaceFamily{
		Title: "release the build", Trigger: "shipping a release", Verdict: "accepted",
		CommonWorkflow: []string{"build", "publish"},
		PerRepo: []familyRepoVariant{
			{RepoKey: "gh/acme/a", Commands: []string{"mise build", "mise deploy"}},
			{RepoKey: "gh/acme/b", Commands: []string{"make build", "make release"}},
		},
	}
	ev := buildWorkspaceFamilyEvidence(fam)
	if !strings.Contains(ev, "Per-repo variations") {
		t.Fatalf("workspace skill input must include a per-repo variations section:\n%s", ev)
	}
	if !strings.Contains(ev, "gh/acme/a") || !strings.Contains(ev, "mise deploy") ||
		!strings.Contains(ev, "gh/acme/b") || !strings.Contains(ev, "make release") {
		t.Errorf("workspace skill input must include each repo's differing commands:\n%s", ev)
	}
}
