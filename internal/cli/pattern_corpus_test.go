package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openCorpus(t *testing.T, brainDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// openCorpusReadSnapshot follows the production reader contract: each call
// pins one immutable published generation. Tests that rebuild must close and
// reopen it to observe the newly published generation.
func openCorpusReadSnapshot(t *testing.T, brainDir string) *patternCorpusReadDB {
	t.Helper()
	db, err := openPatternCorpusReadDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type corpusRowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func corpusCount(t *testing.T, db corpusRowQueryer, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

const codexCommitTranscript = `{"type":"event_msg","payload":{"type":"user_message","message":"run the tests and commit the change"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git commit -am wip\"}"}}
{"type":"event_msg","payload":{"type":"user_message","message":"now push it"}}`

func TestPatternCorpusIdempotentAndExtracts(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: codexCommitTranscript},
	})

	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpusReadSnapshot(t, brainDir)
	ep1 := corpusCount(t, db, "episodes")
	cmd1 := corpusCount(t, db, "episode_commands")
	gram1 := corpusCount(t, db, "grams")
	if ep1 == 0 || cmd1 < 2 || gram1 < 1 {
		t.Fatalf("expected episodes/commands/grams populated: ep=%d cmd=%d gram=%d", ep1, cmd1, gram1)
	}
	if n := corpusCount(t, db, "indexed_sessions"); n != 1 {
		t.Errorf("indexed_sessions=%d, want 1", n)
	}

	// Idempotent: a second build over unchanged input duplicates nothing.
	_ = db.Close()
	if err := buildPatternCorpus(brainDir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	db = openCorpusReadSnapshot(t, brainDir)
	if ep2 := corpusCount(t, db, "episodes"); ep2 != ep1 {
		t.Errorf("episodes after re-build = %d, want %d (no duplication)", ep2, ep1)
	}
	if cmd2 := corpusCount(t, db, "episode_commands"); cmd2 != cmd1 {
		t.Errorf("episode_commands after re-build = %d, want %d", cmd2, cmd1)
	}
	if gram2 := corpusCount(t, db, "grams"); gram2 != gram1 {
		t.Errorf("grams after re-build = %d, want %d", gram2, gram1)
	}

	// An indexer-version bump forces a clean re-index (no duplication). Apply
	// the fixture mutation through the same private-staging publisher used by
	// production; never write through the live SQLite pathname.
	_ = db.Close()
	if err := withBrainWriteLock(brainDir, func() error {
		return withPatternCorpusMutationLocked(brainDir, func(staged *sql.DB) error {
			_, err := staged.Exec(`UPDATE meta SET value='0' WHERE key='pattern_indexer_version'`)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(brainDir, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	db = openCorpusReadSnapshot(t, brainDir)
	if ep3 := corpusCount(t, db, "episodes"); ep3 != ep1 {
		t.Errorf("episodes after version-bump rebuild = %d, want %d", ep3, ep1)
	}
}

func TestPatternCorpusChangedSessionReplaces(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: codexCommitTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpusReadSnapshot(t, brainDir)
	before := corpusCount(t, db, "episodes")

	// Rewrite the transcript with a single user turn → one episode, no commands.
	tp := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	if err := os.WriteFile(tp, []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"just one request now"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := buildPatternCorpus(brainDir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	db = openCorpusReadSnapshot(t, brainDir)
	after := corpusCount(t, db, "episodes")
	if after != 1 {
		t.Fatalf("replacement transcript produced %d episodes, want 1", after)
	}
	if after >= before {
		t.Errorf("changed session should replace rows: before=%d after=%d", before, after)
	}
	// No orphaned child rows from the replaced episodes.
	if c := corpusCount(t, db, "episode_commands"); c != 0 {
		t.Errorf("expected commands replaced to 0 for the new single-turn transcript, got %d", c)
	}
}

func TestPatternCorpusPrunesDeletedSession(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: codexCommitTranscript},
		{id: "s2", branch: "main", agent: "Codex", checkpoint: "cp2", relPath: "sessions/main/s2.jsonl", author: "Bo", transcript: codexCommitTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpusReadSnapshot(t, brainDir)
	if n := corpusCount(t, db, "indexed_sessions"); n != 2 {
		t.Fatalf("indexed_sessions=%d, want 2", n)
	}

	// Drop s2 from the manifest and rebuild → its rows are pruned.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = manifest.Sources.Sessions.Sessions[:1]
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := buildPatternCorpus(brainDir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	db = openCorpusReadSnapshot(t, brainDir)
	if n := corpusCount(t, db, "indexed_sessions"); n != 1 {
		t.Errorf("indexed_sessions after prune = %d, want 1", n)
	}
	var leftover int
	if err := db.QueryRow(`SELECT COUNT(*) FROM episodes WHERE session_id='s2'`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Errorf("pruned session left %d orphaned episodes", leftover)
	}
}
