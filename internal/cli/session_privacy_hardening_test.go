package cli

import (
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tombstonePrivacyFixture(t *testing.T, brainDir string) {
	tombstonePrivacyFixtureSession(t, brainDir, "secret-sess")
}

func tombstonePrivacyFixtureSession(t *testing.T, brainDir, sessionID string) {
	t.Helper()
	stones := loadSessionTombstones(brainDir)
	stones.Excluded[sessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "test"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
}

func TestPrivacyPlanAndVerifyPropagateInjectedStatReadDirAndReadFailures(t *testing.T) {
	t.Run("plan stat", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		target := filepath.Join(brainDir, "sessions", "main", "20260802T000000Z_secret.jsonl")
		original := privacyLstat
		privacyLstat = func(path string) (os.FileInfo, error) {
			if path == target {
				return nil, fs.ErrPermission
			}
			return original(path)
		}
		t.Cleanup(func() { privacyLstat = original })
		if _, err := buildSessionPurgePlan(brainDir, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("plan stat error = %v, want %s", err, memoryErrStateCorrupt)
		}
	})

	t.Run("verify derived stat", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		tombstonePrivacyFixture(t, brainDir)
		target := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
		original := privacyLstat
		privacyLstat = func(path string) (os.FileInfo, error) {
			if path == target {
				return nil, fs.ErrPermission
			}
			return original(path)
		}
		t.Cleanup(func() { privacyLstat = original })
		if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("verify stat error = %v, want %s", err, memoryErrStateCorrupt)
		}
	})

	t.Run("facts readdir", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		if err := os.MkdirAll(filepath.Join(brainDir, factsDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		original := privacyReadDir
		privacyReadDir = func(dir *os.File, n int) ([]os.DirEntry, error) {
			if dir.Name() == filepath.Join(brainDir, factsDirName) {
				return nil, fs.ErrPermission
			}
			return original(dir, n)
		}
		t.Cleanup(func() { privacyReadDir = original })
		if _, err := buildSessionPurgePlan(brainDir, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("plan ReadDir error = %v, want %s", err, memoryErrStateCorrupt)
		}
	})

	t.Run("verify cache read", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		tombstonePrivacyFixture(t, brainDir)
		target := filepath.Join(brainDir, filepath.FromSlash(historyScanCachePath))
		original := privacyReadAll
		privacyReadAll = func(reader io.Reader, max int64, source string) ([]byte, error) {
			if source == target {
				return nil, fs.ErrPermission
			}
			return original(reader, max, source)
		}
		t.Cleanup(func() { privacyReadAll = original })
		if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("verify read error = %v, want %s", err, memoryErrStateCorrupt)
		}
	})
}

func TestMalformedEpisodesFailPlanFilterAndVerifyWithoutRewrite(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(patternsEpisodesPath))
	want := []byte("{malformed episode\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	transcripts := map[string]bool{"sessions/main/20260802T000000Z_secret.jsonl": true}
	if _, _, err := filterEpisodesFile(brainDir, "secret-sess", transcripts, true); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("filter error = %v, want %s", err, memoryErrStateCorrupt)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(want) {
		t.Fatalf("malformed source was rewritten: %q err=%v", got, err)
	}
	if _, err := buildSessionPurgePlan(brainDir, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("plan error = %v, want %s", err, memoryErrStateCorrupt)
	}
	tombstonePrivacyFixture(t, brainDir)
	if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("verify error = %v, want %s", err, memoryErrStateCorrupt)
	}
}

func TestPrivacyFactAnchorsMatchExcludedTranscriptPath(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Now().UTC()
	secretPath := "sessions/main/20260802T000000Z_secret.jsonl"
	cleanPath := "sessions/main/20260801T000000Z_clean.jsonl"
	singleBlank := factRecord{
		ID: factRecordID("blank id secret", nil), Text: "blank id secret", Branch: "main", Origin: "distilled", Status: factStatusActive,
		CreatedAt: now, UpdatedAt: now, Provenance: []factAnchor{{Transcript: "./" + secretPath}},
	}
	singleMismatch := factRecord{
		ID: factRecordID("mismatched id secret", nil), Text: "mismatched id secret", Branch: "main", Origin: "distilled", Status: factStatusActive,
		CreatedAt: now, UpdatedAt: now, Provenance: []factAnchor{{SessionID: "different-id", Transcript: secretPath}},
	}
	multi := factRecord{
		ID: factRecordID("corroborated", nil), Text: "corroborated", Branch: "main", Origin: "distilled", Status: factStatusActive,
		CreatedAt: now, UpdatedAt: now, Provenance: []factAnchor{{Transcript: secretPath}, {SessionID: "clean-sess", Transcript: cleanPath}},
	}
	if err := writeFacts(brainDir, "main", []factRecord{singleBlank, singleMismatch, multi}); err != nil {
		t.Fatal(err)
	}
	tombstonePrivacyFixture(t, brainDir)
	manifest, _ := loadBrainManifest(brainDir)
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !guard.blocksFact(singleBlank) || !guard.blocksFact(singleMismatch) || guard.blocksFact(multi) {
		t.Fatalf("path-aware serving guard mismatch: blank=%v mismatch=%v multi=%v", guard.blocksFact(singleBlank), guard.blocksFact(singleMismatch), guard.blocksFact(multi))
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.FactsDeleted != 2 || plan.FactAnchorsStripped != 1 {
		t.Fatalf("path-aware plan = deleted %d stripped %d, want 2/1", plan.FactsDeleted, plan.FactAnchorsStripped)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	foundPathFact := false
	for _, finding := range report.Findings {
		if finding.Artifact == "facts" && strings.Contains(finding.Detail, "transcript anchor") {
			foundPathFact = true
		}
	}
	if !foundPathFact {
		t.Fatalf("verify missed path-only fact provenance: %+v", report.Findings)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].ID != multi.ID || len(facts[0].Provenance) != 1 || facts[0].Provenance[0].SessionID != "clean-sess" {
		t.Fatalf("surviving corroborated fact = %+v", facts)
	}
}

func TestPurgeSessionFactsPreservesAuthoredFactAfterDistillAnchor(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Now().UTC()
	transcript := "sessions/main/20260802T000000Z_secret.jsonl"
	authored := factRecord{
		ID:   factRecordID("Always preserve authored knowledge.", []string{"workflow.testing.rules"}),
		Text: "Always preserve authored knowledge.", Paths: []string{"workflow.testing.rules"},
		Branch: "main", Origin: factOriginAuthored, Status: factStatusActive,
		CreatedAt: now, UpdatedAt: now,
		Provenance: []factAnchor{{SessionID: "secret-sess", Transcript: transcript, Line: 1, DistillTurnID: "turn-1"}},
	}
	if err := writeFacts(brainDir, "main", []factRecord{authored}); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.FactsDeleted != 0 || plan.FactAnchorsStripped != 1 {
		t.Fatalf("authored purge plan = deleted %d stripped %d, want 0/1", plan.FactsDeleted, plan.FactAnchorsStripped)
	}
	deleted, stripped, err := purgeSessionFacts(brainDir, "secret-sess", map[string]bool{transcript: true})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || stripped != 1 {
		t.Fatalf("authored purge accounting = deleted %d stripped %d, want 0/1", deleted, stripped)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Origin != factOriginAuthored || len(facts[0].Provenance) != 0 {
		t.Fatalf("purge deleted or retained derived provenance on authored fact: %+v", facts)
	}
}

func TestPrivacyDerivationBlocksDuplicateManifestTranscriptAlias(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	secretPath := "sessions/main/20260802T000000Z_secret.jsonl"
	manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
		SessionID: "alias-sess", Branch: "main", TranscriptPath: "./" + secretPath, CreatedAt: time.Now().UTC(),
	})
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	tombstonePrivacyFixture(t, brainDir)
	filtered, err := filterTombstonedSessions(brainDir, manifest.Sources.Sessions.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range filtered {
		if session.SessionID == "secret-sess" || session.SessionID == "alias-sess" {
			t.Fatalf("duplicate transcript alias survived filter: %+v", session)
		}
	}
	episodes, _, err := buildBrainEpisodes(brainDir, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, episode := range episodes {
		if normalizePrivacyTranscriptPath(episode.Source.Path) == secretPath {
			t.Fatalf("duplicate transcript alias produced an episode: %+v", episode)
		}
	}
	if err := buildPatternCorpus(brainDir, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM indexed_sessions WHERE transcript_path = ? OR transcript_path = ?`, secretPath, "./"+secretPath).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("pattern corpus retained %d duplicate excluded transcript rows", rows)
	}
}

func TestPrivacyDerivedReadGateInspectsCurrentCorpusRows(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	if err := buildPatternCorpus(brainDir, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	dirtyCorpus, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, time.Now().UTC(), "test", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corpus, dirtyCorpus, 0o600); err != nil {
		t.Fatal(err)
	}
	dirtyDB, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dirtyDB.Exec(`UPDATE indexed_sessions SET session_id = 'mismatched-id' WHERE session_id = 'secret-sess'`); err != nil {
		dirtyDB.Close()
		t.Fatal(err)
	}
	if _, err := dirtyDB.Exec(`UPDATE episodes SET session_id = 'mismatched-id' WHERE session_id = 'secret-sess'`); err != nil {
		dirtyDB.Close()
		t.Fatal(err)
	}
	if err := dirtyDB.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(corpus, future, future); err != nil {
		t.Fatal(err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Artifact == "pattern_corpus_session" || finding.Artifact == "pattern_corpus_episode" {
			found = true
		}
	}
	if report.Clean || !found {
		t.Fatalf("current but dirty corpus passed verification: %+v", report.Findings)
	}
	allowed, err := privacyDerivedReadGate(brainDir)
	if err != nil || allowed {
		t.Fatalf("dirty corpus gate = allowed %v err %v", allowed, err)
	}
	if err := os.Remove(corpus); err != nil {
		t.Fatal(err)
	}
	allowed, err = privacyDerivedReadGate(brainDir)
	if err != nil || !allowed {
		t.Fatalf("clean tombstoned brain should re-enable corpus reads: allowed %v err %v", allowed, err)
	}
	if err := buildPatternCorpus(brainDir, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	allowed, err = privacyDerivedReadGate(brainDir)
	if err != nil || !allowed {
		t.Fatalf("clean corpus rebuilt under tombstones should remain readable: allowed %v err %v", allowed, err)
	}
}

func TestPrivacyVerifyInspectsCommittedPatternRowsStillInWAL(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, time.Now().UTC(), "test", true); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(brainDir, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Leave a committed excluded-path row only in WAL. An immutable read of a
	// main-file-only copy cannot see it, so privacy verification must snapshot
	// and replay the pinned WAL before doing its row-level checks.
	corpus := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	db, err := sql.Open(sqliteDriverName, corpus)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA wal_autocheckpoint=0`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare WAL fixture with %q: %v", statement, err)
		}
	}
	const secretPath = "sessions/main/20260802T000000Z_secret.jsonl"
	if _, err := db.Exec(`INSERT INTO indexed_sessions
		(id, repo_key, session_id, transcript_path, size, mtime_unix_nano, content_sha, parser_version, episodes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"wal-secret", "repo", "mismatched-id", secretPath, 1, 1, "sha256:wal-secret", patternIndexerVersion, 0); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(corpus + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture did not retain a non-empty WAL: info=%v err=%v", info, err)
	}

	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Artifact == "pattern_corpus_session" && strings.Contains(finding.Detail, secretPath) {
			found = true
		}
	}
	if report.Clean || !found {
		t.Fatalf("committed excluded row in WAL passed verification: %+v", report.Findings)
	}
}

func TestPrivacyFactVectorInventoryHandlesGlobMetacharacters(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	rel := filepath.ToSlash(filepath.Join(factsDirName, "feature-[literal]", embedStoreDirName, embedStoreFileName))
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(embedStoreMagic), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := privacyDerivedStoreArtifacts(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range artifacts {
		if artifact == rel {
			found = true
		}
	}
	if !found {
		t.Fatalf("literal metacharacter branch vector omitted: %v", artifacts)
	}
}

func TestDistillCacheKeySessionIDRecognizesAllSupportedNamespaces(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{key: "legacy-session", want: "legacy-session", ok: true},
		{key: distillSessionCacheKey("main", "slash/session"), want: "slash/session", ok: true},
		{key: distillSessionCacheKeyForPipeline(distillPipelineCandidates, "main", " spaced "), want: "spaced", ok: true},
		{key: "pipeline:unknown/main/session", ok: false},
		{key: "too/many/path/components", ok: false},
	} {
		got, ok := distillCacheKeySessionID(tc.key)
		if ok != tc.ok || got != tc.want {
			t.Errorf("distillCacheKeySessionID(%q) = (%q,%v), want (%q,%v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPurgeDropsWholeDistillCacheAndVerifyRejectsMalformedKeys(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{
		"secret-sess": "sha256:" + strings.Repeat("a", 64),
		distillSessionCacheKey("main", "other-session"):                                     "sha256:" + strings.Repeat("b", 64),
		distillSessionCacheKeyForPipeline(distillPipelineCandidates, "main", "secret-sess"): "sha256:" + strings.Repeat("c", 64),
	}}
	if err := saveDistillCache(brainDir, cache); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DistillCacheReset == nil || plan.DistillCacheReset.Path != distillCachePath || plan.DistillCacheReset.Bytes <= 0 {
		t.Fatalf("purge plan did not disclose whole-cache reset: %+v", plan.DistillCacheReset)
	}
	if err := purgeDistillCacheEntries(brainDir, "secret-sess"); err != nil {
		t.Fatal(err)
	}
	if got := loadDistillCache(brainDir); len(got.Sessions) != 0 {
		t.Fatalf("privacy purge retained ambiguous cache hints: %+v", got.Sessions)
	}

	tombstonePrivacyFixture(t, brainDir)
	if err := saveDistillCache(brainDir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{"bad/key/shape/extra": "sha256:" + strings.Repeat("d", 64)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("privacy verify accepted malformed cache ownership: %v", err)
	}
}

func TestPurgeClearsOversizedOpaqueDistillCacheWithoutReadingIt(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	cachePath := filepath.Join(brainDir, filepath.FromSlash(distillCachePath))
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxManifestBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DistillCacheReset == nil || plan.DistillCacheReset.Bytes != maxManifestBytes+1 {
		t.Fatalf("oversized cache was not inventoried exactly: %+v", plan.DistillCacheReset)
	}
	if err := purgeDistillCacheEntries(brainDir, "secret-sess"); err != nil {
		t.Fatal(err)
	}
	cache, err := loadDistillCacheForPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.Sessions) != 0 {
		t.Fatalf("oversized opaque cache survived purge: %+v", cache.Sessions)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= maxManifestBytes {
		t.Fatalf("purged cache remains oversized: %d bytes", info.Size())
	}
}

func TestPrivacyDistillCacheRejectsAndPurgesOpaqueBytes(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":       `{"version":1,"sessions":{},"candidate_payload":"CANARY"}`,
		"duplicate sessions":  `{"version":1,"sessions":{"secret-sess":"CANARY"},"sessions":{}}`,
		"invalid fingerprint": `{"version":1,"sessions":{"main/safe":"CANARY_SECRET_FROM_PURGED_SESSION"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			brainDir := writePrivacyFixture(t)
			tombstonePrivacyFixture(t, brainDir)
			if err := writeBrainRelativeFileAtomic(brainDir, distillCachePath, []byte(raw+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
				t.Fatalf("privacy verify accepted opaque cache bytes: %v", err)
			}
			if err := purgeDistillCacheEntries(brainDir, "secret-sess"); err != nil {
				t.Fatal(err)
			}
			cache, err := loadDistillCacheForPrivacy(brainDir)
			if err != nil || len(cache.Sessions) != 0 {
				t.Fatalf("purged cache = %+v, %v", cache, err)
			}
			data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(distillCachePath)))
			if err != nil || strings.Contains(string(data), "CANARY") {
				t.Fatalf("opaque cache bytes survived purge: %q, %v", data, err)
			}
		})
	}
}

func TestPrivacyVerifyRecognizesSlashBearingLegacyDistillCacheKey(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixtureSession(t, brainDir, "legacy/session")
	if err := saveDistillCache(brainDir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{
		"legacy/session": "sha256:" + strings.Repeat("e", 64),
	}}); err != nil {
		t.Fatal(err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.SessionID == "legacy/session" && finding.Artifact == "distill_cache" && finding.Detail == "legacy/session" {
			found = true
			break
		}
	}
	if report.Clean || !found {
		t.Fatalf("privacy verify missed slash-bearing legacy cache owner: %+v", report)
	}
}

func TestPrivacyOwnsAtomicFactWriteOrphans(t *testing.T) {
	writeOrphans := func(t *testing.T, brainDir string) []string {
		t.Helper()
		rels := []string{
			".manifest.json.tmp-54321",
			"facts/.distill-cache.json.tmp-12345",
			"facts/main/.facts.ndjson.tmp-AbC123",
			"facts/main/.proposals.ndjson.tmp-98765",
		}
		for _, rel := range rels {
			path := filepath.Join(brainDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("candidate-derived secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return rels
	}

	t.Run("purge inventory removes orphans", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		rels := writeOrphans(t, brainDir)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Sources == nil {
			manifest.Sources = &brainSources{}
		}
		if manifest.Sources.Facts == nil {
			manifest.Sources.Facts = &factSourceManifest{}
		}
		manifest.Sources.Facts.Warnings = []string{"candidate excerpt for secret-sess"}
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
		if err != nil {
			t.Fatal(err)
		}
		planned := map[string]bool{}
		for _, artifact := range plan.DerivedStores {
			planned[artifact.Path] = true
		}
		for _, rel := range rels {
			if !planned[rel] {
				t.Fatalf("purge plan omitted atomic orphan %s: %+v", rel, plan.DerivedStores)
			}
		}
		if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		for _, rel := range rels {
			if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Fatalf("atomic orphan survived purge %s: %v", rel, err)
			}
		}
		manifest, err = loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Sources != nil && manifest.Sources.Facts != nil && len(manifest.Sources.Facts.Warnings) != 0 {
			t.Fatalf("privacy purge retained fact warning excerpts: %+v", manifest.Sources.Facts.Warnings)
		}
	})

	t.Run("verify never blesses post-tombstone orphan", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		tombstonePrivacyFixture(t, brainDir)
		rels := writeOrphans(t, brainDir)
		report, err := verifySessionPrivacy(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, finding := range report.Findings {
			if finding.Artifact == "candidate_atomic_staging" {
				rel, _, _ := strings.Cut(finding.Detail, " is an unpublished")
				found[rel] = true
			}
		}
		for _, rel := range rels {
			if !found[rel] {
				t.Fatalf("privacy verify missed atomic orphan %s: %+v", rel, report.Findings)
			}
		}
	})
}

func TestPrivacyVerifyRejectsCurrentCorruptSQLiteStore(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("corrupt current corpus error = %v, want %s", err, memoryErrStateCorrupt)
	}
}

func TestPrivacyTypedErrorCanBeUnwrapped(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName, sessionTombstonesFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadSessionTombstonesChecked(brainDir)
	var typed *sessionTombstoneLoadError
	if !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt {
		t.Fatalf("nonregular tombstone error = %v", err)
	}
}
