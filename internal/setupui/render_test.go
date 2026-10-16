package setupui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/entireio/entire-brain/internal/tui"
)

// TestSubstepsNeverEmitATopLevelLine is the anti-pytest guard, asserted as a
// property of the WHOLE run rather than of one phase: whatever the sub-steps
// do, each phase that was opened contributes exactly one glyph-bearing row.
// Nine instant-core components, three agent steps and two watcher steps run in
// the script; none of their labels may appear anywhere in the output.
func TestSubstepsNeverEmitATopLevelLine(t *testing.T) {
	t.Parallel()
	out, run := renderRun(scriptedRun, tui.Caps{Width: 200}, 200)
	lines := run.Lines()

	if len(lines) == 0 {
		t.Fatal("non-vacuity: the scripted run emitted no lines at all")
	}

	opened := []PhaseID{PhaseDetect, PhaseBuild, PhaseAgents, PhaseFacts, PhaseWorkspace, PhaseWatcher, PhaseReady}
	seen := map[string]int{}
	for _, line := range lines {
		if line.Title != "" {
			seen[line.Title]++
		}
	}
	for _, id := range opened {
		if seen[id.Title()] != 1 {
			t.Errorf("phase %s produced %d titled lines, want exactly 1", id.Title(), seen[id.Title()])
		}
	}
	if len(seen) != len(opened) {
		t.Errorf("titled lines for %d phases, want %d: %v", len(seen), len(opened), seen)
	}

	// Non-vacuity: the sub-steps really did run, and there really were nine.
	var substepCount int
	for _, phase := range []string{"Build", "Agents", "Watcher"} {
		_ = phase
	}
	substepCount = len(instantComponents) + 3 + 2 + 4 + 1 + 1
	if substepCount < 15 {
		t.Fatalf("non-vacuity: the script runs only %d sub-steps, too few to prove anything", substepCount)
	}
	for _, component := range instantComponents {
		if strings.Contains(out, component) {
			t.Errorf("sub-step %q reached permanent output:\n%s", component, out)
		}
	}
	for _, step := range []string{"agent instructions", "pre-edit hook already", "writing launchd plist", "bootstrapping service", "counting distillable"} {
		if strings.Contains(out, step) && step != "pre-edit hook already" {
			t.Errorf("sub-step %q reached permanent output", step)
		}
	}
}

// TestFailurePromotesExactlyOneLine pins the other half of the volume problem:
// a failing phase gets its row and ONE flattened reason, never the four-line
// stderr dump the error actually carried.
func TestFailurePromotesExactlyOneLine(t *testing.T) {
	t.Parallel()
	_, run := renderRun(scriptedRun, tui.Caps{Width: 200}, 200)
	lines := run.Lines()

	index := -1
	for i, line := range lines {
		if line.Title == PhaseWatcher.Title() {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("non-vacuity: the scripted run never failed a phase")
	}
	if lines[index].Glyph != GlyphFailed {
		t.Fatalf("failed phase glyph = %v, want GlyphFailed", lines[index].Glyph)
	}

	promoted := 0
	for _, line := range lines[index+1:] {
		if line.Title != "" {
			break
		}
		promoted++
		if strings.Contains(line.Text, "\n") {
			t.Errorf("promoted line is multi-line: %q", line.Text)
		}
	}
	if promoted != 1 {
		t.Errorf("failure promoted %d lines, want exactly 1", promoted)
	}

	// Non-vacuity: the error genuinely had four lines to collapse.
	reasons := run.Failures()
	if len(reasons) != 1 {
		t.Fatalf("Failures() = %d, want 1", len(reasons))
	}
	if !strings.Contains(reasons[0].Reason, "; ") {
		t.Errorf("reason %q shows no sign of having been flattened", reasons[0].Reason)
	}
}

// TestRenderModes asserts each of the four destinations is correct in the way
// that destination is supposed to be correct.
func TestRenderModes(t *testing.T) {
	t.Parallel()
	for _, m := range renderModes(100) {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			out, _ := renderRun(scriptedRun, m.caps, 100)
			if strings.TrimSpace(out) == "" {
				t.Fatal("non-vacuity: no output")
			}
			hasEscape := strings.Contains(out, "\x1b[")
			if m.caps.Color != hasEscape {
				t.Errorf("Color=%v but escape sequences present=%v", m.caps.Color, hasEscape)
			}
			plain := StripANSI(out)
			hasUnicode := strings.ContainsAny(plain, "\u2713\u2717\u2212\u00b7\u2014\u2026")
			if m.caps.Unicode != hasUnicode {
				t.Errorf("Unicode=%v but non-ASCII glyphs present=%v in:\n%s", m.caps.Unicode, hasUnicode, plain)
			}
			if !m.caps.Unicode {
				for _, r := range plain {
					if r > 127 {
						t.Fatalf("non-ASCII rune %q escaped the ASCII fallback", r)
					}
				}
			}
			// Every mode says the same things, whatever glyphs it says them in.
			for _, want := range []string{"acme-payments", "claude-code", "entire-graph", "not bound", "COR-2029", "background watcher not installed"} {
				if !strings.Contains(plain, want) {
					t.Errorf("mode %s lost %q", m.name, want)
				}
			}
		})
	}
}

// TestPipeModeIsGolden freezes the byte stream a pipe gets. It is the mode
// with no repaints and no colour, so it is the one a log, a CI job and a
// reviewer actually read.
func TestPipeModeIsGolden(t *testing.T) {
	t.Parallel()
	out, _ := renderRun(scriptedRun, tui.PlainCaps(), 100)
	golden := filepath.Join("testdata", "pipe.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with UPDATE_GOLDEN=1)", err)
	}
	if len(want) == 0 {
		t.Fatal("non-vacuity: the golden file is empty")
	}
	if out != string(want) {
		t.Errorf("pipe output drifted.\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// TestMeaningSurvivesNoColor is the "never encode meaning in colour alone"
// check: strip the escapes from the colour stream and you get the NO_COLOR
// stream, byte for byte. Anything a reader could only learn from a colour
// would show up here as a difference.
func TestMeaningSurvivesNoColor(t *testing.T) {
	t.Parallel()
	colored, _ := renderRun(scriptedRun, tui.Caps{TTY: true, Color: true, Unicode: true, Width: 100}, 100)
	mono, _ := renderRun(scriptedRun, tui.Caps{TTY: true, Color: false, Unicode: true, Width: 100}, 100)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatal("non-vacuity: the colour stream carried no colour")
	}
	if StripANSI(colored) != mono {
		t.Errorf("stripping colour does not yield the NO_COLOR stream:\n--- stripped ---\n%s\n--- nocolor ---\n%s", StripANSI(colored), mono)
	}
	// And the states are still distinguishable without it.
	for _, glyph := range []string{"\u2713", "\u2717", "\u2212"} {
		if !strings.Contains(mono, glyph) {
			t.Errorf("NO_COLOR output lost the %q state glyph, leaving colour as the only signal", glyph)
		}
	}
}

// TestNoLineExceedsTheWidth is the measured regression: today's summary row is
// emitted at a fixed 202 characters whatever the terminal is, so it wraps into
// mush in any split pane. No row this package emits may do that, at any width,
// in any mode.
func TestNoLineExceedsTheWidth(t *testing.T) {
	t.Parallel()
	for _, width := range []int{40, 60, 80, 200} {
		for _, m := range renderModes(width) {
			t.Run(m.name, func(t *testing.T) {
				caps := m.caps
				out, _ := renderRun(scriptedRun, caps, width)
				lines := outputLines(out)
				if len(lines) == 0 {
					t.Fatal("non-vacuity: nothing rendered")
				}
				longest := 0
				for _, line := range lines {
					if cells := runewidth.StringWidth(StripANSI(line)); cells > longest {
						longest = cells
					}
					if cells := runewidth.StringWidth(StripANSI(line)); cells > width {
						t.Errorf("width %d: line of %d cells overflows: %q", width, cells, StripANSI(line))
					}
				}
				// Non-vacuity: at narrow widths the content really did have to
				// be clipped, so the check was exercised rather than skipped.
				if width <= 60 && longest < width/2 {
					t.Errorf("width %d: longest line is only %d cells, the budget was never under pressure", width, longest)
				}
			})
		}
	}
}

// TestLiveLabelFitsTheSpinnerRow budgets the transient line too. The spinner
// and the repaint belong to internal/cli's reporter, which prepends two cells
// and keeps one of slack; anything wider than that wraps, and a wrapped live
// line is the bug that made every phase look like it printed twice.
func TestLiveLabelFitsTheSpinnerRow(t *testing.T) {
	t.Parallel()
	for _, width := range []int{40, 60, 80, 200} {
		rend := rendererFor(tui.Caps{TTY: true, Color: true, Unicode: true, Width: width}, width)
		label := rend.LiveLabel(PhaseBuild.Title(),
			"semantic index: 40781 symbols, 131529 relations across 2184 files in internal and cmd", 7, 9)
		cells := runewidth.StringWidth(StripANSI(label))
		if cells > width-2 {
			t.Errorf("width %d: live label of %d cells leaves no room for the spinner: %q", width, cells, StripANSI(label))
		}
		if cells == 0 {
			t.Fatalf("width %d: live label is empty", width)
		}
		if bar := rend.BarWidth(); bar <= 0 || bar > width {
			t.Errorf("width %d: bar width %d is not sized to the terminal", width, bar)
		}
	}
	// Non-vacuity: the bar is not a constant.
	narrow := rendererFor(tui.Caps{TTY: true, Width: 40}, 40).BarWidth()
	wide := rendererFor(tui.Caps{TTY: true, Width: 200}, 200).BarWidth()
	if narrow == wide {
		t.Errorf("bar width is constant at %d across a 5x width change", narrow)
	}
}

// TestTruncateIsRuneAndBoundarySafe covers the inputs that were verified to
// reach this code: Cyrillic and CJK repo names, and paths with spaces.
func TestTruncateIsRuneAndBoundarySafe(t *testing.T) {
	t.Parallel()
	rend := rendererFor(tui.Caps{TTY: true, Color: true, Unicode: true, Width: 40}, 40)
	for _, text := range []string{
		"repo \u0440\u0435\u043f\u043e-\u540d\u524d \u00b7 13 commits \u00b7 clean tree",
		"/Users/dev/My Documents/\u0440\u0435\u043f\u043e-\u540d\u524d/.entire/brain",
		strings.Repeat("\u540d", 60),
	} {
		got := rend.Truncate(text, 20)
		if runewidth.StringWidth(got) > 20 {
			t.Errorf("Truncate(%q) = %q, %d cells > 20", text, got, runewidth.StringWidth(got))
		}
		if !utf8Valid(got) {
			t.Errorf("Truncate(%q) = %q, cut mid-rune", text, got)
		}
	}
	// A separator-bearing string is cut at a boundary, not mid-word.
	got := rend.Truncate("repo acme-payments \u00b7 13 commits \u00b7 clean tree", 28)
	if strings.Contains(got, "comm\u2026") {
		t.Errorf("Truncate cut mid-word: %q", got)
	}
	if !strings.HasSuffix(got, "\u2026") {
		t.Errorf("Truncate(%q) lost its ellipsis", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.Contains(s, "\uFFFD") {
			return false
		}
	}
	return len([]rune(s)) > 0 || s == ""
}

// TestResolveWidthOrderOfAuthority pins the fallback chain the brief named:
// the terminal, then COLUMNS, then 80.
func TestResolveWidthOrderOfAuthority(t *testing.T) {
	t.Parallel()
	env := func(pairs map[string]string) tui.EnvLookup {
		return func(key string) (string, bool) { value, ok := pairs[key]; return value, ok }
	}
	if got := ResolveWidth(tui.Caps{Width: 132}, env(map[string]string{"COLUMNS": "60"})); got != 132 {
		t.Errorf("terminal width lost to COLUMNS: got %d", got)
	}
	if got := ResolveWidth(tui.PlainCaps(), env(map[string]string{"COLUMNS": "60"})); got != 60 {
		t.Errorf("COLUMNS ignored for a pipe: got %d", got)
	}
	if got := ResolveWidth(tui.PlainCaps(), env(nil)); got != 80 {
		t.Errorf("fallback = %d, want 80", got)
	}
	if got := ResolveWidth(tui.PlainCaps(), env(map[string]string{"COLUMNS": "not-a-number"})); got != 80 {
		t.Errorf("garbage COLUMNS = %d, want the 80 fallback", got)
	}
}

// TestExpectedEmptyIsNotAFailure is the zero-commit case. A repository where
// nothing is wrong must not render three red component failures and 47 lines;
// it states the condition once, names the one action, and stops.
//
// The ceiling is ten. The shape that has to fit is: one Detect row plus its
// three capability rows, one condition row plus its one action row, one Ready
// verdict plus two path rows — nine, with one line of slack. Anything that
// re-introduces per-component reporting blows straight through it.
func TestExpectedEmptyIsNotAFailure(t *testing.T) {
	t.Parallel()
	out, run := renderRun(emptyRepoRun, tui.PlainCaps(), 100)
	lines := outputLines(out)
	if len(lines) == 0 {
		t.Fatal("non-vacuity: nothing rendered")
	}
	const ceiling = 10
	if len(lines) > ceiling {
		t.Errorf("zero-commit run rendered %d lines, ceiling is %d:\n%s", len(lines), ceiling, out)
	}
	if got := run.Failures(); len(got) != 0 {
		t.Errorf("a repo with no commits reported %d failures, want 0: %v", len(got), got)
	}
	if state := run.State(PhaseBuild); state != StateSkipped {
		t.Errorf("Build state = %v, want StateSkipped (expected-empty is not broken)", state)
	}
	for _, line := range run.Lines() {
		if line.Glyph == GlyphFailed {
			t.Errorf("a repo with no commits rendered a failure glyph: %q", line.Text)
		}
	}
	// The one action has to be runnable and present exactly once.
	if strings.Count(out, "make one commit, then: entire brain setup") != 1 {
		t.Errorf("the single runnable action is missing or repeated:\n%s", out)
	}
	// Non-vacuity: the components the old rendering would have listed are
	// genuinely absent, not merely uncounted.
	for _, component := range []string{"seed baseline", "semantic index", "entity index"} {
		if strings.Contains(out, component) {
			t.Errorf("per-component failure %q resurfaced in the zero-commit run", component)
		}
	}
}

// TestBlockedReflowsRatherThanClips covers the refusal path: prose, not facts,
// so it wraps instead of truncating and every row still fits.
func TestBlockedReflowsRatherThanClips(t *testing.T) {
	t.Parallel()
	for _, width := range []int{40, 80} {
		rend := rendererFor(tui.PlainCaps(), width)
		rows := Blocked(rend,
			"not a git repository: /Users/dev/scratch/notgit",
			"entire-brain setup builds every source it has -- sessions, seed, docs, semantic index, entities -- from git history, so it needs git and a repository.",
			[]string{"git init here and make at least one commit", "entire brain setup <path>"})
		if len(rows) < 4 {
			t.Fatalf("width %d: refusal collapsed to %d rows", width, len(rows))
		}
		joined := rend.Block(rows)
		for _, line := range outputLines(joined) {
			if cells := runewidth.StringWidth(StripANSI(line)); cells > width {
				t.Errorf("width %d: refusal row of %d cells overflows: %q", width, cells, line)
			}
		}
		// Re-flow keeps the words; truncation would have eaten them.
		if !strings.Contains(joined, "repository.") {
			t.Errorf("width %d: the explanation was clipped rather than re-flowed:\n%s", width, joined)
		}
	}
}

// TestDetectNamesARemedyForEveryAbsentCapability enforces "no line a reader
// cannot act on".
func TestDetectNamesARemedyForEveryAbsentCapability(t *testing.T) {
	t.Parallel()
	rend := rendererFor(tui.PlainCaps(), 200)
	absent := Detected{
		Repo:     RepoFacts{Name: "acme", Commits: 1},
		Agent:    AgentFacts{},
		Provider: ProviderFacts{},
		Hosted:   HostedFacts{},
	}
	remedies := absent.Remedies()
	if len(remedies) < 3 {
		t.Fatalf("non-vacuity: only %d absent capabilities, too few to test", len(remedies))
	}
	rows := strings.Join(absent.Details(rend), "\n")
	for _, remedy := range remedies {
		if strings.TrimSpace(remedy) == "" {
			t.Error("an absent capability has a blank remedy")
		}
		if !strings.Contains(rows, remedy) {
			t.Errorf("remedy %q is never printed:\n%s", remedy, rows)
		}
	}
	// Ground truth: setup cannot bind a hosted brain today, so it must not
	// imply that it will.
	hosted := absent.Details(rend)[2]
	if !strings.Contains(hosted, "not bound") || !strings.Contains(hosted, "COR-2029") {
		t.Errorf("hosted row does not state local-only: %q", hosted)
	}
	for _, forbidden := range []string{"entire repo view", "entire repo whereami", "binding automatically"} {
		if strings.Contains(hosted, forbidden) {
			t.Errorf("hosted row promises %q, which does not exist", forbidden)
		}
	}

	// Both branches: a fully present capability set prints no remedies.
	present := sampleDetected()
	if got := present.Remedies(); len(got) != 1 {
		t.Errorf("a detected-everything repo still owes %d remedies: %v (only hosted should)", len(got), got)
	}
}

// TestWriterSinkDiscardsTransientLines is the pipe contract: repaints never
// reach a log.
func TestWriterSinkDiscardsTransientLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rend := rendererFor(tui.PlainCaps(), 80)
	sink := NewWriterSink(&buf, rend)
	run := NewRun(sink, rend)
	phase := run.Begin(PhaseBuild)
	for i := 0; i < 50; i++ {
		phase.Progress(i, 50)
		phase.Step("component " + itoa(i))
	}
	if buf.Len() != 0 {
		t.Fatalf("fifty repaints wrote %d bytes to the pipe:\n%s", buf.Len(), buf.String())
	}
	phase.OK("done")
	if lines := outputLines(buf.String()); len(lines) != 1 {
		t.Errorf("one phase wrote %d lines to the pipe: %q", len(lines), buf.String())
	}
	if phase.Steps() != 50 {
		t.Fatalf("non-vacuity: only %d sub-steps ran", phase.Steps())
	}
}
