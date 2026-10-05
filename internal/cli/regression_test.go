package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRegressionExtractSignals(t *testing.T) {
	// A change signal: identifier adjacent to a distinctive string-literal operand.
	excerpt := `the diff range must be scopeBaseRef+"..HEAD" not a hardcoded base`
	changes, _ := regressionExtractSignals("scopebaseref", excerpt, "s.jsonl:1")
	if len(changes) != 1 || changes[0].operand != "..head" {
		t.Fatalf("expected one change signal with operand `..head`, got %+v", changes)
	}

	// A delete signal: a field assignment whose target contains the identifier.
	excerpt = `Update state so reads use the path: state.TranscriptPath = resolved then return resolved`
	_, deletes := regressionExtractSignals("transcriptpath", excerpt, "s.jsonl:2")
	if len(deletes) != 1 || deletes[0].target != "state.transcriptpath=" || deletes[0].rhs != "resolved" {
		t.Fatalf("expected one delete signal (target state.transcriptpath=, rhs resolved), got %+v", deletes)
	}

	// A `:=` local declaration must NOT become a delete signal (ephemeral, not an invariant).
	_, deletes = regressionExtractSignals("scopebaseref", `scopeBaseRef := detectScope(ctx, root, out)`, "s.jsonl:3")
	if len(deletes) != 0 {
		t.Fatalf("`:=` local should not produce a delete signal, got %+v", deletes)
	}

	// A deleted helper call can be anchored to a same-line peer assignment, letting Radar localize
	// "call this after that state update" regressions even when the helper still exists elsewhere.
	_, deletes = regressionExtractSignals("realignattributionbase", `state.RealignAttributionBase(newHead) after state.BaseCommit = newHead`, "s.jsonl:4")
	if len(deletes) != 1 || deletes[0].target != "state.realignattributionbase(" || deletes[0].rhs != "newhead" {
		t.Fatalf("expected one helper-call delete signal, got %+v", deletes)
	}
	if len(deletes[0].anchors) != 1 || deletes[0].anchors[0] != "state.basecommit=" {
		t.Fatalf("expected peer BaseCommit assignment anchor, got %+v", deletes[0].anchors)
	}
}

func TestRegressionPathClassifiers(t *testing.T) {
	if got := regressionCleanRelPath(`pkg\windows_path.go`); got != "pkg/windows_path.go" {
		t.Fatalf("regression paths must be slash-normalized for JSON/MCP output, got %q", got)
	}
	for _, p := range []string{"a/b_test.go", "x/foo.test.ts", "pkg/tests/y.go", "a/testdata/z.go"} {
		if !regressionIsTestPath(p) {
			t.Errorf("expected %q classified as test", p)
		}
	}
	if regressionIsTestPath("cmd/entire/cli/review_context.go") {
		t.Error("implementation file misclassified as test")
	}
	if !regressionIsComment("  // a comment") || !regressionIsComment(" * doc") {
		t.Error("comment lines not detected")
	}
	if regressionIsComment(`base := "master..HEAD"`) {
		t.Error("code line misclassified as comment")
	}
}

// writeRegressionFixture builds a throwaway brain (one session) + repo (one file).
func writeRegressionFixture(t *testing.T, sessionText, repoRel, fileBody string) (brainDir, repoRoot string) {
	t.Helper()
	brainDir = t.TempDir()
	repoRoot = t.TempDir()
	sessDir := filepath.Join(brainDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "s.jsonl"), []byte(sessionText+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(repoRoot, filepath.FromSlash(repoRel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return brainDir, repoRoot
}

func TestRegressionDetectsChangedOperand(t *testing.T) {
	// History asserts `scopeBaseRef+"..HEAD"` in pkg/review_context.go; the current file replaced
	// the identifier with a literal — a regression.
	session := `{"role":"assistant","text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/review_context.go", regressed)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	if len(an) != 1 || an[0].Kind != "changed" || !strings.Contains(an[0].File, "review_context.go") {
		t.Fatalf("expected one changed anomaly on review_context.go, got %+v", an)
	}
	if !strings.Contains(an[0].Current, "master..HEAD") || !strings.Contains(an[0].Expected, "scopeBaseRef") {
		t.Fatalf("anomaly current/expected wrong: %+v", an[0])
	}

	// Clean tree (invariant still present) → no anomaly.
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	_, repoRoot2 := writeRegressionFixture(t, session, "pkg/review_context.go", clean)
	if an2, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot2, nil, "fix scopeBaseRef base scope", 20, false); len(an2) != 0 {
		t.Fatalf("expected no anomaly on clean tree, got %+v", an2)
	}
}

func TestRegressionChangedOperandReportsMissingHintedFileDespiteIntactPeer(t *testing.T) {
	session := `{"text":"pkg/a.go and pkg/b.go should keep the range as scopeBaseRef+\"..HEAD\""}`
	intact := "package x\nfunc a(scopeBaseRef string) string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	regressed := "package x\nfunc b() string {\n\treturn \"master..HEAD\"\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/a.go", intact)
	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "b.go"), []byte(regressed), 0o644); err != nil {
		t.Fatal(err)
	}

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	if len(an) != 1 || an[0].Kind != "changed" || an[0].File != "pkg/b.go" {
		t.Fatalf("expected only regressed hinted file b.go despite intact peer, got %+v", an)
	}
}

func TestRegressionChangedOperandReportsEachRegressedHintedFile(t *testing.T) {
	session := `{"text":"pkg/a.go and pkg/b.go should keep the range as scopeBaseRef+\"..HEAD\""}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/a.go", regressed)
	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "b.go"), []byte(regressed), 0o644); err != nil {
		t.Fatal(err)
	}

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	var files []string
	for _, a := range an {
		if a.Kind == "changed" {
			files = append(files, a.File)
		}
	}
	if len(files) != 2 || files[0] != "pkg/a.go" || files[1] != "pkg/b.go" {
		t.Fatalf("expected changed findings for both hinted files, got files=%v anomalies=%+v", files, an)
	}
}

func TestRegressionBenignRenameNotFlagged(t *testing.T) {
	// scopeBaseRef renamed to baseRef everywhere; the invariant is intact (`<ident> + "..HEAD"`).
	// The detector must NOT report a rename as a regression (the audit's conf-0.85 false positive).
	session := `{"text":"pkg/review_context.go builds the range as scopeBaseRef+\"..HEAD\""}`
	body := "package x\nfunc f(baseRef string) string {\n\treturn baseRef + \"..HEAD\"\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/review_context.go", body)
	if an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false); len(an) != 0 {
		t.Fatalf("benign rename (operand still concatenated to an identifier) must not flag: %+v", an)
	}
}

func TestRegressionCommentCopyDoesNotSuppress(t *testing.T) {
	// Real code regressed to a literal, but a stale COMMENT still shows the old invariant. The
	// "invariant still holds" check must skip comments, or it suppresses the real regression.
	session := `{"text":"pkg/review_context.go uses scopeBaseRef+\"..HEAD\" for the range"}`
	body := "package x\n// historical: scopeBaseRef+\"..HEAD\"\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/review_context.go", body)
	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	if len(an) != 1 || an[0].Kind != "changed" || !strings.Contains(an[0].Current, "master..HEAD") {
		t.Fatalf("a comment copy of the invariant must not suppress the real regression: %+v", an)
	}
}

func TestRegressionIntactInvariantNotFlagged(t *testing.T) {
	// Invariant intact in real code AND a bare `..HEAD` literal collides elsewhere → no flag.
	session := `{"text":"pkg/review_context.go scopeBaseRef+\"..HEAD\""}`
	body := "package x\nfunc f() string {\n\trng := scopeBaseRef + \"..HEAD\"\n\t_ = \"origin/main..HEAD\"\n\treturn rng\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/review_context.go", body)
	if an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false); len(an) != 0 {
		t.Fatalf("intact invariant must not flag despite an operand collision elsewhere: %+v", an)
	}
}

func TestBrainReviewMapsAnomalyToFinding(t *testing.T) {
	// Honest severity: nothing the heuristic detector emits is "high" — that tier is reserved for a
	// future high-confidence (locus+recency) signal. changed (0.8) is rename-ambiguous -> medium;
	// deleted (0.65) is noisier/opt-in -> low.
	if regressionSeverity(0.95) != "high" || regressionSeverity(0.8) != "medium" || regressionSeverity(0.65) != "low" {
		t.Fatalf("severity thresholds wrong: %s/%s/%s", regressionSeverity(0.95), regressionSeverity(0.8), regressionSeverity(0.65))
	}
	f := anomalyToReviewFinding(regressionAnomaly{
		File: "pkg/review_context.go", Line: 412, Kind: "changed", Identifier: "scopebaseref",
		Expected: `scopeBaseRef+"..HEAD"`, Current: `"master..HEAD"`, Confidence: 0.8, Evidence: "sessions/x.jsonl:1",
	})
	// A hedged "could be a rename" changed-finding must NOT surface as HIGH — the machine severity
	// must not contradict the prose hedge.
	if f.Severity != "medium" || f.File != "pkg/review_context.go" || f.Line != 412 {
		t.Fatalf("finding fields wrong: %+v", f)
	}
	if !strings.Contains(f.Title, "Suspected regression") || !strings.Contains(f.Detail, `scopeBaseRef+"..HEAD"`) || !strings.Contains(f.Detail, "master..HEAD") {
		t.Fatalf("finding title/detail wrong: %+v", f)
	}

	// Location-only branch: Expected/Current blanked → generic detail, no expected/current leak.
	loc := anomalyToReviewFinding(regressionAnomaly{
		File: "pkg/review_context.go", Line: 412, Kind: "changed", Identifier: "scopebaseref",
		Expected: "", Current: "", Confidence: 0.8, Evidence: "sessions/x.jsonl:1",
	})
	if !strings.Contains(loc.Detail, "verify against the brain's history") || strings.Contains(loc.Detail, "history shows") {
		t.Fatalf("location-only finding must use the generic detail (no expected/current), got %q", loc.Detail)
	}
}

func TestBrainReviewUsesRuntimeTraceForRankingAndContext(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	tracePath := filepath.Join(repoDir, "runtime-trace.ndjson")
	if err := os.WriteFile(tracePath, []byte(`{"from":"ValidateToken","to":"TestValidateToken","type":"OBSERVED_CALL"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write runtime trace: %v", err)
	}
	if err := runSemanticIngestTraces(&cobra.Command{Use: "ingest"}, opts, semanticTraceIngestOptions{json: true}, tracePath); err != nil {
		t.Fatalf("ingest runtime trace: %v", err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"aaa/noise.go and internal/auth/token.go should keep ValidateToken+\"..HEAD\" for token validation"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	for rel, body := range map[string]string{
		"aaa/noise.go":           "package aaa\nfunc Noise() string {\n\treturn \"master..HEAD\"\n}\n",
		"internal/auth/token.go": "package auth\nfunc ValidateToken() string {\n\treturn \"master..HEAD\"\n}\n",
	} {
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	for _, k := range [][]string{{"git", "status", "--porcelain"}, {"git", "status", "--porcelain", "--untracked-files=all"}, {"git", "diff-index", "-M", "--shortstat", "HEAD"}, {"git", "diff", "--name-status", "-M", "-C", "HEAD"}} {
		runner.responses[fakeCommandKey(k[0], k[1:]...)] = fakeCommandResponse{}
	}
	var out strings.Builder
	reviewCmd := &cobra.Command{Use: "review"}
	reviewCmd.SetOut(&out)
	if err := runBrainReview(reviewCmd.Context(), reviewCmd, opts, regressionDetectorOptions{limit: 5, json: true}, "ValidateToken token regression"); err != nil {
		t.Fatalf("review: %v\n%s", err, out.String())
	}
	var report reviewReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("parse review report: %v\n%s", err, out.String())
	}
	if len(report.Findings) < 2 {
		t.Fatalf("expected both hinted regressions, got %+v", report.Findings)
	}
	if report.Findings[0].File != "internal/auth/token.go" {
		t.Fatalf("runtime trace should rank token.go before alphabetic distractor, got %+v", report.Findings)
	}
	if len(report.RuntimeTraces) == 0 || report.RuntimeTraces[0].Type != "RUNTIME_TRACE" || report.RuntimeTraces[0].FilePath != "internal/auth/token.go" {
		t.Fatalf("review missing runtime trace context: %+v", report.RuntimeTraces)
	}
}

func TestInspectRegressionsCommandJSONAndLocationOnly(t *testing.T) {
	// Exercises the `inspect regressions` CLI command end to end (runRegressionDetect via cobra),
	// the regressionReport JSON envelope + its schema_version, and the --location-only flag wiring —
	// none of which the detector-direct tests cover.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	for _, k := range [][]string{{"git", "status", "--porcelain"}, {"git", "status", "--porcelain", "--untracked-files=all"}, {"git", "diff-index", "-M", "--shortstat", "HEAD"}, {"git", "diff", "--name-status", "-M", "-C", "HEAD"}} {
		runner.responses[fakeCommandKey(k[0], k[1:]...)] = fakeCommandResponse{}
	}
	mk := func() *cobra.Command {
		return NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	}
	if _, err := execute(t, mk(), "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "s.jsonl"),
		[]byte(`{"text":"in pkg/review_context.go the scope range is scopeBaseRef+\"..HEAD\""}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, mk(), "inspect", "regressions", "fix scopeBaseRef base scope review", "--json")
	if err != nil {
		t.Fatalf("inspect regressions --json: %v", err)
	}
	var rep regressionReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse regressionReport: %v\n%s", err, out)
	}
	if rep.SchemaVersion != reviewReportSchemaVersion {
		t.Fatalf("regressionReport schema_version = %d, want %d", rep.SchemaVersion, reviewReportSchemaVersion)
	}
	if !strings.Contains(out, `"schema_version"`) {
		t.Fatalf("regressionReport JSON missing the literal schema_version tag:\n%s", out)
	}
	if len(rep.Anomalies) != 1 || rep.Anomalies[0].Kind != "changed" || !strings.Contains(rep.Anomalies[0].File, "review_context.go") {
		t.Fatalf("anomalies wrong: %+v", rep.Anomalies)
	}
	if rep.Anomalies[0].Expected == "" {
		t.Fatalf("expected should be populated without --location-only: %+v", rep.Anomalies[0])
	}

	locOut, err := execute(t, mk(), "inspect", "regressions", "fix scopeBaseRef base scope review", "--json", "--location-only")
	if err != nil {
		t.Fatalf("inspect regressions --location-only: %v", err)
	}
	var locRep regressionReport
	if err := json.Unmarshal([]byte(locOut), &locRep); err != nil {
		t.Fatalf("parse location-only report: %v\n%s", err, locOut)
	}
	if len(locRep.Anomalies) != 1 || locRep.Anomalies[0].Expected != "" || locRep.Anomalies[0].Current != "" {
		t.Fatalf("--location-only flag must blank expected/current through the command: %+v", locRep.Anomalies)
	}
	if locRep.Anomalies[0].Symbol != "f" {
		t.Fatalf("--location-only should retain enclosing symbol context, got %+v", locRep.Anomalies[0])
	}
}

func TestRegressionDetectorWarnings(t *testing.T) {
	// Each user-facing "found nothing" diagnostic path returns a specific warning + a non-nil slice.
	// (1) no extractable identifier in the query.
	brainDir, repoRoot := writeRegressionFixture(t, `{"text":"x"}`, "a.go", "package a\n")
	an, _, w, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "the and for with", 20, false)
	if an == nil || len(an) != 0 || len(w) == 0 || !strings.Contains(w[0], "no code identifiers") {
		t.Fatalf("expected non-nil empty slice + no-identifiers warning, got an=%+v w=%v", an, w)
	}
	// (2) identifier present, but history holds no precise code assertion.
	brainDir, repoRoot = writeRegressionFixture(t, `{"text":"we touched scopeBaseRef somewhere in prose"}`, "a.go", "package a\n")
	if _, _, w, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef", 20, false); len(w) == 0 || !strings.Contains(w[len(w)-1], "no precise code assertions") {
		t.Fatalf("expected no-precise-assertions warning, got %v", w)
	}
	// (3) a precise assertion names a file that does not exist in the current tree.
	brainDir, repoRoot = writeRegressionFixture(t, `{"text":"in pkg/gone.go the range is scopeBaseRef+\"..HEAD\""}`, "other.go", "package x\n")
	if _, _, w, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false); len(w) == 0 || !strings.Contains(w[len(w)-1], "no current files") {
		t.Fatalf("expected no-current-files warning, got %v", w)
	}
}

func TestRegressionDeletionIsOptIn(t *testing.T) {
	session := `{"text":"in pkg/resolve_transcript.go set state.TranscriptPath = resolved so later reads work"}`
	// current file uses TranscriptPath but the assignment is gone.
	body := "package x\n// TranscriptPath should be updated after re-resolution.\nfunc resolveTranscriptPath(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)
	q := "fix resolveTranscriptPath TranscriptPath"

	if an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, q, 20, false); len(an) != 0 {
		t.Fatalf("deletions must be off by default, got %+v", an)
	}
	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, q, 20, true)
	if len(an) != 1 || an[0].Kind != "deleted" {
		t.Fatalf("expected one deleted anomaly with --include-deletions, got %+v", an)
	}
	if an[0].Line == 2 || strings.HasPrefix(strings.TrimSpace(an[0].Current), "//") {
		t.Fatalf("deleted anomaly should prefer a code locus over a nearby comment, got %+v", an[0])
	}
	// location-only on a DELETED-kind finding: the commands blank Expected/Current before rendering
	// (regression.go), and the mapping must then fall back to the generic detail — no leak. (The
	// command-level blanking itself is covered for the changed kind in TestMCPBrainReviewTool.)
	del := an[0]
	del.Expected, del.Current = "", ""
	if f := anomalyToReviewFinding(del); strings.Contains(f.Detail, "history shows") {
		t.Fatalf("location-only deleted finding must not leak expected/current: %q", f.Detail)
	}
}

func TestRegressionAssignmentDeletionReportsMissingHintedFileDespiteIntactPeer(t *testing.T) {
	session := `{"text":"pkg/resolve_a.go and pkg/resolve_b.go must set state.TranscriptPath = resolved so later reads work"}`
	intact := "package x\nfunc resolveA(state *State) string {\n\tresolved := compute()\n\tstate.TranscriptPath = resolved\n\treturn resolved\n}\n"
	missing := "package x\nfunc resolveB(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_a.go", intact)
	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "resolve_b.go"), []byte(missing), 0o644); err != nil {
		t.Fatal(err)
	}

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved", 20, true)
	var deleted []regressionAnomaly
	for _, a := range an {
		if a.Kind == "deleted" && strings.Contains(strings.ToLower(a.Expected), "transcriptpath") {
			deleted = append(deleted, a)
		}
	}
	if len(deleted) != 1 || deleted[0].File != "pkg/resolve_b.go" {
		t.Fatalf("expected only the missing hinted file despite intact peer assignment, got %+v", an)
	}
	if deleted[0].Line == 0 || strings.HasPrefix(strings.TrimSpace(deleted[0].Current), "//") {
		t.Fatalf("expected an actionable code locus for missing assignment, got %+v", deleted[0])
	}
}

func TestRegressionAssignmentDeletionReportsEachMissingHintedSymbol(t *testing.T) {
	session := `{"text":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc missOne(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\t// state.TranscriptPath = resolved\n\treturn resolved\n}\nfunc missTwo(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\t// state.TranscriptPath = resolved\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved", 20, true)
	var lines []int
	for _, a := range an {
		if a.Kind == "deleted" && a.File == "pkg/resolve_transcript.go" {
			lines = append(lines, a.Line)
		}
	}
	if len(lines) != 2 || lines[0] != 4 || lines[1] != 10 {
		t.Fatalf("expected missing assignment findings at lines 4 and 10, got lines=%v anomalies=%+v", lines, an)
	}
	if an[0].Symbol != "missOne" || an[1].Symbol != "missTwo" {
		t.Fatalf("expected enclosing symbols for missing assignment sites, got %+v", an)
	}
	if len(an[0].RelatedLocations) == 0 || !strings.Contains(an[0].RelatedLocations[0], "missTwo") {
		t.Fatalf("expected related same-file assignment location context, got %+v", an[0])
	}
}

func TestRegressionAssignmentDeletionRelatedLocationsStayOnSameInvariant(t *testing.T) {
	session := `{"text":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved so later reads work, and pkg/resolve_transcript.go must set state.ResolveMode = nextMode when mode changes"}`
	body := "package x\nfunc missPathOne(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missPathTwo(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missMode(state *State) string {\n\tnextMode := computeMode()\n\t_ = state.ResolveMode\n\treturn nextMode\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved ResolveMode nextMode", 20, true)
	bySymbol := map[string]regressionAnomaly{}
	for _, a := range an {
		if a.Kind == "deleted" && a.File == "pkg/resolve_transcript.go" && a.Symbol != "" {
			bySymbol[a.Symbol] = a
		}
	}
	for _, want := range []string{"missPathOne", "missPathTwo", "missMode"} {
		if _, ok := bySymbol[want]; !ok {
			t.Fatalf("missing deletion finding for %s: %+v", want, an)
		}
	}
	for _, sym := range []string{"missPathOne", "missPathTwo"} {
		got := strings.Join(bySymbol[sym].RelatedLocations, "\n")
		if !strings.Contains(got, "missPath") {
			t.Fatalf("path finding %s missing related path peer: %+v", sym, bySymbol[sym])
		}
		if strings.Contains(got, "missMode") {
			t.Fatalf("path finding %s cross-contaminated unrelated mode locus: %+v", sym, bySymbol[sym])
		}
	}
	if got := strings.Join(bySymbol["missMode"].RelatedLocations, "\n"); strings.Contains(got, "missPath") {
		t.Fatalf("mode finding cross-contaminated path loci: %+v", bySymbol["missMode"])
	}
}

func TestRegressionAssignmentDeletionRelatedLocationsDistinguishSameIdentifierDifferentRHS(t *testing.T) {
	session := `{"text":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved for local transcripts, and pkg/resolve_transcript.go must set state.TranscriptPath = fallback for archived transcripts"}`
	body := "package x\nfunc missResolved(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missFallback(state *State) string {\n\tfallback := computeFallback()\n\t_ = state.TranscriptPath\n\treturn fallback\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved fallback", 20, true)
	var foundResolved, foundFallback bool
	for _, a := range an {
		if a.Kind != "deleted" || a.File != "pkg/resolve_transcript.go" {
			continue
		}
		related := strings.Join(a.RelatedLocations, "\n")
		switch {
		case a.Symbol == "missResolved" && strings.Contains(a.Expected, "resolved"):
			foundResolved = true
			if strings.Contains(related, "missFallback") {
				t.Fatalf("resolved finding cross-contaminated fallback locus: %+v", a)
			}
		case a.Symbol == "missFallback" && strings.Contains(a.Expected, "fallback"):
			foundFallback = true
			if strings.Contains(related, "missResolved") {
				t.Fatalf("fallback finding cross-contaminated resolved locus: %+v", a)
			}
		}
	}
	if !foundResolved || !foundFallback {
		t.Fatalf("expected resolved and fallback deletion findings, found resolved=%v fallback=%v anomalies=%+v", foundResolved, foundFallback, an)
	}
}

func TestRegressionAssignmentDeletionDoesNotLetIntactSiblingMaskRHSOnlySite(t *testing.T) {
	session := `{"text":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc ok(state *State) string {\n\tresolved := compute()\n\tstate.TranscriptPath = resolved\n\treturn resolved\n}\nfunc miss(state *State) string {\n\tresolved := compute()\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved", 20, true)
	var deleted []regressionAnomaly
	for _, a := range an {
		if a.Kind == "deleted" && a.File == "pkg/resolve_transcript.go" {
			deleted = append(deleted, a)
		}
	}
	if len(deleted) != 1 || deleted[0].Symbol != "miss" {
		t.Fatalf("expected missing RHS-only assignment site in miss, got %+v", an)
	}
	if deleted[0].Line != 8 {
		t.Fatalf("expected missing site to land on miss resolved line 8, got %+v", deleted[0])
	}
}

func TestRegressionAssignmentDeletionReportsSameNameReceiverMethods(t *testing.T) {
	session := `{"text":"pkg/resolver.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc (a *Alpha) Resolve(state *State) string {\n\tresolved := computeA()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc (b *Beta) Resolve(state *State) string {\n\tresolved := computeB()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolver.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved", 20, true)
	var lines []int
	for _, a := range an {
		if a.Kind == "deleted" && a.File == "pkg/resolver.go" {
			lines = append(lines, a.Line)
		}
	}
	if len(lines) != 2 || lines[0] != 4 || lines[1] != 9 {
		t.Fatalf("same-name receiver methods must each get a missing assignment finding, got lines=%v anomalies=%+v", lines, an)
	}
}

func TestRegressionAssignmentDeletionReportsOneMissingLocusInsideSameFunction(t *testing.T) {
	session := `{"text":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc resolveBoth(state *State) string {\n\t{\n\t\tresolved := computeA()\n\t\tstate.TranscriptPath = resolved\n\t}\n\t{\n\t\tresolved := computeB()\n\t\treturn resolved\n\t}\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix TranscriptPath resolved", 20, true)
	var deleted []regressionAnomaly
	for _, a := range an {
		if a.Kind == "deleted" && a.File == "pkg/resolve_transcript.go" {
			deleted = append(deleted, a)
		}
	}
	if len(deleted) != 1 || deleted[0].Line != 8 {
		t.Fatalf("expected only the second same-function locus to be missing, got %+v", an)
	}
}

func TestRegressionDeletionRanksCallLocusWithHistoryFileHint(t *testing.T) {
	session := `{"text":"cmd/entire/cli/strategy/manual_commit_hooks.go must call state.RealignAttributionBase(newHead) after state.BaseCommit = newHead; cmd/entire/cli/agent/cursor/types.go used HumanAdded+\"total\""}`
	_, deletes := regressionExtractSignals("realignattributionbase", session, "sessions/s.jsonl:1")
	if len(deletes) != 1 || deletes[0].target != "state.realignattributionbase(" || deletes[0].rhs != "newhead" {
		t.Fatalf("expected call delete signal, got %+v", deletes)
	}
	hooks := "package strategy\nfunc f(state *State, newHead string) {\n\tstate.BaseCommit = newHead\n\tlog(\"AttributionBaseCommit\", newHead)\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "cmd/entire/cli/strategy/manual_commit_hooks.go", hooks)
	noise := filepath.Join(repoRoot, "cmd", "entire", "cli", "agent", "cursor")
	if err := os.MkdirAll(noise, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noise, "types.go"), []byte("package cursor\ntype Summary struct { Total int }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "RealignAttributionBase newHead HumanAdded", 20, true)
	if len(an) == 0 {
		t.Fatal("expected a deleted call anomaly")
	}
	if an[0].Kind != "deleted" || an[0].File != "cmd/entire/cli/strategy/manual_commit_hooks.go" {
		t.Fatalf("deleted call should rank the history-hinted hook locus first, got %+v", an)
	}
	if !strings.Contains(strings.ToLower(an[0].Expected), "realignattributionbase") {
		t.Fatalf("expected deleted call in anomaly, got %+v", an[0])
	}
	if an[0].Line != 3 {
		t.Fatalf("expected the current base-advance line as the location, got %+v", an[0])
	}
}

func TestRegressionDeletionReportsEachMissingAnchoredCallSite(t *testing.T) {
	session := `{"text":"cmd/entire/cli/strategy/manual_commit_hooks.go must call state.RealignAttributionBase(newHead) after state.BaseCommit = newHead"}`
	body := "package strategy\nfunc ok(state *State, newHead string) {\n\tstate.BaseCommit = newHead\n\tstate.RealignAttributionBase(newHead)\n}\nfunc missOne(state *State, newHead string) {\n\tstate.BaseCommit = newHead\n\tlog(newHead)\n}\nfunc missTwo(state *State, newHead string) {\n\tstate.BaseCommit = newHead\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "cmd/entire/cli/strategy/manual_commit_hooks.go", body)

	an, _, _, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "RealignAttributionBase newHead manual commit hooks", 20, true)
	var lines []int
	for _, a := range an {
		if a.Kind == "deleted" && strings.Contains(strings.ToLower(a.Expected), "realignattributionbase") {
			lines = append(lines, a.Line)
		}
	}
	if len(lines) != 2 || lines[0] != 7 || lines[1] != 11 {
		t.Fatalf("expected missing helper-call findings at lines 7 and 11, got lines=%v anomalies=%+v", lines, an)
	}
	if an[0].Symbol != "missOne" || an[1].Symbol != "missTwo" {
		t.Fatalf("expected enclosing symbols for missing call sites, got %+v", an)
	}
	if len(an[0].RelatedLocations) == 0 || !strings.Contains(an[0].RelatedLocations[0], "missTwo") {
		t.Fatalf("expected related same-file location context, got %+v", an[0])
	}
}

func TestRegressionDedupeRankPrefersBoostedCallDeletion(t *testing.T) {
	ranked := regressionDedupeRank([]regressionAnomaly{
		{
			File:       "cmd/entire/cli/agent/cursor/types.go",
			Kind:       "changed",
			Expected:   `HumanAdded+"total"`,
			Confidence: 0.8,
		},
		{
			File:       "cmd/entire/cli/strategy/manual_commit_hooks.go",
			Kind:       "deleted",
			Expected:   "state.RealignAttributionBase(newHead)",
			Confidence: 0.65,
			rankBoost:  2,
		},
	})
	if len(ranked) != 2 {
		t.Fatalf("expected two ranked anomalies, got %+v", ranked)
	}
	if ranked[0].Expected != "state.RealignAttributionBase(newHead)" {
		t.Fatalf("boosted call deletion should rank above changed noise, got %+v", ranked)
	}
}
