package cli

import (
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
