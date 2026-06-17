package cli

import (
	"strings"
	"testing"
	"time"
)

func TestCorpusCommitsExtraction(t *testing.T) {
	work := "running git commit\n[main a1b2c3d] ship the release\n 2 files changed\nnoise [not-a-commit]\n[feature/x 0badc0ffee01] another change with ghp_SECRETTOKEN0123456789abcdef token"
	commits := corpusCommits(work)
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d: %+v", len(commits), commits)
	}
	if commits[0].hash != "a1b2c3d" || commits[0].branch != "main" {
		t.Errorf("commit 0 = %+v, want main/a1b2c3d", commits[0])
	}
	if commits[1].branch != "feature/x" {
		t.Errorf("commit 1 branch = %q, want feature/x", commits[1].branch)
	}
	// The subject is redacted.
	if strings.Contains(commits[1].subject, "ghp_SECRETTOKEN0123456789abcdef") {
		t.Errorf("commit subject leaked a secret: %q", commits[1].subject)
	}
}

// A 3-episode session sharing one intent + shape, each landing a commit, so a
// task pattern forms with evidence (driving synapses) and commits are recorded.
const commitFixtureTranscript = `{"type":"event_msg","payload":{"type":"user_message","message":"deploy the release"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"a1","name":"exec_command","arguments":"{\"cmd\":\"mise build\"}"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"a2","name":"exec_command","arguments":"{\"cmd\":\"mise deploy\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"a2","output":"[main aaa1111] ship one"}}
{"type":"event_msg","payload":{"type":"user_message","message":"deploy the release"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"b1","name":"exec_command","arguments":"{\"cmd\":\"mise build\"}"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"b2","name":"exec_command","arguments":"{\"cmd\":\"mise deploy\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"b2","output":"[main bbb2222] ship two"}}
{"type":"event_msg","payload":{"type":"user_message","message":"deploy the release"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"mise build\"}"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"exec_command","arguments":"{\"cmd\":\"mise deploy\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"[main ccc3333] ship three"}}
{"type":"event_msg","payload":{"type":"user_message","message":"thanks"}}`

func TestCorpusNewTablesAndSynapses(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: commitFixtureTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, brainDir)

	// New tables exist and are queryable.
	for _, table := range []string{"episode_symbols", "episode_commits", "themes", "synapses"} {
		if _, err := db.Exec("SELECT COUNT(*) FROM " + table); err != nil {
			t.Errorf("table %s missing/unqueryable: %v", table, err)
		}
	}
	// Commits recorded from the local confirmations.
	if n := corpusCount(t, db, "episode_commits"); n != 3 {
		t.Errorf("expected 3 commits, got %d", n)
	}
	var hashes int
	db.QueryRow(`SELECT COUNT(*) FROM episode_commits WHERE commit_hash IN ('aaa1111','bbb2222','ccc3333')`).Scan(&hashes)
	if hashes != 3 {
		t.Errorf("expected the 3 specific commit hashes, got %d", hashes)
	}
	// A task pattern formed → synapses connect pattern→evidence and evidence→commit.
	if n := corpusCount(t, db, "synapses"); n == 0 {
		t.Fatal("expected synapses to be materialized")
	}
	var pe, ec int
	db.QueryRow(`SELECT COUNT(*) FROM synapses WHERE kind='pattern_evidence'`).Scan(&pe)
	db.QueryRow(`SELECT COUNT(*) FROM synapses WHERE kind='episode_commit'`).Scan(&ec)
	if pe == 0 {
		t.Error("expected pattern_evidence synapses")
	}
	if ec == 0 {
		t.Error("expected episode_commit synapses for evidence episodes")
	}
}

// DV6: episode_symbols must be cleared when the semantic source is absent, so a
// brain that had a semantic index and later lost it does not keep stale links.
func TestEpisodeSymbolsClearedWhenSemanticAbsent(t *testing.T) {
	db := freshCorpusDB(t)
	now := time.Now()
	insertCorpusEpisode(t, db, "episode:s", "deploy:release", "success", 1, now)
	// Simulate links left by a prior build that had a semantic index.
	if _, err := db.Exec(`INSERT INTO episode_symbols (episode_id, symbol_id, name, file_path) VALUES ('episode:s','sym:1','Foo','a.go')`); err != nil {
		t.Fatal(err)
	}
	if n := corpusCount(t, db, "episode_symbols"); n != 1 {
		t.Fatalf("setup: expected 1 stale symbol link, got %d", n)
	}
	// Rebuild with no semantic source → table must degrade to empty.
	if err := linkEpisodeSymbols(db, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if n := corpusCount(t, db, "episode_symbols"); n != 0 {
		t.Errorf("stale symbol links must be cleared when semantic is absent, got %d", n)
	}
}

func TestPatternRunRecorded(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: commitFixtureTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	run, ok := lastPatternRun(brainDir)
	if !ok {
		t.Fatal("expected a recorded run after build")
	}
	if run.Episodes == 0 || run.Commits != 3 || run.IndexerVersion != patternIndexerVersion {
		t.Errorf("run snapshot wrong: %+v", run)
	}

	// Idempotent rebuild: a second build does not duplicate synapses/commits and
	// appends exactly one more run record.
	db := openCorpus(t, brainDir)
	syn1 := corpusCount(t, db, "synapses")
	com1 := corpusCount(t, db, "episode_commits")
	db.Close()
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db2 := openCorpus(t, brainDir)
	if syn2 := corpusCount(t, db2, "synapses"); syn2 != syn1 {
		t.Errorf("synapses duplicated on rebuild: %d -> %d", syn1, syn2)
	}
	if com2 := corpusCount(t, db2, "episode_commits"); com2 != com1 {
		t.Errorf("commits duplicated on rebuild: %d -> %d", com1, com2)
	}
	if runs := loadPatternRuns(brainDir); len(runs) != 2 {
		t.Errorf("expected 2 run records after 2 builds, got %d", len(runs))
	}
}
