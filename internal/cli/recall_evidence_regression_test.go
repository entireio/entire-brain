package cli

import (
	"context"
	"testing"
)

func TestRecallEvidenceBracketPlainText(t *testing.T) {
	for _, text := range []string{"[user]\nRelease requires owner approval.\n", "{user}\nRelease requires owner approval.\n"} {
		fields, incomplete := evidenceFields([]byte(text), "sessions/release.txt")
		if incomplete || len(fields) != 1 || fields[0].text != text || fields[0].pointer != "" {
			t.Fatalf("plain text discarded or changed: fields=%v incomplete=%v", fields, incomplete)
		}
	}
}

func TestRecallEvidenceJSONDoesNotFallBackToPlainText(t *testing.T) {
	for _, tc := range []struct{ path, text string }{
		{"session.jsonl", "{broken\n"},
		{"session.json", "[broken\n"},
		{"session.txt", `{"unrecognized":"private metadata"}`},
		{"session.txt", `[{"unrecognized":"private metadata"}]`},
		{"session.txt", "{broken\n" + `{"type":"system","content":"private instructions"}`},
	} {
		fields, incomplete := evidenceFields([]byte(tc.text), tc.path)
		if !incomplete || len(fields) != 0 {
			t.Fatalf("structured content exposed for %s: fields=%v incomplete=%v", tc.path, fields, incomplete)
		}
	}
}

func TestRecallEvidenceSessionDefaultBranch(t *testing.T) {
	for _, tc := range []struct{ source, manifest, want string }{
		{"develop", "main", "develop"},
		{"", "develop", "develop"},
		{"", "", distillDefaultBranch},
	} {
		t.Run(tc.source+"/"+tc.manifest, func(t *testing.T) {
			_, brain := evidenceFixture(t)
			m, err := loadBrainManifest(brain)
			if err != nil {
				t.Fatal(err)
			}
			m.DefaultBranch = tc.manifest
			m.Sources.Sessions.DefaultBranch = tc.source
			m.Sources.Sessions.Sessions[0].Branch = ""
			if err := writeBrainManifestAndReadme(brain, *m); err != nil {
				t.Fatal(err)
			}
			c, err := collectEvidence(context.Background(), brain, tc.want, "target", 10, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Spans) == 0 {
				t.Fatalf("session default branch skipped: %+v", c)
			}
			for _, span := range c.Spans {
				if span.Branch != tc.want {
					t.Fatalf("wrong citation branch: %s", span.Branch)
				}
			}
			c, err = collectEvidence(context.Background(), brain, "other", "target", 10, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Spans) != 0 {
				t.Fatalf("cross-branch evidence: %+v", c)
			}
		})
	}
}
