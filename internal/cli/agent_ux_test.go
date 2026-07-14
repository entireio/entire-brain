package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// indexFixtureBrain indexes a snapshot into a fresh brain and returns the brain
// directory, the SQLite store path, and the wired Options so tests can exercise
// the read-time query surface end to end.
func indexFixtureBrain(t *testing.T, snapshot string) (string, string, Options) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, snapshot)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	source := mustSemanticSource(t, env)
	storePath := filepath.Join(brainDir, filepath.FromSlash(source.StorePath))
	return brainDir, storePath, opts
}

// idfRankingSnapshot builds a store where a rare token ("zebra") appears in one
// symbol and two common tokens appear in many. Plain hit-counting would rank a
// two-common-token symbol above the single rare-token symbol; IDF weighting must
// invert that.
func idfRankingSnapshot() string {
	var b strings.Builder
	b.WriteString(`{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}` + "\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, `{"record_type":"symbol","id":"common-%d","kind":"function","name":"CommonThing%d","qualified_name":"pkg.CommonThing%d","file_path":"pkg/common%d.go","start_line":1,"end_line":2,"signature":"common1 common2 helper","language":"Go","stable_id_version":"1"}`+"\n", i, i, i, i)
	}
	b.WriteString(`{"record_type":"symbol","id":"rare-zebra","kind":"function","name":"ZebraThing","qualified_name":"pkg.ZebraThing","file_path":"pkg/zebra.go","start_line":1,"end_line":2,"signature":"zebra unique","language":"Go","stable_id_version":"1"}` + "\n")
	return b.String()
}

func TestTokenizedSearchRanksRareTokenAboveCommonTokens(t *testing.T) {
	_, storePath, _ := indexFixtureBrain(t, idfRankingSnapshot())
	// Multi-word query that matches no single symbol verbatim, so the tokenized
	// IDF fallback decides the ordering.
	results, err := findSemanticSymbolsInSQLite(storePath, "zebra common1 common2", 20, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected tokenized fallback to return results")
	}
	if results[0].ID != "rare-zebra" {
		t.Fatalf("expected rare-token symbol first, got %q (score %d); full order: %s", results[0].ID, results[0].Score, summarizeIDs(results))
	}
	if results[0].Score <= 0 {
		t.Fatalf("expected positive relevance score on rare match, got %d", results[0].Score)
	}
}

func TestSearchResolvesByExactRecordID(t *testing.T) {
	_, storePath, _ := indexFixtureBrain(t, semanticFixtureSnapshot("1.0"))
	id := "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"
	results, err := findSemanticSymbolsInSQLite(storePath, id, 10, 0)
	if err != nil {
		t.Fatalf("search by id: %v", err)
	}
	if len(results) != 1 || results[0].ID != id {
		t.Fatalf("expected exact id resolution, got %s", summarizeIDs(results))
	}
}

func TestSearchPathQueryDoesNotTriggerTokenFallback(t *testing.T) {
	_, storePath, _ := indexFixtureBrain(t, semanticFixtureSnapshot("1.0"))
	// A path that does not exist must resolve to nothing, not match half the
	// index on a common token like "file".
	results, err := findSemanticSymbolsInSQLite(storePath, "nonexistent/file.go", 10, 0)
	if err != nil {
		t.Fatalf("search nonexistent path: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for nonexistent path, got %s", summarizeIDs(results))
	}
	// A real path still resolves via substring match on file_path.
	hit, err := findSemanticSymbolsInSQLite(storePath, "internal/auth/token.go", 10, 0)
	if err != nil {
		t.Fatalf("search real path: %v", err)
	}
	if len(hit) != 1 || hit[0].Name != "ValidateToken" {
		t.Fatalf("expected real path to resolve, got %s", summarizeIDs(hit))
	}
}

func TestInspectCodeEmptyResultsEmitArrayNotNull(t *testing.T) {
	_, _, opts := indexFixtureBrain(t, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "inspect", "code", "zzqqxxnomatch", "--json")
	if err != nil {
		t.Fatalf("inspect code: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"results": []`) {
		t.Fatalf("expected empty results to serialize as [], got:\n%s", out)
	}
	if strings.Contains(out, `"results": null`) {
		t.Fatalf("results must not be null:\n%s", out)
	}
}

func TestInspectImpactSurfacesNeighborsBeyondRoots(t *testing.T) {
	brainDir, _, _ := indexFixtureBrain(t, semanticFixtureSnapshot("1.0"))
	source, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// ValidateToken has one caller via a CALLS relation; with the generous
	// default budget the impacted set must include more than the root itself.
	roots, symbols, relations, err := semanticImpactFacts(brainDir, source.Sources.Semantic, "ValidateToken", 1, 200)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if len(roots) == 0 || len(relations) == 0 {
		t.Fatalf("expected roots and relations, got roots=%d relations=%d", len(roots), len(relations))
	}
	if len(symbols) < len(roots) {
		t.Fatalf("impact symbols (%d) should include at least the roots (%d)", len(symbols), len(roots))
	}
}

func TestTokenIDFWeightFavorsRareTokens(t *testing.T) {
	rare := tokenIDFWeight(1000, 1)
	common := tokenIDFWeight(1000, 800)
	if rare <= common {
		t.Fatalf("expected rare token weight (%d) > common token weight (%d)", rare, common)
	}
	if tokenIDFWeight(1000, 1000) < 1 {
		t.Fatal("weight must never drop below 1")
	}
}

func TestSemanticQueryLooksLikePath(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"apps/web/src/lib/feed.ts", true},
		{"feed.ts", true},
		{"internal\\auth\\token.go", true},
		{"why did we choose supabase", false},
		{"ValidateToken", false},
		{"feed refresh worker", false},
	} {
		if got := semanticQueryLooksLikePath(tc.query); got != tc.want {
			t.Errorf("semanticQueryLooksLikePath(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestSemanticQueryTokensDropsStopwordsAndShortTerms(t *testing.T) {
	tokens := semanticQueryTokens("why did we refresh the feed worker at scale")
	joined := strings.Join(tokens, ",")
	for _, dropped := range []string{"why", "did", "we", "the", "at"} {
		if containsToken(tokens, dropped) {
			t.Errorf("expected stopword %q to be dropped, got [%s]", dropped, joined)
		}
	}
	for _, kept := range []string{"refresh", "feed", "worker", "scale"} {
		if !containsToken(tokens, kept) {
			t.Errorf("expected content token %q to be kept, got [%s]", kept, joined)
		}
	}
}

// TestQueryStopwordRegimesShareGenericBase locks in the reconciliation of the
// two token regimes: history/semantic search (historyQueryStopword) and
// brain-brief filename matching (brainBriefFileMatchTermStop) consult ONE shared
// generic-stopword set, but each keeps its own domain layer. It guards against
// the lists silently drifting apart again, and against the divergence being
// "fixed" into a single list (which would regress one regime).
func TestQueryStopwordRegimesShareGenericBase(t *testing.T) {
	// Generic fillers must be stopwords for BOTH regimes (shared source of truth).
	for _, w := range []string{"the", "and", "with", "fix", "should", "when"} {
		if !genericQueryStopwords[w] {
			t.Errorf("%q must be in the shared genericQueryStopwords set", w)
		}
		if !historyQueryStopword(w) {
			t.Errorf("history regime must treat generic %q as a stopword", w)
		}
		if !brainBriefFileMatchTermStop(w) {
			t.Errorf("brief regime must treat generic %q as a stopword", w)
		}
	}
	// Intentional divergence: domain nouns are noise for free-text history
	// search but are valid filename-match terms for the brief (e.g.
	// "regression" must still match regression.go).
	for _, w := range []string{"regression", "preserve", "restore"} {
		if !historyQueryStopword(w) {
			t.Errorf("history regime should drop domain word %q", w)
		}
		if brainBriefFileMatchTermStop(w) {
			t.Errorf("brief regime must KEEP domain word %q as a filename term", w)
		}
		if genericQueryStopwords[w] {
			t.Errorf("domain word %q must not leak into the shared generic set", w)
		}
	}
}

// TestBrainBriefFileMatchTermsExtractsNonLatin asserts the brief tokenizer is
// Unicode-aware: a non-Latin or accented task still yields content terms (the
// English stopword list just doesn't match them, so they pass through) rather
// than the old [a-z0-9] regex silently extracting nothing.
func TestBrainBriefFileMatchTermsExtractsNonLatin(t *testing.T) {
	// Accented Latin: "café" must survive whole, not be truncated to "caf".
	if terms := brainBriefFileMatchTerms("rework the café authentication flow"); !containsToken(terms, "café") {
		t.Errorf("accented term dropped/truncated: %v", terms)
	}
	// Non-Latin scripts (Japanese, Cyrillic) must extract terms, not vanish.
	if terms := brainBriefFileMatchTerms("認証 トークン の検証"); len(terms) == 0 {
		t.Error("Japanese task extracted zero terms (tokenizer not Unicode-aware)")
	}
	if terms := brainBriefFileMatchTerms("исправить проверку токена"); len(terms) == 0 {
		t.Error("Cyrillic task extracted zero terms (tokenizer not Unicode-aware)")
	}
	// ASCII behavior is unchanged: English stopwords still dropped, content kept.
	terms := brainBriefFileMatchTerms("update the auth token validator")
	if containsToken(terms, "the") || containsToken(terms, "update") {
		t.Errorf("English stopwords leaked: %v", terms)
	}
	if !containsToken(terms, "auth") || !containsToken(terms, "validator") {
		t.Errorf("English content terms dropped: %v", terms)
	}
}

func TestHistoryRecordTimestampParsesSessionPath(t *testing.T) {
	ts, ok := historyRecordTimestamp("sessions/main/20260606T070424Z_codex_abc.jsonl")
	if !ok {
		t.Fatal("expected timestamp to parse from session path")
	}
	if got := ts.Format(time.RFC3339); got != "2026-06-06T07:04:24Z" {
		t.Fatalf("timestamp = %s, want 2026-06-06T07:04:24Z", got)
	}
	if _, ok := historyRecordTimestamp("manifest.json"); ok {
		t.Fatal("expected non-session path to have no timestamp")
	}
}

func TestHistoryRecordMatchedTermsExplainsMatch(t *testing.T) {
	record := historyRecord{
		Kind:    "decision",
		Summary: "Chose Supabase over Convex for the queue because Postgres fits transcripts.",
		Terms:   []string{"supabase", "queue"},
	}
	matched := historyRecordMatchedTerms(record, "why did we choose supabase for the queue")
	if !containsToken(matched, "supabase") || !containsToken(matched, "queue") {
		t.Fatalf("expected supabase and queue in matched terms, got %v", matched)
	}
	if containsToken(matched, "why") || containsToken(matched, "did") {
		t.Fatalf("filler words must not appear in matched terms, got %v", matched)
	}
}

func summarizeIDs(records []semanticRecord) string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, fmt.Sprintf("%s(%d)", r.ID, r.Score))
	}
	return strings.Join(ids, ", ")
}

func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if strings.EqualFold(token, want) {
			return true
		}
	}
	return false
}

func TestDedupSeedCommands(t *testing.T) {
	in := []seedCommand{
		{Name: "build", Command: "next build", Source: "web/package.json"},
		{Name: "build", Command: "next build", Source: "web/package.json"}, // exact dup
		{Name: "build", Command: "pnpm --filter web build", Source: "package.json"},
		{Name: "test", Command: "go test ./...", Source: "Makefile"},
	}
	got := dedupSeedCommands(in)
	if len(got) != 3 {
		t.Fatalf("dedupSeedCommands kept %d, want 3: %+v", len(got), got)
	}
	// Exact (Name, Command) dup dropped; same-name different-command kept.
	if got[0].Command != "next build" || got[1].Command != "pnpm --filter web build" || got[2].Name != "test" {
		t.Fatalf("unexpected dedup result: %+v", got)
	}
}

func TestOverviewTextDisambiguatesSameNameCommands(t *testing.T) {
	report := brainOverviewReport{
		Commands: []seedCommand{
			{Name: "build", Command: "next build", Source: "web/package.json"},
			{Name: "build", Command: "pnpm --filter web build", Source: "package.json"},
			{Name: "test", Command: "go test ./...", Source: "Makefile"},
		},
	}
	var out strings.Builder
	cmd := &cobra.Command{Use: "overview"}
	cmd.SetOut(&out)
	renderBrainOverviewText(cmd, report)
	text := out.String()
	// Colliding names carry their source; the unique name does not.
	if !strings.Contains(text, "build (web/package.json): next build") ||
		!strings.Contains(text, "build (package.json): pnpm --filter web build") {
		t.Fatalf("same-name commands not disambiguated by source:\n%s", text)
	}
	if !strings.Contains(text, "  test: go test ./...") {
		t.Fatalf("unique command should render without source:\n%s", text)
	}
}
