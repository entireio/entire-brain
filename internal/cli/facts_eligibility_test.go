package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestFilterFactsByTemporalEligibilityFailsClosedAndCountsReasons(t *testing.T) {
	anchor := func(ids ...string) []factAnchor {
		out := make([]factAnchor, 0, len(ids))
		for _, id := range ids {
			out = append(out, factAnchor{SessionID: id})
		}
		return out
	}
	facts := []factRecord{
		{ID: "eligible-active", Status: factStatusActive, Provenance: anchor("early", "offset-early")},
		{ID: "eligible-superseded", Status: factStatusSuperseded, Provenance: anchor("early")},
		{ID: "empty"},
		{ID: "missing-anchor-id", Provenance: anchor("")},
		// Exclusion reason precedence is stable even when an unknown anchor appears first.
		{ID: "excluded", Provenance: anchor("not-in-map", "own-fix")},
		{ID: "unknown", Provenance: anchor("not-in-map")},
		{ID: "boundary", Provenance: anchor("at-cutoff")},
		{ID: "future", Provenance: anchor("late")},
	}
	dates := map[string]string{
		"early":        "2026-07-14T23:00:00Z",
		"offset-early": "2026-07-15T00:30:00+02:00",
		"own-fix":      "2026-07-14T20:00:00Z",
		"at-cutoff":    "2026-07-15T00:00:00Z",
		"late":         "2026-07-15T00:00:01Z",
	}
	got, audit, err := filterFactsByTemporalEligibility(facts, dates, "2026-07-15T00:00:00Z", []string{"own-fix"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "eligible-active" || got[1].ID != "eligible-superseded" {
		t.Fatalf("eligible facts = %+v", got)
	}
	if audit.PrefilterCorpusCount != 8 || audit.EligibleCount != 2 {
		t.Fatalf("audit counts = %+v", audit)
	}
	wantReasons := map[string]int{
		eligibilityEmptyProvenance: 2,
		eligibilityExcludedSession: 1,
		eligibilityUnknownSession:  1,
		eligibilityAtOrAfterCutoff: 2,
	}
	for reason, want := range wantReasons {
		if audit.ExcludedCounts[reason] != want {
			t.Errorf("excluded %s = %d, want %d", reason, audit.ExcludedCounts[reason], want)
		}
	}
}

func TestRecallTemporalEligibilityPrecedesTopKForLexicalAndSemantic(t *testing.T) {
	repoDir := t.TempDir()
	dataDir := t.TempDir()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env:     EntireEnv{RepoRoot: repoDir, PluginDataDir: dataDir},
		Runner:  runner,
		Now:     func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	facts := make([]factRecord, 0, 6)
	for i := 0; i < 5; i++ {
		text := "target regression exact match future distractor " + string(rune('a'+i))
		facts = append(facts, factRecord{
			ID: factRecordID(text, paths), Paths: paths, Text: text, Branch: "main",
			Status: factStatusActive, UpdatedAt: now.Add(time.Duration(i) * time.Minute),
			Provenance: []factAnchor{{SessionID: "future"}},
		})
	}
	solvingText := "target regression eligible solving fact"
	solvingID := factRecordID(solvingText, paths)
	facts = append(facts, factRecord{
		ID: solvingID, Paths: paths, Text: solvingText, Branch: "main",
		Status: factStatusActive, UpdatedAt: now.Add(-time.Hour),
		Provenance: []factAnchor{{SessionID: "eligible"}},
	})
	if err := writeFacts(storage.BrainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	datesPath := filepath.Join(t.TempDir(), "session-dates.json")
	dates, _ := json.Marshal(map[string]string{
		"eligible": "2026-07-14T00:00:00Z",
		"future":   "2026-07-16T00:00:00Z",
	})
	if err := os.WriteFile(datesPath, dates, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		noSemantic bool
	}{
		{name: "lexical", noSemantic: true},
		{name: "semantic", noSemantic: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"recall", "target regression", "--k", "5", "--json", "--eligible-before", "2026-07-15T00:00:00Z", "--session-dates", datesPath, "--read-only-semantic-cache"}
			if tc.noSemantic {
				args = append(args, "--no-semantic")
			}
			before := treeContentHash(t, storage.BrainDir)
			out, execErr := execute(t, NewRootCommand(opts), args...)
			if execErr != nil {
				t.Fatalf("recall: %v\n%s", execErr, out)
			}
			var payload struct {
				Facts       []factRecord         `json:"facts"`
				Eligibility factEligibilityAudit `json:"eligibility"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("parse output: %v\n%s", err, out)
			}
			if len(payload.Facts) != 1 || payload.Facts[0].ID != solvingID {
				t.Fatalf("facts = %+v, want solving fact", payload.Facts)
			}
			if payload.Eligibility.PrefilterCorpusCount != 6 || payload.Eligibility.EligibleCount != 1 || payload.Eligibility.DeliveredCount != 1 {
				t.Fatalf("eligibility = %+v", payload.Eligibility)
			}
			if payload.Eligibility.ExcludedCounts[eligibilityAtOrAfterCutoff] != 5 {
				t.Fatalf("future exclusions = %+v", payload.Eligibility.ExcludedCounts)
			}
			if after := treeContentHash(t, storage.BrainDir); after != before {
				t.Fatalf("read-only recall mutated frozen brain: before %s after %s", before, after)
			}
		})
	}

	// An eligible corpus with no lexical match still reports complete accounting.
	out, err := execute(t, NewRootCommand(opts), "recall", "zzzz-no-match", "--k", "5", "--json", "--no-semantic", "--eligible-before", "2026-07-15T00:00:00Z", "--session-dates", datesPath, "--read-only-semantic-cache")
	if err != nil {
		t.Fatalf("no-match recall: %v\n%s", err, out)
	}
	var noMatch struct {
		Facts       []factRecord         `json:"facts"`
		Eligibility factEligibilityAudit `json:"eligibility"`
	}
	if err := json.Unmarshal([]byte(out), &noMatch); err != nil {
		t.Fatal(err)
	}
	if len(noMatch.Facts) != 0 || noMatch.Eligibility.EligibleCount != 1 || noMatch.Eligibility.DeliveredCount != 0 {
		t.Fatalf("no-match payload = %+v", noMatch)
	}
}

func treeContentHash(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, path)
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
