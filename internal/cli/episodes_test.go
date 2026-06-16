package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type sessionFixture struct {
	id         string
	branch     string
	agent      string
	checkpoint string
	relPath    string
	author     string
	transcript string
}

// writeEpisodeFixture lays down a brain dir with the given sessions and their
// transcripts, plus a manifest carrying repoKey, and returns the brain dir.
func writeEpisodeFixture(t *testing.T, now time.Time, repoKey string, sessions []sessionFixture) string {
	t.Helper()
	brainDir := t.TempDir()
	manifestSessions := make([]exportSession, 0, len(sessions))
	for _, s := range sessions {
		p := filepath.Join(brainDir, filepath.FromSlash(s.relPath))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s.transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		es := exportSession{
			SessionID:        s.id,
			Branch:           s.branch,
			Agent:            s.agent,
			LatestCheckpoint: s.checkpoint,
			TranscriptPath:   s.relPath,
			CreatedAt:        now.Add(-time.Hour),
		}
		if s.author != "" {
			es.Authors = []exportAuthor{{Name: s.author}}
		}
		manifestSessions = append(manifestSessions, es)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		RepoKey:       repoKey,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: manifestSessions},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

const claudeReviewTranscript = `{"type":"user","message":{"content":"review release readiness for the cli"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Checked changelog and tests."}]}}
{"type":"user","message":{"content":"that didn't work, still failing"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Fixed the flaky test."}]}}
{"type":"user","message":{"content":"perfect, thanks"}}`

func TestBuildBrainEpisodes(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Claude", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript},
	})

	episodes, source, err := buildBrainEpisodes(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 3 {
		t.Fatalf("got %d episodes, want 3", len(episodes))
	}

	// Ordered by source line: review(corrected), reprompt(success), thanks(neutral).
	wantLabels := []string{reinforcementCorrected, reinforcementSuccess, reinforcementNeutral}
	for i, want := range wantLabels {
		if episodes[i].Reinforcement != want {
			t.Errorf("episode %d reinforcement = %q, want %q (intent %q)", i, episodes[i].Reinforcement, want, episodes[i].Intent)
		}
	}

	first := episodes[0]
	if first.IntentSignature != "review:release" {
		t.Errorf("intent_signature = %q, want review:release", first.IntentSignature)
	}
	if first.RepoKey != "gh/acme/cli" {
		t.Errorf("repo_key = %q, want gh/acme/cli", first.RepoKey)
	}
	if first.Author != "Ada" || first.Agent != "Claude" || first.CheckpointID != "cp1" || first.Branch != "main" {
		t.Errorf("identity fields wrong: %+v", first)
	}
	if first.Source.Path != "sessions/main/s1.jsonl" || first.Source.Line != 1 {
		t.Errorf("source anchor = %+v, want sessions/main/s1.jsonl:1", first.Source)
	}
	if first.CreatedAt == nil {
		t.Error("created_at not set")
	}

	wantCounts := reinforcementCounts{Success: 1, Corrected: 1, Neutral: 1}
	if source.Reinforcement != wantCounts {
		t.Errorf("source reinforcement = %+v, want %+v", source.Reinforcement, wantCounts)
	}
	if source.Episodes != 3 {
		t.Errorf("source episodes = %d, want 3", source.Episodes)
	}
	if source.SessionsFingerprint == "" {
		t.Error("source missing sessions fingerprint")
	}
}

func TestBuildBrainEpisodesCrossDialect(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	codex := `{"type":"event_msg","payload":{"type":"user_message","message":"add a benchmark for the parser"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"added"}}
{"type":"event_msg","payload":{"type":"user_message","message":"no, that's wrong - revert it"}}`
	opencode := `{
  "info": {"id": "s3"},
  "messages": [
    {"info": {"role": "user"}, "parts": [{"type": "text", "text": "remove the header border"}]},
    {"info": {"role": "assistant"}, "parts": [{"type": "text", "text": "removed"}]},
    {"info": {"role": "user"}, "parts": [{"type": "text", "text": "looks good, thanks"}]}
  ]
}`
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s2", branch: "main", agent: "Codex", checkpoint: "cp2", relPath: "sessions/main/s2.jsonl", author: "Bo", transcript: codex},
		{id: "s3", branch: "main", agent: "opencode", checkpoint: "cp3", relPath: "sessions/main/s3.json", author: "Cy", transcript: opencode},
	})

	episodes, _, err := buildBrainEpisodes(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	// s2: 2 user turns -> 2 episodes (add..corrected, revert..neutral).
	// s3: 2 user turns -> 2 episodes (remove..success, thanks..neutral).
	bySession := map[string][]episodeRecord{}
	for _, ep := range episodes {
		bySession[ep.SessionID] = append(bySession[ep.SessionID], ep)
	}
	if len(bySession["s2"]) != 2 || len(bySession["s3"]) != 2 {
		t.Fatalf("per-session counts wrong: s2=%d s3=%d", len(bySession["s2"]), len(bySession["s3"]))
	}
	if got := bySession["s2"][0].Reinforcement; got != reinforcementCorrected {
		t.Errorf("codex first episode = %q, want corrected", got)
	}
	if got := bySession["s3"][0].Reinforcement; got != reinforcementSuccess {
		t.Errorf("opencode first episode = %q, want success", got)
	}
}

func TestEpisodeCommitSignal(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	// A Codex work segment that lands a commit, with a neutral next user turn:
	// next-turn cues say nothing, but the commit confirmation makes it success.
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"commit the rename"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git commit -am rename\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","output":"[main 7249258] Rename plugin to entire-brain\n 3 files changed"}}
{"type":"event_msg","payload":{"type":"user_message","message":"now update the docs"}}`
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: transcript},
	})

	episodes, _, err := buildBrainEpisodes(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("got %d episodes, want 2", len(episodes))
	}
	if episodes[0].Reinforcement != reinforcementSuccess {
		t.Errorf("commit episode = %q, want success (neutral next turn, but commit landed)", episodes[0].Reinforcement)
	}
	if episodes[1].Reinforcement != reinforcementNeutral {
		t.Errorf("trailing episode = %q, want neutral", episodes[1].Reinforcement)
	}
}

func TestEpisodeIDStableAcrossRebuilds(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Claude", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript},
	})

	first, _, err := buildBrainEpisodes(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	// A later rebuild at a different time must yield identical records — ids and
	// content depend only on identity-defining fields, not on when refresh ran.
	second, _, err := buildBrainEpisodes(brainDir, now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("episodes not stable across rebuilds")
	}

	// A repeated identical request in the same session must not collide.
	seen := map[string]bool{}
	for _, ep := range first {
		if seen[ep.ID] {
			t.Errorf("duplicate episode id %s", ep.ID)
		}
		seen[ep.ID] = true
	}
}

func TestWriteLoadAndStatus(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Claude", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript},
	})

	// Missing before refresh.
	if got := buildPatternsStatusReport(brainDir); got.Present || got.Freshness != "missing" {
		t.Errorf("pre-refresh status = %+v, want present=false freshness=missing", got)
	}

	source, err := writeBrainEpisodesAndSource(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := loadBrainEpisodes(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	built, _, _ := buildBrainEpisodes(brainDir, now)
	if !reflect.DeepEqual(loaded, built) {
		t.Fatal("persisted episodes differ from freshly built")
	}

	status := buildPatternsStatusReport(brainDir)
	if !status.Present || status.Freshness != "current" {
		t.Errorf("status = %+v, want present=true freshness=current", status)
	}
	if status.Episodes != source.Episodes {
		t.Errorf("status episodes %d != source episodes %d", status.Episodes, source.Episodes)
	}

	// Mutating the session fingerprint makes the layer stale without rebuilding.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions[0].LatestCheckpoint = "cp1-moved"
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if got := buildPatternsStatusReport(brainDir); got.Freshness != "stale" {
		t.Errorf("post-mutation freshness = %q, want stale", got.Freshness)
	}
}
