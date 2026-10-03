package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ignoreForTest(t *testing.T, patterns ...string) (brainIgnore, string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".brainignore"),
		[]byte(strings.Join(patterns, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	ig, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	return ig, repo
}

// A withheld-count notice is not a partial failure. Appending the disclosure
// to each list separately put "N provider warning(s) were withheld" inside
// PartialFailures, where it is wrong in kind AND miscounts what it names: the
// entries it was counting there were failures, not warnings.
func TestWithheldDisclosureNeverLandsInPartialFailures(t *testing.T) {
	t.Parallel()

	ig, repo := ignoreForTest(t, "secret/")
	warnings := []semanticWarning{{Code: "kept", Detail: "internal/auth/token.go unreadable"}}
	failures := []semanticWarning{
		{Code: "dropped", Detail: repo + "/secret/config.go failed to parse"},
		{Code: "keptfail", Detail: "internal/db/pool.go failed to parse"},
	}

	keptWarnings, keptFailures := ig.filterIgnoredWarningLists(warnings, failures)

	for _, f := range keptFailures {
		if f.Code == warningsHiddenCode {
			t.Fatalf("a withheld-count notice is not a partial failure; found one in the failures list: %+v", f)
		}
	}
	var notice *semanticWarning
	for i := range keptWarnings {
		if keptWarnings[i].Code == warningsHiddenCode {
			notice = &keptWarnings[i]
		}
	}
	if notice == nil {
		t.Fatalf("withholding a partial failure must still be disclosed; warnings = %+v", keptWarnings)
	}
	// Wording has to match what was actually withheld. "provider warning(s)"
	// here would be a false statement about a failure.
	if !strings.Contains(notice.Detail, "partial indexing failure") {
		t.Fatalf("the notice must name what it withheld, got: %s", notice.Detail)
	}
	if strings.Contains(notice.Detail, "secret/config.go") || strings.Contains(notice.Detail, repo) {
		t.Fatalf("the disclosure must not leak the ignored path: %s", notice.Detail)
	}
}

// Both lists losing entries produces one notice naming both counts, not two
// notices or a single conflated total.
func TestWithheldDisclosureNamesBothCountsSeparately(t *testing.T) {
	t.Parallel()

	ig, repo := ignoreForTest(t, "secret/")
	keptWarnings, _ := ig.filterIgnoredWarningLists(
		[]semanticWarning{{Code: "w", Detail: repo + "/secret/a.go warned"}, {Code: "kept", Detail: "src/b.go warned"}},
		[]semanticWarning{{Code: "f", Detail: repo + "/secret/c.go failed"}},
	)

	var notices []semanticWarning
	for _, w := range keptWarnings {
		if w.Code == warningsHiddenCode {
			notices = append(notices, w)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("want exactly one disclosure, got %d: %+v", len(notices), notices)
	}
	d := notices[0].Detail
	if !strings.Contains(d, "provider warning") || !strings.Contains(d, "partial indexing failure") {
		t.Fatalf("one notice must name both kinds, got: %s", d)
	}
}

// Nothing withheld, nothing said. A notice that always fires is noise.
func TestNoDisclosureWhenNothingWithheld(t *testing.T) {
	t.Parallel()

	ig, _ := ignoreForTest(t, "secret/")
	keptWarnings, keptFailures := ig.filterIgnoredWarningLists(
		[]semanticWarning{{Code: "w", Detail: "src/a.go warned"}},
		[]semanticWarning{{Code: "f", Detail: "src/b.go failed"}},
	)
	for _, w := range append(append([]semanticWarning{}, keptWarnings...), keptFailures...) {
		if w.Code == warningsHiddenCode {
			t.Fatalf("nothing was withheld but a disclosure fired: %+v", w)
		}
	}
}

// The premise of the path-parsing fix: provider warnings are built from raw
// subprocess output (Detail is serr.Error()), so their paths are ABSOLUTE,
// while every .brainignore pattern is repo-relative. Matching without
// relativising means a warning naming an ignored file is never withheld --
// an under-drop that replaces the over-drop this change set set out to fix.
func TestAbsolutePathsInWarningTextAreMatchedAgainstIgnoreRules(t *testing.T) {
	t.Parallel()

	ig, repo := ignoreForTest(t, "secret/")

	if !ig.MentionsIgnoredPath(repo + "/secret/config.go: failed to parse") {
		t.Fatal("an absolute path inside the repo must match a repo-relative ignore pattern")
	}
	if !ig.MentionsIgnoredPath("secret/config.go: failed to parse") {
		t.Fatal("a repo-relative path must still match")
	}
	// A path outside the repo is not covered by THIS repo's rules, even when
	// the tail looks like a match. Relativising must not become a substring
	// match by another route -- that is the original bug.
	if ig.MentionsIgnoredPath("/somewhere/else/secret/config.go: failed to parse") {
		t.Fatal("a path outside the repo must not be matched against the repo's ignore rules")
	}
	// And the original defect itself: a pattern that is a substring of the
	// repo path must not swallow every warning in the repo.
	wide, wideRepo := ignoreForTest(t, filepath.Base(t.TempDir()))
	if wide.MentionsIgnoredPath(wideRepo + "/internal/cli/semantic.go: failed to parse") {
		t.Fatal("a pattern that merely appears in the absolute path discarded an unrelated warning")
	}
}
