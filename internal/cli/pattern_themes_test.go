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
	stats, err := verifyThemes(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
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
	stats2, _ := verifyThemes(context.Background(), db, t.TempDir(), "codex", "", "", run2, now)
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

func TestThemeVerifyNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	db := themeCorpus(t, time.Now(), "how:worktree-fingerprint", 4)
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := verifyThemes(context.Background(), db, t.TempDir(), "codex", "", "", run, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress rejection, got %v", err)
	}
	if called {
		t.Error("agent must not run under no-egress")
	}
}
