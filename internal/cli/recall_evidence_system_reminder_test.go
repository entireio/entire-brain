package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// Harness-injected <system-reminder> text inside a user turn is a system
// instruction, not something the user said, so it must not come back as
// role:"user" evidence (docs/recall-evidence.md). entireio/entire-brain#565.
func TestRecallEvidenceSkipsSystemReminderUserTurns(t *testing.T) {
	opts, brain := setupRecallIdentityTest(t)
	rel := "sessions/main/sysrem.jsonl"
	body := strings.Join([]string{
		// 1: string form.
		`{"type":"user","message":{"role":"user","content":"<system-reminder>zebrafoo string-form reminder</system-reminder>"}}`,
		// 2: text-block form.
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>zebrafoo block-form reminder</system-reminder>"}]}}`,
		// 3: a real request.
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"please fix the zebrafoo parser"}]}}`,
		// 4: assistant text is untouched.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed the zebrafoo parser."}]}}`,
		// 5: system-role records stay dropped.
		`{"type":"system","content":"zebrafoo system-role record"}`,
		// 6: a reminder block next to a real block in one user turn.
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>zebrafoo mixed reminder</system-reminder>"},{"type":"text","text":"also rename the zebrafoo flag"}]}}`,
		// 7: several reminders with surrounding whitespace in one field.
		`{"type":"user","message":{"role":"user","content":"  <system-reminder>zebrafoo one</system-reminder>\n\n<system-reminder>zebrafoo two</system-reminder>\n"}}`,
		// 8: an assistant quoting reminder markup keeps its text.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"<system-reminder>zebrafoo quoted by the assistant</system-reminder>"}]}}`,
		// 9: user text that merely starts with markup is still the user's.
		`{"type":"user","message":{"role":"user","content":"<div> zebrafoo renders twice"}}`,
	}, "\n") + "\n"
	if err := os.MkdirAll(filepath.Join(brain, "sessions", "main"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, filepath.FromSlash(rel)), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	m := exportManifest{SchemaVersion: bmanifest.SchemaVersion, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{DefaultBranch: "main", Sessions: []exportSession{{SessionID: "sysrem", Branch: "main", TranscriptPath: rel, CreatedAt: opts.Now()}}}}}
	if err := writeBrainManifestAndReadme(brain, m); err != nil {
		t.Fatal(err)
	}
	c, err := collectEvidence(context.Background(), brain, "main", "zebrafoo", 10, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "available" || len(c.Warnings) != 0 {
		t.Fatalf("state=%s warnings=%v", c.State, c.Warnings)
	}
	type key struct {
		line    int
		pointer string
		role    string
	}
	got := map[key]string{}
	for _, s := range c.Spans {
		if s.Role == "user" && strings.Contains(s.Text, "system-reminder") {
			t.Errorf("system reminder returned as user evidence: line=%d pointer=%s text=%q", s.Line, s.JSONPointer, s.Text)
		}
		if strings.Contains(s.Text, "system-role record") {
			t.Errorf("system-role record returned as evidence: %+v", s)
		}
		got[key{s.Line, s.JSONPointer, s.Role}] = s.Text
	}
	for k, want := range map[key]string{
		{3, "/message/content/0/text", "user"}:      "please fix the zebrafoo parser",
		{4, "/message/content/0/text", "assistant"}: "Fixed the zebrafoo parser.",
		{6, "/message/content/1/text", "user"}:      "also rename the zebrafoo flag",
		{8, "/message/content/0/text", "assistant"}: "<system-reminder>zebrafoo quoted by the assistant</system-reminder>",
		{9, "/message/content", "user"}:             "<div> zebrafoo renders twice",
	} {
		if got[k] != want {
			t.Errorf("missing span %+v: want %q, got %q", k, want, got[k])
		}
	}
	if len(c.Spans) != 5 {
		for _, s := range c.Spans {
			t.Logf("span line=%d pointer=%s role=%q text=%q", s.Line, s.JSONPointer, s.Role, s.Text)
		}
		t.Fatalf("want 5 spans, got %d", len(c.Spans))
	}
}

// The same rule holds for every user-role schema the evidence reader knows.
func TestRecallEvidenceSystemReminderAcrossSchemas(t *testing.T) {
	const reminder = "<system-reminder>injected</system-reminder>"
	for name, tc := range map[string]struct {
		line string
		keep string // the one field that must survive, "" for none
	}{
		"codex response_item user":   {`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + reminder + `"},{"type":"input_text","text":"real codex ask"}]}}`, "real codex ask"},
		"codex event_msg user":       {`{"type":"event_msg","payload":{"type":"user_message","message":"` + reminder + `"}}`, ""},
		"role/content jsonl":         {`{"role":"user","content":"` + reminder + `"}`, ""},
		"opencode parts":             {`{"info":{"role":"user"},"parts":[{"type":"text","text":"` + reminder + `"},{"type":"text","text":"real opencode ask"}]}`, "real opencode ask"},
		"message without role field": {`{"type":"user","message":{"content":"` + reminder + `"}}`, ""},
		"unclosed tag is kept":       {`{"type":"user","message":{"role":"user","content":"<system-reminder> is the tag I mean"}}`, "<system-reminder> is the tag I mean"},
		"trailing user text is kept": {`{"type":"user","message":{"role":"user","content":"` + reminder + ` but this part is mine"}}`, reminder + " but this part is mine"},
	} {
		t.Run(name, func(t *testing.T) {
			fields, _ := evidenceFields([]byte(tc.line+"\n"), "s.jsonl")
			var texts []string
			for _, f := range fields {
				texts = append(texts, f.text)
			}
			if tc.keep == "" {
				if len(fields) != 0 {
					t.Fatalf("want no fields, got %q", texts)
				}
				return
			}
			if len(fields) != 1 || fields[0].text != tc.keep {
				t.Fatalf("want only %q, got %q", tc.keep, texts)
			}
		})
	}
}
