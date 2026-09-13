package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEntityIndexForPrivacy persists a derived entity index that attributes
// one symbol to one commit, one checkpoint, and one captured session.
func writeEntityIndexForPrivacy(t *testing.T, brainDir, sessionID string) {
	t.Helper()
	cache := entityIndexCache{
		SchemaVersion: entityIndexCacheVersion,
		GeneratedAt:   time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		MetaTip:       "0000000000000000000000000000000000000001",
		Entries: map[string][]entityIndexOccurrence{
			"internal/secret.go#function#Rotate": {{
				Commit:        "1111111111111111111111111111111111111111",
				CheckpointIDs: []string{"cp2"},
				SessionIDs:    []string{sessionID},
				Subject:       "rotate the key",
			}},
		},
		Aliases:   map[string]string{},
		LastTouch: map[string]int64{},
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, entitiesDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The entity index is a derived projection of captured sessions: it answers
// "which sessions changed this symbol". Excluding a session used to leave that
// answer intact, and `privacy verify` reported "clean: no excluded content in
// any inspected projection" while `entities history` still named the excluded
// session.
func TestSessionExclusionStripsTheEntityIndexSessionLinkage(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	writeEntityIndexForPrivacy(t, brainDir, "secret-sess")
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// executeSessionCleanup refuses to publish success unless its own
	// post-cleanup verification is clean, so a retained linkage fails here.
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-sess") {
		t.Fatalf("excluded session id survives in %s: %s", entitiesCachePath, raw)
	}
	// The commit and the checkpoint are ordinary git history and stay: privacy
	// cleanup erases the session linkage, not the repository's own record.
	if !strings.Contains(string(raw), "1111111111111111111111111111111111111111") {
		t.Fatal("cleanup dropped the commit occurrence instead of just its session linkage")
	}

	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.Clean {
		t.Fatalf("verify not clean after cleanup: %+v", report.Findings)
	}
}

// Verification must be able to FAIL on this artifact, not merely pass over it:
// a stale index that still names a tombstoned session is a finding.
func TestPrivacyVerifyFlagsARetainedEntityIndexLinkage(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	// A later `entities backfill` from a binary without the build-time guard,
	// or a restored backup, puts the linkage back.
	writeEntityIndexForPrivacy(t, brainDir, "secret-sess")

	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Clean {
		t.Fatal("verify reported clean while the entity index still names an excluded session")
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Artifact == "entity_index" && finding.SessionID == "secret-sess" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no entity_index finding: %+v", report.Findings)
	}
}

// An unreadable index is state whose attribution cannot be inspected. Treating
// it as empty would let cleanup claim erasure from a file it never read.
func TestPrivacyVerifyFailsClosedOnACorruptEntityIndex(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, entitiesDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySessionPrivacy(brainDir); err == nil {
		t.Fatal("verification passed over an unreadable entity index")
	}
}

// A forward-version index may attribute sessions in a shape this binary cannot
// see; reading it as empty would publish Clean=true for a file whose linkage
// was never inspected.
func TestPrivacyVerifyFailsClosedOnAForwardVersionEntityIndex(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	writeEntityIndexForPrivacy(t, brainDir, "clean-sess")
	raw, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["schema_version"] = entityIndexCacheVersion + 1
	forward, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)), forward, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = verifySessionPrivacy(brainDir)
	if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("verification accepted a forward-version entity index: %v", err)
	}
}
