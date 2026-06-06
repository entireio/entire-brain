package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func TestRegressionPathClassifiers(t *testing.T) {
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

	an, _, _ := detectRegressionAnomalies(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	if len(an) != 1 || an[0].Kind != "changed" || !strings.Contains(an[0].File, "review_context.go") {
		t.Fatalf("expected one changed anomaly on review_context.go, got %+v", an)
	}
	if !strings.Contains(an[0].Current, "master..HEAD") || !strings.Contains(an[0].Expected, "scopeBaseRef") {
		t.Fatalf("anomaly current/expected wrong: %+v", an[0])
	}

	// Clean tree (invariant still present) → no anomaly.
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	_, repoRoot2 := writeRegressionFixture(t, session, "pkg/review_context.go", clean)
	if an2, _, _ := detectRegressionAnomalies(brainDir, repoRoot2, nil, "fix scopeBaseRef base scope", 20, false); len(an2) != 0 {
		t.Fatalf("expected no anomaly on clean tree, got %+v", an2)
	}
}

func TestRegressionDeletionIsOptIn(t *testing.T) {
	session := `{"text":"in pkg/resolve_transcript.go set state.TranscriptPath = resolved so later reads work"}`
	// current file uses TranscriptPath but the assignment is gone.
	body := "package x\nfunc resolveTranscriptPath() string {\n\tresolved := compute()\n\treturn resolved // TranscriptPath\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/resolve_transcript.go", body)
	q := "fix resolveTranscriptPath TranscriptPath"

	if an, _, _ := detectRegressionAnomalies(brainDir, repoRoot, nil, q, 20, false); len(an) != 0 {
		t.Fatalf("deletions must be off by default, got %+v", an)
	}
	an, _, _ := detectRegressionAnomalies(brainDir, repoRoot, nil, q, 20, true)
	if len(an) != 1 || an[0].Kind != "deleted" {
		t.Fatalf("expected one deleted anomaly with --include-deletions, got %+v", an)
	}
}
