package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const brainBriefSemanticFreshnessFixture = `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["typescript"],"relation_set":["TESTS","CALLS"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"stale-root","kind":"function","name":"NormalizeQueryLimitLegacy","qualified_name":"a.NormalizeQueryLimitLegacy","file_path":"packages/store/legacy_query.ts","start_line":1,"end_line":8,"signature":"function NormalizeQueryLimitLegacy(limit: number)","language":"TypeScript","stable_id_version":"1"}
{"record_type":"symbol","id":"current-root","kind":"function","name":"NormalizeQueryLimit","qualified_name":"b.NormalizeQueryLimit","file_path":"packages/store/query.ts","start_line":1,"end_line":8,"signature":"function NormalizeQueryLimit(limit: number)","language":"TypeScript","stable_id_version":"1"}
{"record_type":"symbol","id":"stale-test","kind":"test","name":"TestNormalizeQueryLimitLegacy","qualified_name":"c.TestNormalizeQueryLimitLegacy","file_path":"packages/store/legacy_query.test.ts","start_line":1,"end_line":8,"signature":"function TestNormalizeQueryLimitLegacy()","language":"TypeScript","stable_id_version":"1"}
{"record_type":"symbol","id":"current-test","kind":"test","name":"TestNormalizeQueryLimit","qualified_name":"d.TestNormalizeQueryLimit","file_path":"packages/store/query.test.ts","start_line":1,"end_line":8,"signature":"function TestNormalizeQueryLimit()","language":"TypeScript","stable_id_version":"1"}
{"record_type":"symbol","id":"pathless-policy","kind":"external_contract","name":"NormalizeQueryLimitPolicy","qualified_name":"z.NormalizeQueryLimitPolicy","file_path":"","start_line":0,"end_line":0,"signature":"NormalizeQueryLimitPolicy","language":"","stable_id_version":"1"}
{"record_type":"relation","from_id":"stale-test","to_id":"stale-root","type":"TESTS","confidence":1,"relation_scope":"file","resolution":"exact","target_kind":"symbol","evidence":[{"kind":"test_target","file_path":"packages/store/legacy_query.test.ts","start_line":2,"end_line":2,"detail":"legacy test"}]}
{"record_type":"relation","from_id":"current-test","to_id":"current-root","type":"TESTS","confidence":1,"relation_scope":"file","resolution":"exact","target_kind":"symbol","evidence":[{"kind":"test_target","file_path":"packages/store/query.test.ts","start_line":2,"end_line":2,"detail":"current test"}]}
{"record_type":"relation","from_id":"current-root","to_id":"pathless-policy","type":"CALLS","confidence":0.8,"relation_scope":"external","resolution":"name_only","target_kind":"external"}
`

type brainBriefSemanticFreshnessFixtureEnv struct {
	repoDir string
	opts    Options
}

func newBrainBriefSemanticFreshnessFixture(t *testing.T) brainBriefSemanticFreshnessFixtureEnv {
	t.Helper()
	repoDir := t.TempDir()
	for rel, content := range map[string]string{
		"packages/store/query.ts": `listRows(limit: number) {
  return db.query("SELECT * FROM rows LIMIT ?", [limit]);
}
`,
		"packages/store/query.test.ts": "test('normalizes the query limit', () => {});\n",
	} {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, brainBriefSemanticFreshnessFixture)
	opts := Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
	}
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	// The index describes aaa111, while the clean worktree is now at bbb222.
	// The current files above remain; the two legacy semantic files departed.
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "bbb222\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	return brainBriefSemanticFreshnessFixtureEnv{repoDir: repoDir, opts: opts}
}

func renderBrainBriefSemanticFreshnessPacket(t *testing.T, fixture brainBriefSemanticFreshnessFixtureEnv, limit int) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrainBrief(context.Background(), cmd, fixture.opts, brainBriefOptions{
		limit:        limit,
		packetFormat: brainBriefPacketCompactV3,
	}, "normalize query limit")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestBrainBriefSemanticFreshnessOmitsDepartedRecordsFromActualPacket(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	packet := renderBrainBriefSemanticFreshnessPacket(t, fixture, 8)
	for _, stale := range []string{"packages/store/legacy_query.ts", "packages/store/legacy_query.test.ts", "stale-root", "stale-test"} {
		if strings.Contains(packet, stale) {
			t.Fatalf("departed semantic record %q leaked into packet:\n%s", stale, packet)
		}
	}
	for _, current := range []string{
		"packages/store/query.ts",
		"packages/store/query.test.ts",
		"current-root",
		"current-test",
		"pathless-policy",
		`action file="packages/store/query.ts" symbol="listRows"`,
	} {
		if !strings.Contains(packet, current) {
			t.Fatalf("current/pathless context %q missing from packet:\n%s", current, packet)
		}
	}
	if got := strings.Count(packet, "\naction "); got != 1 {
		t.Fatalf("current action rank/count = %d, want one rank-1 action:\n%s", got, packet)
	}
	const baselineBytes = 4165
	if len(packet) >= baselineBytes {
		t.Fatalf("freshness boundary did not reduce packet: candidate=%d baseline=%d", len(packet), baselineBytes)
	}
	t.Logf("compact_v3 packet bytes=%d vs %d; token proxy=%d vs %d", len(packet), baselineBytes, (len(packet)+3)/4, (baselineBytes+3)/4)
}

func TestBrainBriefSemanticFreshnessRefillsThenHonorsCaps(t *testing.T) {
	if got := brainBriefSemanticCandidateLimit(1, 5); got != 4 {
		t.Fatalf("candidate limit = %d, want 4", got)
	}
	maxCandidate := int(^uint(0)>>1) / 8
	if got := brainBriefSemanticCandidateLimit(maxCandidate+1, 0); got != maxCandidate {
		t.Fatalf("overflow-safe candidate limit = %d, want %d", got, maxCandidate)
	}
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	packet := renderBrainBriefSemanticFreshnessPacket(t, fixture, 1)
	if strings.Contains(packet, "stale-root") || strings.Contains(packet, "legacy_query") {
		t.Fatalf("departed top-ranked record leaked through capped packet:\n%s", packet)
	}
	if !strings.Contains(packet, "current-root") || !strings.Contains(packet, `action file="packages/store/query.ts"`) {
		t.Fatalf("current record hidden behind stale capped candidate:\n%s", packet)
	}
	if got := strings.Count(packet, "\ny\t") + strings.Count(packet, "\nsymbol "); got != 1 {
		t.Fatalf("symbol cap = %d, want 1:\n%s", got, packet)
	}
	if got := strings.Count(packet, "\no\t") + strings.Count(packet, "\ntest_root "); got != 1 {
		t.Fatalf("test-root cap = %d, want 1:\n%s", got, packet)
	}
	if got := strings.Count(packet, "\ntest_suggestion "); got > 1 {
		t.Fatalf("test suggestion cap = %d, want <= 1:\n%s", got, packet)
	}
	if strings.Contains(packet, `type="TESTS" from="current-test"`) {
		t.Fatalf("relation endpoint removed by the symbol/neighbor cap survived:\n%s", packet)
	}
}

func TestBrainBriefSemanticFreshnessPrecisionRecallAndRank(t *testing.T) {
	repoDir := t.TempDir()
	for _, rel := range []string{"packages/store/query.ts", "packages/store/query.test.ts"} {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("current\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	semantic := brainBriefSemanticFreshnessMetricInput()
	if semantic.Context.Symbols[1].ID != "current-root" {
		t.Fatalf("fixture current symbol rank changed: %+v", semantic.Context.Symbols)
	}
	stats := brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
	if stats.InputRecords != 16 || stats.KeptRecords != 10 || stats.RemovedRecords != 6 || stats.RemovedEvidence != 1 {
		t.Fatalf("unexpected boundary metrics: %+v", stats)
	}
	if len(semantic.Context.Symbols) == 0 || semantic.Context.Symbols[0].ID != "current-root" {
		t.Fatalf("current symbol did not improve from rank 2 to rank 1: %+v", semantic.Context.Symbols)
	}
	if !slices.ContainsFunc(semantic.Context.Symbols, func(record semanticRecord) bool { return record.ID == "pathless-policy" }) {
		t.Fatalf("legitimate pathless semantic record was removed: %+v", semantic.Context.Symbols)
	}
	if !slices.ContainsFunc(semantic.Context.Relations, func(record semanticRecord) bool {
		return record.FromID == "current-root" && record.ToID == "pathless-policy"
	}) {
		t.Fatalf("legitimate relation to pathless record was removed: %+v", semantic.Context.Relations)
	}
	if slices.ContainsFunc(semantic.Context.Relations, func(record semanticRecord) bool {
		return record.FromID == "stale-test" || record.ToID == "stale-root"
	}) {
		t.Fatalf("relation to a departed record survived: %+v", semantic.Context.Relations)
	}
	for _, relation := range semantic.Context.Relations {
		if len(relation.Evidence) != 0 {
			t.Fatalf("departed relation evidence survived: %+v", relation.Evidence)
		}
	}
	t.Log("current-record precision 62.5% -> 100%; current recall 100%; stale-removal precision/recall 100%; current symbol rank 2 -> 1")
}

func TestBrainBriefSemanticFreshnessKeepsLiveDeletionAndHistoryCreate(t *testing.T) {
	repoDir := t.TempDir()
	deleted := "packages/store/deleted_query.ts"
	planned := "packages/store/new_query.ts"
	report := brainBriefReport{
		Status: brainStatusReport{Live: brainLiveState{ChangedFiles: []string{deleted}}},
		Semantic: brainBriefSemantic{Context: semanticContextResult{Symbols: []semanticRecord{
			{ID: "live-delete", FilePath: deleted},
			{ID: "departed", FilePath: "packages/store/old_query.ts"},
		}}},
		History: brainBriefHistory{Matches: []brainTextMatch{{Excerpt: "Create ./" + planned + " for the replacement query."}}},
	}
	stats := brainBriefFilterDepartedSemantic(repoDir, report.Status.Live, &report.Semantic)
	if stats.KeptRecords != 1 || stats.RemovedRecords != 1 || len(report.Semantic.Context.Symbols) != 1 || report.Semantic.Context.Symbols[0].ID != "live-delete" {
		t.Fatalf("live deletion was not authoritative: stats=%+v symbols=%+v", stats, report.Semantic.Context.Symbols)
	}
	editFiles, _, _ := brainBriefLikelyFileGroups(repoDir, report, "restore the deleted query and create its replacement")
	if !slices.Contains(editFiles, deleted) || !slices.Contains(editFiles, planned) {
		t.Fatalf("live deletion/history intended-create context was erased: %+v", editFiles)
	}
}

func TestBrainBriefSemanticFreshnessRejectsOutsideSymlinkEvenWhenLive(t *testing.T) {
	repoDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "query.ts"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(repoDir, "packages", "store")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rel := "packages/store/query.ts"
	semantic := brainBriefSemantic{Context: semanticContextResult{Symbols: []semanticRecord{
		{ID: "outside", FilePath: rel},
		{ID: "pathless-policy"},
	}}}
	live := brainLiveState{ChangedFiles: []string{rel}}
	stats := brainBriefFilterDepartedSemantic(repoDir, live, &semantic)
	if stats.KeptRecords != 1 || stats.RemovedRecords != 1 || len(semantic.Context.Symbols) != 1 || semantic.Context.Symbols[0].ID != "pathless-policy" {
		t.Fatalf("outside symlink semantic path was trusted: stats=%+v symbols=%+v", stats, semantic.Context.Symbols)
	}
	if len(live.ChangedFiles) != 1 || live.ChangedFiles[0] != rel {
		t.Fatalf("live overlay was mutated: %+v", live)
	}

	// A missing leaf beneath that outside symlink is not a deleted in-repo file.
	// Lstat on the joined leaf alone returns IsNotExist, so this specifically
	// protects the existing-component containment check.
	missingRel := "packages/store/missing.ts"
	missing := brainBriefSemantic{Context: semanticContextResult{Symbols: []semanticRecord{{
		ID:       "outside-missing",
		FilePath: missingRel,
	}}}}
	missingStats := brainBriefFilterDepartedSemantic(repoDir, brainLiveState{ChangedFiles: []string{missingRel}}, &missing)
	if missingStats.RemovedRecords != 1 || len(missing.Context.Symbols) != 0 {
		t.Fatalf("missing leaf beneath outside symlink was trusted: stats=%+v symbols=%+v", missingStats, missing.Context.Symbols)
	}
}

func TestBrainBriefSemanticFreshnessDropsUnverifiableRelationEndpoints(t *testing.T) {
	repoDir := t.TempDir()
	currentPath := filepath.Join(repoDir, "packages", "store", "query.ts")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	semantic := brainBriefSemantic{Context: semanticContextResult{
		Symbols: []semanticRecord{{ID: "current-root", FilePath: "packages/store/query.ts"}},
		Relations: []semanticRecord{
			{FromID: "current-root", ToID: "unreturned-opaque-endpoint"},
			{FromID: "current-root", ToID: "external:service:rate-policy"},
		},
	}}
	stats := brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
	if stats.RemovedRecords != 1 || len(semantic.Context.Relations) != 1 {
		t.Fatalf("unverifiable relation endpoint survived: stats=%+v relations=%+v", stats, semantic.Context.Relations)
	}
	if semantic.Context.Relations[0].ToID != "external:service:rate-policy" {
		t.Fatalf("legitimate pathless external endpoint was removed: %+v", semantic.Context.Relations)
	}
}

func TestBrainBriefSemanticFreshnessRequiresEveryDeclaredLocus(t *testing.T) {
	repoDir := t.TempDir()
	currentPath := filepath.Join(repoDir, "packages", "store", "query.ts")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	semantic := brainBriefSemantic{Context: semanticContextResult{Symbols: []semanticRecord{{
		ID:       "mixed-locus",
		FilePath: "packages/store/query.ts",
		Path:     "packages/store/departed_alias.ts",
	}}}}
	stats := brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
	if stats.RemovedRecords != 1 || len(semantic.Context.Symbols) != 0 {
		t.Fatalf("current alias smuggled a departed locus: stats=%+v symbols=%+v", stats, semantic.Context.Symbols)
	}
}

func TestBrainBriefSemanticFreshnessRunsForPartialCoverage(t *testing.T) {
	status := brainStatusReport{Semantic: &brainStatusSemantic{Freshness: &staleReport{
		Severity: "degraded",
		Axes: map[string]staleAxis{
			"semantic_completeness": {State: "degraded", Detail: "partial coverage"},
		},
	}}}
	if !brainBriefSemanticNeedsCurrentBoundary(status) {
		t.Fatal("partial semantic coverage must cross the current-worktree boundary")
	}
	status.Semantic.Freshness.Severity = "ok"
	if brainBriefSemanticNeedsCurrentBoundary(status) {
		t.Fatal("fully current semantic data should avoid boundary overfetch")
	}
}

func brainBriefSemanticFreshnessMetricInput() brainBriefSemantic {
	staleRoot := semanticRecord{ID: "stale-root", FilePath: "packages/store/legacy_query.ts"}
	currentRoot := semanticRecord{ID: "current-root", FilePath: "packages/store/query.ts"}
	staleTest := semanticRecord{ID: "stale-test", FilePath: "packages/store/legacy_query.test.ts"}
	currentTest := semanticRecord{ID: "current-test", FilePath: "packages/store/query.test.ts"}
	pathless := semanticRecord{ID: "pathless-policy"}
	staleRelation := semanticRecord{FromID: "stale-test", ToID: "stale-root"}
	currentRelation := semanticRecord{FromID: "current-test", ToID: "current-root"}
	externalRelation := semanticRecord{FromID: "current-root", ToID: "pathless-policy"}
	staleEvidence := semanticRecord{
		FromID: "current-root",
		ToID:   "pathless-policy",
		Evidence: []semanticEvidence{{
			FilePath: "packages/store/legacy_query.ts",
		}},
	}
	return brainBriefSemantic{
		Context: semanticContextResult{
			Symbols:   []semanticRecord{staleRoot, currentRoot, staleTest, currentTest, pathless},
			Relations: []semanticRecord{staleRelation, currentRelation, externalRelation, staleEvidence},
		},
		Tests: semanticTestsResult{
			Roots: []semanticRecord{staleRoot, currentRoot, staleTest, currentTest, pathless},
			Suggestions: []semanticTestSuggestion{
				{Symbol: staleTest},
				{Symbol: currentTest},
			},
		},
	}
}
