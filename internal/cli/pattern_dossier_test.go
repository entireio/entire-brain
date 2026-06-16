package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// promotableCorpus builds a corpus with one promotable, corroborated task
// (deploy:release via "mise build ▷ mise deploy", weight>=2 fact-linked) plus
// generic noise so the deploy shape is specific. Returns the db.
func promotableCorpus(t *testing.T, now time.Time) *sql.DB {
	t.Helper()
	db := freshCorpusDB(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:d%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "success", 2, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
		if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
			id, "fact:deploy", "architecture", "ops/deploy.go"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("episode:u%d", i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}
	return db
}

func TestDossierBuiltForPromotablePattern(t *testing.T) {
	now := time.Now()
	db := promotableCorpus(t, now)
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}

	var (
		clusterKey, jsonBlob, status, verdict string
		fingerprint                           string
	)
	err := db.QueryRow(`SELECT d.cluster_key, d.json_redacted, d.status, COALESCE(d.verdict,''), d.fingerprint
		FROM dossiers d JOIN patterns p ON p.id=d.pattern_id
		WHERE p.type='task' AND p.intent_sig='deploy:release'`).Scan(&clusterKey, &jsonBlob, &status, &verdict, &fingerprint)
	if err == sql.ErrNoRows {
		t.Fatal("expected a dossier for the promotable deploy:release task")
	}
	if err != nil {
		t.Fatal(err)
	}
	if status != "current" {
		t.Errorf("fresh dossier status = %q, want current", status)
	}
	if verdict != "" {
		t.Errorf("deterministic refresh must not set a verdict, got %q", verdict)
	}
	if fingerprint == "" {
		t.Error("dossier must carry an evidence fingerprint")
	}
	for _, want := range []string{`"mise build"`, `"mise deploy"`, "deploy:release", "fact:deploy"} {
		if !strings.Contains(jsonBlob, want) {
			t.Errorf("dossier JSON missing %q:\n%s", want, jsonBlob)
		}
	}
}

func TestDossierOnlyForPromotable(t *testing.T) {
	now := time.Now()
	db := freshCorpusDB(t)
	// A generic, uncorroborated task: many episodes, generic commands → not promotable.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:g%d", i)
		insertCorpusEpisode(t, db, id, "commit:push", "neutral", 2, now)
		insertCorpusShape(t, db, id, "git push", "git status")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var dossiers int
	db.QueryRow(`SELECT COUNT(*) FROM dossiers`).Scan(&dossiers)
	if dossiers != 0 {
		t.Errorf("no promotable pattern, want 0 dossiers, got %d", dossiers)
	}
	// The pattern itself still exists (visible, just capped below threshold).
	var patterns int
	db.QueryRow(`SELECT COUNT(*) FROM patterns`).Scan(&patterns)
	if patterns == 0 {
		t.Error("expected the (non-promotable) pattern to still be recorded")
	}
}

func TestDossierFingerprintStableAndRebuildIdempotent(t *testing.T) {
	now := time.Now()
	db := promotableCorpus(t, now)
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	fp1 := dossierFP(t, db)
	n1 := corpusCount(t, db, "dossiers")
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	if fp2 := dossierFP(t, db); fp2 != fp1 {
		t.Errorf("fingerprint not stable across rebuild: %q -> %q", fp1, fp2)
	}
	if n2 := corpusCount(t, db, "dossiers"); n2 != n1 {
		t.Errorf("dossier rebuild duplicated rows: %d -> %d", n1, n2)
	}
}

func TestDossierPrunedWhenPatternVanishes(t *testing.T) {
	now := time.Now()
	db := promotableCorpus(t, now)
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	if n := corpusCount(t, db, "dossiers"); n == 0 {
		t.Fatal("expected a dossier before pruning")
	}
	// Remove the deploy episodes' corroboration so the task drops below threshold.
	if _, err := db.Exec(`DELETE FROM episode_facts WHERE fact_id='fact:deploy'`); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	if n := corpusCount(t, db, "dossiers"); n != 0 {
		t.Errorf("dossier should be pruned when its pattern is no longer promotable, got %d", n)
	}
}

func TestDossierStaleWhenEvidenceMovesUnderCachedVerdict(t *testing.T) {
	now := time.Now()
	db := promotableCorpus(t, now)
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	pid, fp := onePromotableDossier(t, db)
	// Simulate a cached agent verdict for the current fingerprint.
	if _, err := db.Exec(`UPDATE dossiers SET verdict='accepted', verifier_json_redacted='{}', verified_fingerprint=?, status='current' WHERE pattern_id=?`, fp, pid); err != nil {
		t.Fatal(err)
	}
	// Materially change the evidence: add a fresh corrected episode to the cluster.
	id := "episode:dx"
	insertCorpusEpisode(t, db, id, "deploy:release", "corrected", 2, now)
	insertCorpusShape(t, db, id, "mise build", "mise deploy")
	if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
		id, "fact:deploy", "architecture", "ops/deploy.go"); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var status, verdict string
	db.QueryRow(`SELECT status, COALESCE(verdict,'') FROM dossiers WHERE pattern_id=?`, pid).Scan(&status, &verdict)
	if status != "stale" {
		t.Errorf("dossier status after evidence change = %q, want stale", status)
	}
	if verdict != "accepted" {
		t.Errorf("cached verdict should survive (for reference), got %q", verdict)
	}
}

func dossierFP(t *testing.T, db *sql.DB) string {
	t.Helper()
	var fp string
	db.QueryRow(`SELECT fingerprint FROM dossiers ORDER BY pattern_id LIMIT 1`).Scan(&fp)
	return fp
}

func onePromotableDossier(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	var pid, fp string
	if err := db.QueryRow(`SELECT pattern_id, fingerprint FROM dossiers ORDER BY pattern_id LIMIT 1`).Scan(&pid, &fp); err != nil {
		t.Fatal(err)
	}
	return pid, fp
}
