package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/tui"
)

// surface_honesty_test.go covers one defect class across the human-facing
// surfaces: a confident summary line contradicted by its own detail lines.
// Every case here was reproduced against a real brain on disk first — a
// never-built one, a mid-build one, and one with a deliberately corrupted
// store — because none of them changes an exit code.

// TestReviewDoesNotClaimCleanWhenNothingWasCompared is the headline case.
//
// `review` printed "Diff-less review: no suspected regressions (current tree
// matches the brain's memory)." with, on the very next line, "note: history
// holds no precise code assertions for these identifiers". The detector had
// returned before opening a single file; there was no comparison, and therefore
// no basis for a clean verdict. It said the same thing against a brain that had
// never been built at all.
func TestReviewDoesNotClaimCleanWhenNothingWasCompared(t *testing.T) {
	f := newVerifyFixture(t)

	out, err := execute(t, NewRootCommand(f.opts), "review", "widget renderer")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if strings.Contains(out, "no suspected regressions") {
		t.Errorf("review claims a clean tree without comparing anything:\n%s", out)
	}
	if strings.Contains(out, "current tree matches the brain's memory") {
		t.Errorf("review claims the tree matches a memory it never read:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "INCONCLUSIVE") {
		t.Errorf("review does not say the result is inconclusive:\n%s", out)
	}

	jsonOut, err := execute(t, NewRootCommand(f.opts), "review", "widget renderer", "--json")
	if err != nil {
		t.Fatalf("review --json: %v", err)
	}
	var report reviewReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("review --json is not parseable JSON: %v\n%s", err, jsonOut)
	}
	// A machine consumer must not have to parse the prose to learn this.
	if report.Checked {
		t.Errorf("review --json reports checked=true having scanned %d files", report.FilesScanned)
	}
	if report.FilesScanned != 0 {
		t.Errorf("files_scanned = %d, want 0", report.FilesScanned)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("fixture unexpectedly produced findings: %+v", report.Findings)
	}
	// The JSON summary and the text summary are the same sentence: the two
	// renderings may not disagree about whether the tree was checked.
	if !strings.Contains(strings.ToUpper(report.Summary), "INCONCLUSIVE") {
		t.Errorf("json summary disagrees with the text rendering: %q", report.Summary)
	}
}

// TestInspectRegressionsDoesNotClaimCleanWhenNothingWasScanned holds the sibling
// surface to the same standard; `review` and `inspect regressions` share a
// detector and must not disagree about what a zero-file scan means.
func TestInspectRegressionsDoesNotClaimCleanWhenNothingWasScanned(t *testing.T) {
	f := newVerifyFixture(t)
	out, err := execute(t, NewRootCommand(f.opts), "inspect", "regressions", "widget renderer")
	if err != nil {
		t.Fatalf("inspect regressions: %v", err)
	}
	if strings.Contains(out, "No suspected regressions") {
		t.Errorf("inspect regressions reports a clean scan of zero files:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "INCONCLUSIVE") {
		t.Errorf("inspect regressions does not say the result is inconclusive:\n%s", out)
	}
}

// TestOverviewSaysWhenTheBrainWasNeverBuilt: overview is the first command the
// guide tells an agent to run, and on an unbuilt brain every number in it is a
// real zero — "semantic: 0 files, 0 symbols, 0 relations", no boundaries, no
// decisions. Read alone that is indistinguishable from a built brain over an
// empty repository. `dash` already drew the distinction; `overview` did not, and
// printed a bare "freshness: " with nothing after the colon on top of it.
func TestOverviewSaysWhenTheBrainWasNeverBuilt(t *testing.T) {
	f := newVerifyFixture(t)
	out, err := execute(t, NewRootCommand(f.opts), "overview")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if !strings.Contains(out, "brain not built yet") {
		t.Errorf("overview does not say the brain was never built:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "freshness:" {
			t.Errorf("overview printed a freshness label with no verdict:\n%s", out)
		}
	}
	if !strings.Contains(out, "freshness: unknown") {
		t.Errorf("overview does not report freshness as unknown:\n%s", out)
	}

	jsonOut, err := execute(t, NewRootCommand(f.opts), "overview", "--json")
	if err != nil {
		t.Fatalf("overview --json: %v", err)
	}
	var report brainOverviewReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("overview --json is not parseable JSON: %v\n%s", err, jsonOut)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "brain not built yet") {
		t.Errorf("overview --json omits the not-built warning the text rendering carries: %v", report.Warnings)
	}
}

// TestDashReportsAnUnreadableSourceAsUnreadable: the Sources list is built from
// the manifest, which only says what the brain CLAIMS to hold. A corrupt
// semantic snapshot rendered as "Semantic present (8 symbols · 18 relations · 2
// files)" — manifest counts quoted with full confidence — directly above a
// warning that the same file could not be parsed, and a tab bar reading
// "Semantic (0)". The row has to carry the bad news, because the row is what a
// reader scans.
func TestDashReportsAnUnreadableSourceAsUnreadable(t *testing.T) {
	opts, repoDir, brainDir := factsAvailabilityFixture(t)
	factsPath := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("feature")))
	if err := os.WriteFile(factsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = repoDir

	out, err := execute(t, NewRootCommand(opts), "dash", "--plain")
	if err != nil {
		t.Fatalf("dash --plain: %v", err)
	}
	factsLine := ""
	inSources := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.TrimSpace(line) == "Sources":
			inSources = true
		case strings.TrimSpace(line) == "":
			inSources = false
		case inSources && strings.HasPrefix(strings.TrimSpace(line), "Facts "):
			factsLine = line
		}
	}
	if factsLine == "" {
		t.Fatalf("no Facts row in the Sources list:\n%s", out)
	}
	if strings.Contains(factsLine, "present") {
		t.Errorf("a fact store that could not be read is reported as present: %q\n%s", factsLine, out)
	}
	if !strings.Contains(factsLine, "unreadable") {
		t.Errorf("the Facts row does not say the store is unreadable: %q\n%s", factsLine, out)
	}

	jsonOut, err := execute(t, NewRootCommand(opts), "dash", "--json")
	if err != nil {
		t.Fatalf("dash --json: %v", err)
	}
	var snap tui.Snapshot
	if err := json.Unmarshal([]byte(jsonOut), &snap); err != nil {
		t.Fatalf("dash --json is not parseable JSON: %v\n%s", err, jsonOut)
	}
	found := false
	for _, source := range snap.Home.Sources {
		if source.Name != "Facts" {
			continue
		}
		found = true
		// The JSON rendering must carry the same fact as the text one.
		if source.State() != "unreadable" {
			t.Errorf("dash --json reports Facts as %q while --plain reports unreadable", source.State())
		}
	}
	if !found {
		t.Fatalf("dash --json has no Facts source: %s", jsonOut)
	}
}

// TestDashTerminalWriterKeepsTheTerminalVisible is the fix for a dashboard that
// never painted.
//
// bubbletea learns the window size by calling Fd() on its output, and only when
// that output satisfies term.File (an io.ReadWriteCloser with an Fd). Handing it
// a bare privacy wrapper — one Write method — meant "can't query window size":
// no initial WindowSizeMsg, no SIGWINCH watcher, and a Model that stayed on its
// pre-size placeholder forever. Driven through a pty, `dash` printed "loading…"
// and nothing else, while still accepting keys and exiting 0 on q, which is why
// no exit-code check ever caught it.
func TestDashTerminalWriterKeepsTheTerminalVisible(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "dash-out")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	writer := dashTerminalWriter(file, retrievalPrivacyPolicy{})
	probe, ok := writer.(interface {
		Fd() uintptr
		Read([]byte) (int, error)
		Close() error
	})
	if !ok {
		t.Fatalf("dash hands bubbletea a %T, which cannot report a window size; the dashboard would never leave its loading state", writer)
	}
	if probe.Fd() != file.Fd() {
		t.Errorf("Fd() = %d, want the terminal's %d", probe.Fd(), file.Fd())
	}

	// The privacy boundary is unchanged: writes still land on the wrapped stream.
	if _, err := writer.Write([]byte("frame")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "frame" {
		t.Errorf("wrote %q to the terminal, want %q", data, "frame")
	}

	// A non-file stream (a pipe, a test buffer) keeps the plain guarded writer.
	if _, ok := dashTerminalWriter(&strings.Builder{}, retrievalPrivacyPolicy{}).(dashTTYWriter); ok {
		t.Error("a non-terminal stream was wrapped as a terminal")
	}
}

// TestDashPlainAndJSONAgree: --plain, --json and the dashboard all render the
// same tui.Snapshot, so a fact present in one must be present in all three. The
// plain renderer is the one that can drift, because it re-derives its labels
// rather than printing the model's own — which is exactly how "present" came to
// be printed for a store that had failed to load.
func TestDashPlainAndJSONAgree(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)
	// Break one source so the comparison covers a degraded state, not just a
	// happy path where everything is trivially equal.
	factsPath := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("feature")))
	if err := os.WriteFile(factsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	plain, err := execute(t, NewRootCommand(opts), "dash", "--plain")
	if err != nil {
		t.Fatalf("dash --plain: %v", err)
	}
	jsonOut, err := execute(t, NewRootCommand(opts), "dash", "--json")
	if err != nil {
		t.Fatalf("dash --json: %v", err)
	}
	var snap tui.Snapshot
	if err := json.Unmarshal([]byte(jsonOut), &snap); err != nil {
		t.Fatalf("dash --json is not parseable JSON: %v", err)
	}

	// Every source's name and state, as the model holds them, must appear in the
	// plain rendering.
	for _, source := range snap.Home.Sources {
		line := ""
		for _, candidate := range strings.Split(plain, "\n") {
			fields := strings.Fields(candidate)
			if len(fields) >= 2 && fields[0] == source.Name {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Errorf("--json has source %q but --plain does not list it:\n%s", source.Name, plain)
			continue
		}
		if !strings.Contains(line, source.State()) {
			t.Errorf("source %q is %q in --json but --plain says %q", source.Name, source.State(), strings.TrimSpace(line))
		}
	}

	// The counts line must match the model's own list lengths — the same numbers
	// the dashboard's tab bar shows.
	wantCounts := fmt.Sprintf("Facts (%d) · Sessions (%d) · History (%d) · Semantic (%d)",
		len(snap.Facts), len(snap.Sessions), len(snap.History), len(snap.Semantic))
	if !strings.Contains(plain, wantCounts) {
		t.Errorf("--plain counts disagree with --json; want %q in:\n%s", wantCounts, plain)
	}

	// Warnings and notes travel in both.
	for _, warning := range snap.Home.Warnings {
		if !strings.Contains(plain, warning) {
			t.Errorf("--json carries warning %q that --plain drops", warning)
		}
	}
	for _, note := range snap.Notes {
		if !strings.Contains(plain, note) {
			t.Errorf("--json carries note %q that --plain drops", note)
		}
	}
	if snap.Repo != "" && !strings.Contains(plain, snap.Repo) {
		t.Errorf("--plain does not name the repo --json reports (%s)", snap.Repo)
	}
	if snap.Branch != "" && !strings.Contains(plain, snap.Branch) {
		t.Errorf("--plain does not name the branch --json reports (%s)", snap.Branch)
	}
}
