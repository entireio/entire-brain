package cli

import (
	"context"
	"encoding/json"
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
		{0, 0, true, false, "write"},
		{3, 0, false, false, "shell"},
		{0, 2, false, false, "read_only"},
		{0, 0, false, true, "read_only"},
		{0, 0, false, false, "conversation"},
	}
	for _, c := range cases {
		if got := classifyEpisodeShape(c.nCmds, c.nTools, c.hasWrite, c.hasRead); got != c.want {
			t.Errorf("classifyEpisodeShape(%d,%d,%v,%v)=%q want %q", c.nCmds, c.nTools, c.hasWrite, c.hasRead, got, c.want)
		}
	}
}

// SR4 fixture: 3 worktree episodes with VARIED wording (different intent_sigs)
// that share one latent practice, and 3 "investigate the issue" episodes with
// the SAME intent_sig but UNRELATED topics.
const themeProposalTranscript = `{"type":"event_msg","payload":{"type":"user_message","message":"how does the worktree fingerprint work"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"The WORKTREE fingerprint hashes git status and the binary diff."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"explain the worktree hash to me"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"The WORKTREE hash is recomputed on each refresh."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"clarify what makes the worktree id change"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"The WORKTREE id changes when tracked files change."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This is about CSS color variables and theming."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This concerns TCP networking socket timeouts."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"investigate the issue"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"This is about JSON parsing edge cases."}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"thanks"}}`

func themeProposalBrain(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: themeProposalTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

// A stub agent that groups by MEANING from the evidence: any sampled episode
// whose excerpt/intent mentions "worktree" forms one accepted theme; unrelated
// episodes are left ungrouped.
func worktreeGroupingAgent(t *testing.T) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		var payload struct {
			Episodes []map[string]string `json:"episodes"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			t.Fatalf("proposal input not JSON: %v\n%s", err, input)
		}
		var members []string
		for _, e := range payload.Episodes {
			if strings.Contains(strings.ToLower(e["excerpt"]+" "+e["intent"]), "worktree") {
				members = append(members, e["episode_key"])
			}
		}
		resp := map[string]any{"themes": []map[string]any{{
			"title":       "worktree fingerprint investigations",
			"description": "recurring questions about how the worktree fingerprint is computed",
			"member_keys": members,
			"rationale":   "all ask how the worktree fingerprint/hash/id behaves",
			"verdict":     "accepted",
		}}}
		b, _ := json.Marshal(resp)
		return string(b), nil
	}
}

func TestThemeProposalGroupsByMeaningNotIntentSig(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := themeProposalBrain(t, now)
	db := openCorpus(t, brainDir)

	// Refresh does NOT create themes (agent-only).
	if n := corpusCount(t, db, "themes"); n != 0 {
		t.Fatalf("refresh must not create themes (agent-only), got %d", n)
	}

	stats, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", worktreeGroupingAgent(t), now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 {
		t.Fatalf("expected 1 proposed theme, got %+v", stats)
	}

	// One accepted theme; it spans the worktree episodes regardless of wording.
	views := queryThemeViews(db, true)
	if len(views) != 1 {
		t.Fatalf("expected 1 accepted theme, got %d", len(views))
	}
	var memberKeys string
	db.QueryRow(`SELECT member_keys FROM themes WHERE id=?`, views[0].ID).Scan(&memberKeys)
	var members []string
	json.Unmarshal([]byte(memberKeys), &members)
	if len(members) != 3 {
		t.Fatalf("expected 3 worktree members, got %d: %v", len(members), members)
	}
	// (2) Varied wording → the members carry DIFFERENT intent_sigs (grouped by
	// meaning, not intent_sig bucket).
	sigs := map[string]bool{}
	for _, k := range members {
		var sig string
		db.QueryRow(`SELECT intent_sig FROM episodes WHERE episode_key=?`, k).Scan(&sig)
		sigs[sig] = true
	}
	if len(sigs) < 2 {
		t.Errorf("expected members to span >1 intent_sig (cross-wording), got %v", sigs)
	}
	// (1) Same-intent_sig-but-unrelated episodes are NOT grouped into the theme.
	rows, _ := db.Query(`SELECT episode_key FROM episodes WHERE intent_sig='investigate:issue'`)
	var investigate []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		investigate = append(investigate, k)
	}
	rows.Close()
	if len(investigate) != 3 {
		t.Fatalf("setup: expected 3 investigate episodes, got %d", len(investigate))
	}
	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m] = true
	}
	for _, k := range investigate {
		if memberSet[k] {
			t.Errorf("an unrelated same-intent_sig episode must not be in the accepted theme: %s", k)
		}
	}

	// (3) The theme record uses the agent's description, not an intent_sig label.
	r, ok := getCorpusTheme(brainDir, views[0].ID)
	if !ok {
		t.Fatal("expected to fetch the theme")
	}
	if !strings.Contains(r.Text, "worktree fingerprint is computed") {
		t.Errorf("theme should carry the agent description, got:\n%s", r.Text)
	}
	if strings.Contains(r.Text, "Recurring conversation work:") {
		t.Errorf("theme must not use the old intent_sig bucket label:\n%s", r.Text)
	}
}

func TestThemeProposalCachedBySample(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := themeProposalBrain(t, now)
	db := openCorpus(t, brainDir)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return worktreeGroupingAgent(t)(ctx, dir, args, input, timeout)
	}
	if _, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	// Second run, unchanged sample → cached (agent not called again).
	stats2, _ := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now)
	if calls != 1 || stats2.Cached == 0 {
		t.Errorf("expected cached re-run (1 agent call), got calls=%d stats=%+v", calls, stats2)
	}
}

// verdictThemeAgent groups worktree episodes and returns the given verdict.
func verdictThemeAgent(t *testing.T, verdict string) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		var payload struct {
			Episodes []map[string]string `json:"episodes"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			t.Fatalf("proposal input not JSON: %v", err)
		}
		var members []string
		for _, e := range payload.Episodes {
			if strings.Contains(strings.ToLower(e["excerpt"]+" "+e["intent"]), "worktree") {
				members = append(members, e["episode_key"])
			}
		}
		resp := map[string]any{"themes": []map[string]any{{
			"title": "worktree investigations", "description": "how the worktree fingerprint is computed",
			"member_keys": members, "verdict": verdict,
		}}}
		b, _ := json.Marshal(resp)
		return string(b), nil
	}
}

// Item #3: a missing/unknown/non-accepting verdict must never surface a theme or
// count as Verified.
func TestThemeProposalUnknownVerdictNotAccepted(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	for _, v := range []string{"", "bogus", "needs_split", "low_confidence"} {
		brainDir := themeProposalBrain(t, now)
		db := openCorpus(t, brainDir)
		stats, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", verdictThemeAgent(t, v), now)
		if err != nil {
			t.Fatalf("verdict %q: %v", v, err)
		}
		if stats.Verified != 0 {
			t.Errorf("verdict %q must not count as Verified, got %+v", v, stats)
		}
		if n := len(queryThemeViews(db, true)); n != 0 {
			t.Errorf("verdict %q must not surface as an accepted theme, got %d", v, n)
		}
		var stored string
		db.QueryRow(`SELECT COALESCE(verdict,'') FROM themes LIMIT 1`).Scan(&stored)
		if stored == "accepted" {
			t.Errorf("verdict %q must not be stored as accepted", v)
		}
	}
}

// Item #2: editing episode CONTENT (intent) while keeping the same episode keys
// invalidates the cached theme proposal (the agent re-runs).
func TestThemeProposalCacheInvalidatesOnContentChange(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	brainDir := themeProposalBrain(t, now)
	db := openCorpus(t, brainDir)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return worktreeGroupingAgent(t)(ctx, dir, args, input, timeout)
	}
	if _, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unchanged sample must reuse cache, calls=%d", calls)
	}
	// Mutate a sampled episode's content WITHOUT changing its key.
	if _, err := db.Exec(`UPDATE episodes SET intent_raw='totally different content xyzzy'
		WHERE id IN (SELECT e.id FROM episodes e JOIN episode_shapes s ON s.episode_id=e.id
		             WHERE s.shape IN ('read_only','conversation') LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("changed episode content must invalidate the cache, calls=%d", calls)
	}
}

func TestThemeProposalNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	now := time.Now()
	brainDir := themeProposalBrain(t, now)
	db := openCorpus(t, brainDir)
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := verifyThemes(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now)
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress rejection, got %v", err)
	}
	if called {
		t.Error("agent must not run under no-egress")
	}
}
