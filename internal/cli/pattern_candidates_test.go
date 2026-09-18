package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func freshCorpusDB(t *testing.T) *sql.DB {
	t.Helper()
	path, err := prepareBrainRelativeSQLiteFile(t.TempDir(), patternCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range patternCorpusSchema {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertCorpusEpisode(t *testing.T, db *sql.DB, id, intentSig, outcome string, nCmds int, now time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO episodes
		(id, episode_key, repo_key, session_id, turn_ord, source_path, start_line, end_line, intent_sig, n_cmds, outcome, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, id, "gh/acme/cli", id, 0, "sessions/main/x.jsonl", 1, 5, intentSig, nCmds, outcome, now.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}

// insertCorpusShape records a command shape for an episode: one gram joining the
// heads with gramSep (matching production) and the per-command episode_commands
// rows that feed loadCommandDF / gramSpecificity.
func insertCorpusShape(t *testing.T, db *sql.DB, episodeID string, heads ...string) {
	t.Helper()
	gram := strings.Join(heads, gramSep)
	if _, err := db.Exec(`INSERT INTO grams (episode_id, n, gram) VALUES (?,?,?)`, episodeID, len(heads), gram); err != nil {
		t.Fatal(err)
	}
	for i, h := range heads {
		if _, err := db.Exec(`INSERT INTO episode_commands (episode_id, ord, head, raw_redacted, line, failed) VALUES (?,?,?,?,?,0)`,
			episodeID, i, h, h, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

func taskStrengthFor(t *testing.T, db *sql.DB, intentSig string) (float64, bool) {
	t.Helper()
	var s float64
	err := db.QueryRow(`SELECT strength FROM patterns WHERE type='task' AND intent_sig=?`, intentSig).Scan(&s)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, true
}

func TestCandidatePromotionGate(t *testing.T) {
	db := freshCorpusDB(t)
	now := time.Now()

	// A specific, high-support, successful workflow — but with NO corroboration.
	// "mise deploy" is rare across the corpus, so its per-command idf is high.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:d%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "success", 2, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
	}
	// Many unrelated episodes running only generic commands so "ls"/"cat" are
	// common (low specificity) and the deploy shape stands out.
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("episode:u%d", i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}

	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	uncorr, ok := taskStrengthFor(t, db, "deploy:release")
	if !ok {
		t.Fatal("expected a deploy:release task candidate")
	}
	if uncorr >= promotionThreshold {
		t.Errorf("uncorroborated task must be capped below %.2f, got %.3f", promotionThreshold, uncorr)
	}

	// Add strong (locus-match, weight>=2) durable-fact corroboration to the same
	// episodes → now promotable. Weak (weight=1) text-overlap links do not count.
	for i := 0; i < 12; i++ {
		if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, weight) VALUES (?,?,2)`, fmt.Sprintf("episode:d%d", i), "fact:x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	corr, _ := taskStrengthFor(t, db, "deploy:release")
	if corr <= uncorr {
		t.Errorf("corroboration should lift strength: %.3f -> %.3f", uncorr, corr)
	}
	if corr < promotionThreshold {
		t.Errorf("corroborated high-support specific task should be promotable, got %.3f", corr)
	}
}

func TestRiskCandidatesFromFailures(t *testing.T) {
	db := freshCorpusDB(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("episode:r%d", i)
		insertCorpusEpisode(t, db, id, "migrate:schema", "corrected", 2, now)
		insertCorpusShape(t, db, id, "goose up", "goose status")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM patterns WHERE type='risk' AND intent_sig='migrate:schema'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("expected a risk candidate from repeated corrected episodes")
	}
	// Risk evidence cites the corrected episodes.
	var ev int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pattern_evidence pe JOIN patterns p ON p.id=pe.pattern_id WHERE p.type='risk'`).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	if ev == 0 {
		t.Error("expected risk pattern_evidence anchors")
	}
}

func TestCandidatesRebuildNotDuplicated(t *testing.T) {
	db := freshCorpusDB(t)
	now := time.Now()
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("episode:p%d", i)
		insertCorpusEpisode(t, db, id, "build:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "go build", "go test")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	first := corpusCount(t, db, "patterns")
	if first == 0 {
		t.Fatal("fixture produced no candidates, so rebuild idempotence would be vacuous")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	if again := corpusCount(t, db, "patterns"); again != first {
		t.Errorf("candidate rebuild duplicated patterns: %d -> %d", first, again)
	}
}
