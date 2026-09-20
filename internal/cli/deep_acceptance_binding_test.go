package cli

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStaleDeepDossierRejected(t *testing.T) {
	d := promotableCorpusDir(t, time.Now())
	db := openCorpus(t, d)
	var pid string
	if err := db.QueryRow(`SELECT id FROM patterns WHERE type='task' LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	current, err := buildDeepDossier(db, d, pid)
	if err != nil {
		t.Fatal(err)
	}
	seedAcceptedDeepDossier(t, db, pid, current, nil)
	if _, err := db.Exec(`UPDATE deep_dossiers SET status='stale'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, ok, err := loadAcceptedDeepDossierChecked(d, pid)
	if err != nil || ok {
		t.Fatalf("regression ok=%v err=%v", ok, err)
	}
	t.Log("stale dossier rejected")
}
func TestAcceptedDeepDossierRequiresCurrentBoundEvidence(t *testing.T) {
	for _, mutation := range []string{"payload", "verdict_binding", "source_command"} {
		t.Run(mutation, func(t *testing.T) {
			d := promotableCorpusDir(t, time.Now())
			db := openCorpus(t, d)
			defer db.Close()
			var pid string
			if err := db.QueryRow(`SELECT id FROM patterns WHERE type='task' LIMIT 1`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			rec, err := buildDeepDossier(db, d, pid)
			if err != nil {
				t.Fatal(err)
			}
			seedAcceptedDeepDossier(t, db, pid, rec, nil)
			if _, ok, err := loadAcceptedDeepDossierChecked(d, pid); err != nil || !ok {
				t.Fatalf("valid accepted evidence: ok=%v err=%v", ok, err)
			}
			switch mutation {
			case "payload":
				rec.Workflow = append(rec.Workflow, "unverified deployment step")
				b, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`UPDATE deep_dossiers SET json_redacted=? WHERE pattern_id=?`, string(b), pid)
				if err != nil {
					t.Fatal(err)
				}
			case "verdict_binding":
				_, err = db.Exec(`UPDATE deep_dossiers SET verifier_json_redacted='{"verdict":"accepted","evidence_fingerprint":"wrong"}' WHERE pattern_id=?`, pid)
				if err != nil {
					t.Fatal(err)
				}
			case "source_command":
				_, err = db.Exec(`UPDATE episode_commands SET raw_redacted=raw_redacted || ' --changed-evidence'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, err := loadAcceptedDeepDossierChecked(d, pid); err != nil || ok {
				t.Fatalf("changed evidence accepted: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestAcceptedKnowledgeRejectsChangedSourceSample(t *testing.T) {
	d := t.TempDir()
	_, convention := seedRoutingKnowledge(t, d)
	if _, ok, err := loadAcceptedDeepDossierChecked(d, convention); err != nil || !ok {
		t.Fatalf("valid convention: ok=%v err=%v", ok, err)
	}
	seedCapabilityFactsFile(t, d, []factRecord{{ID: "fact:pinned", Kind: "convention", Branch: "main", Status: "active", Text: "changed source requirement", Locus: []string{"mcp.go"}}})
	if _, ok, err := loadAcceptedDeepDossierChecked(d, convention); err != nil || ok {
		t.Fatalf("stale convention: ok=%v err=%v", ok, err)
	}
}
