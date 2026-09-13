package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// coverageFixture builds a history-coverage record whose per-commit gap list has
// n entries the size real ones are: a full hash, two parents, an author, a
// subject and a body excerpt. Those bytes are the whole point -- a bound that
// holds for toy rows and not for real ones is not a bound.
func coverageFixture(n int) *seedHistoryCoverage {
	coverage := &seedHistoryCoverage{
		Path:          "seed/history-gaps.md",
		TotalCommits:  n,
		GeneratedFrom: "git log",
	}
	for i := 0; i < n; i++ {
		coverage.NoSessionHistoryCommits++
		coverage.UncoveredCommits = append(coverage.UncoveredCommits, seedCoveredCommit{
			Hash:        fmt.Sprintf("%040x", i),
			CommittedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			AuthorName:  "A Long Enough Author Name",
			AuthorEmail: "author.name@example.com",
			Subject:     "churn: adjust the widget retry budget for the transport layer",
			Coverage:    "no_session_history",
			Parents:     []string{fmt.Sprintf("%040x", i-1), fmt.Sprintf("%040x", i+1)},
			BodyExcerpt: "Decision: bump the deadline because the previous value was too tight under load.",
		})
	}
	return coverage
}

// TestSeedHistoryCoverageInManifestStaysUnderTheReadBound is the regression for
// the store that bricked itself. buildSeedHistoryCoverage appends one record per
// commit with no Entire checkpoint trailer -- that is every commit of every
// repository that predates Entire -- and the whole list went into the manifest.
// At 50,000 commits that manifest was 28 MB against a 16 MiB read bound, so the
// next command could not read what the previous one had just written.
func TestSeedHistoryCoverageInManifestStaysUnderTheReadBound(t *testing.T) {
	full := coverageFixture(50000)

	unbounded, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if int64(len(unbounded)) <= maxManifestBytes {
		t.Fatalf("fixture is too small to exercise the bound: %d bytes", len(unbounded))
	}

	bounded := manifestSeedHistoryCoverage(full)
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatalf("marshal bounded: %v", err)
	}
	if int64(len(encoded)) > maxManifestBytes {
		t.Fatalf("bounded coverage is still over the manifest read bound: %d bytes", len(encoded))
	}
	if got := len(bounded.UncoveredCommits); got != seedManifestMaxUncoveredCommits {
		t.Fatalf("uncovered commits = %d, want %d", got, seedManifestMaxUncoveredCommits)
	}

	// Bounded is not the same as quiet: the record must say it is a sample and
	// how large the real set was, or a reader counting rows learns a number
	// smaller than the truth and has no way to know it.
	if !bounded.UncoveredCommitsTruncated {
		t.Fatal("truncated list did not set uncovered_commits_truncated")
	}
	if bounded.UncoveredCommitsTotal != 50000 {
		t.Fatalf("uncovered_commits_total = %d, want 50000", bounded.UncoveredCommitsTotal)
	}
	// The counts are totals over every commit and must survive untouched.
	if bounded.NoSessionHistoryCommits != 50000 || bounded.TotalCommits != 50000 {
		t.Fatalf("counts changed: %+v", bounded)
	}

	// seed/history-gaps.md is rendered from the caller's record and is supposed
	// to be complete, so bounding the manifest copy must not reach back into it.
	if len(full.UncoveredCommits) != 50000 || full.UncoveredCommitsTruncated {
		t.Fatalf("bounding mutated the source record: %d rows, truncated=%v", len(full.UncoveredCommits), full.UncoveredCommitsTruncated)
	}
}

// A coverage record that already fits is passed through unchanged, markers and
// all, so an ordinary repository's manifest is byte-identical to what earlier
// builds wrote.
func TestSeedHistoryCoverageUnderTheBoundIsUnchanged(t *testing.T) {
	small := coverageFixture(10)
	bounded := manifestSeedHistoryCoverage(small)
	if bounded != small {
		t.Fatal("a record under the bound should be returned as-is")
	}
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, marker := range []string{"uncovered_commits_total", "uncovered_commits_truncated"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("untruncated record carries %q:\n%s", marker, encoded)
		}
	}
	if manifestSeedHistoryCoverage(nil) != nil {
		t.Fatal("nil coverage must stay nil")
	}
}

// TestSeedOnAManyCommitRepositoryLeavesAReadableManifest is the end-to-end
// regression, and the one that pins the CALL SITE rather than the helper: a
// bounded projection that nothing uses bounds nothing. It drives the real seed
// command over a repository with 50,000 commits that carry no Entire checkpoint
// trailer -- the ordinary state of any repository that predates Entire -- and
// asserts the only property that matters afterwards: the brain can still read
// what it just wrote.
func TestSeedOnAManyCommitRepositoryLeavesAReadableManifest(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "log", "--reverse", "--format=%H%x00%P%x00%aI%x00%an%x00%ae%x00%B%x1e")] = fakeCommandResponse{
		stdout: fakeSeedGitLog(50000),
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})

	out, err := execute(t, cmd, "refresh", "seed", repoDir)
	if err != nil {
		t.Fatalf("seed on a 50,000-commit repository failed: %v\n%s", err, out)
	}

	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
	info, err := os.Stat(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if info.Size() > maxManifestBytes {
		t.Fatalf("seed wrote a %d byte manifest against a %d byte read bound", info.Size(), maxManifestBytes)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("the brain cannot read the manifest it just wrote: %v", err)
	}

	coverage := manifest.Sources.Seed.HistoryCoverage
	if coverage == nil {
		t.Fatal("history coverage missing from manifest")
	}
	// Bounded, and saying so. The totals stay exact.
	if !coverage.UncoveredCommitsTruncated || coverage.UncoveredCommitsTotal != 50000 {
		t.Fatalf("manifest coverage does not admit it is a sample: %+v", coverage.UncoveredCommits[:0])
	}
	if coverage.TotalCommits != 50000 || coverage.NoSessionHistoryCommits != 50000 {
		t.Fatalf("counts are not totals: total=%d uncovered=%d", coverage.TotalCommits, coverage.NoSessionHistoryCommits)
	}
	// And the user is told, on the command that did it.
	if !strings.Contains(out, "manifest lists 500 of 50000 uncovered commits") {
		t.Fatalf("seed did not report that the manifest list was sampled:\n%s", out)
	}

	// The full list still has a home: seed/history-gaps.md is rendered from the
	// unbounded record and must not have been trimmed with the manifest.
	gaps, err := os.ReadFile(filepath.Join(brainDir, seedDirName, "history-gaps.md"))
	if err != nil {
		t.Fatalf("read history-gaps.md: %v", err)
	}
	if !strings.Contains(string(gaps), shortCommitHash(fmt.Sprintf("%040x", 49999))) {
		t.Fatal("history-gaps.md lost the commits the manifest sampled away")
	}
}

// fakeSeedGitLog renders n commits in the exact record format
// buildSeedHistoryCoverage asks git for, with realistic field widths.
func fakeSeedGitLog(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%040x%s%040x%s%s%sA Long Enough Author Name%sauthor.name@example.com%schurn: adjust the widget retry budget for the transport layer\n\nDecision: bump the deadline because the previous value was too tight under load.%s",
			i, gitLogFieldSeparator,
			i-1, gitLogFieldSeparator,
			time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Minute).Format(time.RFC3339), gitLogFieldSeparator,
			gitLogFieldSeparator, gitLogFieldSeparator,
			gitLogRecordSeparator)
	}
	return b.String()
}

// TestWriteBrainManifestRefusesWhatItCannotReadBack pins the writer/reader
// symmetry. readBrainManifest has always capped the manifest at
// maxManifestBytes; the writer had no cap, so an oversized record was written
// successfully and every later read -- status and doctor included -- failed. The
// write must fail instead, and the manifest that was readable a moment ago must
// still be there afterwards.
func TestWriteBrainManifestRefusesWhatItCannotReadBack(t *testing.T) {
	brainDir := t.TempDir()
	good := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       "local/example",
		Sources:       &brainSources{},
	}
	if err := writeBrainManifestAndReadme(brainDir, good); err != nil {
		t.Fatalf("write baseline manifest: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}

	oversized := good
	oversized.Sources = &brainSources{Seed: &seedSourceManifest{
		GeneratedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		HistoryCoverage: coverageFixture(50000),
	}}
	err = writeBrainManifestAndReadme(brainDir, oversized)
	if err == nil {
		t.Fatal("writing a manifest larger than the read bound must fail")
	}
	var bound *readBoundExceededError
	if !errors.As(err, &bound) {
		t.Fatalf("want a read-bound error naming the size, got %v", err)
	}
	// An error the caller cannot act on is only half a refusal.
	if !strings.Contains(err.Error(), "previous manifest") {
		t.Fatalf("refusal does not say the previous manifest survived: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read after refusal: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("refused write replaced the readable manifest")
	}
	// The real assertion: the brain still works.
	if _, err := loadBrainManifest(brainDir); err != nil {
		t.Fatalf("manifest unreadable after a refused write: %v", err)
	}
}

// TestSeedPrintsEveryWarningNotJustACount pins the one line that tells a user a
// large repository was only partly read. `refresh seed` printed "warnings: 1"
// and stopped, so the whole report of a brain that skipped most of the
// repository was a digit. `refresh sessions` has always printed the text too.
func TestSeedPrintsEveryWarningNotJustACount(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: seedFixtureRunner(repoDir),
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})

	// The fixture lists 11 files; a cap of 3 drops 8 of them.
	out, err := execute(t, cmd, "refresh", "seed", repoDir, "--max-files", "3")
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warnings: 1") {
		t.Fatalf("seed did not report a warning count:\n%s", out)
	}
	// The text, not just the tally.
	if !strings.Contains(out, "warning: file scan capped at 3 of 11 files") {
		t.Fatalf("seed did not print the warning text:\n%s", out)
	}
	// And the number that decides whether the cap matters: how many files were
	// never read. "capped at 3 files" is equally true of a 4-file repository.
	if !strings.Contains(out, "8 files were not read") {
		t.Fatalf("seed did not say how many files it skipped:\n%s", out)
	}
	if !strings.Contains(out, "--max-files") {
		t.Fatalf("seed did not name the flag that lifts the cap:\n%s", out)
	}
}
