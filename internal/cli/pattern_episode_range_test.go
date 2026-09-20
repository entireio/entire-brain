package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFinalEpisodeIncludesAssistantEvidence(t *testing.T) {
	now := time.Now()
	txt := `{"type":"user","message":{"content":"deploy release"}}` + "\n" + `{"type":"assistant","message":{"content":"First validate staging."}}` + "\n" + `{"type":"assistant","message":{"content":"Then deploy."}}` + "\n"
	d := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", relPath: "sessions/main/s1.jsonl", transcript: txt}})
	if err := buildPatternCorpus(d, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, d)
	var start, end int
	if err := db.QueryRow(`SELECT start_line,end_line FROM episodes LIMIT 1`).Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	if start != 1 || end != 3 {
		t.Fatalf("regression %d %d", start, end)
	}
	t.Logf("3-line final exchange is anchored as %d-%d", start, end)
}

func TestFinalEpisodeEndAcrossTranscriptDialects(t *testing.T) {
	for name, text := range map[string]string{
		"codex": `{"type":"event_msg","payload":{"type":"user_message","message":"deploy release"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"Validate staging"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"Deploy finished"}}`,
		"pi": `{"type":"message","message":{"role":"user","content":"deploy release"}}
{"type":"message","message":{"role":"assistant","content":"Validate staging"}}
{"type":"message","message":{"role":"assistant","content":"Deploy finished"}}`,
		"opencode": `{
 "info":{"id":"session"},
 "messages":[
 {"info":{"role":"user"},"parts":[{"type":"text","text":"deploy release"}]},
 {"info":{"role":"assistant"},"parts":[{"type":"text","text":"Validate staging"}]},
 {"info":{"role":"assistant"},"parts":[{"type":"text","text":"Deploy finished"}]}
 ]
}`,
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			d := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", relPath: "sessions/main/s1.jsonl", transcript: text}})
			if err := buildPatternCorpus(d, now); err != nil {
				t.Fatal(err)
			}
			db := openCorpus(t, d)
			defer db.Close()
			var start, end int
			if err := db.QueryRow(`SELECT start_line,end_line FROM episodes`).Scan(&start, &end); err != nil {
				t.Fatal(err)
			}
			wantStart, wantEnd := 1, 3
			if name == "opencode" {
				wantStart, wantEnd = 4, 8
			}
			if start != wantStart || end != wantEnd {
				t.Fatalf("source range=%d-%d, want %d-%d", start, end, wantStart, wantEnd)
			}
		})
	}
}

func TestNonFinalEpisodeSharingPhysicalLineKeepsBoundedEvidence(t *testing.T) {
	text := `{
 "messages":[
 {"info":{"role":"user"},"parts":[{"type":"text","text":"review release readiness"}]},{"info":{"role":"assistant"},"parts":[{"type":"text","text":"first response"}]},{"info":{"role":"user"},"parts":[{"type":"text","text":"deploy release"}]},
 {"info":{"role":"assistant"},"parts":[{"type":"text","text":"second response"}]}
 ]
}`
	now := time.Now()
	d := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s", branch: "main", relPath: "sessions/main/s.json", transcript: text}})
	if err := buildPatternCorpus(d, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, d)
	defer db.Close()
	rows, err := db.Query(`SELECT start_line,end_line FROM episodes ORDER BY turn_ord`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var spans [][2]int
	for rows.Next() {
		var span [2]int
		if err := rows.Scan(&span[0], &span[1]); err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[0] != [2]int{3, 3} || spans[1] != [2]int{3, 6} {
		t.Fatalf("shared-line source spans: %v, want [3,3] [3,6]", spans)
	}
}

func TestPatternsRefreshMigratesVersionEightEvidence(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	transcript := `{
 "messages":[
 {"info":{"role":"user"},"parts":[{"type":"text","text":"review release readiness"}]},{"info":{"role":"assistant"},"parts":[{"type":"text","text":"first response"}]},{"info":{"role":"user"},"parts":[{"type":"text","text":"Please implement caching properly, this still doesn't work"}]},
 {"info":{"role":"assistant"},"parts":[{"type":"text","text":"second response"}]}
 ]
}`
	path := filepath.Join(brainDir, "sessions", "main", "session-1.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	refresh := func() {
		t.Helper()
		out, stderr, err := executeSplit(t, NewRootCommand(opts), "patterns", "refresh", repoDir)
		if err != nil {
			t.Fatalf("public refresh failed: %v\n%s\n%s", err, out, stderr)
		}
	}
	refresh()
	db := openCorpus(t, brainDir)
	// Model the durable version8 cache for the same source: prior task blanking
	// mislabeled this turn and the nonfinal shared-line range consumed later work.
	for _, stmt := range []string{`UPDATE indexed_sessions SET parser_version=8`, `UPDATE meta SET value='8' WHERE key='pattern_indexer_version'`, `UPDATE episodes SET outcome='success',end_line=6 WHERE session_id='session-1' AND turn_ord=0`} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	refresh()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != transcript || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("migration changed source transcript")
	}
	db = openCorpus(t, brainDir)
	defer db.Close()
	var outcome string
	var start, end, version int
	if err := db.QueryRow(`SELECT outcome,start_line,end_line FROM episodes WHERE session_id='session-1' AND turn_ord=0`).Scan(&outcome, &start, &end); err != nil {
		t.Fatal(err)
	}
	if outcome != reinforcementCorrected || start != 3 || end != 3 {
		t.Fatalf("version8 evidence not rebuilt: outcome=%s span=%d-%d", outcome, start, end)
	}
	if err := db.QueryRow(`SELECT parser_version FROM indexed_sessions WHERE session_id='session-1'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != patternIndexerVersion || version <= 8 {
		t.Fatalf("version8 cache retained: %d", version)
	}
}
