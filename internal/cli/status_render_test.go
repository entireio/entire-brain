package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/tui"
)

// statusFixtureReport is the report the screenshot that started this work came
// from, reduced to the shape that matters: forty-four vendored JSON corpora the
// semantic provider skipped, reported once as partial failures and AGAIN as
// blind spots, plus a relation-type histogram.
func statusFixtureReport(t *testing.T, blindSpotCount int) brainStatusReport {
	t.Helper()
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	spots := make([]brainBlindSpot, 0, blindSpotCount)
	warnings := make([]semanticWarning, 0, blindSpotCount)
	for i := 0; i < blindSpotCount; i++ {
		path := "benchmarks/agent-brain/mined/entire-cli-" + strconv.Itoa(i) + ".json"
		spots = append(spots, brainBlindSpot{
			Path:   path,
			Code:   "E_MINIFIED",
			Detail: "file appears minified/bundled (very long lines); not analyzed as source",
		})
		warnings = append(warnings, semanticWarning{
			Code:     "E_MINIFIED",
			Severity: "warning",
			Path:     path,
			Effect:   "file record emitted but symbol parsing skipped",
			Detail:   "file appears minified/bundled (very long lines); not analyzed as source",
		})
	}
	return brainStatusReport{
		GeneratedAt: now,
		Repo:        brainStatusRepo{Root: "/repo", Key: "gh/entireio/entire-brain"},
		Brain:       brainStatusBrain{Path: "/brain", ManifestState: "current", Schema: 3, SupportedSchema: 3},
		Sources:     brainStatusSources{Seed: true, Sessions: true, Semantic: true, Docs: true, Facts: true},
		Facts:       &brainStatusFacts{Facts: 375, Distilled: 375, Branches: 2},
		Semantic: &brainStatusSemantic{
			Coverage: &brainStatusSemanticCoverage{
				Files:                 1179,
				Symbols:               40781,
				Relations:             131529,
				PartialFailures:       blindSpotCount,
				PartialFailureDetails: warnings,
				RelationTypes:         []semanticAuditCount{{Name: "CALLS", Count: 34677}, {Name: "DEFINES", Count: 40781}},
			},
			BlindSpots: spots,
			Freshness: &staleReport{Severity: "degraded", Axes: map[string]staleAxis{
				"semantic_completeness": {State: "degraded", Detail: "44 partial failures"},
			}},
		},
		Onboarding: &brainStatusOnboarding{
			Facts: factsBackfillStatus{Sessions: 185, Distilled: 20, Running: true, PID: 4242},
			// Current: the unit on disk IS the one this repo would write. Said
			// explicitly because it is now load-bearing — a fixture that left it
			// false would be describing a drifted watcher and would pin the
			// wrong line as the ordinary case.
			Daemon:     daemonState{Installed: true, Current: true, Label: "io.entire.brain-watch.a2d3fd66"},
			LastTickAt: now.Add(-17 * time.Minute),
			Components: []brainStatusComponent{
				{Name: "sessions", State: "built"},
				{Name: "seed", State: "built"},
				{Name: "docs", State: "built"},
				{Name: "semantic", State: "built"},
				{Name: "history", State: "missing"},
			},
		},
		Live: brainLiveState{Branch: "main", Head: "58b0c54e"},
	}
}

func renderStatus(t *testing.T, report brainStatusReport, verbose bool) string {
	t.Helper()
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	renderBrainStatusText(cmd, report, verbose)
	return out.String()
}

// TestShortStatusGolden pins the DEFAULT report byte for byte. This is the
// screenshot's replacement: 130-odd lines of wall become ten lines that answer
// the three questions people run status to ask.
func TestShortStatusGolden(t *testing.T) {
	t.Parallel()
	got := renderStatus(t, statusFixtureReport(t, 44), false)
	const want = "Brain  gh/entireio/entire-brain\n" +
		"  /brain | manifest current | 1179 files, 40781 symbols | 375 facts\n" +
		"\n" +
		"Onboarding\n" +
		"  facts     #............. 20/185 distilled  (backfill running, pid 4242)\n" +
		"  daemon    - installed but not running (io.entire.brain-watch.a2d3fd66) (last watcher tick 17m0s ago)\n" +
		"  instant   + sessions  + seed  + docs  + semantic  - history\n" +
		"\n" +
		"- freshness degraded -- run `entire-brain refresh --agent none`\n" +
		"  semantic_completeness=degraded (44 partial failures)\n" +
		"  `entire-brain status --verbose` for the full report\n"
	if got != want {
		t.Fatalf("short status\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestShortStatusNeverLeaksTheVerboseWall is the mutation guard named in the
// brief: if any part of the full dump escapes into the default view, this
// fails. Line count is the honest proxy — the wall was 130 lines.
func TestShortStatusNeverLeaksTheVerboseWall(t *testing.T) {
	t.Parallel()
	got := renderStatus(t, statusFixtureReport(t, 44), false)
	if lines := strings.Count(got, "\n"); lines > 12 {
		t.Fatalf("the default report grew to %d lines; it exists to be short:\n%s", lines, got)
	}
	for _, forbidden := range []string{
		"E_MINIFIED",     // per-file warnings
		"relation types", // the histogram
		// The duplicate listing, named by its section header. The verdict is
		// allowed to say "44 partial failures" as a COUNT on its one cause
		// line -- that is the collapse working, and the number is why the
		// reader would open --verbose. What must never appear is the section
		// that enumerates them, which the path check below also catches.
		"partial failures:",
		"benchmarks/agent-brain", // any enumerated path
		"Retrieval",              // full-report sections
		"Live",
		"file languages",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the verbose wall leaked %q into the default report:\n%s", forbidden, got)
		}
	}
}

// TestShortStatusHealthyVerdict covers the other branch of the one-line verdict.
func TestShortStatusHealthyVerdict(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Semantic.Freshness.Severity = "ok"
	report.Semantic.BlindSpots = nil
	got := renderStatus(t, report, false)
	if !strings.Contains(got, "+ healthy\n") {
		t.Fatalf("a clean brain must say so in one line:\n%s", got)
	}
	if strings.Contains(got, "doctor") {
		t.Errorf("a healthy brain must not be told to run doctor:\n%s", got)
	}
}

// TestVerboseStatusCollapsesRepeatedWarnings is the second half of the bug: the
// old report printed the same forty-four files twice, once as partial failures
// and once as blind spots.
func TestVerboseStatusCollapsesRepeatedWarnings(t *testing.T) {
	t.Parallel()
	got := renderStatus(t, statusFixtureReport(t, 44), true)

	if n := strings.Count(got, "benchmarks/agent-brain"); n != 0 {
		t.Errorf("data-file paths must not be enumerated in status (doctor lists them); found %d", n)
	}
	if !strings.Contains(got, "44 data files skipped (minified/bundled)") {
		t.Fatalf("the collapsed summary line is missing:\n%s", got)
	}
	if n := strings.Count(got, "44 data files skipped"); n != 1 {
		t.Errorf("the collapsed line itself is printed %d times, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "all of them are the semantic warnings listed above") {
		t.Errorf("the blind-spot section must defer to the section that already listed them:\n%s", got)
	}
	// The full report still has to BE the full report.
	for _, want := range []string{"relation types:", "freshness: degraded", "blind spots: 44", "Live"} {
		if !strings.Contains(got, want) {
			t.Errorf("the verbose report dropped %q:\n%s", want, got)
		}
	}
}

// TestVerboseStatusPrintsNoLineTwice is the same duplicate-line assertion the
// progress renderer gets, applied to the report.
func TestVerboseStatusPrintsNoLineTwice(t *testing.T) {
	t.Parallel()
	got := renderStatus(t, statusFixtureReport(t, 44), true)
	seen := map[string]int{}
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		seen[line]++
	}
	for line, count := range seen {
		if count > 1 {
			t.Errorf("verbose report printed %q %d times", line, count)
		}
	}
}

// TestVerboseStatusEnumeratesRealSourceBlindSpots guards the opposite failure:
// collapsing must not hide a blind spot that is actual source code, which is a
// thing the reader has to act on.
func TestVerboseStatusEnumeratesRealSourceBlindSpots(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Semantic.BlindSpots = []brainBlindSpot{
		{Path: "internal/cli/huge.go", Code: "E_PARSE", Detail: "parser failure"},
	}
	got := renderStatus(t, report, true)
	if !strings.Contains(got, "internal/cli/huge.go") {
		t.Fatalf("a source-file blind spot must still be named:\n%s", got)
	}
}

func TestGroupStatusBlindSpots(t *testing.T) {
	t.Parallel()
	groups := groupStatusBlindSpots([]brainBlindSpot{
		{Path: "a.json", Code: "E_MINIFIED", Detail: "file appears minified/bundled (very long lines); not analyzed as source"},
		{Path: "b.json", Code: "E_MINIFIED", Detail: "file appears minified/bundled (very long lines); not analyzed as source"},
		{Path: "c.go", Code: "E_PARSE", Detail: "parser failure"},
	})
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(groups), groups)
	}
	// Biggest group first: the reader's attention should land on the thing that
	// accounts for most of the noise.
	if groups[0].Code != "E_MINIFIED" || groups[0].Count() != 2 || !groups[0].DataFiles {
		t.Errorf("first group = %+v", groups[0])
	}
	if groups[1].DataFiles {
		t.Errorf("a .go file is not a data file: %+v", groups[1])
	}
	if got := groups[0].Summary(); got != "2 data files skipped (minified/bundled)" {
		t.Errorf("summary = %q", got)
	}
}

func TestIsStatusDataFile(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"corpus.json":            true,
		"a/b/c.JSONL":            true,
		"bundle.min.js":          true,
		"styles.min.css":         true,
		"go.sum":                 false,
		"internal/cli/status.go": false,
		"vendor.map":             true,
		"pnpm-lock.lock":         true,
	} {
		if got := isStatusDataFile(path); got != want {
			t.Errorf("isStatusDataFile(%q) = %t, want %t", path, got, want)
		}
	}
}

func TestStatusHealthCounts(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Semantic.Freshness.Severity = "ok"
	report.Onboarding.Components[4].State = "failed"
	report.Issues = []memoryHealthIssue{{Code: "memory_source_stale"}}
	health := buildStatusHealth(report)
	if health.Healthy() {
		t.Fatal("a failed component plus an issue is not healthy")
	}
	if len(health.Failed) != 1 || health.Failed[0] != "history" {
		t.Errorf("failed components = %+v", health.Failed)
	}
	render := tui.NewRendererWith(tui.Theme{}, tui.PlainCaps())
	line := health.Line(render, setupBrainBinaryName)
	for _, want := range []string{"history failed", "1 issue", "run `entire-brain doctor`"} {
		if !strings.Contains(line, want) {
			t.Errorf("verdict %q missing %q", line, want)
		}
	}
}

func TestStatusHealthWarningUsesSkippedMark(t *testing.T) {
	t.Parallel()
	render := tui.NewRendererWith(tui.Theme{}, tui.PlainCaps())
	line := (statusHealth{Warnings: 1}).Line(render, setupBrainBinaryName)
	if !strings.HasPrefix(line, "- 1 warning") {
		t.Fatalf("warning-only health rendered as a hard failure: %q", line)
	}
}

// A watcher can be running and still not be YOURS: another repo's `setup`, or an
// older build, leaves a unit on disk that is not the one this repo would write.
// inspectDaemon computes exactly that (Current), and the status line dropped it,
// so the reader saw "running" and had no way to learn the watcher was not theirs.
func TestStatusSaysWhenTheInstalledDaemonIsNotCurrent(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 44)
	report.Onboarding.Daemon = daemonState{
		Installed: true,
		Running:   true,
		Current:   false,
		Label:     "io.entire.brain-watch.a2d3fd66",
	}
	got := renderStatus(t, report, false)
	if !strings.Contains(got, "not current") {
		t.Fatalf("a running-but-not-current watcher must say so in status:\n%s", got)
	}
	if !strings.Contains(got, "entire-brain setup") {
		t.Fatalf("status must say what to do about it:\n%s", got)
	}
}
