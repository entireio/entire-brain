package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// deepCorpus builds a corpus with a promotable deploy:release task whose evidence
// includes corrected episodes with a failed command, so the deep dossier has
// real failure modes. Returns the db and the deploy pattern id.
func deepCorpus(t *testing.T, now time.Time) (*sql.DB, string) {
	t.Helper()
	db := freshCorpusDB(t)
	mkFact := func(id string) {
		if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
			id, "fact:deploy", "architecture", "ops/deploy.go"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:ok%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "success", 2, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
		mkFact(id)
	}
	// Two corrected episodes whose go-test command failed (the failure mode).
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("episode:fix%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "corrected", 3, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
		if _, err := db.Exec(`INSERT INTO episode_commands (episode_id, ord, head, raw_redacted, line, exit_code, failed) VALUES (?,?,?,?,?,?,1)`,
			id, 9, "go test", "go test ./... # token ghp_SECRETTOKEN0123456789abcdef", 9, 1); err != nil {
			t.Fatal(err)
		}
		mkFact(id)
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("episode:noise%d", i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(`SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return db, pid
}

func TestDeepDossierFullSpanAndFailureModes(t *testing.T) {
	db, pid := deepCorpus(t, time.Now())
	deep, err := buildDeepDossier(db, pid)
	if err != nil {
		t.Fatal(err)
	}
	// Full evidence set, not 3 sampled anchors.
	if deep.EvidenceEpisodes != 14 {
		t.Errorf("expected 14 evidence episodes, got %d", deep.EvidenceEpisodes)
	}
	if len(deep.SourceAnchors) < 14 {
		t.Errorf("deep anchors should cover the full set (capped), got %d", len(deep.SourceAnchors))
	}
	// Corrected first.
	if deep.SourceAnchors[0].Outcome != "corrected" {
		t.Errorf("expected corrected episodes stratified first, got %q", deep.SourceAnchors[0].Outcome)
	}
	if len(deep.FailureModes) != 2 {
		t.Errorf("expected 2 failure modes from the corrected episodes, got %d: %+v", len(deep.FailureModes), deep.FailureModes)
	}
	if len(deep.Facts) == 0 {
		t.Error("deep dossier should carry corroborating facts")
	}
	if deep.Fingerprint == "" {
		t.Error("deep dossier needs a fingerprint")
	}
}

func TestDeepVerifyCachesAndInvalidates(t *testing.T) {
	now := time.Now()
	db, _ := deepCorpus(t, now)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		// The full deep export reaches the agent, and it is redacted.
		if !strings.Contains(string(input), "deploy:release") {
			t.Errorf("deep verifier input missing the dossier: %s", string(input))
		}
		if strings.Contains(string(input), "ghp_SECRETTOKEN0123456789abcdef") {
			t.Errorf("deep verifier input leaked a secret: %s", string(input))
		}
		return `{"schema_version":1,"verdict":"accepted","reason":"sound"}`, nil
	}
	stats, err := verifyDeepDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified == 0 || stats.Verified != stats.Considered {
		t.Fatalf("expected all considered dossiers verified, got %+v", stats)
	}
	firstCalls := calls
	var verdict string
	db.QueryRow(`SELECT verdict FROM deep_dossiers LIMIT 1`).Scan(&verdict)
	if verdict != "accepted" {
		t.Errorf("verdict = %q, want accepted", verdict)
	}

	// Second run: unchanged evidence → all cached, agent not called again.
	stats2, _ := verifyDeepDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if stats2.Cached != stats2.Considered || calls != firstCalls {
		t.Errorf("expected full cache reuse (cached=%d considered=%d, no new calls), got %+v calls=%d (was %d)",
			stats2.Cached, stats2.Considered, stats2, calls, firstCalls)
	}

	// Materially change the evidence → fingerprint moves → re-verify.
	id := "episode:fix-new"
	insertCorpusEpisode(t, db, id, "deploy:release", "corrected", 2, now)
	insertCorpusShape(t, db, id, "mise build", "mise deploy")
	stats3, _ := verifyDeepDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if stats3.Verified == 0 || calls <= firstCalls {
		t.Errorf("changed evidence should re-verify, got %+v calls=%d (was %d)", stats3, calls, firstCalls)
	}
}

func TestDeepVerifyNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	db, _ := deepCorpus(t, time.Now())
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := verifyDeepDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress rejection, got %v", err)
	}
	if called {
		t.Error("agent must not run under no-egress")
	}
}
