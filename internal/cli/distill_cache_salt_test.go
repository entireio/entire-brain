package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The facts are the product, not the model that produced them: two runs that
// differ only in agent, model, effort or agent command must derive the SAME
// cache salt, so the setup backfill (codex, effort low) and the session-end
// hook (auto, no effort) share every entry instead of re-spending on each
// other's sessions.
func TestDistillCacheSaltIgnoresAgentModelEffort(t *testing.T) {
	base := distillCacheSalt("prompt", "reconcile", 0.75, distillCommandOptions{agent: "codex", effort: "low"})
	variants := map[string]distillCommandOptions{
		"auto, no effort":      {agent: "auto"},
		"claude-code + sonnet": {agent: "claude-code", model: "sonnet", effort: "low"},
		"codex + model":        {agent: "codex", model: "gpt-5-mini", effort: "high"},
		"command agent":        {agent: "command", agentCommand: []string{"fake", "--flag"}},
		"empty":                {},
	}
	for name, opts := range variants {
		if got := distillCacheSalt("prompt", "reconcile", 0.75, opts); got != base {
			t.Errorf("%s: salt differs from base (%s vs %s)", name, got, base)
		}
	}
	// What the extractor is asked still invalidates: prompt (taxonomy lives in
	// it), reconcile prompt, threshold and preprocessing version.
	if distillCacheSalt("other prompt", "reconcile", 0.75, distillCommandOptions{}) == base {
		t.Error("prompt change must change the salt")
	}
	if distillCacheSalt("prompt", "other reconcile", 0.75, distillCommandOptions{}) == base {
		t.Error("reconcile prompt change must change the salt")
	}
	if distillCacheSalt("prompt", "reconcile", 0.9, distillCommandOptions{}) == base {
		t.Error("threshold change must change the salt")
	}
	if distillCacheSalt("prompt", "reconcile", 0.75, distillCommandOptions{checkpointBacked: true}) == base {
		t.Error("checkpoint-backed preprocessing version must change the salt")
	}
}

// A cache a pre-upgrade binary wrote under the agent-bearing salt still
// proves its sessions unchanged. A run with the SAME agent/effort (the setup
// backfill's codex + low) must treat them as cached — zero agent calls — and
// rewrite the entries under the agent-independent salt, after which a run
// with a DIFFERENT agent (the hook's auto) is cached too.
func TestDistillGrandfathersAgentSaltedCacheEntries(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	sessions := []exportSession{
		{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
		{SessionID: "s2", Branch: "main", LatestCheckpoint: "cp2", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(-time.Hour)},
	}
	transcript := "turn one\nturn two\n"
	for _, s := range sessions {
		p := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	var calls int
	countRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "", nil
	}
	backfill := distillCommandOptions{agent: "codex", effort: "low", run: countRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, concurrency: 1}

	// Seed the cache exactly as the old binary left it after a codex/low
	// backfill: current keys, agent-bearing salt for THAT configuration.
	legacySalts, err := legacyAgentDistillCacheSaltsForBrain(brainDir, backfill, now)
	if err != nil {
		t.Fatal(err)
	}
	input := preprocessTranscriptForDistill(transcript)
	seeded := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}}
	for _, s := range sessions {
		seeded.Sessions[distillSessionCacheKey("main", s.SessionID)] = distillSessionFingerprint(s, "main", input, legacySalts[0])
	}
	if err := saveDistillCache(brainDir, seeded); err != nil {
		t.Fatal(err)
	}

	// Dry run sees them as cached before any run touches the cache.
	report, err := buildDistillDryRunReport(brainDir, backfill, now)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.CachedSessions != len(sessions) || report.SessionsToDistill != 0 {
		t.Fatalf("dry run must grandfather agent-salted entries: cached=%d to_distill=%d", report.CachedSessions, report.SessionsToDistill)
	}

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, backfill, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if calls != 0 {
		t.Fatalf("grandfathered sessions must cost ZERO agent calls, got %d", calls)
	}
	migrated := loadDistillCache(brainDir)
	newSalt, err := distillCacheSaltForBrain(brainDir, distillCommandOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		key := distillSessionCacheKey("main", s.SessionID)
		want := distillSessionFingerprint(s, "main", input, newSalt)
		if got := migrated.Sessions[key]; got != want {
			t.Errorf("%s: entry not rewritten under the agent-independent salt: got %q want %q (legacy %q)", s.SessionID, got, want, seeded.Sessions[key])
		}
	}

	// The hook's configuration now shares the cache: different agent, no
	// effort, still zero calls.
	hook := backfill
	hook.effort = ""
	hook.agent, hook.agentCommand = "command", []string{"fake"}
	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, hook, now); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("a different agent must reuse the shared cache, got %d agent calls", calls)
	}

	// The legacy salts enumerate the KNOWN producers, so even a run with
	// different flags migrates what the old backfill wrote: re-seed the
	// legacy entries and distill with the hook's configuration.
	if err := saveDistillCache(brainDir, seeded); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, hook, now); err != nil {
		t.Fatalf("hook migration run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("hook configuration must migrate backfill-written legacy entries, got %d agent calls", calls)
	}

	// Grandfathering proves content unchanged, nothing more: a changed
	// transcript behind a legacy entry must still re-distill.
	if err := saveDistillCache(brainDir, seeded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(sessions[0].TranscriptPath)), []byte(transcript+"a new turn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, backfill, now); err != nil {
		t.Fatalf("changed-content run: %v", err)
	}
	if calls == 0 {
		t.Fatal("changed session must re-distill, not be grandfathered")
	}
}

// The legacy salt list is built from the known producers, not just this
// run's flags: the setup backfill (resolved agent, effort low) and the hook
// (resolved agent, no effort), for every agent name that reached the salt.
func TestLegacyAgentDistillCacheSaltsEnumerateKnownProducers(t *testing.T) {
	hookRun := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}}
	salts := legacyAgentDistillCacheSalts("prompt", "reconcile", 0.75, hookRun)
	want := map[string]distillCommandOptions{
		"own config":            hookRun,
		"codex backfill":        {agent: "codex", effort: "low"},
		"claude-code backfill":  {agent: "claude-code", effort: "low"},
		"codex hook":            {agent: "codex"},
		"claude-code hook":      {agent: "claude-code"},
		"own agent, effort low": {agent: "command", agentCommand: []string{"fake"}, effort: "low"},
	}
	have := map[string]bool{}
	for _, salt := range salts {
		if have[salt] {
			t.Errorf("duplicate legacy salt %s", salt)
		}
		have[salt] = true
	}
	for name, opts := range want {
		if !have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, opts, 0)] {
			t.Errorf("%s: producer salt missing from the legacy list", name)
		}
		if have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, opts, distillPreprocessingVersion)] {
			t.Errorf("%s: versioned salt must not be listed for a file-backed brain", name)
		}
	}
	// A model the user pinned is a producer too, alongside the no-model one.
	pinned := distillCommandOptions{agent: "codex", model: "gpt-5-mini", effort: "low"}
	salts = legacyAgentDistillCacheSalts("prompt", "reconcile", 0.75, pinned)
	have = map[string]bool{}
	for _, salt := range salts {
		have[salt] = true
	}
	if !have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, pinned, 0)] || !have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, distillCommandOptions{agent: "codex", effort: "low"}, 0)] {
		t.Error("pinned-model run must list both its own and the model-less backfill salt")
	}
	// Checkpoint-backed brains add the versioned formula for every producer.
	checkpointed := distillCommandOptions{agent: "codex", effort: "low", checkpointBacked: true}
	salts = legacyAgentDistillCacheSalts("prompt", "reconcile", 0.75, checkpointed)
	have = map[string]bool{}
	for _, salt := range salts {
		have[salt] = true
	}
	if !have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, checkpointed, distillPreprocessingVersion)] || !have[legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, checkpointed, 0)] {
		t.Error("checkpoint-backed run must list both the versioned and the pre-checkpoint salt")
	}
}

// Checkpoint-backed brains: an identity (checkpoint-sha256) entry an old
// agent-salted binary wrote hits the fast path with no transcript read, and a
// file-era content entry from before the brain became checkpoint-backed is
// grandfathered too (the migration grandfatheredContentFingerprint covers
// only for agent-independent salts).
func TestDistillGrandfathersAgentSaltedCheckpointEntries(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	oldWriter := distillCommandOptions{agent: "codex", effort: "low"}
	current := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, checkpointBacked: true, transcriptMode: "raw"}
	current.cacheSalt = distillCacheSalt("prompt", "reconcile", 0.8, current)
	current.legacyCacheSalt = distillCacheSaltVersion("prompt", "reconcile", 0.8, 0)
	current.legacyAgentCacheSalts = legacyAgentDistillCacheSalts("prompt", "reconcile", 0.8, current)
	key := distillSessionCacheKey("main", session.SessionID)
	wantFingerprint := distillFingerprintForSession(session, "main", "", current)

	// Identity entry under the old writer's versioned, agent-bearing salt.
	oldWriterVersioned := oldWriter
	oldWriterVersioned.checkpointBacked, oldWriterVersioned.transcriptMode = true, "raw"
	oldWriterVersioned.cacheSalt = legacyAgentDistillCacheSalt("prompt", "reconcile", 0.8, oldWriter, distillPreprocessingVersion)
	identity := distillFingerprintForSession(session, "main", "", oldWriterVersioned)
	if identity == wantFingerprint {
		t.Fatal("fixture: legacy and current identity fingerprints must differ")
	}
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{key: identity}}
	prepared := <-startSessionPrefetch(context.Background(), brain, "", nil, []exportSession{session}, cache, current, func(exportSession) string { return "main" })
	if !prepared.cached || prepared.readErr != nil || prepared.rawBytes != 0 || prepared.fingerprint != wantFingerprint {
		t.Fatalf("agent-salted identity entry must hit the fast path and be rewritten: %+v", prepared)
	}

	// File-era content entry under the old writer's pre-checkpoint salt.
	content := preprocessTranscriptForDistill(conversationClaudeFixture)
	fileEra := distillSessionFingerprint(session, "main", content, legacyAgentDistillCacheSalt("prompt", "reconcile", 0.8, oldWriter, 0))
	cache = distillCache{Version: distillCacheVersion, Sessions: map[string]string{key: fileEra}}
	prepared = <-startSessionPrefetch(context.Background(), brain, "", nil, []exportSession{session}, cache, current, func(exportSession) string { return "main" })
	if !prepared.cached || prepared.readErr != nil || prepared.fingerprint != wantFingerprint {
		t.Fatalf("file-era agent-salted entry must be grandfathered on a checkpoint-backed brain: %+v", prepared)
	}

	// Changed content invalidates the file-era entry.
	if grandfatheredAgentSaltFingerprint(fileEra, session, "main", content+"\nmore", current) {
		t.Fatal("changed content must not be grandfathered")
	}
}

// A grown transcript re-sends only its new chunks across the salt change:
// chunks an older agent-salted binary already extracted are recognised under
// the legacy salts.
func TestDistillChunkDoneUnderLegacySalt(t *testing.T) {
	legacy := legacyAgentDistillCacheSalt("prompt", "reconcile", 0.75, distillCommandOptions{agent: "codex", effort: "low"}, 0)
	current := distillCacheSalt("prompt", "reconcile", 0.75, distillCommandOptions{})
	done := map[string]bool{distillChunkHash(legacy, "old chunk"): true}
	salts := legacyAgentDistillCacheSalts("prompt", "reconcile", 0.75, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}})
	if done[distillChunkHash(current, "old chunk")] {
		t.Fatal("fixture: current-salt hash must miss")
	}
	if !distillChunkDoneUnderLegacySalt(done, "old chunk", salts) {
		t.Error("chunk extracted under a legacy producer salt must count as done")
	}
	if distillChunkDoneUnderLegacySalt(done, "new chunk", salts) {
		t.Error("a chunk never extracted must not count as done")
	}
	if distillChunkDoneUnderLegacySalt(done, "old chunk", nil) {
		t.Error("no legacy salts, no match")
	}
}
