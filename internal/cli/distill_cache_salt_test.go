package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	// backfill: current keys, agent-bearing salt for THAT configuration, and
	// a chunk record hashed under the same salt.
	prompt, reconcileText, threshold, err := distillSaltInputsForBrain(brainDir, backfill, now)
	if err != nil {
		t.Fatal(err)
	}
	legacySalts := legacyAgentDistillCacheSalts(prompt, reconcileText, threshold, backfill)
	input := preprocessTranscriptForDistill(transcript)
	chunks := chunkTranscript(input, defaultDistillChunkSize)
	seeded := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}, Chunks: map[string][]string{}}
	for _, s := range sessions {
		key := distillSessionCacheKey("main", s.SessionID)
		seeded.Sessions[key] = distillSessionFingerprint(s, "main", input, legacySalts[0])
		seeded.Chunks[key] = distillChunkHashes(chunks, legacySalts[0])
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
	newSalt := distillCacheSalt(prompt, reconcileText, threshold, distillCommandOptions{})
	wantChunks := distillChunkHashes(chunks, newSalt)
	for _, s := range sessions {
		key := distillSessionCacheKey("main", s.SessionID)
		want := distillSessionFingerprint(s, "main", input, newSalt)
		if got := migrated.Sessions[key]; got != want {
			t.Errorf("%s: entry not rewritten under the agent-independent salt: got %q want %q (legacy %q)", s.SessionID, got, want, seeded.Sessions[key])
		}
		// The chunk record is rewritten too, not carried by reference: a
		// legacy-salted record would be dead weight after the next
		// preprocessing-version bump.
		if got := migrated.Chunks[key]; strings.Join(got, ",") != strings.Join(wantChunks, ",") {
			t.Errorf("%s: chunk record not rewritten under the agent-independent salt: got %v want %v", s.SessionID, got, wantChunks)
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
		"ollama backfill":       {agent: "ollama", effort: "low"},
		"ollama hook":           {agent: "ollama"},
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

// The chunk grandfathering CALL SITE, not just its helper: a transcript an
// older agent-salted binary distilled in two chunks grows by one. The run
// under the hook's configuration must dispatch only the new chunk (the two
// old ones are recognised under the legacy producer salt) and rewrite the
// whole chunk record under the agent-independent salt. Deleting the legacy
// clause at the call site dispatches all three.
func TestDistillChunkReuseSurvivesSaltChangeEndToEnd(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := "turn one\nturn two\n"
	brainDir := writeSingleSessionFixture(t, now, old)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	key := distillSessionCacheKey("main", session.SessionID)

	var calls int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "workflow.testing.rules\tRun go test before pushing.\n", nil
	}
	// maxChunkBytes=1 puts every line on its own chunk.
	hook := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: 1, timeout: time.Minute}
	prompt, reconcileText, threshold, err := distillSaltInputsForBrain(brainDir, hook, now)
	if err != nil {
		t.Fatal(err)
	}
	legacySalt := legacyAgentDistillCacheSalt(prompt, reconcileText, threshold, distillCommandOptions{agent: "codex", effort: "low"}, 0)
	oldInput := preprocessTranscriptForDistill(old)
	oldChunks := chunkTranscript(oldInput, 1)
	if len(oldChunks) != 2 {
		t.Fatalf("fixture: want 2 chunks, got %d", len(oldChunks))
	}
	seeded := distillCache{
		Version:  distillCacheVersion,
		Sessions: map[string]string{key: distillSessionFingerprint(session, "main", oldInput, legacySalt)},
		Chunks:   map[string][]string{key: distillChunkHashes(oldChunks, legacySalt)},
	}
	if err := saveDistillCache(brainDir, seeded); err != nil {
		t.Fatal(err)
	}

	grown := old + "a new turn\n"
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(session.TranscriptPath)), []byte(grown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, hook, now); err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if calls != 1 {
		t.Fatalf("only the new chunk should be dispatched across the salt change, got %d agent calls", calls)
	}

	newInput := preprocessTranscriptForDistill(grown)
	newSalt := distillCacheSalt(prompt, reconcileText, threshold, hook)
	cache := loadDistillCache(brainDir)
	if got, want := cache.Sessions[key], distillSessionFingerprint(session, "main", newInput, newSalt); got != want {
		t.Errorf("session entry: got %q want %q", got, want)
	}
	wantChunks := distillChunkHashes(chunkTranscript(newInput, 1), newSalt)
	if got := cache.Chunks[key]; strings.Join(got, ",") != strings.Join(wantChunks, ",") {
		t.Errorf("chunk record must cover all three chunks under the agent-independent salt: got %v want %v", got, wantChunks)
	}
}

// With the agent out of the salt, a chunk whose agent call exits 0 with no
// usable records (a refusal, a rate-limit body, the wrong format) would be
// cached as distilled for EVERY later agent, model and effort, with a
// branch-wide --force the only recovery. An all-empty extraction is recorded
// as tentative instead: not cached, not distilled for status, re-derived once
// by the next run, and accepted only when it comes back empty again.
func TestDistillEmptyExtractionIsRetriedOnceAcrossProducers(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "turn one\nturn two\n")
	key := distillSessionCacheKey("main", "s1")
	runWith := func(output string) (calls, facts int) {
		t.Helper()
		run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
			calls++
			return output, nil
		}
		opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
		source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
		if err != nil {
			t.Fatalf("runDistillForBrain: %v", err)
		}
		return calls, source.Facts
	}

	// Pass 1 (the backfill): the agent succeeds but yields nothing.
	if calls, facts := runWith(""); calls != 1 || facts != 0 {
		t.Fatalf("pass 1: calls=%d facts=%d", calls, facts)
	}
	cache := loadDistillCache(brainDir)
	tentative := cache.Sessions[key]
	if !isDistillEmptyFingerprint(tentative) {
		t.Fatalf("an all-empty extraction must be recorded as tentative, got %q", tentative)
	}
	if len(cache.Chunks[key]) != 0 {
		t.Fatalf("a tentative entry must not mark its empty chunks done: %v", cache.Chunks[key])
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 0 || status.Sessions != 1 {
		t.Fatalf("a tentative session is not distilled for status: %+v", status)
	}
	report, err := buildDistillDryRunReport(brainDir, distillCommandOptions{agent: "codex", effort: "low"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionsToDistill != 1 || report.CachedSessions != 0 {
		t.Fatalf("dry run must predict the tentative session as to-distill: %+v", report)
	}

	// Pass 2 (the hook, another producer): re-derived once, facts land, final.
	if calls, facts := runWith("preferences.coding.style\tThe user prefers concise commits.\n"); calls != 1 || facts != 1 {
		t.Fatalf("pass 2 must re-dispatch the empty session: calls=%d facts=%d", calls, facts)
	}
	cache = loadDistillCache(brainDir)
	if got, want := cache.Sessions[key], strings.TrimPrefix(tentative, distillEmptyFingerprintPrefix); got != want {
		t.Fatalf("pass 2 must write the final fingerprint: got %q want %q", got, want)
	}
	if len(cache.Chunks[key]) != 1 {
		t.Fatalf("final entry must record its chunk: %v", cache.Chunks[key])
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 1 {
		t.Fatalf("final entry counts as distilled: %+v", status)
	}
	// Pass 3: cached, whatever the agent.
	if calls, _ := runWith(""); calls != 0 {
		t.Fatalf("pass 3 must be cached, got %d agent calls", calls)
	}

	// Two empty results in a row ARE the answer: a genuinely trivial session
	// is retried once, not re-spent on forever.
	brainDir = writeSingleSessionFixture(t, now, "hello\n")
	if calls, _ := runWith(""); calls != 1 {
		t.Fatalf("trivial session pass 1: got %d agent calls", calls)
	}
	if calls, _ := runWith(""); calls != 1 {
		t.Fatalf("trivial session must be retried exactly once: got %d agent calls", calls)
	}
	if cache := loadDistillCache(brainDir); isDistillEmptyFingerprint(cache.Sessions[key]) || len(cache.Chunks[key]) != 1 {
		t.Fatalf("second empty result must be accepted as final: %+v", cache)
	}
	if calls, _ := runWith(""); calls != 0 {
		t.Fatalf("confirmed-empty session must be cached, got %d agent calls", calls)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 1 {
		t.Fatalf("confirmed-empty session counts as distilled: %+v", status)
	}
}

// The tentative-empty state and the pre-upgrade legacy-key fallback do not
// interact: a legacy entry that proves content unchanged is honoured ahead of
// any extraction and lands as a FINAL entry (no tentative detour), while a
// legacy entry whose content is stale is dropped the moment the session is
// visited, so it can never resurrect a session the new key says is tentative.
func TestDistillTentativeEntryAndLegacyKeyDoNotInteract(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	transcript := "turn one\nturn two\n"
	emptyAgent := func(calls *int) distillCommandOptions {
		return distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
			run: func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
				*calls++
				return "", nil
			}}
	}
	key := distillSessionCacheKey("main", "s1")

	// Stale legacy entry: hashed against content the transcript no longer has.
	brainDir := writeSingleSessionFixture(t, now, transcript)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	stale := distillCache{Version: distillCacheVersion, Sessions: map[string]string{
		"s1": legacyDistillSessionFingerprint(session, "main", session.TranscriptPath, preprocessTranscriptForDistill("turn one\n")),
	}}
	if err := saveDistillCache(brainDir, stale); err != nil {
		t.Fatal(err)
	}
	var calls int
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, emptyAgent(&calls), now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("stale legacy entry must not be grandfathered: got %d agent calls", calls)
	}
	cache := loadDistillCache(brainDir)
	if !isDistillEmptyFingerprint(cache.Sessions[key]) {
		t.Fatalf("empty extraction must be tentative under the new key, got %q", cache.Sessions[key])
	}
	if _, ok := cache.Sessions["s1"]; ok {
		t.Fatalf("legacy key must be dropped once the session is visited, so it cannot rescue the tentative entry: %v", cache.Sessions)
	}
	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, emptyAgent(&calls), now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("tentative session must be retried once, got %d agent calls", calls)
	}
	if cache := loadDistillCache(brainDir); isDistillEmptyFingerprint(cache.Sessions[key]) {
		t.Fatalf("second empty result must finalise the entry, got %q", cache.Sessions[key])
	}

	// Matching legacy entry: grandfathered with zero calls, straight to final.
	brainDir = writeSingleSessionFixture(t, now, transcript)
	matching := distillCache{Version: distillCacheVersion, Sessions: map[string]string{
		"s1": legacyDistillSessionFingerprint(session, "main", session.TranscriptPath, preprocessTranscriptForDistill(transcript)),
	}}
	if err := saveDistillCache(brainDir, matching); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, emptyAgent(&calls), now); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("matching legacy entry must cost zero agent calls, got %d", calls)
	}
	cache = loadDistillCache(brainDir)
	if fp := cache.Sessions[key]; fp == "" || isDistillEmptyFingerprint(fp) {
		t.Fatalf("grandfathering must write a FINAL entry under the new key, got %q", fp)
	}
	if status := factsBackfillStatusForBrain(brainDir); status.Distilled != 1 {
		t.Fatalf("grandfathered session counts as distilled: %+v", status)
	}
}

// A grown session whose only NEW chunks come back empty: the tentative entry
// keeps the chunk hashes the cache had already seen succeed, so the retry
// re-sends only the empty chunk, and the second empty result finalises the
// entry with the full chunk record.
func TestDistillTentativeEntryKeepsReusedChunks(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, "turn one\nturn two\n")
	key := distillSessionCacheKey("main", "s1")
	runWith := func(output string) (calls int) {
		t.Helper()
		opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, maxChunkBytes: 1, timeout: time.Minute,
			run: func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
				calls++
				return output, nil
			}}
		if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
			t.Fatalf("runDistillForBrain: %v", err)
		}
		return calls
	}
	// maxChunkBytes=1: one chunk per line. Two chunks, both with facts.
	if calls := runWith("workflow.testing.rules\tRun go test before pushing.\n"); calls != 2 {
		t.Fatalf("seed: want 2 agent calls, got %d", calls)
	}
	if cache := loadDistillCache(brainDir); len(cache.Chunks[key]) != 2 {
		t.Fatalf("seed: want 2 chunk hashes, got %v", cache.Chunks[key])
	}

	// Grow by one line; the agent now returns nothing for the new chunk.
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash("sessions/main/s1.jsonl")), []byte("turn one\nturn two\na new turn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if calls := runWith(""); calls != 1 {
		t.Fatalf("grown session must re-send only its new chunk, got %d agent calls", calls)
	}
	cache := loadDistillCache(brainDir)
	if !isDistillEmptyFingerprint(cache.Sessions[key]) {
		t.Fatalf("an all-empty run over the dispatched chunks must be tentative, got %q", cache.Sessions[key])
	}
	if len(cache.Chunks[key]) != 2 {
		t.Fatalf("tentative entry must keep the two reused chunk hashes and drop the empty one, got %v", cache.Chunks[key])
	}

	// The retry re-sends only the empty chunk; a second empty result finalises.
	if calls := runWith(""); calls != 1 {
		t.Fatalf("retry must re-send only the empty chunk, got %d agent calls", calls)
	}
	cache = loadDistillCache(brainDir)
	if isDistillEmptyFingerprint(cache.Sessions[key]) {
		t.Fatalf("second empty result must finalise, got %q", cache.Sessions[key])
	}
	if len(cache.Chunks[key]) != 3 {
		t.Fatalf("final entry must record all three chunks, got %v", cache.Chunks[key])
	}
	if calls := runWith(""); calls != 0 {
		t.Fatalf("finalised session must be cached, got %d agent calls", calls)
	}
}
