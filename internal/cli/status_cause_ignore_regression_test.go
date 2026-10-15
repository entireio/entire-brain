package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFilterWarningsKeepsWarningNamingSubstringOfIgnorePattern pins the defect
// that made `status` lie about a degraded index.
//
// `.brainignore` holds repository-relative patterns, and the first line of this
// repository's own .brainignore is `entire-brain` — the checkout directory's
// name. FilterWarnings ran before sanitizeSemanticWarnings, so warning Detail
// still carried the provider's ABSOLUTE paths, every one of which contains
// `entire-brain` as a substring. A free-text strings.Contains therefore matched
// every single index warning, the list came out empty, and the short report
// called the index clean over files it had failed to parse.
func TestFilterWarningsKeepsWarningNamingSubstringOfIgnorePattern(t *testing.T) {
	t.Parallel()

	ignore := brainIgnore{patterns: []string{"entire-brain"}}
	warnings := []semanticWarning{{
		Code:     "parse_failed",
		Severity: "warning",
		Path:     "internal/cli/semantic.go",
		Effect:   "missing symbols for internal/cli/semantic.go",
		Detail:   "parse /Users/dev/src/entire-brain/internal/cli/semantic.go: unexpected token at line 12",
	}}

	got := ignore.FilterWarnings(warnings)
	if len(got) != 1 {
		t.Fatalf("a warning whose absolute path merely CONTAINS the ignore pattern %q was discarded; kept %d of 1: %+v", "entire-brain", len(got), got)
	}
	if got[0].Code != "parse_failed" {
		t.Fatalf("wrong warning survived: %+v", got[0])
	}
}

// TestFilterWarningsStillDropsPathsTheIgnoreRulesCover guards the other
// direction: parsing paths out of the text must not stop the filter from
// honoring a rule that genuinely names the warning's subject.
func TestFilterWarningsStillDropsPathsTheIgnoreRulesCover(t *testing.T) {
	t.Parallel()

	ignore := brainIgnore{patterns: []string{"secrets/", "entire-brain"}}
	for name, warning := range map[string]semanticWarning{
		"path field": {Code: "a", Path: "secrets/token.go"},
		"detail names a repo-relative ignored path": {
			Code:   "b",
			Detail: "could not read secrets/token.go: permission denied",
		},
		"detail names a path under an ignored directory tree": {
			Code:   "c",
			Detail: "could not read entire-brain/internal/x.go: permission denied",
		},
		"effect names a secret by extension": {
			Code:   "d",
			Effect: "skipped config/private.pem",
		},
	} {
		if got := ignore.FilterWarnings([]semanticWarning{warning}); len(got) != 0 {
			t.Errorf("%s: ignored warning survived the filter: %+v", name, got)
		}
	}
}

// TestFilterWarningsReportedCountsWithheldWarnings pins the second half of the
// defect: the filter used to drop silently, so a reader could not tell an index
// with no warnings from an index whose warnings were all suppressed.
func TestFilterWarningsReportedCountsWithheldWarnings(t *testing.T) {
	t.Parallel()

	ignore := brainIgnore{patterns: []string{"secrets/"}}
	warnings := []semanticWarning{
		{Code: "hidden_one", Path: "secrets/a.go"},
		{Code: "hidden_two", Path: "secrets/b.go"},
		{Code: "visible", Path: "internal/cli/ok.go"},
	}

	got := func() []semanticWarning { kept, _ := ignore.filterIgnoredWarningLists(warnings, nil); return kept }()
	if len(got) != 2 {
		t.Fatalf("expected the kept warning plus one withheld-count notice, got %d: %+v", len(got), got)
	}
	notice := got[len(got)-1]
	if notice.Code != warningsHiddenCode {
		t.Fatalf("no withheld-count notice was appended; last entry = %+v", notice)
	}
	if !strings.Contains(notice.Detail, "2 provider warning(s)") {
		t.Fatalf("the notice does not report HOW MANY warnings were withheld: %q", notice.Detail)
	}

	// Nothing dropped means nothing to report: the notice must not appear on a
	// clean filter, or every status report would carry a permanent warning.
	clean := ignore.FilterWarningsReported([]semanticWarning{{Code: "visible", Path: "internal/cli/ok.go"}})
	if len(clean) != 1 {
		t.Fatalf("a filter that dropped nothing still added a notice: %+v", clean)
	}
}

// TestStatusFreshnessCauseBudgetsCauseInRunesNotBytes pins the user-facing
// truncation. The cause budget is advertised as a width, but it was measured in
// BYTES, so a cause carrying non-ASCII characters was charged two to four bytes
// each and cut to a fraction of the stated width.
func TestStatusFreshnessCauseBudgetsCauseInRunesNotBytes(t *testing.T) {
	t.Parallel()

	// 100 three-byte runes. With the "store=unsafe (…)" wrapper the cause is
	// 115 characters — comfortably inside a 140-RUNE budget — but 330 bytes,
	// more than twice a 140-BYTE one.
	detail := strings.Repeat("\u672c", 100)
	report := brainStatusReport{Semantic: &brainStatusSemantic{Freshness: &staleReport{
		Severity: "degraded",
		Axes:     map[string]staleAxis{"store": {State: "unsafe", Detail: detail}},
	}}}

	cause := statusFreshnessCause(report)
	if !strings.Contains(cause, detail) {
		t.Fatalf("a %d-rune (%d-byte) cause was truncated inside a %d-rune budget; got %q", utf8.RuneCountInString(detail), len(detail), statusCauseWidth, cause)
	}
}

// TestStatusFreshnessCauseNeverSplitsARune pins the overflow path: a cause that
// really is too long must be cut on a character boundary, never mid-rune.
func TestStatusFreshnessCauseNeverSplitsARune(t *testing.T) {
	t.Parallel()

	report := brainStatusReport{Semantic: &brainStatusSemantic{Freshness: &staleReport{
		Severity: "degraded",
		Axes:     map[string]staleAxis{"store": {State: "unsafe", Detail: strings.Repeat("\u672c", 400)}},
	}}}

	cause := statusFreshnessCause(report)
	if !utf8.ValidString(cause) {
		t.Fatalf("the truncated cause is not valid UTF-8: %q", cause)
	}
	// Exactly the budget: over it is an unreadable report, under it is the
	// byte-counting bug spending four characters of budget on one character.
	if got := utf8.RuneCountInString(cause); got != statusCauseWidth {
		t.Fatalf("an overlong cause was cut to %d runes, not the %d-rune budget: %q", got, statusCauseWidth, cause)
	}
	if strings.ContainsRune(cause, utf8.RuneError) {
		t.Fatalf("the truncated cause contains a replacement character, so a rune was split: %q", cause)
	}
}

// TestStatusCauseStringsFitStatusCauseWidth is the guard that stops a new long
// message from reintroducing the mid-word cut. It fails on the message as
// WRITTEN rather than on a rendered report, because the defect is authored into
// the source: a 182-character sentence cannot survive a 140-character budget no
// matter which code path renders it.
//
// Scope — the two construct classes that reach statusFreshnessCause:
//
//  1. Any package source: the Detail/Effect of a semanticWarning, staleAxis or
//     brainBlindSpot composite literal. freshnessSummary renders an axis as
//     "key=state (detail)" straight into the cause.
//  2. The semantic index and status files: fmt.Errorf format strings. Those
//     errors are recorded verbatim as an axis Detail (see semanticStaleReport,
//     which stores err.Error() as staleAxis.Detail).
//
// A format string is measured by its literal length, which is a lower bound:
// the verbs only expand it.
func TestStatusCauseStringsFitStatusCauseWidth(t *testing.T) {
	t.Parallel()

	causeFieldTypes := map[string]bool{"semanticWarning": true, "staleAxis": true, "brainBlindSpot": true}
	errorfFiles := map[string]bool{"semantic.go": true, "semantic_stream.go": true, "status_render.go": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	fieldsChecked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				ident, ok := n.Type.(*ast.Ident)
				if !ok || !causeFieldTypes[ident.Name] {
					return true
				}
				for _, element := range n.Elts {
					kv, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || (key.Name != "Detail" && key.Name != "Effect") {
						continue
					}
					fieldsChecked++
					for _, text := range staticStringsOf(kv.Value) {
						if utf8.RuneCountInString(text) > statusCauseWidth {
							t.Errorf("%s:%d: %s.%s is %d characters, over statusCauseWidth=%d, so the status cause cuts it mid-word: %q",
								name, fset.Position(kv.Pos()).Line, ident.Name, key.Name, utf8.RuneCountInString(text), statusCauseWidth, text)
						}
					}
				}
			case *ast.CallExpr:
				if !errorfFiles[name] || len(n.Args) == 0 {
					return true
				}
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Errorf" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" {
					return true
				}
				for _, text := range staticStringsOf(n.Args[0]) {
					if utf8.RuneCountInString(text) > statusCauseWidth {
						t.Errorf("%s:%d: fmt.Errorf format is %d characters, over statusCauseWidth=%d, and this file's errors are recorded as a freshness axis detail: %q",
							name, fset.Position(n.Pos()).Line, utf8.RuneCountInString(text), statusCauseWidth, text)
					}
				}
			}
			return true
		})
	}

	// Non-vacuity: a guard that scanned nothing passes for the wrong reason.
	if scanned < 10 {
		t.Fatalf("only %d package source files were scanned; the guard is not looking at the package", scanned)
	}
	if fieldsChecked == 0 {
		t.Fatal("no Detail/Effect field was examined; the composite-literal half of the guard is vacuous")
	}
}

// staticStringsOf returns the string content of an expression that is knowable
// at parse time: a literal, a concatenation of literals, or the string
// arguments of a call (which covers fmt.Sprintf format strings).
func staticStringsOf(expr ast.Expr) []string {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return nil
		}
		text, err := strconv.Unquote(value.Value)
		if err != nil {
			return nil
		}
		return []string{text}
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return nil
		}
		joined := strings.Join(staticStringsOf(value.X), "") + strings.Join(staticStringsOf(value.Y), "")
		if joined == "" {
			return nil
		}
		return []string{joined}
	case *ast.CallExpr:
		var out []string
		for _, arg := range value.Args {
			out = append(out, staticStringsOf(arg)...)
		}
		return out
	case *ast.ParenExpr:
		return staticStringsOf(value.X)
	}
	return nil
}
