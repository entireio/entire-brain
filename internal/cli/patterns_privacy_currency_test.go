package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func patternEpisodeLineCount(t *testing.T, brainDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(patternsEpisodesPath)))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// The patterns layer derives its freshness from its recorded inputs. The
// sessions fingerprint alone cannot see a privacy change -- exclude and include
// leave the canonical session set untouched -- so a corpus rewritten in place
// by the exclusion sweep reported the PRE-SWEEP episode count as "current",
// and a later `privacy include` never rebuilt the corpus at all, losing the
// re-included session's episodes permanently.
func TestPatternsCurrencyTracksThePrivacyEpoch(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)

	built, err := writeBrainEpisodesAndSource(brainDir, now)
	if err != nil {
		t.Fatalf("build episodes: %v", err)
	}
	if built.Episodes != patternEpisodeLineCount(t, brainDir) {
		t.Fatalf("manifest says %d episodes, file has %d", built.Episodes, patternEpisodeLineCount(t, brainDir))
	}
	baseline := built.Episodes
	if baseline == 0 {
		t.Fatal("fixture produced no episodes; the test would prove nothing")
	}
	if report, err := buildPatternsStatusReportChecked(brainDir); err != nil || report.Freshness != "current" {
		t.Fatalf("freshly built corpus: freshness=%q err=%v", report.Freshness, err)
	}

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	excluded := patternEpisodeLineCount(t, brainDir)
	if excluded >= baseline {
		t.Fatalf("exclusion did not remove any episode (%d -> %d)", baseline, excluded)
	}
	report, err := buildPatternsStatusReportChecked(brainDir)
	if err != nil {
		t.Fatalf("status after exclude: %v", err)
	}
	if report.Freshness != "stale" {
		t.Fatalf("after exclusion the corpus is %q while the manifest still claims %d episodes for a file holding %d",
			report.Freshness, report.Episodes, excluded)
	}

	// A refresh reconciles the manifest with the corpus the sweep left behind.
	rebuilt, err := refreshPatternLayer(brainDir, false, now)
	if err != nil {
		t.Fatalf("refresh pattern layer: %v", err)
	}
	if !rebuilt {
		t.Fatal("refresh skipped the pattern layer after a privacy change")
	}
	report, err = buildPatternsStatusReportChecked(brainDir)
	if err != nil {
		t.Fatalf("status after rebuild: %v", err)
	}
	if report.Freshness != "current" || report.Episodes != patternEpisodeLineCount(t, brainDir) {
		t.Fatalf("after rebuild: freshness=%q episodes=%d file=%d",
			report.Freshness, report.Episodes, patternEpisodeLineCount(t, brainDir))
	}

	// Re-including the session is the other half: the corpus is missing its
	// episodes and nothing but the privacy epoch says so.
	if err := includeSessionLocked(brainDir, "secret-sess", now); err != nil {
		t.Fatalf("include: %v", err)
	}
	report, err = buildPatternsStatusReportChecked(brainDir)
	if err != nil {
		t.Fatalf("status after include: %v", err)
	}
	if report.Freshness != "stale" {
		t.Fatalf("after include the corpus is reported %q, so no refresh ever restores the re-included session", report.Freshness)
	}
	if _, err := refreshPatternLayer(brainDir, false, now); err != nil {
		t.Fatalf("refresh after include: %v", err)
	}
	if got := patternEpisodeLineCount(t, brainDir); got != baseline {
		t.Fatalf("re-included session not restored to the corpus: %d episodes, want %d", got, baseline)
	}
}

// An existing corpus built before privacy_identity existed must not be declared
// stale just because the field is new -- only once a tombstone exists, which is
// exactly when the recorded corpus can no longer be trusted.
func TestPatternSourceWithoutPrivacyIdentityStaysCurrentUntilATombstoneExists(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	if _, err := writeBrainEpisodesAndSource(brainDir, now); err != nil {
		t.Fatalf("build episodes: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Patterns.PrivacyIdentity = "" // as a pre-field binary wrote it
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if !patternSourceCurrent(manifest.Sources.Patterns, brainDir) {
		t.Fatal("a legacy patterns source on a tombstone-free brain must stay current")
	}

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	after, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	after.Sources.Patterns.PrivacyIdentity = ""
	if patternSourceCurrent(after.Sources.Patterns, brainDir) {
		t.Fatal("a corpus that cannot name the tombstone epoch it filtered against is not current once a tombstone exists")
	}
}
