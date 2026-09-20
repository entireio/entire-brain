package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDeepVerdictInvalidatedByChangedEvidence(t *testing.T) {
	now := time.Now()
	db, pid := deepCorpus(t, now)
	dir := t.TempDir()
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return `{"verdict":"accepted"}`, nil
	}
	_, err := verifyDeepDossiers(context.Background(), db, dir, dir, "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	before := calls
	if _, err = db.Exec(`UPDATE episode_commands SET raw_redacted='mise deploy --unsafe-no-approval' WHERE head='mise deploy'`); err != nil {
		t.Fatal(err)
	}
	deep, err := buildDeepDossier(db, dir, pid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range deep.Parameters {
		if strings.Contains(p, "unsafe-no-approval") {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture did not change dossier")
	}
	stats, err := verifyDeepDossiers(context.Background(), db, dir, dir, "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls <= before || stats.Verified == 0 {
		t.Fatalf("regression %+v", stats)
	}
	t.Logf("changed concrete deployment invocation, prior verifier calls %d; stats=%+v", calls, stats)
}

func TestDeepVerificationRepairsUnboundCachedVerdict(t *testing.T) {
	now := time.Now()
	db, _ := deepCorpus(t, now)
	dir := t.TempDir()
	calls := 0
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		calls++
		return `{"verdict":"accepted"}`, nil
	}
	if _, err := verifyDeepDossiers(context.Background(), db, dir, dir, "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	before := calls
	if _, err := db.Exec(`UPDATE deep_dossiers SET verifier_json_redacted='{"verdict":"accepted"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDeepDossiers(context.Background(), db, dir, dir, "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls <= before {
		t.Fatal("unbound cached verdict reused")
	}
	before = calls
	if _, err := verifyDeepDossiers(context.Background(), db, dir, dir, "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("unchanged bound verdict was not reused")
	}
}

func TestDossierFingerprintBindsShallowVerifierEvidence(t *testing.T) {
	base := dossierRecord{Title: "deploy", Variants: []string{"staging"}, SourceAnchors: []dossierAnchor{{SessionID: "s", Transcript: "sessions/s.jsonl", StartLine: 1, EndLine: 3, CheckpointID: "checkpoint"}}}
	fingerprint := dossierFingerprint(base, 0.9)
	for _, field := range []string{"title", "variants", "path", "end", "checkpoint"} {
		t.Run(field, func(t *testing.T) {
			changed := base
			changed.Variants = append([]string(nil), base.Variants...)
			changed.SourceAnchors = append([]dossierAnchor(nil), base.SourceAnchors...)
			switch field {
			case "title":
				changed.Title = "ship"
			case "variants":
				changed.Variants[0] = "production"
			case "path":
				changed.SourceAnchors[0].Transcript = "sessions/other.jsonl"
			case "end":
				changed.SourceAnchors[0].EndLine = 6
			case "checkpoint":
				changed.SourceAnchors[0].CheckpointID = "other"
			}
			if dossierFingerprint(changed, 0.9) == fingerprint {
				t.Fatalf("%s omitted from evidence identity", field)
			}
		})
	}
	base.Fingerprint = "stored-value"
	if dossierFingerprint(base, 0.9) != fingerprint {
		t.Fatal("self-referential fingerprint changes evidence identity")
	}
}
