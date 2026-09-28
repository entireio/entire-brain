package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyEmptyTranscriptCannotResolveLine(t *testing.T) {
	b := t.TempDir()
	rel := "sessions/empty.jsonl"
	if e := os.MkdirAll(filepath.Join(b, "sessions"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(b, rel), nil, 0600); e != nil {
		t.Fatal(e)
	}
	v := verifyContext{ctx: context.Background(), brainDir: b, manifest: &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{{SessionID: "empty", TranscriptPath: rel}}}}}}
	for _, line := range []int{1, 42} {
		got := v.verifyAnchor(factAnchor{SessionID: "empty", Transcript: rel, Line: line}, "")
		if got.Verdict != verifyVerdictOrphaned {
			t.Fatalf("empty line %d: %+v", line, got)
		}
	}
}

func TestVerifyEmptyTranscriptStillComparedToCheckpoint(t *testing.T) {
	f := newVerifyFixture(t)
	const cp = "aaa111aaa111"
	const rel = "sessions/main/session.jsonl"
	f.writeBrainFile(t, rel, "")
	f.writeSessions(t, []exportSession{{SessionID: "sess1", Branch: "main", LatestCheckpoint: cp, TranscriptPath: rel}})
	f.addLocalCheckpoint(t, cp, "sess1", "", "retained transcript\n")
	fact := verifyFactFixture("fact:empty", "Empty export differs from its retained checkpoint.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{SessionID: "sess1", CheckpointID: cp, Transcript: rel}})
	f.writeFacts(t, "main", []factRecord{fact})
	out, err := execute(t, NewRootCommand(f.opts), "verify", "fact:empty", "--json")
	if err == nil {
		t.Fatalf("empty export verified: %s", out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Stale != 1 {
		t.Fatalf("expected stale checkpoint comparison: %+v", report)
	}
}
