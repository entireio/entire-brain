package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyEpisodeShape(t *testing.T) {
	cases := []struct {
		nCmds, nTools     int
		hasWrite, hasRead bool
		want              string
	}{
		{0, 0, true, false, "write"},      // wrote a file
		{3, 0, false, false, "shell"},     // ran commands, no writes
		{0, 2, false, false, "read_only"}, // used read tools
		{0, 0, false, true, "read_only"},  // read a file
		{0, 0, false, false, "conversation"},
	}
	for _, c := range cases {
		if got := classifyEpisodeShape(c.nCmds, c.nTools, c.hasWrite, c.hasRead); got != c.want {
			t.Errorf("classifyEpisodeShape(%d,%d,%v,%v)=%q want %q", c.nCmds, c.nTools, c.hasWrite, c.hasRead, got, c.want)
		}
	}
}

// themeCorpus seeds N conversation episodes under one intent so a theme forms.
func themeCorpus(t *testing.T, now time.Time, intentSig string, n int) *sql.DB {
	t.Helper()
	db := freshCorpusDB(t)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("episode:c%d", i)
		insertCorpusEpisode(t, db, id, intentSig, "neutral", 0, now) // 0 cmds, no files -> conversation
	}
	if err := classifyEpisodeShapes(db, now); err != nil {
		t.Fatal(err)
	}
	if err := buildThemes(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestThemeCandidatesFromReadOnlyWork(t *testing.T) {
	now := time.Now()
	db := themeCorpus(t, now, "how:worktree-fingerprint", 4)

	// Shapes classified.
	var conv int
	db.QueryRow(`SELECT COUNT(*) FROM episode_shapes WHERE shape='conversation'`).Scan(&conv)
	if conv != 4 {
		t.Errorf("expected 4 conversation episodes, got %d", conv)
	}
	// A candidate theme formed (unverified -> not in verified-only view).
	cands := queryThemeViews(db, false)
	if len(cands) != 1 {
		t.Fatalf("expected 1 theme candidate, got %d", len(cands))
	}
	if cands[0].Status != "candidate" || cands[0].Verdict != "" {
		t.Errorf("fresh theme should be an unverified candidate: %+v", cands[0])
	}
	if v := queryThemeViews(db, true); len(v) != 0 {
		t.Errorf("no theme is verified yet, want 0 verified, got %d", len(v))
	}

	// Below-threshold clusters do not form themes.
	db2 := themeCorpus(t, now, "how:rare-thing", 2)
	if c := queryThemeViews(db2, false); len(c) != 0 {
		t.Errorf("a 2-episode cluster must not form a theme, got %d", len(c))
	}
}

func TestThemeVerifyPromotesAndSuppresses(t *testing.T) {
	now := time.Now()
	db := themeCorpus(t, now, "how:worktree-fingerprint", 4)

	// Accept it.
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if !strings.Contains(string(input), "worktree") {
			t.Errorf("theme verifier input missing the theme: %s", string(input))
		}
		return `{"schema_version":1,"verdict":"accepted","reason":"coherent recurring investigation"}`, nil
	}
	stats, err := verifyThemes(context.Background(), db, t.TempDir(), t.TempDir(), "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 {
		t.Fatalf("expected 1 theme verified, got %+v", stats)
	}
	verified := queryThemeViews(db, true)
	if len(verified) != 1 || verified[0].Status != "active" {
		t.Errorf("accepted theme should be active + verified-visible: %+v", verified)
	}

	// Re-run: cached (no second agent call needed since verdict present).
	calls := 0
	run2 := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return `{"verdict":"accepted"}`, nil
	}
	stats2, _ := verifyThemes(context.Background(), db, t.TempDir(), t.TempDir(), "codex", "", "", run2, now)
	if stats2.Cached != 1 || calls != 0 {
		t.Errorf("expected cached theme (no new agent call), got %+v calls=%d", stats2, calls)
	}

	// A rejected theme is suppressed from all surfaces.
	db.Exec(`UPDATE themes SET verdict='rejected', status='candidate'`)
	if v := queryThemeViews(db, false); len(v) != 0 {
		t.Errorf("rejected theme must be suppressed, got %d", len(v))
	}
}

func TestThemeGrowthRefingerprints(t *testing.T) {
	now := time.Now()
	db := themeCorpus(t, now, "how:worktree-fingerprint", 4)
	var fp1 string
	db.QueryRow(`SELECT fingerprint FROM themes LIMIT 1`).Scan(&fp1)
	// Mark accepted, then grow the cluster → fingerprint changes → demoted to candidate.
	db.Exec(`UPDATE themes SET verdict='accepted', status='active'`)
	insertCorpusEpisode(t, db, "episode:grow", "how:worktree-fingerprint", "neutral", 0, now)
	if err := classifyEpisodeShapes(db, now); err != nil {
		t.Fatal(err)
	}
	if err := buildThemes(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var fp2, status, verdict string
	db.QueryRow(`SELECT fingerprint, status, COALESCE(verdict,'') FROM themes LIMIT 1`).Scan(&fp2, &status, &verdict)
	if fp2 == fp1 {
		t.Error("growing the member set should refingerprint the theme")
	}
	if status != "candidate" || verdict != "" {
		t.Errorf("a grown theme must drop back to unverified candidate, got status=%q verdict=%q", status, verdict)
	}
}

// DV2: the theme verifier must receive member evidence (keys + redacted
// excerpts), so a coherent theme is distinguishable from a same-intent_sig
// grab-bag. A stub agent that accepts only when the members share a keyword
// proves the input is rich enough to make that call.
const coherentThemeTranscript = `{"type":"event_msg","payload":{"type":"user_message","message":"how does the worktree fingerprint work"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"WORKTREE fingerprint hashes git status; secret ghp_SECRETTOKEN0123456789abcdef at /Users/alice/x"}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"how does the worktree fingerprint work"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"the WORKTREE fingerprint also covers the binary diff"}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"how does the worktree fingerprint work"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"WORKTREE fingerprint is recomputed each refresh"}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"thanks"}}`

const incoherentThemeTranscript = `{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This is about CSS color variables and theming."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This concerns TCP networking socket timeouts."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This is about JSON parsing edge cases."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"done"}}`

func TestThemeVerifierEvidenceBased(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "sa", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/sa.jsonl", author: "Ada", transcript: coherentThemeTranscript},
		{id: "sb", branch: "main", agent: "Codex", checkpoint: "c2", relPath: "sessions/main/sb.jsonl", author: "Ada", transcript: incoherentThemeTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, brainDir)
	if n := corpusCount(t, db, "themes"); n < 2 {
		t.Fatalf("expected at least 2 theme candidates (coherent + incoherent), got %d", n)
	}

	var allInput string
	// Stub agent: accept only when the members visibly share the WORKTREE keyword
	// in their evidence; otherwise the members are an incoherent grab-bag.
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		in := string(input)
		allInput += in + "\n"
		if strings.Count(strings.ToLower(in), "worktree") >= 2 {
			return `{"verdict":"accepted","reason":"coherent"}`, nil
		}
		return `{"verdict":"needs_split","reason":"members unrelated"}`, nil
	}
	if _, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}

	// The verifier input carried member evidence, not just label/count.
	if !strings.Contains(allInput, "member_evidence") || !strings.Contains(allInput, "episode_key") {
		t.Errorf("theme verifier input missing member evidence:\n%s", allInput)
	}
	// Redaction held on the theme evidence input.
	if strings.Contains(allInput, "ghp_SECRETTOKEN0123456789abcdef") || strings.Contains(allInput, "/Users/alice") {
		t.Errorf("theme verifier input leaked a secret/home path:\n%s", allInput)
	}
	// Coherent worktree theme accepted; incoherent investigate theme not accepted.
	var accepted, notAccepted int
	rows, _ := db.Query(`SELECT description, verdict FROM themes`)
	for rows.Next() {
		var desc, verdict string
		rows.Scan(&desc, &verdict)
		if verdict == "accepted" {
			accepted++
			if !strings.Contains(strings.ToLower(desc), "worktree") {
				t.Errorf("the accepted theme should be the coherent worktree one, got %q", desc)
			}
		} else if verdict == "needs_split" || verdict == "rejected" {
			notAccepted++
		}
	}
	rows.Close()
	if accepted != 1 || notAccepted < 1 {
		t.Errorf("expected 1 accepted (coherent) + >=1 not-accepted (incoherent), got accepted=%d notAccepted=%d", accepted, notAccepted)
	}
}

func TestThemeVerifyNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	db := themeCorpus(t, time.Now(), "how:worktree-fingerprint", 4)
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := verifyThemes(context.Background(), db, t.TempDir(), t.TempDir(), "codex", "", "", run, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress rejection, got %v", err)
	}
	if called {
		t.Error("agent must not run under no-egress")
	}
}
