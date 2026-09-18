package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedAcceptedDeepDossier builds a deepDossierRecord with the given fields, marks
// it accepted (with required edits), and stores it in deep_dossiers for patternID.
func seedAcceptedDeepDossier(t *testing.T, db *sql.DB, patternID string, rec deepDossierRecord, requiredEdits []string) {
	t.Helper()
	rec.PatternID = patternID
	blob, _ := json.Marshal(rec)
	verdict := dossierVerdict{Verdict: "accepted", Reason: "coherent repo-specific task", RequiredEdits: requiredEdits}
	vblob, _ := json.Marshal(verdict)
	if _, err := db.Exec(`INSERT INTO deep_dossiers (pattern_id, fingerprint, json_redacted, verifier_json_redacted, verdict, status, created_at, updated_at)
		VALUES (?,?,?,?, 'accepted','current','t','t')`, patternID, rec.Fingerprint, string(blob), string(vblob)); err != nil {
		t.Fatal(err)
	}
}

func deployDeepDossier() deepDossierRecord {
	return deepDossierRecord{
		SchemaVersion: deepSchemaVersion,
		Title:         "deploy:release — mise build ▷ mise deploy",
		Trigger:       "recurring intent: deploy:release",
		Workflow:      []string{"mise build", "mise deploy"},
		Verification:  []string{"go test"},
		Parameters:    []string{"mise deploy --env staging"},
		FailureModes:  []deepFailureMode{{Episode: "s1", Failure: "failed: mise deploy (missing STAGING_TOKEN)", Recovery: "re-ran go test to verify"}},
		Facts:         []string{"fact:deploy (architecture) @ ops/deploy.go"},
		SourceAnchors: []dossierAnchor{{SessionID: "s1", Outcome: "corrected", Excerpt: "ZEBRA_GOTCHA: set the staging toggle before mise deploy"}},
		Confidence:    0.7,
	}
}

// SR1+SR3: skill synthesis input is built from the verified deep dossier and
// carries the dossier's failure modes, verification, parameters, evidence refs,
// and the gotcha that lives only in the transcript span.
func TestDeepSkillEvidenceFromDossier(t *testing.T) {
	in := deepSkillInput{rec: deployDeepDossier(), verdict: dossierVerdict{Verdict: "accepted", RequiredEdits: []string{"document the STAGING_TOKEN precondition"}}}
	ev := buildDeepSkillEvidence(in)
	for _, want := range []string{
		"mise build", "mise deploy", // exact workflow commands
		"go test",                    // verification
		"mise deploy --env staging",  // parameters
		"failed: mise deploy",        // failure mode
		"recovery:",                  // recovery
		"ZEBRA_GOTCHA",               // gotcha from the transcript span excerpt
		"document the STAGING_TOKEN", // verifier required edit
		"fact:deploy",                // evidence/fact ref
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("deep skill evidence missing %q:\n%s", want, ev)
		}
	}
}

// SR1: a promotable task with no accepted deep dossier cannot be formed from
// shallow evidence — the form must require deep verification first.
func TestAcceptedDeepDossierLoadsAndDrivesSynthesis(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(`SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// No accepted deep dossier → form refuses (and never calls the agent).
	if _, ok := loadAcceptedDeepDossier(brainDir, pid); ok {
		t.Fatal("setup: expected no accepted deep dossier yet")
	}

	// With an accepted deep dossier, the dossier-driven synthesis runs and the
	// agent receives the dossier (not shallow snippets).
	db2, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	seedAcceptedDeepDossier(t, db2, pid, deployDeepDossier(), nil)
	db2.Close()
	in, ok := loadAcceptedDeepDossier(brainDir, pid)
	if !ok {
		t.Fatal("expected the accepted deep dossier to load")
	}
	var gotInput string
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		gotInput = string(input)
		return "---\nname: deploy-release\ndescription: Deploy a release. Use when shipping a tagged build.\n---\n# Workflow\n1. mise build\n2. mise deploy\n## Verification\ngo test\n## Gotchas\nset staging toggle\n", nil
	}
	res, err := synthesizeSkillFromDossier(context.Background(), t.TempDir(), in, "codex", "", "", run)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsSkill {
		t.Errorf("expected a skill from the accepted dossier, got NOT_A_SKILL: %s", res.Reason)
	}
	if !strings.Contains(gotInput, "VERIFIED SKILL DOSSIER") || !strings.Contains(gotInput, "ZEBRA_GOTCHA") {
		t.Errorf("synthesis input was not the verified dossier:\n%s", gotInput)
	}
}

// SR3 golden: a generic git dossier yields NOT_A_SKILL (the writer still refuses
// non-skill-worthy material even though it was nominally accepted).
func TestDeepSkillSynthesisPreservesProviderNotASkill(t *testing.T) {
	for _, tc := range []struct {
		name             string
		workflow         []string
		response, reason string
	}{
		{"push-status", []string{"git push", "git status"}, "NOT_A_SKILL: generic git push/status, nothing repo-specific", "generic git push/status"},
		{"add-push", []string{"git add", "git push"}, "NOT_A_SKILL: generic git add/push, nothing repo-specific", "generic git add/push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := deepSkillInput{rec: deepDossierRecord{Title: "commit:push", Trigger: "recurring intent: commit:push", Workflow: tc.workflow}, verdict: dossierVerdict{Verdict: "accepted"}}
			res, err := synthesizeSkillFromDossier(context.Background(), t.TempDir(), in, "codex", "", "", stubRunner(tc.response))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsSkill || !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("provider refusal = %+v, want reason containing %q", res, tc.reason)
			}
		})
	}
}
