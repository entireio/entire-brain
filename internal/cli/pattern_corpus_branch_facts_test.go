package cli

import (
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
	"strings"
	"testing"
	"time"
)

// Fact freshness is per branch. This was one fingerprint over every branch's
// facts, so a `remember` anywhere re-indexed every session in the corpus --
// inside the brain write lock, which is the same lock a retrieval holds to
// emit its response. That is what made a concurrent `brief` fail brain_locked.
//
// The link builder is what makes per-branch sound rather than merely cheaper:
// episodes are linked from factsByBranch[s.Branch], so a fact on branch X can
// never enrich a session on branch Y.

func patternCorpusIndexedSHAs(t *testing.T, brainDir string) map[string]string {
	t.Helper()
	db, err := patterntest.OpenCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, content_sha FROM indexed_sessions`)
	if err != nil {
		t.Fatalf("query indexed_sessions: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, sha string
		if err := rows.Scan(&id, &sha); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = sha
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func twoBranchCorpusFixture(t *testing.T, now time.Time) string {
	t.Helper()
	return writeEpisodeFixture(t, now, "gh/acme/branchfacts", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "c1",
			relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript},
		{id: "s2", branch: "feature", agent: "Codex", checkpoint: "c2",
			relPath: "sessions/branches/feature/s2.jsonl", author: "Ada", transcript: claudeReviewTranscript},
	})
}

func seedBranchFact(t *testing.T, brainDir, branch, id string, now time.Time) {
	t.Helper()
	if err := factstore.Write(brainDir, branch, []factRecord{{
		ID: id, Paths: []string{"preferences.style.x"}, Text: "The user prefers table-driven tests.",
		Branch: branch, Origin: "distilled", Status: "active", UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("write facts on %s: %v", branch, err)
	}
}

// recordIndexedSessions installs the test seam and returns the ids the next
// build actually re-indexed. Counting the work is the only way to tell a skip
// from a re-index: see onPatternCorpusSessionIndexed for why the columns
// cannot.
func recordIndexedSessions(t *testing.T) *[]string {
	t.Helper()
	var indexed []string
	original := onPatternCorpusSessionIndexed
	onPatternCorpusSessionIndexed = func(id string) { indexed = append(indexed, id) }
	t.Cleanup(func() { onPatternCorpusSessionIndexed = original })
	return &indexed
}

func namesSession(ids []string, want string) bool {
	for _, id := range ids {
		if strings.Contains(id, want) {
			return true
		}
	}
	return false
}

// TestFactOnOneBranchDoesNotReindexAnotherBranchsSessions is the guard for the
// whole change: a fact on `feature` must not re-index main's session.
func TestFactOnOneBranchDoesNotReindexAnotherBranchsSessions(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := twoBranchCorpusFixture(t, now)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}

	indexed := recordIndexedSessions(t)
	seedBranchFact(t, brainDir, "feature", "fact:feature-only", now.Add(time.Minute))
	if err := buildPatternCorpus(brainDir, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if namesSession(*indexed, "sessions/main/s1.jsonl") {
		t.Fatalf("a fact on `feature` re-indexed main's session; re-indexed: %v", *indexed)
	}
	// And the other half, in the same run, so the fix cannot be "skip
	// everything": feature's own session MUST be re-indexed.
	if !namesSession(*indexed, "sessions/branches/feature/s2.jsonl") {
		t.Fatalf("a fact on `feature` did NOT re-index feature's own session; re-indexed: %v", *indexed)
	}
}

// TestUnchangedInputsReindexNothing: with no input moving at all, a second
// build must do no per-session work. This is the 18.7s-per-tick cost that made
// a concurrent brief fail brain_locked.
func TestUnchangedInputsReindexNothing(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := twoBranchCorpusFixture(t, now)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	indexed := recordIndexedSessions(t)
	if err := buildPatternCorpus(brainDir, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*indexed) != 0 {
		t.Fatalf("a build with no changed input re-indexed %d session(s): %v", len(*indexed), *indexed)
	}
}

// TestFactOnABranchStillReindexesThatBranchsSessions keeps the fix honest from
// the other direction, including for a branch that had NO facts before.
func TestFactOnABranchStillReindexesThatBranchsSessions(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := twoBranchCorpusFixture(t, now)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	indexed := recordIndexedSessions(t)
	// main had no facts at all in the first build, so this also covers the
	// factless-branch case: its fingerprint must have been recorded, or it
	// could never be invalidated.
	seedBranchFact(t, brainDir, "main", "fact:main-only", now.Add(time.Minute))
	if err := buildPatternCorpus(brainDir, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !namesSession(*indexed, "sessions/main/s1.jsonl") {
		t.Fatalf("a fact written to a previously factless branch did not re-index its session; re-indexed: %v", *indexed)
	}
	if namesSession(*indexed, "sessions/branches/feature/s2.jsonl") {
		t.Fatalf("a fact on `main` re-indexed feature's session; re-indexed: %v", *indexed)
	}
}

// TestPatternCorpusFactsMetaKeyCannotCollide: branch names contain slashes and
// arbitrary bytes, and the meta table is a flat key space shared with keys like
// "facts_fingerprint". An interpolated branch name would collide.
func TestPatternCorpusFactsMetaKeyCannotCollide(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, branch := range []string{
		"main", "feature", "feature/x", "feature/x/y", "", "facts_fingerprint",
		"facts_fingerprint/main", "../escape", "a b", "日本語",
	} {
		key := patternCorpusFactsMetaKey(branch)
		if prior, dup := seen[key]; dup {
			t.Fatalf("branches %q and %q share the meta key %q", prior, branch, key)
		}
		seen[key] = branch
		for _, reserved := range []string{"schema_version", "pattern_indexer_version", "facts_fingerprint", "repo_key", "sessions_fingerprint", "privacy_policy_identity", "generated_at"} {
			if key == reserved {
				t.Fatalf("branch %q produced the reserved meta key %q", branch, reserved)
			}
		}
	}
}
