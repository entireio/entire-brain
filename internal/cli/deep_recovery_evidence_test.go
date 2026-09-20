package cli

import (
	"testing"
	"time"
)

func TestFailedCommandDoesNotInventRecovery(t *testing.T) {
	db := freshCorpusDB(t)
	insertCorpusEpisode(t, db, "ep", "deploy", "corrected", 1, time.Now())
	if _, err := db.Exec(`INSERT INTO episode_commands(episode_id,ord,head,raw_redacted,line,exit_code,failed) VALUES('ep',0,'go test','go test ./...',2,1,1)`); err != nil {
		t.Fatal(err)
	}
	modes := deepFailureModes(db, []deepEpisode{{id: "ep", session: "s", outcome: "corrected"}})
	if len(modes) != 1 || modes[0].Recovery != "" {
		t.Fatalf("regression %+v", modes)
	}
	t.Logf("single failed test invocation => %+v", modes[0])
}

func TestRecoveryRequiresSubsequentKnownSuccessfulValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exits []any
		want  bool
	}{
		{"failed_only", []any{1}, false},
		{"success_before_failure", []any{0, 1}, false},
		{"unknown_after_failure", []any{1, nil}, false},
		{"failed_retry", []any{1, 1}, false},
		{"successful_retry", []any{1, 0}, true},
		{"later_failure", []any{1, 0, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := freshCorpusDB(t)
			insertCorpusEpisode(t, db, "ep", "deploy", "corrected", len(tc.exits), time.Now())
			for i, exit := range tc.exits {
				failed := exit == 1
				if _, err := db.Exec(`INSERT INTO episode_commands(episode_id,ord,head,raw_redacted,line,exit_code,failed) VALUES('ep',?,'go test','go test ./...',?,?,?)`, i, i+1, exit, failed); err != nil {
					t.Fatal(err)
				}
			}
			modes := deepFailureModes(db, []deepEpisode{{id: "ep", session: "s", outcome: "corrected"}})
			if len(modes) != 1 || (modes[0].Recovery != "") != tc.want {
				t.Fatalf("recovery=%+v wantKnown=%v", modes, tc.want)
			}
		})
	}
}
